// Package api provides the REST API server for controlling the crawler
// and querying the search index from external tools and frontends.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/crawler"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// ServerMode determines which routes are registered.
type ServerMode string

const (
	// ModeSearch registers read-only search endpoints (no crawler control).
	ModeSearch ServerMode = "search"
	// ModeCrawler registers crawler control endpoints (crawl, queue, seeds, etc.).
	ModeCrawler ServerMode = "crawler"
)

// Server wraps the HTTP server and its dependencies.
type Server struct {
	httpServer *http.Server
	handlers   *Handlers
	logger     *slog.Logger
}

// NewServer creates a new API server with routes registered based on mode.
func NewServer(
	cfg *config.APIConfig,
	store storage.Storage,
	engine *crawler.Engine,
	front *frontier.Frontier,
	mode ServerMode,
	logger *slog.Logger,
) *Server {
	handlers := NewHandlers(store, engine, front, logger)
	apiLogger := logger.With("component", "api-server")

	mux := http.NewServeMux()

	// Health is always registered.
	mux.HandleFunc("GET /api/v1/health", handlers.HealthCheck)

	// Stats is always registered (response differs by mode).
	mux.HandleFunc("GET /api/v1/stats", handlers.GetStats)

	switch mode {
	case ModeSearch:
		registerSearchRoutes(mux, handlers)
	case ModeCrawler:
		registerCrawlerRoutes(mux, handlers)
	}

	// Apply middleware stack.
	handler := chain(
		mux,
		RecoveryMiddleware(apiLogger),
		SecurityHeadersMiddleware,
		RateLimitMiddleware(apiLogger),
		LoggingMiddleware(apiLogger),
		CORSMiddleware,
		RequestIDMiddleware,
	)

	httpServer := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return &Server{
		httpServer: httpServer,
		handlers:   handlers,
		logger:     apiLogger,
	}
}

// registerSearchRoutes adds read-only endpoints for the Search API node.
func registerSearchRoutes(mux *http.ServeMux, h *Handlers) {
	// Search
	mux.HandleFunc("GET /api/v1/search", h.Search)
	mux.HandleFunc("GET /api/v1/images/search", h.SearchImages)
	mux.HandleFunc("POST /api/v1/interact", h.Interact)

	// Pages
	mux.HandleFunc("GET /api/v1/pages/{id}", h.GetPage)
	mux.HandleFunc("GET /api/v1/pages/lookup", h.LookupPage)
	mux.HandleFunc("GET /api/v1/pages/{id}/outlinks", h.GetOutlinks)

	// Backlinks
	mux.HandleFunc("GET /api/v1/backlinks", h.GetBacklinks)
}

// registerCrawlerRoutes adds crawler control endpoints for the Crawler node.
func registerCrawlerRoutes(mux *http.ServeMux, h *Handlers) {
	// Crawl
	mux.HandleFunc("POST /api/v1/crawl", h.CrawlURLs)

	// Queue
	mux.HandleFunc("GET /api/v1/queue", h.GetQueue)

	// Seeds
	mux.HandleFunc("GET /api/v1/seeds", h.ListSeeds)
	mux.HandleFunc("POST /api/v1/seeds", h.AddSeed)
	mux.HandleFunc("DELETE /api/v1/seeds/{url}", h.DeleteSeed)

	// Crawler control
	mux.HandleFunc("POST /api/v1/crawler/start", h.StartCrawler)
	mux.HandleFunc("POST /api/v1/crawler/stop", h.StopCrawler)
	mux.HandleFunc("GET /api/v1/crawler/status", h.CrawlerStatus)

	// Interests
	mux.HandleFunc("GET /api/v1/interests", h.ListInterests)
	mux.HandleFunc("POST /api/v1/interests", h.AddInterest)
	mux.HandleFunc("DELETE /api/v1/interests/{term}", h.DeleteInterest)

	// Ingest (extension page push)
	mux.HandleFunc("POST /api/v1/ingest/page", h.IngestPage)
	mux.HandleFunc("POST /api/v1/ingest/batch", h.IngestBatch)

	// Maintenance
	mux.HandleFunc("POST /api/v1/maintenance/rank", h.UpdateRankings)
}

// Start begins listening for HTTP requests. This method blocks until
// the server is shut down. Returns http.ErrServerClosed on graceful shutdown.
func (s *Server) Start() error {
	s.logger.Info("API server starting",
		"addr", s.httpServer.Addr,
	)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server with a timeout.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("API server shutting down")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// Addr returns the address the server is configured to listen on.
func (s *Server) Addr() string {
	return fmt.Sprintf("http://%s", s.httpServer.Addr)
}
