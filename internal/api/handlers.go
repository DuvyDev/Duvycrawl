package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/DuvyDev/Duvycrawl/internal/crawler"
	"github.com/DuvyDev/Duvycrawl/internal/frontier"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// Handlers holds the dependencies needed by the API handlers.
type Handlers struct {
	store    storage.Storage
	engine   *crawler.Engine
	frontier *frontier.Frontier
	logger   *slog.Logger
}

const maxIngestOutlinks = 3

// NewHandlers creates a new Handlers instance.
func NewHandlers(store storage.Storage, engine *crawler.Engine, front *frontier.Frontier, logger *slog.Logger) *Handlers {
	return &Handlers{
		store:    store,
		engine:   engine,
		frontier: front,
		logger:   logger.With("component", "api"),
	}
}

// --- Response Helpers ---

type apiResponse struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiResponse{Error: msg})
}

func writeSuccess(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiResponse{Data: data})
}

// --- Health ---

// HealthCheck returns a simple health check response.
func (h *Handlers) HealthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"version":   "1.0.0",
	})
}

// --- Search ---

type searchResponse struct {
	Query   string                 `json:"query"`
	Total   int                    `json:"total"`
	Page    int                    `json:"page"`
	Limit   int                    `json:"limit"`
	Domain  string                 `json:"domain,omitempty"`
	Type    string                 `json:"type,omitempty"`
	Results []storage.SearchResult `json:"results"`
}

// Search performs a full-text search over crawled pages.
func (h *Handlers) Search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter 'q'")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 10
	}

	offset := (page - 1) * limit

	lang := r.URL.Query().Get("lang")
	domain := r.URL.Query().Get("domain")
	schemaType := r.URL.Query().Get("type")

	results, total, err := h.store.SearchPages(r.Context(), query, limit, offset, lang, domain, schemaType)
	if err != nil {
		h.logger.Error("search failed", "query", query, "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	if results == nil {
		results = []storage.SearchResult{}
	}

	// Record the query for adaptive scoring (fire-and-forget).
	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	if normalizedQuery != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.RecordSearchQuery(ctx, query, normalizedQuery, lang); err != nil {
				h.logger.Warn("failed to record search query", "error", err)
			}
		}()
	}

	// Allow Cloudflare / reverse proxies to cache identical queries briefly.
	w.Header().Set("Cache-Control", "public, s-maxage=60, max-age=10, stale-while-revalidate=300")

	writeJSON(w, http.StatusOK, searchResponse{
		Query:   query,
		Total:   total,
		Page:    page,
		Limit:   limit,
		Domain:  domain,
		Type:    schemaType,
		Results: results,
	})
}

// --- Pages ---

// GetPage returns a single page by ID.
func (h *Handlers) GetPage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page ID")
		return
	}

	page, err := h.store.GetPageByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to get page", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get page")
		return
	}

	if page == nil {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}

	writeSuccess(w, page)
}

// LookupPage looks up a page by its exact URL.
func (h *Handlers) LookupPage(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter 'url'")
		return
	}

	page, err := h.store.GetPageByURL(r.Context(), url)
	if err != nil {
		h.logger.Error("failed to lookup page", "url", url, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to lookup page")
		return
	}

	if page == nil {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}

	writeSuccess(w, page)
}

// --- Stats ---

// GetStats returns overall statistics.
// On the crawler node this includes engine session metrics.
// On the search node it returns storage stats only.
func (h *Handlers) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get stats")
		return
	}

	result := map[string]any{
		"stats": stats,
	}

	if h.engine != nil {
		crawled, errored := h.engine.Stats()
		result["engine_status"] = h.engine.Status()
		result["session"] = map[string]any{
			"pages_crawled": crawled,
			"pages_errored": errored,
		}
	}

	writeSuccess(w, result)
}

// --- Crawl ---

type crawlRequest struct {
	URLs     []string `json:"urls"`
	Priority int      `json:"priority,omitempty"`
	Force    bool     `json:"force,omitempty"`
}

