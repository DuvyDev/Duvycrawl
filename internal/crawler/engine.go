package crawler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/ratelimit"
	"github.com/DuvyDev/Duvycrawl/internal/render"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
	"github.com/DuvyDev/Duvycrawl/internal/urlfilter"
)

// EngineStatus represents the current state of the crawler engine.
type EngineStatus string

const (
	StatusIdle     EngineStatus = "idle"
	StatusRunning  EngineStatus = "running"
	StatusStopping EngineStatus = "stopping"
)

type renderBacklogItem struct {
	job       *queue.Job
	targetURL string
	reason    string
	attempts  int
	available time.Time
}

// Engine is the main crawler orchestrator. It manages a pool of worker
// goroutines that fetch, parse, and store web pages.
type Engine struct {
	cfg          *config.CrawlerConfig
	store        storage.Storage
	batchWriter  *storage.BatchWriter
	frontier     *frontier.Frontier
	fetcher      *Fetcher
	proxyManager *ProxyManager
	renderingCfg config.RenderingConfig
	renderer     render.Renderer
	parser       *Parser
	robots       *RobotsCache
	limiter      *ratelimit.DomainLimiter
	domainStats  *DomainStatsCollector
	logger       *slog.Logger

	noFollowDomains map[string]struct{}

	status  atomic.Value // EngineStatus
	cancel  context.CancelFunc
	crawlWG sync.WaitGroup

	renderMu      sync.Mutex
	renderBacklog []*renderBacklogItem
	renderQueued  map[string]struct{}

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
	renderingCfg config.RenderingConfig,
	logger *slog.Logger,
) *Engine {
	pm := NewProxyManager(cfg.ProxyURLs, cfg.ProxyCheckInterval, cfg.RequestTimeout, cfg.MaxIdleConnsPerHost, cfg.DisableCookies, logger)

	var browserRenderer render.Renderer
	if renderingEnabled(renderingCfg) {
		var err error
		primaryProxy := pm.PrimaryProxyURL()
		browserRenderer, err = render.NewBrowserRenderer(renderingCfg, cfg.UserAgent, primaryProxy, logger)
		if err != nil {
			logger.Warn("browser renderer disabled after initialization failure", "error", err)
		} else {
			logger.Info("browser renderer enabled",
				"mode", renderingCfg.Mode,
				"max_concurrency", renderingCfg.MaxConcurrency,
				"remote", renderingCfg.BrowserWSURL != "",
				"primary_proxy", primaryProxy,
			)
		}
	}

	e := &Engine{
		cfg:             cfg,
		store:           store,
		batchWriter:     batchWriter,
		frontier:        front,
		proxyManager:    pm,
		fetcher:         NewFetcher(cfg.UserAgent, cfg.MaxPageSizeKB, cfg.MaxRetries, pm, logger),
		renderingCfg:    renderingCfg,
		renderer:        browserRenderer,
		parser:          NewParser(),
		robots:          NewRobotsCache(cfg.UserAgent, 24*time.Hour, logger),
		limiter:         limiter,
		domainStats:     domainStats,
		logger:          logger.With("component", "engine"),
		renderQueued:    make(map[string]struct{}),
		noFollowDomains: make(map[string]struct{}, len(cfg.NoFollowDomains)),
	}
	for _, d := range cfg.NoFollowDomains {
		e.noFollowDomains[strings.ToLower(d)] = struct{}{}
	}
	e.status.Store(StatusIdle)
	return e
}

