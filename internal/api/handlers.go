package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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

// GetStats returns overall crawler statistics.
func (h *Handlers) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get stats")
		return
	}

	crawled, errored := h.engine.Stats()
	result := map[string]any{
		"stats":         stats,
		"engine_status": h.engine.Status(),
		"session": map[string]any{
			"pages_crawled": crawled,
			"pages_errored": errored,
		},
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

	var queued int
	if len(urlsToEnqueue) > 0 {
		var err error
		queued, err = h.frontier.AddBatchDirect(r.Context(), urlsToEnqueue, 0, baseScore)
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
	writeSuccess(w, stats)
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

	// Also enqueue the URL immediately.
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

// --- Embeddings ---

// GetEmbeddingStats returns statistics about the semantic embedding index.
func (h *Handlers) GetEmbeddingStats(w http.ResponseWriter, r *http.Request) {
	totalPages, embeddedPages, avgDimensions, model, err := h.store.GetEmbeddingStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get embedding stats", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get embedding stats")
		return
	}

	coverage := 0.0
	if totalPages > 0 {
		coverage = float64(embeddedPages) / float64(totalPages) * 100.0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_pages":    totalPages,
		"embedded_pages": embeddedPages,
		"coverage_pct":   roundFloat(coverage, 2),
		"avg_dimensions": avgDimensions,
		"model":          model,
	})
}

// roundFloat rounds a float64 to the given decimal places.
func roundFloat(val float64, decimals int) float64 {
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10.0
	}
	return float64(int(val*pow+0.5)) / pow
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
