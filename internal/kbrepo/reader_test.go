package kbrepo

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// A fact recorded in a detail file that no index links to must still be
// reachable: the index layer only ever references a small slice of the repo.
func TestBuildContextFindsFactInUnlinkedDetailFile(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"indexes/topics.md":     "# Topics\n\n## Funding\n- [E-SPARK](../google/docs/espark.md)\n",
		"google/docs/espark.md": "# E-SPARK\n지원사업 일정입니다.\n",
		"slack/channels/04-mgmt/2026-06-18.md": "# 경영지원\n" +
			"법인 명의 회선 개통 완료. 네이버 지도 대표번호를 메인 번호(010-2997-7077)로 변경했습니다.\n",
	})

	ctx, err := r.BuildContext("대표번호 알려줘")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx, "010-2997-7077") {
		t.Errorf("BuildContext() did not surface the fact from the unlinked detail file:\n%s", ctx)
	}
}

// Korean attaches particles to nouns and compounds nouns without spaces, so a
// query eojeol rarely equals the form stored in the KB.
func TestQueryTermsExpandsHangulFragments(t *testing.T) {
	terms := queryTerms("대표전화번호가")

	byText := make(map[string]float64, len(terms))
	for _, term := range terms {
		byText[term.text] = term.weight
	}

	if w := byText["대표전화번호가"]; w != 1.0 {
		t.Errorf("surface token weight = %v, want 1.0", w)
	}
	for _, want := range []string{"대표", "번호", "전화번호"} {
		w, ok := byText[want]
		if !ok {
			t.Errorf("fragment %q missing from %v", want, terms)
			continue
		}
		if w >= 1.0 {
			t.Errorf("fragment %q weight = %v, want less than the surface token", want, w)
		}
	}

	// Latin tokens are left alone — they need no fragment matching.
	for _, term := range queryTerms("registration") {
		if term.text != "registration" {
			t.Errorf("unexpected fragment %q for a Latin token", term.text)
		}
	}
}

// The index layer is always included and grows with every channel added. Once
// it fills the budget on its own, a plain tail-truncation drops the detail
// layer entirely — the one part selected because it answers this query.
func TestBuildContextReservesRoomForDetails(t *testing.T) {
	files := map[string]string{
		"slack/channels/04-mgmt/2026-06-18.md": "# 경영지원\n대표번호를 010-2997-7077로 변경했습니다.\n",
	}
	// Index content large enough to fill the whole budget by itself.
	for i := range 12 {
		files[fmt.Sprintf("slack/channels/ch%02d/README.md", i)] =
			fmt.Sprintf("# 채널 %d\n%s\n", i, strings.Repeat("배경 설명 문장입니다. ", 400))
	}

	r := newTestRepo(t, files)
	r.maxContextBytes = 20000

	ctx, err := r.BuildContext("대표번호")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(ctx, "010-2997-7077") {
		t.Error("BuildContext() dropped the detail layer to fit the index layer")
	}
	if len(ctx) > r.maxContextBytes {
		t.Errorf("BuildContext() returned %d bytes, over the %d budget", len(ctx), r.maxContextBytes)
	}
	if !utf8.ValidString(ctx) {
		t.Error("BuildContext() split a rune while truncating")
	}
}

func TestTruncateUTF8(t *testing.T) {
	// "한" is 3 bytes; cutting at 4 must back off to the rune boundary.
	if got := truncateUTF8("한국어", 4); got != "한" {
		t.Errorf("truncateUTF8() = %q, want %q", got, "한")
	}
	if got := truncateUTF8("한국어", 99); got != "한국어" {
		t.Errorf("truncateUTF8() = %q, want the input unchanged", got)
	}
	if got := truncateUTF8("한국어", 0); got != "" {
		t.Errorf("truncateUTF8() = %q, want empty", got)
	}
}
