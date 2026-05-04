package crawler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/embedder"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/ratelimit"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// EngineStatus represents the current state of the crawler engine.
type EngineStatus string

const (
	StatusIdle     EngineStatus = "idle"
	StatusRunning  EngineStatus = "running"
	StatusStopping EngineStatus = "stopping"
)

// embedJob carries the data needed to generate an embedding for a page.
type embedJob struct {
	pageID      int64
	pageURL     string
	title       string
	description string
	content     string
}

// Engine is the main crawler orchestrator. It manages a pool of worker
// goroutines that fetch, parse, and store web pages.
type Engine struct {
	cfg         *config.CrawlerConfig
	store       storage.Storage
	batchWriter *storage.BatchWriter
	frontier    *frontier.Frontier
	fetcher     *Fetcher
	parser      *Parser
	robots      *RobotsCache
	limiter     *ratelimit.DomainLimiter
	domainStats *DomainStatsCollector
	embedder    *embedder.Client
	logger      *slog.Logger

	status atomic.Value // EngineStatus
	cancel context.CancelFunc
	crawlWG sync.WaitGroup
	embedWG sync.WaitGroup

	// seedDomains holds the set of seed domain names for SeedDomainsOnly filtering.
	seedDomains map[string]bool

	// High-priority jobs are freshly persisted pages; low-priority jobs are
	// backfill of older pages missing embeddings.
	highEmbedQueue chan embedJob
	lowEmbedQueue  chan embedJob
	embedQueued    sync.Map // pageID -> struct{}{}

	// Metrics
	pagesCrawled atomic.Int64
	pagesErrored atomic.Int64
}

// NewEngine creates a new crawler engine wired with all its dependencies.
func NewEngine(
	cfg *config.CrawlerConfig,
	store storage.Storage,
	batchWriter *storage.BatchWriter,
	front *frontier.Frontier,
	limiter *ratelimit.DomainLimiter,
	domainStats *DomainStatsCollector,
	embedClient *embedder.Client,
	proxyURL string,
	logger *slog.Logger,
) *Engine {
	e := &Engine{
		cfg:         cfg,
		store:       store,
		batchWriter: batchWriter,
		frontier:    front,
		fetcher:     NewFetcher(cfg.UserAgent, cfg.RequestTimeout, cfg.MaxPageSizeKB, cfg.MaxRetries, cfg.MaxIdleConnsPerHost, cfg.DisableCookies, proxyURL, logger),
		parser:      NewParser(),
		robots:      NewRobotsCache(cfg.UserAgent, 24*time.Hour, logger),
		limiter:     limiter,
		domainStats: domainStats,
		embedder:    embedClient,
		logger:      logger.With("component", "engine"),
		highEmbedQueue: make(chan embedJob, 25000),
		lowEmbedQueue:  make(chan embedJob, 5000),
	}
	if batchWriter != nil {
		batchWriter.SetPagesPersistedHook(e.onPagesPersisted)
	}
	e.status.Store(StatusIdle)
	return e
}

