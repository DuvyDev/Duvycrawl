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
	adminPort := flag.Int("admin-port", 8081, "port for the crawler admin API")
	flag.Parse()

	// --- Load Configuration ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	autoStart := cfg.Crawler.AutoStart && !*noStart

	// --- Initialize Logger ---
	logger := initLogger(cfg.Logging)
	logger.Info("Duvycrawl Engine starting",
		"config", *configPath,
		"workers", cfg.Crawler.Workers,
		"db_path", cfg.Storage.DBPath,
	)

	// --- Initialize Storage ---
	ctx := context.Background()
	store, err := storage.NewSQLiteStorage(ctx, cfg.Storage.DBPath, storage.ModeCrawler, logger)
	if err != nil {
		return fmt.Errorf("initializing storage: %w", err)
	}
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

	// --- Initialize Components ---
	crawlQueue := queue.New()
	front := frontier.New(crawlQueue, store, scoringEngine, logger)
	limiter := ratelimit.NewDomainLimiter(cfg.Crawler.PolitenessDelay, cfg.Crawler.RandomDelay, cfg.Crawler.ParallelismPerDomain)
	defer limiter.Close()

	batchWriter := storage.NewBatchWriter(store.WriteContentDB(), store.GraphDB(), logger)
	defer batchWriter.Stop()

	domainStats := crawler.NewDomainStatsCollector(store, cfg.Crawler.DomainStatsFlushInterval, logger)
	defer domainStats.Stop()

	var embedClient *embedder.Client
	if cfg.Embedder.Enabled {
		embedClient = embedder.NewClient(cfg.Embedder)
		store.WithEmbedder(embedClient)
	}

	engine := crawler.NewEngine(&cfg.Crawler, store, batchWriter, front, limiter, domainStats, embedClient, cfg.Rendering, logger)

	sched := scheduler.New(store, front, cfg.Crawler.Scheduler, logger)

	// Admin API Server
	cfg.API.Port = *adminPort
	adminServer := api.NewServer(&cfg.API, store, engine, front, logger)

	// --- Seed Bloom filter ---
	if err := seedBloomFilter(ctx, store, crawlQueue, logger); err != nil {
		logger.Warn("failed to seed bloom filter", "error", err)
	}

	// --- Seed URLs ---
	if err := seedURLsFromConfig(ctx, cfg, store, front, logger); err != nil {
		return fmt.Errorf("seeding urls from config: %w", err)
	}

	// --- Setup Graceful Shutdown ---
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start Components ---
	crawlQueue.StartPeriodicWake(ctx)
	go sched.Start(ctx)

	if autoStart {
		engine.Start(ctx)
	} else {
		logger.Info("crawler not auto-started, use Admin API to start: POST /api/v1/crawler/start")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := adminServer.Start(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("Admin API server error: %w", err)
		}
	}()

	logger.Info("Crawler Engine is running", "admin_api", adminServer.Addr())

	// --- Wait for Shutdown Signal ---
	select {
	case sig := <-sigCh:
		logger.Info("received shutdown signal", "signal", sig)
	case err := <-errCh:
		logger.Error("component error, initiating shutdown", "error", err)
	}

	logger.Info("initiating graceful shutdown...")
	engine.Stop()
	sched.Stop()
	adminServer.Shutdown(context.Background())

	logger.Info("Crawler Engine shut down gracefully")
	return nil
}

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

	logger.Info("bloom filter seeded from database", "urls", len(urls), "discovered", len(discURLs), "marked", marked)
	return nil
}

func seedURLsFromConfig(ctx context.Context, cfg *config.Config, store storage.Storage, front *frontier.Frontier, logger *slog.Logger) error {
	if len(cfg.Seeds) == 0 {
		return nil
	}

	defaultInterval := int(cfg.Crawler.Scheduler.SeedRecrawlInterval.Seconds())
	if defaultInterval <= 0 {
		defaultInterval = 86400
	}

	enqueued := 0
	for _, seedCfg := range cfg.Seeds {
		domain := frontier.ExtractDomain(seedCfg.URL)
		if domain == "" {
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
			continue
		}

		if err := front.Add(ctx, seedCfg.URL, 0, storage.PrioritySeed); err == nil {
			enqueued++
		}
	}

	logger.Info("seed urls configured", "count", len(cfg.Seeds), "enqueued", enqueued)
	return nil
}
