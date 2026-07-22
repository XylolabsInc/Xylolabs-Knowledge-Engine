package gemini

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		{
			name: "same function called twice in one message correlates FIFO",
			req: GenerateRequest{Messages: []Message{
				{Role: "user", Content: "weather in two cities?"},
				{Role: "model", FunctionCalls: []FunctionCall{
					{Name: "get_weather", Args: map[string]any{"city": "Seoul"}},
					{Name: "get_weather", Args: map[string]any{"city": "Busan"}},
				}},
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "get_weather", Response: map[string]any{"temp": 30},
				}}},
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "get_weather", Response: map[string]any{"temp": 25},
				}}},
			}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				calls := msgs[1].(map[string]any)["tool_calls"].([]any)
				id0 := calls[0].(map[string]any)["id"].(string)
				id1 := calls[1].(map[string]any)["id"].(string)
				if id0 == id1 {
					t.Fatalf("expected distinct ids for the two calls, got %q twice", id0)
				}
				toolMsg0 := msgs[2].(map[string]any)
				toolMsg1 := msgs[3].(map[string]any)
				if toolMsg0["tool_call_id"] != id0 {
					t.Errorf("first response tool_call_id = %v, want %q (first call)", toolMsg0["tool_call_id"], id0)
				}
				if toolMsg1["tool_call_id"] != id1 {
					t.Errorf("second response tool_call_id = %v, want %q (second call)", toolMsg1["tool_call_id"], id1)
				}
			},
		},
		{
			name: "FIFO correlation holds across messages",
			req: GenerateRequest{Messages: []Message{
				{Role: "user", Content: "weather?"},
				{Role: "model", FunctionCalls: []FunctionCall{{
					Name: "get_weather", Args: map[string]any{"city": "Seoul"},
				}}},
				{Role: "user", Content: "and also?"}, // interleaving message, no function traffic
				{Role: "model", FunctionCalls: []FunctionCall{{
					Name: "get_weather", Args: map[string]any{"city": "Busan"},
				}}},
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "get_weather", Response: map[string]any{"temp": 30},
				}}},
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "get_weather", Response: map[string]any{"temp": 25},
				}}},
			}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				firstCallID := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
				secondCallID := msgs[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
				if firstCallID == secondCallID {
					t.Fatalf("expected distinct ids for the two calls, got %q twice", firstCallID)
				}
				firstResp := msgs[4].(map[string]any)
				secondResp := msgs[5].(map[string]any)
				if firstResp["tool_call_id"] != firstCallID {
					t.Errorf("first response tool_call_id = %v, want %q (first assistant message's call)", firstResp["tool_call_id"], firstCallID)
				}
				if secondResp["tool_call_id"] != secondCallID {
					t.Errorf("second response tool_call_id = %v, want %q (second assistant message's call)", secondResp["tool_call_id"], secondCallID)
				}
			},
		},
		{
			name: "orphan function response falls back to call_<name>",
			req: GenerateRequest{Messages: []Message{
				{Role: "user", FunctionResponses: []FunctionResponse{{
					Name: "orphan_fn", Response: map[string]any{"ok": true},
				}}},
			}},
			check: func(t *testing.T, body map[string]any) {
				msgs := body["messages"].([]any)
				toolMsg := msgs[0].(map[string]any)
				if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_orphan_fn" {
					t.Errorf("orphan tool message = %v, want tool_call_id %q", toolMsg, "call_orphan_fn")
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
			name: "reasoning_content falls back into Thinking when reasoning is absent",
			json: `{"choices":[{"message":{"content":"x","reasoning_content":"because too"}}],"usage":{"total_tokens":1}}`,
			want: func(t *testing.T, r *GenerateResponse) {
				if r.Thinking != "because too" {
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

// TestDoWithRetryRetriesOn429ThenSucceeds exercises doWithRetry directly
// (shared by both the native Gemini path and the openai-compatible path):
// a 429 with a Retry-After hint must be retried, honoring the hint's delay,
// with the same header set attached to every attempt.
func TestDoWithRetryRetriesOn429ThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	var requestCount int
	var authHeaders []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		n := requestCount
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		_, _ = w.Write([]byte("ok body"))
	}))
	defer srv.Close()

	c := NewClient("retry-key", "test-model", slog.Default())
	header := http.Header{}
	header.Set("Authorization", "Bearer retry-key")

	data, err := c.doWithRetry(context.Background(), srv.URL, header, []byte(`{}`))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	if string(data) != "ok body" {
		t.Errorf("data = %q, want %q", data, "ok body")
	}

	mu.Lock()
	defer mu.Unlock()
	if requestCount != 2 {
		t.Fatalf("request count = %d, want 2", requestCount)
	}
	for i, a := range authHeaders {
		if a != "Bearer retry-key" {
			t.Errorf("request %d Authorization = %q, want %q", i, a, "Bearer retry-key")
		}
	}
}