// CrawlURLs enqueues one or more URLs for crawling.
// By default (force=false), URLs that were crawled within the last 24 hours
// are skipped to avoid redundant re-indexing. Set force=true to override.
func (h *Handlers) CrawlURLs(w http.ResponseWriter, r *http.Request) {
	var req crawlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "at least one URL is required")
		return
	}

	if len(req.URLs) > 1000 {
		writeError(w, http.StatusBadRequest, "maximum 1000 URLs per request")
		return
	}

	baseScore := float64(req.Priority)
	if baseScore <= 0 {
		baseScore = storage.PriorityNormal
	}

	urlsToEnqueue := req.URLs
	var skipped int

	if !req.Force {
		freshTTL := 24 * time.Hour
		cutoff := time.Now().Add(-freshTTL)
		fresh, err := h.store.GetFreshURLs(r.Context(), req.URLs, cutoff)
		if err != nil {
			h.logger.Warn("failed to check fresh URLs, enqueuing all", "error", err)
		} else if len(fresh) > 0 {
			filtered := make([]string, 0, len(req.URLs))
			for _, u := range req.URLs {
				if _, isFresh := fresh[u]; isFresh {
					skipped++
				} else {
					filtered = append(filtered, u)
				}
			}
			urlsToEnqueue = filtered
		}
	}

	var startingDepth int
	cfg := h.engine.Config()
	if cfg.MaxDepth > cfg.APIMaxDepth {
		startingDepth = cfg.MaxDepth - cfg.APIMaxDepth
	}

	var queued int
	if len(urlsToEnqueue) > 0 {
		var err error
		queued, err = h.frontier.AddBatchDirect(r.Context(), urlsToEnqueue, startingDepth, baseScore)
		if err != nil {
			h.logger.Error("failed to enqueue URLs", "error", err, "count", len(urlsToEnqueue))
			writeError(w, http.StatusInternalServerError, "failed to enqueue URLs")
			return
		}
	}

	resp := map[string]any{
		"queued":  queued,
		"skipped": skipped,
	}
	if skipped > 0 {
		resp["reason"] = "already_indexed"
	}

	writeJSON(w, http.StatusAccepted, resp)
}

// --- Queue ---

// GetQueue returns the current crawl queue status.
func (h *Handlers) GetQueue(w http.ResponseWriter, r *http.Request) {
	stats := h.frontier.Stats()
	renderBacklog := h.engine.RenderBacklogLen()
	writeJSON(w, http.StatusOK, map[string]any{
		"pending":        stats.Pending,
		"domains":        stats.Domains,
		"total_enqueued": stats.Enqueued,
		"total_dequeued": stats.Dequeued,
		"render_backlog": renderBacklog,
	})
}

// --- Image Search ---

type imageSearchResponse struct {
	Query   string                      `json:"query"`
	Total   int                         `json:"total"`
	Page    int                         `json:"page"`
	Limit   int                         `json:"limit"`
	Results []storage.ImageSearchResult `json:"results"`
}

// SearchImages performs a full-text search over image metadata.
func (h *Handlers) SearchImages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter 'q'")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}

	offset := (page - 1) * limit

	results, total, err := h.store.SearchImages(r.Context(), query, limit, offset)
	if err != nil {
		h.logger.Error("image search failed", "query", query, "error", err)
		writeError(w, http.StatusInternalServerError, "image search failed")
		return
	}

	if results == nil {
		results = []storage.ImageSearchResult{}
	}

	writeJSON(w, http.StatusOK, imageSearchResponse{
		Query:   query,
		Total:   total,
		Page:    page,
		Limit:   limit,
		Results: results,
	})
}

// --- Seeds ---

type seedURLRequest struct {
	URL             string        `json:"url"`
	RecrawlInterval time.Duration `json:"recrawl_interval,omitempty"`
}

