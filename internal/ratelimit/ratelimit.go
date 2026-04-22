// Package ratelimit provides per-domain request rate limiting to ensure
// polite crawling behavior. Each domain has an independent rate that
// prevents the crawler from overwhelming any single server.
package ratelimit

import (
	"sync"
	"time"
)

// DomainLimiter enforces a minimum delay between requests to the same domain.
// It is safe for concurrent use by multiple goroutines.
type DomainLimiter struct {
	mu       sync.Mutex
	domains  map[string]time.Time // domain → timestamp of last request
	delay    time.Duration        // minimum delay between requests to the same domain
	cleanupT *time.Ticker         // periodic cleanup of stale entries
	done     chan struct{}
}

// NewDomainLimiter creates a new per-domain rate limiter with the given
// minimum delay between requests. It starts a background goroutine that
// periodically cleans up stale domain entries to prevent memory leaks.
func NewDomainLimiter(delay time.Duration) *DomainLimiter {
	dl := &DomainLimiter{
		domains: make(map[string]time.Time),
		delay:   delay,
		done:    make(chan struct{}),
	}

	// Clean up domains not accessed in the last 10 minutes, every 5 minutes.
	dl.cleanupT = time.NewTicker(5 * time.Minute)
	go dl.cleanupLoop()

	return dl
}

// Wait blocks until it is safe to make a request to the given domain,
// respecting the configured politeness delay. Returns immediately if
// enough time has passed since the last request to this domain.
func (dl *DomainLimiter) Wait(domain string) {
	dl.mu.Lock()
	lastAccess, exists := dl.domains[domain]
	now := time.Now()

	if !exists {
		// First request to this domain — allow immediately.
		dl.domains[domain] = now
		dl.mu.Unlock()
		return
	}

	elapsed := now.Sub(lastAccess)
	if elapsed >= dl.delay {
		// Enough time has passed — allow immediately.
		dl.domains[domain] = now
		dl.mu.Unlock()
		return
	}

	// Need to wait for the remaining delay.
	waitDuration := dl.delay - elapsed
	dl.mu.Unlock()

	time.Sleep(waitDuration)

	// Update the last access time after waiting.
	dl.mu.Lock()
	dl.domains[domain] = time.Now()
	dl.mu.Unlock()
}

// TryWait checks if a request to the given domain is allowed right now
// without blocking. If the domain is ready (first request, or enough
// time has elapsed since the last request), it reserves the slot and
// returns true. If the domain is still rate-limited, it returns false
// immediately — the caller should try a different domain.
func (dl *DomainLimiter) TryWait(domain string) bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	lastAccess, exists := dl.domains[domain]
	now := time.Now()

	if !exists || now.Sub(lastAccess) >= dl.delay {
		dl.domains[domain] = now
		return true
	}

	return false
}

// cleanupLoop removes domain entries that haven't been accessed recently
// to prevent unbounded memory growth.
func (dl *DomainLimiter) cleanupLoop() {
	for {
		select {
		case <-dl.done:
			return
		case <-dl.cleanupT.C:
			dl.cleanup()
		}
	}
}

func (dl *DomainLimiter) cleanup() {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	cutoff := time.Now().Add(-10 * time.Minute)
	for domain, lastAccess := range dl.domains {
		if lastAccess.Before(cutoff) {
			delete(dl.domains, domain)
		}
	}
}

// Close stops the background cleanup goroutine and releases resources.
func (dl *DomainLimiter) Close() {
	dl.cleanupT.Stop()
	close(dl.done)
}
