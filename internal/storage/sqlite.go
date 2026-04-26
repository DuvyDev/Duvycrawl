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

type searchMode string

const (
	searchModeNavigational searchMode = "navigational"
	searchModeFTSPhrase    searchMode = "fts_phrase"
	searchModeFTSProximity searchMode = "fts_proximity"
	searchModeFTSExact     searchMode = "fts_exact"
	searchModeFTSMajority  searchMode = "fts_majority"
	searchModeFTSPrefix    searchMode = "fts_prefix"
	searchModeFTSRelaxed   searchMode = "fts_relaxed"
	searchModeFuzzy        searchMode = "fuzzy"
)

type searchQuery struct {
	raw          string
	lowered      string
	normalized   string
	compact      string
	tokens       []string
	fragments    []string
	navTerm      string
	domainLike   string
	navigational bool
	idfMap       map[string]float64
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
	writeDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&_loc=auto", dbPath)
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
	readDSN := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=ON&mode=ro&_loc=auto", dbPath)
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("opening read database: %w", err)
	}
	readDB.SetMaxOpenConns(8)
	readDB.SetMaxIdleConns(8)
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

// UpdatePageRankings recalculates the referring_domains score for pages.
// It uses a batched approach to avoid locking the database for too long.
func (s *SQLiteStorage) WriteDB() *sql.DB {
	return s.writeDB
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
