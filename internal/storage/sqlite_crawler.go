package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"github.com/DuvyDev/Duvycrawl/internal/utils"
)

func (s *SQLiteStorage) UpsertPage(ctx context.Context, page *Page) error {
	query := `
		INSERT INTO pages (url, domain, title, h1, h2, description, content, language, region, status_code, content_hash, url_fingerprint, published_at, crawled_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating, is_seed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			domain           = excluded.domain,
			title            = excluded.title,
			h1               = excluded.h1,
			h2               = excluded.h2,
			description      = excluded.description,
			content          = excluded.content,
			language         = excluded.language,
			region           = excluded.region,
			status_code      = excluded.status_code,
			content_hash     = excluded.content_hash,
			url_fingerprint  = excluded.url_fingerprint,
			published_at     = COALESCE(excluded.published_at, pages.published_at),
			crawled_at       = excluded.crawled_at,
			updated_at       = CURRENT_TIMESTAMP,
			schema_type      = excluded.schema_type,
			schema_title     = excluded.schema_title,
			schema_description = excluded.schema_description,
			schema_image     = excluded.schema_image,
			schema_author    = excluded.schema_author,
			schema_keywords  = excluded.schema_keywords,
			schema_rating    = excluded.schema_rating,
			is_seed          = excluded.is_seed
	`

	var publishedAt any
	if !page.PublishedAt.IsZero() {
		publishedAt = page.PublishedAt
	}
	var schemaRating any
	if page.SchemaRating > 0 {
		schemaRating = page.SchemaRating
	}

	_, err := s.writeContentDB.ExecContext(ctx, query,
		page.URL, page.Domain, page.Title, page.H1, page.H2, page.Description,
		page.Content, page.Language, page.Region,
		page.StatusCode, page.ContentHash, page.URLFingerprint,
		publishedAt, page.CrawledAt,
		page.SchemaType, page.SchemaTitle, page.SchemaDescription, page.SchemaImage,
		page.SchemaAuthor, page.SchemaKeywords, schemaRating, page.IsSeed,
	)
	if err != nil {
		return fmt.Errorf("upserting page %q: %w", page.URL, err)
	}
	return nil
}

// GetPageByURL retrieves a single page by its URL.
func (s *SQLiteStorage) GetPageByURL(ctx context.Context, url string) (*Page, error) {
	return s.getPage(ctx, "SELECT id, url, domain, title, h1, h2, description, content, status_code, content_hash, url_fingerprint, published_at, crawled_at, created_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating FROM pages WHERE url = ?", url)
}

// GetPageByFingerprint retrieves a single page by its structural fingerprint.
func (s *SQLiteStorage) GetPageByFingerprint(ctx context.Context, fingerprint string) (*Page, error) {
	return s.getPage(ctx, "SELECT id, url, domain, title, h1, h2, description, content, status_code, content_hash, url_fingerprint, published_at, crawled_at, created_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating FROM pages WHERE url_fingerprint = ? LIMIT 1", fingerprint)
}

// GetPageByID retrieves a single page by its database ID.
func (s *SQLiteStorage) GetPageByID(ctx context.Context, id int64) (*Page, error) {
	return s.getPage(ctx, "SELECT id, url, domain, title, h1, h2, description, content, status_code, content_hash, url_fingerprint, published_at, crawled_at, created_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating FROM pages WHERE id = ?", id)
}

func (s *SQLiteStorage) getPage(ctx context.Context, query string, arg any) (*Page, error) {
	var p Page
	var crawledAt, createdAt, updatedAt sql.NullTime
	var publishedAt sql.NullTime
	var schemaRating sql.NullFloat64

	err := s.readContentDB.QueryRowContext(ctx, query, arg).Scan(
		&p.ID, &p.URL, &p.Domain, &p.Title, &p.H1, &p.H2, &p.Description,
		&p.Content, &p.StatusCode, &p.ContentHash, &p.URLFingerprint,
		&publishedAt, &crawledAt, &createdAt, &updatedAt,
		&p.SchemaType, &p.SchemaTitle, &p.SchemaDescription, &p.SchemaImage,
		&p.SchemaAuthor, &p.SchemaKeywords, &schemaRating,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying page: %w", err)
	}

	if publishedAt.Valid {
		p.PublishedAt = publishedAt.Time
	}
	if crawledAt.Valid {
		p.CrawledAt = crawledAt.Time
	}
	if createdAt.Valid {
		p.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		p.UpdatedAt = updatedAt.Time
	}
	if schemaRating.Valid {
		p.SchemaRating = schemaRating.Float64
	}
	return &p, nil
}

