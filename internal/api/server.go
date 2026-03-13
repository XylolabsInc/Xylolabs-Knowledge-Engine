package api

import (
	_ "embed"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
	"github.com/xylolabsinc/xylolabs-kb/internal/worker"
)

//go:embed console.html
var consoleHTML []byte

// JobLister provides access to scheduled jobs for the console.
type JobLister interface {
	ListScheduledJobs() ([]storage.ScheduledJob, error)
}

// Server is the HTTP API server for the knowledge base.
type Server struct {
	httpServer   *http.Server
	engine       *kb.Engine
	store        kb.Storage
	scheduler    *worker.Scheduler
	syncManager  *worker.SyncManager
	auth         *authMiddleware
	jobLister    JobLister
	kbRepoDir    string
	extractor    *extractor.Extractor
	logger       *slog.Logger
	startedAt    time.Time
	requestCount atomic.Int64
	errorCount   atomic.Int64
}

// NewServer creates an API server.
func NewServer(host string, port int, engine *kb.Engine, store kb.Storage, scheduler *worker.Scheduler, syncManager *worker.SyncManager, jobLister JobLister, consoleUsername, consolePassword, kbRepoDir string, ext *extractor.Extractor, logger *slog.Logger) *Server {
	auth := newAuthMiddleware(consoleUsername, consolePassword)
	s := &Server{
		engine:      engine,
		store:       store,
		scheduler:   scheduler,
		syncManager: syncManager,
		auth:        auth,
		jobLister:   jobLister,
		kbRepoDir:   kbRepoDir,
		extractor:   ext,
		logger:      logger.With("component", "api-server"),
		startedAt:   time.Now(),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		Handler:      s.withMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	s.logger.Info("api server starting", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("api server shutting down")
	if s.auth != nil {
		s.auth.stop()
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/v1/search", s.auth.requireAuth(s.handleSearch))
	mux.HandleFunc("GET /api/v1/documents", s.auth.requireAuth(s.handleListDocuments))
	mux.HandleFunc("GET /api/v1/documents/{id}", s.auth.requireAuth(s.handleGetDocument))
	mux.HandleFunc("GET /api/v1/stats", s.auth.requireAuth(s.handleGetStats))
	mux.HandleFunc("GET /api/v1/sources", s.auth.requireAuth(s.handleListSources))
	mux.HandleFunc("POST /api/v1/sync/{source}", s.auth.requireAuth(s.auth.requireCSRF(s.handleTriggerSync)))
	mux.HandleFunc("POST /api/v1/documents", s.auth.requireAuth(s.auth.requireCSRF(s.handleCreateDocument)))
	mux.HandleFunc("PUT /api/v1/documents/{id}", s.auth.requireAuth(s.auth.requireCSRF(s.handleUpdateDocument)))
	mux.HandleFunc("DELETE /api/v1/documents/{id}", s.auth.requireAuth(s.auth.requireCSRF(s.handleDeleteDocument)))

	// Root redirect to console
	mux.HandleFunc("GET /", s.handleRootRedirect)

	// Console routes
	mux.HandleFunc("GET /console", s.handleConsole)
	mux.HandleFunc("POST /console/login", s.auth.handleLogin)
	mux.HandleFunc("POST /console/logout", s.auth.requireCSRF(s.auth.handleLogout))
	mux.HandleFunc("GET /console/auth/check", s.auth.handleAuthCheck)

	// Console-protected API
	mux.HandleFunc("GET /api/v1/jobs", s.auth.requireAuth(s.handleListJobs))

	// KB repo browsing (auth-protected)
	mux.HandleFunc("GET /api/v1/kb/tree", s.auth.requireAuth(s.handleKBTree))
	mux.HandleFunc("GET /api/v1/kb/file", s.auth.requireAuth(s.handleKBFile))

	// KB database documents browsing (auth-protected)
	mux.HandleFunc("GET /api/v1/kb/docs/tree", s.auth.requireAuth(s.handleKBDocTree))
	mux.HandleFunc("GET /api/v1/kb/docs/file", s.auth.requireAuth(s.handleKBDocFile))
}

func (s *Server) handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/console", http.StatusFound)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	type metricsResponse struct {
		UptimeSeconds  float64   `json:"uptime_seconds"`
		GoroutineCount int       `json:"goroutine_count"`
		RequestCount   int64     `json:"request_count"`
		ErrorCount     int64     `json:"error_count"`
		StartedAt      time.Time `json:"started_at"`
	}
	resp := metricsResponse{
		UptimeSeconds:  time.Since(s.startedAt).Seconds(),
		GoroutineCount: runtime.NumGoroutine(),
		RequestCount:   s.requestCount.Load(),
		ErrorCount:     s.errorCount.Load(),
		StartedAt:      s.startedAt,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Generate unique request ID
		var buf [8]byte
		rand.Read(buf[:])
		requestID := hex.EncodeToString(buf[:])

		// CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, X-Request-ID")
		w.Header().Set("X-Request-ID", requestID)

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		s.requestCount.Add(1)

		// Wrap response writer to capture status code
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		if wrapped.status >= 400 {
			s.errorCount.Add(1)
		}

		s.logger.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration", time.Since(start),
			"request_id", requestID,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
