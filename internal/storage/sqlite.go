package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/publicsuffix"
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
func (s *SQLiteStorage) UpsertPage(ctx context.Context, page *Page) error {
	query := `
		INSERT INTO pages (url, domain, title, h1, h2, description, content, language, region, status_code, content_hash, url_fingerprint, published_at, crawled_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?, ?)
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
			schema_rating    = excluded.schema_rating
	`

	var publishedAt any
	if !page.PublishedAt.IsZero() {
		publishedAt = page.PublishedAt
	}
	var schemaRating any
	if page.SchemaRating > 0 {
		schemaRating = page.SchemaRating
	}

	_, err := s.writeDB.ExecContext(ctx, query,
		page.URL, page.Domain, page.Title, page.H1, page.H2, page.Description,
		page.Content, page.Language, page.Region,
		page.StatusCode, page.ContentHash, page.URLFingerprint,
		publishedAt, page.CrawledAt,
		page.SchemaType, page.SchemaTitle, page.SchemaDescription, page.SchemaImage,
		page.SchemaAuthor, page.SchemaKeywords, schemaRating,
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

	err := s.readDB.QueryRowContext(ctx, query, arg).Scan(
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
	rows, err := s.readDB.QueryContext(ctx, "SELECT url, url_fingerprint FROM pages")
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

// SearchPages performs hybrid search optimized for navigational queries and
// typo tolerance. It tries increasingly permissive retrieval modes and then
// re-ranks candidates in Go using title/domain phrase quality, domain-root
// homepage boosts, token coverage, typo similarity, freshness, and language.
func (s *SQLiteStorage) SearchPages(ctx context.Context, query string, limit, offset int, lang string, domain string, schemaType string) ([]SearchResult, int, error) {
	q := newSearchQuery(query)
	if q.normalized == "" {
		return nil, 0, nil
	}

	searchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	q.idfMap, _ = s.getSearchIDFMap(searchCtx, q.tokens)

	candidateLimit := searchCandidateLimit(limit, offset)

	var (
		candidates   []searchCandidate
		total        int
		selectedMode searchMode
	)

	// --- FTS modes: try phrase → proximity → exact → majority (N-1) → prefix → relaxed ---
	plans := []struct {
		mode     searchMode
		ftsQuery string
	}{
		{mode: searchModeFTSPhrase, ftsQuery: buildFTSPhraseQuery(q.tokens)},
		{mode: searchModeFTSProximity, ftsQuery: buildFTSProximityQuery(q.tokens)},
		{mode: searchModeFTSExact, ftsQuery: buildFTSExactQuery(q.tokens)},
		{mode: searchModeFTSMajority, ftsQuery: buildFTSMajorityQuery(q.tokens)},
		{mode: searchModeFTSPrefix, ftsQuery: buildFTSPrefixQuery(q.tokens)},
		{mode: searchModeFTSRelaxed, ftsQuery: buildFTSRelaxedQuery(q.tokens)},
	}

	for _, plan := range plans {
		if searchCtx.Err() != nil {
			break
		}
		if plan.ftsQuery == "" {
			continue
		}

		count, err := s.countFTSCandidates(searchCtx, plan.ftsQuery)
		if err != nil {
			if searchCtx.Err() != nil {
				break
			}
			return nil, 0, fmt.Errorf("counting %s results for %q: %w", plan.mode, query, err)
		}
		if count == 0 {
			continue
		}

		results, err := s.searchFTSCandidates(searchCtx, plan.mode, plan.ftsQuery, q, lang, candidateLimit)
		if err != nil {
			if searchCtx.Err() != nil {
				break
			}
			return nil, 0, fmt.Errorf("searching %s candidates for %q: %w", plan.mode, query, err)
		}
		if len(results) == 0 {
			continue
		}

		if selectedMode == "" {
			selectedMode = plan.mode
		}
		candidates = mergeSearchCandidates(candidates, results)
		total += count

		// If we've collected a decent number of solid candidates, stop evaluating relaxed modes.
		if len(candidates) >= candidateLimit/2 {
			break
		}
	}

	// --- Fallback to fuzzy LIKE-based search if FTS found nothing ---
	// Only apply fuzzy search if the query is a single word to avoid polluting multi-word searches.
	if len(candidates) == 0 && searchCtx.Err() == nil && len(q.tokens) == 1 {
		results, fuzzyTotal, err := s.searchFuzzyCandidates(searchCtx, q, lang, candidateLimit)
		if err != nil {
			if searchCtx.Err() != nil {
				return nil, 0, fmt.Errorf("search timed out for %q", query)
			}
			return nil, 0, fmt.Errorf("searching fuzzy candidates for %q: %w", query, err)
		}
		if len(results) == 0 {
			return nil, 0, nil
		}

		candidates = results
		total = fuzzyTotal
		selectedMode = searchModeFuzzy
	}

	// --- Navigational boost when FTS/fuzzy gave few results ---
	if q.navigational && len(candidates) < 30 && searchCtx.Err() == nil {
		navigationCandidates, err := s.searchNavigationalCandidates(searchCtx, q, lang, min(candidateLimit, 60))
		if err == nil {
			candidates = mergeSearchCandidates(candidates, navigationCandidates)
		}
	}

	reranked := rerankSearchCandidates(candidates, q, lang)
	if len(reranked) == 0 {
		return nil, 0, nil
	}

	// --- Domain diversity: interleave results so one domain doesn't
	//     dominate the first page. First 3 positions = max 1 per domain,
	//     positions 4-10 = max 2, positions 11+ = max 3.
	reranked = diversifyByDomain(reranked)

	// --- Optional domain filter ---
	if domain != "" {
		filtered := make([]searchCandidate, 0, len(reranked))
		for _, c := range reranked {
			if strings.EqualFold(c.Domain, domain) {
				filtered = append(filtered, c)
			}
		}
		reranked = filtered
	}

	// --- Optional schema type filter ---
	if schemaType != "" {
		filtered := make([]searchCandidate, 0, len(reranked))
		for _, c := range reranked {
			if strings.EqualFold(c.SchemaType, schemaType) {
				filtered = append(filtered, c)
			}
		}
		reranked = filtered
	}

	if selectedMode == searchModeFuzzy || domain != "" || schemaType != "" {
		total = len(reranked)
	} else if total < len(reranked) {
		total = len(reranked)
	}

	if offset >= len(reranked) {
		return []SearchResult{}, total, nil
	}

	end := min(offset+limit, len(reranked))

	// --- Load body previews only for the final result set ---
	if err := s.loadResultBodies(searchCtx, reranked[offset:end]); err != nil {
		s.logger.Warn("failed to load result bodies", "error", err)
	}

	results := make([]SearchResult, 0, end-offset)
	for _, candidate := range reranked[offset:end] {
		results = append(results, candidate.SearchResult)
	}

	return results, total, nil
}

// loadResultBodies fills in BodyPreview and Snippet for the final result set
// by fetching content from the database. This avoids loading 4KB of content
// per candidate during the initial search query.
func (s *SQLiteStorage) loadResultBodies(ctx context.Context, candidates []searchCandidate) error {
	if len(candidates) == 0 {
		return nil
	}

	ids := make([]string, len(candidates))
	args := make([]any, len(candidates))
	for i, c := range candidates {
		ids[i] = "?"
		args[i] = c.ID
	}

	query := fmt.Sprintf(`
		SELECT id, SUBSTR(content, 1, 4000) FROM pages WHERE id IN (%s)
	`, strings.Join(ids, ","))

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("loading result bodies: %w", err)
	}
	defer rows.Close()

	bodyMap := make(map[int64]string, len(candidates))
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			continue
		}
		bodyMap[id] = body
	}

	for i := range candidates {
		if body, ok := bodyMap[candidates[i].ID]; ok {
			candidates[i].BodyPreview = body
			if candidates[i].Snippet == "" {
				if candidates[i].Description != "" {
					candidates[i].Snippet = candidates[i].Description
				} else if len(body) > 240 {
					candidates[i].Snippet = body[:240]
				} else {
					candidates[i].Snippet = body
				}
			}
		}
	}
	return nil
}

