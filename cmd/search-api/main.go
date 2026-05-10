package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/DuvyDev/Duvycrawl/internal/api"
	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/embedder"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// --- Parse CLI Flags ---
	configPath := flag.String("config", "configs/default.yaml", "path to YAML configuration file")
	flag.Parse()

	// --- Load Configuration ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	// --- Initialize Logger ---
	logger := initLogger(cfg.Logging)
	logger.Info("Duvycrawl Search API starting",
		"config", *configPath,
		"db_path", cfg.Storage.DBPath,
		"api_addr", cfg.API.Addr(),
	)

	// --- Initialize Storage ---
	ctx := context.Background()
	store, err := storage.NewSQLiteStorage(ctx, cfg.Storage.DBPath, storage.ModeSearchAPI, logger)
	if err != nil {
		return fmt.Errorf("initializing storage: %w", err)
	}
	store.WithSearchIntents(cfg.SearchIntents.SiteTypes, cfg.SearchIntents.PlatformDomains)
	store.WithScoringConfig(storage.ScoringConfig{
		LanguageBoost:          cfg.Scoring.LanguageBoost,
		SecondaryLanguageBoost: cfg.Scoring.SecondaryLanguageBoost,
		SecondaryLanguage:      cfg.Scoring.SecondaryLanguage,
	})
	defer store.Close()

	// Initialize Ollama embedder for semantic search
	var embedClient *embedder.Client
	if cfg.Embedder.Enabled {
		embedClient = embedder.NewClient(cfg.Embedder)
		store.WithEmbedder(embedClient)
	}

	// Create API server without Crawler Engine and Frontier
	apiServer := api.NewServer(&cfg.API, store, nil, nil, logger)

	// --- Setup Graceful Shutdown ---
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start Server ---
	errCh := make(chan error, 1)
	go func() {
		if err := apiServer.Start(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("API server error: %w", err)
		}
	}()

	logger.Info("Search API is running",
		"api", apiServer.Addr(),
	)

	// --- Wait for Shutdown Signal ---
	select {
	case sig := <-sigCh:
		logger.Info("received shutdown signal", "signal", sig)
	case err := <-errCh:
		logger.Error("component error, initiating shutdown", "error", err)
	}

	// --- Graceful Shutdown ---
	logger.Info("initiating graceful shutdown...")

	if err := apiServer.Shutdown(context.Background()); err != nil {
		logger.Error("API server shutdown error", "error", err)
	}

	logger.Info("Search API shut down gracefully")
	return nil
}

// initLogger configures the slog logger based on configuration.
func initLogger(cfg config.LoggingConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: level == slog.LevelDebug,
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