// ListSeeds returns all seed URLs.
func (h *Handlers) ListSeeds(w http.ResponseWriter, r *http.Request) {
	seeds, err := h.store.GetSeedURLs(r.Context())
	if err != nil {
		h.logger.Error("failed to list seeds", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list seeds")
		return
	}

	if seeds == nil {
		seeds = []storage.SeedURL{}
	}

	writeSuccess(w, seeds)
}

// AddSeed adds a new seed URL.
func (h *Handlers) AddSeed(w http.ResponseWriter, r *http.Request) {
	var req seedURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	domain := frontier.ExtractDomain(req.URL)
	if domain == "" {
		writeError(w, http.StatusBadRequest, "invalid url")
		return
	}

	seed := &storage.SeedURL{
		URL:                    req.URL,
		Domain:                 domain,
		RecrawlIntervalSeconds: int(req.RecrawlInterval.Seconds()),
	}

	if err := h.store.UpsertSeedURL(r.Context(), seed); err != nil {
		h.logger.Error("failed to add seed", "url", req.URL, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add seed")
		return
	}

	// Enqueue the URL immediately via the frontier.
	if err := h.frontier.Add(r.Context(), req.URL, 0, storage.PrioritySeed); err != nil {
		h.logger.Warn("failed to enqueue seed url", "url", req.URL, "error", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message": "seed url added",
		"url":     req.URL,
	})
}

// DeleteSeed removes a seed URL.
func (h *Handlers) DeleteSeed(w http.ResponseWriter, r *http.Request) {
	url := r.PathValue("url")
	if url == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	if err := h.store.DeleteSeedURL(r.Context(), url); err != nil {
		h.logger.Error("failed to delete seed", "url", url, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete seed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "seed url removed",
		"url":     url,
	})
}

// --- Crawler Control ---

// StartCrawler starts the crawler engine.
func (h *Handlers) StartCrawler(w http.ResponseWriter, r *http.Request) {
	if h.engine.Status() == crawler.StatusRunning {
		writeError(w, http.StatusConflict, "crawler is already running")
		return
	}

	go h.engine.Start(context.Background())

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "crawler started",
	})
}

// StopCrawler stops the crawler engine.
func (h *Handlers) StopCrawler(w http.ResponseWriter, r *http.Request) {
	if h.engine.Status() != crawler.StatusRunning {
		writeError(w, http.StatusConflict, "crawler is not running")
		return
	}

	go h.engine.Stop()

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "crawler stop initiated",
	})
}

// CrawlerStatus returns the current status of the crawler engine.
func (h *Handlers) CrawlerStatus(w http.ResponseWriter, r *http.Request) {
	crawled, errored := h.engine.Stats()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        h.engine.Status(),
		"pages_crawled": crawled,
		"pages_errored": errored,
	})
}

// --- Backlinks ---

type backlinksResponse struct {
	TargetURL string                   `json:"target_url"`
	Total     int                      `json:"total"`
	Page      int                      `json:"page"`
	Limit     int                      `json:"limit"`
	Results   []storage.BacklinkResult `json:"results"`
}

func (h *Handlers) GetBacklinks(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter 'url'")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}

	offset := (page - 1) * limit

	results, total, err := h.store.GetBacklinks(r.Context(), targetURL, limit, offset)
	if err != nil {
		h.logger.Error("backlink query failed", "url", targetURL, "error", err)
		writeError(w, http.StatusInternalServerError, "backlink query failed")
		return
	}

	if results == nil {
		results = []storage.BacklinkResult{}
	}

	writeJSON(w, http.StatusOK, backlinksResponse{
		TargetURL: targetURL,
		Total:     total,
		Page:      page,
		Limit:     limit,
		Results:   results,
	})
}

// --- Outlinks ---

type outlinksResponse struct {
	PageID  int64                   `json:"page_id"`
	Total   int                     `json:"total"`
	Page    int                     `json:"page"`
	Limit   int                     `json:"limit"`
	Results []storage.OutlinkResult `json:"results"`
}