func newSearchQuery(query string) searchQuery {
	normalized := normalizeSearchText(query)
	tokens := strings.Fields(normalized)
	domainLike := normalizeDomainLikeQuery(query)
	navTerm := ""
	if domainLike != "" {
		navTerm = strings.ReplaceAll(normalizeSearchText(classifySearchDomain(domainLike).rootLabel), " ", "")
	}
	if navTerm == "" && len(tokens) == 1 {
		navTerm = tokens[0]
	}

	return searchQuery{
		raw:          query,
		lowered:      strings.ToLower(strings.TrimSpace(query)),
		normalized:   normalized,
		compact:      strings.ReplaceAll(normalized, " ", ""),
		tokens:       tokens,
		fragments:    buildSearchFragments(tokens),
		navTerm:      navTerm,
		domainLike:   domainLike,
		navigational: navTerm != "" || domainLike != "",
	}
}

func searchCandidateLimit(limit, offset int) int {
	want := limit*8 + 30
	if want > 200 {
		want = 200
	}
	return want
}

func (s *SQLiteStorage) getSearchIDFMap(ctx context.Context, tokens []string) (map[string]float64, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	var totalDocs float64
	if err := s.readDB.QueryRowContext(ctx, `SELECT MAX(rowid) FROM pages_fts`).Scan(&totalDocs); err != nil {
		totalDocs = 10000.0
	}
	if totalDocs <= 0 {
		totalDocs = 1.0
	}

	idfMap := make(map[string]float64, len(tokens))
	defaultIDF := math.Log(totalDocs / 1.0)
	for _, t := range tokens {
		idfMap[t] = defaultIDF
	}

	placeholders := make([]string, len(tokens))
	args := make([]any, len(tokens))
	for i, t := range tokens {
		placeholders[i] = "?"
		args[i] = t
	}

	query := fmt.Sprintf(`SELECT term, doc FROM pages_fts_vocab WHERE term IN (%s)`, strings.Join(placeholders, ","))
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		s.logger.Warn("failed to fetch IDF map", "error", err)
		return idfMap, nil
	}
	defer rows.Close()

	for rows.Next() {
		var term string
		var docCount float64
		if err := rows.Scan(&term, &docCount); err == nil && docCount > 0 {
			idfMap[term] = math.Log(totalDocs / docCount)
		}
	}

	return idfMap, nil
}

