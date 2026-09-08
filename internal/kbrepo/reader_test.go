package kbrepo

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// newTestRepo lays out a minimal KB repo on disk and returns a Reader for it
// with git pull suppressed.
func newTestRepo(t *testing.T, files map[string]string) *Reader {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Keep Pull() from shelling out to git during tests.
	r.lastPull = time.Now()
	return r
}

// listDetailFiles walks from ".", whose filepath.Base is also "." — a
// hidden-directory guard that did not exempt the walk root skipped the whole
// repo, so nothing outside indexes/ was ever searchable.
func TestListDetailFilesWalksRepoRoot(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"indexes/topics.md":                    "# Topics\n",
		"slack/channels/04-mgmt/2026-06-18.md": "# Mgmt\n대표번호를 변경했습니다.\n",
		"user-provided/2026-03-13-info.md":     "# Info\n",
		"README.md":                            "# Repo\n",
		".git/config":                          "[core]\n",
		"_meta/sync-state.json":                "{}\n",
	})

	files := r.listDetailFiles()
	if len(files) != 2 {
		t.Fatalf("listDetailFiles() = %v, want the 2 detail files", files)
	}
	for _, f := range files {
		if strings.HasPrefix(f, "indexes/") || strings.HasPrefix(f, "_meta/") || strings.HasPrefix(f, ".git/") {
			t.Errorf("listDetailFiles() returned excluded path %q", f)
		}
	}
}