func (h *Handlers) GetOutlinks(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page ID")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 50
	}

	offset := (page - 1) * limit

	results, total, err := h.store.GetOutlinks(r.Context(), id, limit, offset)
	if err != nil {
		h.logger.Error("outlink query failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "outlink query failed")
		return
	}

	if results == nil {
		results = []storage.OutlinkResult{}
	}

	writeJSON(w, http.StatusOK, outlinksResponse{
		PageID:  id,
		Total:   total,
		Page:    page,
		Limit:   limit,
		Results: results,
	})
}

// --- Interactions ---

type interactRequest struct {
	Query string `json:"query"`
	URL   string `json:"url"`
}

// Interact records a user click on a search result and boosts the adaptive
// profile with terms from the clicked page.
func (h *Handlers) Interact(w http.ResponseWriter, r *http.Request) {
	var req interactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Query == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, "query and url are required")
		return
	}

	if err := h.store.RecordClick(r.Context(), req.Query, req.URL); err != nil {
		h.logger.Error("failed to record click", "query", req.Query, "url", req.URL, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record interaction")
		return
	}

	// Boost terms from the clicked page in the background.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.store.BoostTermsFromClick(ctx, req.Query, req.URL, 3.0); err != nil {
			h.logger.Warn("failed to boost terms from click", "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"message": "interaction recorded",
	})
}

// --- Interests ---

type addInterestRequest struct {
	Term   string  `json:"term"`
	Weight float64 `json:"weight"`
	Lang   string  `json:"lang,omitempty"`
}

// AddInterest creates or accumulates a manual interest term in the adaptive profile.
func (h *Handlers) AddInterest(w http.ResponseWriter, r *http.Request) {
	var req addInterestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Term == "" {
		writeError(w, http.StatusBadRequest, "term is required")
		return
	}
	if req.Weight <= 0 {
		writeError(w, http.StatusBadRequest, "weight must be > 0")
		return
	}

	tokens := normalizeInterestAPITokens(req.Term)
	if len(tokens) == 0 {
		writeError(w, http.StatusBadRequest, "term produces no valid tokens (must be > 2 chars)")
		return
	}

	for _, tok := range tokens {
		if err := h.store.SetInterestTerm(r.Context(), tok, req.Weight, "manual", req.Lang); err != nil {
			h.logger.Error("failed to set interest term", "term", tok, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to set interest term")
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message": "interest term added",
		"tokens":  tokens,
		"weight":  req.Weight,
	})
}

// ListInterests returns all manually declared interest terms.
func (h *Handlers) ListInterests(w http.ResponseWriter, r *http.Request) {
	interests, err := h.store.GetManualInterests(r.Context(), "manual")
	if err != nil {
		h.logger.Error("failed to list interests", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list interests")
		return
	}
	if interests == nil {
		interests = []storage.InterestTermRecord{}
	}
	writeSuccess(w, interests)
}

// DeleteInterest removes a manual interest term.
func (h *Handlers) DeleteInterest(w http.ResponseWriter, r *http.Request) {
	rawTerm := r.PathValue("term")
	if rawTerm == "" {
		writeError(w, http.StatusBadRequest, "term is required")
		return
	}

	tokens := normalizeInterestAPITokens(rawTerm)
	if len(tokens) == 0 {
		writeError(w, http.StatusBadRequest, "term produces no valid tokens (must be > 2 chars)")
		return
	}

	for _, tok := range tokens {
		if err := h.store.RemoveInterestTerm(r.Context(), tok, "manual"); err != nil {
			h.logger.Error("failed to delete interest term", "term", tok, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete interest term")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "interest term removed",
		"term":    rawTerm,
		"tokens":  tokens,
	})
}

// normalizeInterestAPITokens mirrors storage.normalizeInterestTokens for API use.
func normalizeInterestAPITokens(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	var out []string
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			if b.Len() > 2 {
				out = append(out, b.String())
			}
			b.Reset()
		}
	}
	if b.Len() > 2 {
		out = append(out, b.String())
	}
	return out
}

func clampIngestOutlinks(outlinks []string) []string {
	if len(outlinks) <= maxIngestOutlinks {
		return outlinks
	}
	return outlinks[:maxIngestOutlinks]
}

func parseIngestTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, value); err == nil && isSaneIngestTime(t) {
			return t
		}
	}
	return time.Time{}
}

