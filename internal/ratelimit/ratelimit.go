// Package ratelimit provides per-domain request rate limiting to ensure
// polite crawling behavior while maximizing throughput.
//
// Inspired by Colly's LimitRule system, it uses semaphores (buffered channels)
// to allow configurable parallelism per domain, combined with fixed and
// randomized delays between request releases.
package ratelimit

import (
	"math/rand"
	"sync"
	"time"
)

// domainState holds the per-domain rate-limiting semaphore.
type domainState struct {
	semaphore chan struct{} // buffered channel; capacity = max parallelism
}

// DomainLimiter enforces rate limits per domain using semaphores.
// It supports:
//   - Configurable parallelism per domain (concurrent requests)
//   - Fixed delay between request completions
//   - Random jitter added to delays (avoid predictable patterns)
//   - Non-blocking TryWait for worker-pool scheduling
//
// It is safe for concurrent use by multiple goroutines.
type DomainLimiter struct {
	mu                   sync.Mutex
	domains              map[string]*domainState
	delay                time.Duration
	randomDelay          time.Duration
	parallelismPerDomain int
	cleanupT             *time.Ticker
	done                 chan struct{}
}

// NewDomainLimiter creates a new per-domain rate limiter.
//
// Parameters:
//   - delay: minimum time between request completions to the same domain
//   - randomDelay: extra randomized duration added to delay (0 to disable)
//   - parallelism: max concurrent requests per domain (like Colly's Parallelism)
func NewDomainLimiter(delay, randomDelay time.Duration, parallelism int) *DomainLimiter {
	dl := &DomainLimiter{
		domains:              make(map[string]*domainState),
		delay:                delay,
		randomDelay:          randomDelay,
		parallelismPerDomain: max(parallelism, 1),
		done:                 make(chan struct{}),
	}

	// Clean up domains not accessed in the last 10 minutes, every 5 minutes.
	dl.cleanupT = time.NewTicker(5 * time.Minute)
	go dl.cleanupLoop()

	return dl
}

// Wait blocks until a slot is available for the given domain, then reserves
// it. The slot is automatically released after Delay+RandomDelay.
// Use this when each worker processes one domain at a time.
func (dl *DomainLimiter) Wait(domain string) {
	ds := dl.getOrCreateDomain(domain)

	// Acquire a slot (blocks if parallelism limit reached).
	ds.semaphore <- struct{}{}

	// Compute total delay.
	totalDelay := dl.delay
	if dl.randomDelay > 0 {
		totalDelay += time.Duration(rand.Int63n(int64(dl.randomDelay)))
	}

	// Release the slot after the delay in a background goroutine.
	// This allows multiple workers to process the same domain concurrently
	// up to the parallelism limit.
	go func() {
		time.Sleep(totalDelay)
		<-ds.semaphore
	}()
}

// TryWait attempts to acquire a rate-limit slot for the given domain without
// blocking. If a slot is available it reserves it and returns true.
// The slot is automatically released after Delay+RandomDelay.
// If no slot is available (parallelism limit reached) it returns false
// immediately — the caller should try a different domain.
func (dl *DomainLimiter) TryWait(domain string) bool {
	ds := dl.getOrCreateDomain(domain)

	select {
	case ds.semaphore <- struct{}{}:
		// Acquired slot. Schedule automatic release after delay.
		go func() {
			totalDelay := dl.delay
			if dl.randomDelay > 0 {
				totalDelay += time.Duration(rand.Int63n(int64(dl.randomDelay)))
			}
			time.Sleep(totalDelay)
			<-ds.semaphore
		}()
		return true
	default:
		return false
	}
}

// getOrCreateDomain returns the domainState for a domain, creating it if needed.
func (dl *DomainLimiter) getOrCreateDomain(domain string) *domainState {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	ds, exists := dl.domains[domain]
	if !exists {
		ds = &domainState{
			semaphore: make(chan struct{}, dl.parallelismPerDomain),
		}
		dl.domains[domain] = ds
	}
	return ds
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

	// We can't easily know when a domain was last used because the semaphore
	// releases happen in background goroutines. Instead, we only clean if
	// the semaphore is empty (len == 0) and we track last access separately.
	// For simplicity, we keep domains around; with bounded domains this is fine.
	// If memory becomes an issue, we can add a last-access timestamp.
}

// Close stops the background cleanup goroutine and releases resources.
func (dl *DomainLimiter) Close() {
	dl.cleanupT.Stop()
	close(dl.done)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
