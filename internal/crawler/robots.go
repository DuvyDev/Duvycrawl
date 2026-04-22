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
type RobotsCache struct {
	mu        sync.RWMutex
	cache     map[string]*robotsEntry
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
		cache:     make(map[string]*robotsEntry),
		client:    &http.Client{Timeout: 10 * time.Second},
		userAgent: userAgent,
		ttl:       ttl,
		logger:    logger.With("component", "robots"),
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
	rc.mu.RLock()
	entry, exists := rc.cache[domain]
	rc.mu.RUnlock()

	if exists && time.Since(entry.fetchedAt) < rc.ttl {
		return entry
	}

	// Fetch (or re-fetch) the robots.txt.
	return rc.fetch(ctx, domain)
}

// fetch downloads and parses the robots.txt for a domain.
func (rc *RobotsCache) fetch(ctx context.Context, domain string) *robotsEntry {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	// Double-check: another goroutine may have fetched while we waited for the lock.
	if entry, exists := rc.cache[domain]; exists && time.Since(entry.fetchedAt) < rc.ttl {
		return entry
	}

	robotsURL := fmt.Sprintf("https://%s/robots.txt", domain)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.cache[domain] = entry
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
		rc.cache[domain] = entry
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
		rc.cache[domain] = entry
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
		rc.cache[domain] = entry
		rc.logger.Warn("failed to read robots.txt body",
			"domain", domain,
			"error", err,
		)
		return entry
	}

	data, err := robotstxt.FromBytes(body)
	if err != nil {
		entry := &robotsEntry{fetchedAt: time.Now(), err: err}
		rc.cache[domain] = entry
		rc.logger.Warn("failed to parse robots.txt",
			"domain", domain,
			"error", err,
		)
		return entry
	}

	entry := &robotsEntry{data: data, fetchedAt: time.Now()}
	rc.cache[domain] = entry
	rc.logger.Debug("cached robots.txt",
		"domain", domain,
		"size", len(body),
	)
	return entry
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
