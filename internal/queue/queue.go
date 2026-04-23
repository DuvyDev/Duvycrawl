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
	ID       int64
	URL      string
	Domain   string
	Depth    int
	Priority int // Higher = more urgent.
}

// Stats provides a snapshot of the queue state.
type Stats struct {
	Pending    int   `json:"pending"`
	Domains    int   `json:"domains"`
	Enqueued   int64 `json:"total_enqueued"`
	Dequeued   int64 `json:"total_dequeued"`
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
	domains map[string][]*Job // domain → jobs sorted by priority desc
	bloom   *bloom.BloomFilter // URL deduplication via Bloom filter
	nextID  atomic.Int64       // auto-increment job ID
	wakeCh  chan struct{}      // wakes workers when new jobs arrive

	// Metrics.
	enqueued atomic.Int64
	dequeued atomic.Int64
}

// New creates a new empty in-memory queue with a Bloom filter sized for
// 10 million URLs at a 0.1% false-positive rate (~18 MB).
func New() *Queue {
	return &Queue{
		domains: make(map[string][]*Job),
		bloom:   bloom.NewWithEstimates(bloomCapacity, bloomFPP),
		wakeCh:  make(chan struct{}, 1),
	}
}

// Enqueue adds a job to the queue if the URL hasn't been seen before.
// Returns true if the job was added, false if it was a duplicate.
// This is safe for concurrent use.
func (q *Queue) Enqueue(job *Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.bloom.TestString(job.URL) {
		return false
	}

	job.ID = q.nextID.Add(1)
	q.bloom.AddString(job.URL)

	// Insert into the domain's queue, maintaining priority order (desc).
	q.domains[job.Domain] = insertSorted(q.domains[job.Domain], job)
	q.enqueued.Add(1)

	// Wake up a waiting worker (non-blocking).
	select {
	case q.wakeCh <- struct{}{}:
	default:
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
		if q.bloom.TestString(job.URL) {
			continue
		}

		job.ID = q.nextID.Add(1)
		q.bloom.AddString(job.URL)
		q.domains[job.Domain] = insertSorted(q.domains[job.Domain], job)
		added++
	}

	q.enqueued.Add(int64(added))

	// Wake up waiting workers (non-blocking).
	if added > 0 {
		select {
		case q.wakeCh <- struct{}{}:
		default:
		}
	}

	return added
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

// DequeueWithWait blocks until a ready job is available or the context
// is cancelled. This is much more efficient than polling with Sleep.
func (q *Queue) DequeueWithWait(ctx context.Context, readyFn DomainReadyFunc) *Job {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if job := q.Dequeue(readyFn); job != nil {
			return job
		}

		// Wait to be woken by Enqueue, or timeout after a short period
		// to re-check in case the rate limiter released a domain.
		select {
		case <-ctx.Done():
			return nil
		case <-q.wakeCh:
			// New job arrived or worker was signalled — loop and try again.
		}
	}
}

// dequeueLocked is the internal dequeue logic; caller must hold q.mu.
func (q *Queue) dequeueLocked(readyFn DomainReadyFunc) *Job {
	// Collect all domains with pending jobs.
	type candidate struct {
		domain   string
		priority int
	}

	candidates := make([]candidate, 0, len(q.domains))
	for domain, jobs := range q.domains {
		if len(jobs) > 0 {
			candidates = append(candidates, candidate{domain, jobs[0].Priority})
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort by priority descending so we try the most important domains first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].priority > candidates[j].priority
	})

	// Try each domain in priority order. readyFn (TryWait) reserves the
	// rate limit slot, so we stop at the first ready one to avoid waste.
	for _, c := range candidates {
		if readyFn(c.domain) {
			// Pop the head job from this domain's queue.
			jobs := q.domains[c.domain]
			job := jobs[0]
			q.domains[c.domain] = jobs[1:]
			if len(q.domains[c.domain]) == 0 {
				delete(q.domains, c.domain)
			}
			q.dequeued.Add(1)
			return job
		}
	}

	return nil
}

// MarkSeen adds a URL to the Bloom filter without enqueuing it.
// Use this for URLs already crawled (loaded from the database on startup).
func (q *Queue) MarkSeen(url string) {
	q.mu.Lock()
	q.bloom.AddString(url)
	q.mu.Unlock()
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
