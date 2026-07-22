# OpenRouter (OpenAI-Compatible) Endpoint Support Implementation Plan — Xylolabs-Knowledge-Engine

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the KB service talk to any OpenAI-compatible `chat/completions` endpoint (primarily OpenRouter) via `LLM_ENDPOINT`/`LLM_API_KEY`, while native Gemini stays the zero-config default; bump every `gemini-3.5-flash` default to `gemini-3.6-flash`.

**Architecture:** One client, two transports. `internal/gemini/client.go` keeps the native path untouched; a new `openai_transport.go` translates `GenerateRequest`/`GenerateResponse` to/from the OpenAI wire format when the client has an endpoint override. The retry/timeout/size-cap HTTP execution path is shared.

**Tech Stack:** Go 1.26+, stdlib only (`net/http`, `encoding/json`, `httptest`). slog structured logging, error wrapping `"<pkg>: <action>: %w"`, table-driven tests per AGENTS.md.

**Spec:** `docs/superpowers/specs/2026-07-22-openrouter-endpoint-design.md`

## Global Constraints

- `LLM_ENDPOINT` is the FULL chat/completions URL (e.g. `https://openrouter.ai/api/v1/chat/completions`) — same convention as xylolabs-api
- Key fallback: `LLM_API_KEY` → `GEMINI_API_KEY`; model vars unchanged (`GEMINI_MODEL`, `GEMINI_PRO_MODEL`, `KB_GEN_MODEL`)
- `GoogleSearch` in OpenAI mode: silently dropped, one `slog.Warn` per process (`sync.Once`); `ThoughtSignature` dropped
- All model-default bumps go to exactly `gemini-3.6-flash`
- Commits GPG-signed (`-S`), Conventional Commits + gitmoji, no Co-Authored-By, `git pull --rebase` before push, push after each commit. TWO commits total (bump, then feature). No deploy.
- Validation gate for every task: `go build ./...` and `go test -race ./...` green.

---

### Task 1: Model-default bump (commit 1)

**Files:**
- Modify: `internal/gemini/client.go:20` (`defaultModel`)
- Modify: `internal/config/config.go:101-102` (env defaults)
- Modify: `cmd/kb-gen/main.go:62` (`defaultModel`), `:110` (flag help text)
- Modify: `scripts/generate-kb.sh:30`
- Modify: `scripts/deploy.sh:297` (hardcoded smoke/translator URL)
- Modify: `README.md:211,220,232`, `docs/slack-bot.md:52,333,457`, `docs/setup.md:233`

**Interfaces:** none — pure string bump, `gemini-3.5-flash` → `gemini-3.6-flash` at every listed site (and any other hit `rg -n 'gemini-3\.5' --iglob '!docs/superpowers'` finds; do not touch git history artifacts).

- [ ] **Step 1: Bump all sites** — mechanical replace at the paths above; re-run `rg -n 'gemini-3\.5' -g '!docs/superpowers/**'` and confirm zero hits remain.

- [ ] **Step 2: Validate**

Run: `go build ./... && go test -race ./...`
Expected: PASS (client_test.go has no model-string assertions that pin 3.5 — if one fails, update the expectation in the same commit).

- [ ] **Step 3: Commit**

```bash
git add -A
git commit -S -m "chore(gemini): ⬆️ bump default model to gemini-3.6-flash"
git pull --rebase && git push
```

### Task 2: Config — `LLM_ENDPOINT` / `LLM_API_KEY`

**Files:**
- Modify: `internal/config/config.go` (struct ~line 39-41, load ~line 100-102, `GeminiEnabled()` ~line 148, `Validate()` ~line 181)
- Test: `internal/config/config_test.go` (create if absent; follow existing test-file conventions in the package)

**Interfaces:**
- Produces: `Config.LLMEndpoint string`, `Config.LLMAPIKey string`, method `func (c *Config) LLMKey() string` (returns `LLMAPIKey` if non-empty, else `GeminiAPIKey`). Task 4 wires these into client construction.

- [ ] **Step 1: Write failing tests**