// Start launches the worker pool and begins crawling.
// It is non-blocking and returns immediately. Use Stop() to shut down.
func (e *Engine) Start(ctx context.Context) {
	if e.Status() == StatusRunning {
		e.logger.Warn("engine already running, ignoring start request")
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.status.Store(StatusRunning)

	e.logger.Info("starting crawler engine",
		"workers", e.cfg.Workers,
		"max_depth", e.cfg.MaxDepth,
		"politeness_delay", e.cfg.PolitenessDelay,
		"random_delay", e.cfg.RandomDelay,
		"parallelism_per_domain", e.cfg.ParallelismPerDomain,
		"max_retries", e.cfg.MaxRetries,
		"disable_cookies", e.cfg.DisableCookies,
		"seed_domains_only", e.cfg.SeedDomainsOnly,
	)

	// Load seed domains so we can tag pages with IsSeed.
	if err := e.loadSeedDomains(ctx); err != nil {
		e.logger.Error("failed to load seed domains", "error", err)
	}

	// Launch workers.
	for i := range e.cfg.Workers {
		e.crawlWG.Add(1)
		go e.worker(ctx, i)
	}

	// Launch background embedding workers (limited concurrency to avoid
	// overwhelming the Ollama API).
	if e.embedder != nil {
		embedWorkers := e.embedder.Workers()
		for i := 0; i < embedWorkers; i++ {
			e.embedWG.Add(1)
			go e.embedWorker(ctx, i)
		}
		e.crawlWG.Add(1)
		go e.backfillEmbeddings(ctx)
		e.logger.Info("embedding workers started", "count", embedWorkers)
	}

	e.logger.Info("all workers started")
}

// Stop gracefully shuts down the crawler engine.
// Workers finish their current page before exiting.
func (e *Engine) Stop() {
	if e.Status() != StatusRunning {
		return
	}

	e.logger.Info("stopping crawler engine...")
	e.status.Store(StatusStopping)

	if e.cancel != nil {
		e.cancel()
	}

	e.crawlWG.Wait()

	if e.batchWriter != nil {
		if err := e.batchWriter.Flush(); err != nil {
			e.logger.Warn("final batch flush failed", "error", err)
		}
	}

	if e.fetcher != nil {
		if err := e.fetcher.Close(); err != nil {
			e.logger.Warn("fetcher close failed", "error", err)
		}
	}

	// Signal embedding workers to stop once the queue is drained.
	close(e.highEmbedQueue)
	close(e.lowEmbedQueue)

	e.embedWG.Wait()

	e.status.Store(StatusIdle)

	e.logger.Info("crawler engine stopped",
		"pages_crawled", e.pagesCrawled.Load(),
		"pages_errored", e.pagesErrored.Load(),
	)
}

// Status returns the current engine status.
func (e *Engine) Status() EngineStatus {
	return e.status.Load().(EngineStatus)
}

// Stats returns current crawl metrics.
func (e *Engine) Stats() (crawled, errored int64) {
	return e.pagesCrawled.Load(), e.pagesErrored.Load()
}

// worker is the main loop for a single crawler worker goroutine.
// The entire domain-selection logic happens in memory via the queue's
// Dequeue method — no database round-trips in the scheduling hot path.
func (e *Engine) worker(ctx context.Context, id int) {
	defer e.crawlWG.Done()

	logger := e.logger.With("worker", id)
	logger.Debug("worker started")

	// readyFn is passed to the queue to check rate limits in-memory.
	readyFn := func(domain string) bool {
		return e.limiter.TryWait(domain)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Debug("worker shutting down")
			return
		default:
		}

		// Ask the queue for a job from any ready domain.
		// This is entirely in-memory — O(domains), zero DB calls.
		// DequeueWithWait blocks efficiently until work is available
		// (woken by enqueue operations) instead of polling with Sleep.
		job := e.frontier.DequeueWithWait(ctx, readyFn)
		if job == nil {
			// Context cancelled or no work available after wait.
			return
		}

		if ctx.Err() != nil {
			return
		}

		e.processJob(ctx, logger, job)
	}
}

// retryOrFail re-enqueues a failed job if retries remain, otherwise counts it as errored.
func (e *Engine) retryOrFail(job *queue.Job, logger *slog.Logger, reason string, err error) {
	if job.Retries < e.cfg.MaxRetries {
		job.Retries++
		e.frontier.Retry(job)
		logger.Info("re-queuing job for retry",
			"reason", reason,
			"retries", job.Retries,
			"url", job.URL,
		)
		return
	}
	e.pagesErrored.Add(1)
	if err != nil {
		logger.Warn(reason, "error", err)
	} else {
		logger.Warn(reason)
	}
}

