package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/utils"
)

// BatchWriter accumulates write operations (pages, links, images) and flushes
// them to SQLite in periodic transactions. This dramatically reduces the
// per-page transaction overhead from ~3 separate transactions to 1 transaction
// per batch.
//
// It is safe for concurrent use — workers call the Write* methods from
// multiple goroutines and the internal mutex serializes buffer access.
type BatchWriter struct {
	writeContentDB *sql.DB
	graphDB        *sql.DB
	logger         *slog.Logger

	mu               sync.Mutex
	pages            []*Page
	links            map[string][]OutgoingLink // sourceURL → links
	images           []ImageRecord
	onPagesPersisted func([]*Page)
	flushErr         error // last flush error, cleared on successful flush

	maxPages      int
	maxLinks      int
	maxImages     int
	flushInterval time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

// NewBatchWriter creates a background batch writer. Call Stop() on shutdown
// to flush any remaining buffered data.
func NewBatchWriter(writeContentDB, graphDB *sql.DB, logger *slog.Logger) *BatchWriter {
	bw := &BatchWriter{
		writeContentDB: writeContentDB,
		graphDB:        graphDB,
		logger:         logger.With("component", "batch_writer"),
		links:          make(map[string][]OutgoingLink),
		maxPages:       100,
		maxLinks:       500,
		maxImages:      500,
		flushInterval:  2 * time.Second,
		done:           make(chan struct{}),
	}
	bw.wg.Add(1)
	go bw.loop()
	return bw
}

// SetPagesPersistedHook registers a callback invoked after a successful page
// flush, once page IDs are known.
func (bw *BatchWriter) SetPagesPersistedHook(hook func([]*Page)) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	bw.onPagesPersisted = hook
}

// WritePage buffers a page for batch upsert.
func (bw *BatchWriter) WritePage(page *Page) {
	bw.mu.Lock()
	bw.pages = append(bw.pages, page)
	shouldFlush := len(bw.pages) >= bw.maxPages
	bw.mu.Unlock()
	if shouldFlush {
		if err := bw.Flush(); err != nil {
			bw.logger.Warn("inline page flush failed", "error", err)
		}
	}
}

// WriteLinks buffers outgoing links for a given source page URL.
// The sourceID is resolved automatically during flush.
func (bw *BatchWriter) WriteLinks(sourceURL string, links []OutgoingLink) {
	if len(links) == 0 {
		return
	}
	bw.mu.Lock()
	bw.links[sourceURL] = append(bw.links[sourceURL], links...)
	totalLinks := 0
	for _, l := range bw.links {
		totalLinks += len(l)
	}
	shouldFlush := totalLinks >= bw.maxLinks
	bw.mu.Unlock()
	if shouldFlush {
		if err := bw.Flush(); err != nil {
			bw.logger.Warn("inline links flush failed", "error", err)
		}
	}
}

// WriteImages buffers image records for batch upsert.
func (bw *BatchWriter) WriteImages(images []ImageRecord) {
	if len(images) == 0 {
		return
	}
	bw.mu.Lock()
	bw.images = append(bw.images, images...)
	shouldFlush := len(bw.images) >= bw.maxImages
	bw.mu.Unlock()
	if shouldFlush {
		if err := bw.Flush(); err != nil {
			bw.logger.Warn("inline images flush failed", "error", err)
		}
	}
}

// Flush synchronously writes all buffered data to the database.
// It is safe to call concurrently — only one flush executes at a time.
func (bw *BatchWriter) Flush() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return bw.flushLocked()
}

// Stop shuts down the background ticker and performs a final flush.
func (bw *BatchWriter) Stop() {
	close(bw.done)
	bw.wg.Wait()
	if err := bw.Flush(); err != nil {
		bw.logger.Warn("final flush failed", "error", err)
	}
}

func (bw *BatchWriter) loop() {
	defer bw.wg.Done()
	ticker := time.NewTicker(bw.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-bw.done:
			return
		case <-ticker.C:
			if err := bw.Flush(); err != nil {
				bw.logger.Warn("periodic flush failed", "error", err)
			}
		}
	}
}

