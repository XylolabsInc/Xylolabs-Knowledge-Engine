package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// OpenAI-compatible wire types (chat/completions).

type oaImageURL struct {
	URL string `json:"url"`
}

type oaContentPart struct {
	Type     string      `json:"type"` // "text" | "image_url"
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded args object
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"` // always "function"
	Function oaFunction `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"` // string or []oaContentPart
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaTool struct {
	Type     string    `json:"type"` // always "function"
	Function oaToolDef `json:"function"`
}

type oaRequest struct {
	Model           string      `json:"model"`
	Messages        []oaMessage `json:"messages"`
	Tools           []oaTool    `json:"tools,omitempty"`
	ReasoningEffort string      `json:"reasoning_effort,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Content   string       `json:"content"`
			Reasoning string       `json:"reasoning"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// buildOpenAIRequestBody translates a GenerateRequest into an
// OpenAI-compatible chat/completions request. GoogleSearch has no
// equivalent and is dropped (the caller logs the warning);
// ThoughtSignature is dropped.
func buildOpenAIRequestBody(req GenerateRequest, model string) ([]byte, error) {
	oar := oaRequest{Model: model}

	switch req.ThinkingLevel {
	case "none":
		// omit
	case "low", "high":
		oar.ReasoningEffort = req.ThinkingLevel
	default:
		// "medium" or empty → medium, matching the native default budget
		oar.ReasoningEffort = "medium"
	}

	for _, t := range req.Tools {
		oar.Tools = append(oar.Tools, oaTool{Type: "function", Function: oaToolDef(t)})
	}

	if req.SystemPrompt != "" {
		oar.Messages = append(oar.Messages, oaMessage{Role: "system", Content: req.SystemPrompt})
	}

	// pendingCallIDs correlates synthesized tool_call ids with the
	// FunctionResponses that answer them (FIFO per function name — the
	// native format carries no ids).
	pendingCallIDs := map[string][]string{}

	for i, msg := range req.Messages {
		role := msg.Role
		if role == "model" {
			role = "assistant"
		}

		if len(msg.FunctionCalls) > 0 {
			calls := make([]oaToolCall, len(msg.FunctionCalls))
			for j, fc := range msg.FunctionCalls {
				argsJSON, err := json.Marshal(fc.Args)
				if err != nil {
					return nil, fmt.Errorf("marshal args for %s: %w", fc.Name, err)
				}
				id := fmt.Sprintf("call_%d_%d_%s", i, j, fc.Name)
				pendingCallIDs[fc.Name] = append(pendingCallIDs[fc.Name], id)
				calls[j] = oaToolCall{
					ID:       id,
					Type:     "function",
					Function: oaFunction{Name: fc.Name, Arguments: string(argsJSON)},
				}
			}
			m := oaMessage{Role: "assistant", ToolCalls: calls}
			if msg.Content != "" {
				m.Content = msg.Content
			}
			oar.Messages = append(oar.Messages, m)
			continue
		}

		if len(msg.FunctionResponses) > 0 {
			for _, fr := range msg.FunctionResponses {
				respJSON, err := json.Marshal(fr.Response)
				if err != nil {
					return nil, fmt.Errorf("marshal response for %s: %w", fr.Name, err)
				}
				id := "call_" + fr.Name
				if ids := pendingCallIDs[fr.Name]; len(ids) > 0 {
					id = ids[0]
					pendingCallIDs[fr.Name] = ids[1:]
				}
				oar.Messages = append(oar.Messages, oaMessage{
					Role:       "tool",
					ToolCallID: id,
					Content:    string(respJSON),
				})
			}
			continue
		}

		if len(msg.Images) > 0 {
			parts := []oaContentPart{}
			if msg.Content != "" {
				parts = append(parts, oaContentPart{Type: "text", Text: msg.Content})
			}
			for _, img := range msg.Images {
				parts = append(parts, oaContentPart{
					Type: "image_url",
					ImageURL: &oaImageURL{
						URL: "data:" + img.MimeType + ";base64," +
							base64.StdEncoding.EncodeToString(img.Data),
					},
				})
			}
			oar.Messages = append(oar.Messages, oaMessage{Role: role, Content: parts})
			continue
		}

		oar.Messages = append(oar.Messages, oaMessage{Role: role, Content: msg.Content})
	}

	return json.Marshal(oar)
}

func parseOpenAIResponse(data []byte) (*GenerateResponse, error) {
	var oar oaResponse
	if err := json.Unmarshal(data, &oar); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(oar.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	msg := oar.Choices[0].Message
	result := &GenerateResponse{
		Text:       msg.Content,
		Thinking:   msg.Reasoning,
		TokensUsed: oar.Usage.TotalTokens,
	}
	for _, tc := range msg.ToolCalls {
		args := map[string]any{}
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("decode tool-call arguments for %s: %w", tc.Function.Name, err)
			}
		}
		result.FunctionCalls = append(result.FunctionCalls, FunctionCall{
			Name: tc.Function.Name,
			Args: args,
		})
	}
	return result, nil
}
