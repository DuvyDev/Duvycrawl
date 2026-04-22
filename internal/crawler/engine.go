package crawler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/ratelimit"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// EngineStatus represents the current state of the crawler engine.
type EngineStatus string

const (
	StatusIdle    EngineStatus = "idle"
	StatusRunning EngineStatus = "running"
	StatusStopping EngineStatus = "stopping"
)

// Engine is the main crawler orchestrator. It manages a pool of worker
// goroutines that fetch, parse, and store web pages.
type Engine struct {
	cfg      *config.CrawlerConfig
	store    storage.Storage
	frontier *frontier.Frontier
	fetcher  *Fetcher
	parser   *Parser
	robots   *RobotsCache
	limiter  *ratelimit.DomainLimiter
	logger   *slog.Logger

	status   atomic.Value // EngineStatus
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// seedDomains holds the set of seed domain names for SeedDomainsOnly filtering.
	seedDomains map[string]bool

	// Metrics
	pagesCrawled atomic.Int64
	pagesErrored atomic.Int64
}

// NewEngine creates a new crawler engine wired with all its dependencies.
func NewEngine(
	cfg *config.CrawlerConfig,
	store storage.Storage,
	front *frontier.Frontier,
	limiter *ratelimit.DomainLimiter,
	logger *slog.Logger,
) *Engine {
	e := &Engine{
		cfg:      cfg,
		store:    store,
		frontier: front,
		fetcher:  NewFetcher(cfg.UserAgent, cfg.RequestTimeout, cfg.MaxPageSizeKB, logger),
		parser:   NewParser(),
		robots:   NewRobotsCache(cfg.UserAgent, 24*time.Hour, logger),
		limiter:  limiter,
		logger:   logger.With("component", "engine"),
	}
	e.status.Store(StatusIdle)
	return e
}

// Start launches the worker pool and begins crawling.
// It is non-blocking and returns immediately. Use Stop() to shut down.
func (e *Engine) Start(ctx context.Context) {
	if e.Status() == StatusRunning {
		e.logger.Warn("engine already running, ignoring start request")
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.status.Store(StatusRunning)

	e.logger.Info("starting crawler engine",
		"workers", e.cfg.Workers,
		"max_depth", e.cfg.MaxDepth,
		"politeness_delay", e.cfg.PolitenessDelay,
		"seed_domains_only", e.cfg.SeedDomainsOnly,
	)

	// Load seed domains for filtering if SeedDomainsOnly is enabled.
	if e.cfg.SeedDomainsOnly {
		if err := e.loadSeedDomains(ctx); err != nil {
			e.logger.Error("failed to load seed domains for filtering", "error", err)
		}
	}

	// Launch workers.
	for i := range e.cfg.Workers {
		e.wg.Add(1)
		go e.worker(ctx, i)
	}

	e.logger.Info("all workers started")
}

// Stop gracefully shuts down the crawler engine.
// Workers finish their current page before exiting.
func (e *Engine) Stop() {
	if e.Status() != StatusRunning {
		return
	}

	e.logger.Info("stopping crawler engine...")
	e.status.Store(StatusStopping)

	if e.cancel != nil {
		e.cancel()
	}

	e.wg.Wait()
	e.status.Store(StatusIdle)

	e.logger.Info("crawler engine stopped",
		"pages_crawled", e.pagesCrawled.Load(),
		"pages_errored", e.pagesErrored.Load(),
	)
}

// Status returns the current engine status.
func (e *Engine) Status() EngineStatus {
	return e.status.Load().(EngineStatus)
}

// Stats returns current crawl metrics.
func (e *Engine) Stats() (crawled, errored int64) {
	return e.pagesCrawled.Load(), e.pagesErrored.Load()
}

// worker is the main loop for a single crawler worker goroutine.
// The entire domain-selection logic happens in memory via the queue's
// Dequeue method — no database round-trips in the scheduling hot path.
func (e *Engine) worker(ctx context.Context, id int) {
	defer e.wg.Done()

	logger := e.logger.With("worker", id)
	logger.Debug("worker started")

	// readyFn is passed to the queue to check rate limits in-memory.
	readyFn := func(domain string) bool {
		return e.limiter.TryWait(domain)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Debug("worker shutting down")
			return
		default:
		}

		// Ask the queue for a job from any ready domain.
		// This is entirely in-memory — O(domains), zero DB calls.
		job := e.frontier.Dequeue(readyFn)
		if job == nil {
			// No ready work — wait before checking again.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		if ctx.Err() != nil {
			return
		}

		e.processJob(ctx, logger, job)
	}
}

