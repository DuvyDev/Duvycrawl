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
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"

	"github.com/DuvyDev/Duvycrawl/internal/cache"
)

// StorageMode determines which database connections are opened.
type StorageMode int

const (
	ModeMonolith  StorageMode = iota // Opens everything with write permissions.
	ModeSearchAPI                    // Opens content.db/graph.db as read-only, crawler.db with write (to enqueue URLs).
	ModeCrawler                      // Opens everything with write permissions.
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
	readContentDB   *sql.DB
	searchContentDB *sql.DB
	writeContentDB  *sql.DB
	crawlerDB       *sql.DB
	graphDB         *sql.DB
	logger          *slog.Logger
	dataDir         string
	siteTypes       map[string]struct{}
	platformDomains map[string]struct{}
	spellChecker    *SpellChecker
	scoringCfg      ScoringConfig
	cache           *cache.Cache
}

// Search types are defined in sqlite_search.go

var searchTextNormalizer = transform.Chain(
	norm.NFD,
	runes.Remove(runes.In(unicode.Mn)),
	norm.NFC,
)

// NewSQLiteStorage creates a new SQLite-backed storage based on the provided mode.
// It creates the database directory if needed and opens content.db, crawler.db, and graph.db.
func NewSQLiteStorage(ctx context.Context, dbPath string, mode StorageMode, logger *slog.Logger) (*SQLiteStorage, error) {
	// Ensure the directory exists.
	// We treat dbPath as the base path. If it ends in .db, we use its directory.
	dir := dbPath
	if strings.HasSuffix(dbPath, ".db") {
		dir = filepath.Dir(dbPath)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating database directory %q: %w", dir, err)
	}

	contentPath := filepath.Join(dir, "content.db")
	crawlerPath := filepath.Join(dir, "crawler.db")
	graphPath := filepath.Join(dir, "graph.db")

	// Helper to open a DB with a specific DSN
	openDB := func(dsn string, maxOpen, maxIdle int) (*sql.DB, error) {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxIdle)
		db.SetConnMaxLifetime(0)
		return db, nil
	}

	// --- 1. Content DB (Search Engine Core) ---
	var err error
	var writeContentDB *sql.DB
	if mode == ModeMonolith || mode == ModeCrawler || mode == ModeSearchAPI {
		writeContentDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-200000&_foreign_keys=ON&_loc=auto", contentPath)
		writeContentDB, err = openDB(writeContentDSN, 1, 1)
		if err != nil {
			return nil, fmt.Errorf("opening write content database: %w", err)
		}
		if err := configureWriteDB(ctx, writeContentDB); err != nil {
			writeContentDB.Close()
			return nil, fmt.Errorf("configuring write content database: %w", err)
		}
		if err := migrateDB(ctx, writeContentDB, contentMigrations); err != nil {
			writeContentDB.Close()
			return nil, fmt.Errorf("running content migrations: %w", err)
		}
	}

	readContentDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-200000&_foreign_keys=ON&mode=ro&_loc=auto", contentPath)
	readContentDB, err := openDB(readContentDSN, 16, 8)
	if err != nil {
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("opening read content database: %w", err)
	}
	if err := configureReadDB(ctx, readContentDB); err != nil {
		readContentDB.Close()
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("configuring read content database: %w", err)
	}

	searchContentDB, err := openDB(readContentDSN, 8, 8)
	if err != nil {
		readContentDB.Close()
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("opening search content database: %w", err)
	}
	if err := configureReadDB(ctx, searchContentDB); err != nil {
		searchContentDB.Close()
		readContentDB.Close()
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("configuring search content database: %w", err)
	}

	// --- 2. Crawler DB (State Machine) ---
	// The API needs write access to enqueue manual URLs.
	crawlerDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-50000&_foreign_keys=ON&_loc=auto", crawlerPath)
	crawlerDB, err := openDB(crawlerDSN, 1, 1) // Keep single writer for queue
	if err != nil {
		searchContentDB.Close()
		readContentDB.Close()
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("opening crawler database: %w", err)
	}
	if err := configureWriteDB(ctx, crawlerDB); err != nil {
		crawlerDB.Close()
		searchContentDB.Close()
		readContentDB.Close()
		if writeContentDB != nil {
			writeContentDB.Close()
		}
		return nil, fmt.Errorf("configuring crawler database: %w", err)
	}
	if mode == ModeMonolith || mode == ModeCrawler {
		if err := migrateDB(ctx, crawlerDB, crawlerMigrations); err != nil {
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("running crawler migrations: %w", err)
		}
	}

	// --- 3. Graph DB (Web Graph) ---
	var graphDB *sql.DB
	if mode == ModeMonolith || mode == ModeCrawler {
		// _synchronous=OFF for extreme write speed on links
		graphDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=OFF&_cache_size=-50000&_foreign_keys=ON&_loc=auto", graphPath)
		graphDB, err = openDB(graphDSN, 1, 1)
		if err != nil {
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("opening graph database: %w", err)
		}
		if err := configureWriteDB(ctx, graphDB); err != nil {
			graphDB.Close()
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("configuring graph database: %w", err)
		}
		if err := migrateDB(ctx, graphDB, graphMigrations); err != nil {
			graphDB.Close()
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("running graph migrations: %w", err)
		}
	} else {
		// Read-only connection for API to serve backlinks
		graphDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-50000&_foreign_keys=ON&mode=ro&_loc=auto", graphPath)
		graphDB, err = openDB(graphDSN, 4, 2)
		if err != nil {
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("opening read graph database: %w", err)
		}
		if err := configureReadDB(ctx, graphDB); err != nil {
			graphDB.Close()
			crawlerDB.Close()
			searchContentDB.Close()
			readContentDB.Close()
			if writeContentDB != nil {
				writeContentDB.Close()
			}
			return nil, fmt.Errorf("configuring read graph database: %w", err)
		}
	}

	c, err := cache.NewCache(cache.Config{
		NumCounters: 1e7,     // 10M keys
		MaxCost:     1 << 29, // 512MB RAM max
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("initializing ristretto cache: %w", err)
	}

	s := &SQLiteStorage{
		readContentDB:   readContentDB,
		searchContentDB: searchContentDB,
		writeContentDB:  writeContentDB,
		crawlerDB:       crawlerDB,
		graphDB:         graphDB,
		logger:          logger.With("component", "storage"),
		dataDir:         dir,
		spellChecker:    NewSpellChecker(logger),
		cache:           c,
	}

	// Start loading the spell checker vocabulary and reload it every 2 hours
	s.spellChecker.StartPeriodicReload(context.Background(), readContentDB, 2*time.Hour)

	logger.Info("SQLite storage initialized (Multi-DB Architecture)",
		"dir", dir,
	)

	return s, nil
}

