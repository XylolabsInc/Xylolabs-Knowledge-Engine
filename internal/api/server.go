package api

import (
	_ "embed"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

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
	httpServer  *http.Server
	engine      *kb.Engine
	store       kb.Storage
	scheduler   *worker.Scheduler
	syncManager *worker.SyncManager
	auth        *authMiddleware
	jobLister   JobLister
	kbRepoDir   string
	logger      *slog.Logger
}

// NewServer creates an API server.
func NewServer(host string, port int, engine *kb.Engine, store kb.Storage, scheduler *worker.Scheduler, syncManager *worker.SyncManager, jobLister JobLister, consoleUsername, consolePassword, kbRepoDir string, logger *slog.Logger) *Server {
	auth := newAuthMiddleware(consoleUsername, consolePassword)
	s := &Server{
		engine:      engine,
		store:       store,
		scheduler:   scheduler,
		syncManager: syncManager,
		auth:        auth,
		jobLister:   jobLister,
		kbRepoDir:   kbRepoDir,
		logger:      logger.With("component", "api-server"),
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
	mux.HandleFunc("GET /api/v1/search", s.handleSearch)
	mux.HandleFunc("GET /api/v1/documents", s.handleListDocuments)
	mux.HandleFunc("GET /api/v1/documents/{id}", s.handleGetDocument)
	mux.HandleFunc("GET /api/v1/stats", s.handleGetStats)
	mux.HandleFunc("GET /api/v1/sources", s.handleListSources)
	mux.HandleFunc("POST /api/v1/sync/{source}", s.handleTriggerSync)

	// Root redirect to console
	mux.HandleFunc("GET /", s.handleRootRedirect)

	// Console routes
	mux.HandleFunc("GET /console", s.handleConsole)
	mux.HandleFunc("POST /console/login", s.auth.handleLogin)
	mux.HandleFunc("POST /console/logout", s.auth.handleLogout)
	mux.HandleFunc("GET /console/auth/check", s.auth.handleAuthCheck)

	// Console-protected API
	mux.HandleFunc("GET /api/v1/jobs", s.auth.requireAuth(s.handleListJobs))

	// KB repo browsing (auth-protected)
	mux.HandleFunc("GET /api/v1/kb/tree", s.auth.requireAuth(s.handleKBTree))
	mux.HandleFunc("GET /api/v1/kb/file", s.auth.requireAuth(s.handleKBFile))
}

func (s *Server) handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/console", http.StatusFound)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Wrap response writer to capture status code
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		s.logger.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration", time.Since(start),
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
