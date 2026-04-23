// Package frontier implements a priority-based URL queue with deduplication
// and domain-level fairness. It wraps the in-memory queue and handles
// URL normalization and deduplication against already-crawled pages.
package frontier

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
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
	normalized, domain, err := CanonicalizeURL(rawURL)
	if err != nil {
		return nil // Silently ignore unparseable URLs.
	}

	fingerprint := FingerprintURL(normalized)

	// Check exact URL first (fast path).
	existing, err := f.store.GetPageByURL(ctx, normalized)
	if err != nil {
		return fmt.Errorf("checking existing page: %w", err)
	}
	if existing != nil {
		f.queue.MarkSeen(fingerprint)
		return nil
	}

	// Check structural fingerprint (catches URLs with different query values).
	existingFingerprint, err := f.store.GetPageByFingerprint(ctx, fingerprint)
	if err != nil {
		return fmt.Errorf("checking fingerprint: %w", err)
	}
	if existingFingerprint != nil {
		f.queue.MarkSeen(fingerprint)
		return nil
	}

	f.queue.Enqueue(&queue.Job{
		URL:         normalized,
		Domain:      domain,
		Depth:       depth,
		Priority:    priority,
		Fingerprint: fingerprint,
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
		normalized, domain, err := CanonicalizeURL(rawURL)
		if err != nil {
			continue
		}

		fingerprint := FingerprintURL(normalized)

		// Skip URLs already crawled (exact match or structural match).
		existing, _ := f.store.GetPageByURL(ctx, normalized)
		if existing != nil {
			f.queue.MarkSeen(fingerprint)
			continue
		}

		existingFingerprint, _ := f.store.GetPageByFingerprint(ctx, fingerprint)
		if existingFingerprint != nil {
			f.queue.MarkSeen(fingerprint)
			continue
		}

		jobs = append(jobs, &queue.Job{
			URL:         normalized,
			Domain:      domain,
			Depth:       depth,
			Priority:    priority,
			Fingerprint: fingerprint,
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
		normalized, domain, err := CanonicalizeURL(rawURL)
		if err != nil {
			continue
		}

		fingerprint := FingerprintURL(normalized)
		jobs = append(jobs, &queue.Job{
			URL:         normalized,
			Domain:      domain,
			Depth:       depth,
			Priority:    priority,
			Fingerprint: fingerprint,
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

// DequeueWithWait blocks until a ready job is available or the context is
// cancelled. It is much more efficient than polling Dequeue in a loop.
func (f *Frontier) DequeueWithWait(ctx context.Context, readyFn queue.DomainReadyFunc) *queue.Job {
	return f.queue.DequeueWithWait(ctx, readyFn)
}

// Retry re-enqueues a failed job for another attempt, bypassing the
// Bloom filter so the retry is not deduplicated away.
func (f *Frontier) Retry(job *queue.Job) {
	f.queue.EnqueueRetry(job)
}

// Stats returns the current queue statistics.
func (f *Frontier) Stats() queue.Stats {
	return f.queue.Stats()
}

// CanonicalizeURL parses and normalizes a URL for consistent deduplication.
// Returns the canonical URL string and the normalized domain name.
func CanonicalizeURL(rawURL string) (string, string, error) {
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
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = hostname + ":" + port
	} else {
		parsed.Host = hostname
	}
	parsed.Fragment = ""                              // Remove fragments.
	parsed.Path = strings.TrimRight(parsed.Path, "/") // Remove trailing slashes.
	if parsed.Path == "" {
		parsed.Path = "/"
	}

	// Remove tracking/auth query parameters and sort the rest.
	query := parsed.Query()
	if len(query) > 0 {
		cleaned := make(url.Values, len(query))
		for k, values := range query {
			if shouldDropQueryParam(k) {
				continue
			}
			cleaned[k] = values
		}

		if len(cleaned) == 0 {
			parsed.RawQuery = ""
		} else {
			for k := range cleaned {
				sort.Strings(cleaned[k])
			}
			parsed.RawQuery = cleaned.Encode()
		}
	}

	domain := parsed.Hostname()
	// Remove "www." prefix for consistent domain matching.
	domain = strings.TrimPrefix(domain, "www.")

	return parsed.String(), domain, nil
}

func shouldDropQueryParam(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return true
	}

	if strings.HasPrefix(k, "utm_") {
		return true
	}

	if strings.HasPrefix(k, "_hs") || strings.HasPrefix(k, "mkt_") {
		return true
	}

	if strings.Contains(k, "token") || strings.Contains(k, "session") || strings.Contains(k, "signature") {
		return true
	}

	if strings.HasPrefix(k, "x-amz-") {
		return true
	}

	switch k {
	case "fbclid", "gclid", "dclid", "msclkid", "yclid", "igshid",
		"mc_cid", "mc_eid", "zanpid", "_gl",
		"auth", "sid", "phpsessid", "jsessionid",
		"expires", "expiry", "exp":
		return true
	}

	return false
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
