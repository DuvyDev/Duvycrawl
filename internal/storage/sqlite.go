package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStorage implements the Storage interface using SQLite with FTS5
// for full-text search capabilities.
//
// It uses two connection pools:
//   - readDB:  multiple connections for concurrent reads (searches, lookups)
//   - writeDB: single connection for serialized writes (upserts, deletes)
//
// This eliminates SQLITE_BUSY errors under high concurrency while
// maximizing read throughput in WAL mode.
type SQLiteStorage struct {
	readDB  *sql.DB
	writeDB *sql.DB
	logger  *slog.Logger
	dbPath  string
}

// NewSQLiteStorage creates a new SQLite-backed storage.
// It creates the database directory if needed, opens the database,
// configures optimal SQLite pragmas, and runs schema migrations.
func NewSQLiteStorage(ctx context.Context, dbPath string, logger *slog.Logger) (*SQLiteStorage, error) {
	// Ensure the directory exists.
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating database directory %q: %w", dir, err)
	}

	// --- Write pool: single connection, serialized writes ---
	// High busy_timeout (30s) ensures writes wait instead of failing.
	writeDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON", dbPath)
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("opening write database: %w", err)
	}
	// Single writer â€” this is the key: only one write can happen at a time,
	// preventing SQLITE_BUSY errors entirely.
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)
	if err := configureWriteDB(ctx, writeDB); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("configuring write database: %w", err)
	}

	// Verify write connection works.
	if err := writeDB.PingContext(ctx); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("pinging write database: %w", err)
	}

	s := &SQLiteStorage{
		writeDB: writeDB,
		logger:  logger.With("component", "storage"),
		dbPath:  dbPath,
	}

	// Run schema migrations (using write connection).
	if err := s.migrate(ctx); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	// --- Read pool: multiple connections for concurrent reads ---
	// Open after migrations so mode=ro works on first startup.
	readDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&mode=ro", dbPath)
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("opening read database: %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	readDB.SetConnMaxLifetime(0)
	if err := configureReadDB(ctx, readDB); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("configuring read database: %w", err)
	}

	if err := readDB.PingContext(ctx); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("pinging read database: %w", err)
	}
	s.readDB = readDB

	logger.Info("SQLite storage initialized",
		"path", dbPath,
		"journal_mode", "WAL",
		"read_conns", 4,
		"write_conns", 1,
	)

	return s, nil
}

// migrate executes all schema migrations in a single transaction.
// ALTER TABLE ADD COLUMN errors are silently ignored (column may already exist).
func (s *SQLiteStorage) migrate(ctx context.Context) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration transaction: %w", err)
	}
	defer tx.Rollback()

	for i, m := range migrations {
		if _, err := tx.ExecContext(ctx, m); err != nil {
			// Tolerate "duplicate column name" errors from ALTER TABLE.
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("executing migration %d: %w", i, err)
		}
	}

	return tx.Commit()
}

// --------------------------------------------------------------------------
// Page Operations
// --------------------------------------------------------------------------

// UpsertPage inserts a new page or updates an existing one matched by URL.
func (s *SQLiteStorage) UpsertPage(ctx context.Context, page *Page) error {
	query := `
		INSERT INTO pages (url, domain, title, description, content, language, region, status_code, content_hash, crawled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(url) DO UPDATE SET
			domain       = excluded.domain,
			title        = excluded.title,
			description  = excluded.description,
			content      = excluded.content,
			language     = excluded.language,
			region       = excluded.region,
			status_code  = excluded.status_code,
			content_hash = excluded.content_hash,
			crawled_at   = excluded.crawled_at,
			updated_at   = CURRENT_TIMESTAMP
	`

	_, err := s.writeDB.ExecContext(ctx, query,
		page.URL, page.Domain, page.Title, page.Description,
		page.Content, page.Language, page.Region,
		page.StatusCode, page.ContentHash, page.CrawledAt,
	)
	if err != nil {
		return fmt.Errorf("upserting page %q: %w", page.URL, err)
	}
	return nil
}

// GetPageByURL retrieves a single page by its URL.
func (s *SQLiteStorage) GetPageByURL(ctx context.Context, url string) (*Page, error) {
	return s.getPage(ctx, "SELECT id, url, domain, title, description, content, status_code, content_hash, crawled_at, created_at, updated_at FROM pages WHERE url = ?", url)
}