```go
func TestLLMKeyFallback(t *testing.T) {
	tests := []struct {
		name   string
		llmKey string
		gemKey string
		want   string
	}{
		{"llm key wins", "or-key", "g-key", "or-key"},
		{"falls back to gemini", "", "g-key", "g-key"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{LLMAPIKey: tt.llmKey, GeminiAPIKey: tt.gemKey}
			if got := c.LLMKey(); got != tt.want {
				t.Errorf("LLMKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateLLMEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		key      string
		wantErr  bool
	}{
		{"unset is fine", "", "", false},
		{"https ok", "https://openrouter.ai/api/v1/chat/completions", "k", false},
		{"http ok", "http://localhost:8080/v1/chat/completions", "k", false},
		{"bad scheme", "ftp://openrouter.ai/x", "k", true},
		{"not a url", "openrouter.ai", "k", true},
		{"endpoint without any key", "https://openrouter.ai/api/v1/chat/completions", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := minimalValidConfig() // helper: smallest Config that passes Validate()
			c.LLMEndpoint = tt.endpoint
			c.LLMAPIKey = tt.key
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

If the package has no existing `minimalValidConfig` helper, write one in the test file by constructing a `Config` with just the fields `Validate()` requires (read `Validate()` top-to-bottom and satisfy each check).

- [ ] **Step 2: Run to verify failure** — `go test -race ./internal/config/` → compile error (fields/method missing).

- [ ] **Step 3: Implement**

Struct (after `GeminiProModel`):

```go
	// LLMEndpoint, when set, switches the LLM client to an OpenAI-compatible
	// chat/completions endpoint (full URL, e.g. OpenRouter). Empty = native Gemini.
	LLMEndpoint string
	// LLMAPIKey is the bearer key for LLMEndpoint; falls back to GeminiAPIKey.
	LLMAPIKey string
```

Load (after the Gemini lines):

```go
		LLMEndpoint: os.Getenv("LLM_ENDPOINT"),
		LLMAPIKey:   os.Getenv("LLM_API_KEY"),
```

Method + `GeminiEnabled()` update:

```go
// LLMKey returns the bearer key for the configured LLM endpoint:
// LLM_API_KEY when set, else GEMINI_API_KEY.
func (c *Config) LLMKey() string {
	if c.LLMAPIKey != "" {
		return c.LLMAPIKey
	}
	return c.GeminiAPIKey
}
```

`GeminiEnabled()` (line ~148) becomes `return c.LLMKey() != ""`.

`Validate()` (after the Gemini model check, import `net/url`):

```go
	if c.LLMEndpoint != "" {
		u, err := url.Parse(c.LLMEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("LLM_ENDPOINT %q is not an http(s) URL", c.LLMEndpoint))
		}
		if c.LLMKey() == "" {
			errs = append(errs, "LLM_ENDPOINT is set but neither LLM_API_KEY nor GEMINI_API_KEY is")
		}
	}
```

- [ ] **Step 4: Run** — `go test -race ./internal/config/` → PASS.

- [ ] **Step 5: Commit is deferred** — Tasks 2-5 land together as the feature commit in Task 5 Step 4 (they are one deliverable; the repo stays green between tasks because nothing consumes the new fields yet).

### Task 3: OpenAI transport (`internal/gemini/openai_transport.go`)

**Files:**
- Create: `internal/gemini/openai_transport.go`
- Test: `internal/gemini/openai_transport_test.go`
- Modify: `internal/gemini/client.go` (add `endpoint` field + `WithEndpoint`; branch in `Generate`; extract shared retry helper)

**Interfaces:**
- Consumes: `GenerateRequest`, `GenerateResponse`, `Message`, `FunctionCall`, `FunctionResponse`, `FunctionDeclaration`, `Image` (existing types, unchanged).
- Produces: `func (c *Client) WithEndpoint(endpoint string) *Client` (chainable setter; empty string = native mode); package-private `buildOpenAIRequestBody(req GenerateRequest, model string) ([]byte, error)`, `parseOpenAIResponse(data []byte) (*GenerateResponse, error)`, `func (c *Client) doWithRetry(ctx context.Context, url string, header http.Header, body []byte) ([]byte, error)`.

- [ ] **Step 1: Write failing tests** (`openai_transport_test.go`; table-driven; no live API)

```go
package gemini

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}
	return m
}