func (s *SQLiteStorage) countFTSCandidates(ctx context.Context, ftsQuery string) (int, error) {
	var total int
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_fts WHERE pages_fts MATCH ?`, ftsQuery).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *SQLiteStorage) searchFTSCandidates(ctx context.Context, mode searchMode, matchQuery string, query searchQuery, lang string, limit int) ([]searchCandidate, error) {
	// Two-phase scoring:
	//   SQL: cheap boosts on short columns (title, domain) + FTS rank for initial
	//        pre-filter ordering. Avoids LIKE on content/h1/h2/description (expensive).
	//   Go:  full phrase/token/coverage scoring in scoreSearchCandidate.
	domainExact := query.domainLike
	if domainExact == "" {
		domainExact = query.navTerm
	}

	titleExact := query.lowered
	titlePrefix := query.lowered + "%"
	titleContains := "%" + query.lowered + "%"

	scoreExpr := "rank * 100.0" +
		" + CASE WHEN LOWER(p.title) = ? THEN 500.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.title) LIKE ? THEN 300.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.title) LIKE ? THEN 180.0 ELSE 0 END" +
		" + CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 800.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.url) LIKE ? THEN 100.0 ELSE 0 END" +
		" + CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 1 THEN 350.0 ELSE 0 END" +
		" + CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) BETWEEN 2 AND 3 THEN 150.0 ELSE 0 END" +
		" + CASE WHEN COALESCE(d.is_seed, 0) = 1 THEN 40.0 ELSE 0 END" +
		" + MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))"

	urlContains := "%" + query.lowered + "%"
	args := []any{titleExact, titlePrefix, titleContains, domainExact, domainExact, urlContains}

	for _, token := range query.tokens {
		if len(token) > 2 {
			scoreExpr += " + CASE WHEN LOWER(p.title) LIKE ? THEN 40.0 ELSE 0 END"
			args = append(args, "%"+token+"%")
			scoreExpr += " + CASE WHEN LOWER(p.h1) LIKE ? THEN 25.0 ELSE 0 END"
			args = append(args, "%"+token+"%")
		}
	}

	if lang != "" {
		scoreExpr += " + CASE WHEN p.language = ? THEN 35.0 ELSE 0 END"
		args = append(args, lang)
	}

	searchSQL := fmt.Sprintf(`
		WITH base_matches AS (
			SELECT
				p.id,
				(%s) AS sql_score,
				COALESCE(sc.clicks, 0) AS clicks,
				p.crawled_at
			FROM pages_fts
			JOIN pages p ON p.id = pages_fts.rowid
			LEFT JOIN domains d ON d.domain = p.domain
			LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
			WHERE pages_fts MATCH ?
		)
		SELECT
			p.id,
			p.url,
			p.title,
			p.h1,
			p.h2,
			p.description,
			snippet(pages_fts, 2, '<mark>', '</mark>', '...', 32) AS snippet,
			p.domain,
			p.language,
			p.region,
			p.crawled_at,
			p.published_at,
			p.updated_at,
			m.sql_score,
			COALESCE(d.is_seed, 0) AS is_seed,
			LENGTH(p.content) AS content_len,
			p.schema_type,
			p.schema_image,
			p.schema_author,
			p.schema_keywords,
			p.schema_rating,
			p.referring_domains,
			COALESCE(p.pagerank, 1.0) AS pagerank,
			m.clicks
		FROM (
			SELECT * FROM base_matches
			ORDER BY (sql_score + clicks * 150.0) DESC, crawled_at DESC
			LIMIT ?
		) m
		JOIN pages p ON p.id = m.id
		JOIN pages_fts ON pages_fts.rowid = m.id
		LEFT JOIN domains d ON d.domain = p.domain
		WHERE pages_fts MATCH ?
		ORDER BY (m.sql_score + m.clicks * 150.0) DESC, m.crawled_at DESC
	`, scoreExpr)
	args = append(args, query.raw, matchQuery, limit, matchQuery)

	rows, err := s.readDB.QueryContext(ctx, searchSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []searchCandidate
	for rows.Next() {
		var (
			candidate      searchCandidate
			crawledAt      sql.NullTime
			publishedAtStr sql.NullString
			updatedAtStr   sql.NullString
			seedFlag       int
			snippetText    sql.NullString
			clicksCount    int
		)
		var schemaRating sql.NullFloat64
		if err := rows.Scan(
			&candidate.ID,
			&candidate.URL,
			&candidate.Title,
			&candidate.H1,
			&candidate.H2,
			&candidate.Description,
			&snippetText,
			&candidate.Domain,
			&candidate.Language,
			&candidate.Region,
			&crawledAt,
			&publishedAtStr,
			&updatedAtStr,
			&candidate.sqlScore,
			&seedFlag,
			&candidate.contentLen,
			&candidate.SchemaType,
			&candidate.SchemaImage,
			&candidate.SchemaAuthor,
			&candidate.SchemaKeywords,
			&schemaRating,
			&candidate.ReferringDomains,
			&candidate.PageRank,
			&clicksCount,
		); err != nil {
			return nil, fmt.Errorf("scanning FTS candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 150.0

		candidate.Snippet = snippetText.String
		if candidate.Snippet == "" {
			candidate.Snippet = candidate.Description
		}
		candidate.mode = mode
		candidate.isSeed = seedFlag == 1
		if crawledAt.Valid {
			candidate.CrawledAt = crawledAt.Time
		}
		if publishedAtStr.Valid && publishedAtStr.String != "" {
			t := parseFlexibleTime(publishedAtStr.String)
			if !t.IsZero() {
				candidate.publishedAtTime = t
				candidate.PublishedAt = &t
			}
		}
		if updatedAtStr.Valid && updatedAtStr.String != "" {
			t := parseFlexibleTime(updatedAtStr.String)
			if !t.IsZero() {
				candidate.UpdatedAt = &t
			}
		}
		if schemaRating.Valid {
			candidate.SchemaRating = schemaRating.Float64
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating FTS candidates: %w", err)
	}

	return candidates, nil
}

func (s *SQLiteStorage) searchFuzzyCandidates(ctx context.Context, query searchQuery, lang string, limit int) ([]searchCandidate, int, error) {
	if len(query.fragments) == 0 {
		return nil, 0, nil
	}

	var whereParts []string
	var whereArgs []any
	var scoreParts []string
	var scoreArgs []any

	for _, fragment := range query.fragments {
		like := "%" + fragment + "%"
		whereParts = append(whereParts, "(LOWER(p.title) LIKE ? OR LOWER(p.h1) LIKE ? OR LOWER(p.h2) LIKE ? OR LOWER(p.domain) LIKE ? OR LOWER(p.url) LIKE ?)")
		whereArgs = append(whereArgs, like, like, like, like, like)
		scoreParts = append(scoreParts,
			"CASE WHEN LOWER(p.title) LIKE ? THEN 12.0 ELSE 0 END",
			"CASE WHEN LOWER(p.h1) LIKE ? THEN 8.0 ELSE 0 END",
			"CASE WHEN LOWER(p.h2) LIKE ? THEN 8.0 ELSE 0 END",
			"CASE WHEN LOWER(p.domain) LIKE ? THEN 16.0 ELSE 0 END",
			"CASE WHEN LOWER(p.url) LIKE ? THEN 6.0 ELSE 0 END",
		)
		scoreArgs = append(scoreArgs, like, like, like, like, like)
	}

	if query.navTerm != "" {
		navLike := "%" + query.navTerm + "%"
		whereParts = append(whereParts, "(LOWER(p.title) LIKE ? OR LOWER(p.h1) LIKE ? OR LOWER(p.h2) LIKE ? OR LOWER(p.domain) LIKE ? OR LOWER(p.url) LIKE ?)")
		whereArgs = append(whereArgs, navLike, navLike, navLike, navLike, navLike)
		scoreParts = append(scoreParts,
			"CASE WHEN LOWER(p.title) LIKE ? THEN 20.0 ELSE 0 END",
			"CASE WHEN LOWER(p.h1) LIKE ? THEN 16.0 ELSE 0 END",
			"CASE WHEN LOWER(p.h2) LIKE ? THEN 16.0 ELSE 0 END",
			"CASE WHEN LOWER(p.domain) LIKE ? THEN 40.0 ELSE 0 END",
			"CASE WHEN LOWER(p.url) LIKE ? THEN 12.0 ELSE 0 END",
		)
		scoreArgs = append(scoreArgs, navLike, navLike, navLike, navLike, navLike)
	}

	if len(whereParts) == 0 {
		return nil, 0, nil
	}

	whereClause := strings.Join(whereParts, " OR ")

	querySQL := fmt.Sprintf(`
		SELECT
			p.id,
			p.url,
			p.title,
			p.h1,
			p.h2,
			p.description,
			CASE
				WHEN p.description != '' THEN SUBSTR(p.description, 1, 240)
				ELSE ''
			END AS snippet,
			p.domain,
			p.language,
			p.region,
			p.crawled_at,
			p.published_at,
			p.updated_at,
			(%s
				+ CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 3 THEN 50.0 ELSE 0 END
				+ CASE WHEN COALESCE(d.is_seed, 0) = 1 THEN 20.0 ELSE 0 END
				+ MAX(0.0, 15.0 - 0.15 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))
				%s
			) AS sql_score,
			COALESCE(d.is_seed, 0) AS is_seed,
			LENGTH(p.content) AS content_len,
			COUNT(*) OVER() AS total_count,
			p.schema_type,
			p.schema_image,
			p.schema_author,
			p.schema_keywords,
			p.schema_rating,
			p.referring_domains,
			COALESCE(p.pagerank, 1.0) AS pagerank,
			COALESCE(sc.clicks, 0) AS clicks
		FROM pages p
		LEFT JOIN domains d ON d.domain = p.domain
		LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
		WHERE %s
		ORDER BY (sql_score + COALESCE(sc.clicks, 0) * 150.0) DESC, p.crawled_at DESC
		LIMIT ?
	`, strings.Join(scoreParts, " + "), fuzzyLanguageSQL(lang), whereClause)

	args := append([]any{}, scoreArgs...)
	if lang != "" {
		args = append(args, lang)
	}
	args = append(args, query.raw)
	args = append(args, whereArgs...)
	args = append(args, limit)

	rows, err := s.readDB.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying fuzzy candidates: %w", err)
	}
	defer rows.Close()

	var candidates []searchCandidate
	var totalCount int
	for rows.Next() {
		var (
			candidate      searchCandidate
			crawledAt      sql.NullTime
			publishedAtStr sql.NullString
			updatedAtStr   sql.NullString
			seedFlag       int
			snippetText    sql.NullString
			clicksCount    int
		)
		var schemaRating sql.NullFloat64
		if err := rows.Scan(
			&candidate.ID,
			&candidate.URL,
			&candidate.Title,
			&candidate.H1,
			&candidate.H2,
			&candidate.Description,
			&snippetText,
			&candidate.Domain,
			&candidate.Language,
			&candidate.Region,
			&crawledAt,
			&publishedAtStr,
			&updatedAtStr,
			&candidate.sqlScore,
			&seedFlag,
			&candidate.contentLen,
			&totalCount,
			&candidate.SchemaType,
			&candidate.SchemaImage,
			&candidate.SchemaAuthor,
			&candidate.SchemaKeywords,
			&schemaRating,
			&candidate.ReferringDomains,
			&candidate.PageRank,
			&clicksCount,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning fuzzy candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 150.0

		candidate.Snippet = snippetText.String
		candidate.mode = searchModeFuzzy
		candidate.isSeed = seedFlag == 1
		if crawledAt.Valid {
			candidate.CrawledAt = crawledAt.Time
		}
		if publishedAtStr.Valid && publishedAtStr.String != "" {
			t := parseFlexibleTime(publishedAtStr.String)
			if !t.IsZero() {
				candidate.publishedAtTime = t
				candidate.PublishedAt = &t
			}
		}
		if updatedAtStr.Valid && updatedAtStr.String != "" {
			t := parseFlexibleTime(updatedAtStr.String)
			if !t.IsZero() {
				candidate.UpdatedAt = &t
			}
		}
		if schemaRating.Valid {
			candidate.SchemaRating = schemaRating.Float64
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating fuzzy candidates: %w", err)
	}

	return candidates, totalCount, nil
}

func (s *SQLiteStorage) searchNavigationalCandidates(ctx context.Context, query searchQuery, lang string, limit int) ([]searchCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}

	navTerm := query.navTerm
	if navTerm == "" && query.domainLike != "" {
		navTerm = strings.ReplaceAll(normalizeSearchText(classifySearchDomain(query.domainLike).rootLabel), " ", "")
	}
	if navTerm == "" && query.domainLike == "" {
		return nil, nil
	}

	domainExact := query.domainLike
	if domainExact == "" {
		domainExact = navTerm
	}
	navLike := "%" + navTerm + "%"
	if navTerm == "" {
		navLike = "%" + query.lowered + "%"
	}
	titleLike := "%" + query.lowered + "%"
	if query.lowered == "" {
		titleLike = navLike
	}

	querySQL := `
		SELECT
			p.id,
			p.url,
			p.title,
			p.h1,
			p.h2,
			p.description,
			CASE
				WHEN p.description != '' THEN SUBSTR(p.description, 1, 240)
				ELSE ''
			END AS snippet,
			p.domain,
			p.language,
			p.region,
			p.crawled_at,
			p.published_at,
			p.updated_at,
			(
				CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 900.0 ELSE 0 END
				+ CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 650.0 ELSE 0 END
				+ CASE WHEN LOWER(p.url) LIKE ? THEN 220.0 ELSE 0 END
				+ CASE WHEN LOWER(p.title) LIKE ? THEN 160.0 ELSE 0 END
				+ CASE WHEN LOWER(p.h1) LIKE ? THEN 120.0 ELSE 0 END
				+ CASE WHEN LOWER(p.h2) LIKE ? THEN 100.0 ELSE 0 END
				+ CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 3 THEN 220.0 ELSE 0 END
				+ CASE WHEN COALESCE(d.is_seed, 0) = 1 THEN 20.0 ELSE 0 END
				+ MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))
				+ CASE WHEN ? != '' AND p.language = ? THEN 20.0 ELSE 0 END
			) AS sql_score,
			COALESCE(d.is_seed, 0) AS is_seed,
			LENGTH(p.content) AS content_len,
			p.schema_type,
			p.schema_image,
			p.schema_author,
			p.schema_keywords,
			p.schema_rating,
			p.referring_domains,
			COALESCE(p.pagerank, 1.0) AS pagerank,
			COALESCE(sc.clicks, 0) AS clicks
		FROM pages p
		LEFT JOIN domains d ON d.domain = p.domain
		LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
		WHERE
			(? != '' AND LOWER(p.domain) = ?)
			OR (? != '' AND LOWER(p.domain) LIKE ?)
			OR LOWER(p.url) LIKE ?
			OR LOWER(p.title) LIKE ?
			OR LOWER(p.h1) LIKE ?
			OR LOWER(p.h2) LIKE ?
		ORDER BY (sql_score + COALESCE(sc.clicks, 0) * 150.0) DESC, p.crawled_at DESC
		LIMIT ?
	`

	args := []any{
		domainExact,
		domainExact,
		navTerm,
		navLike,
		navLike,
		titleLike,
		titleLike,
		titleLike,
		lang,
		lang,
		query.raw,
		domainExact,
		domainExact,
		navTerm,
		navLike,
		navLike,
		titleLike,
		titleLike,
		titleLike,
		limit,
	}

	rows, err := s.readDB.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("querying navigational candidates: %w", err)
	}
	defer rows.Close()

	var candidates []searchCandidate
	for rows.Next() {
		var (
			candidate      searchCandidate
			crawledAt      sql.NullTime
			publishedAtStr sql.NullString
			updatedAtStr   sql.NullString
			seedFlag       int
			snippetText    sql.NullString
			clicksCount    int
		)
		var schemaRating sql.NullFloat64
		if err := rows.Scan(
			&candidate.ID,
			&candidate.URL,
			&candidate.Title,
			&candidate.H1,
			&candidate.H2,
			&candidate.Description,
			&snippetText,
			&candidate.Domain,
			&candidate.Language,
			&candidate.Region,
			&crawledAt,
			&publishedAtStr,
			&updatedAtStr,
			&candidate.sqlScore,
			&seedFlag,
			&candidate.contentLen,
			&candidate.SchemaType,
			&candidate.SchemaImage,
			&candidate.SchemaAuthor,
			&candidate.SchemaKeywords,
			&schemaRating,
			&candidate.ReferringDomains,
			&candidate.PageRank,
			&clicksCount,
		); err != nil {
			return nil, fmt.Errorf("scanning navigational candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 150.0

		candidate.Snippet = snippetText.String
		candidate.mode = searchModeNavigational
		candidate.isSeed = seedFlag == 1
		if crawledAt.Valid {
			candidate.CrawledAt = crawledAt.Time
		}
		if publishedAtStr.Valid && publishedAtStr.String != "" {
			t := parseFlexibleTime(publishedAtStr.String)
			if !t.IsZero() {
				candidate.publishedAtTime = t
				candidate.PublishedAt = &t
			}
		}
		if updatedAtStr.Valid && updatedAtStr.String != "" {
			t := parseFlexibleTime(updatedAtStr.String)
			if !t.IsZero() {
				candidate.UpdatedAt = &t
			}
		}
		if schemaRating.Valid {
			candidate.SchemaRating = schemaRating.Float64
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating navigational candidates: %w", err)
	}

	return candidates, nil
}

func fuzzyLanguageSQL(lang string) string {
	if lang == "" {
		return ""
	}
	return " + CASE WHEN p.language = ? THEN 20.0 ELSE 0 END"
}

func rerankSearchCandidates(candidates []searchCandidate, query searchQuery, lang string) []searchCandidate {
	reranked := make([]searchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		score, keep := scoreSearchCandidate(candidate, query, lang)
		if !keep {
			continue
		}
		candidate.Rank = score
		reranked = append(reranked, candidate)
	}

	sort.SliceStable(reranked, func(i, j int) bool {
		if reranked[i].Rank == reranked[j].Rank {
			iHomepage := isSearchHomepage(reranked[i].URL)
			jHomepage := isSearchHomepage(reranked[j].URL)
			if iHomepage != jHomepage {
				return iHomepage
			}
			if !reranked[i].CrawledAt.Equal(reranked[j].CrawledAt) {
				return reranked[i].CrawledAt.After(reranked[j].CrawledAt)
			}
			return reranked[i].URL < reranked[j].URL
		}
		return reranked[i].Rank > reranked[j].Rank
	})

	return reranked
}

func scoreSearchCandidate(candidate searchCandidate, query searchQuery, lang string) (float64, bool) {
	titleNorm := normalizeSearchText(candidate.Title)
	h1Norm := normalizeSearchText(candidate.H1)
	h2Norm := normalizeSearchText(candidate.H2)
	bodyNorm := normalizeSearchText(candidate.BodyPreview)
	if bodyNorm == "" {
		bodyNorm = normalizeSearchText(candidate.Description)
	}
	urlNorm := normalizeSearchText(candidate.URL)
	domainNorm := normalizeSearchText(candidate.Domain)
	urlDomain := candidate.Domain
	if parsedURL, err := url.Parse(candidate.URL); err == nil && parsedURL.Hostname() != "" {
		urlDomain = parsedURL.Hostname()
	}
	urlDomain = strings.TrimPrefix(urlDomain, "www.")
	domainInfo := classifySearchDomain(urlDomain)
	effectiveDomainNorm := normalizeSearchText(domainInfo.effectiveDomain)
	rootLabelNorm := normalizeSearchText(domainInfo.rootLabel)

	titleTokens := uniqueStrings(strings.Fields(titleNorm))
	h1Tokens := uniqueStrings(strings.Fields(h1Norm))
	h2Tokens := uniqueStrings(strings.Fields(h2Norm))
	bodyTokens := uniqueStrings(strings.Fields(bodyNorm))
	urlTokens := uniqueStrings(strings.Fields(urlNorm))
	domainTokens := uniqueStrings(append(strings.Fields(domainNorm), strings.Fields(effectiveDomainNorm)...))
	domainTokens = uniqueStrings(append(domainTokens, strings.Fields(rootLabelNorm)...))

	titlePhrase := bestFieldPhraseScore(query.normalized, query.tokens, titleNorm, titleTokens)
	h1Phrase := bestFieldPhraseScore(query.normalized, query.tokens, h1Norm, h1Tokens)
	h2Phrase := bestFieldPhraseScore(query.normalized, query.tokens, h2Norm, h2Tokens)
	bodyPhrase := bestFieldPhraseScore(query.normalized, query.tokens, bodyNorm, bodyTokens)
	urlPhrase := bestFieldPhraseScore(query.normalized, query.tokens, urlNorm, urlTokens)
	domainPhrase := max(
		bestFieldPhraseScore(query.normalized, query.tokens, domainNorm, domainTokens),
		bestFieldPhraseScore(query.normalized, query.tokens, effectiveDomainNorm, strings.Fields(effectiveDomainNorm)),
		bestFieldPhraseScore(query.normalized, query.tokens, rootLabelNorm, strings.Fields(rootLabelNorm)),
	)

	titleAvg, titleCoverage, titleExact := searchTokenCoverage(query.tokens, titleTokens)
	h1Avg, h1Coverage, _ := searchTokenCoverage(query.tokens, h1Tokens)
	h2Avg, h2Coverage, _ := searchTokenCoverage(query.tokens, h2Tokens)
	bodyAvg, bodyCoverage, _ := searchTokenCoverage(query.tokens, bodyTokens)
	domainAvg, domainCoverage, _ := searchTokenCoverage(query.tokens, domainTokens)
	urlAvg, urlCoverage, _ := searchTokenCoverage(query.tokens, urlTokens)

	weightedFieldAvg := (3.0*titleAvg + 2.0*h1Avg + 2.0*h2Avg + bodyAvg) / 8.0
	weightedFieldCoverage := (3.0*titleCoverage + 2.0*h1Coverage + 2.0*h2Coverage + bodyCoverage) / 8.0
	fixedTF := fixedWeightedTermFrequency(query.tokens, query.idfMap, titleNorm, h1Norm, h2Norm, bodyNorm)

	isHomepage := isSearchHomepage(candidate.URL)
	domainPhraseWeight := 260.0
	domainTokenWeight := 200.0
	domainCoverageWeight := 60.0
	rootHomepageBonus := 180.0
	subdomainHomepageBonus := 60.0
	if query.navigational {
		domainPhraseWeight = 820.0
		domainTokenWeight = 560.0
		domainCoverageWeight = 180.0
		rootHomepageBonus = 950.0
		subdomainHomepageBonus = 260.0
	}

	// Schema keywords boost (lower weight to avoid noise).
	var schemaKeywordsAvg, schemaKeywordsCoverage float64
	if candidate.SchemaKeywords != "" {
		schemaNorm := normalizeSearchText(candidate.SchemaKeywords)
		schemaTokens := uniqueStrings(strings.Fields(schemaNorm))
		schemaKeywordsAvg, schemaKeywordsCoverage, _ = searchTokenCoverage(query.tokens, schemaTokens)
	}

	score := candidate.sqlScore * 0.35
	score += searchModeBonus(candidate.mode)
	score += 560.0*titlePhrase + 360.0*h1Phrase + 360.0*h2Phrase + 170.0*bodyPhrase
	score += domainPhraseWeight * domainPhrase
	score += 90.0 * urlPhrase
	score += 430.0*weightedFieldAvg + 180.0*weightedFieldCoverage
	score += 55.0 * fixedTF
	score += domainTokenWeight*domainAvg + domainCoverageWeight*domainCoverage
	score += 130.0*urlAvg + 40.0*urlCoverage
	score += 25.0*schemaKeywordsAvg + 15.0*schemaKeywordsCoverage

	// PageRank logarithmic boost
	// math.Log1p(x) = ln(1 + x). 
	// Multiply by a weight that makes a highly authoritative page rank above a similar content page.
	if candidate.PageRank > 0 {
		score += math.Log1p(candidate.PageRank) * 200.0
	} else if candidate.ReferringDomains > 0 {
		score += math.Log1p(float64(candidate.ReferringDomains)) * 140.0
	}

	if len(query.tokens) > 0 && titleExact == len(query.tokens) {
		score += 180.0
	}
	if domainInfo.isRootDomain && isHomepage && domainPhrase >= 0.85 {
		score += rootHomepageBonus
	} else if isHomepage && domainPhrase >= 0.80 {
		score += subdomainHomepageBonus
	}
	if !domainInfo.isRootDomain && domainPhrase >= 0.90 {
		score -= 160.0
	}
	if lang != "" && candidate.Language == lang {
		score += 60.0
	}
	if candidate.isSeed {
		score += 35.0
	}
	score += searchFreshnessScore(candidate.CrawledAt, candidate.publishedAtTime)
	score += searchContentLengthScore(candidate.contentLen)

	// --- Direct domain match boost for navigational queries ---
	// Boosts pages whose domain exactly matches the search term.
	// Only boosts exact domain=term matches (e.g. "reddit" → reddit.com),
	// never domains that merely contain the term (e.g. redditstatus.com).
	if query.domainLike != "" || query.navTerm != "" {
		candidateDomain := strings.TrimPrefix(candidate.Domain, "www.")
		matchDomain := query.domainLike
		if matchDomain == "" {
			matchDomain = query.navTerm
		}
		if candidateDomain == matchDomain {
			// e.g. query="reddit.com" → reddit.com, or query="reddit" → reddit (root domain only)
			score += 5000.0
			if isHomepage {
				score += 3000.0
			}
		} else if candidateDomain == matchDomain+".com" ||
			candidateDomain == matchDomain+".net" ||
			candidateDomain == matchDomain+".org" ||
			candidateDomain == matchDomain+".ai" ||
			candidateDomain == matchDomain+".gg" ||
			candidateDomain == matchDomain+".dev" ||
			candidateDomain == matchDomain+".io" ||
			candidateDomain == matchDomain+".es" ||
			candidateDomain == matchDomain+".uk" ||
			candidateDomain == matchDomain+".de" ||
			candidateDomain == matchDomain+".fr" ||
			candidateDomain == matchDomain+".it" ||
			candidateDomain == matchDomain+".pt" ||
			candidateDomain == matchDomain+".mx" ||
			candidateDomain == matchDomain+".ar" ||
			candidateDomain == matchDomain+".br" ||
			candidateDomain == matchDomain+".uy" {
			// e.g. query="reddit" → reddit.com, reddit.org, reddit.es, etc.
			score += 4000.0
			if isHomepage {
				score += 2500.0
			}
		}
	}

	if query.navigational && domainInfo.isRootDomain && isHomepage && domainPhrase >= 0.95 {
		score += 1200.0
	}
	if query.navigational && !domainInfo.isRootDomain {
		score -= 300.0
	}

	if candidate.mode == searchModeFuzzy {
		strongestPhrase := max(max(titlePhrase, h1Phrase), max(max(h2Phrase, bodyPhrase), domainPhrase))
		strongestToken := max(weightedFieldAvg, max(domainAvg, urlAvg))
		if strongestPhrase < 0.72 && strongestToken < 0.78 {
			return 0, false
		}
	}

	return score, true
}

// diversifyByDomain reorders search results so that one domain doesn't dominate
// the first page. It preserves score order within each domain but interleaves
// different domains to produce a Google-like result layout:
//
//   - Positions 0-2:  max 1 result per domain
//   - Positions 3-9:  max 2 results per domain
//   - Position 10+:   max 3 results per domain
//
// Deferred results (that exceeded their domain's limit) are re-inserted in
// subsequent passes as the per-domain quota increases at deeper positions.
func diversifyByDomain(candidates []searchCandidate) []searchCandidate {
	if len(candidates) <= 3 {
		return candidates
	}

	var result []searchCandidate
	domainCount := make(map[string]int)

	// First pass: place results respecting per-domain limits.
	var deferred []searchCandidate
	for _, c := range candidates {
		maxForDomain := maxPerDomainAtPosition(len(result))
		if domainCount[c.Domain] < maxForDomain {
			result = append(result, c)
			domainCount[c.Domain]++
		} else {
			deferred = append(deferred, c)
		}
	}

	// Second pass: try to place deferred results now that positions have
	// shifted and per-domain quotas have increased.
	for len(deferred) > 0 {
		placed := false
		var remaining []searchCandidate
		for _, c := range deferred {
			maxForDomain := maxPerDomainAtPosition(len(result))
			if domainCount[c.Domain] < maxForDomain {
				result = append(result, c)
				domainCount[c.Domain]++
				placed = true
			} else {
				remaining = append(remaining, c)
			}
		}
		deferred = remaining
		if !placed {
			// Nothing more can be placed, append the rest.
			result = append(result, deferred...)
			break
		}
	}

	return result
}

// maxPerDomainAtPosition returns the maximum number of results allowed from
// a single domain at the given result position. This enforces domain diversity
// in search results.
func maxPerDomainAtPosition(pos int) int {
	switch {
	case pos < 3:
		return 1
	case pos < 10:
		return 2
	default:
		return 3
	}
}

func searchModeBonus(mode searchMode) float64 {
	switch mode {
	case searchModeNavigational:
		return 120.0
	case searchModeFTSPhrase:
		return 180.0
	case searchModeFTSProximity:
		return 160.0
	case searchModeFTSExact:
		return 140.0
	case searchModeFTSMajority:
		return 110.0
	case searchModeFTSPrefix:
		return 90.0
	case searchModeFTSRelaxed:
		return 40.0
	default:
		return 0.0
	}
}

func searchFreshnessScore(crawledAt time.Time, publishedAt time.Time) float64 {
	bestTime := crawledAt
	if !publishedAt.IsZero() {
		bestTime = publishedAt
	}
	if bestTime.IsZero() {
		return 0
	}
	days := time.Since(bestTime).Hours() / 24
	return max(0.0, 35.0-days*0.25)
}

// parseFlexibleTime parses a time string from SQLite which can come in
// multiple formats depending on how it was stored.
func parseFlexibleTime(s string) time.Time {
	for _, fmt := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(fmt, s); err == nil && !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func searchContentLengthScore(contentLen int) float64 {
	if contentLen <= 0 {
		return 0
	}
	return min(float64(contentLen)/800.0, 25.0)
}

func fixedWeightedTermFrequency(queryTokens []string, idfMap map[string]float64, titleNorm, h1Norm, h2Norm, bodyNorm string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}

	queryTokens = uniqueStrings(queryTokens)
	titleFreq := tokenFrequencyMap(strings.Fields(titleNorm))
	h1Freq := tokenFrequencyMap(strings.Fields(h1Norm))
	h2Freq := tokenFrequencyMap(strings.Fields(h2Norm))
	bodyFreq := tokenFrequencyMap(strings.Fields(bodyNorm))

	return weightedTokenFrequency(queryTokens, idfMap, titleFreq, 3.0, 4) +
		weightedTokenFrequency(queryTokens, idfMap, h1Freq, 2.0, 5) +
		weightedTokenFrequency(queryTokens, idfMap, h2Freq, 2.0, 5) +
		weightedTokenFrequency(queryTokens, idfMap, bodyFreq, 1.0, 10)
}

func tokenFrequencyMap(tokens []string) map[string]int {
	freq := make(map[string]int, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		freq[token]++
	}
	return freq
}

func weightedTokenFrequency(queryTokens []string, idfMap map[string]float64, fieldFreq map[string]int, weight float64, maxPerToken int) float64 {
	if len(queryTokens) == 0 || len(fieldFreq) == 0 {
		return 0
	}

	total := 0.0
	for _, token := range queryTokens {
		count := fieldFreq[token]
		if maxPerToken > 0 && count > maxPerToken {
			count = maxPerToken
		}
		idf := 1.0
		if val, ok := idfMap[token]; ok && val > 0 {
			idf = val
		}
		total += float64(count) * weight * idf
	}
	return total
}

func searchTokenCoverage(queryTokens, fieldTokens []string) (avg float64, coverage float64, exactMatches int) {
	if len(queryTokens) == 0 || len(fieldTokens) == 0 {
		return 0, 0, 0
	}

	matched := 0
	total := 0.0
	for _, queryToken := range queryTokens {
		best := 0.0
		for _, fieldToken := range fieldTokens {
			similarity := searchTokenSimilarity(queryToken, fieldToken)
			if similarity > best {
				best = similarity
			}
		}
		total += best
		if best >= 0.72 {
			matched++
		}
		if best == 1.0 {
			exactMatches++
		}
	}

	return total / float64(len(queryTokens)), float64(matched) / float64(len(queryTokens)), exactMatches
}

func searchTokenSimilarity(queryToken, fieldToken string) float64 {
	if queryToken == "" || fieldToken == "" {
		return 0
	}
	if queryToken == fieldToken {
		return 1.0
	}
	if strings.HasPrefix(fieldToken, queryToken) || strings.HasPrefix(queryToken, fieldToken) {
		return 0.94
	}
	if strings.Contains(fieldToken, queryToken) || strings.Contains(queryToken, fieldToken) {
		return 0.88
	}
	if len([]rune(queryToken)) < 4 || len([]rune(fieldToken)) < 4 {
		return 0
	}
	similarity := normalizedEditSimilarity(queryToken, fieldToken)
	if similarity < 0.72 {
		return 0
	}
	return similarity
}

func bestFieldPhraseScore(queryNormalized string, queryTokens []string, fieldNormalized string, fieldTokens []string) float64 {
	if queryNormalized == "" || fieldNormalized == "" {
		return 0
	}
	if fieldNormalized == queryNormalized {
		return 1.0
	}
	if strings.HasPrefix(fieldNormalized, queryNormalized+" ") || strings.HasPrefix(fieldNormalized, queryNormalized) {
		return 0.97
	}
	if strings.Contains(fieldNormalized, queryNormalized) {
		return 0.92
	}
	if len(queryTokens) == 0 || len(fieldTokens) == 0 {
		return 0
	}

	minWindow := max(1, len(queryTokens)-1)
	maxWindow := min(len(fieldTokens), len(queryTokens)+1)
	best := 0.0
	for size := minWindow; size <= maxWindow; size++ {
		for i := 0; i+size <= len(fieldTokens); i++ {
			window := strings.Join(fieldTokens[i:i+size], " ")
			similarity := normalizedEditSimilarity(queryNormalized, window)
			if similarity > best {
				best = similarity
			}
		}
	}
	return best
}

func normalizedEditSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1.0
	}
	distance := damerauLevenshteinDistance(a, b)
	maxLen := max(len([]rune(a)), len([]rune(b)))
	if maxLen == 0 {
		return 1.0
	}
	return max(0.0, 1.0-float64(distance)/float64(maxLen))
}

func damerauLevenshteinDistance(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	rows := len(ar) + 1
	cols := len(br) + 1

	dp := make([][]int, rows)
	for i := range dp {
		dp[i] = make([]int, cols)
	}
	for i := 0; i < rows; i++ {
		dp[i][0] = i
	}
	for j := 0; j < cols; j++ {
		dp[0][j] = j
	}

	for i := 1; i < rows; i++ {
		for j := 1; j < cols; j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}

			dp[i][j] = min(
				dp[i-1][j]+1,
				min(dp[i][j-1]+1, dp[i-1][j-1]+cost),
			)

			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				dp[i][j] = min(dp[i][j], dp[i-2][j-2]+1)
			}
		}
	}

	return dp[len(ar)][len(br)]
}

func normalizeSearchText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	if normalized, _, err := transform.String(searchTextNormalizer, value); err == nil {
		value = normalized
	}

	var b strings.Builder
	b.Grow(len(value))
	lastSpace := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}

	return strings.TrimSpace(b.String())
}

func normalizeDomainLikeQuery(query string) string {
	value := strings.ToLower(strings.TrimSpace(query))
	if value == "" {
		return ""
	}

	if strings.Contains(value, "://") || (strings.Contains(value, ".") && !strings.Contains(value, " ")) {
		if !strings.Contains(value, "://") {
			value = "https://" + value
		}
		if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
			value = parsed.Hostname()
		}
	}

	value = strings.TrimPrefix(value, "www.")
	if !strings.Contains(value, ".") {
		return ""
	}
	return strings.Trim(value, ". /")
}

func classifySearchDomain(domain string) searchDomainInfo {
	domain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, "www.")))
	if domain == "" {
		return searchDomainInfo{}
	}

	effectiveDomain := domain
	if resolved, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		effectiveDomain = resolved
	}

	rootLabel := effectiveDomain
	if dot := strings.Index(rootLabel, "."); dot >= 0 {
		rootLabel = rootLabel[:dot]
	}

	return searchDomainInfo{
		effectiveDomain: effectiveDomain,
		rootLabel:       rootLabel,
		isRootDomain:    domain == effectiveDomain,
	}
}

func isSearchHomepage(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Path == "" || parsed.Path == "/"
}

func buildFTSPhraseQuery(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	escaped := make([]string, len(tokens))
	for i, t := range tokens {
		escaped[i] = escapeFTS5Token(t)
	}
	return `"` + strings.Join(escaped, " ") + `"`
}

func buildFTSProximityQuery(tokens []string) string {
	if len(tokens) < 2 {
		return "" // Proximity only makes sense for multiple tokens.
	}
	escaped := make([]string, len(tokens))
	for i, t := range tokens {
		escaped[i] = `"` + escapeFTS5Token(t) + `"`
	}
	return `NEAR(` + strings.Join(escaped, " ") + `, 4)`
}

func buildFTSExactQuery(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}

	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		parts = append(parts, quoteFTS5Token(token))
	}
	return strings.Join(parts, " ")
}

func buildFTSMajorityQuery(tokens []string) string {
	n := len(tokens)
	if n < 3 {
		return "" // Majority fallback only makes sense for 3+ words.
	}

	var clauses []string
	// Generate combinations of N-1 tokens. For 4 tokens, this generates 4 clauses of 3 tokens each.
	// (A B C) OR (A B D) OR (A C D) OR (B C D)
	for skip := 0; skip < n; skip++ {
		var parts []string
		for i, t := range tokens {
			if i == skip {
				continue
			}
			parts = append(parts, quoteFTS5Token(t))
		}
		clauses = append(clauses, "("+strings.Join(parts, " ")+")")
	}
	return strings.Join(clauses, " OR ")
}

func buildFTSPrefixQuery(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}

	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len([]rune(token)) < 3 {
			parts = append(parts, quoteFTS5Token(token))
			continue
		}
		parts = append(parts, escapeFTS5Token(token)+"*")
	}
	return strings.Join(parts, " ")
}

func buildFTSRelaxedQuery(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}

	var parts []string
	seen := make(map[string]struct{}, len(tokens)*2)
	for _, token := range tokens {
		exact := quoteFTS5Token(token)
		if _, ok := seen[exact]; !ok {
			seen[exact] = struct{}{}
			parts = append(parts, exact)
		}
		if len([]rune(token)) >= 3 {
			prefix := escapeFTS5Token(token) + "*"
			if _, ok := seen[prefix]; !ok {
				seen[prefix] = struct{}{}
				parts = append(parts, prefix)
			}
		}
	}
	return strings.Join(parts, " OR ")
}

func quoteFTS5Token(token string) string {
	return `"` + escapeFTS5Token(token) + `"`
}

func escapeFTS5Token(token string) string {
	return strings.ReplaceAll(token, `"`, `""`)
}

