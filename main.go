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
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/api"
	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/crawler"
	"github.com/DuvyDev/Duvycrawl/internal/embedder"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/ratelimit"
	"github.com/DuvyDev/Duvycrawl/internal/scheduler"
	"github.com/DuvyDev/Duvycrawl/internal/scorer"
	"github.com/DuvyDev/Duvycrawl/internal/seeds"
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
	embedClient := embedder.NewClient(cfg.Embedder)
	store.WithEmbedder(embedClient)

	engine := crawler.NewEngine(&cfg.Crawler, store, batchWriter, front, limiter, domainStats, embedClient, cfg.Crawler.ProxyURL, logger)

	sched := scheduler.New(store, front, scheduler.DefaultPolicy(), logger)

	apiServer := api.NewServer(&cfg.API, store, engine, front, logger)

	// --- Seed Bloom filter from existing DB pages ---
	if err := seedBloomFilter(ctx, store, crawlQueue, logger); err != nil {
		logger.Warn("failed to seed bloom filter", "error", err)
	}

	// --- Re-queue stale pages to give the crawler initial work on restart.
	if err := requeueStalePagesOnStartup(ctx, store, front, logger); err != nil {
		logger.Warn("failed to re-queue stale pages on startup", "error", err)
	}

	// --- Seed Default Domains ---
	if err := seedDefaultDomains(ctx, cfg, store, front, logger); err != nil {
		return fmt.Errorf("seeding default domains: %w", err)
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

// seedDefaultDomains registers seed domains and enqueues their start URLs.
// Seeds are read from the YAML config. If none are defined, the built-in
// defaults from internal/seeds are used as a fallback.
//
// Domain registration (in SQLite) only happens once, but seed URLs are
// always enqueued into the in-memory queue on every startup so crawling
// resumes immediately.
func seedDefaultDomains(ctx context.Context, cfg *config.Config, store storage.Storage, front *frontier.Frontier, logger *slog.Logger) error {
	// Build the seed list: prefer config, fall back to hardcoded defaults.
	var seedList []config.SeedConfig

	if len(cfg.Seeds) > 0 {
		seedList = cfg.Seeds
		logger.Info("using seeds from configuration file", "count", len(seedList))
	} else {
		// Convert hardcoded defaults to SeedConfig format.
		for _, s := range seeds.DefaultSeeds() {
			seedList = append(seedList, config.SeedConfig{
				Domain:    s.Domain,
				Priority:  s.Priority,
				StartURLs: s.StartURLs,
			})
		}
		logger.Info("no seeds in config, using built-in defaults", "count", len(seedList))
	}

	registered := 0
	enqueued := 0
	for _, seed := range seedList {
		// Apply default base score if not set.
		baseScore := float64(seed.Priority)
		if baseScore <= 0 {
			baseScore = storage.PrioritySeed
		}

		// Register the domain as a seed (only if not already registered).
		existing, err := store.GetDomain(ctx, seed.Domain)
		if err != nil {
			return fmt.Errorf("checking seed domain %q: %w", seed.Domain, err)
		}

		if existing == nil || !existing.IsSeed {
			domain := &storage.Domain{
				Domain: seed.Domain,
				IsSeed: true,
			}
			if err := store.UpsertDomain(ctx, domain); err != nil {
				return fmt.Errorf("upserting seed domain %q: %w", seed.Domain, err)
			}
			registered++
		}

		// Always enqueue start URLs into the in-memory queue.
		// The queue's deduplication set prevents double-processing within
		// the same session, and already-crawled pages will be re-crawled
		// only if their content has changed (via content hash).
		startURLs := seed.StartURLs
		if len(startURLs) == 0 {
			startURLs = []string{"https://" + seed.Domain + "/"}
		}

		if _, err := front.AddBatchDirect(ctx, startURLs, 0, baseScore); err != nil {
			logger.Warn("failed to enqueue seed URLs",
				"domain", seed.Domain,
				"error", err,
			)
			continue
		}

		enqueued++
	}

	logger.Info("seeded domains",
		"new_domains", registered,
		"enqueued", enqueued,
	)

	return nil
}

// requeueStalePagesOnStartup re-queues pages that haven't been crawled recently,
// giving the crawler initial work on restart without re-crawling fresh content.
func requeueStalePagesOnStartup(ctx context.Context, store storage.Storage, front *frontier.Frontier, logger *slog.Logger) error {
	// Seed pages older than 4 hours get re-queued with high priority.
	seedCutoff := time.Now().Add(-4 * time.Hour)
	staleSeeds, err := store.GetStalePages(ctx, seedCutoff, 200)
	if err != nil {
		return err
	}

	// Normal pages older than 3 days get re-queued with low priority.
	normalCutoff := time.Now().Add(-72 * time.Hour)
	stalePages, err := store.GetStalePages(ctx, normalCutoff, 2000)
	if err != nil {
		return err
	}

	var allURLs []string
	for _, p := range staleSeeds {
		allURLs = append(allURLs, p.URL)
	}
	for _, p := range stalePages {
		allURLs = append(allURLs, p.URL)
	}

	if len(allURLs) == 0 {
		logger.Info("no stale pages to re-queue on startup")
		return nil
	}

	added, err := front.AddBatchDirect(ctx, allURLs, 0, storage.PriorityNormal)
	if err != nil {
		return fmt.Errorf("re-queuing stale pages on startup: %w", err)
	}

	logger.Info("re-queued stale pages on startup",
		"stale_seeds", len(staleSeeds),
		"stale_pages", len(stalePages),
		"enqueued", added,
	)
	return nil
}
