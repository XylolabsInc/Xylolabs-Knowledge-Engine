package api

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestIsTrustedLoopbackRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		req    *http.Request
		expect bool
	}{
		{
			name: "direct localhost request is trusted",
			req: &http.Request{
				RemoteAddr: "127.0.0.1:12345",
				Host:       "localhost:8080",
				Header:     make(http.Header),
			},
			expect: true,
		},
		{
			name: "loopback remote with public host is rejected",
			req: &http.Request{
				RemoteAddr: "127.0.0.1:12345",
				Host:       "kb.example.com",
				Header:     make(http.Header),
			},
			expect: false,
		},
		{
			name: "forwarded loopback request is rejected",
			req: &http.Request{
				RemoteAddr: "127.0.0.1:12345",
				Host:       "localhost:8080",
				Header:     http.Header{"X-Forwarded-For": []string{"203.0.113.10"}},
			},
			expect: false,
		},
		{
			name: "remote public address is rejected",
			req: &http.Request{
				RemoteAddr: "203.0.113.10:443",
				Host:       "localhost:8080",
				Header:     make(http.Header),
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTrustedLoopbackRequest(tt.req); got != tt.expect {
				t.Fatalf("isTrustedLoopbackRequest() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestRequestWantsSecureCookies(t *testing.T) {
	t.Parallel()

	httpsReq := &http.Request{TLS: &tls.ConnectionState{}, Header: make(http.Header)}
	if !requestWantsSecureCookies(httpsReq) {
		t.Fatal("expected TLS request to require secure cookies")
	}

	proxyReq := &http.Request{Header: http.Header{"X-Forwarded-Proto": []string{"https"}}}
	if !requestWantsSecureCookies(proxyReq) {
		t.Fatal("expected X-Forwarded-Proto=https to require secure cookies")
	}

	httpReq := &http.Request{Header: make(http.Header)}
	if requestWantsSecureCookies(httpReq) {
		t.Fatal("expected plain HTTP request to not require secure cookies")
	}
}

func TestKBRepoPathRestrictions(t *testing.T) {
	t.Parallel()

	if !isAllowedKBRepoPath("docs/guide.md") {
		t.Fatal("expected normal markdown path to be allowed")
	}
	if isAllowedKBRepoPath(".git/config") {
		t.Fatal("expected hidden git path to be rejected")
	}
	if isAllowedKBRepoPath("_meta/secrets.md") {
		t.Fatal("expected _meta path to be rejected")
	}
	if isAllowedKBRepoPath("docs/guide.txt") {
		t.Fatal("expected non-markdown path to be rejected")
	}
}

func TestPathWithinBase(t *testing.T) {
	t.Parallel()

	base := "/tmp/repo"
	if !pathWithinBase(base, "/tmp/repo/docs/file.md") {
		t.Fatal("expected child path to be inside base")
	}
	if pathWithinBase(base, "/tmp/repo-sibling/docs/file.md") {
		t.Fatal("expected sibling path to be outside base")
	}
}
