package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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

	results, total, err := h.store.SearchPages(r.Context(), query, limit, offset)
	if err != nil {
		h.logger.Error("search failed", "query", query, "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	if results == nil {
		results = []storage.SearchResult{}
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Query:   query,
		Total:   total,
		Page:    page,
		Limit:   limit,
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
}

// CrawlURLs enqueues one or more URLs for crawling.
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

	priority := req.Priority
	if priority <= 0 {
		priority = storage.PriorityNormal
	}

	if err := h.frontier.AddBatchDirect(r.Context(), req.URLs, 0, priority); err != nil {
		h.logger.Error("failed to enqueue URLs", "error", err, "count", len(req.URLs))
		writeError(w, http.StatusInternalServerError, "failed to enqueue URLs")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"message": "URLs enqueued for crawling",
		"count":   len(req.URLs),
	})
}

// --- Queue ---

// GetQueue returns the current crawl queue status.
func (h *Handlers) GetQueue(w http.ResponseWriter, r *http.Request) {
	stats := h.frontier.Stats()
	writeSuccess(w, stats)
}

// --- Seeds ---

type addSeedRequest struct {
	Domain   string `json:"domain"`
	Priority int    `json:"priority,omitempty"`
}

// ListSeeds returns all seed domains.
func (h *Handlers) ListSeeds(w http.ResponseWriter, r *http.Request) {
	seeds, err := h.store.GetSeedDomains(r.Context())
	if err != nil {
		h.logger.Error("failed to list seeds", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list seeds")
		return
	}

	if seeds == nil {
		seeds = []storage.Domain{}
	}

	writeSuccess(w, seeds)
}

// AddSeed adds a new seed domain.
func (h *Handlers) AddSeed(w http.ResponseWriter, r *http.Request) {
	var req addSeedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Domain == "" {
		writeError(w, http.StatusBadRequest, "domain is required")
		return
	}

	domain := &storage.Domain{
		Domain: req.Domain,
		IsSeed: true,
	}

	if err := h.store.UpsertDomain(r.Context(), domain); err != nil {
		h.logger.Error("failed to add seed", "domain", req.Domain, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add seed")
		return
	}

	// Also enqueue the domain's home page.
	priority := req.Priority
	if priority <= 0 {
		priority = storage.PrioritySeed
	}

	homeURL := "https://" + req.Domain + "/"
	if err := h.frontier.Add(r.Context(), homeURL, 0, priority); err != nil {
		h.logger.Warn("failed to enqueue seed home page", "url", homeURL, "error", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message": "seed domain added",
		"domain":  req.Domain,
	})
}

// DeleteSeed removes a seed domain.
func (h *Handlers) DeleteSeed(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	if domain == "" {
		writeError(w, http.StatusBadRequest, "domain is required")
		return
	}

	if err := h.store.DeleteDomain(r.Context(), domain); err != nil {
		h.logger.Error("failed to delete seed", "domain", domain, "error", err)
		writeError(w, http.StatusNotFound, "domain not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "seed domain removed",
		"domain":  domain,
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