// GetPageByID retrieves a single page by its database ID.
func (s *SQLiteStorage) GetPageByID(ctx context.Context, id int64) (*Page, error) {
	return s.getPage(ctx, "SELECT id, url, domain, title, description, content, status_code, content_hash, crawled_at, created_at, updated_at FROM pages WHERE id = ?", id)
}

func (s *SQLiteStorage) getPage(ctx context.Context, query string, arg any) (*Page, error) {
	var p Page
	var crawledAt, createdAt, updatedAt sql.NullTime

	err := s.readDB.QueryRowContext(ctx, query, arg).Scan(
		&p.ID, &p.URL, &p.Domain, &p.Title, &p.Description,
		&p.Content, &p.StatusCode, &p.ContentHash,
		&crawledAt, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying page: %w", err)
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
	return &p, nil
}

// SearchPages performs a full-text search using FTS5.
// Returns matching results and the total count.
// If lang is non-empty, results in that language get a ranking boost.
func (s *SQLiteStorage) SearchPages(ctx context.Context, query string, limit, offset int, lang string) ([]SearchResult, int, error) {
	if query == "" {
		return nil, 0, nil
	}

	// Count total matches.
	var total int
	countQuery := `SELECT COUNT(*) FROM pages_fts WHERE pages_fts MATCH ?`
	if err := s.readDB.QueryRowContext(ctx, countQuery, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting search results for %q: %w", query, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	// Fetch paginated results with snippets and relevance ranking.
	// When a language is specified, boost matching results by multiplying
	// rank by 1.5 (FTS5 rank is negative, so more negative = better;
	// multiplying by 1.5 makes matching-lang results "more negative").
	var searchQuery string
	var args []any

	if lang != "" {
		searchQuery = `
			SELECT
				p.id,
				p.url,
				p.title,
				p.description,
				snippet(pages_fts, 2, '<mark>', '</mark>', '...', 32) AS snippet,
				p.domain,
				p.language,
				p.region,
				p.crawled_at,
				CASE WHEN p.language = ? THEN rank * 1.5 ELSE rank END AS boosted_rank
			FROM pages_fts
			JOIN pages p ON p.id = pages_fts.rowid
			WHERE pages_fts MATCH ?
			ORDER BY boosted_rank
			LIMIT ? OFFSET ?
		`
		args = []any{lang, query, limit, offset}
	} else {
		searchQuery = `
			SELECT
				p.id,
				p.url,
				p.title,
				p.description,
				snippet(pages_fts, 2, '<mark>', '</mark>', '...', 32) AS snippet,
				p.domain,
				p.language,
				p.region,
				p.crawled_at,
				rank
			FROM pages_fts
			JOIN pages p ON p.id = pages_fts.rowid
			WHERE pages_fts MATCH ?
			ORDER BY rank
			LIMIT ? OFFSET ?
		`
		args = []any{query, limit, offset}
	}

	rows, err := s.readDB.QueryContext(ctx, searchQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("searching pages for %q: %w", query, err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var crawledAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.URL, &r.Title, &r.Description, &r.Snippet, &r.Domain, &r.Language, &r.Region, &crawledAt, &r.Rank); err != nil {
			return nil, 0, fmt.Errorf("scanning search result: %w", err)
		}
		if crawledAt.Valid {
			r.CrawledAt = crawledAt.Time
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating search results: %w", err)
	}

	return results, total, nil
}

// GetStalePages returns pages crawled before the given time, for re-crawling.
func (s *SQLiteStorage) GetStalePages(ctx context.Context, olderThan time.Time, limit int) ([]Page, error) {
	query := `
		SELECT id, url, domain, title, description, '', status_code, content_hash, crawled_at, created_at, updated_at
		FROM pages
		WHERE crawled_at < ?
		ORDER BY crawled_at ASC
		LIMIT ?
	`

	rows, err := s.readDB.QueryContext(ctx, query, olderThan, limit)
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

// --------------------------------------------------------------------------
// Crawl Queue Operations
// --------------------------------------------------------------------------

// EnqueueURL adds a single URL to the crawl queue.
// If the URL already exists in the queue, it is silently ignored.
func (s *SQLiteStorage) EnqueueURL(ctx context.Context, job *CrawlJob) error {
	query := `
		INSERT OR IGNORE INTO crawl_queue (url, domain, depth, priority, status)
		VALUES (?, ?, ?, ?, ?)
	`
	_, err := s.writeDB.ExecContext(ctx, query, job.URL, job.Domain, job.Depth, job.Priority, JobStatusPending)
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

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning enqueue transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO crawl_queue (url, domain, depth, priority, status) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing enqueue statement: %w", err)
	}
	defer stmt.Close()

	for _, job := range jobs {
		if _, err := stmt.ExecContext(ctx, job.URL, job.Domain, job.Depth, job.Priority, JobStatusPending); err != nil {
			return fmt.Errorf("enqueuing URL %q: %w", job.URL, err)
		}
	}

	return tx.Commit()
}

// DequeueURLs atomically claims up to `limit` pending jobs from the queue.
// Jobs are ordered by priority (descending), then by FIFO (added_at ascending).
// Claimed jobs are marked as in_progress.
func (s *SQLiteStorage) DequeueURLs(ctx context.Context, limit int) ([]*CrawlJob, error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning dequeue transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	// Select and lock pending jobs.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, url, domain, depth, priority, retries
		FROM crawl_queue
		WHERE status = ?
		ORDER BY priority DESC, added_at ASC
		LIMIT ?
	`, JobStatusPending, limit)
	if err != nil {
		return nil, fmt.Errorf("selecting pending jobs: %w", err)
	}

	var jobs []*CrawlJob
	for rows.Next() {
		var j CrawlJob
		if err := rows.Scan(&j.ID, &j.URL, &j.Domain, &j.Depth, &j.Priority, &j.Retries); err != nil {
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
		_, err := s.writeDB.ExecContext(ctx, `DELETE FROM crawl_queue WHERE id = ?`, jobID)
		if err != nil {
			return fmt.Errorf("deleting completed job %d: %w", jobID, err)
		}
		return nil
	}

	// Failure â€” mark as failed with error message, increment retries.
	_, err := s.writeDB.ExecContext(ctx, `
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

	tx, err := s.writeDB.BeginTx(ctx, nil)
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
		SELECT id, url, domain, depth, priority, retries
		FROM crawl_queue
		WHERE status = ? AND domain NOT IN (%s)
		ORDER BY priority DESC, added_at ASC
		LIMIT ?
	`, strings.Join(placeholders, ","))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("selecting pending jobs (excluding domains): %w", err)
	}

	var jobs []*CrawlJob
	for rows.Next() {
		var j CrawlJob
		if err := rows.Scan(&j.ID, &j.URL, &j.Domain, &j.Depth, &j.Priority, &j.Retries); err != nil {
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
	_, err := s.writeDB.ExecContext(ctx, `
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
	err := s.readDB.QueryRowContext(ctx, query).Scan(
		&stats.Pending, &stats.InProgress, &stats.Done, &stats.Failed, &stats.Total,
	)
	if err != nil {
		return nil, fmt.Errorf("querying queue stats: %w", err)
	}
	return &stats, nil
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

	_, err := s.writeDB.ExecContext(ctx, query,
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

	err := s.readDB.QueryRowContext(ctx, `
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
	rows, err := s.readDB.QueryContext(ctx, `
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
	result, err := s.writeDB.ExecContext(ctx, `UPDATE domains SET is_seed = FALSE WHERE domain = ?`, domainName)
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
func (s *SQLiteStorage) GetStats(ctx context.Context) (*CrawlerStats, error) {
	var stats CrawlerStats

	// Page & domain counts.
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages`).Scan(&stats.TotalPages); err != nil {
		return nil, fmt.Errorf("counting pages: %w", err)
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains`).Scan(&stats.TotalDomains); err != nil {
		return nil, fmt.Errorf("counting domains: %w", err)
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains WHERE is_seed = TRUE`).Scan(&stats.SeedDomains); err != nil {
		return nil, fmt.Errorf("counting seed domains: %w", err)
	}

	// Queue stats.
	queueStats, err := s.GetQueueStats(ctx)
	if err != nil {
		return nil, err
	}
	stats.Queue = *queueStats

	// Database size.
	info, err := os.Stat(s.dbPath)
	if err == nil {
		stats.DatabaseSizeMB = float64(info.Size()) / (1024 * 1024)
	}

	return &stats, nil
}

// --------------------------------------------------------------------------
// Maintenance
// --------------------------------------------------------------------------

// PurgeOldPages deletes pages crawled before the given timestamp.
func (s *SQLiteStorage) PurgeOldPages(ctx context.Context, olderThan time.Time) (int64, error) {
	result, err := s.writeDB.ExecContext(ctx, `DELETE FROM pages WHERE crawled_at < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("purging old pages: %w", err)
	}
	return result.RowsAffected()
}

// ResetStalledJobs resets jobs stuck in in_progress back to pending.
func (s *SQLiteStorage) ResetStalledJobs(ctx context.Context, stalledAfter time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-stalledAfter)
	result, err := s.writeDB.ExecContext(ctx, `
		UPDATE crawl_queue
		SET status = ?, locked_at = NULL
		WHERE status = ? AND locked_at < ?
	`, JobStatusPending, JobStatusInProgress, cutoff)
	if err != nil {
		return 0, fmt.Errorf("resetting stalled jobs: %w", err)
	}
	return result.RowsAffected()
}

// Vacuum reclaims unused disk space in the database.
func (s *SQLiteStorage) Vacuum(ctx context.Context) error {
	_, err := s.writeDB.ExecContext(ctx, "VACUUM")
	if err != nil {
		return fmt.Errorf("vacuuming database: %w", err)
	}
	s.logger.Info("database vacuumed successfully")
	return nil
}

// Close closes both SQLite connection pools.
func (s *SQLiteStorage) Close() error {
	s.logger.Info("closing SQLite storage")

	var writeErr, readErr error
	if s.writeDB != nil {
		writeErr = s.writeDB.Close()
	}
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}

	if writeErr != nil && readErr != nil {
		return fmt.Errorf("closing write database: %v; closing read database: %w", writeErr, readErr)
	}
	if writeErr != nil {
		return fmt.Errorf("closing write database: %w", writeErr)
	}
	if readErr != nil {
		return fmt.Errorf("closing read database: %w", readErr)
	}

	return nil
}

func configureWriteDB(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 30000",
		"PRAGMA cache_size = -20000",
	}

	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("executing %q: %w", pragma, err)
		}
	}

	return nil
}