// GetAllPageURLs returns every crawled URL and its structural fingerprint.
func (s *SQLiteStorage) GetAllPageURLs(ctx context.Context) (urls []string, fingerprints []string, err error) {
	rows, err := s.readContentDB.QueryContext(ctx, "SELECT url, url_fingerprint FROM pages")
	if err != nil {
		return nil, nil, fmt.Errorf("querying all page URLs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var u, f string
		if err := rows.Scan(&u, &f); err != nil {
			return nil, nil, fmt.Errorf("scanning page URL: %w", err)
		}
		urls = append(urls, u)
		fingerprints = append(fingerprints, f)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating page URLs: %w", err)
	}
	return urls, fingerprints, nil
}

// UpsertDiscoveredResource inserts or updates a discovered resource.
func (s *SQLiteStorage) UpsertDiscoveredResource(ctx context.Context, resource *DiscoveredResource) error {
	_, err := s.writeContentDB.ExecContext(ctx, `
		INSERT INTO discovered_resources (url, url_fingerprint, kind, status_code, last_crawled)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			url_fingerprint = excluded.url_fingerprint,
			kind            = excluded.kind,
			status_code     = excluded.status_code,
			last_crawled    = excluded.last_crawled
	`, resource.URL, resource.URLFingerprint, resource.Kind, resource.StatusCode, resource.LastCrawled)
	if err != nil {
		return fmt.Errorf("upsert discovered resource: %w", err)
	}
	return nil
}

// GetAllDiscoveredURLs returns every discovered resource URL and fingerprint.
func (s *SQLiteStorage) GetAllDiscoveredURLs(ctx context.Context) (urls []string, fingerprints []string, err error) {
	rows, err := s.readContentDB.QueryContext(ctx, "SELECT url, url_fingerprint FROM discovered_resources")
	if err != nil {
		return nil, nil, fmt.Errorf("querying discovered resources: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var u, f string
		if err := rows.Scan(&u, &f); err != nil {
			return nil, nil, fmt.Errorf("scanning discovered resource: %w", err)
		}
		urls = append(urls, u)
		fingerprints = append(fingerprints, f)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating discovered resources: %w", err)
	}
	return urls, fingerprints, nil
}

// SearchPages performs hybrid search optimized for navigational queries and
// typo tolerance. It tries increasingly permissive retrieval modes and then
// re-ranks candidates in Go using title/domain phrase quality, domain-root
// homepage boosts, token coverage, typo similarity, freshness, and language.
func (s *SQLiteStorage) GetStalePages(ctx context.Context, olderThan time.Time, limit int) ([]Page, error) {
	query := `
		SELECT id, url, domain, title, description, '', status_code, content_hash, crawled_at, created_at, updated_at
		FROM pages
		WHERE crawled_at < ?
		ORDER BY crawled_at ASC
		LIMIT ?
	`

	rows, err := s.readContentDB.QueryContext(ctx, query, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("querying stale pages: %w", err)
	}
	defer rows.Close()

	var pages []Page
	for rows.Next() {
		var p Page
		var crawledAt, createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&p.ID, &p.URL, &p.Domain, &p.Title, &p.Description, &p.Content, &p.StatusCode, &p.ContentHash, &crawledAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning stale page: %w", err)
		}
		if crawledAt.Valid {
			p.CrawledAt = crawledAt.Time
		}
		if createdAt.Valid {
			p.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			p.UpdatedAt = updatedAt.Time
		}
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

// GetFreshURLs returns a set of URLs from the given list that were crawled
// more recently than the given TTL. Used to skip recently-indexed URLs.
func (s *SQLiteStorage) GetFreshURLs(ctx context.Context, urls []string, newerThan time.Time) (map[string]struct{}, error) {
	if len(urls) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(urls))
	args := make([]any, len(urls)+1)
	for i, u := range urls {
		placeholders[i] = "?"
		args[i] = u
	}
	args[len(urls)] = newerThan.Format(time.RFC3339)

	query := fmt.Sprintf(`
		SELECT url FROM pages
		WHERE url IN (%s) AND crawled_at >= ?
	`, strings.Join(placeholders, ","))

	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying fresh URLs: %w", err)
	}
	defer rows.Close()

	fresh := make(map[string]struct{})
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			continue
		}
		fresh[url] = struct{}{}
	}
	return fresh, rows.Err()
}

// ListPagesWithoutEmbeddings returns pages missing embeddings in ascending ID order.
func (s *SQLiteStorage) ListPagesWithoutEmbeddings(ctx context.Context, afterID int64, limit int) ([]Page, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.readContentDB.QueryContext(ctx, `
		SELECT p.id, p.url, p.title, p.description, SUBSTR(p.content, 1, 2048)
		FROM pages p
		LEFT JOIN page_embeddings pe ON pe.page_id = p.id
		WHERE pe.page_id IS NULL AND p.id > ?
		ORDER BY p.id ASC
		LIMIT ?
	`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying pages without embeddings: %w", err)
	}
	defer rows.Close()

	pages := make([]Page, 0, limit)
	for rows.Next() {
		var page Page
		if err := rows.Scan(&page.ID, &page.URL, &page.Title, &page.Description, &page.Content); err != nil {
			return nil, fmt.Errorf("scanning page without embedding: %w", err)
		}
		pages = append(pages, page)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pages without embeddings: %w", err)
	}

	return pages, nil
}

