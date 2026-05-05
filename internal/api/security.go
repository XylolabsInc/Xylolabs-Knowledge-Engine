package api

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	urlpkg "net/url"
	"path/filepath"
	"strings"
	"time"
)

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newRequestID() string {
	id, err := randomHex(8)
	if err == nil {
		return id
	}
	return time.Now().UTC().Format("20060102150405.000000000")
}

func requestWantsSecureCookies(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}

	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		return false
	}
	return strings.EqualFold(strings.Split(proto, ",")[0], "https")
}

func requestScheme(r *http.Request) string {
	if requestWantsSecureCookies(r) {
		return "https"
	}
	return "http"
}

func isSameOrigin(r *http.Request, origin string) bool {
	if r == nil || origin == "" {
		return false
	}
	u, err := urlpkg.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, requestScheme(r)) && strings.EqualFold(u.Host, r.Host)
}

func isTrustedLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}

	remoteHost := splitHostPortLoose(r.RemoteAddr)
	remoteIP := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remoteIP == nil || !remoteIP.IsLoopback() {
		return false
	}

	host := splitHostPortLoose(r.Host)
	if strings.EqualFold(host, "localhost") {
		return true
	}

	hostIP := net.ParseIP(strings.Trim(host, "[]"))
	return hostIP != nil && hostIP.IsLoopback()
}

func splitHostPortLoose(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return host
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return strings.Trim(value, "[]")
	}
	return value
}

func pathWithinBase(basePath, targetPath string) bool {
	rel, err := filepath.Rel(basePath, targetPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isAllowedKBRepoPath(relPath string) bool {
	relPath = filepath.Clean(relPath)
	if relPath == "." || filepath.IsAbs(relPath) || !strings.HasSuffix(strings.ToLower(relPath), ".md") {
		return false
	}

	for _, part := range strings.Split(relPath, string(filepath.Separator)) {
		switch {
		case part == "", part == ".":
			continue
		case strings.HasPrefix(part, "."):
			return false
		case part == "_meta":
			return false
		}
	}

	return true
}
