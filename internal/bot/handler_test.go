package bot

import (
	"strings"
	"testing"
)

func TestEmptyResponseFallbackLanguage(t *testing.T) {
	// A Korean-speaking workspace must not receive an English apology when the
	// model returns nothing.
	for _, tc := range []struct {
		language string
		wantKo   bool
	}{
		{"ko", true},
		{"KO", true},
		{"en", false},
		{"", false},
	} {
		b := &Bot{language: tc.language}
		got := b.emptyResponseFallback()
		if got == "" {
			t.Fatalf("language %q: fallback must never be empty", tc.language)
		}
		isKo := strings.Contains(got, "죄송")
		if isKo != tc.wantKo {
			t.Errorf("language %q: korean=%v, want %v (got %q)", tc.language, isKo, tc.wantKo, got)
		}
	}
}