// --------------------------------------------------------------------------
// Crawl Queue Operations
// --------------------------------------------------------------------------

// EnqueueURL adds a single URL to the crawl queue.
// If the URL already exists in the queue, it is silently ignored.
func (s *SQLiteStorage) EnqueueURL(ctx context.Context, job *CrawlJob) error {
	query := `
		INSERT OR IGNORE INTO crawl_queue (url, domain, depth, score, status)
		VALUES (?, ?, ?, ?, ?)
	`
	_, err := s.crawlerDB.ExecContext(ctx, query, job.URL, job.Domain, job.Depth, job.Score, JobStatusPending)
	if err != nil {
		return fmt.Errorf("enqueuing URL %q: %w", job.URL, err)
	}
	return nil
}

// EnqueueURLs adds multiple URLs to the crawl queue in a single transaction.
func (s *SQLiteStorage) EnqueueURLs(ctx context.Context, jobs []*CrawlJob) error {
	if len(jobs) == 0 {
		return nil
	}

	tx, err := s.crawlerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning enqueue transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO crawl_queue (url, domain, depth, score, status) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing enqueue statement: %w", err)
	}
	defer stmt.Close()

	for _, job := range jobs {
		if _, err := stmt.ExecContext(ctx, job.URL, job.Domain, job.Depth, job.Score, JobStatusPending); err != nil {
			return fmt.Errorf("enqueuing URL %q: %w", job.URL, err)
		}
	}

	return tx.Commit()
}

// DequeueURLs atomically claims up to `limit` pending jobs from the queue.
// Jobs are ordered by priority (descending), then by FIFO (added_at ascending).
// Claimed jobs are marked as in_progress.
func (s *SQLiteStorage) DequeueURLs(ctx context.Context, limit int) ([]*CrawlJob, error) {
	tx, err := s.crawlerDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning dequeue transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	// Select and lock pending jobs.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, url, domain, depth, score, retries
		FROM crawl_queue
		WHERE status = ?
		ORDER BY score DESC, added_at ASC
		LIMIT ?
	`, JobStatusPending, limit)
	if err != nil {
		return nil, fmt.Errorf("selecting pending jobs: %w", err)
	}

	var jobs []*CrawlJob
	for rows.Next() {
		var j CrawlJob
		if err := rows.Scan(&j.ID, &j.URL, &j.Domain, &j.Depth, &j.Score, &j.Retries); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning job: %w", err)
		}
		j.Status = JobStatusInProgress
		j.LockedAt = now
		jobs = append(jobs, &j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating jobs: %w", err)
	}

	// Mark them as in_progress.
	for _, j := range jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE crawl_queue SET status = ?, locked_at = ? WHERE id = ?`, JobStatusInProgress, now, j.ID); err != nil {
			return nil, fmt.Errorf("marking job %d as in_progress: %w", j.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing dequeue transaction: %w", err)
	}

	return jobs, nil
}

// CompleteJob marks a crawl job as done or failed.
// If crawlErr is nil, the job is marked as done and removed from the queue.
// If crawlErr is not nil, the job is marked as failed with the error message.
func (s *SQLiteStorage) CompleteJob(ctx context.Context, jobID int64, crawlErr error) error {
	if crawlErr == nil {
		// Success â€” remove from queue.
		_, err := s.crawlerDB.ExecContext(ctx, `DELETE FROM crawl_queue WHERE id = ?`, jobID)
		if err != nil {
			return fmt.Errorf("deleting completed job %d: %w", jobID, err)
		}
		return nil
	}

	// Failure â€” mark as failed with error message, increment retries.
	_, err := s.crawlerDB.ExecContext(ctx, `
		UPDATE crawl_queue
		SET status = ?, error_msg = ?, retries = retries + 1
		WHERE id = ?
	`, JobStatusFailed, crawlErr.Error(), jobID)
	if err != nil {
		return fmt.Errorf("marking job %d as failed: %w", jobID, err)
	}
	return nil
}

