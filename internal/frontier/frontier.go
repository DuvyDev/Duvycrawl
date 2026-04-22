// Package frontier implements a priority-based URL queue with deduplication
// and domain-level fairness. It serves as the bridge between the storage
// layer (persistent queue in SQLite) and the crawler workers.
package frontier

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// Frontier manages the URL crawl queue, providing deduplication and
// priority-based dispatching of URLs to crawler workers.
type Frontier struct {
	store  storage.Storage
	logger *slog.Logger
	mu     sync.Mutex
}

// New creates a new Frontier backed by the given storage.
func New(store storage.Storage, logger *slog.Logger) *Frontier {
	return &Frontier{
		store:  store,
		logger: logger.With("component", "frontier"),
	}
}

// Add enqueues a single URL for crawling with the given depth and priority.
// The URL is normalized and deduplicated (ignored if already in the queue or
// already crawled).
func (f *Frontier) Add(ctx context.Context, rawURL string, depth, priority int) error {
	normalized, domain, err := normalizeURL(rawURL)
	if err != nil {
		return nil // Silently ignore unparseable URLs.
	}

	// Check if the page was already crawled.
	existing, err := f.store.GetPageByURL(ctx, normalized)
	if err != nil {
		return fmt.Errorf("checking existing page: %w", err)
	}
	if existing != nil {
		return nil // Already crawled, skip.
	}

	job := &storage.CrawlJob{
		URL:      normalized,
		Domain:   domain,
		Depth:    depth,
		Priority: priority,
	}

	return f.store.EnqueueURL(ctx, job)
}

// AddBatch enqueues multiple URLs for crawling in a single transaction.
// Invalid or already-known URLs are silently skipped.
func (f *Frontier) AddBatch(ctx context.Context, rawURLs []string, depth, priority int) error {
	if len(rawURLs) == 0 {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var jobs []*storage.CrawlJob
	for _, rawURL := range rawURLs {
		normalized, domain, err := normalizeURL(rawURL)
		if err != nil {
			continue
		}

		jobs = append(jobs, &storage.CrawlJob{
			URL:      normalized,
			Domain:   domain,
			Depth:    depth,
			Priority: priority,
		})
	}

	if len(jobs) == 0 {
		return nil
	}

	if err := f.store.EnqueueURLs(ctx, jobs); err != nil {
		return fmt.Errorf("batch enqueue of %d URLs: %w", len(jobs), err)
	}

	f.logger.Debug("batch enqueued URLs",
		"count", len(jobs),
		"depth", depth,
		"priority", priority,
	)
	return nil
}

// Next retrieves the next batch of URLs to crawl, ordered by priority.
func (f *Frontier) Next(ctx context.Context, limit int) ([]*storage.CrawlJob, error) {
	jobs, err := f.store.DequeueURLs(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("dequeuing URLs: %w", err)
	}
	return jobs, nil
}

// NextExcluding retrieves the next batch of URLs to crawl, skipping any
// jobs belonging to the excluded domains. This allows workers to avoid
// domains they know are currently rate-limited.
func (f *Frontier) NextExcluding(ctx context.Context, limit int, excludedDomains []string) ([]*storage.CrawlJob, error) {
	jobs, err := f.store.DequeueURLsExcluding(ctx, limit, excludedDomains)
	if err != nil {
		return nil, fmt.Errorf("dequeuing URLs (excluding %d domains): %w", len(excludedDomains), err)
	}
	return jobs, nil
}

// Complete marks a crawl job as done or failed.
func (f *Frontier) Complete(ctx context.Context, jobID int64, crawlErr error) error {
	return f.store.CompleteJob(ctx, jobID, crawlErr)
}

// Return puts a claimed job back into the pending state so it can be
// picked up by another worker or retried later.
func (f *Frontier) Return(ctx context.Context, jobID int64) error {
	return f.store.ReturnJob(ctx, jobID)
}

// normalizeURL parses and normalizes a URL for consistent deduplication.
// Returns the normalized URL string and the domain name.
func normalizeURL(rawURL string) (string, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", fmt.Errorf("empty URL")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing URL: %w", err)
	}

	// Only accept HTTP(S) schemes.
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", fmt.Errorf("unsupported scheme: %s", scheme)
	}

	// Normalize.
	parsed.Scheme = scheme
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""                          // Remove fragments.
	parsed.Path = strings.TrimRight(parsed.Path, "/") // Remove trailing slashes.
	if parsed.Path == "" {
		parsed.Path = "/"
	}

	domain := parsed.Hostname()
	// Remove "www." prefix for consistent domain matching.
	domain = strings.TrimPrefix(domain, "www.")

	return parsed.String(), domain, nil
}

// ExtractDomain extracts and normalizes the domain from a URL.
func ExtractDomain(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	domain := strings.ToLower(parsed.Hostname())
	return strings.TrimPrefix(domain, "www.")
}