func buildSearchFragments(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}

	ordered := append([]string(nil), tokens...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len([]rune(ordered[i])) > len([]rune(ordered[j]))
	})

	var fragments []string
	for _, token := range ordered {
		fragments = append(fragments, sampleSearchFragments(token)...)
		fragments = uniqueStringsLimit(fragments, 8)
		if len(fragments) >= 8 {
			break
		}
	}

	return fragments
}

func sampleSearchFragments(token string) []string {
	runes := []rune(token)
	if len(runes) == 0 {
		return nil
	}
	if len(runes) <= 3 {
		return []string{token}
	}

	var trigrams []string
	for i := 0; i+3 <= len(runes); i++ {
		trigrams = append(trigrams, string(runes[i:i+3]))
	}
	if len(trigrams) <= 5 {
		return trigrams
	}

	indices := []int{0, len(trigrams) / 4, len(trigrams) / 2, (len(trigrams) * 3) / 4, len(trigrams) - 1}
	fragments := make([]string, 0, len(indices))
	for _, index := range indices {
		fragments = append(fragments, trigrams[index])
	}
	return uniqueStrings(fragments)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStringsLimit(values []string, limit int) []string {
	if limit <= 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, min(len(values), limit))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == limit {
			break
		}
	}
	return result
}

func mergeSearchCandidates(groups ...[]searchCandidate) []searchCandidate {
	merged := make(map[int64]searchCandidate)
	for _, group := range groups {
		for _, candidate := range group {
			existing, ok := merged[candidate.ID]
			if !ok || candidate.sqlScore > existing.sqlScore || (existing.Snippet == "" && candidate.Snippet != "") {
				if ok && candidate.Snippet == "" {
					candidate.Snippet = existing.Snippet
				}
				merged[candidate.ID] = candidate
			}
		}
	}

	results := make([]searchCandidate, 0, len(merged))
	for _, candidate := range merged {
		results = append(results, candidate)
	}
	return results
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

	rows, err := s.readDB.QueryContext(ctx, query, args...)
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
	rows, err := s.readDB.QueryContext(ctx, query, args...)
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
	rows, err := s.readDB.QueryContext(ctx, query, args...)
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

// UpdatePageRankings recalculates the referring_domains score for pages.
// It uses a batched approach to avoid locking the database for too long.
func (s *SQLiteStorage) UpdatePageRankings(ctx context.Context) error {
	s.logger.Info("starting page ranking update (referring domains)")
	start := time.Now()

	// 1. Reset all referring_domains to 0. We'll update the ones that have links.
	// This is fast enough as a single transaction if the DB isn't massive,
	// but can be optimized later if needed.
	_, err := s.writeDB.ExecContext(ctx, `UPDATE pages SET referring_domains = 0`)
	if err != nil {
		return fmt.Errorf("resetting referring_domains: %w", err)
	}

	// 2. We can update all pages that have backlinks in a single UPDATE ... FROM query.
	// SQLite supports UPDATE ... FROM since version 3.33.0.
	query := `
		WITH domain_counts AS (
			SELECT
				l.target_url,
				COUNT(DISTINCT p_source.domain) as unique_domains
			FROM links l
			JOIN pages p_source ON p_source.id = l.source_id
			JOIN pages p_target ON p_target.url = l.target_url
			WHERE p_source.domain != p_target.domain
			GROUP BY l.target_url
		)
		UPDATE pages
		SET referring_domains = domain_counts.unique_domains
		FROM domain_counts
		WHERE pages.url = domain_counts.target_url;
	`

	res, err := s.writeDB.ExecContext(ctx, query)
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

	rows, err := s.readDB.QueryContext(ctx, `SELECT id FROM pages`)
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

	edgeRows, err := s.readDB.QueryContext(ctx, `
		SELECT l.source_id, p.id 
		FROM links l
		JOIN pages p ON p.url = l.target_url
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
			newScores[i] = (1.0 - damping) + damping*(sum + danglingSum/float64(N))
		}
		
		for i := 0; i < N; i++ {
			scores[i] = newScores[i]
		}
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
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
func (s *SQLiteStorage) RecordClick(ctx context.Context, query string, url string) error {
	normalizedQuery := normalizeSearchText(query)
	if normalizedQuery == "" || url == "" {
		return nil
	}

	_, err := s.writeDB.ExecContext(ctx, `
		INSERT INTO search_clicks (query, url, clicks)
		VALUES (?, ?, 1)
		ON CONFLICT(query, url) DO UPDATE SET clicks = clicks + 1
	`, normalizedQuery, url)
	return err
}

// WriteDB returns the underlying write database connection.
// Used by the BatchWriter to share the same serialized write connection.
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
		i.id, i.url, i.page_url, i.domain, i.alt_text, i.title, i.context,
		i.width, i.height, rank
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

// --------------------------------------------------------------------------
// Link / Backlink Operations
// --------------------------------------------------------------------------

func (s *SQLiteStorage) StoreLinks(ctx context.Context, sourceID int64, sourceURL string, links []OutgoingLink) error {
	if len(links) == 0 {
		return nil
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning store-links transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM links WHERE source_id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("deleting old links for page %d: %w", sourceID, err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO links (source_id, source_url, target_url, anchor_text)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("preparing link insert: %w", err)
	}
	defer stmt.Close()

	for _, link := range links {
		if _, err := stmt.ExecContext(ctx, sourceID, sourceURL, link.TargetURL, link.AnchorText); err != nil {
			s.logger.Warn("failed to insert link", "source", sourceURL, "target", link.TargetURL, "error", err)
			continue
		}
	}

	return tx.Commit()
}

func (s *SQLiteStorage) GetBacklinks(ctx context.Context, targetURL string, limit, offset int) ([]BacklinkResult, int, error) {
	var total int
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE target_url = ?`, targetURL,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting backlinks for %q: %w", targetURL, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.readDB.QueryContext(ctx, `
		SELECT l.source_id, l.source_url, COALESCE(p.title, ''), l.anchor_text, l.created_at
		FROM links l
		LEFT JOIN pages p ON p.id = l.source_id
		WHERE l.target_url = ?
		ORDER BY l.created_at DESC
		LIMIT ? OFFSET ?
	`, targetURL, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying backlinks for %q: %w", targetURL, err)
	}
	defer rows.Close()

	var results []BacklinkResult
	for rows.Next() {
		var r BacklinkResult
		var createdAt sql.NullTime
		if err := rows.Scan(&r.SourceID, &r.SourceURL, &r.SourceTitle, &r.AnchorText, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scanning backlink: %w", err)
		}
		if createdAt.Valid {
			r.CreatedAt = createdAt.Time
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating backlinks: %w", err)
	}

	return results, total, nil
}

func (s *SQLiteStorage) GetOutlinks(ctx context.Context, pageID int64, limit, offset int) ([]OutlinkResult, int, error) {
	var total int
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE source_id = ?`, pageID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting outlinks for page %d: %w", pageID, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.readDB.QueryContext(ctx, `
		SELECT target_url, anchor_text, created_at
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
	for rows.Next() {
		var r OutlinkResult
		var createdAt sql.NullTime
		if err := rows.Scan(&r.TargetURL, &r.AnchorText, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scanning outlink: %w", err)
		}
		if createdAt.Valid {
			r.CreatedAt = createdAt.Time
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating outlinks: %w", err)
	}

	return results, total, nil
}

func (s *SQLiteStorage) GetBacklinkCount(ctx context.Context, targetURL string) (int, error) {
	var count int
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE target_url = ?`, targetURL,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting backlinks for %q: %w", targetURL, err)
	}
	return count, nil
}
