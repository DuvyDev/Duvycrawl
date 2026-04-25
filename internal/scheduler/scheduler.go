// Package scheduler manages automatic re-crawling of stale pages
// and periodic injection of seed URLs into the frontier.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// FreshnessPolicy defines how often different types of pages should be re-crawled.
type FreshnessPolicy struct {
	SeedInterval   time.Duration // How often to re-crawl seed domain pages.
	NormalInterval time.Duration // How often to re-crawl normal pages.
	UnchangedBonus time.Duration // Extra time before re-crawling unchanged pages.
}

// DefaultPolicy returns the default freshness policy.
func DefaultPolicy() FreshnessPolicy {
	return FreshnessPolicy{
		SeedInterval:   24 * time.Hour,
		NormalInterval: 7 * 24 * time.Hour, // 7 days
		UnchangedBonus: 7 * 24 * time.Hour, // +7 days for unchanged pages (total: 14 days)
	}
}

// Scheduler periodically checks for stale pages and injects them
// back into the frontier for re-crawling.
type Scheduler struct {
	store    storage.Storage
	frontier *frontier.Frontier
	policy   FreshnessPolicy
	logger   *slog.Logger
	done     chan struct{}
}

// New creates a new scheduler.
func New(store storage.Storage, front *frontier.Frontier, policy FreshnessPolicy, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:    store,
		frontier: front,
		policy:   policy,
		logger:   logger.With("component", "scheduler"),
		done:     make(chan struct{}),
	}
}

// Start begins the scheduling loop. It runs until Stop() is called
// or the context is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.logger.Info("scheduler started",
		"seed_interval", s.policy.SeedInterval,
		"normal_interval", s.policy.NormalInterval,
	)

	// Run immediately on start, then periodically.
	s.tick(ctx)

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopped (context cancelled)")
			return
		case <-s.done:
			s.logger.Info("scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// Stop signals the scheduler to stop.
func (s *Scheduler) Stop() {
	select {
	case <-s.done:
		// Already closed.
	default:
		close(s.done)
	}
}

// tick performs a single scheduling cycle:
// 1. Check for stale seed pages → re-enqueue with high priority.
// 2. Check for stale normal pages → re-enqueue with low priority.
// 3. Reset any stalled (orphaned) jobs.
func (s *Scheduler) tick(ctx context.Context) {
	s.logger.Debug("scheduler tick starting")

	// Re-queue stale seed pages.
	seedCutoff := time.Now().Add(-s.policy.SeedInterval)
	s.requeueStalePages(ctx, seedCutoff, storage.PriorityRecrawl+5, 50) // Slightly higher than normal recrawl.

	// Re-queue stale normal pages.
	normalCutoff := time.Now().Add(-s.policy.NormalInterval)
	s.requeueStalePages(ctx, normalCutoff, storage.PriorityRecrawl, 100)

	s.logger.Debug("scheduler tick complete")
}

// requeueStalePages finds pages older than the cutoff and re-adds them to the frontier.
func (s *Scheduler) requeueStalePages(ctx context.Context, olderThan time.Time, priority, limit int) {
	pages, err := s.store.GetStalePages(ctx, olderThan, limit)
	if err != nil {
		s.logger.Error("failed to get stale pages", "error", err)
		return
	}

	if len(pages) == 0 {
		return
	}

	var urls []string
	for _, p := range pages {
		urls = append(urls, p.URL)
	}

	if _, err := s.frontier.AddBatchDirect(ctx, urls, 0, priority); err != nil {
		s.logger.Error("failed to re-enqueue stale pages",
			"error", err,
			"count", len(urls),
		)
		return
	}

	s.logger.Info("re-enqueued stale pages for re-crawl",
		"count", len(urls),
		"priority", priority,
		"older_than", olderThan.Format(time.RFC3339),
	)
}
