package gemini

import (
	"encoding/json"
	"testing"
)

func TestParseResponse(t *testing.T) {
	raw := `{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "Hello world", "thought": false}
				]
			}
		}],
		"usageMetadata": {"totalTokenCount": 42}
	}`

	resp, err := parseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parseResponse() error = %v", err)
	}
	if resp.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", resp.Text, "Hello world")
	}
	if resp.TokensUsed != 42 {
		t.Errorf("TokensUsed = %d, want 42", resp.TokensUsed)
	}
}

func TestParseResponseWithThinking(t *testing.T) {
	raw := `{
		"candidates": [{
			"content": {
				"parts": [
					{"text": "Let me think...", "thought": true},
					{"text": "The answer is 42"}
				]
			}
		}],
		"usageMetadata": {"totalTokenCount": 100}
	}`

	resp, err := parseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if resp.Text != "The answer is 42" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Thinking != "Let me think..." {
		t.Errorf("Thinking = %q", resp.Thinking)
	}
}

func TestParseResponseNoCandidates(t *testing.T) {
	raw := `{"candidates": [], "usageMetadata": {"totalTokenCount": 0}}`
	_, err := parseResponse([]byte(raw))
	if err == nil {
		t.Error("expected error for no candidates")
	}
}

func TestParseResponseWithFunctionCall(t *testing.T) {
	raw := `{
		"candidates": [{
			"content": {
				"parts": [{
					"functionCall": {
						"name": "search",
						"args": {"query": "test"}
					}
				}]
			}
		}],
		"usageMetadata": {"totalTokenCount": 10}
	}`

	resp, err := parseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(resp.FunctionCalls) != 1 {
		t.Fatalf("expected 1 function call, got %d", len(resp.FunctionCalls))
	}
	if resp.FunctionCalls[0].Name != "search" {
		t.Errorf("function call name = %q", resp.FunctionCalls[0].Name)
	}
}

func TestBuildRequestBody(t *testing.T) {
	req := GenerateRequest{
		SystemPrompt:  "You are helpful",
		Messages:      []Message{{Role: "user", Content: "Hi"}},
		ThinkingLevel: "low",
	}
	body, err := buildRequestBody(req, 2048)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["systemInstruction"] == nil {
		t.Error("expected systemInstruction in body")
	}
	contents := parsed["contents"].([]any)
	if len(contents) != 1 {
		t.Errorf("expected 1 content, got %d", len(contents))
	}
}

func TestThinkingBudgetFor(t *testing.T) {
	tests := []struct {
		level string
		want  int
	}{
		{"none", 0},
		{"low", 2048},
		{"medium", 8192},
		{"high", 32768},
		{"", 8192}, // default is medium
	}
	for _, tt := range tests {
		got := thinkingBudgetFor(tt.level)
		if got != tt.want {
			t.Errorf("thinkingBudgetFor(%q) = %d, want %d", tt.level, got, tt.want)
		}
	}
}
