package kbrepo

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello World", "hello-world"},
		{"  spaces  ", "spaces"},
		{"한글 테스트", "한글-테스트"},
		{"file.name", "filename"},
		{"a/b\\c", "abc"},
		{"UPPERCASE", "uppercase"},
		{"multiple---hyphens", "multiple-hyphens"},
		{"", ""},
	}

	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.expected {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeCommitString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"normal text", "normal text"},
		{"line1\nline2", "line1 line2"},
		{"line1\r\nline2", "line1 line2"},
		{"cr\ralso", "cralso"},
		{"\n\nleading newlines", "  leading newlines"},
	}

	for _, tt := range tests {
		got := sanitizeCommitString(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeCommitString(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractDatePatternsWithExplicitYear(t *testing.T) {
	patterns := extractDatePatterns("2025년 6월 회의록")
	found := false
	for _, p := range patterns {
		if p == "2025-06" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pattern 2025-06 in %v", patterns)
	}
}

func TestExtractDatePatternsNoYear(t *testing.T) {
	// Without explicit year, should use current year
	patterns := extractDatePatterns("3월 회의록")
	if len(patterns) == 0 {
		t.Fatal("expected at least one pattern")
	}
	// Should start with current year
	if patterns[0][4:5] != "-" {
		t.Errorf("expected YYYY-MM format, got %q", patterns[0])
	}
}

func TestExtractDatePatternsNoMatch(t *testing.T) {
	patterns := extractDatePatterns("no date here")
	if len(patterns) != 0 {
		t.Errorf("expected no patterns, got %v", patterns)
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			"heading",
			"# My Title\n\nSome content",
			"My Title",
		},
		{
			"no heading",
			"Just some text without a heading",
			"",
		},
		{
			"empty content",
			"",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTitle(tt.content)
			if got != tt.expected {
				t.Errorf("extractTitle() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  int // minimum number of tokens expected
	}{
		{"hello world", 2},
		{"한글 테스트", 2},
		{"hello-world", 2}, // split on hyphen
		{"UPPERCASE", 1},
		{"", 0},
	}

	for _, tt := range tests {
		tokens := tokenize(tt.input)
		if len(tokens) < tt.want {
			t.Errorf("tokenize(%q) = %v, want at least %d tokens", tt.input, tokens, tt.want)
		}
	}
}