func TestBuildOpenAIRequestBody(t *testing.T) {
	tests := []struct {
		name  string
		req   GenerateRequest
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "system prompt becomes leading system message",
			req: GenerateRequest{
				SystemPrompt: "be terse",
				Messages:     []Message{{Role: "user", Content: "hi"}},
			},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				first := msgs[0].(map[string]any)
				if first["role"] != "system" || first["content"] != "be terse" {
					t.Errorf("first message = %v", first)
				}
				second := msgs[1].(map[string]any)
				if second["role"] != "user" {
					t.Errorf("second role = %v", second["role"])
				}
			},
		},
		{
			name: "model role maps to assistant",
			req: GenerateRequest{Messages: []Message{
				{Role: "user", Content: "q"},
				{Role: "model", Content: "a"},
			}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				if msgs[1].(map[string]any)["role"] != "assistant" {
					t.Errorf("model role not mapped to assistant")
				}
			},
		},
		{
			name: "thinking levels map to reasoning_effort",
			req: GenerateRequest{ThinkingLevel: "high",
				Messages: []Message{{Role: "user", Content: "q"}}},
			check: func(t *testing.T, body map[string]any) {
				if body["reasoning_effort"] != "high" {
					t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
				}
			},
		},
		{
			name: "thinking none omits reasoning_effort",
			req: GenerateRequest{ThinkingLevel: "none",
				Messages: []Message{{Role: "user", Content: "q"}}},
			check: func(t *testing.T, body map[string]any) {
				if _, ok := body["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be omitted for none")
				}
			},
		},
		{
			name: "tools become openai function tools",
			req: GenerateRequest{
				Messages: []Message{{Role: "user", Content: "q"}},
				Tools: []FunctionDeclaration{{
					Name:        "get_weather",
					Description: "d",
					Parameters:  map[string]any{"type": "object"},
				}},
			},
			check: func(t *testing.T, body map[string]any) {
				tools := body["tools"].([]any)
				fn := tools[0].(map[string]any)["function"].(map[string]any)
				if tools[0].(map[string]any)["type"] != "function" || fn["name"] != "get_weather" {
					t.Errorf("tools = %v", tools)
				}
			},
		},
		{
			name: "google search silently dropped",
			req: GenerateRequest{GoogleSearch: true,
				Messages: []Message{{Role: "user", Content: "q"}}},
			check: func(t *testing.T, body map[string]any) {
				if _, ok := body["tools"]; ok {
					t.Errorf("googleSearch must not produce tools in openai mode")
				}
			},
		},
		{
			name: "image becomes data-uri image_url part",
			req: GenerateRequest{Messages: []Message{{
				Role: "user", Content: "what is this",
				Images: []Image{{MimeType: "image/png", Data: []byte{1, 2}}},
			}}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				parts := msgs[0].(map[string]any)["content"].([]any)
				img := parts[1].(map[string]any)
				url := img["image_url"].(map[string]any)["url"].(string)
				if img["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") {
					t.Errorf("image part = %v", img)
				}
			},
		},
		{
			name: "tool call round-trip correlates ids",
			req: GenerateRequest{Messages: []Message{
				{Role: "user", Content: "weather?"},
				{Role: "model", FunctionCalls: []FunctionCall{{
					Name: "get_weather", Args: map[string]any{"city": "Seoul"},
				}}},
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "get_weather", Response: map[string]any{"temp": 30},
				}}},
			}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
				callID := call["id"].(string)
				var args map[string]any
				if err := json.Unmarshal([]byte(call["function"].(map[string]any)["arguments"].(string)), &args); err != nil || args["city"] != "Seoul" {
					t.Errorf("arguments not a JSON string of args: %v", call)
				}
				toolMsg := msgs[2].(map[string]any)
				if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != callID {
					t.Errorf("tool response not correlated: %v (want id %s)", toolMsg, callID)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := buildOpenAIRequestBody(tt.req, "test-model")
			if err != nil {
				t.Fatalf("buildOpenAIRequestBody: %v", err)
			}
			body := decodeBody(t, data)
			if body["model"] != "test-model" {
				t.Errorf("model = %v", body["model"])
			}
			tt.check(t, body)
		})
	}
}

