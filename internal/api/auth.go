package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "xke_session"
	sessionTTL        = 14 * 24 * time.Hour // 2 weeks
	cleanupInterval   = 15 * time.Minute

	csrfTokenLength = 32
	csrfHeaderName  = "X-CSRF-Token"
)

type session struct {
	username  string
	csrfToken string
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

func (am *authMiddleware) createSession(username string) (string, string, error) {
	id, err := randomHex(32)
	if err != nil {
		return "", "", err
	}
	csrfToken, err := am.generateCSRFToken()
	if err != nil {
		return "", "", err
	}
	am.mu.Lock()
	am.sessions[id] = &session{username: username, csrfToken: csrfToken, createdAt: time.Now()}
	am.mu.Unlock()
	return id, csrfToken, nil
}

func (am *authMiddleware) getSession(id string) (*session, bool) {
	am.mu.RLock()
	s, ok := am.sessions[id]
	am.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(s.createdAt) > sessionTTL {
		am.deleteSession(id)
		return nil, false
	}
	return s, true
}

func (am *authMiddleware) validateSession(id string) bool {
	_, ok := am.getSession(id)
	return ok
}

func (am *authMiddleware) deleteSession(id string) {
	am.mu.Lock()
	delete(am.sessions, id)
	am.mu.Unlock()
}

func (am *authMiddleware) generateCSRFToken() (string, error) {
	return randomHex(csrfTokenLength)
}

func (am *authMiddleware) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	usernameMatch := subtle.ConstantTimeCompare([]byte(req.Username), []byte(am.username)) == 1
	passwordMatch := subtle.ConstantTimeCompare([]byte(req.Password), []byte(am.password)) == 1

	if !usernameMatch || !passwordMatch {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	sessionID, csrfToken, err := am.createSession(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	expiresAt := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
		Expires:  expiresAt,
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
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (am *authMiddleware) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_required": false,
			"authenticated": true,
		})
		return
	}
	if isTrustedLoopbackRequest(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_required": true,
			"authenticated": true,
		})
		return
	}

	authenticated := false
	csrfToken := ""
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if session, ok := am.getSession(cookie.Value); ok {
			authenticated = true
			csrfToken = session.csrfToken
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": true,
		"authenticated": authenticated,
		"csrf_token":    csrfToken,
	})
}

// requireAuth wraps a handler and returns 401 if the user is not authenticated.
func (am *authMiddleware) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !am.enabled {
			next(w, r)
			return
		}
		// Allow only direct loopback requests without auth.
		if isTrustedLoopbackRequest(r) {
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
		// Allow only direct loopback requests without CSRF.
		if isTrustedLoopbackRequest(r) {
			next(w, r)
			return
		}
		sessionCookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		session, ok := am.getSession(sessionCookie.Value)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		headerToken := r.Header.Get(csrfHeaderName)
		if headerToken == "" {
			writeError(w, http.StatusForbidden, "CSRF token header missing")
			return
		}
		if subtle.ConstantTimeCompare([]byte(session.csrfToken), []byte(headerToken)) != 1 {
			writeError(w, http.StatusForbidden, "CSRF token mismatch")
			return
		}
		next(w, r)
	}
}
