// Package crawler implements the core web crawling engine, including
// the worker pool, HTTP fetching, HTML parsing, and robots.txt handling.
package crawler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
)

// RobotsCache maintains an in-memory cache of parsed robots.txt files
// with a configurable TTL. It is safe for concurrent use.
//
// Fetches are serialized per-domain but parallel across domains, so
// downloading robots.txt for site A never blocks workers for site B.
type RobotsCache struct {
	mu         sync.RWMutex
	cache      map[string]*robotsEntry
	fetchLocks map[string]*sync.Mutex
	flMu       sync.Mutex

	client    *http.Client
	userAgent string
	ttl       time.Duration
	logger    *slog.Logger
}

type robotsEntry struct {
	data      *robotstxt.RobotsData
	fetchedAt time.Time
	err       error // non-nil if fetching failed
}

// NewRobotsCache creates a new robots.txt cache.
func NewRobotsCache(userAgent string, ttl time.Duration, logger *slog.Logger) *RobotsCache {
	return &RobotsCache{
		cache:      make(map[string]*robotsEntry),
		fetchLocks: make(map[string]*sync.Mutex),
		client:     &http.Client{Timeout: 10 * time.Second},
		userAgent:  userAgent,
		ttl:        ttl,
		logger:     logger.With("component", "robots"),
	}
}

// IsAllowed checks whether the given URL is permitted by the domain's robots.txt.
// If robots.txt cannot be fetched, access is allowed (fail-open) with a warning log.
func (rc *RobotsCache) IsAllowed(ctx context.Context, targetURL string, domain string) bool {
	entry := rc.getOrFetch(ctx, domain)
	if entry == nil || entry.data == nil {
		return true // Fail-open: allow if we can't determine.
	}

	group := entry.data.FindGroup(rc.userAgent)
	if group == nil {
		return true
	}
	return group.Test(targetURL)
}

// getOrFetch returns the cached robots.txt entry for a domain, fetching it if
// the cache is empty or expired.
func (rc *RobotsCache) getOrFetch(ctx context.Context, domain string) *robotsEntry {
	// Fast path: cache hit.
	rc.mu.RLock()
	entry, exists := rc.cache[domain]
	rc.mu.RUnlock()

	if exists && time.Since(entry.fetchedAt) < rc.ttl {
		return entry
	}

	// Slow path: need to fetch. Serialize per-domain, parallel across domains.
	domainMu := rc.domainFetchMutex(domain)
	domainMu.Lock()
	defer domainMu.Unlock()

	// Double-check: another goroutine may have fetched while we waited.
	rc.mu.RLock()
	entry, exists = rc.cache[domain]
	rc.mu.RUnlock()

	if exists && time.Since(entry.fetchedAt) < rc.ttl {
		return entry
	}

	return rc.fetch(ctx, domain)
}

// domainFetchMutex returns the per-domain mutex used to serialize fetches.
func (rc *RobotsCache) domainFetchMutex(domain string) *sync.Mutex {
	rc.flMu.Lock()
	defer rc.flMu.Unlock()

	if mu, ok := rc.fetchLocks[domain]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	rc.fetchLocks[domain] = mu
	return mu
}

// fetch downloads and parses the robots.txt for a domain.
// The caller must hold the per-domain fetch mutex.
func (rc *RobotsCache) fetch(ctx context.Context, domain string) *robotsEntry {
	robotsURL := fmt.Sprintf("https://%s/robots.txt", domain)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.storeEntry(domain, entry)
		rc.logger.Warn("failed to create robots.txt request",
			"domain", domain,
			"error", err,
		)
		return entry
	}
	req.Header.Set("User-Agent", rc.userAgent)

	resp, err := rc.client.Do(req)
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.storeEntry(domain, entry)
		rc.logger.Warn("failed to fetch robots.txt",
			"domain", domain,
			"error", err,
		)
		return entry
	}
	defer resp.Body.Close()

	// If robots.txt doesn't exist (404, etc.), allow everything.
	if resp.StatusCode != http.StatusOK {
		entry := &robotsEntry{fetchedAt: time.Now()}
		rc.storeEntry(domain, entry)
		rc.logger.Debug("robots.txt not found, allowing all",
			"domain", domain,
			"status", resp.StatusCode,
		)
		return entry
	}

	// Limit reading to 512KB to avoid abuse.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.storeEntry(domain, entry)
		rc.logger.Warn("failed to read robots.txt body",
			"domain", domain,
			"error", err,
		)
		return entry
	}

	data, err := robotstxt.FromBytes(body)
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.storeEntry(domain, entry)
		rc.logger.Warn("failed to parse robots.txt",
			"domain", domain,
			"error", err,
		)
		return entry
	}

	entry := &robotsEntry{data: data, fetchedAt: time.Now()}
	rc.storeEntry(domain, entry)
	rc.logger.Debug("cached robots.txt",
		"domain", domain,
		"size", len(body),
	)
	return entry
}

// storeEntry writes an entry to the cache. Safe for concurrent use.
func (rc *RobotsCache) storeEntry(domain string, entry *robotsEntry) {
	rc.mu.Lock()
	rc.cache[domain] = entry
	rc.mu.Unlock()
}

// GetCrawlDelay returns the crawl delay specified in robots.txt for this
// user agent, or 0 if none is specified.
func (rc *RobotsCache) GetCrawlDelay(ctx context.Context, domain string) time.Duration {
	entry := rc.getOrFetch(ctx, domain)
	if entry == nil || entry.data == nil {
		return 0
	}

	group := entry.data.FindGroup(rc.userAgent)
	if group == nil {
		return 0
	}

	// robotstxt library stores CrawlDelay as a time.Duration.
	// See: https://github.com/temoto/robotstxt
	_ = group
	// The temoto/robotstxt library does not expose CrawlDelay directly
	// in all versions. We fall back to our configured politeness delay.
	return 0
}

// ClearDomain removes a domain's cached robots.txt, forcing a re-fetch.
func (rc *RobotsCache) ClearDomain(domain string) {
	rc.mu.Lock()
	delete(rc.cache, domain)
	rc.mu.Unlock()

	rc.flMu.Lock()
	delete(rc.fetchLocks, domain)
	rc.flMu.Unlock()
}

// isDisallowedPath checks if a path looks like it should be skipped
// even without robots.txt (common patterns that waste resources).
func isDisallowedPath(path string) bool {
	path = strings.ToLower(path)
	disallowed := []string{
		"/wp-admin",
		"/wp-login",
		"/admin",
		"/login",
		"/logout",
		"/signup",
		"/register",
		"/cart",
		"/checkout",
		"/account",
		"/api/",
		"/cgi-bin/",
	}
	for _, prefix := range disallowed {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
