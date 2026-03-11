package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
	"github.com/xylolabsinc/xylolabs-kb/internal/worker"
)

// Server is the HTTP API server for the knowledge base.
type Server struct {
	httpServer  *http.Server
	engine      *kb.Engine
	store       kb.Storage
	scheduler   *worker.Scheduler
	syncManager *worker.SyncManager
	logger      *slog.Logger
}

// NewServer creates an API server.
func NewServer(host string, port int, engine *kb.Engine, store kb.Storage, scheduler *worker.Scheduler, syncManager *worker.SyncManager, logger *slog.Logger) *Server {
	s := &Server{
		engine:      engine,
		store:       store,
		scheduler:   scheduler,
		syncManager: syncManager,
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
