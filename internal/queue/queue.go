// Package queue provides a concurrent, in-memory priority queue with
// per-domain fairness for the crawler's URL frontier. Unlike the previous
// SQLite-backed queue, all enqueue/dequeue operations happen in memory,
// eliminating database contention from the hot path.
//
// URL deduplication is performed by a Bloom filter instead of a naive map,
// reducing memory usage from ~100+ bytes per URL to ~2 bits per URL.
// With the default settings (10M capacity, 0.1% false-positive rate) the
// filter consumes ~18 MB regardless of how many URLs have been inserted.
package queue

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

// bloomCapacity is the expected number of unique URLs the Bloom filter
// should handle before the false-positive rate starts to degrade.
const bloomCapacity uint = 10_000_000

// bloomFPP is the target false-positive probability. 0.001 = 0.1%.
// In practice this means ~1 in 1,000 truly new URLs may be incorrectly
// treated as a duplicate and skipped. This is an acceptable trade-off
// for the massive memory savings.
const bloomFPP float64 = 0.001

// Job represents a URL queued for crawling.
type Job struct {
	ID          int64
	URL         string
	Domain      string
	Depth       int
	Priority    int    // Higher = more urgent.
	Fingerprint string // Structural fingerprint for deduplication.
	Retries     int    // Number of times this job has been retried.
}

// Stats provides a snapshot of the queue state.
type Stats struct {
	Pending  int   `json:"pending"`
	Domains  int   `json:"domains"`
	Enqueued int64 `json:"total_enqueued"`
	Dequeued int64 `json:"total_dequeued"`
}

// DomainReadyFunc is called by Dequeue to check if a domain's rate limit
// has expired. If it returns true, a job from that domain can be dispatched.
// It should also reserve the rate limit slot (like TryWait).
type DomainReadyFunc func(domain string) bool

// Queue is a concurrent, in-memory priority queue organized by domain.
// Workers call Dequeue with a domain-readiness function, and the queue
// finds the highest-priority job from any ready domain — entirely in
// memory, with no database round-trips.
//
// Deduplication uses a Bloom filter for O(1) constant-time membership
// tests with minimal memory footprint.
type Queue struct {
	mu      sync.Mutex
	cond    *sync.Cond         // wakes ALL waiting workers on new jobs
	domains map[string][]*Job  // domain → jobs sorted by priority desc
	bloom   *bloom.BloomFilter // URL deduplication via Bloom filter
	nextID  atomic.Int64       // auto-increment job ID
	round   atomic.Uint64      // dequeue round counter for random offset

	// workerWaiters tracks how many workers are currently blocked on cond.Wait.
	// Used to avoid unnecessary Broadcast when nobody is waiting.
	workerWaiters int

	// candidateBuf is reused across dequeueLocked calls to avoid allocating
	// a new slice on every dequeue (reduces GC pressure under high concurrency).
	candidateBuf []candidate

	// Metrics.
	enqueued atomic.Int64
	dequeued atomic.Int64
}

// New creates a new empty in-memory queue with a Bloom filter sized for
// 10 million URLs at a 0.1% false-positive rate (~18 MB).
func New() *Queue {
	q := &Queue{
		domains: make(map[string][]*Job),
		bloom:   bloom.NewWithEstimates(bloomCapacity, bloomFPP),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// dedupKey returns the fingerprint if available, otherwise the raw URL.
func dedupKey(job *Job) string {
	if job.Fingerprint != "" {
		return job.Fingerprint
	}
	return job.URL
}

// Enqueue adds a job to the queue if the URL hasn't been seen before.
// Returns true if the job was added, false if it was a duplicate.
// This is safe for concurrent use.
func (q *Queue) Enqueue(job *Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := dedupKey(job)
	if q.bloom.TestString(key) {
		return false
	}

	job.ID = q.nextID.Add(1)
	q.bloom.AddString(key)

	// Insert into the domain's queue, maintaining priority order (desc).
	q.domains[job.Domain] = insertSorted(q.domains[job.Domain], job)
	q.enqueued.Add(1)

	// Wake up waiting workers only if someone is actually blocked.
	if q.workerWaiters > 0 {
		q.cond.Broadcast()
	}

	return true
}

// EnqueueBatch adds multiple jobs, skipping duplicates. Returns the
// number of jobs actually enqueued.
func (q *Queue) EnqueueBatch(jobs []*Job) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	added := 0
	for _, job := range jobs {
		key := dedupKey(job)
		if q.bloom.TestString(key) {
			continue
		}

		job.ID = q.nextID.Add(1)
		q.bloom.AddString(key)
		q.domains[job.Domain] = insertSorted(q.domains[job.Domain], job)
		added++
	}

	q.enqueued.Add(int64(added))

	// Wake up waiting workers only if someone is blocked and we added work.
	if added > 0 && q.workerWaiters > 0 {
		q.cond.Broadcast()
	}

	return added
}

// EnqueueRetry adds a job back to the queue without checking the Bloom filter.
// Use this when re-queuing a job that previously failed, so retries are not
// deduplicated away.
func (q *Queue) EnqueueRetry(job *Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	job.ID = q.nextID.Add(1)
	q.domains[job.Domain] = insertSorted(q.domains[job.Domain], job)
	q.enqueued.Add(1)

	// Wake up waiting workers only if someone is blocked.
	if q.workerWaiters > 0 {
		q.cond.Broadcast()
	}

	return true
}

// Dequeue finds the highest-priority job from any domain where
// readyFn returns true. It sorts candidates by priority first, then
// checks readiness in order — calling readyFn only once (on the first
// ready domain) to avoid wasting rate limit reservations.
//
// This entire operation happens in memory — zero DB calls.
func (q *Queue) Dequeue(readyFn DomainReadyFunc) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.dequeueLocked(readyFn)
}

