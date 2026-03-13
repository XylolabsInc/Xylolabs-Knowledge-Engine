package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "xke_session"
	sessionTTL        = 14 * 24 * time.Hour // 2 weeks
	cleanupInterval   = 15 * time.Minute

	csrfTokenLength = 32
	csrfCookieName  = "xke_csrf"
	csrfHeaderName  = "X-CSRF-Token"
)

type session struct {
	username  string
	createdAt time.Time
}

type authMiddleware struct {
	username string
	password string
	enabled  bool

	mu       sync.RWMutex
	sessions map[string]*session

	done chan struct{}
}

func newAuthMiddleware(username, password string) *authMiddleware {
	am := &authMiddleware{
		username: username,
		password: password,
		enabled:  password != "",
		sessions: make(map[string]*session),
		done:     make(chan struct{}),
	}
	if am.enabled {
		go am.cleanupLoop()
	}
	return am
}

func (am *authMiddleware) stop() {
	close(am.done)
}

func (am *authMiddleware) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-am.done:
			return
		case <-ticker.C:
			am.cleanup()
		}
	}
}

func (am *authMiddleware) cleanup() {
	am.mu.Lock()
	defer am.mu.Unlock()
	now := time.Now()
	for id, s := range am.sessions {
		if now.Sub(s.createdAt) > sessionTTL {
			delete(am.sessions, id)
		}
	}
}

func (am *authMiddleware) createSession(username string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	am.mu.Lock()
	am.sessions[id] = &session{username: username, createdAt: time.Now()}
	am.mu.Unlock()
	return id, nil
}

func (am *authMiddleware) validateSession(id string) bool {
	am.mu.RLock()
	s, ok := am.sessions[id]
	am.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Since(s.createdAt) <= sessionTTL
}

func (am *authMiddleware) deleteSession(id string) {
	am.mu.Lock()
	delete(am.sessions, id)
	am.mu.Unlock()
}

func (am *authMiddleware) generateCSRFToken() string {
	b := make([]byte, csrfTokenLength)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (am *authMiddleware) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	usernameMatch := subtle.ConstantTimeCompare([]byte(req.Username), []byte(am.username)) == 1
	passwordMatch := subtle.ConstantTimeCompare([]byte(req.Password), []byte(am.password)) == 1

	if !usernameMatch || !passwordMatch {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	sessionID, err := am.createSession(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	csrfToken := am.generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: false, // JS needs to read this
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": csrfToken})
}

func (am *authMiddleware) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		am.deleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (am *authMiddleware) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_required":  false,
			"authenticated": true,
		})
		return
	}

	authenticated := false
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		authenticated = am.validateSession(cookie.Value)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required":  true,
		"authenticated": authenticated,
	})
}

// requireAuth wraps a handler and returns 401 if the user is not authenticated.
func (am *authMiddleware) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !am.enabled {
			next(w, r)
			return
		}
		// Allow localhost access without auth (internal scripts)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "127.0.0.1" || host == "::1" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !am.validateSession(cookie.Value) {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

// requireCSRF validates the CSRF token on state-changing requests.
func (am *authMiddleware) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !am.enabled {
			next(w, r)
			return
		}
		// Only validate on state-changing methods
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		// Allow localhost without CSRF (internal scripts)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "127.0.0.1" || host == "::1" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie(csrfCookieName)
		if err != nil {
			writeError(w, http.StatusForbidden, "CSRF token missing")
			return
		}
		headerToken := r.Header.Get(csrfHeaderName)
		if headerToken == "" {
			writeError(w, http.StatusForbidden, "CSRF token header missing")
			return
		}
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(headerToken)) != 1 {
			writeError(w, http.StatusForbidden, "CSRF token mismatch")
			return
		}
		next(w, r)
	}
}
