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

	"github.com/DuvyDev/Duvycrawl/internal/embedder"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
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
	readContentDB  *sql.DB
	writeContentDB *sql.DB
	crawlerDB      *sql.DB
	graphDB        *sql.DB
	logger         *slog.Logger
	dataDir         string
	embedder        *embedder.Client
	siteTypes       map[string]struct{}
	platformDomains map[string]struct{}
}

// ... skipped types, keeping them below ...
type searchMode string

const (
	searchModeNavigational searchMode = "navigational"
	searchModeFTSPhrase    searchMode = "fts_phrase"
	searchModeFTSProximity searchMode = "fts_proximity"
	searchModeFTSExact     searchMode = "fts_exact"
	searchModeFTSMajority  searchMode = "fts_majority"
	searchModeFTSPrefix    searchMode = "fts_prefix"
	searchModeFTSCore      searchMode = "fts_core"
	searchModeFTSCore2     searchMode = "fts_core2"
	searchModeFTSRelaxed   searchMode = "fts_relaxed"
	searchModeFuzzy        searchMode = "fuzzy"
)

type searchQuery struct {
	raw            string
	lowered        string
	normalized     string
	compact        string
	tokens         []string
	fragments      []string
	navTerm        string
	domainLike     string
	navigational   bool
	siteTypeIntent string // e.g. "wiki", "docs", "blog" — detected from query tokens
	platformIntent string // e.g. "reddit", "youtube" — detected from query tokens
	idfMap         map[string]float64
}

type searchCandidate struct {
	SearchResult
	H1              string
	H2              string
	BodyPreview     string
	sqlScore        float64
	isSeed          bool
	contentLen      int
	mode            searchMode
	publishedAtTime time.Time
}

type searchDomainInfo struct {
	effectiveDomain string
	rootLabel       string
	isRootDomain    bool
}

var searchTextNormalizer = transform.Chain(
	norm.NFD,
	runes.Remove(runes.In(unicode.Mn)),
	norm.NFC,
)

// NewSQLiteStorage creates a new SQLite-backed storage.
// It creates the database directory if needed and opens content.db, crawler.db, and graph.db.
func NewSQLiteStorage(ctx context.Context, dbPath string, logger *slog.Logger) (*SQLiteStorage, error) {
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
	writeContentDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&_loc=auto", contentPath)
	writeContentDB, err := openDB(writeContentDSN, 1, 1)
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

	readContentDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&mode=ro&_loc=auto", contentPath)
	readContentDB, err := openDB(readContentDSN, 8, 8)
	if err != nil {
		writeContentDB.Close()
		return nil, fmt.Errorf("opening read content database: %w", err)
	}
	if err := configureReadDB(ctx, readContentDB); err != nil {
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("configuring read content database: %w", err)
	}

	// --- 2. Crawler DB (State Machine) ---
	crawlerDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-10000&_foreign_keys=ON&_loc=auto", crawlerPath)
	crawlerDB, err := openDB(crawlerDSN, 1, 1) // Keep single writer for queue
	if err != nil {
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("opening crawler database: %w", err)
	}
	if err := configureWriteDB(ctx, crawlerDB); err != nil {
		crawlerDB.Close()
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("configuring crawler database: %w", err)
	}
	if err := migrateDB(ctx, crawlerDB, crawlerMigrations); err != nil {
		crawlerDB.Close()
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("running crawler migrations: %w", err)
	}

	// --- 3. Graph DB (Web Graph) ---
	// _synchronous=OFF for extreme write speed on links, corruption here is acceptable as it can be rebuilt
	graphDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=OFF&_cache_size=-10000&_foreign_keys=ON&_loc=auto", graphPath)
	graphDB, err := openDB(graphDSN, 1, 1)
	if err != nil {
		crawlerDB.Close()
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("opening graph database: %w", err)
	}
	if err := configureWriteDB(ctx, graphDB); err != nil {
		graphDB.Close()
		crawlerDB.Close()
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("configuring graph database: %w", err)
	}
	if err := migrateDB(ctx, graphDB, graphMigrations); err != nil {
		graphDB.Close()
		crawlerDB.Close()
		readContentDB.Close()
		writeContentDB.Close()
		return nil, fmt.Errorf("running graph migrations: %w", err)
	}

	s := &SQLiteStorage{
		readContentDB:  readContentDB,
		writeContentDB: writeContentDB,
		crawlerDB:      crawlerDB,
		graphDB:        graphDB,
		logger:         logger.With("component", "storage"),
		dataDir:        dir,
	}

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
	if err := s.crawlerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains WHERE is_seed = TRUE`).Scan(&stats.SeedDomains); err != nil {
		return nil, fmt.Errorf("counting seed domains: %w", err)
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

// WithEmbedder attaches an Ollama embedding client for semantic search.
func (s *SQLiteStorage) WithEmbedder(client *embedder.Client) *SQLiteStorage {
	s.embedder = client
	return s
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
