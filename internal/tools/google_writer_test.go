package tools

import (
	"strings"
	"testing"
)

func TestMarkdownToHTMLEscapesDangerousContent(t *testing.T) {
	t.Parallel()

	html := markdownToHTML("# Title\n\n<script>alert(1)</script>\n\n[click me](javascript:alert(1))\n\n`<b>safe</b>`")

	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("expected raw script tag to be escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("expected escaped script content in output")
	}
	if !strings.Contains(html, `href="#"`) {
		t.Fatal("expected unsafe javascript link to be neutralized")
	}
	if !strings.Contains(html, "<code>&lt;b&gt;safe&lt;/b&gt;</code>") {
		t.Fatal("expected inline code to stay escaped")
	}
}

func TestSanitizeGoogleHTMLURL(t *testing.T) {
	t.Parallel()

	if got := sanitizeGoogleHTMLURL("https://example.com/path?q=1&x=2"); !strings.Contains(got, "https://example.com/path?q=1&amp;x=2") {
		t.Fatalf("expected https URL to remain allowed, got %q", got)
	}
	if got := sanitizeGoogleHTMLURL("javascript:alert(1)"); got != "#" {
		t.Fatalf("expected javascript URL to be rejected, got %q", got)
	}
}
