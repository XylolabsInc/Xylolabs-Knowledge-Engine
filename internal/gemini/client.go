package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	geminiAPIBase      = "https://generativelanguage.googleapis.com/v1beta/models"
	defaultModel       = "gemini-3.1-flash-lite-preview"
	httpTimeout         = 120 * time.Second
	maxAPIResponseSize  = 50 << 20 // 50 MB
	maxRetries          = 3
	retryBaseDelay      = 1 * time.Second
)

// Client wraps the Gemini REST API.
type Client struct {
	apiKey     string
	model      string
	httpClient *http.Client
	mu         sync.Mutex
	logger     *slog.Logger
}

// Message represents a conversation turn.
type Message struct {
	Role              string             // "user" or "model"
	Content           string
	Images            []Image            // inline images
	FunctionCalls     []FunctionCall     // model's tool calls (for history)
	FunctionResponses []FunctionResponse // tool results (for follow-up)
}

// Image holds base64-encoded image data.
type Image struct {
	MimeType string
	Data     []byte
}

// GenerateRequest holds all parameters for a generation call.
type GenerateRequest struct {
	Model         string                // model override (empty = use client default)
	SystemPrompt  string
	Messages      []Message
	ThinkingLevel string                // "none", "low", "medium", "high"
	GoogleSearch  bool                  // enable Google Search grounding
	Tools         []FunctionDeclaration // available tools for function calling
}

// GenerateResponse holds the API response.
type GenerateResponse struct {
	Text          string
	Thinking      string
	TokensUsed    int
	FunctionCalls []FunctionCall // non-empty when model wants to call tools
}

// FunctionDeclaration describes a tool the model can call.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// FunctionCall represents a tool invocation returned by the model.
type FunctionCall struct {
	Name             string         `json:"name"`
	Args             map[string]any `json:"args"`
	ThoughtSignature string         `json:"-"` // opaque signature for thinking continuity
}

// FunctionResponse carries the result of a tool execution back to the model.
type FunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

// NewClient creates a Gemini API client.
// If model is empty, defaults to "gemini-3.1-flash-lite-preview".
func NewClient(apiKey, model string, logger *slog.Logger) *Client {
	if model == "" {
		model = defaultModel
	}
	return &Client{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
		logger: logger.With("component", "gemini-client"),
	}
}

// SetTimeout overrides the default HTTP timeout for long-running requests
// (e.g., KB generation with high thinking budgets).
func (c *Client) SetTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient.Timeout = d
}

// Generate calls the Gemini generateContent endpoint and returns the response.
// Retries on 429 and 5xx responses with exponential backoff.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	model := c.model
	if req.Model != "" {
		model = req.Model
	}

	thinkingBudget := thinkingBudgetFor(req.ThinkingLevel)

	body, err := buildRequestBody(req, thinkingBudget)
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}

	url := fmt.Sprintf("%s/%s:generateContent", geminiAPIBase, model)

	c.logger.Debug("calling gemini API", "model", model, "thinking_level", req.ThinkingLevel)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * time.Duration(1<<uint(attempt-1))
			retryAfter := ""
			if lastErr != nil {
				// Check if the previous error included a Retry-After hint
				if parts := strings.SplitN(lastErr.Error(), "(retry-after: ", 2); len(parts) == 2 {
					retryAfter = strings.TrimSuffix(parts[1], ")")
				}
			}
			if retryAfter != "" {
				if sec, parseErr := strconv.Atoi(retryAfter); parseErr == nil && sec > 0 {
					delay = time.Duration(sec) * time.Second
				}
			}
			c.logger.Warn("gemini API retryable error, retrying", "attempt", attempt, "delay", delay, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gemini: create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-goog-api-key", c.apiKey)

		c.mu.Lock()
		resp, err := c.httpClient.Do(httpReq)
		c.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("gemini: do request: %w", err)
		}

		respData, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("gemini: read response: %w", err)
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			retryAfter := resp.Header.Get("Retry-After")
			errBody := string(respData)
			if len(errBody) > 512 {
				errBody = errBody[:512] + "... (truncated)"
			}
			lastErr = fmt.Errorf("gemini: API error %d (retry-after: %s): %s", resp.StatusCode, retryAfter, errBody)
			if attempt < maxRetries-1 {
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode >= 400 {
			errBody := string(respData)
			if len(errBody) > 512 {
				errBody = errBody[:512] + "... (truncated)"
			}
			return nil, fmt.Errorf("gemini: API error %d: %s", resp.StatusCode, errBody)
		}

		result, err := parseResponse(respData)
		if err != nil {
			return nil, fmt.Errorf("gemini: parse response: %w", err)
		}

		c.logger.Debug("gemini API call complete", "tokens_used", result.TokensUsed)
		return result, nil
	}

	return nil, fmt.Errorf("gemini: %w", lastErr)
}

// thinkingBudgetFor maps a level name to a token budget.
func thinkingBudgetFor(level string) int {
	switch level {
	case "none":
		return 0
	case "low":
		return 2048
	case "high":
		return 32768
	default:
		// "medium" or empty → default to medium
		return 8192
	}
}

// apiPart is a single part in a Gemini content array.
type apiPart struct {
	Text             string               `json:"text,omitempty"`
	InlineData       *apiInlineData       `json:"inlineData,omitempty"`
	Thought          bool                 `json:"thought,omitempty"`
	FunctionCall     *apiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *apiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string               `json:"thoughtSignature,omitempty"`
}

type apiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded
}

type apiContent struct {
	Role  string    `json:"role"`
	Parts []apiPart `json:"parts"`
}

type apiSystemInstruction struct {
	Parts []apiPart `json:"parts"`
}

type apiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type apiGenerationConfig struct {
	ThinkingConfig apiThinkingConfig `json:"thinkingConfig"`
}

type apiGoogleSearch struct{}

type apiFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type apiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type apiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type apiTool struct {
	GoogleSearch         *apiGoogleSearch         `json:"googleSearch,omitempty"`
	FunctionDeclarations []apiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type apiFunctionCallingConfig struct {
	Mode string `json:"mode"` // "AUTO", "ANY", "NONE"
}

type apiToolConfig struct {
	FunctionCallingConfig apiFunctionCallingConfig `json:"functionCallingConfig"`
}

type apiRequest struct {
	Contents            []apiContent         `json:"contents"`
	SystemInstruction   *apiSystemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig    apiGenerationConfig   `json:"generationConfig"`
	Tools               []apiTool             `json:"tools,omitempty"`
	ToolConfig          *apiToolConfig        `json:"toolConfig,omitempty"`
}

func buildRequestBody(req GenerateRequest, thinkingBudget int) ([]byte, error) {
	ar := apiRequest{
		GenerationConfig: apiGenerationConfig{
			ThinkingConfig: apiThinkingConfig{
				ThinkingBudget: thinkingBudget,
			},
		},
	}

	// Gemini API does not allow combining built-in tools (google_search)
	// with custom tools (function calling) in the same request.
	// Prioritize function calling when both are requested.
	if len(req.Tools) > 0 {
		var decls []apiFunctionDeclaration
		for _, t := range req.Tools {
			decls = append(decls, apiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		ar.Tools = []apiTool{{FunctionDeclarations: decls}}
		ar.ToolConfig = &apiToolConfig{
			FunctionCallingConfig: apiFunctionCallingConfig{
				Mode: "AUTO",
			},
		}
	} else if req.GoogleSearch {
		ar.Tools = []apiTool{{GoogleSearch: &apiGoogleSearch{}}}
	}

	if req.SystemPrompt != "" {
		ar.SystemInstruction = &apiSystemInstruction{
			Parts: []apiPart{{Text: req.SystemPrompt}},
		}
	}

	for _, msg := range req.Messages {
		var parts []apiPart

		if msg.Content != "" {
			parts = append(parts, apiPart{Text: msg.Content})
		}

		for _, img := range msg.Images {
			parts = append(parts, apiPart{
				InlineData: &apiInlineData{
					MimeType: img.MimeType,
					Data:     base64.StdEncoding.EncodeToString(img.Data),
				},
			})
		}

		// Add function call parts (model's tool invocations in history)
		for _, fc := range msg.FunctionCalls {
			parts = append(parts, apiPart{
				FunctionCall: &apiFunctionCall{
					Name: fc.Name,
					Args: fc.Args,
				},
				ThoughtSignature: fc.ThoughtSignature,
			})
		}

		// Add function response parts (tool execution results)
		for _, fr := range msg.FunctionResponses {
			parts = append(parts, apiPart{
				FunctionResponse: &apiFunctionResponse{
					Name:     fr.Name,
					Response: fr.Response,
				},
			})
		}

		ar.Contents = append(ar.Contents, apiContent{
			Role:  msg.Role,
			Parts: parts,
		})
	}

	return json.Marshal(ar)
}

// apiResponsePart mirrors parts in the response, where thought parts have Thought==true.
type apiResponsePart struct {
	Text             string           `json:"text"`
	Thought          bool             `json:"thought"`
	FunctionCall     *apiFunctionCall `json:"functionCall,omitempty"`
	ThoughtSignature string           `json:"thoughtSignature,omitempty"`
}

type apiResponseContent struct {
	Parts []apiResponsePart `json:"parts"`
}

type apiCandidate struct {
	Content apiResponseContent `json:"content"`
}

type apiUsageMetadata struct {
	TotalTokenCount int `json:"totalTokenCount"`
}

type apiResponse struct {
	Candidates    []apiCandidate   `json:"candidates"`
	UsageMetadata apiUsageMetadata `json:"usageMetadata"`
}

// GenerateFromImage is a convenience method for single-image prompts.
// It satisfies the extractor.GeminiClient interface.
func (c *Client) GenerateFromImage(ctx context.Context, prompt string, imageData []byte, mimeType string) (string, error) {
	resp, err := c.Generate(ctx, GenerateRequest{
		Messages: []Message{{
			Role:    "user",
			Content: prompt,
			Images:  []Image{{MimeType: mimeType, Data: imageData}},
		}},
		ThinkingLevel: "none",
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func parseResponse(data []byte) (*GenerateResponse, error) {
	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	if len(ar.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	result := &GenerateResponse{
		TokensUsed: ar.UsageMetadata.TotalTokenCount,
	}

	for _, part := range ar.Candidates[0].Content.Parts {
		if part.Thought {
			result.Thinking += part.Text
		} else if part.FunctionCall != nil {
			result.FunctionCalls = append(result.FunctionCalls, FunctionCall{
				Name:             part.FunctionCall.Name,
				Args:             part.FunctionCall.Args,
				ThoughtSignature: part.ThoughtSignature,
			})
		} else {
			result.Text += part.Text
		}
	}

	return result, nil
}
