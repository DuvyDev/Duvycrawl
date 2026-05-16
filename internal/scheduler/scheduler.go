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
		"domain_recrawl_interval", s.cfg.DomainRecrawlInterval,
		"domain_recrawl_min_pages", s.cfg.DomainRecrawlMinPages,
		"domain_recrawl_batch_limit", s.cfg.DomainRecrawlBatchLimit,
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
// 5. Re-enqueue stale domain homepages.
// 6. Fetch manually enqueued jobs.
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

	// Domain homepage re-crawl: revisit homepages of known domains to
	// discover new content without re-crawling every individual page.
	if s.cfg.DomainRecrawlInterval > 0 {
		minPages := s.cfg.DomainRecrawlMinPages
		if minPages <= 0 {
			minPages = 10
		}
		batchLimit := s.cfg.DomainRecrawlBatchLimit
		if batchLimit <= 0 {
			batchLimit = 50
		}

		staleDomains, err := s.store.GetStaleDomainsForRecrawl(
			ctx, minPages, s.cfg.DomainRecrawlInterval, batchLimit,
		)
		if err != nil {
			s.logger.Error("failed to get stale domains for recrawl", "error", err)
		} else if len(staleDomains) > 0 {
			homepageURLs := make([]string, len(staleDomains))
			for i, d := range staleDomains {
				homepageURLs[i] = "https://" + d.Domain + "/"
			}

			if _, err := s.frontier.AddBatchDirect(ctx, homepageURLs, s.cfg.DomainRecrawlDepth, storage.PriorityRecrawl); err != nil {
				s.logger.Error("failed to re-enqueue domain homepages",
					"error", err,
					"count", len(homepageURLs),
				)
			} else {
				now := time.Now().UTC()
				for _, d := range staleDomains {
					if err := s.store.UpdateDomainHomepageRecrawl(ctx, d.Domain, now); err != nil {
						s.logger.Warn("failed to update domain last_homepage_recrawl",
							"domain", d.Domain, "error", err)
					}
				}
				s.logger.Info("re-enqueued stale domain homepages for re-crawl",
					"count", len(staleDomains),
				)
			}
		}
	}

	// Fetch manually enqueued jobs (e.g. from the Search API)
	if manualJobs, err := s.store.DequeueURLs(ctx, 100); err != nil {
		s.logger.Error("failed to dequeue manual jobs", "error", err)
	} else if len(manualJobs) > 0 {
		var rawURLs []string
		var jobIDs []int64
		for _, j := range manualJobs {
			rawURLs = append(rawURLs, j.URL)
			jobIDs = append(jobIDs, j.ID)
		}

		if added, err := s.frontier.AddBatchDirect(ctx, rawURLs, 0, storage.PriorityNormal); err != nil {
			s.logger.Error("failed to enqueue manual jobs into frontier", "error", err)
		} else {
			s.logger.Info("loaded manual jobs from database", "fetched", len(manualJobs), "enqueued", added)
		}

		// Delete them from the database so we don't fetch them again (since the queue is in-memory now)
		for _, id := range jobIDs {
			s.store.CompleteJob(ctx, id, nil)
		}
	}

	s.logger.Debug("scheduler tick complete")
}