// processJob handles the complete lifecycle of crawling a single URL.
func (e *Engine) processJob(ctx context.Context, logger *slog.Logger, job *queue.Job) {
	logger = logger.With(
		"url", job.URL,
		"domain", job.Domain,
		"depth", job.Depth,
	)

	// Check robots.txt if enabled.
	if e.cfg.RespectRobots && !e.robots.IsAllowed(ctx, job.URL, job.Domain) {
		logger.Debug("blocked by robots.txt")
		return
	}

	// Fetch the page.
	logger.Debug("fetching page")
	result, err := e.fetcher.Fetch(ctx, job.URL)
	if err != nil {
		e.retryOrFail(job, logger, "fetch failed", err)
		return
	}

	// Skip non-2xx responses.
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		e.retryOrFail(job, logger, "non-2xx status", nil)
		return
	}

	if result.Truncated {
		logger.Warn("page truncated at size limit, metadata and links preserved but content incomplete",
			"url", job.URL,
			"limit_kb", e.cfg.MaxPageSizeKB,
		)
	}

	// Discovery-only path for non-HTML assets (CSS, JS, JSON, XML, RSS, Atom).
	if !isHTMLContentType(result.ContentType) {
		e.processDiscoveryOnly(ctx, logger, job, result)
		return
	}

	// Parse the HTML.
	parsed, err := e.parser.Parse(result.Body, result.ContentType, result.FinalURL)
	if err != nil {
		e.retryOrFail(job, logger, "parse failed", err)
		return
	}

	// Compute content hash for change detection.
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(parsed.Content)))
	pageURL := result.FinalURL
	if parsed.Canonical != "" {
		if canonicalURL, canonicalDomain, err := frontier.CanonicalizeURL(parsed.Canonical); err == nil && canonicalDomain == job.Domain {
			pageURL = canonicalURL
		}
	}
	if normalizedURL, _, err := frontier.CanonicalizeURL(pageURL); err == nil {
		pageURL = normalizedURL
	}

	// Infer region from TLD if not already known.
	region := inferRegion(job.Domain)

	// Compute structural fingerprint for deduplication.
	urlFingerprint := frontier.FingerprintURL(pageURL)

	// Store the page.
	page := &storage.Page{
		URL:               pageURL,
		Domain:            job.Domain,
		Title:             truncateString(parsed.Title, 500),
		H1:                truncateString(parsed.H1, 1000),
		H2:                truncateString(parsed.H2, 2000),
		Description:       truncateString(parsed.Description, 1000),
		Content:           parsed.Content,
		Language:          parsed.Language,
		Region:            region,
		StatusCode:        result.StatusCode,
		ContentHash:       contentHash,
		URLFingerprint:    urlFingerprint,
		PublishedAt:       parsed.PublishedAt,
		CrawledAt:         time.Now().UTC(),
		SchemaType:        parsed.SchemaType,
		SchemaTitle:       truncateString(parsed.SchemaTitle, 500),
		SchemaDescription: truncateString(parsed.SchemaDescription, 1000),
		SchemaImage:       truncateString(parsed.SchemaImage, 2000),
		SchemaAuthor:      truncateString(parsed.SchemaAuthor, 500),
		SchemaKeywords:    truncateString(parsed.SchemaKeywords, 1000),
		SchemaRating:      parsed.SchemaRating,
		IsSeed:            e.seedDomains[job.Domain],
	}

	e.batchWriter.WritePage(page)

	e.pagesCrawled.Add(1)
	logger.Info("page crawled successfully",
		"title", page.Title,
		"lang", page.Language,
		"region", region,
		"links_found", len(parsed.Links),
		"images_found", len(parsed.Images),
		"duration", result.Duration,
	)

	if len(parsed.Anchors) > 0 {
		outgoing := make([]storage.OutgoingLink, 0, len(parsed.Anchors))
		for _, a := range parsed.Anchors {
			// Skip intra-domain links to prevent massive graph.db bloat
			targetDomain := frontier.ExtractDomain(a.URL)
			if targetDomain == job.Domain {
				continue
			}
			outgoing = append(outgoing, storage.OutgoingLink{
				TargetURL:  a.URL,
				AnchorText: a.Anchor,
			})
		}
		e.batchWriter.WriteLinks(pageURL, outgoing)
	}

	// Store extracted images.
	if len(parsed.Images) > 0 {
		now := time.Now().UTC()
		var imageRecords []storage.ImageRecord
		for _, img := range parsed.Images {
			imageRecords = append(imageRecords, storage.ImageRecord{
				URL:       img.URL,
				PageURL:   pageURL,
				Domain:    job.Domain,
				AltText:   truncateString(img.Alt, 500),
				Title:     truncateString(img.Title, 500),
				Context:   truncateString(img.Context, 500),
				Width:     img.Width,
				Height:    img.Height,
				CrawledAt: now,
			})
		}
		e.batchWriter.WriteImages(imageRecords)
	}

	// Enqueue discovered links if we haven't exceeded max depth.
	if job.Depth < e.cfg.MaxDepth && len(parsed.Anchors) > 0 {
		e.enqueueDiscoveredLinks(ctx, logger, parsed.Anchors, job.Depth+1, page)
	}

	// Update domain stats (async, in-memory accumulator).
	e.domainStats.Record(job.Domain, result.Duration)
}

