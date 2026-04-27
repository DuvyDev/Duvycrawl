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
	"github.com/DuvyDev/Duvycrawl/internal/scorer"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// LinkContext carries metadata about a discovered link that the scorer can
// use to estimate relevance *before* the page is crawled.
type LinkContext struct {
	URL              string
	AnchorText       string
	SourcePageTitle  string
	SourceSchemaType string
	SourceLanguage   string
}

// Frontier manages the URL crawl queue, providing deduplication and
// priority-based dispatching of URLs to crawler workers.
type Frontier struct {
	queue  *queue.Queue
	store  storage.Storage
	scorer scorer.Scorer
	logger *slog.Logger
}

// New creates a new Frontier backed by the given in-memory queue,
// storage (used for checking already-crawled pages), and scorer.
func New(q *queue.Queue, store storage.Storage, sc scorer.Scorer, logger *slog.Logger) *Frontier {
	return &Frontier{
		queue:  q,
		store:  store,
		scorer: sc,
		logger: logger.With("component", "frontier"),
	}
}

// Add enqueues a single URL for crawling with the given depth.
// The URL is normalized and deduplicated (ignored if already in the queue or
// already crawled). The score is computed by the scorer.
func (f *Frontier) Add(ctx context.Context, rawURL string, depth int, baseScore float64) error {
	normalized, domain, err := CanonicalizeURL(rawURL)
	if err != nil {
		return nil // Silently ignore unparseable URLs.
	}

	fingerprint := FingerprintURL(normalized)

	// Fast path: check Bloom filter first (O(1), in-memory, ~0.1% FP rate).
	// This skips the vast majority of DB queries for already-seen URLs.
	if f.queue.HasSeen(fingerprint) {
		return nil
	}

	// Bloom says "not seen" — confirm with DB (handles the 0.1% false negatives
	// where the URL was crawled before the Bloom was populated).
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

	job := &queue.Job{
		URL:         normalized,
		Domain:      domain,
		Depth:       depth,
		BaseScore:   baseScore,
		Fingerprint: fingerprint,
	}
	job.Score = f.scorer.Score(job)
	f.queue.Enqueue(job)

	return nil
}

// AddBatch enqueues multiple URLs for crawling, skipping URLs that have
// already been crawled (exist in the pages table). Use this for discovered
// links during crawling to avoid re-fetching known pages.
//
// Deduplication is performed in two stages:
//  1. Bloom filter (in-memory, O(1)) — catches ~99.9% of duplicates.
//  2. SQLite batch lookup — confirms the remainder in 2 queries instead of 2N.
func (f *Frontier) AddBatch(ctx context.Context, links []LinkContext, depth int, baseScore float64) error {
	if len(links) == 0 {
		return nil
	}

	type candidate struct {
		normalized  string
		domain      string
		fingerprint string
		ctx         LinkContext
	}

	var candidates []candidate
	var urlBatch []string
	var fingerprintBatch []string

	for _, link := range links {
		normalized, domain, err := CanonicalizeURL(link.URL)
		if err != nil {
			continue
		}

		fingerprint := FingerprintURL(normalized)

		// Fast path: check Bloom filter first — avoids ~99.9% of DB queries.
		if f.queue.HasSeen(fingerprint) {
			continue
		}

		candidates = append(candidates, candidate{normalized, domain, fingerprint, link})
		urlBatch = append(urlBatch, normalized)
		fingerprintBatch = append(fingerprintBatch, fingerprint)
	}

	if len(candidates) == 0 {
		return nil
	}

	// Batch confirmation against the database: 2 queries total instead of 2N.
	existingURLs, err := f.store.FilterExistingURLs(ctx, urlBatch)
	if err != nil {
		f.logger.Warn("batch URL deduplication failed, proceeding with Bloom-only filter", "error", err)
		existingURLs = map[string]struct{}{}
	}

	existingFingerprints, err := f.store.FilterExistingFingerprints(ctx, fingerprintBatch)
	if err != nil {
		f.logger.Warn("batch fingerprint deduplication failed, proceeding with Bloom-only filter", "error", err)
		existingFingerprints = map[string]struct{}{}
	}

	var jobs []*queue.Job
	for _, c := range candidates {
		if _, exists := existingURLs[c.normalized]; exists {
			f.queue.MarkSeen(c.fingerprint)
			continue
		}
		if _, exists := existingFingerprints[c.fingerprint]; exists {
			f.queue.MarkSeen(c.fingerprint)
			continue
		}

		job := &queue.Job{
			URL:              c.normalized,
			Domain:           c.domain,
			Depth:            depth,
			BaseScore:        baseScore,
			Fingerprint:      c.fingerprint,
			AnchorText:       c.ctx.AnchorText,
			SourcePageTitle:  c.ctx.SourcePageTitle,
			SourceSchemaType: c.ctx.SourceSchemaType,
			SourceLanguage:   c.ctx.SourceLanguage,
		}
		job.Score = f.scorer.Score(job)
		jobs = append(jobs, job)
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
		)
	}
	return nil
}

// AddBatchDirect enqueues URLs without checking the pages table or the Bloom
// filter. Use this for seed injection (startup), scheduler re-crawls, and
// API-requested crawls — cases where we explicitly want to (re)crawl.
// Returns the number of URLs actually enqueued.
func (f *Frontier) AddBatchDirect(ctx context.Context, rawURLs []string, depth int, baseScore float64) (int, error) {
	if len(rawURLs) == 0 {
		return 0, nil
	}

	var jobs []*queue.Job
	for _, rawURL := range rawURLs {
		normalized, domain, err := CanonicalizeURL(rawURL)
		if err != nil {
			continue
		}

		fingerprint := FingerprintURL(normalized)
		job := &queue.Job{
			URL:         normalized,
			Domain:      domain,
			Depth:       depth,
			BaseScore:   baseScore,
			Fingerprint: fingerprint,
		}
		job.Score = f.scorer.Score(job)
		jobs = append(jobs, job)
	}

	if len(jobs) == 0 {
		return 0, nil
	}

	added := f.queue.EnqueueBatchDirect(jobs)
	if added > 0 {
		f.logger.Debug("batch enqueued URLs (direct)",
			"submitted", len(jobs),
			"added", added,
			"depth", depth,
		)
	}
	return added, nil
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