// migrate executes schema migrations on a given database.
func migrateDB(ctx context.Context, db *sql.DB, migrations []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration transaction: %w", err)
	}
	defer tx.Rollback()

	for i, m := range migrations {
		if _, err := tx.ExecContext(ctx, m); err != nil {
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
func (s *SQLiteStorage) GetStats(ctx context.Context) (*CrawlerStats, error) {
	var stats CrawlerStats

	// Page counts.
	if err := s.readContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages`).Scan(&stats.TotalPages); err != nil {
		return nil, fmt.Errorf("counting pages: %w", err)
	}
	// Domain counts.
	if err := s.crawlerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains`).Scan(&stats.TotalDomains); err != nil {
		return nil, fmt.Errorf("counting domains: %w", err)
	}
	if err := s.crawlerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM seed_urls`).Scan(&stats.SeedDomains); err != nil {
		return nil, fmt.Errorf("counting seed urls: %w", err)
	}

	// Queue stats.
	queueStats, err := s.GetQueueStats(ctx)
	if err != nil {
		return nil, err
	}
	stats.Queue = *queueStats

	// Database size.
	var totalSize int64
	for _, file := range []string{"content.db", "crawler.db", "graph.db"} {
		info, err := os.Stat(filepath.Join(s.dataDir, file))
		if err == nil {
			totalSize += info.Size()
		}
	}
	stats.DatabaseSizeMB = float64(totalSize) / (1024 * 1024)

	return &stats, nil
}

// --------------------------------------------------------------------------
// Maintenance
// --------------------------------------------------------------------------

// PurgeOldPages deletes pages crawled before the given timestamp.
func (s *SQLiteStorage) PurgeOldPages(ctx context.Context, olderThan time.Time) (int64, error) {
	result, err := s.writeContentDB.ExecContext(ctx, `DELETE FROM pages WHERE crawled_at < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("purging old pages: %w", err)
	}
	return result.RowsAffected()
}

