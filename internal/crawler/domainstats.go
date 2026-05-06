package crawler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// domainStatsAccum holds in-memory accumulated statistics for a single domain
// between flushes to SQLite.
type domainStatsAccum struct {
	pagesCount    int
	totalDuration time.Duration
	lastCrawled   time.Time
}

// DomainStatsCollector accumulates per-domain crawl statistics in memory and
// flushes them to SQLite periodically. This removes the write hot-spot that
// occurs when every worker synchronously updates the domains table after each
// fetch.
type DomainStatsCollector struct {
	store    storage.Storage
	logger   *slog.Logger
	interval time.Duration

	mu    sync.Mutex
	stats map[string]*domainStatsAccum

	done chan struct{}
	wg   sync.WaitGroup
}

// NewDomainStatsCollector creates a background collector. Call Stop() on
// shutdown to perform a final flush.
func NewDomainStatsCollector(store storage.Storage, interval time.Duration, logger *slog.Logger) *DomainStatsCollector {
	ds := &DomainStatsCollector{
		store:    store,
		interval: interval,
		logger:   logger.With("component", "domain_stats_collector"),
		stats:    make(map[string]*domainStatsAccum),
		done:     make(chan struct{}),
	}
	ds.wg.Add(1)
	go ds.loop()
	return ds
}

// Record adds a crawl observation for the given domain. This is safe for
// concurrent use and never blocks.
func (ds *DomainStatsCollector) Record(domain string, duration time.Duration) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	acc, ok := ds.stats[domain]
	if !ok {
		acc = &domainStatsAccum{}
		ds.stats[domain] = acc
	}
	acc.pagesCount++
	acc.totalDuration += duration
	acc.lastCrawled = time.Now().UTC()
}

// Stop signals the background loop to exit and performs a final flush.
func (ds *DomainStatsCollector) Stop() {
	close(ds.done)
	ds.wg.Wait()
	if err := ds.flush(); err != nil {
		ds.logger.Warn("final flush failed", "error", err)
	}
}

func (ds *DomainStatsCollector) loop() {
	defer ds.wg.Done()
	ticker := time.NewTicker(ds.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ds.done:
			return
		case <-ticker.C:
			if err := ds.flush(); err != nil {
				ds.logger.Warn("periodic flush failed", "error", err)
			}
		}
	}
}

// flush writes all accumulated stats to SQLite. It fetches the current domain
// row, merges counts and recomputes the exponential moving average for response
// times, then upserts the result.
func (ds *DomainStatsCollector) flush() error {
	ds.mu.Lock()
	snapshot := ds.stats
	ds.stats = make(map[string]*domainStatsAccum)
	ds.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	flushed := 0
	for domain, acc := range snapshot {
		if acc.pagesCount == 0 {
			continue
		}

		existing, err := ds.store.GetDomain(ctx, domain)
		if err != nil {
			ds.logger.Warn("failed to get domain for stats merge", "domain", domain, "error", err)
			continue
		}

		d := &storage.Domain{
			Domain:      domain,
			LastCrawled: acc.lastCrawled,
		}

		avgMs := int(acc.totalDuration.Milliseconds() / int64(acc.pagesCount))

		if existing != nil {
			d.RobotsTxt = existing.RobotsTxt
			d.RobotsFetched = existing.RobotsFetched
			d.PagesCount = existing.PagesCount + acc.pagesCount
			// Exponential moving average with alpha = 0.3
			alpha := 0.3
			d.AvgResponseMs = int(alpha*float64(avgMs) + (1-alpha)*float64(existing.AvgResponseMs))
		} else {
			d.PagesCount = acc.pagesCount
			d.AvgResponseMs = avgMs
		}

		if err := ds.store.UpsertDomain(ctx, d); err != nil {
			ds.logger.Warn("failed to upsert domain stats", "domain", domain, "error", err)
			continue
		}
		flushed++
	}

	ds.logger.Debug("flushed domain stats", "domains", flushed, "accumulated", len(snapshot))
	return nil
}