func configureReadDB(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA cache_size = -20000",
		"PRAGMA query_only = ON",
	}

	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("executing %q: %w", pragma, err)
		}
	}

	return nil
}

// --------------------------------------------------------------------------
// Image Operations
// --------------------------------------------------------------------------

// UpsertImages inserts or updates image records in bulk.
func (s *SQLiteStorage) UpsertImages(ctx context.Context, images []ImageRecord) error {
	if len(images) == 0 {
		return nil
	}

	stmt, err := s.writeDB.PrepareContext(ctx, `
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
func (s *SQLiteStorage) SearchImages(ctx context.Context, query string, limit, offset int) ([]ImageSearchResult, int, error) {
	if query == "" {
		return nil, 0, nil
	}

	// Count total matches.
	var total int
	countQuery := `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH ?`
	if err := s.readDB.QueryRowContext(ctx, countQuery, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting image results for %q: %w", query, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	searchQuery := `
		SELECT
			i.id,
			i.url,
			i.page_url,
			i.domain,
			i.alt_text,
			i.title,
			i.context,
			i.width,
			i.height,
			rank
		FROM images_fts
		JOIN images i ON i.id = images_fts.rowid
		WHERE images_fts MATCH ?
		ORDER BY rank
		LIMIT ? OFFSET ?
	`

	rows, err := s.readDB.QueryContext(ctx, searchQuery, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("searching images for %q: %w", query, err)
	}
	defer rows.Close()

	var results []ImageSearchResult
	for rows.Next() {
		var r ImageSearchResult
		if err := rows.Scan(&r.ID, &r.URL, &r.PageURL, &r.Domain, &r.AltText, &r.Title, &r.Context, &r.Width, &r.Height, &r.Rank); err != nil {
			return nil, 0, fmt.Errorf("scanning image result: %w", err)
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating image results: %w", err)
	}

	return results, total, nil
}