// PurgeBlacklistedDomains removes all data for the given domains from all 3 databases.
// For each domain d, it matches both exact (domain = d) and subdomains (domain LIKE '%.d').
// Returns total rows affected across all tables.
func (s *SQLiteStorage) PurgeBlacklistedDomains(ctx context.Context, domains []string) (int64, error) {
	if len(domains) == 0 {
		return 0, nil
	}

	var totalAffected int64

	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		subdomainPattern := "%." + d

		affected, err := s.purgeDomainFromContentDB(ctx, d, subdomainPattern)
		if err != nil {
			return totalAffected, fmt.Errorf("purging domain %q from content DB: %w", d, err)
		}
		totalAffected += affected

		affected, err = s.purgeDomainFromCrawlerDB(ctx, d, subdomainPattern)
		if err != nil {
			return totalAffected, fmt.Errorf("purging domain %q from crawler DB: %w", d, err)
		}
		totalAffected += affected

		affected, err = s.purgeDomainFromGraphDB(ctx, d, subdomainPattern)
		if err != nil {
			return totalAffected, fmt.Errorf("purging domain %q from graph DB: %w", d, err)
		}
		totalAffected += affected
	}

	if totalAffected > 0 {
		s.logger.Info("purged blacklisted domains", "domains", domains, "rows_affected", totalAffected)
	}

	return totalAffected, nil
}

func (s *SQLiteStorage) purgeDomainFromContentDB(ctx context.Context, domain, subdomainPattern string) (int64, error) {
	var total int64

	// 1. search_clicks — must be deleted BEFORE pages (no domain column, uses url)
	clicksResult, err := s.writeContentDB.ExecContext(ctx, `
		DELETE FROM search_clicks
		WHERE url IN (SELECT url FROM pages WHERE domain = ? OR domain LIKE ?)
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging search_clicks: %w", err)
	}
	if n, _ := clicksResult.RowsAffected(); n > 0 {
		total += n
	}

	// 2. images — has domain column
	imgResult, err := s.writeContentDB.ExecContext(ctx, `
		DELETE FROM images WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging images: %w", err)
	}
	if n, _ := imgResult.RowsAffected(); n > 0 {
		total += n
	}

	// 3. domain_relevance — has domain column
	drResult, err := s.writeContentDB.ExecContext(ctx, `
		DELETE FROM domain_relevance WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging domain_relevance: %w", err)
	}
	if n, _ := drResult.RowsAffected(); n > 0 {
		total += n
	}

	// 4. discovered_resources — no domain column, match by URL pattern
	// URLs look like "https://sub.example.com/path"
	httpPattern := "http://%." + domain + "/%"
	httpPattern2 := "http://" + domain + "/%"
	drDiscResult, err := s.writeContentDB.ExecContext(ctx, `
		DELETE FROM discovered_resources
		WHERE url LIKE ? OR url LIKE ?
	`, httpPattern, httpPattern2)
	if err != nil {
		return total, fmt.Errorf("purging discovered_resources: %w", err)
	}
	if n, _ := drDiscResult.RowsAffected(); n > 0 {
		total += n
	}

	// 5. pages — FTS5 triggers handle pages_fts automatically
	pagesResult, err := s.writeContentDB.ExecContext(ctx, `
		DELETE FROM pages WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging pages: %w", err)
	}
	if n, _ := pagesResult.RowsAffected(); n > 0 {
		total += n
	}

	return total, nil
}