// Config returns the engine's crawler configuration.
func (e *Engine) Config() *config.CrawlerConfig {
	return e.cfg
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
	)

	// Start proxy health monitor
	e.proxyManager.StartMonitor(ctx)

	// Launch workers.
	for i := range e.cfg.Workers {
		e.crawlWG.Add(1)
		go e.worker(ctx, i)
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

	if e.proxyManager != nil {
		e.proxyManager.StopMonitor()
	}

	if e.fetcher != nil {
		if err := e.fetcher.Close(); err != nil {
			e.logger.Warn("fetcher close failed", "error", err)
		}
	}
	if e.renderer != nil {
		if err := e.renderer.Close(); err != nil {
			e.logger.Warn("renderer close failed", "error", err)
		}
	}

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

// isNoFollowDomain returns true if the domain or any of its parent domains
// (e.g. "wikipedia.org" for "es.wikipedia.org") is configured as a leaf node.
func (e *Engine) isNoFollowDomain(domain string) bool {
	d := domain
	for {
		if _, exists := e.noFollowDomains[d]; exists {
			return true
		}
		idx := strings.IndexByte(d, '.')
		if idx == -1 {
			break
		}
		d = d[idx+1:]
	}
	return false
}

// RenderBacklogLen returns the number of URLs waiting in the browser render backlog.
func (e *Engine) RenderBacklogLen() int {
	e.renderMu.Lock()
	defer e.renderMu.Unlock()
	return len(e.renderBacklog)
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

		if e.frontier.Stats().Pending == 0 {
			if item := e.popRenderBacklog(); item != nil {
				e.processRenderBacklogItem(ctx, logger, item)
				continue
			}
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
	if urlfilter.IsNonIndexableDocumentURL(job.URL) {
		logger.Debug("skipping non-indexable URL")
		return
	}

	// Check robots.txt if enabled.
	if e.cfg.RespectRobots && !e.robots.IsAllowed(ctx, job.URL, job.Domain) {
		logger.Debug("blocked by robots.txt")
		return
	}

	// Fetch the page.
	logger.Debug("fetching page")
	result, err := e.fetcher.Fetch(ctx, job.URL)
	if err != nil {
		if errors.Is(err, ErrUnsupportedContentType) {
			logger.Debug("skipping unsupported content type")
			return
		}
		e.retryOrFail(job, logger, "fetch failed", err)
		return
	}

	// Skip non-2xx responses.
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		if ok, reason := shouldRenderStatus(e.renderingCfg, result); ok {
			if e.enqueueRenderBacklog(job, job.URL, reason, logger) {
				return
			}
		}
	}

	if result.StatusCode < 200 || result.StatusCode >= 300 {
		logger.Warn("non-2xx response",
			"status_code", result.StatusCode,
			"content_type", result.ContentType,
			"final_url", result.FinalURL,
			"fetch_mode", result.Mode,
		)
		// Don't waste retries on permanent client errors (4xx except 429).
		if result.StatusCode >= 400 && result.StatusCode < 500 && result.StatusCode != http.StatusTooManyRequests {
			e.pagesErrored.Add(1)
			logger.Warn("permanent client error, skipping retries", "status_code", result.StatusCode, "url", job.URL)
			return
		}
		e.retryOrFail(job, logger, fmt.Sprintf("non-2xx status: %d", result.StatusCode), nil)
		return
	}
	if urlfilter.IsNonIndexableDocumentURL(result.FinalURL) {
		logger.Debug("skipping non-indexable final URL", "final_url", result.FinalURL)
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

	if ok, reason := shouldRenderParsed(e.renderingCfg, result, parsed); ok {
		if e.enqueueRenderBacklog(job, result.FinalURL, reason, logger) {
			return
		}
	}
	if urlfilter.IsNonIndexableDocumentURL(result.FinalURL) {
		logger.Debug("skipping non-indexable final URL", "final_url", result.FinalURL)
		return
	}
	if containsKnownInterstitial(lowerBodySample(result.Body)) {
		logger.Warn("page still looks like WAF interstitial, skipping index", "fetch_mode", result.Mode)
		e.pagesErrored.Add(1)
		return
	}

	e.persistParsedPage(ctx, logger, job, result, parsed)
}

func (e *Engine) persistParsedPage(ctx context.Context, logger *slog.Logger, job *queue.Job, result *FetchResult, parsed *ParseResult) {
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
		FetchMode:         result.Mode,
		RenderReason:      result.RenderReason,
		PublishedAt:       parsed.PublishedAt,
		CrawledAt:         time.Now().UTC(),
		SchemaType:        parsed.SchemaType,
		SchemaTitle:       truncateString(parsed.SchemaTitle, 500),
		SchemaDescription: truncateString(parsed.SchemaDescription, 1000),
		SchemaImage:       truncateString(parsed.SchemaImage, 2000),
		SchemaAuthor:      truncateString(parsed.SchemaAuthor, 500),
		SchemaKeywords:    truncateString(parsed.SchemaKeywords, 1000),
		SchemaRating:      parsed.SchemaRating,
	}

	e.batchWriter.WritePage(page)

	e.pagesCrawled.Add(1)
	logger.Info("page crawled successfully",
		"title", page.Title,
		"lang", page.Language,
		"region", region,
		"fetch_mode", result.Mode,
		"render_reason", result.RenderReason,
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
		if !e.isNoFollowDomain(job.Domain) {
			e.enqueueDiscoveredLinks(ctx, logger, parsed.Anchors, job.Depth+1, page)
		} else {
			logger.Debug("skipping outlinks for no-follow domain")
		}
	}

	// Update domain stats (async, in-memory accumulator).
	e.domainStats.Record(job.Domain, result.Duration)
}

func (e *Engine) enqueueRenderBacklog(job *queue.Job, targetURL, reason string, logger *slog.Logger) bool {
	if e.renderer == nil || !renderingEnabled(e.renderingCfg) {
		return false
	}
	if urlfilter.IsNonIndexableDocumentURL(targetURL) {
		return false
	}
	normalized, _, err := frontier.CanonicalizeURL(targetURL)
	if err == nil {
		targetURL = normalized
	}

	key := frontier.FingerprintURL(targetURL) + ":" + reason
	e.renderMu.Lock()
	defer e.renderMu.Unlock()
	if _, exists := e.renderQueued[key]; exists {
		logger.Debug("browser render already queued", "reason", reason, "render_url", targetURL)
		return true
	}

	jobCopy := *job
	e.renderQueued[key] = struct{}{}
	e.renderBacklog = append(e.renderBacklog, &renderBacklogItem{
		job:       &jobCopy,
		targetURL: targetURL,
		reason:    reason,
		available: time.Now(),
	})
	logger.Debug("queued URL for browser render backlog", "reason", reason, "render_url", targetURL, "backlog", len(e.renderBacklog))
	return true
}

func (e *Engine) popRenderBacklog() *renderBacklogItem {
	e.renderMu.Lock()
	defer e.renderMu.Unlock()
	if len(e.renderBacklog) == 0 {
		return nil
	}
	item := e.renderBacklog[0]
	copy(e.renderBacklog, e.renderBacklog[1:])
	e.renderBacklog[len(e.renderBacklog)-1] = nil
	e.renderBacklog = e.renderBacklog[:len(e.renderBacklog)-1]
	delete(e.renderQueued, frontier.FingerprintURL(item.targetURL)+":"+item.reason)
	return item
}

func (e *Engine) pushRenderBacklog(item *renderBacklogItem) {
	key := frontier.FingerprintURL(item.targetURL) + ":" + item.reason
	e.renderMu.Lock()
	defer e.renderMu.Unlock()
	if _, exists := e.renderQueued[key]; exists {
		return
	}
	e.renderQueued[key] = struct{}{}
	e.renderBacklog = append(e.renderBacklog, item)
}

func (e *Engine) processRenderBacklogItem(ctx context.Context, logger *slog.Logger, item *renderBacklogItem) {
	job := item.job
	logger = logger.With(
		"url", job.URL,
		"domain", job.Domain,
		"depth", job.Depth,
		"render_url", item.targetURL,
		"render_reason", item.reason,
	)
	if wait := time.Until(item.available); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

	if e.frontier.Stats().Pending > 0 {
		e.pushRenderBacklog(item)
		return
	}

	rendered, err := e.renderJob(ctx, logger, item.targetURL, item.reason)
	if errors.Is(err, render.ErrRendererBusy) {
		if item.attempts < e.renderingCfg.BusyRetryMaxAttempts {
			item.attempts++
			delay := time.Duration(item.attempts) * e.renderingCfg.BusyRetryBaseDelay
			if delay > e.renderingCfg.BusyRetryMaxDelay {
				delay = e.renderingCfg.BusyRetryMaxDelay
			}
			item.available = time.Now().Add(delay)
			e.pushRenderBacklog(item)
			logger.Debug("browser renderer busy, keeping URL in render backlog", "attempts", item.attempts, "delay", delay)
			return
		}
		logger.Warn("browser renderer remained busy, dropping render backlog item", "attempts", item.attempts)
		e.pagesErrored.Add(1)
		return
	}
	if err != nil {
		logger.Warn("browser render failed for backlog item", "error", err)
		// Re-enqueue the job for retry on fetch failure (proxy down, timeout, etc)
		e.retryOrFail(job, logger, "browser render failed", err)
		return
	}
	if rendered.StatusCode < 200 || rendered.StatusCode >= 300 {
		logger.Warn("rendered backlog item returned non-2xx", "status_code", rendered.StatusCode)
		e.pagesErrored.Add(1)
		return
	}
	if urlfilter.IsNonIndexableDocumentURL(rendered.FinalURL) {
		logger.Debug("skipping non-indexable rendered final URL", "final_url", rendered.FinalURL)
		return
	}
	if !isHTMLContentType(rendered.ContentType) {
		e.processDiscoveryOnly(ctx, logger, job, rendered)
		return
	}
	parsed, err := e.parser.Parse(rendered.Body, rendered.ContentType, rendered.FinalURL)
	if err != nil {
		logger.Warn("rendered backlog HTML parse failed", "error", err)
		e.pagesErrored.Add(1)
		return
	}
	if containsKnownInterstitial(lowerBodySample(rendered.Body)) {
		logger.Warn("rendered backlog page still looks like WAF interstitial, skipping index")
		e.pagesErrored.Add(1)
		return
	}

	e.persistParsedPage(ctx, logger, job, rendered, parsed)
}

func (e *Engine) renderJob(ctx context.Context, logger *slog.Logger, targetURL, reason string) (*FetchResult, error) {
	if e.renderer == nil {
		return nil, fmt.Errorf("renderer unavailable")
	}
	logger.Info("rendering page with browser", "reason", reason, "render_url", targetURL)
	rendered, err := e.renderer.Render(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	return &FetchResult{
		StatusCode:   rendered.StatusCode,
		Body:         rendered.Body,
		ContentType:  rendered.ContentType,
		FinalURL:     rendered.FinalURL,
		Duration:     rendered.Duration,
		Truncated:    rendered.Truncated,
		Mode:         "browser",
		RenderReason: reason,
	}, nil
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
		if urlfilter.IsNonIndexableDocumentURL(a.URL) {
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
		if urlfilter.IsNonIndexableDocumentURL(l) {
			continue
		}
		if frontier.ExtractDomain(l) == job.Domain {
			anchors = append(anchors, LinkAnchor{URL: l})
		}
	}
	if len(anchors) > 0 && job.Depth < e.cfg.MaxDepth {
		if !e.isNoFollowDomain(job.Domain) {
			e.enqueueDiscoveredLinks(ctx, logger, anchors, job.Depth+1, nil)
		}
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
