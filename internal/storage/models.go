// Package storage defines the data models used across all layers of the crawler.
package storage

import (
	"time"
)

// Page represents a crawled web page with its extracted content and metadata.
type Page struct {
	ID                int64     `json:"id"`
	URL               string    `json:"url"`
	URLHash           int64     `json:"url_hash"`
	Domain            string    `json:"domain"`
	Title             string    `json:"title"`
	H1                string    `json:"h1,omitempty"`
	H2                string    `json:"h2,omitempty"`
	Description       string    `json:"description"`
	Content           string    `json:"content,omitempty"` // Clean text, no HTML
	Language          string    `json:"language"`          // ISO 639-1 code (es, en, pt...)
	Region            string    `json:"region"`            // Country code from TLD (uy, es, ar...)
	StatusCode        int       `json:"status_code"`
	ContentHash       string    `json:"content_hash"`           // SHA-256 of content for change detection
	URLFingerprint    string    `json:"url_fingerprint"`        // Structural fingerprint for deduplication
	PublishedAt       time.Time `json:"published_at,omitempty"` // Article publication date
	CrawledAt         time.Time `json:"crawled_at"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	SchemaType        string    `json:"schema_type,omitempty"`        // e.g. Recipe, NewsArticle, Product
	SchemaTitle       string    `json:"schema_title,omitempty"`       // JSON-LD headline/name
	SchemaDescription string    `json:"schema_description,omitempty"` // JSON-LD description
	SchemaImage       string    `json:"schema_image,omitempty"`       // JSON-LD image URL
	SchemaAuthor      string    `json:"schema_author,omitempty"`      // JSON-LD author name
	SchemaKeywords    string    `json:"schema_keywords,omitempty"`    // JSON-LD keywords (comma-separated)
	SchemaRating      float64   `json:"schema_rating,omitempty"`      // JSON-LD aggregateRating value
	IsSeed            bool      `json:"is_seed"`                      // True if domain is a seed domain
}

// CrawlJob represents a URL queued for crawling in the frontier.
type CrawlJob struct {
	ID       int64     `json:"id"`
	URL      string    `json:"url"`
	Domain   string    `json:"domain"`
	Depth    int       `json:"depth"`
	Score    float64   `json:"score"`  // Higher = more urgent (was Priority int)
	Status   string    `json:"status"` // pending, in_progress, done, failed
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

// Priority levels for the URL frontier (used as BaseScore in adaptive mode).
const (
	PrioritySeed      = 100.0 // Manually specified seed domains
	PriorityDiscovery = 50.0  // Domains with few pages
	PriorityNormal    = 10.0  // Standard discovered URLs
	PriorityRecrawl   = 5.0   // Re-crawl of existing pages
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
	ID               int64      `json:"id"`
	URL              string     `json:"url"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Snippet          string     `json:"snippet"` // FTS5 highlighted snippet
	Domain           string     `json:"domain"`
	Language         string     `json:"language"`
	Region           string     `json:"region"`
	CrawledAt        time.Time  `json:"crawled_at"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	Rank             float64    `json:"rank"` // Composite relevance score, higher = better
	ReferringDomains int        `json:"referring_domains"`
	PageRank         float64    `json:"pagerank"`
	SchemaType       string     `json:"schema_type,omitempty"`
	SchemaImage      string     `json:"schema_image,omitempty"`
	SchemaAuthor     string     `json:"schema_author,omitempty"`
	SchemaKeywords   string     `json:"schema_keywords,omitempty"`
	SchemaRating     float64    `json:"schema_rating,omitempty"`
}

// DiscoveredResource represents a non-HTML asset crawled only for link discovery.
type DiscoveredResource struct {
	ID             int64     `json:"id"`
	URL            string    `json:"url"`
	URLFingerprint string    `json:"url_fingerprint"`
	Kind           string    `json:"kind"`
	StatusCode     int       `json:"status_code"`
	LastCrawled    time.Time `json:"last_crawled"`
}

// ImageRecord represents an image discovered during crawling.
type ImageRecord struct {
	ID        int64     `json:"id"`
	URL       string    `json:"url"`
	PageURL   string    `json:"page_url"`
	PageID    int64     `json:"page_id"`
	Domain    string    `json:"domain"`
	AltText   string    `json:"alt_text"`
	Title     string    `json:"title"`
	Context   string    `json:"context"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	CrawledAt time.Time `json:"crawled_at"`
}

// ImageSearchResult represents a single image search result.
type ImageSearchResult struct {
	ID      int64   `json:"id"`
	URL     string  `json:"url"`
	PageURL string  `json:"page_url"`
	Domain  string  `json:"domain"`
	AltText string  `json:"alt_text"`
	Title   string  `json:"title"`
	Context string  `json:"context"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Rank    float64 `json:"rank"`
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
	TotalPages     int        `json:"total_pages"`
	TotalDomains   int        `json:"total_domains"`
	SeedDomains    int        `json:"seed_domains"`
	Queue          QueueStats `json:"queue"`
	DatabaseSizeMB float64    `json:"database_size_mb"`
}

// PageEmbedding stores a dense vector embedding for semantic search.
type PageEmbedding struct {
	PageID     int64     `json:"page_id"`
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
	Embedding  []float32 `json:"embedding"`
	CreatedAt  time.Time `json:"created_at"`
}

// OutgoingLink represents a link from a source page to a target URL with anchor text.
type OutgoingLink struct {
	TargetURL  string
	TargetHash int64
	AnchorText string
}

// LinkRecord represents an outgoing link from one page to another.
type LinkRecord struct {
	SourceID   int64     `json:"source_id"`
	TargetHash int64     `json:"target_hash"`
	AnchorText string    `json:"anchor_text"`
	CreatedAt  time.Time `json:"created_at"`
}

// BacklinkResult represents a page that links to a target URL.
type BacklinkResult struct {
	SourceID    int64     `json:"source_id"`
	SourceURL   string    `json:"source_url"`
	SourceTitle string    `json:"source_title"`
	AnchorText  string    `json:"anchor_text"`
	CreatedAt   time.Time `json:"created_at"`
}

// OutlinkResult represents a page that a source page links to.
type OutlinkResult struct {
	TargetURL  string    `json:"target_url"`
	AnchorText string    `json:"anchor_text"`
	CreatedAt  time.Time `json:"created_at"`
}

// InterestTermRecord represents a single interest term stored in the database.
type InterestTermRecord struct {
	Term     string  `json:"term"`
	Weight   float64 `json:"weight"`
	Source   string  `json:"source"`
	Language string  `json:"language,omitempty"`
}
