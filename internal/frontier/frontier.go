// Package frontier implements a priority-based URL queue with deduplication
// and domain-level fairness. It wraps the in-memory queue and handles
// URL normalization and deduplication against already-crawled pages.
package frontier

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// Frontier manages the URL crawl queue, providing deduplication and
// priority-based dispatching of URLs to crawler workers.
type Frontier struct {
	queue  *queue.Queue
	store  storage.Storage
	logger *slog.Logger
}

// New creates a new Frontier backed by the given in-memory queue
// and storage (used only for checking already-crawled pages).
func New(q *queue.Queue, store storage.Storage, logger *slog.Logger) *Frontier {
	return &Frontier{
		queue:  q,
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
		// Already crawled — mark as seen so the queue skips it too.
		f.queue.MarkSeen(normalized)
		return nil
	}

	f.queue.Enqueue(&queue.Job{
		URL:      normalized,
		Domain:   domain,
		Depth:    depth,
		Priority: priority,
	})

	return nil
}

// AddBatch enqueues multiple URLs for crawling, skipping URLs that have
// already been crawled (exist in the pages table). Use this for discovered
// links during crawling to avoid re-fetching known pages.
func (f *Frontier) AddBatch(ctx context.Context, rawURLs []string, depth, priority int) error {
	if len(rawURLs) == 0 {
		return nil
	}

	var jobs []*queue.Job
	for _, rawURL := range rawURLs {
		normalized, domain, err := normalizeURL(rawURL)
		if err != nil {
			continue
		}

		// Skip URLs already crawled in a previous session.
		existing, _ := f.store.GetPageByURL(ctx, normalized)
		if existing != nil {
			f.queue.MarkSeen(normalized)
			continue
		}

		jobs = append(jobs, &queue.Job{
			URL:      normalized,
			Domain:   domain,
			Depth:    depth,
			Priority: priority,
		})
	}

	if len(jobs) == 0 {
		return nil
	}

	added := f.queue.EnqueueBatch(jobs)
	if added > 0 {
		f.logger.Debug("batch enqueued URLs",
			"submitted", len(jobs),
			"added", added,
			"depth", depth,
			"priority", priority,
		)
	}
	return nil
}

// AddBatchDirect enqueues URLs without checking the pages table.
// Use this for seed injection (startup), scheduler re-crawls, and
// API-requested crawls — cases where we explicitly want to (re)crawl.
func (f *Frontier) AddBatchDirect(ctx context.Context, rawURLs []string, depth, priority int) error {
	if len(rawURLs) == 0 {
		return nil
	}

	var jobs []*queue.Job
	for _, rawURL := range rawURLs {
		normalized, domain, err := normalizeURL(rawURL)
		if err != nil {
			continue
		}

		jobs = append(jobs, &queue.Job{
			URL:      normalized,
			Domain:   domain,
			Depth:    depth,
			Priority: priority,
		})
	}

	if len(jobs) == 0 {
		return nil
	}

	added := f.queue.EnqueueBatch(jobs)
	if added > 0 {
		f.logger.Debug("batch enqueued URLs (direct)",
			"submitted", len(jobs),
			"added", added,
			"depth", depth,
			"priority", priority,
		)
	}
	return nil
}

// Dequeue finds the next job whose domain is ready according to readyFn.
// The entire operation happens in memory — no database calls.
// Returns nil if no ready job exists.
func (f *Frontier) Dequeue(readyFn queue.DomainReadyFunc) *queue.Job {
	return f.queue.Dequeue(readyFn)
}

// Stats returns the current queue statistics.
func (f *Frontier) Stats() queue.Stats {
	return f.queue.Stats()
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
