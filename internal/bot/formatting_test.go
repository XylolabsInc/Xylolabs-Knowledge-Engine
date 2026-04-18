package bot

import (
	"testing"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

func TestMergeConsecutiveRolesEmpty(t *testing.T) {
	result := mergeConsecutiveRoles(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestMergeConsecutiveRolesSingle(t *testing.T) {
	msgs := []gemini.Message{{Role: "user", Content: "hello"}}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Content != "hello" {
		t.Errorf("expected 'hello', got %q", result[0].Content)
	}
}

func TestMergeConsecutiveRolesAlternating(t *testing.T) {
	msgs := []gemini.Message{
		{Role: "user", Content: "hello"},
		{Role: "model", Content: "hi"},
		{Role: "user", Content: "how are you"},
	}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (already alternating), got %d", len(result))
	}
}

func TestMergeConsecutiveRolesMergesContent(t *testing.T) {
	msgs := []gemini.Message{
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "world"},
	}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(result))
	}
	if result[0].Content != "hello\n\nworld" {
		t.Errorf("expected 'hello\\n\\nworld', got %q", result[0].Content)
	}
}

func TestMergeConsecutiveRolesMergesFunctionCalls(t *testing.T) {
	msgs := []gemini.Message{
		{Role: "model", FunctionCalls: []gemini.FunctionCall{{Name: "search_drive"}}},
		{Role: "model", Content: "I found the file"},
	}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(result))
	}
	if len(result[0].FunctionCalls) != 1 {
		t.Errorf("expected 1 function call, got %d", len(result[0].FunctionCalls))
	}
	if result[0].Content != "I found the file" {
		t.Errorf("expected content preserved, got %q", result[0].Content)
	}
}

func TestMergeConsecutiveRolesMergesFunctionResponses(t *testing.T) {
	msgs := []gemini.Message{
		{Role: "user", FunctionResponses: []gemini.FunctionResponse{{Name: "search_drive"}}},
		{Role: "user", Content: "additional info"},
	}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(result))
	}
	if len(result[0].FunctionResponses) != 1 {
		t.Errorf("expected 1 function response, got %d", len(result[0].FunctionResponses))
	}
	if result[0].Content != "additional info" {
		t.Errorf("expected content 'additional info', got %q", result[0].Content)
	}
}

func TestMergeConsecutiveRolesMergesImages(t *testing.T) {
	msgs := []gemini.Message{
		{Role: "user", Images: []gemini.Image{{MimeType: "image/png", Data: []byte("a")}}},
		{Role: "user", Images: []gemini.Image{{MimeType: "image/jpeg", Data: []byte("b")}}},
	}
	result := mergeConsecutiveRoles(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(result))
	}
	if len(result[0].Images) != 2 {
		t.Errorf("expected 2 images, got %d", len(result[0].Images))
	}
}

func TestIsCreationTask(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"보고서 만들어", true},
		{"create a document", true},
		{"write a report", true},
		{"what is the weather", false},
		{"search for files", false},
		{"문서 작성해 줘", true},
	}

	for _, tt := range tests {
		got := isCreationTask(tt.query)
		if got != tt.want {
			t.Errorf("isCreationTask(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestIsComplexQuery(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"분석해 줘", true},
		{"compare A and B", true},
		{"hello", false},
		{"find the file", false},
	}

	for _, tt := range tests {
		got := isComplexQuery(tt.query)
		if got != tt.want {
			t.Errorf("isComplexQuery(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}
