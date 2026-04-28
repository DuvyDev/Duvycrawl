package storage

// migrations contains the SQL schema executed when the database is first created
// or when a new migration is added. Each migration is idempotent (uses IF NOT EXISTS).
var contentMigrations = []string{
	// --- Pages table ---
	`CREATE TABLE IF NOT EXISTS pages (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		url                TEXT    NOT NULL UNIQUE,
		domain             TEXT    NOT NULL DEFAULT '',
		title              TEXT    NOT NULL DEFAULT '',
		h1                 TEXT    NOT NULL DEFAULT '',
		h2                 TEXT    NOT NULL DEFAULT '',
		description        TEXT    NOT NULL DEFAULT '',
		content            TEXT    NOT NULL DEFAULT '',
		language           TEXT    NOT NULL DEFAULT '',
		region             TEXT    NOT NULL DEFAULT '',
		status_code        INTEGER NOT NULL DEFAULT 0,
		content_hash       TEXT    NOT NULL DEFAULT '',
		url_fingerprint    TEXT    NOT NULL DEFAULT '',
		published_at       DATETIME,
		crawled_at         DATETIME,
		created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		schema_type        TEXT    NOT NULL DEFAULT '',
		schema_title       TEXT    NOT NULL DEFAULT '',
		schema_description TEXT    NOT NULL DEFAULT '',
		schema_image       TEXT    NOT NULL DEFAULT '',
		schema_author      TEXT    NOT NULL DEFAULT '',
		schema_keywords    TEXT    NOT NULL DEFAULT '',
		schema_rating      REAL,
		referring_domains  INTEGER NOT NULL DEFAULT 0,
		pagerank           REAL    NOT NULL DEFAULT 1.0,
		is_seed            BOOLEAN NOT NULL DEFAULT FALSE
	)`,

	// --- Performance indexes ---
	`CREATE INDEX IF NOT EXISTS idx_pages_domain ON pages(domain)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_crawled_at ON pages(crawled_at)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_content_hash ON pages(content_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_language ON pages(language)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_pages_fingerprint ON pages(url_fingerprint)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_published_at ON pages(published_at)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_schema_type ON pages(schema_type)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_referring_domains ON pages(referring_domains)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_pagerank ON pages(pagerank)`,
	`CREATE INDEX IF NOT EXISTS idx_pages_is_seed ON pages(is_seed)`,

	// --- Full-text search index ---
	// Includes title, h1, description, schema_title, schema_description and content
	// to improve matching on SPAs where visible content may be minimal.
	`CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
		title,
		h1,
		description,
		schema_title,
		schema_description,
		content,
		content='pages',
		content_rowid='id',
		tokenize='porter unicode61'
	)`,

	// FTS5 synchronization triggers
	`CREATE TRIGGER IF NOT EXISTS pages_fts_insert AFTER INSERT ON pages BEGIN
		INSERT INTO pages_fts(rowid, title, h1, description, schema_title, schema_description, content)
		VALUES (new.id, new.title, new.h1, new.description, new.schema_title, new.schema_description, new.content);
	END`,

	`CREATE TRIGGER IF NOT EXISTS pages_fts_delete AFTER DELETE ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, h1, description, schema_title, schema_description, content)
		VALUES ('delete', old.id, old.title, old.h1, old.description, old.schema_title, old.schema_description, old.content);
	END`,

	`CREATE TRIGGER IF NOT EXISTS pages_fts_update AFTER UPDATE ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, h1, description, schema_title, schema_description, content)
		VALUES ('delete', old.id, old.title, old.h1, old.description, old.schema_title, old.schema_description, old.content);
		INSERT INTO pages_fts(rowid, title, h1, description, schema_title, schema_description, content)
		VALUES (new.id, new.title, new.h1, new.description, new.schema_title, new.schema_description, new.content);
	END`,

	// --- Phase 2: Global IDF (FTS5 Vocab) ---
	`CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts_vocab USING fts5vocab(pages_fts, row)`,

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
	`CREATE INDEX IF NOT EXISTS idx_images_domain ON images(domain)`,
	`CREATE INDEX IF NOT EXISTS idx_images_page_id ON images(page_id)`,

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

	// --- Phase 3: User Signals (CTR) ---
	`CREATE TABLE IF NOT EXISTS search_clicks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		query TEXT NOT NULL,
		url TEXT NOT NULL,
		clicks INTEGER NOT NULL DEFAULT 1,
		UNIQUE(query, url)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_search_clicks_query ON search_clicks(query)`,

    // --- Phase 4: Adaptive Interest Profile ---
    `CREATE TABLE IF NOT EXISTS search_queries (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        query TEXT NOT NULL,
        normalized_query TEXT NOT NULL,
        lang TEXT,
        created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`,
    `CREATE INDEX IF NOT EXISTS idx_search_queries_normalized ON search_queries(normalized_query)`,

    `CREATE TABLE IF NOT EXISTS interest_terms (
        term TEXT NOT NULL,
        weight REAL NOT NULL DEFAULT 1.0,
        source TEXT NOT NULL DEFAULT 'query',
        lang TEXT,
        last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
        PRIMARY KEY(term, source)
    )`,

    `CREATE TABLE IF NOT EXISTS domain_relevance (
        domain TEXT NOT NULL PRIMARY KEY,
        score REAL NOT NULL DEFAULT 0.0,
        pages_count INTEGER NOT NULL DEFAULT 0,
        last_updated DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`,

    // --- Phase 5: Semantic Embeddings for AI Re-ranking ---
    `CREATE TABLE IF NOT EXISTS page_embeddings (
        page_id INTEGER PRIMARY KEY,
        model TEXT NOT NULL DEFAULT 'all-minilm',
        dimensions INTEGER NOT NULL DEFAULT 384,
        embedding BLOB NOT NULL,
        created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY (page_id) REFERENCES pages(id) ON DELETE CASCADE
    )`,
    `CREATE INDEX IF NOT EXISTS idx_embeddings_model ON page_embeddings(model)`,
}

var crawlerMigrations = []string{
	// --- Crawl queue table ---
	`CREATE TABLE IF NOT EXISTS crawl_queue (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		url       TEXT    NOT NULL UNIQUE,
		domain    TEXT    NOT NULL DEFAULT '',
		depth     INTEGER NOT NULL DEFAULT 0,
		score     REAL    NOT NULL DEFAULT 0,
		status    TEXT    NOT NULL DEFAULT 'pending',
		retries   INTEGER NOT NULL DEFAULT 0,
		error_msg TEXT    NOT NULL DEFAULT '',
		added_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		locked_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_queue_status_score ON crawl_queue(status, score DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_queue_domain ON crawl_queue(domain)`,

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
	`CREATE INDEX IF NOT EXISTS idx_domains_seed ON domains(is_seed)`,
	`CREATE INDEX IF NOT EXISTS idx_domains_domain ON domains(domain)`,
}

var graphMigrations = []string{
	// --- Links table (for backlink analysis) ---
	`CREATE TABLE IF NOT EXISTS links (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL,
		source_url TEXT NOT NULL,
		target_url TEXT NOT NULL,
		anchor_text TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_links_source_target ON links(source_id, target_url)`,
	`CREATE INDEX IF NOT EXISTS idx_links_target_url ON links(target_url)`,
	`CREATE INDEX IF NOT EXISTS idx_links_source_id ON links(source_id)`,
}