// processJob handles the complete lifecycle of crawling a single URL.
func (e *Engine) processJob(ctx context.Context, logger *slog.Logger, job *queue.Job) {
	logger = logger.With(
		"url", job.URL,
		"domain", job.Domain,
		"depth", job.Depth,
	)

	// Check robots.txt if enabled.
	if e.cfg.RespectRobots && !e.robots.IsAllowed(ctx, job.URL, job.Domain) {
		logger.Debug("blocked by robots.txt")
		return
	}

	// Fetch the page.
	logger.Debug("fetching page")
	result, err := e.fetcher.Fetch(ctx, job.URL)
	if err != nil {
		e.pagesErrored.Add(1)
		logger.Warn("fetch failed", "error", err)
		return
	}

	// Skip non-2xx responses.
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		logger.Debug("non-2xx status", "status", result.StatusCode)
		return
	}

	// Parse the HTML.
	parsed, err := e.parser.Parse(result.Body, result.ContentType, result.FinalURL)
	if err != nil {
		e.pagesErrored.Add(1)
		logger.Warn("parse failed", "error", err)
		return
	}

	// Compute content hash for change detection.
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(parsed.Content)))

	// Store the page.
	page := &storage.Page{
		URL:         result.FinalURL,
		Domain:      job.Domain,
		Title:       truncateString(parsed.Title, 500),
		Description: truncateString(parsed.Description, 1000),
		Content:     parsed.Content,
		StatusCode:  result.StatusCode,
		ContentHash: contentHash,
		CrawledAt:   time.Now().UTC(),
	}

	if err := e.store.UpsertPage(ctx, page); err != nil {
		e.pagesErrored.Add(1)
		logger.Error("failed to store page", "error", err)
		return
	}

	e.pagesCrawled.Add(1)
	logger.Info("page crawled successfully",
		"title", page.Title,
		"links_found", len(parsed.Links),
		"duration", result.Duration,
	)

	// Enqueue discovered links if we haven't exceeded max depth.
	if job.Depth < e.cfg.MaxDepth && len(parsed.Links) > 0 {
		e.enqueueDiscoveredLinks(ctx, logger, parsed.Links, job.Depth+1)
	}

	// Update domain stats.
	e.updateDomainStats(ctx, job.Domain, result.Duration)
}

// enqueueDiscoveredLinks adds newly found URLs to the frontier.
// When SeedDomainsOnly is enabled, links to non-seed domains are discarded.
func (e *Engine) enqueueDiscoveredLinks(ctx context.Context, logger *slog.Logger, links []string, depth int) {
	priority := storage.PriorityNormal

	// Filter by seed domains if configured.
	if e.cfg.SeedDomainsOnly && len(e.seedDomains) > 0 {
		filtered := make([]string, 0, len(links))
		for _, link := range links {
			domain := frontier.ExtractDomain(link)
			if e.seedDomains[domain] {
				filtered = append(filtered, link)
			}
		}
		if len(links) != len(filtered) {
			logger.Debug("filtered links by seed domains",
				"total", len(links),
				"kept", len(filtered),
				"discarded", len(links)-len(filtered),
			)
		}
		links = filtered
	}

	// Limit the number of links per page to prevent flooding.
	maxLinks := 100
	if len(links) > maxLinks {
		links = links[:maxLinks]
	}

	if len(links) == 0 {
		return
	}

	if err := e.frontier.AddBatch(ctx, links, depth, priority); err != nil {
		logger.Warn("failed to enqueue discovered links", "error", err, "count", len(links))
	}
}

// updateDomainStats updates the crawl statistics for a domain.
func (e *Engine) updateDomainStats(ctx context.Context, domainName string, fetchDuration time.Duration) {
	domain, err := e.store.GetDomain(ctx, domainName)
	if err != nil || domain == nil {
		// Domain not tracked yet, create it.
		domain = &storage.Domain{
			Domain:        domainName,
			LastCrawled:   time.Now().UTC(),
			PagesCount:    1,
			AvgResponseMs: int(fetchDuration.Milliseconds()),
		}
	} else {
		domain.LastCrawled = time.Now().UTC()
		domain.PagesCount++
		// Exponential moving average for response time.
		alpha := 0.3
		domain.AvgResponseMs = int(alpha*float64(fetchDuration.Milliseconds()) + (1-alpha)*float64(domain.AvgResponseMs))
	}

	if err := e.store.UpsertDomain(ctx, domain); err != nil {
		e.logger.Warn("failed to update domain stats",
			"domain", domainName,
			"error", err,
		)
	}
}

// truncateString truncates a string to the given maximum length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// loadSeedDomains populates the seedDomains set from the database.
func (e *Engine) loadSeedDomains(ctx context.Context) error {
	seeds, err := e.store.GetSeedDomains(ctx)
	if err != nil {
		return err
	}

	e.seedDomains = make(map[string]bool, len(seeds))
	for _, s := range seeds {
		e.seedDomains[s.Domain] = true
	}

	e.logger.Info("loaded seed domains for filtering",
		"count", len(e.seedDomains),
	)
	return nil
}

// RefreshSeedDomains reloads the seed domains set.
// Call this after adding/removing seeds via the API.
func (e *Engine) RefreshSeedDomains(ctx context.Context) error {
	if !e.cfg.SeedDomainsOnly {
		return nil
	}
	return e.loadSeedDomains(ctx)
}
