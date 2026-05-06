// Package scheduler manages automatic re-crawling of seed URLs
// based on their configured recrawl intervals.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// Scheduler periodically checks seed URLs and re-enqueues those
// that have exceeded their recrawl interval.
type Scheduler struct {
	store    storage.Storage
	frontier *frontier.Frontier
	cfg      config.SchedulerConfig
	logger   *slog.Logger
	done     chan struct{}
}

// New creates a new scheduler.
func New(store storage.Storage, front *frontier.Frontier, cfg config.SchedulerConfig, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:    store,
		frontier: front,
		cfg:      cfg,
		logger:   logger.With("component", "scheduler"),
		done:     make(chan struct{}),
	}
}

// Start begins the scheduling loop. It runs until Stop() is called
// or the context is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.logger.Info("scheduler started",
		"tick_interval", s.cfg.TickInterval,
		"default_seed_interval", s.cfg.SeedRecrawlInterval,
	)

	// Run immediately on start, then periodically.
	s.tick(ctx)

	ticker := time.NewTicker(s.cfg.TickInterval)
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
// 1. Find stale seed URLs.
// 2. Re-enqueue them into the frontier.
// 3. Update their last_enqueued timestamp.
// 4. Reset any stalled (orphaned) jobs.
func (s *Scheduler) tick(ctx context.Context) {
	s.logger.Debug("scheduler tick starting")

	staleSeeds, err := s.store.GetStaleSeedURLs(ctx, 100)
	if err != nil {
		s.logger.Error("failed to get stale seed urls", "error", err)
		return
	}

	if len(staleSeeds) > 0 {
		urls := make([]string, len(staleSeeds))
		for i, seed := range staleSeeds {
			urls[i] = seed.URL
		}

		if _, err := s.frontier.AddBatchDirect(ctx, urls, 0, storage.PriorityRecrawl+5); err != nil {
			s.logger.Error("failed to re-enqueue stale seed urls",
				"error", err,
				"count", len(urls),
			)
		} else {
			now := time.Now().UTC()
			for _, seed := range staleSeeds {
				if err := s.store.UpdateSeedURLLastEnqueued(ctx, seed.URL, now); err != nil {
					s.logger.Warn("failed to update seed last_enqueued", "url", seed.URL, "error", err)
				}
			}
			s.logger.Info("re-enqueued stale seed urls for re-crawl",
				"count", len(urls),
			)
		}
	}

	// Reset stalled jobs regardless of seed re-crawl result.
	if _, err := s.store.ResetStalledJobs(ctx, 30*time.Minute); err != nil {
		s.logger.Warn("failed to reset stalled jobs", "error", err)
	}

	s.logger.Debug("scheduler tick complete")
}