// enqueueDiscoveredLinks adds newly found URLs to the frontier.
// When SeedDomainsOnly is enabled, links to non-seed domains are discarded.
func (e *Engine) enqueueDiscoveredLinks(ctx context.Context, logger *slog.Logger, anchors []LinkAnchor, depth int, page *storage.Page) {
	baseScore := storage.PriorityNormal

	var sourceTitle, sourceSchemaType, sourceLanguage string
	if page != nil {
		sourceTitle = page.Title
		sourceSchemaType = page.SchemaType
		sourceLanguage = page.Language
	}

	// Build LinkContext for each anchor.
	var links []frontier.LinkContext
	for _, a := range anchors {
		// Restrict asset links (CSS/JS/JSON/XML/RSS/Atom) to the same domain.
		if isAssetExtension(a.URL) && page != nil && frontier.ExtractDomain(a.URL) != page.Domain {
			continue
		}
		links = append(links, frontier.LinkContext{
			URL:              a.URL,
			AnchorText:       a.Anchor,
			SourcePageTitle:  sourceTitle,
			SourceSchemaType: sourceSchemaType,
			SourceLanguage:   sourceLanguage,
		})
	}

	// Filter by seed domains if configured.
	if e.cfg.SeedDomainsOnly && len(e.seedDomains) > 0 {
		filtered := make([]frontier.LinkContext, 0, len(links))
		for _, link := range links {
			domain := frontier.ExtractDomain(link.URL)
			if e.seedDomains[domain] {
				filtered = append(filtered, link)
			}
		}
		if len(links) != len(filtered) {
			logger.Debug("filtered links by seed domains",
				"total", len(links),
				"kept", len(filtered),
				"discarded", len(links)-len(filtered),
			)
		}
		links = filtered
	}

	// Limit the number of links per page to prevent flooding.
	maxLinks := 100
	if len(links) > maxLinks {
		links = links[:maxLinks]
	}

	if len(links) == 0 {
		return
	}

	if err := e.frontier.AddBatch(ctx, links, depth, baseScore); err != nil {
		logger.Warn("failed to enqueue discovered links", "error", err, "count", len(links))
	}
}

// processDiscoveryOnly handles non-HTML assets (CSS, JS, JSON, XML, RSS, Atom)
// that are crawled solely to discover additional links. They are NOT indexed.
func (e *Engine) processDiscoveryOnly(ctx context.Context, logger *slog.Logger, job *queue.Job, result *FetchResult) {
	links := ExtractResourceLinks(result.Body, result.ContentType, result.FinalURL)
	if len(links) == 0 {
		return
	}

	var anchors []LinkAnchor
	for _, l := range links {
		if frontier.ExtractDomain(l) == job.Domain {
			anchors = append(anchors, LinkAnchor{URL: l})
		}
	}
	if len(anchors) > 0 && job.Depth < e.cfg.MaxDepth {
		e.enqueueDiscoveredLinks(ctx, logger, anchors, job.Depth+1, nil)
	}

	fingerprint := frontier.FingerprintURL(job.URL)
	dr := &storage.DiscoveredResource{
		URL:            job.URL,
		URLFingerprint: fingerprint,
		Kind:           kindFromContentType(result.ContentType),
		StatusCode:     result.StatusCode,
		LastCrawled:    time.Now().UTC(),
	}
	if err := e.store.UpsertDiscoveredResource(ctx, dr); err != nil {
		logger.Warn("failed to persist discovered resource", "error", err)
	}
}

func kindFromContentType(ct string) string {
	ct = strings.ToLower(ct)
	switch {
	case strings.Contains(ct, "text/css"):
		return "css"
	case strings.Contains(ct, "javascript"), strings.Contains(ct, "ecmascript"):
		return "js"
	case strings.Contains(ct, "json"):
		return "json"
	case strings.Contains(ct, "rss"):
		return "rss"
	case strings.Contains(ct, "atom"):
		return "atom"
	case strings.Contains(ct, "xml"):
		return "xml"
	default:
		return "other"
	}
}

// truncateString truncates a string to the given maximum length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// loadSeedDomains populates the seedDomains set from the database.
func (e *Engine) loadSeedDomains(ctx context.Context) error {
	seeds, err := e.store.GetSeedDomains(ctx)
	if err != nil {
		return err
	}

	e.seedDomains = make(map[string]bool, len(seeds))
	for _, s := range seeds {
		e.seedDomains[s.Domain] = true
	}

	e.logger.Info("loaded seed domains for filtering",
		"count", len(e.seedDomains),
	)
	return nil
}

// RefreshSeedDomains reloads the seed domains set.
// Call this after adding/removing seeds via the API.
func (e *Engine) RefreshSeedDomains(ctx context.Context) error {
	return e.loadSeedDomains(ctx)
}