func isSaneIngestTime(t time.Time) bool {
	year := t.Year()
	return !t.IsZero() && year >= 1990 && year <= time.Now().UTC().Year()+1
}

func isSearchResultsPageURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	host := strings.ToLower(parsed.Hostname())
	path := strings.ToLower(parsed.Path)

	query := parsed.Query()
	hasSearchTerm := query.Has("q") || query.Has("query") || query.Has("p") || query.Has("text") || query.Has("s")
	looksLikeSearxInstance :=
		path == "/search" && query.Has("q") &&
			(query.Has("categories") || query.Has("language") || query.Has("engines") || query.Has("theme") || query.Has("time_range") || query.Has("safesearch"))
	looksLikeWhoogleInstance :=
		(path == "/search" || path == "/") && query.Has("q") &&
			(query.Has("safe") || query.Has("tbm") || query.Has("start") || query.Has("region") || query.Has("near"))

	if strings.HasPrefix(host, "www.google.") || strings.HasPrefix(host, "google.") {
		return path == "/search" || strings.HasPrefix(path, "/search/") || path == "/imgres" || strings.HasPrefix(path, "/sorry/")
	}
	if host == "www.bing.com" || host == "bing.com" {
		return path == "/search"
	}
	if host == "duckduckgo.com" || host == "www.duckduckgo.com" {
		return (path == "/" && hasSearchTerm) || path == "/html"
	}
	if host == "search.yahoo.com" {
		return path == "/search"
	}
	if strings.HasPrefix(host, "www.yandex.") || strings.HasPrefix(host, "yandex.") {
		return path == "/search"
	}
	if host == "www.baidu.com" || host == "baidu.com" {
		return path == "/s"
	}
	if host == "www.ecosia.org" || host == "ecosia.org" {
		return path == "/search"
	}
	if host == "www.mojeek.com" || host == "mojeek.com" {
		return path == "/search"
	}
	if host == "search.brave.com" {
		return path == "/search"
	}
	if host == "www.metager.org" || host == "metager.org" {
		return strings.HasPrefix(path, "/meta/") || path == "/"
	}
	if host == "www.stract.com" || host == "stract.com" {
		return path == "/search"
	}
	if host == "www.mwmbl.org" || host == "mwmbl.org" {
		return path == "/" && hasSearchTerm
	}
	if strings.Contains(host, "yacy") {
		return path == "/yacysearch.html" || path == "/solr/select"
	}
	if host == "www.startpage.com" || host == "startpage.com" {
		return path == "/sp/search" || (path == "/" && hasSearchTerm)
	}
	if strings.Contains(host, "searx") || looksLikeSearxInstance {
		return path == "/search"
	}
	if strings.Contains(host, "whoogle") || looksLikeWhoogleInstance {
		return (path == "/search" || path == "/") && query.Has("q")
	}

	return false
}

// --- Ingest (Extension) ---

