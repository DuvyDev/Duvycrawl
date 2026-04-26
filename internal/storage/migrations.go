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
		h1           TEXT    NOT NULL DEFAULT '',
		h2           TEXT    NOT NULL DEFAULT '',
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

	// --- Migration: add language/region columns to pages ---
	`ALTER TABLE pages ADD COLUMN language TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN region TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN h1 TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN h2 TEXT NOT NULL DEFAULT ''`,

	// --- Images table ---
	`CREATE TABLE IF NOT EXISTS images (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		url        TEXT    NOT NULL UNIQUE,
		page_url   TEXT    NOT NULL DEFAULT '',
		page_id    INTEGER NOT NULL DEFAULT 0,
		domain     TEXT    NOT NULL DEFAULT '',
		alt_text   TEXT    NOT NULL DEFAULT '',
		title      TEXT    NOT NULL DEFAULT '',
		context    TEXT    NOT NULL DEFAULT '',
		width      INTEGER NOT NULL DEFAULT 0,
		height     INTEGER NOT NULL DEFAULT 0,
		crawled_at DATETIME
	)`,

	// --- Images FTS5 index ---
	`CREATE VIRTUAL TABLE IF NOT EXISTS images_fts USING fts5(
		alt_text,
		title,
		context,
		content='images',
		content_rowid='id',
		tokenize='porter unicode61'
	)`,

	// Images FTS5 sync triggers.
	`CREATE TRIGGER IF NOT EXISTS images_fts_insert AFTER INSERT ON images BEGIN
		INSERT INTO images_fts(rowid, alt_text, title, context)
		VALUES (new.id, new.alt_text, new.title, new.context);
	END`,

	`CREATE TRIGGER IF NOT EXISTS images_fts_delete AFTER DELETE ON images BEGIN
		INSERT INTO images_fts(images_fts, rowid, alt_text, title, context)
		VALUES ('delete', old.id, old.alt_text, old.title, old.context);
	END`,

	`CREATE TRIGGER IF NOT EXISTS images_fts_update AFTER UPDATE ON images BEGIN
		INSERT INTO images_fts(images_fts, rowid, alt_text, title, context)
		VALUES ('delete', old.id, old.alt_text, old.title, old.context);
		INSERT INTO images_fts(rowid, alt_text, title, context)
		VALUES (new.id, new.alt_text, new.title, new.context);
	END`,

	// --- Performance indexes for new tables ---
	`CREATE INDEX IF NOT EXISTS idx_pages_language ON pages(language)`,
	`CREATE INDEX IF NOT EXISTS idx_images_domain ON images(domain)`,
	`CREATE INDEX IF NOT EXISTS idx_images_page_id ON images(page_id)`,

	// --- URL fingerprint for structural deduplication ---
	`ALTER TABLE pages ADD COLUMN url_fingerprint TEXT NOT NULL DEFAULT ''`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_pages_fingerprint ON pages(url_fingerprint)`,

	// --- Published date for freshness scoring ---
	`ALTER TABLE pages ADD COLUMN published_at DATETIME`,
	`CREATE INDEX IF NOT EXISTS idx_pages_published_at ON pages(published_at)`,

	// --- Schema.org (JSON-LD) structured data ---
	`ALTER TABLE pages ADD COLUMN schema_type TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_title TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_description TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_image TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_author TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_keywords TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pages ADD COLUMN schema_rating REAL`,
	`CREATE INDEX IF NOT EXISTS idx_pages_schema_type ON pages(schema_type)`,

	// --- Links table (for backlink analysis) ---
	`CREATE TABLE IF NOT EXISTS links (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL,
		source_url TEXT NOT NULL,
		target_url TEXT NOT NULL,
		anchor_text TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (source_id) REFERENCES pages(id) ON DELETE CASCADE
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_links_source_target ON links(source_id, target_url)`,
	`CREATE INDEX IF NOT EXISTS idx_links_target_url ON links(target_url)`,
	`CREATE INDEX IF NOT EXISTS idx_links_source_id ON links(source_id)`,

	// --- Backlinks Scoring ---
	`ALTER TABLE pages ADD COLUMN referring_domains INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX IF NOT EXISTS idx_pages_referring_domains ON pages(referring_domains)`,

	// --- Phase 1: Iterative PageRank ---
	`ALTER TABLE pages ADD COLUMN pagerank REAL NOT NULL DEFAULT 1.0`,
	`CREATE INDEX IF NOT EXISTS idx_pages_pagerank ON pages(pagerank)`,

	// --- Phase 2: Global IDF (FTS5 Vocab) ---
	`CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts_vocab USING fts5vocab(pages_fts, row)`,

	// --- Phase 3: User Signals (CTR) ---
	`CREATE TABLE IF NOT EXISTS search_clicks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		query TEXT NOT NULL,
		url TEXT NOT NULL,
		clicks INTEGER NOT NULL DEFAULT 1,
		UNIQUE(query, url)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_search_clicks_query ON search_clicks(query)`,
}