func TestParseOpenAIResponse(t *testing.T) {
	tests := []struct {
		name string
		json string
		want func(t *testing.T, r *GenerateResponse)
	}{
		{
			name: "text and usage",
			json: `{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":42}}`,
			want: func(t *testing.T, r *GenerateResponse) {
				if r.Text != "hello" || r.TokensUsed != 42 {
					t.Errorf("got %+v", r)
				}
			},
		},
		{
			name: "reasoning goes to Thinking",
			json: `{"choices":[{"message":{"content":"x","reasoning":"because"}}],"usage":{"total_tokens":1}}`,
			want: func(t *testing.T, r *GenerateResponse) {
				if r.Thinking != "because" {
					t.Errorf("Thinking = %q", r.Thinking)
				}
			},
		},
		{
			name: "tool calls decode arguments",
			json: `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Seoul\"}"}}]}}],"usage":{"total_tokens":5}}`,
			want: func(t *testing.T, r *GenerateResponse) {
				if len(r.FunctionCalls) != 1 || r.FunctionCalls[0].Name != "get_weather" ||
					r.FunctionCalls[0].Args["city"] != "Seoul" {
					t.Errorf("FunctionCalls = %+v", r.FunctionCalls)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := parseOpenAIResponse([]byte(tt.json))
			if err != nil {
				t.Fatalf("parseOpenAIResponse: %v", err)
			}
			tt.want(t, r)
		})
	}
	t.Run("no choices errors", func(t *testing.T) {
		if _, err := parseOpenAIResponse([]byte(`{"choices":[]}`)); err == nil {
			t.Error("expected error for empty choices")
		}
	})
}

func TestGenerateOpenAIMode(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}],"usage":{"total_tokens":7}}`))
	}))
	defer srv.Close()

	c := NewClient("or-key", "google/gemini-3.6-flash", slog.Default()).
		WithEndpoint(srv.URL + "/api/v1/chat/completions")
	resp, err := c.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Text != "pong" || resp.TokensUsed != 7 {
		t.Errorf("resp = %+v", resp)
	}
	if gotAuth != "Bearer or-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["model"] != "google/gemini-3.6-flash" {
		t.Errorf("model = %v", gotBody["model"])
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test -race ./internal/gemini/` → compile errors (`buildOpenAIRequestBody`, `WithEndpoint` undefined).

- [ ] **Step 3: Implement `openai_transport.go`**

```go
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
					ID:   id,
					Type: "function",
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
```

Note: the `null` content in tool-call responses decodes into `""` for the `string` field — no special handling needed.

- [ ] **Step 4: Wire into `client.go`**

Add to the `Client` struct: `endpoint string` (below `model`) and a `googleSearchWarn sync.Once` field (import `sync`). Add:

```go
// WithEndpoint switches the client to an OpenAI-compatible
// chat/completions endpoint (full URL, e.g. OpenRouter). Chainable.
// Empty endpoint keeps the native Gemini transport.
func (c *Client) WithEndpoint(endpoint string) *Client {
	c.endpoint = endpoint
	return c
}
```

Extract the retry loop (lines 132-204) into a shared helper — same behavior, parameterized URL/headers (error strings keep the existing `gemini:` prefixes so log greps stay stable):

```go
func (c *Client) doWithRetry(ctx context.Context, url string, header http.Header, body []byte) ([]byte, error) {
	// …existing loop verbatim, with the two header .Set calls replaced by:
	//     for k, vs := range header { for _, v := range vs { httpReq.Header.Set(k, v) } }
	// and returning respData instead of calling parseResponse.
}
```

`Generate` becomes:

```go
	var (
		respData []byte
	)
	if c.endpoint != "" {
		if req.GoogleSearch {
			c.googleSearchWarn.Do(func() {
				c.logger.Warn("google search grounding unsupported on openai-compatible endpoint, dropping", "endpoint", c.endpoint)
			})
		}
		body, err := buildOpenAIRequestBody(req, model)
		if err != nil {
			return nil, fmt.Errorf("gemini: build openai request: %w", err)
		}
		header := http.Header{}
		header.Set("Content-Type", "application/json")
		header.Set("Authorization", "Bearer "+c.apiKey)
		c.logger.Debug("calling openai-compatible API", "endpoint", c.endpoint, "model", model, "thinking_level", req.ThinkingLevel)
		respData, err = c.doWithRetry(ctx, c.endpoint, header, body)
		if err != nil {
			return nil, err
		}
		result, err := parseOpenAIResponse(respData)
		if err != nil {
			return nil, fmt.Errorf("gemini: parse openai response: %w", err)
		}
		c.logger.Debug("openai-compatible API call complete", "tokens_used", result.TokensUsed)
		return result, nil
	}
	// …native path exactly as before, now via doWithRetry with the
	// x-goog-api-key header and parseResponse.
```