// IngestPage receives a single pre-extracted page from the browser extension.
// It stores the page content directly (skips HTTP fetch) and optionally records
// outlinks. Outlinks are enqueued for future crawling by the frontier.
func (h *Handlers) IngestPage(w http.ResponseWriter, r *http.Request) {
	var req storage.IngestPageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	if isSearchResultsPageURL(req.URL) {
		writeJSON(w, http.StatusOK, map[string]any{
			"indexed":         false,
			"url":             req.URL,
			"reason":          "search_results_page",
			"outlinks_queued": 0,
		})
		return
	}

	if h.frontier == nil {
		writeError(w, http.StatusInternalServerError, "frontier not available on this node")
		return
	}

	normalized, domain, err := frontier.CanonicalizeURL(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid url")
		return
	}

	crawledAt := time.Now().UTC()
	if req.CrawledAt != "" {
		if t := parseIngestTime(req.CrawledAt); !t.IsZero() {
			crawledAt = t
		}
	}

	var publishedAt time.Time
	if req.PublishedAt != "" {
		publishedAt = parseIngestTime(req.PublishedAt)
	}

	page := &storage.Page{
		URL:               normalized,
		Domain:            domain,
		Title:             req.Title,
		H1:                req.H1,
		Description:       req.Description,
		Content:           req.Content,
		Language:          req.Language,
		StatusCode:        200,
		ContentHash:       fmt.Sprintf("%x", sha256.Sum256([]byte(req.Content))),
		URLFingerprint:    frontier.FingerprintURL(normalized),
		FetchMode:         "extension",
		RenderReason:      "submitted_by_extension",
		IngestHits:        1,
		LastSeenAt:        crawledAt,
		CrawledAt:         crawledAt,
		PublishedAt:       publishedAt,
		SchemaType:        req.SchemaType,
		SchemaTitle:       req.SchemaTitle,
		SchemaDescription: req.SchemaDescription,
		SchemaImage:       req.SchemaImage,
		SchemaAuthor:      req.SchemaAuthor,
		SchemaKeywords:    req.SchemaKeywords,
	}

	limitedOutlinks := clampIngestOutlinks(req.Outlinks)

	pageID, err := h.store.IngestPage(r.Context(), page, limitedOutlinks)
	if err != nil {
		h.logger.Error("ingest page failed", "url", normalized, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to ingest page")
		return
	}

	// Enqueue outlinks for future crawling
	outlinksQueued := 0
	if len(limitedOutlinks) > 0 {
		linkCtxs := make([]frontier.LinkContext, 0, len(limitedOutlinks))
		for _, raw := range limitedOutlinks {
			linkCtxs = append(linkCtxs, frontier.LinkContext{
				URL:              raw,
				SourcePageTitle:  req.Title,
				SourceSchemaType: req.SchemaType,
				SourceLanguage:   req.Language,
			})
		}
		startingDepth := h.engine.Config().MaxDepth
		if err := h.frontier.AddBatch(r.Context(), linkCtxs, startingDepth, storage.PriorityDiscovery); err != nil {
			h.logger.Warn("ingest enqueue outlinks failed", "error", err)
		} else {
			outlinksQueued = len(linkCtxs)
		}
	}

	h.logger.Info("extension page ingested", "url", normalized, "page_id", pageID, "outlinks_queued", outlinksQueued)

	writeJSON(w, http.StatusOK, map[string]any{
		"indexed":         true,
		"page_id":         pageID,
		"url":             normalized,
		"outlinks_queued": outlinksQueued,
	})
}

// IngestBatch receives multiple pre-extracted pages in one request.
func (h *Handlers) IngestBatch(w http.ResponseWriter, r *http.Request) {
	var req storage.IngestBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Error("invalid request body in IngestBatch", "error", err)
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if len(req.Pages) == 0 {
		h.logger.Error("IngestBatch rejected: empty pages array")
		writeError(w, http.StatusBadRequest, "at least one page is required")
		return
	}
	if len(req.Pages) > 1000 {
		h.logger.Error("IngestBatch rejected: too many pages", "count", len(req.Pages))
		writeError(w, http.StatusBadRequest, "maximum 1000 pages per batch request")
		return
	}

	if h.frontier == nil {
		writeError(w, http.StatusInternalServerError, "frontier not available on this node")
		return
	}

	type result struct {
		URL            string `json:"url"`
		Indexed        bool   `json:"indexed"`
		PageID         int64  `json:"page_id,omitempty"`
		OutlinksQueued int    `json:"outlinks_queued,omitempty"`
		Error          string `json:"error,omitempty"`
	}

	results := make([]result, 0, len(req.Pages))
	processed := 0

	for _, p := range req.Pages {
		if p.URL == "" {
			results = append(results, result{URL: p.URL, Indexed: false, Error: "url is required"})
			continue
		}
		if isSearchResultsPageURL(p.URL) {
			results = append(results, result{URL: p.URL, Indexed: false, Error: "search_results_page"})
			continue
		}

		normalized, domain, err := frontier.CanonicalizeURL(p.URL)
		if err != nil {
			results = append(results, result{URL: p.URL, Indexed: false, Error: "invalid url"})
			continue
		}

		crawledAt := time.Now().UTC()
		if p.CrawledAt != "" {
			if t := parseIngestTime(p.CrawledAt); !t.IsZero() {
				crawledAt = t
			}
		}

		var publishedAt time.Time
		if p.PublishedAt != "" {
			publishedAt = parseIngestTime(p.PublishedAt)
		}

		page := &storage.Page{
			URL:               normalized,
			Domain:            domain,
			Title:             p.Title,
			H1:                p.H1,
			Description:       p.Description,
			Content:           p.Content,
			Language:          p.Language,
			StatusCode:        200,
			ContentHash:       fmt.Sprintf("%x", sha256.Sum256([]byte(p.Content))),
			URLFingerprint:    frontier.FingerprintURL(normalized),
			FetchMode:         "extension",
			RenderReason:      "submitted_by_extension",
			IngestHits:        1,
			LastSeenAt:        crawledAt,
			CrawledAt:         crawledAt,
			PublishedAt:       publishedAt,
			SchemaType:        p.SchemaType,
			SchemaTitle:       p.SchemaTitle,
			SchemaDescription: p.SchemaDescription,
			SchemaImage:       p.SchemaImage,
			SchemaAuthor:      p.SchemaAuthor,
			SchemaKeywords:    p.SchemaKeywords,
		}

		limitedOutlinks := clampIngestOutlinks(p.Outlinks)

		pageID, err := h.store.IngestPage(r.Context(), page, limitedOutlinks)
		if err != nil {
			h.logger.Warn("ingest batch page failed", "url", normalized, "error", err)
			results = append(results, result{URL: normalized, Indexed: false, Error: err.Error()})
			continue
		}

		outlinksQueued := 0
		if len(limitedOutlinks) > 0 {
			linkCtxs := make([]frontier.LinkContext, 0, len(limitedOutlinks))
			for _, raw := range limitedOutlinks {
				linkCtxs = append(linkCtxs, frontier.LinkContext{
					URL:              raw,
					SourcePageTitle:  p.Title,
					SourceSchemaType: p.SchemaType,
					SourceLanguage:   p.Language,
				})
			}
			startingDepth := h.engine.Config().MaxDepth
			if err := h.frontier.AddBatch(r.Context(), linkCtxs, startingDepth, storage.PriorityDiscovery); err != nil {
				h.logger.Warn("ingest batch enqueue outlinks failed", "error", err)
			} else {
				outlinksQueued = len(linkCtxs)
			}
		}

		processed++
		h.logger.Info("extension batch page ingested", "url", normalized, "page_id", pageID, "outlinks_queued", outlinksQueued)
		results = append(results, result{
			URL:            normalized,
			Indexed:        true,
			PageID:         pageID,
			OutlinksQueued: outlinksQueued,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"processed": processed,
		"failed":    len(req.Pages) - processed,
		"results":   results,
	})
}

// --- Maintenance ---

// UpdateRankings triggers an asynchronous recalculation of referring domains for all pages.
func (h *Handlers) UpdateRankings(w http.ResponseWriter, r *http.Request) {
	// Execute async to not block the HTTP response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := h.store.UpdatePageRankings(ctx); err != nil {
			h.logger.Error("failed to update page rankings", "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"message": "page ranking update initiated in background",
	})
}