// DequeueURLsExcluding is like DequeueURLs but skips jobs belonging to
// any of the excluded domains. This enables workers to skip rate-limited
// domains and find work for other domains instead.
func (s *SQLiteStorage) DequeueURLsExcluding(ctx context.Context, limit int, excludedDomains []string) ([]*CrawlJob, error) {
	if len(excludedDomains) == 0 {
		return s.DequeueURLs(ctx, limit)
	}

	tx, err := s.crawlerDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning dequeue transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	// Build the NOT IN clause dynamically.
	placeholders := make([]string, len(excludedDomains))
	args := make([]any, 0, len(excludedDomains)+2)
	args = append(args, JobStatusPending)
	for i, d := range excludedDomains {
		placeholders[i] = "?"
		args = append(args, d)
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT id, url, domain, depth, score, retries
		FROM crawl_queue
		WHERE status = ? AND domain NOT IN (%s)
		ORDER BY score DESC, added_at ASC
		LIMIT ?
	`, strings.Join(placeholders, ","))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("selecting pending jobs (excluding domains): %w", err)
	}

	var jobs []*CrawlJob
	for rows.Next() {
		var j CrawlJob
		if err := rows.Scan(&j.ID, &j.URL, &j.Domain, &j.Depth, &j.Score, &j.Retries); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning job: %w", err)
		}
		j.Status = JobStatusInProgress
		j.LockedAt = now
		jobs = append(jobs, &j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating jobs: %w", err)
	}

	// Mark them as in_progress.
	for _, j := range jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE crawl_queue SET status = ?, locked_at = ? WHERE id = ?`, JobStatusInProgress, now, j.ID); err != nil {
			return nil, fmt.Errorf("marking job %d as in_progress: %w", j.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing dequeue transaction: %w", err)
	}

	return jobs, nil
}

// ReturnJob puts a claimed (in_progress) job back into the pending state.
// This is used when a worker cannot process a job due to rate limiting
// and wants to release it for another worker or a later attempt.
func (s *SQLiteStorage) ReturnJob(ctx context.Context, jobID int64) error {
	_, err := s.crawlerDB.ExecContext(ctx, `
		UPDATE crawl_queue
		SET status = ?, locked_at = NULL
		WHERE id = ? AND status = ?
	`, JobStatusPending, jobID, JobStatusInProgress)
	if err != nil {
		return fmt.Errorf("returning job %d to pending: %w", jobID, err)
	}
	return nil
}

// GetQueueStats returns the current state of the crawl queue.
func (s *SQLiteStorage) GetQueueStats(ctx context.Context) (*QueueStats, error) {
	var stats QueueStats
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'pending'     THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'done'        THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed'      THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM crawl_queue
	`
	err := s.crawlerDB.QueryRowContext(ctx, query).Scan(
		&stats.Pending, &stats.InProgress, &stats.Done, &stats.Failed, &stats.Total,
	)
	if err != nil {
		return nil, fmt.Errorf("querying queue stats: %w", err)
	}
	return &stats, nil
}

// --------------------------------------------------------------------------
// Batch Deduplication
// --------------------------------------------------------------------------

// FilterExistingURLs returns a set of URLs that already exist in the pages table.
// It performs a single batched query instead of N individual lookups.
func (s *SQLiteStorage) FilterExistingURLs(ctx context.Context, urls []string) (map[string]struct{}, error) {
	if len(urls) == 0 {
		return map[string]struct{}{}, nil
	}

	placeholders := make([]string, len(urls))
	args := make([]any, len(urls))
	for i, u := range urls {
		placeholders[i] = "?"
		args[i] = u
	}

	query := fmt.Sprintf("SELECT url FROM pages WHERE url IN (%s)", strings.Join(placeholders, ","))
	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch URL lookup: %w", err)
	}
	defer rows.Close()

	existing := make(map[string]struct{}, len(urls))
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scanning existing URL: %w", err)
		}
		existing[u] = struct{}{}
	}
	return existing, rows.Err()
}

// FilterExistingFingerprints returns a set of URL fingerprints that already exist.
func (s *SQLiteStorage) FilterExistingFingerprints(ctx context.Context, fingerprints []string) (map[string]struct{}, error) {
	if len(fingerprints) == 0 {
		return map[string]struct{}{}, nil
	}

	placeholders := make([]string, len(fingerprints))
	args := make([]any, len(fingerprints))
	for i, fp := range fingerprints {
		placeholders[i] = "?"
		args[i] = fp
	}

	query := fmt.Sprintf("SELECT url_fingerprint FROM pages WHERE url_fingerprint IN (%s)", strings.Join(placeholders, ","))
	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch fingerprint lookup: %w", err)
	}
	defer rows.Close()

	existing := make(map[string]struct{}, len(fingerprints))
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning existing fingerprint: %w", err)
		}
		existing[fp] = struct{}{}
	}
	return existing, rows.Err()
}

// --------------------------------------------------------------------------
// Domain Operations
// --------------------------------------------------------------------------

// UpsertDomain inserts a new domain or updates an existing one.
func (s *SQLiteStorage) UpsertDomain(ctx context.Context, domain *Domain) error {
	query := `
		INSERT INTO domains (domain, is_seed, robots_txt, robots_fetched, last_crawled, pages_count, avg_response_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain) DO UPDATE SET
			is_seed         = excluded.is_seed,
			robots_txt      = CASE WHEN excluded.robots_txt != '' THEN excluded.robots_txt ELSE domains.robots_txt END,
			robots_fetched  = CASE WHEN excluded.robots_fetched IS NOT NULL THEN excluded.robots_fetched ELSE domains.robots_fetched END,
			last_crawled    = CASE WHEN excluded.last_crawled IS NOT NULL THEN excluded.last_crawled ELSE domains.last_crawled END,
			pages_count     = excluded.pages_count,
			avg_response_ms = excluded.avg_response_ms
	`

	var robotsFetched, lastCrawled *time.Time
	if !domain.RobotsFetched.IsZero() {
		robotsFetched = &domain.RobotsFetched
	}
	if !domain.LastCrawled.IsZero() {
		lastCrawled = &domain.LastCrawled
	}

	_, err := s.crawlerDB.ExecContext(ctx, query,
		domain.Domain, domain.IsSeed, domain.RobotsTxt,
		robotsFetched, lastCrawled,
		domain.PagesCount, domain.AvgResponseMs,
	)
	if err != nil {
		return fmt.Errorf("upserting domain %q: %w", domain.Domain, err)
	}
	return nil
}

// GetDomain retrieves a domain by its name.
func (s *SQLiteStorage) GetDomain(ctx context.Context, domainName string) (*Domain, error) {
	var d Domain
	var robotsFetched, lastCrawled, createdAt sql.NullTime

	err := s.crawlerDB.QueryRowContext(ctx, `
		SELECT id, domain, is_seed, robots_txt, robots_fetched, last_crawled, pages_count, avg_response_ms, created_at
		FROM domains WHERE domain = ?
	`, domainName).Scan(
		&d.ID, &d.Domain, &d.IsSeed, &d.RobotsTxt,
		&robotsFetched, &lastCrawled, &d.PagesCount,
		&d.AvgResponseMs, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying domain %q: %w", domainName, err)
	}

	if robotsFetched.Valid {
		d.RobotsFetched = robotsFetched.Time
	}
	if lastCrawled.Valid {
		d.LastCrawled = lastCrawled.Time
	}
	if createdAt.Valid {
		d.CreatedAt = createdAt.Time
	}
	return &d, nil
}

// GetSeedDomains returns all domains marked as seeds.
func (s *SQLiteStorage) GetSeedDomains(ctx context.Context) ([]Domain, error) {
	rows, err := s.crawlerDB.QueryContext(ctx, `
		SELECT id, domain, is_seed, robots_fetched, last_crawled, pages_count, avg_response_ms, created_at
		FROM domains WHERE is_seed = TRUE
		ORDER BY domain ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("querying seed domains: %w", err)
	}
	defer rows.Close()

	var domains []Domain
	for rows.Next() {
		var d Domain
		var robotsFetched, lastCrawled, createdAt sql.NullTime
		if err := rows.Scan(&d.ID, &d.Domain, &d.IsSeed, &robotsFetched, &lastCrawled, &d.PagesCount, &d.AvgResponseMs, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning seed domain: %w", err)
		}
		if robotsFetched.Valid {
			d.RobotsFetched = robotsFetched.Time
		}
		if lastCrawled.Valid {
			d.LastCrawled = lastCrawled.Time
		}
		if createdAt.Valid {
			d.CreatedAt = createdAt.Time
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// DeleteDomain removes a domain from the seed list (sets is_seed = false).
func (s *SQLiteStorage) DeleteDomain(ctx context.Context, domainName string) error {
	result, err := s.crawlerDB.ExecContext(ctx, `UPDATE domains SET is_seed = FALSE WHERE domain = ?`, domainName)
	if err != nil {
		return fmt.Errorf("deleting domain %q: %w", domainName, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("domain %q not found", domainName)
	}
	return nil
}

// --------------------------------------------------------------------------
// Statistics
// --------------------------------------------------------------------------

// GetStats returns overall crawler statistics.
func (s *SQLiteStorage) UpdatePageRankings(ctx context.Context) error {
	s.logger.Info("starting page ranking update (referring domains)")
	start := time.Now()

	// Use writeContentDB and attach graph.db for cross-database operations
	graphPath := filepath.Join(s.dataDir, "graph.db")
	_, err := s.writeContentDB.ExecContext(ctx, fmt.Sprintf("ATTACH DATABASE '%s' AS graph", graphPath))
	if err != nil {
		return fmt.Errorf("attaching graph database: %w", err)
	}
	defer s.writeContentDB.ExecContext(context.Background(), "DETACH DATABASE graph")

	_, err = s.writeContentDB.ExecContext(ctx, `UPDATE pages SET referring_domains = 0`)
	if err != nil {
		return fmt.Errorf("resetting referring_domains: %w", err)
	}

	query := `
		WITH domain_counts AS (
			SELECT
				l.target_hash,
				COUNT(DISTINCT p_source.domain) as unique_domains
			FROM graph.links l
			JOIN pages p_source ON p_source.id = l.source_id
			JOIN pages p_target ON p_target.url_hash = l.target_hash
			WHERE p_source.domain != p_target.domain
			GROUP BY l.target_hash
		)
		UPDATE pages
		SET referring_domains = domain_counts.unique_domains
		FROM domain_counts
		WHERE pages.url_hash = domain_counts.target_hash;
	`

	res, err := s.writeContentDB.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("updating referring_domains: %w", err)
	}

	affected, _ := res.RowsAffected()
	s.logger.Info("page ranking update completed", "affected", affected, "duration", time.Since(start))

	// --- Phase 1: Iterative PageRank ---
	if err := s.computeIterativePageRank(ctx); err != nil {
		return fmt.Errorf("computing iterative pagerank: %w", err)
	}

	return nil
}

// computeIterativePageRank loads the graph in memory, runs power iteration for PageRank,
// and updates the pagerank column in the database.
func (s *SQLiteStorage) computeIterativePageRank(ctx context.Context) error {
	s.logger.Info("starting iterative PageRank computation")
	start := time.Now()

	rows, err := s.readContentDB.QueryContext(ctx, `SELECT id FROM pages`)
	if err != nil {
		return err
	}

	var pageIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		pageIDs = append(pageIDs, id)
	}
	rows.Close()

	if len(pageIDs) == 0 {
		return nil
	}

	idToIndex := make(map[int64]int, len(pageIDs))
	for i, id := range pageIDs {
		idToIndex[id] = i
	}

	N := len(pageIDs)
	outDegree := make([]int, N)
	inboundLinks := make([][]int, N)

	// Since we attached graph in the caller (UpdatePageRankings) on writeContentDB,
	// we should use writeContentDB to read the joined data.
	edgeRows, err := s.writeContentDB.QueryContext(ctx, `
		SELECT l.source_id, p.id 
		FROM graph.links l
		JOIN pages p ON p.url_hash = l.target_hash
		WHERE l.source_id != p.id
	`)
	if err != nil {
		return err
	}

	for edgeRows.Next() {
		var srcID, dstID int64
		if err := edgeRows.Scan(&srcID, &dstID); err != nil {
			edgeRows.Close()
			return err
		}
		srcIdx, srcOk := idToIndex[srcID]
		dstIdx, dstOk := idToIndex[dstID]
		if srcOk && dstOk {
			outDegree[srcIdx]++
			inboundLinks[dstIdx] = append(inboundLinks[dstIdx], srcIdx)
		}
	}
	edgeRows.Close()

	damping := 0.85
	scores := make([]float64, N)
	newScores := make([]float64, N)

	for i := 0; i < N; i++ {
		scores[i] = 1.0
	}

	iterations := 15
	for iter := 0; iter < iterations; iter++ {
		danglingSum := 0.0
		for i := 0; i < N; i++ {
			if outDegree[i] == 0 {
				danglingSum += scores[i]
			}
		}

		for i := 0; i < N; i++ {
			sum := 0.0
			for _, srcIdx := range inboundLinks[i] {
				sum += scores[srcIdx] / float64(outDegree[srcIdx])
			}
			newScores[i] = (1.0 - damping) + damping*(sum+danglingSum/float64(N))
		}

		for i := 0; i < N; i++ {
			scores[i] = newScores[i]
		}
	}

	tx, err := s.writeContentDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `UPDATE pages SET pagerank = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i, score := range scores {
		_, err := stmt.ExecContext(ctx, score, pageIDs[i])
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.logger.Info("iterative PageRank computation completed", "nodes", N, "duration", time.Since(start))
	return nil
}

// RecordClick upserts a search interaction for a query and url, incrementing the click count.
func (s *SQLiteStorage) UpsertImages(ctx context.Context, images []ImageRecord) error {
	if len(images) == 0 {
		return nil
	}

	stmt, err := s.writeContentDB.PrepareContext(ctx, `
		INSERT INTO images (url, page_url, page_id, domain, alt_text, title, context, width, height, crawled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			page_url   = excluded.page_url,
			page_id    = excluded.page_id,
			domain     = excluded.domain,
			alt_text   = excluded.alt_text,
			title      = excluded.title,
			context    = excluded.context,
			width      = excluded.width,
			height     = excluded.height,
			crawled_at = excluded.crawled_at
	`)
	if err != nil {
		return fmt.Errorf("preparing image upsert: %w", err)
	}
	defer stmt.Close()

	for _, img := range images {
		_, err := stmt.ExecContext(ctx,
			img.URL, img.PageURL, img.PageID, img.Domain,
			img.AltText, img.Title, img.Context,
			img.Width, img.Height, img.CrawledAt,
		)
		if err != nil {
			s.logger.Warn("failed to upsert image", "url", img.URL, "error", err)
			continue
		}
	}

	return nil
}

// SearchImages performs a full-text search over image metadata.
func (s *SQLiteStorage) StoreLinks(ctx context.Context, sourceID int64, sourceURL string, links []OutgoingLink) error {
	if len(links) == 0 {
		return nil
	}

	tx, err := s.graphDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning store-links transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM links WHERE source_id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("deleting old links for page %d: %w", sourceID, err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO links (source_id, target_hash, anchor_text)
		VALUES (?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("preparing link insert: %w", err)
	}
	defer stmt.Close()

	for _, link := range links {
		if link.TargetHash == 0 {
			link.TargetHash = utils.HashURL(link.TargetURL)
		}
		if _, err := stmt.ExecContext(ctx, sourceID, link.TargetHash, link.AnchorText); err != nil {
			s.logger.Warn("failed to insert link", "source_id", sourceID, "target_hash", link.TargetHash, "error", err)
			continue
		}
	}

	return tx.Commit()
}

func (s *SQLiteStorage) GetBacklinks(ctx context.Context, targetURL string, limit, offset int) ([]BacklinkResult, int, error) {
	var total int
	targetHash := utils.HashURL(targetURL)
	if err := s.graphDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE target_hash = ?`, targetHash,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting backlinks for %q: %w", targetURL, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.graphDB.QueryContext(ctx, `
		SELECT source_id, anchor_text, created_at
		FROM links
		WHERE target_hash = ?
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
	`, targetHash, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying backlinks for %q: %w", targetURL, err)
	}
	defer rows.Close()

	var results []BacklinkResult
	var sourceIDs []int64
	for rows.Next() {
		var r BacklinkResult
		var createdAt sql.NullTime
		if err := rows.Scan(&r.SourceID, &r.AnchorText, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scanning backlink: %w", err)
		}
		if createdAt.Valid {
			r.CreatedAt = createdAt.Time
		}
		results = append(results, r)
		sourceIDs = append(sourceIDs, r.SourceID)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating backlinks: %w", err)
	}

	// Fetch titles from content.db
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		args := make([]any, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		query := fmt.Sprintf("SELECT id, url, title FROM pages WHERE id IN (%s)", strings.Join(placeholders, ","))
		titleRows, err := s.readContentDB.QueryContext(ctx, query, args...)
		if err == nil {
			defer titleRows.Close()
			titleMap := make(map[int64]string)
			urlMap := make(map[int64]string)
			for titleRows.Next() {
				var id int64
				var url string
				var title string
				if err := titleRows.Scan(&id, &url, &title); err == nil {
					titleMap[id] = title
					urlMap[id] = url
				}
			}
			for i := range results {
				if t, ok := titleMap[results[i].SourceID]; ok {
					results[i].SourceTitle = t
				}
				if u, ok := urlMap[results[i].SourceID]; ok {
					results[i].SourceURL = u
				}
			}
		}
	}

	return results, total, nil
}

func (s *SQLiteStorage) GetOutlinks(ctx context.Context, pageID int64, limit, offset int) ([]OutlinkResult, int, error) {
	var total int
	if err := s.graphDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE source_id = ?`, pageID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting outlinks for page %d: %w", pageID, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.graphDB.QueryContext(ctx, `
		SELECT target_hash, anchor_text, created_at
		FROM links
		WHERE source_id = ?
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
	`, pageID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying outlinks for page %d: %w", pageID, err)
	}
	defer rows.Close()

	var results []OutlinkResult
	var targetHashes []int64
	for rows.Next() {
		var r OutlinkResult
		var targetHash int64
		var createdAt sql.NullTime
		if err := rows.Scan(&targetHash, &r.AnchorText, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scanning outlink: %w", err)
		}
		if createdAt.Valid {
			r.CreatedAt = createdAt.Time
		}
		r.TargetURL = fmt.Sprintf("hash:%d", targetHash)
		results = append(results, r)
		targetHashes = append(targetHashes, targetHash)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating outlinks: %w", err)
	}

	// Fetch target URLs from content.db using target_hashes
	if len(targetHashes) > 0 {
		placeholders := make([]string, len(targetHashes))
		args := make([]any, len(targetHashes))
		for i, h := range targetHashes {
			placeholders[i] = "?"
			args[i] = h
		}
		query := fmt.Sprintf("SELECT url_hash, url FROM pages WHERE url_hash IN (%s)", strings.Join(placeholders, ","))
		urlRows, err := s.readContentDB.QueryContext(ctx, query, args...)
		if err == nil {
			defer urlRows.Close()
			urlMap := make(map[int64]string)
			for urlRows.Next() {
				var h int64
				var url string
				if err := urlRows.Scan(&h, &url); err == nil {
					urlMap[h] = url
				}
			}
			for i, h := range targetHashes {
				if u, ok := urlMap[h]; ok {
					results[i].TargetURL = u
				}
			}
		}
	}

	return results, total, nil
}

func (s *SQLiteStorage) GetBacklinkCount(ctx context.Context, targetURL string) (int, error) {
	var count int
	targetHash := utils.HashURL(targetURL)
	if err := s.graphDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE target_hash = ?`, targetHash,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting backlinks for %q: %w", targetURL, err)
	}
	return count, nil
}

// --------------------------------------------------------------------------
// Embeddings
// --------------------------------------------------------------------------

// float32SliceToBytes converts a []float32 to a little-endian byte slice for SQLite BLOB storage.
func float32SliceToBytes(f []float32) []byte {
	buf := new(bytes.Buffer)
	for _, v := range f {
		_ = binary.Write(buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

// bytesToFloat32Slice converts a little-endian byte slice back to []float32.
func bytesToFloat32Slice(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	count := len(b) / 4
	f := make([]float32, count)
	buf := bytes.NewReader(b)
	for i := 0; i < count; i++ {
		_ = binary.Read(buf, binary.LittleEndian, &f[i])
	}
	return f
}

// SavePageEmbedding stores or replaces a vector embedding for a page.
func (s *SQLiteStorage) SavePageEmbedding(ctx context.Context, emb *PageEmbedding) error {
	if emb == nil || len(emb.Embedding) == 0 {
		return nil
	}
	blob := float32SliceToBytes(emb.Embedding)
	_, err := s.writeContentDB.ExecContext(ctx, `
		INSERT INTO page_embeddings (page_id, model, dimensions, embedding, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(page_id) DO UPDATE SET
			model = excluded.model,
			dimensions = excluded.dimensions,
			embedding = excluded.embedding,
			created_at = CURRENT_TIMESTAMP
	`, emb.PageID, emb.Model, emb.Dimensions, blob)
	if err != nil {
		return fmt.Errorf("saving page embedding for page %d: %w", emb.PageID, err)
	}
	return nil
}

// GetEmbeddingStats returns statistics about the embedding index.
func (s *SQLiteStorage) GetEmbeddingStats(ctx context.Context) (totalPages, embeddedPages, avgDimensions int, model string, err error) {
	if err := s.readContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages`).Scan(&totalPages); err != nil {
		return 0, 0, 0, "", fmt.Errorf("counting total pages: %w", err)
	}

	if err := s.readContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM page_embeddings`).Scan(&embeddedPages); err != nil {
		return totalPages, 0, 0, "", fmt.Errorf("counting embedded pages: %w", err)
	}

	var avgDim sql.NullFloat64
	var modelName sql.NullString
	if err := s.readContentDB.QueryRowContext(ctx, `
		SELECT AVG(dimensions), MAX(model) FROM page_embeddings
	`).Scan(&avgDim, &modelName); err != nil {
		return totalPages, embeddedPages, 0, "", fmt.Errorf("computing embedding stats: %w", err)
	}

	if avgDim.Valid {
		avgDimensions = int(avgDim.Float64)
	}
	if modelName.Valid {
		model = modelName.String
	}
	return totalPages, embeddedPages, avgDimensions, model, nil
}

// GetPageEmbeddings returns embeddings for the given page IDs.
func (s *SQLiteStorage) GetPageEmbeddings(ctx context.Context, pageIDs []int64) (map[int64]*PageEmbedding, error) {
	if len(pageIDs) == 0 {
		return map[int64]*PageEmbedding{}, nil
	}

	placeholders := make([]string, len(pageIDs))
	args := make([]any, len(pageIDs))
	for i, id := range pageIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(
		"SELECT page_id, model, dimensions, embedding, created_at FROM page_embeddings WHERE page_id IN (%s)",
		strings.Join(placeholders, ","),
	)
	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying page embeddings: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]*PageEmbedding, len(pageIDs))
	for rows.Next() {
		var emb PageEmbedding
		var blob []byte
		var createdAt sql.NullTime
		if err := rows.Scan(&emb.PageID, &emb.Model, &emb.Dimensions, &blob, &createdAt); err != nil {
			continue
		}
		emb.Embedding = bytesToFloat32Slice(blob)
		if createdAt.Valid {
			emb.CreatedAt = createdAt.Time
		}
		result[emb.PageID] = &emb
	}
	return result, rows.Err()
}
