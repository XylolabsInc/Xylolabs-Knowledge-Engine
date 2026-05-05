package attachment

import "testing"

func TestValidateSourceURL(t *testing.T) {
	t.Parallel()

	if err := validateSourceURL("https://example.com/file.pdf"); err != nil {
		t.Fatalf("expected https URL to be allowed: %v", err)
	}
	if err := validateSourceURL("file:///etc/passwd"); err == nil {
		t.Fatal("expected non-http URL to be rejected")
	}
}

func TestDocumentPrefix(t *testing.T) {
	t.Parallel()

	if got := documentPrefix("abcdef123456"); got != "abcdef12" {
		t.Fatalf("documentPrefix() = %q, want %q", got, "abcdef12")
	}
	if got := documentPrefix("abc"); got != "abc" {
		t.Fatalf("documentPrefix() = %q, want %q", got, "abc")
	}
	if got := documentPrefix(""); got != "unknown" {
		t.Fatalf("documentPrefix() = %q, want %q", got, "unknown")
	}
}