// StartPeriodicWake runs a single background goroutine that broadcasts
// to all waiting workers every 500ms. This handles the case where rate
// limiter slots free up asynchronously but no new URLs are being enqueued.
// Call this once from the engine, NOT per-worker.
func (q *Queue) StartPeriodicWake(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				q.cond.Broadcast()
				return
			case <-ticker.C:
				q.cond.Broadcast()
			}
		}
	}()
}

// DequeueWithWait blocks until a ready job is available or the context
// is cancelled. Uses sync.Cond to efficiently suspend workers.
//
// Workers are woken by:
//   - Enqueue/EnqueueBatch/EnqueueRetry → Broadcast (new work arrived)
//   - StartPeriodicWake ticker → Broadcast (rate limiter slots freed)
//   - Context cancellation → Broadcast (shutdown)
func (q *Queue) DequeueWithWait(ctx context.Context, readyFn DomainReadyFunc) *Job {
	for {
		if ctx.Err() != nil {
			return nil
		}

		q.mu.Lock()
		job := q.dequeueLocked(readyFn)
		if job != nil {
			q.mu.Unlock()
			return job
		}
		// No ready job — wait for Broadcast from Enqueue, periodic tick,
		// or context cancel.
		// cond.Wait() atomically releases q.mu and suspends the goroutine.
		q.workerWaiters++
		q.cond.Wait()
		q.workerWaiters--
		q.mu.Unlock()
	}
}

// dequeueLocked is the internal dequeue logic; caller must hold q.mu.
//
// Instead of sorting all domains (O(n log n) under lock), it uses a
// two-pass approach:
//  1. Scan all domains to find the max priority.
//  2. Iterate from a random offset, trying only domains at max priority.
//     If none are ready, try any domain.
//
// The random offset prevents thundering herd where all 300 workers
// compete for the same high-priority domain.
// candidate represents a domain with at least one pending job.
type candidate struct {
	domain   string
	priority int
}

func (q *Queue) dequeueLocked(readyFn DomainReadyFunc) *Job {
	n := len(q.domains)
	if n == 0 {
		return nil
	}

	// Reuse the internal buffer to avoid allocating a new slice on every
	// dequeue call (significant GC savings with hundreds of workers).
	q.candidateBuf = q.candidateBuf[:0]
	maxPriority := -1
	for domain, jobs := range q.domains {
		if len(jobs) > 0 {
			p := jobs[0].Priority
			q.candidateBuf = append(q.candidateBuf, candidate{domain, p})
			if p > maxPriority {
				maxPriority = p
			}
		}
	}

	if len(q.candidateBuf) == 0 {
		return nil
	}

	// Use an atomic counter as a cheap pseudo-random offset so workers
	// start scanning from different positions, spreading load across domains.
	offset := int(q.round.Add(1)) % len(q.candidateBuf)

	// Pass 1: try domains at max priority (from random offset).
	for i := 0; i < len(q.candidateBuf); i++ {
		c := q.candidateBuf[(offset+i)%len(q.candidateBuf)]
		if c.priority != maxPriority {
			continue
		}
		if readyFn(c.domain) {
			return q.popJob(c.domain)
		}
	}

	// Pass 2: try any domain (from random offset) — lower priority is
	// better than no work at all.
	for i := 0; i < len(q.candidateBuf); i++ {
		c := q.candidateBuf[(offset+i)%len(q.candidateBuf)]
		if c.priority == maxPriority {
			continue // already tried in pass 1
		}
		if readyFn(c.domain) {
			return q.popJob(c.domain)
		}
	}

	return nil
}

// popJob removes and returns the head job from a domain's queue.
// Caller must hold q.mu.
func (q *Queue) popJob(domain string) *Job {
	jobs := q.domains[domain]
	job := jobs[0]
	q.domains[domain] = jobs[1:]
	if len(q.domains[domain]) == 0 {
		delete(q.domains, domain)
	}
	q.dequeued.Add(1)
	return job
}

// MarkSeen adds a URL (or its fingerprint) to the Bloom filter without
// enqueuing it. Use this for URLs already crawled (loaded from the database
// on startup).
func (q *Queue) MarkSeen(key string) {
	q.mu.Lock()
	q.bloom.AddString(key)
	q.mu.Unlock()
}

// HasSeen checks if a key (URL or fingerprint) has been seen by the Bloom
// filter without adding it. Returns true if the key was probably seen before
// (with the configured false-positive rate). This allows callers to skip
// expensive DB lookups for URLs that are almost certainly duplicates.
func (q *Queue) HasSeen(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bloom.TestString(key)
}

// Len returns the total number of pending jobs across all domains.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	total := 0
	for _, jobs := range q.domains {
		total += len(jobs)
	}
	return total
}

// Stats returns a snapshot of the queue metrics.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	domainCount := len(q.domains)
	pending := 0
	for _, jobs := range q.domains {
		pending += len(jobs)
	}
	q.mu.Unlock()

	return Stats{
		Pending:  pending,
		Domains:  domainCount,
		Enqueued: q.enqueued.Load(),
		Dequeued: q.dequeued.Load(),
	}
}

// insertSorted inserts a job into a slice maintaining descending priority order.
func insertSorted(jobs []*Job, job *Job) []*Job {
	i := sort.Search(len(jobs), func(i int) bool {
		return jobs[i].Priority < job.Priority
	})
	// Insert at position i.
	jobs = append(jobs, nil)
	copy(jobs[i+1:], jobs[i:])
	jobs[i] = job
	return jobs
}
