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

	// GetPageByFingerprint retrieves a single page by its structural fingerprint.
	GetPageByFingerprint(ctx context.Context, fingerprint string) (*Page, error)

	// GetPageByID retrieves a single page by its database ID.
	GetPageByID(ctx context.Context, id int64) (*Page, error)

	// GetAllPageURLs returns every crawled URL and its structural fingerprint.
	// Used to seed the in-memory Bloom filter on startup.
	GetAllPageURLs(ctx context.Context) (urls []string, fingerprints []string, err error)

	// SearchPages performs a full-text search and returns paginated results.
	// Returns the matching results and the total count of matches.
	// If lang is non-empty, results in that language are boosted in ranking.
	SearchPages(ctx context.Context, query string, limit, offset int, lang string, domain string, schemaType string) ([]SearchResult, int, error)

	// --- Image Operations ---

	// UpsertImages inserts or updates image records in bulk.
	UpsertImages(ctx context.Context, images []ImageRecord) error

	// SearchImages performs a full-text search over image metadata.
	SearchImages(ctx context.Context, query string, limit, offset int) ([]ImageSearchResult, int, error)

	// --- Link / Backlink Operations ---

	// StoreLinks replaces all outgoing links for a given source page.
	// Each OutgoingLink contains the target URL and its anchor text.
	StoreLinks(ctx context.Context, sourceID int64, sourceURL string, links []OutgoingLink) error

	// GetBacklinks returns pages that link to the given target URL.
	GetBacklinks(ctx context.Context, targetURL string, limit, offset int) ([]BacklinkResult, int, error)

	// GetOutlinks returns the outgoing links from a given page ID.
	GetOutlinks(ctx context.Context, pageID int64, limit, offset int) ([]OutlinkResult, int, error)

	// GetBacklinkCount returns the number of backlinks for a given URL.
	GetBacklinkCount(ctx context.Context, targetURL string) (int, error)

	// GetStalePages returns pages that were crawled before the given timestamp,
	// suitable for re-crawling.
	GetStalePages(ctx context.Context, olderThan time.Time, limit int) ([]Page, error)

	// GetFreshURLs returns a set of URLs from the given list that were crawled
	// more recently than the given TTL. Used to skip recently-indexed URLs.
	GetFreshURLs(ctx context.Context, urls []string, newerThan time.Time) (map[string]struct{}, error)

	// ListPagesWithoutEmbeddings returns pages that do not yet have a stored
	// embedding, ordered by ID for incremental backfill.
	ListPagesWithoutEmbeddings(ctx context.Context, afterID int64, limit int) ([]Page, error)

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

	// FilterExistingURLs returns a set of URLs that already exist in the pages table.
	// This is used for batch deduplication in the frontier.
	FilterExistingURLs(ctx context.Context, urls []string) (map[string]struct{}, error)

	// FilterExistingFingerprints returns a set of URL fingerprints that already exist.
	FilterExistingFingerprints(ctx context.Context, fingerprints []string) (map[string]struct{}, error)

	// GetDomain retrieves a domain by its name.
	GetDomain(ctx context.Context, domainName string) (*Domain, error)

	// GetSeedDomains returns all domains marked as seeds.
	GetSeedDomains(ctx context.Context) ([]Domain, error)

	// DeleteDomain removes a domain from the seed list (sets is_seed = false).
	DeleteDomain(ctx context.Context, domainName string) error

	// --- Discovered Resources ---

	// UpsertDiscoveredResource inserts or updates a non-HTML discovery asset.
	UpsertDiscoveredResource(ctx context.Context, resource *DiscoveredResource) error

	// GetAllDiscoveredURLs returns every discovered resource URL and fingerprint.
	// Used to seed the in-memory Bloom filter on startup.
	GetAllDiscoveredURLs(ctx context.Context) (urls []string, fingerprints []string, err error)

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

	// UpdatePageRankings recalculates the referring_domains score for pages asynchronously.
	UpdatePageRankings(ctx context.Context) error

	// RecordClick records a user interaction with a search result to improve future rankings.
	RecordClick(ctx context.Context, query string, url string) error

	// --- Embeddings ---

	// SavePageEmbedding stores a vector embedding for a crawled page.
	SavePageEmbedding(ctx context.Context, emb *PageEmbedding) error

	// GetPageEmbeddings returns embeddings for the given page IDs.
	GetPageEmbeddings(ctx context.Context, pageIDs []int64) (map[int64]*PageEmbedding, error)

	// GetEmbeddingStats returns statistics about the embedding index.
	GetEmbeddingStats(ctx context.Context) (totalPages, embeddedPages, avgDimensions int, model string, err error)

	// --- Adaptive Scoring / Interest Profile ---

	// RecordSearchQuery stores a normalized search query and updates interest term weights.
	RecordSearchQuery(ctx context.Context, query, normalizedQuery, lang string) error

	// GetInterestProfile returns the accumulated term weights for adaptive scoring.
	GetInterestProfile(ctx context.Context) (map[string]float64, error)

	// GetDomainReputations returns the reputation score per domain.
	GetDomainReputations(ctx context.Context) (map[string]float64, error)

	// GetSearchQueryCount returns the total number of recorded queries.
	GetSearchQueryCount(ctx context.Context) (int, error)

	// UpdateDomainReputation adjusts the relevance score for a domain.
	UpdateDomainReputation(ctx context.Context, domain string, relevanceDelta float64) error

	// BoostTermsFromClick increases term weights based on a clicked result's content.
	BoostTermsFromClick(ctx context.Context, query string, pageURL string, clickWeight float64) error

	// SetInterestTerm inserts or accumulates a manual interest term weight.
	SetInterestTerm(ctx context.Context, term string, weight float64, source string, lang string) error

	// RemoveInterestTerm deletes a manual interest term.
	RemoveInterestTerm(ctx context.Context, term string, source string) error

	// GetManualInterests returns interest terms with the given source (e.g. 'manual').
	GetManualInterests(ctx context.Context, source string) ([]InterestTermRecord, error)

	// Close gracefully shuts down the storage layer.
	Close() error
}