func (e *Engine) onPagesPersisted(pages []*storage.Page) {
	for _, page := range pages {
		e.enqueueEmbedJob(embedJob{
			pageID:      page.ID,
			pageURL:     page.URL,
			title:       page.Title,
			description: page.Description,
			content:     truncateString(page.Content, 2048),
		}, true)
	}
}

func (e *Engine) enqueueEmbedJob(job embedJob, highPriority bool) {
	if job.pageID <= 0 {
		return
	}
	if _, loaded := e.embedQueued.LoadOrStore(job.pageID, struct{}{}); loaded {
		return
	}

	queue := e.lowEmbedQueue
	queueName := "low"
	if highPriority {
		queue = e.highEmbedQueue
		queueName = "high"
	}

	select {
	case queue <- job:
	default:
		e.embedQueued.Delete(job.pageID)
		e.logger.Debug("embedding queue full, dropping job", "page_id", job.pageID, "queue", queueName)
	}
}

func (e *Engine) backfillEmbeddings(ctx context.Context) {
	defer e.crawlWG.Done()
	logger := e.logger.With("component", "embed_backfill")

	const batchSize = 250
	for {
		afterID := int64(0)
		for {
			if ctx.Err() != nil {
				return
			}

			pages, err := e.store.ListPagesWithoutEmbeddings(ctx, afterID, batchSize)
			if err != nil {
				logger.Warn("failed to load pages without embeddings", "error", err)
				break
			}
			if len(pages) == 0 {
				break
			}

			for _, page := range pages {
				e.enqueueEmbedJob(embedJob{
					pageID:      page.ID,
					pageURL:     page.URL,
					title:       page.Title,
					description: page.Description,
					content:     page.Content,
				}, false)
				afterID = page.ID
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// embedWorker consumes embedding jobs from the queue and persists them.
// Limited concurrency protects the Ollama API from being overwhelmed.
func (e *Engine) embedWorker(ctx context.Context, id int) {
	defer e.embedWG.Done()
	logger := e.logger.With("embed_worker", id)
	logger.Debug("embedding worker started")
	highQueue := e.highEmbedQueue
	lowQueue := e.lowEmbedQueue

	for {
		if highQueue == nil && lowQueue == nil {
			logger.Debug("embedding worker shutting down (queues drained)")
			return
		}

		select {
		case job, ok := <-highQueue:
			if !ok {
				highQueue = nil
				continue
			}
			e.processEmbedJob(job, logger)
			continue
		default:
		}

		select {
		case job, ok := <-highQueue:
			if !ok {
				highQueue = nil
				continue
			}
			e.processEmbedJob(job, logger)
		case job, ok := <-lowQueue:
			if !ok {
				lowQueue = nil
				continue
			}
			e.processEmbedJob(job, logger)
		}
	}
}

func (e *Engine) processEmbedJob(job embedJob, logger *slog.Logger) {
	defer e.embedQueued.Delete(job.pageID)

	// Build a clean representation prioritising title and description over raw
	// content. Title is the strongest semantic signal, description next; content
	// is used only if budget remains.
	title := sanitizeEmbedText(job.title)
	desc := sanitizeEmbedText(job.description)
	content := sanitizeEmbedText(job.content)

	text := buildEmbedText(title, desc, content, 768)
	if text == "" {
		return
	}

	// Try with progressively shorter text if Ollama rejects for context length.
	maxLengths := []int{768, 512, 256}
	for _, maxLen := range maxLengths {
		candidate := buildEmbedText(title, desc, content, maxLen)

		vec, err := e.embedder.GenerateEmbedding(candidate)
		if err == nil {
			e.saveEmbedding(job, vec, logger)
			return
		}
		if !isContextLengthError(err) {
			logger.Debug("embedding generation failed", "url", job.pageURL, "error", err)
			return
		}
		logger.Debug("embedding context exceeded, retrying shorter", "url", job.pageURL, "len", maxLen)
	}
}

// buildEmbedText assembles title + description + content, always keeping the
// title intact, then fitting as much description as possible, and finally
// padding with content up to maxLen. Never cuts mid-word.
func buildEmbedText(title, desc, content string, maxLen int) string {
	if title == "" && desc == "" && content == "" {
		return ""
	}

	maxWords := embedWordBudget(maxLen)
	parts := []string{}
	remaining := maxLen
	usedWords := 0

	// Title always goes in full (it's the strongest signal).
	if title != "" {
		title = truncateByWordAndBytes(title, maxWords, remaining)
		if len(title) > remaining {
			title = truncateAtWordBoundary(title, remaining)
		}
		parts = append(parts, title)
		remaining -= len(title)
		usedWords += len(strings.Fields(title))
	}

	// Fit description next, truncated at last full word if needed.
	if desc != "" && remaining > 1 && usedWords < maxWords {
		descPart := desc
		descPart = truncateByWordAndBytes(descPart, maxWords-usedWords, remaining-1)
		if descPart != "" {
			parts = append(parts, descPart)
			remaining -= len(descPart) + 1
			usedWords += len(strings.Fields(descPart))
		}
	}

	// Whatever space is left goes to content.
	if content != "" && remaining > 1 && usedWords < maxWords {
		contentPart := content
		contentPart = truncateByWordAndBytes(contentPart, maxWords-usedWords, remaining-1)
		if contentPart != "" {
			parts = append(parts, contentPart)
		}
	}

	return strings.Join(parts, " ")
}

func embedWordBudget(maxLen int) int {
	switch {
	case maxLen >= 768:
		return 120
	case maxLen >= 512:
		return 80
	default:
		return 40
	}
}

func truncateByWordAndBytes(s string, maxWords, maxBytes int) string {
	if s == "" || maxWords <= 0 || maxBytes <= 0 {
		return ""
	}
	words := strings.Fields(s)
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	joined := strings.Join(words, " ")
	if len(joined) <= maxBytes {
		return joined
	}
	return truncateAtWordBoundary(joined, maxBytes)
}

// truncateAtWordBoundary cuts s to fit within maxLen bytes without breaking
// a word. If the first word is longer than maxLen, it falls back to a hard
// truncation.
func truncateAtWordBoundary(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Walk backwards from maxLen to find a space.
	for i := maxLen; i > 0; i-- {
		if s[i] == ' ' {
			return strings.TrimSpace(s[:i])
		}
	}
	// No space found — hard truncate.
	return s[:maxLen]
}

// isContextLengthError detects if an error is due to exceeding the model's context window.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "context length") ||
		strings.Contains(errStr, "input length")
}

// sanitizeEmbedText removes problematic characters and normalizes whitespace
// to prevent tokenizer issues.
func sanitizeEmbedText(s string) string {
	if s == "" {
		return ""
	}
	// Strip HTML tags if any slipped through.
	s = stripHTMLTags(s)
	// Replace control chars and excessive whitespace with single spaces.
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		// Skip null bytes and other control characters.
		if r < 32 {
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	clean := strings.TrimSpace(b.String())
	if clean == "" {
		return ""
	}

	// Remove pathological tokens that explode into many subword pieces.
	words := strings.Fields(clean)
	filtered := make([]string, 0, len(words))
	for _, w := range words {
		if shouldSkipEmbedToken(w) {
			continue
		}
		if len(w) > 32 {
			w = w[:32]
		}
		filtered = append(filtered, w)
	}

	return strings.Join(filtered, " ")
}

func shouldSkipEmbedToken(token string) bool {
	lower := strings.ToLower(token)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "www.") {
		return true
	}
	if len(token) > 96 {
		return true
	}
	letters := 0
	for _, r := range token {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	// Drop long mostly-non-letter blobs like hashes, query strings, base64-ish text.
	if len(token) > 24 && letters < len(token)/4 {
		return true
	}
	return false
}

// stripHTMLTags removes simple HTML tags from a string.
func stripHTMLTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (e *Engine) saveEmbedding(job embedJob, vec []float32, logger *slog.Logger) {
	ctxSave, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	emb := &storage.PageEmbedding{
		PageID:     job.pageID,
		Model:      e.embedder.Model(),
		Dimensions: len(vec),
		Embedding:  vec,
	}
	if err := e.store.SavePageEmbedding(ctxSave, emb); err != nil {
		logger.Debug("embedding save failed", "url", job.pageURL, "page_id", job.pageID, "error", err)
	}
}

// inferRegion extracts a country/region code from a domain's TLD.
// For example: "elpais.com.uy" → "uy", "vandal.elespanol.com" → "".
func inferRegion(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return ""
	}
	tld := parts[len(parts)-1]
	// Check if TLD is a known country code (2 letters).
	if len(tld) == 2 && tld != "io" && tld != "tv" && tld != "co" && tld != "me" {
		return strings.ToLower(tld)
	}
	return ""
}
