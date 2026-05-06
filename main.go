// Duvycrawl is a personal web crawler designed to power a custom search engine.
// It crawls web pages, indexes them using SQLite FTS5, and exposes a REST API
// for searching and controlling the crawler from a frontend application.
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
	"github.com/DuvyDev/Duvycrawl/internal/crawler"
	"github.com/DuvyDev/Duvycrawl/internal/embedder"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/ratelimit"
	"github.com/DuvyDev/Duvycrawl/internal/scheduler"
	"github.com/DuvyDev/Duvycrawl/internal/scorer"
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
	noStart := flag.Bool("no-start", false, "override auto_start in config: do not start crawler on launch")
	flag.Parse()

	// --- Load Configuration ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	// --no-start flag overrides auto_start from config.
	autoStart := cfg.Crawler.AutoStart && !*noStart

	// --- Initialize Logger ---
	logger := initLogger(cfg.Logging)
	logger.Info("Duvycrawl starting",
		"config", *configPath,
		"workers", cfg.Crawler.Workers,
		"db_path", cfg.Storage.DBPath,
		"api_addr", cfg.API.Addr(),
	)

	// --- Initialize Storage ---
	ctx := context.Background()
	store, err := storage.NewSQLiteStorage(ctx, cfg.Storage.DBPath, logger)
	if err != nil {
		return fmt.Errorf("initializing storage: %w", err)
	}
	store.WithSearchIntents(cfg.SearchIntents.SiteTypes, cfg.SearchIntents.PlatformDomains)
	defer store.Close()

	// --- Initialize Scorer ---
	var scoringEngine scorer.Scorer
	if cfg.Crawler.ScoringStrategy == "adaptive" {
		adaptive := scorer.NewAdaptive(store, cfg.Crawler.Adaptive, logger)
		if err := adaptive.SeedInterestsFromConfig(ctx, cfg.Crawler.Adaptive.Interests); err != nil {
			logger.Warn("failed to seed interests from config", "error", err)
		}
		adaptive.StartRefreshLoop(ctx)
		scoringEngine = adaptive
	} else {
		scoringEngine = scorer.NewStatic(
			cfg.Crawler.Adaptive.SeedBonus,
			storage.PriorityNormal,
			storage.PriorityRecrawl,
			cfg.Crawler.Adaptive.DepthPenaltyK,
			logger,
		)
	}
	logger.Info("scoring strategy", "strategy", scoringEngine.Name())

	// --- Initialize Components ---
	crawlQueue := queue.New()
	front := frontier.New(crawlQueue, store, scoringEngine, logger)
	limiter := ratelimit.NewDomainLimiter(cfg.Crawler.PolitenessDelay, cfg.Crawler.RandomDelay, cfg.Crawler.ParallelismPerDomain)
	defer limiter.Close()

	batchWriter := storage.NewBatchWriter(store.WriteContentDB(), store.GraphDB(), logger)
	defer batchWriter.Stop()

	domainStats := crawler.NewDomainStatsCollector(store, cfg.Crawler.DomainStatsFlushInterval, logger)
	defer domainStats.Stop()

	// Initialize Ollama embedder for semantic search (optional — falls back to lexical-only if unavailable).
	var embedClient *embedder.Client
	if cfg.Embedder.Enabled {
		embedClient = embedder.NewClient(cfg.Embedder)
		store.WithEmbedder(embedClient)
	}

	engine := crawler.NewEngine(&cfg.Crawler, store, batchWriter, front, limiter, domainStats, embedClient, cfg.Crawler.ProxyURL, logger)

	sched := scheduler.New(store, front, cfg.Crawler.Scheduler, logger)

	apiServer := api.NewServer(&cfg.API, store, engine, front, logger)

	// --- Seed Bloom filter from existing DB pages ---
	if err := seedBloomFilter(ctx, store, crawlQueue, logger); err != nil {
		logger.Warn("failed to seed bloom filter", "error", err)
	}

	// --- Seed URLs from config ---
	if err := seedURLsFromConfig(ctx, cfg, store, front, logger); err != nil {
		return fmt.Errorf("seeding urls from config: %w", err)
	}

	// --- Setup Graceful Shutdown ---
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start Components ---

	// Start the periodic wake goroutine so workers unblock when rate limiter
	// slots free up even if no new URLs are being enqueued.
	crawlQueue.StartPeriodicWake(ctx)

	// Start the scheduler in a goroutine.
	go sched.Start(ctx)

	// Start the crawler engine if auto-start is enabled.
	if autoStart {
		engine.Start(ctx)
	} else {
		logger.Info("crawler not auto-started, use API to start: POST /api/v1/crawler/start")
	}

	// Start the API server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		if err := apiServer.Start(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("API server error: %w", err)
		}
	}()

	logger.Info("Duvycrawl is running",
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

	// Stop the crawler engine first (wait for in-progress pages).
	engine.Stop()

	// Stop the scheduler.
	sched.Stop()

	// Shutdown the API server.
	if err := apiServer.Shutdown(context.Background()); err != nil {
		logger.Error("API server shutdown error", "error", err)
	}

	logger.Info("Duvycrawl shut down gracefully")
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

// seedBloomFilter loads all already-crawled URLs from the database and marks
// them in the in-memory Bloom filter so they are not re-enqueued as new.
func seedBloomFilter(ctx context.Context, store storage.Storage, q *queue.Queue, logger *slog.Logger) error {
	urls, fingerprints, err := store.GetAllPageURLs(ctx)
	if err != nil {
		return err
	}

	marked := 0
	for i := range urls {
		q.MarkSeen(fingerprints[i])
		q.MarkSeen(urls[i])
		marked += 2
	}

	// Also seed from discovered resources (non-HTML assets crawled for link discovery).
	discURLs, discFingerprints, err := store.GetAllDiscoveredURLs(ctx)
	if err != nil {
		logger.Warn("failed to seed bloom filter from discovered resources", "error", err)
	} else {
		for i := range discURLs {
			q.MarkSeen(discFingerprints[i])
			q.MarkSeen(discURLs[i])
			marked += 2
		}
	}

	logger.Info("bloom filter seeded from database",
		"urls", len(urls),
		"discovered", len(discURLs),
		"marked", marked,
	)
	return nil
}

// seedURLsFromConfig persists seed URLs from the config into the database
// and enqueues them into the frontier. Each seed gets its own recrawl interval.
func seedURLsFromConfig(ctx context.Context, cfg *config.Config, store storage.Storage, front *frontier.Frontier, logger *slog.Logger) error {
	if len(cfg.Seeds) == 0 {
		logger.Info("no seed urls configured")
		return nil
	}

	defaultInterval := int(cfg.Crawler.Scheduler.SeedRecrawlInterval.Seconds())
	if defaultInterval <= 0 {
		defaultInterval = 86400 // 24h fallback
	}

	enqueued := 0
	for _, seedCfg := range cfg.Seeds {
		domain := frontier.ExtractDomain(seedCfg.URL)
		if domain == "" {
			logger.Warn("skipping invalid seed url", "url", seedCfg.URL)
			continue
		}

		interval := int(seedCfg.RecrawlInterval.Seconds())
		if interval <= 0 {
			interval = defaultInterval
		}

		seed := &storage.SeedURL{
			URL:                    seedCfg.URL,
			Domain:                 domain,
			RecrawlIntervalSeconds: interval,
		}

		if err := store.UpsertSeedURL(ctx, seed); err != nil {
			logger.Warn("failed to persist seed url", "url", seedCfg.URL, "error", err)
			continue
		}

		if err := front.Add(ctx, seedCfg.URL, 0, storage.PrioritySeed); err != nil {
			logger.Warn("failed to enqueue seed url", "url", seedCfg.URL, "error", err)
			continue
		}

		enqueued++
	}

	logger.Info("seed urls configured",
		"count", len(cfg.Seeds),
		"enqueued", enqueued,
	)
	return nil
}
