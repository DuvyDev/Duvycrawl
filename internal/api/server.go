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

// Server wraps the HTTP server and its dependencies.
type Server struct {
	httpServer *http.Server
	handlers   *Handlers
	logger     *slog.Logger
}

// NewServer creates a new API server with all routes registered.
func NewServer(
	cfg *config.APIConfig,
	store storage.Storage,
	engine *crawler.Engine,
	front *frontier.Frontier,
	logger *slog.Logger,
) *Server {
	handlers := NewHandlers(store, engine, front, logger)
	apiLogger := logger.With("component", "api-server")

	mux := http.NewServeMux()

	// --- Register routes ---

	// Health
	mux.HandleFunc("GET /api/v1/health", handlers.HealthCheck)

	// Search
	mux.HandleFunc("GET /api/v1/search", handlers.Search)
	mux.HandleFunc("GET /api/v1/images/search", handlers.SearchImages)

	// Pages
	mux.HandleFunc("GET /api/v1/pages/{id}", handlers.GetPage)
	mux.HandleFunc("GET /api/v1/pages/lookup", handlers.LookupPage)
	mux.HandleFunc("GET /api/v1/pages/{id}/outlinks", handlers.GetOutlinks)

	// Backlinks
	mux.HandleFunc("GET /api/v1/backlinks", handlers.GetBacklinks)

	// Stats
	mux.HandleFunc("GET /api/v1/stats", handlers.GetStats)

	// Crawl
	mux.HandleFunc("POST /api/v1/crawl", handlers.CrawlURLs)

	// Queue
	mux.HandleFunc("GET /api/v1/queue", handlers.GetQueue)

	// Seeds
	mux.HandleFunc("GET /api/v1/seeds", handlers.ListSeeds)
	mux.HandleFunc("POST /api/v1/seeds", handlers.AddSeed)
	mux.HandleFunc("DELETE /api/v1/seeds/{domain}", handlers.DeleteSeed)

	// Crawler control
	mux.HandleFunc("POST /api/v1/crawler/start", handlers.StartCrawler)
	mux.HandleFunc("POST /api/v1/crawler/stop", handlers.StopCrawler)
	mux.HandleFunc("GET /api/v1/crawler/status", handlers.CrawlerStatus)

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
