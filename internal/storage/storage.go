package storage

import (
	"context"
	"time"
)

// Storage defines the persistence interface used by all crawler components.
// It abstracts the underlying database so implementations can be swapped
// without affecting business logic.
type Storage interface {
	// --- Page Operations ---

	// UpsertPage inserts a new page or updates an existing one (matched by URL).
	UpsertPage(ctx context.Context, page *Page) error

	// GetPageByURL retrieves a single page by its URL.
	GetPageByURL(ctx context.Context, url string) (*Page, error)

	// GetPageByID retrieves a single page by its database ID.
	GetPageByID(ctx context.Context, id int64) (*Page, error)

	// SearchPages performs a full-text search and returns paginated results.
	// Returns the matching results and the total count of matches.
	// If lang is non-empty, results in that language are boosted in ranking.
	SearchPages(ctx context.Context, query string, limit, offset int, lang string) ([]SearchResult, int, error)

	// --- Image Operations ---

	// UpsertImages inserts or updates image records in bulk.
	UpsertImages(ctx context.Context, images []ImageRecord) error

	// SearchImages performs a full-text search over image metadata.
	SearchImages(ctx context.Context, query string, limit, offset int) ([]ImageSearchResult, int, error)

	// GetStalePages returns pages that were crawled before the given timestamp,
	// suitable for re-crawling.
	GetStalePages(ctx context.Context, olderThan time.Time, limit int) ([]Page, error)

	// --- Crawl Queue Operations ---

	// EnqueueURL adds a single URL to the crawl queue if it doesn't already exist.
	EnqueueURL(ctx context.Context, job *CrawlJob) error

	// EnqueueURLs adds multiple URLs to the crawl queue in a single transaction.
	EnqueueURLs(ctx context.Context, jobs []*CrawlJob) error

	// DequeueURLs atomically claims up to `limit` pending jobs from the queue,
	// ordered by priority (descending). Claimed jobs are marked as in_progress.
	DequeueURLs(ctx context.Context, limit int) ([]*CrawlJob, error)

	// DequeueURLsExcluding is like DequeueURLs but skips jobs belonging to
	// any of the excluded domains. This enables domain-aware parallelism:
	// workers can skip rate-limited domains and grab work for other domains.
	DequeueURLsExcluding(ctx context.Context, limit int, excludedDomains []string) ([]*CrawlJob, error)

	// CompleteJob marks a crawl job as done or failed, depending on the error.
	CompleteJob(ctx context.Context, jobID int64, crawlErr error) error

	// ReturnJob returns a claimed (in_progress) job back to the pending state.
	// Used when a worker cannot process a job (e.g. domain is rate-limited)
	// and wants to release it for another worker to pick up.
	ReturnJob(ctx context.Context, jobID int64) error

	// GetQueueStats returns the current state of the crawl queue.
	GetQueueStats(ctx context.Context) (*QueueStats, error)

	// --- Domain Operations ---

	// UpsertDomain inserts a new domain or updates an existing one.
	UpsertDomain(ctx context.Context, domain *Domain) error

	// GetDomain retrieves a domain by its name.
	GetDomain(ctx context.Context, domainName string) (*Domain, error)

	// GetSeedDomains returns all domains marked as seeds.
	GetSeedDomains(ctx context.Context) ([]Domain, error)

	// DeleteDomain removes a domain from the seed list (sets is_seed = false).
	DeleteDomain(ctx context.Context, domainName string) error

	// --- Statistics ---

	// GetStats returns overall crawler statistics.
	GetStats(ctx context.Context) (*CrawlerStats, error)

	// --- Maintenance ---

	// PurgeOldPages deletes pages crawled before the given timestamp.
	// Returns the number of deleted pages.
	PurgeOldPages(ctx context.Context, olderThan time.Time) (int64, error)

	// ResetStalledJobs resets jobs that have been in_progress for too long
	// back to pending, so they can be retried.
	ResetStalledJobs(ctx context.Context, stalledAfter time.Duration) (int64, error)

	// Vacuum runs SQLite's VACUUM command to reclaim disk space.
	Vacuum(ctx context.Context) error

	// Close gracefully shuts down the storage layer.
	Close() error
}