- [ ] **Step 5: Run** — `go test -race ./internal/gemini/` → all PASS (new tests + existing client_test.go untouched and green).

### Task 4: Wiring — service main + kb-gen

**Files:**
- Modify: `cmd/xylolabs-kb/main.go:77-83` (client construction)
- Modify: `cmd/kb-gen/main.go` (~line 246 client construction; api-key resolution block — grep `GEMINI_API_KEY` in the file)

**Interfaces:**
- Consumes: `cfg.LLMEndpoint`, `cfg.LLMKey()` (Task 2); `(*gemini.Client).WithEndpoint` (Task 3).

- [ ] **Step 1: main.go**

```go
	var geminiClient *gemini.Client
	if cfg.GeminiEnabled() {
		geminiClient = gemini.NewClient(cfg.LLMKey(), cfg.GeminiModel, logger)
		if cfg.LLMEndpoint != "" {
			geminiClient.WithEndpoint(cfg.LLMEndpoint)
			logger.Info("llm client enabled (openai-compatible endpoint)", "endpoint", cfg.LLMEndpoint, "model", cfg.GeminiModel)
		} else {
			logger.Info("gemini client enabled", "model", cfg.GeminiModel)
		}
	} else {
		logger.Info("llm client disabled (missing API key)")
	}
```

- [ ] **Step 2: kb-gen** — where the client is built (~line 246):

```go
	client := gemini.NewClient(apiKey, model, logger)
	if ep := os.Getenv("LLM_ENDPOINT"); ep != "" {
		client.WithEndpoint(ep)
	}
	client.SetTimeout(10 * time.Minute)
```

and in kb-gen's api-key fallback chain, prefer `LLM_API_KEY` before `GEMINI_API_KEY` (mirror the flag help text: `"Gemini API key (or LLM_API_KEY/GEMINI_API_KEY env var)"`).

- [ ] **Step 3: Validate** — `go build ./... && go test -race ./...` → green.

### Task 5: Scripts, docs, feature commit

**Files:**
- Modify: `scripts/deploy.sh` (~line 297 inline-Python translator)
- Modify: `scripts/generate-kb.sh` (comment block near line 27-33)
- Modify: `README.md` (env-var table/section), `docs/setup.md` (~line 233 region), `docs/slack-bot.md` (model notes)

**Interfaces:** none.

- [ ] **Step 1: deploy.sh translator** — make the inline Python honor the deployed env (it already receives the environment; `api_key` comes from the .env). Replace the fixed URL/payload with:

```python
llm_endpoint = os.environ.get("LLM_ENDPOINT", "")
model = os.environ.get("GEMINI_MODEL", "gemini-3.6-flash")
if llm_endpoint:
    payload = json.dumps({
        "model": os.environ.get("LLM_MODEL", model),
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(llm_endpoint, data=payload, headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer " + (os.environ.get("LLM_API_KEY") or api_key),
    }, method="POST")
else:
    payload = json.dumps({
        "contents": [{"parts": [{"text": prompt}]}],
        "generationConfig": {"temperature": 0.1, "maxOutputTokens": 1024},
    }).encode()
    url = f"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?key={api_key}"
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"}, method="POST")
```

and adapt the response parsing to branch: OpenAI → `data["choices"][0]["message"]["content"]`, native → existing `candidates` path. (`import os` if the snippet lacks it.) Run `bash -n scripts/deploy.sh`.

- [ ] **Step 2: generate-kb.sh + docs** — add to generate-kb.sh's comment block: `# Set LLM_ENDPOINT/LLM_API_KEY to use an OpenAI-compatible endpoint (e.g. OpenRouter); model ids then use the provider's naming (google/gemini-3.6-flash).` README/setup.md: document the two new env vars in the configuration table with the OpenRouter example URL; slack-bot.md: one sentence noting Google Search grounding is disabled when `LLM_ENDPOINT` is set.

- [ ] **Step 3: Full validation** — `go build ./... && go test -race ./... && bash -n scripts/deploy.sh && bash -n scripts/generate-kb.sh` → all green.

- [ ] **Step 4: Feature commit**

```bash
git add -A
git commit -S -m "feat(gemini): ✨ OpenAI-compatible endpoint support (OpenRouter)"
git pull --rebase && git push
```

No deploy (spec: operator decides separately).
