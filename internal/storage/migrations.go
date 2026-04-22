package storage

// migrations contains the SQL schema executed when the database is first created
// or when a new migration is added. Each migration is idempotent (uses IF NOT EXISTS).
var migrations = []string{
	// --- Pages table ---
	`CREATE TABLE IF NOT EXISTS pages (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		url          TEXT    NOT NULL UNIQUE,
		domain       TEXT    NOT NULL DEFAULT '',
		title        TEXT    NOT NULL DEFAULT '',
		description  TEXT    NOT NULL DEFAULT '',
		content      TEXT    NOT NULL DEFAULT '',
		status_code  INTEGER NOT NULL DEFAULT 0,
		content_hash TEXT    NOT NULL DEFAULT '',
		crawled_at   DATETIME,
		created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// --- Full-text search index ---
	`CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
		title,
		description,
		content,
		content='pages',
		content_rowid='id',
		tokenize='porter unicode61'
	)`,

	// FTS5 synchronization triggers: keep the FTS index in sync with the pages table.
	`CREATE TRIGGER IF NOT EXISTS pages_fts_insert AFTER INSERT ON pages BEGIN
		INSERT INTO pages_fts(rowid, title, description, content)
		VALUES (new.id, new.title, new.description, new.content);
	END`,

	`CREATE TRIGGER IF NOT EXISTS pages_fts_delete AFTER DELETE ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, description, content)
		VALUES ('delete', old.id, old.title, old.description, old.content);
	END`,

	`CREATE TRIGGER IF NOT EXISTS pages_fts_update AFTER UPDATE ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, description, content)
		VALUES ('delete', old.id, old.title, old.description, old.content);
		INSERT INTO pages_fts(rowid, title, description, content)
		VALUES (new.id, new.title, new.description, new.content);
	END`,

	// --- Crawl queue table ---
	`CREATE TABLE IF NOT EXISTS crawl_queue (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		url       TEXT    NOT NULL UNIQUE,
		domain    TEXT    NOT NULL DEFAULT '',
		depth     INTEGER NOT NULL DEFAULT 0,
		priority  INTEGER NOT NULL DEFAULT 0,
		status    TEXT    NOT NULL DEFAULT 'pending',
		retries   INTEGER NOT NULL DEFAULT 0,
		error_msg TEXT    NOT NULL DEFAULT '',
		added_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		locked_at DATETIME
	)`,

	// --- Domains table ---
	`CREATE TABLE IF NOT EXISTS domains (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		domain          TEXT    NOT NULL UNIQUE,
		is_seed         BOOLEAN NOT NULL DEFAULT FALSE,
		robots_txt      TEXT    NOT NULL DEFAULT '',
		robots_fetched  DATETIME,
		last_crawled    DATETIME,
		pages_count     INTEGER NOT NULL DEFAULT 0,
		avg_response_ms INTEGER NOT NULL DEFAULT 0,
		created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// --- Performance indexes ---
	`CREATE INDEX IF NOT EXISTS idx_pages_domain ON pages(domain)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_crawled_at ON pages(crawled_at)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_content_hash ON pages(content_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_queue_status_priority ON crawl_queue(status, priority DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_queue_domain ON crawl_queue(domain)`,
	`CREATE INDEX IF NOT EXISTS idx_domains_seed ON domains(is_seed)`,
	`CREATE INDEX IF NOT EXISTS idx_domains_domain ON domains(domain)`,
}
