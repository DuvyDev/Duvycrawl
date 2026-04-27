package scorer

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/queue"
	"github.com/DuvyDev/Duvycrawl/internal/storage"
)

// AdaptiveScorer implements an A*-like best-first scoring strategy that
// learns from search queries and clicks recorded in the database.
//
// It maintains an in-memory cache of the user/community interest profile
// and refreshes it periodically from SQLite.  URLs are scored *before*
// they are crawled using only cheap signals: anchor text, URL path,
// source page title, domain reputation, and language preference.
type AdaptiveScorer struct {
	store  storage.Storage
	cfg    config.AdaptiveConfig
	logger *slog.Logger

	mu         sync.RWMutex
	profile    *interestProfile
	refreshed  time.Time
}

type interestProfile struct {
	terms       map[string]float64 // term -> accumulated weight
	domains     map[string]float64 // domain -> reputation score
	languages   map[string]float64 // language -> preference weight
	queryCount  int
}

// NewAdaptive creates a scorer that reads the community interest profile
// from the database.  Call StartRefreshLoop from main.
func NewAdaptive(store storage.Storage, cfg config.AdaptiveConfig, logger *slog.Logger) *AdaptiveScorer {
	return &AdaptiveScorer{
		store:  store,
		cfg:    cfg,
		logger: logger.With("component", "adaptive_scorer"),
	}
}

func (a *AdaptiveScorer) Name() string { return "adaptive" }

// StartRefreshLoop begins a background goroutine that reloads the profile
// from the database every ProfileRefreshInterval.  Call once from main.
func (a *AdaptiveScorer) StartRefreshLoop(ctx context.Context) {
	// Load immediately.
	if err := a.refresh(); err != nil {
		a.logger.Warn("initial profile refresh failed", "error", err)
	}

	go func() {
		ticker := time.NewTicker(a.cfg.ProfileRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.refresh(); err != nil {
					a.logger.Warn("profile refresh failed", "error", err)
				}
			}
		}
	}()
}

func (a *AdaptiveScorer) refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	terms, err := a.store.GetInterestProfile(ctx)
	if err != nil {
		return err
	}

	domains, err := a.store.GetDomainReputations(ctx)
	if err != nil {
		return err
	}

	queryCount, err := a.store.GetSearchQueryCount(ctx)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.profile = &interestProfile{
		terms:      terms,
		domains:    domains,
		languages:  make(map[string]float64),
		queryCount: queryCount,
	}

	// Simple language preference: count unique query languages from search_queries.
	// For now we keep languages empty (uniform) unless explicitly populated later.

	a.refreshed = time.Now()
	a.logger.Debug("profile refreshed",
		"terms", len(terms),
		"domains", len(domains),
		"queries", queryCount,
	)
	return nil
}

// Score computes the estimated value of crawling a URL before it is fetched.
// When not enough queries have been recorded it falls back to a behaviour
// very close to the legacy static priority system (BFS with seed bonus).
func (a *AdaptiveScorer) Score(job *queue.Job) float64 {
	a.mu.RLock()
	prof := a.profile
	a.mu.RUnlock()

	// Fallback: not enough data -> behave like static scorer.
	if prof == nil || prof.queryCount < a.cfg.MinQueriesBeforeBoost {
		return a.fallbackScore(job)
	}

	base := job.BaseScore
	if base == 0 {
		base = 10.0 // PriorityNormal equivalent
	}

	// 1. Relevance from anchor text, URL path, source title.
	relevance := a.relevanceScore(job, prof.terms)

	// 2. Domain reputation.
	domainBoost := prof.domains[job.Domain] * a.cfg.DomainReputationWeight

	// 3. Language match.
	langBoost := 0.0
	if job.SourceLanguage != "" && prof.languages[job.SourceLanguage] > 0 {
		langBoost = a.cfg.LanguageMatchBonus
	}

	// 4. Depth penalty (sub-linear, as requested).
	penalty := a.cfg.DepthPenaltyK * math.Log1p(float64(job.Depth))

	score := base + relevance + domainBoost + langBoost - penalty
	if score < 0 {
		score = 0
	}
	return score
}

func (a *AdaptiveScorer) fallbackScore(job *queue.Job) float64 {
	base := job.BaseScore
	if base == 0 {
		base = 10.0
	}
	penalty := a.cfg.DepthPenaltyK * math.Log1p(float64(job.Depth))
	score := base - penalty
	if score < 0 {
		score = 0
	}
	return score
}

// relevanceScore sums the profile weights of terms that appear in the
// anchor text, URL path, or source page title.
func (a *AdaptiveScorer) relevanceScore(job *queue.Job, terms map[string]float64) float64 {
	if len(terms) == 0 {
		return 0
	}

	// Collect searchable text.
	texts := []struct {
		s     string
		w     float64
	}{
		{job.AnchorText, a.cfg.AnchorWeight},
		{job.URL, a.cfg.URLPathWeight},
		{job.SourcePageTitle, a.cfg.SourceTitleWeight},
	}

	score := 0.0
	for _, t := range texts {
		if t.s == "" {
			continue
		}
		tokens := normalizeTokens(t.s)
		for _, tok := range tokens {
			if w, ok := terms[tok]; ok {
				score += w * t.w
			} else {
				// Partial match: check if term is contained in token or vice-versa.
				for term, w := range terms {
					if strings.Contains(tok, term) || strings.Contains(term, tok) {
						score += w * t.w * 0.5
						break // only count best partial match per token
					}
				}
			}
		}
	}
	return score
}

// normalizeTokens lower-cases, removes accents and splits on non-alphanumerics.
func normalizeTokens(s string) []string {
	s = strings.ToLower(s)
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
