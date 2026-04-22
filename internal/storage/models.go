// Package storage defines the data models used across all layers of the crawler.
package storage

import (
	"time"
)

// Page represents a crawled web page with its extracted content and metadata.
type Page struct {
	ID          int64     `json:"id"`
	URL         string    `json:"url"`
	Domain      string    `json:"domain"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Content     string    `json:"content,omitempty"` // Clean text, no HTML
	StatusCode  int       `json:"status_code"`
	ContentHash string    `json:"content_hash"` // SHA-256 of content for change detection
	CrawledAt   time.Time `json:"crawled_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CrawlJob represents a URL queued for crawling in the frontier.
type CrawlJob struct {
	ID       int64     `json:"id"`
	URL      string    `json:"url"`
	Domain   string    `json:"domain"`
	Depth    int       `json:"depth"`
	Priority int       `json:"priority"` // Higher = more urgent
	Status   string    `json:"status"`   // pending, in_progress, done, failed
	Retries  int       `json:"retries"`
	ErrorMsg string    `json:"error_msg,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	LockedAt time.Time `json:"locked_at,omitempty"`
}

// CrawlJob status constants.
const (
	JobStatusPending    = "pending"
	JobStatusInProgress = "in_progress"
	JobStatusDone       = "done"
	JobStatusFailed     = "failed"
)

// Priority levels for the URL frontier.
const (
	PrioritySeed      = 100 // Manually specified seed domains
	PriorityDiscovery = 50  // Domains with few pages
	PriorityNormal    = 10  // Standard discovered URLs
	PriorityRecrawl   = 5   // Re-crawl of existing pages
)

// Domain represents a known domain with crawl metadata and statistics.
type Domain struct {
	ID            int64     `json:"id"`
	Domain        string    `json:"domain"`
	IsSeed        bool      `json:"is_seed"`
	RobotsTxt     string    `json:"-"` // Raw robots.txt content, excluded from JSON
	RobotsFetched time.Time `json:"robots_fetched,omitempty"`
	LastCrawled   time.Time `json:"last_crawled,omitempty"`
	PagesCount    int       `json:"pages_count"`
	AvgResponseMs int       `json:"avg_response_ms"`
	CreatedAt     time.Time `json:"created_at"`
}

// SearchResult represents a single result from a full-text search query.
type SearchResult struct {
	ID          int64     `json:"id"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Snippet     string    `json:"snippet"` // FTS5 highlighted snippet
	Domain      string    `json:"domain"`
	CrawledAt   time.Time `json:"crawled_at"`
	Rank        float64   `json:"rank"` // FTS5 relevance score
}

// QueueStats provides a snapshot of the crawl queue state.
type QueueStats struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Total      int `json:"total"`
}

// CrawlerStats provides overall statistics about the crawler's work.
type CrawlerStats struct {
	TotalPages     int    `json:"total_pages"`
	TotalDomains   int    `json:"total_domains"`
	SeedDomains    int    `json:"seed_domains"`
	Queue          QueueStats `json:"queue"`
	DatabaseSizeMB float64 `json:"database_size_mb"`
}