func (s *SQLiteStorage) purgeDomainFromCrawlerDB(ctx context.Context, domain, subdomainPattern string) (int64, error) {
	var total int64

	// 1. crawl_queue
	cqResult, err := s.crawlerDB.ExecContext(ctx, `
		DELETE FROM crawl_queue WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging crawl_queue: %w", err)
	}
	if n, _ := cqResult.RowsAffected(); n > 0 {
		total += n
	}

	// 2. domains
	dResult, err := s.crawlerDB.ExecContext(ctx, `
		DELETE FROM domains WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging domains: %w", err)
	}
	if n, _ := dResult.RowsAffected(); n > 0 {
		total += n
	}

	// 3. seed_urls
	suResult, err := s.crawlerDB.ExecContext(ctx, `
		DELETE FROM seed_urls WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return total, fmt.Errorf("purging seed_urls: %w", err)
	}
	if n, _ := suResult.RowsAffected(); n > 0 {
		total += n
	}

	return total, nil
}

func (s *SQLiteStorage) purgeDomainFromGraphDB(ctx context.Context, domain, subdomainPattern string) (int64, error) {
	// links table has no domain column — uses source_id which references pages.id.
	// We need to cross-reference with content.db.
	// Strategy: ATTACH content.db to graph.db, delete links where source_id
	// references a blacklisted domain, then DETACH.

	// First, collect page IDs for this domain from content.db.
	rows, err := s.writeContentDB.QueryContext(ctx, `
		SELECT id FROM pages WHERE domain = ? OR domain LIKE ?
	`, domain, subdomainPattern)
	if err != nil {
		return 0, fmt.Errorf("querying page IDs for graph purge: %w", err)
	}
	defer rows.Close()

	var pageIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scanning page ID: %w", err)
		}
		pageIDs = append(pageIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating page IDs: %w", err)
	}

	if len(pageIDs) == 0 {
		return 0, nil
	}

	// Delete links in batches to avoid SQLite variable limit (999).
	var total int64
	const batchSize = 500
	for i := 0; i < len(pageIDs); i += batchSize {
		end := i + batchSize
		if end > len(pageIDs) {
			end = len(pageIDs)
		}
		batch := pageIDs[i:end]

		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			placeholders[j] = "?"
			args[j] = id
		}
		query := fmt.Sprintf("DELETE FROM links WHERE source_id IN (%s)", strings.Join(placeholders, ","))
		result, err := s.graphDB.ExecContext(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("purging links batch: %w", err)
		}
		if n, _ := result.RowsAffected(); n > 0 {
			total += n
		}
	}

	return total, nil
}

// ResetStalledJobs resets jobs stuck in in_progress back to pending.
func (s *SQLiteStorage) ResetStalledJobs(ctx context.Context, stalledAfter time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-stalledAfter)
	result, err := s.crawlerDB.ExecContext(ctx, `
		UPDATE crawl_queue
		SET status = ?, locked_at = NULL
		WHERE status = ? AND locked_at < ?
	`, JobStatusPending, JobStatusInProgress, cutoff)
	if err != nil {
		return 0, fmt.Errorf("resetting stalled jobs: %w", err)
	}
	return result.RowsAffected()
}

// Vacuum reclaims unused disk space in all databases.
func (s *SQLiteStorage) Vacuum(ctx context.Context) error {
	if _, err := s.writeContentDB.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuuming content database: %w", err)
	}
	if _, err := s.crawlerDB.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuuming crawler database: %w", err)
	}
	if _, err := s.graphDB.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuuming graph database: %w", err)
	}
	s.logger.Info("all databases vacuumed successfully")
	return nil
}

// UpdatePageRankings recalculates the referring_domains score for pages.
// It uses a batched approach to avoid locking the database for too long.
// WriteContentDB returns the underlying write connection pool for content.
func (s *SQLiteStorage) WriteContentDB() *sql.DB {
	return s.writeContentDB
}

// GraphDB returns the underlying connection pool for the graph database.
func (s *SQLiteStorage) GraphDB() *sql.DB {
	return s.graphDB
}

// WithSearchIntents configures the dictionaries for navigational search intent extraction.
func (s *SQLiteStorage) WithSearchIntents(siteTypes, platformDomains []string) *SQLiteStorage {
	s.siteTypes = make(map[string]struct{}, len(siteTypes))
	for _, t := range siteTypes {
		s.siteTypes[strings.ToLower(t)] = struct{}{}
	}
	s.platformDomains = make(map[string]struct{}, len(platformDomains))
	for _, p := range platformDomains {
		s.platformDomains[strings.ToLower(p)] = struct{}{}
	}
	return s
}

// WithScoringConfig sets the search scoring configuration for language-aware ranking.
func (s *SQLiteStorage) WithScoringConfig(cfg ScoringConfig) *SQLiteStorage {
	if cfg.LanguageBoost <= 0 {
		cfg.LanguageBoost = 3000.0
	}
	if cfg.SecondaryLanguage == "" {
		cfg.SecondaryLanguage = "en"
	}
	s.scoringCfg = cfg
	return s
}

// Close closes all SQLite connection pools.
func (s *SQLiteStorage) Close() error {
	s.logger.Info("closing SQLite storage")

	var errs []error
	if s.writeContentDB != nil {
		if err := s.writeContentDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing writeContentDB: %w", err))
		}
	}
	if s.readContentDB != nil {
		if err := s.readContentDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing readContentDB: %w", err))
		}
	}
	if s.searchContentDB != nil {
		if err := s.searchContentDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing searchContentDB: %w", err))
		}
	}
	if s.crawlerDB != nil {
		if err := s.crawlerDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing crawlerDB: %w", err))
		}
	}
	if s.graphDB != nil {
		if err := s.graphDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing graphDB: %w", err))
		}
	}

	if s.cache != nil {
		s.cache.Close()
	}

	if len(errs) > 0 {
		return fmt.Errorf("encountered errors during close: %v", errs)
	}
	return nil
}

func configureWriteDB(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 30000",
		"PRAGMA cache_size = -200000",
		"PRAGMA mmap_size = 30000000000",
		"PRAGMA temp_store = MEMORY",
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
		"PRAGMA cache_size = -200000",
		"PRAGMA mmap_size = 30000000000",
		"PRAGMA temp_store = MEMORY",
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