func (bw *BatchWriter) flushLocked() error {
	if len(bw.pages) == 0 && len(bw.links) == 0 && len(bw.images) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := bw.writeContentDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch transaction: %w", err)
	}
	defer tx.Rollback()

	// -----------------------------------------------------------------
	// 1. Upsert all buffered pages
	// -----------------------------------------------------------------
	pageStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO pages (url, url_hash, domain, title, h1, h2, description, content, language, region, status_code, content_hash, url_fingerprint, published_at, crawled_at, updated_at, schema_type, schema_title, schema_description, schema_image, schema_author, schema_keywords, schema_rating)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			url_hash         = excluded.url_hash,
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
	`)
	if err != nil {
		return fmt.Errorf("prepare page upsert: %w", err)
	}
	defer pageStmt.Close()

	for _, page := range bw.pages {
		var publishedAt any
		if !page.PublishedAt.IsZero() {
			publishedAt = page.PublishedAt
		}
		var schemaRating any
		if page.SchemaRating > 0 {
			schemaRating = page.SchemaRating
		}
		
		page.URLHash = utils.HashURL(page.URL)
		
		_, err := pageStmt.ExecContext(ctx,
			page.URL, page.URLHash, page.Domain, page.Title, page.H1, page.H2, page.Description,
			page.Content, page.Language, page.Region,
			page.StatusCode, page.ContentHash, page.URLFingerprint,
			publishedAt, page.CrawledAt,
			page.SchemaType, page.SchemaTitle, page.SchemaDescription, page.SchemaImage,
			page.SchemaAuthor, page.SchemaKeywords, schemaRating,
		)
		if err != nil {
			bw.logger.Warn("batch upsert page failed", "url", page.URL, "error", err)
		}
	}

	// -----------------------------------------------------------------
	// 2. Resolve page IDs for links/images
	// -----------------------------------------------------------------
	urlToID := make(map[string]int64, len(bw.pages))
	if len(bw.pages) > 0 {
		urls := make([]string, 0, len(bw.pages))
		for _, p := range bw.pages {
			urls = append(urls, p.URL)
		}

		placeholders := make([]string, len(urls))
		args := make([]any, len(urls))
		for i, u := range urls {
			placeholders[i] = "?"
			args[i] = u
		}

		query := fmt.Sprintf(
			"SELECT id, url FROM pages WHERE url IN (%s)",
			strings.Join(placeholders, ","),
		)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			bw.logger.Warn("failed to resolve page IDs", "error", err)
		} else {
			for rows.Next() {
				var id int64
				var url string
				if err := rows.Scan(&id, &url); err != nil {
					continue
				}
				urlToID[url] = id
			}
			rows.Close()
		}

		for _, page := range bw.pages {
			if id, ok := urlToID[page.URL]; ok {
				page.ID = id
			}
		}
	}

	// -----------------------------------------------------------------
	// 3. Insert outgoing links
	// -----------------------------------------------------------------
	if len(bw.links) > 0 {
		graphTx, err := bw.graphDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin graph transaction: %w", err)
		}
		defer graphTx.Rollback()

		linkStmt, err := graphTx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO links (source_id, target_hash, anchor_text)
			VALUES (?, ?, ?)
		`)
		if err != nil {
			return fmt.Errorf("prepare link insert: %w", err)
		}
		defer linkStmt.Close()

		for sourceURL, links := range bw.links {
			sourceID, ok := urlToID[sourceURL]
			if !ok {
				// Fallback: query DB directly from writeContentDB transaction
				if err := tx.QueryRowContext(ctx,
					"SELECT id FROM pages WHERE url = ?", sourceURL,
				).Scan(&sourceID); err != nil {
					bw.logger.Warn("cannot resolve source_id for links", "url", sourceURL, "error", err)
					continue
				}
			}

			if _, err := graphTx.ExecContext(ctx, `DELETE FROM links WHERE source_id = ?`, sourceID); err != nil {
				bw.logger.Warn("failed to delete old links", "source_id", sourceID, "error", err)
			}

			for _, link := range links {
				if link.TargetHash == 0 {
					link.TargetHash = utils.HashURL(link.TargetURL)
				}
				if _, err := linkStmt.ExecContext(ctx, sourceID, link.TargetHash, link.AnchorText); err != nil {
					bw.logger.Warn("batch link insert failed", "source", sourceURL, "target_hash", link.TargetHash, "error", err)
				}
			}
		}

		if err := graphTx.Commit(); err != nil {
			return fmt.Errorf("commit graph transaction: %w", err)
		}
	}

	// -----------------------------------------------------------------
	// 4. Upsert images
	// -----------------------------------------------------------------
	if len(bw.images) > 0 {
		imgStmt, err := tx.PrepareContext(ctx, `
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
			return fmt.Errorf("prepare image upsert: %w", err)
		}
		defer imgStmt.Close()

		for _, img := range bw.images {
			pageID := img.PageID
			if pageID == 0 && img.PageURL != "" {
				if id, ok := urlToID[img.PageURL]; ok {
					pageID = id
				} else {
					// Fallback query.
					_ = tx.QueryRowContext(ctx,
						"SELECT id FROM pages WHERE url = ?", img.PageURL,
					).Scan(&pageID)
				}
			}
			_, err := imgStmt.ExecContext(ctx,
				img.URL, img.PageURL, pageID, img.Domain,
				img.AltText, img.Title, img.Context,
				img.Width, img.Height, img.CrawledAt,
			)
			if err != nil {
				bw.logger.Warn("batch image upsert failed", "url", img.URL, "error", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch transaction: %w", err)
	}

	persistedPages := make([]*Page, 0, len(bw.pages))
	for _, page := range bw.pages {
		if page.ID > 0 {
			persistedPages = append(persistedPages, page)
		}
	}
	persistHook := bw.onPagesPersisted

	// Only clear buffers on successful commit so a retry on the next cycle
	// can recover from transient SQLite errors (writes are idempotent thanks
	// to ON CONFLICT clauses).
	bw.pages = bw.pages[:0]
	for k := range bw.links {
		delete(bw.links, k)
	}
	bw.images = bw.images[:0]

	if persistHook != nil && len(persistedPages) > 0 {
		persistHook(persistedPages)
	}

	return nil
}
