package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/DuvyDev/Duvycrawl/internal/sitehints"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/transform"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type queryIntent string

const (
	intentNavigational  queryIntent = "navigational"
	intentInformational queryIntent = "informational"
)

type searchMode string

const (
	searchModeNavigational searchMode = "navigational"
	searchModeFTSPhrase    searchMode = "fts_phrase"
	searchModeFTSExact     searchMode = "fts_exact"
	searchModeFTSCore      searchMode = "fts_core"
	searchModeFTSRelaxed   searchMode = "fts_relaxed"
)

type searchQuery struct {
	raw            string
	lowered        string
	normalized     string
	tokens         []string // significant tokens (stopwords removed)
	ftsTokens      []string // stopword-filtered tokens for FTS queries
	navTerm        string
	domainLike     string
	intent         queryIntent
	idfMap         map[string]float64
	siteTypeIntent string
	platformIntent string
}

type searchCandidate struct {
	SearchResult
	H1              string
	H2              string
	BodyPreview     string
	sqlScore        float64
	contentLen      int
	mode            searchMode
	publishedAtTime time.Time
}

type searchDomainInfo struct {
	effectiveDomain string
	rootLabel       string
	isRootDomain    bool
}

// ---------------------------------------------------------------------------
// Main entry point
// ---------------------------------------------------------------------------

func (s *SQLiteStorage) SearchPages(ctx context.Context, query string, limit, offset int, lang string, domain string, schemaType string) ([]SearchResult, int, error) {
	q := s.newSearchQuery(query)
	if q.normalized == "" {
		return nil, 0, nil
	}

	// Cache Key: query + lang + domain + schemaType + offset + limit
	cacheKey := fmt.Sprintf("q:%s|l:%s|d:%s|s:%s|o:%d|lim:%d", q.normalized, lang, domain, schemaType, offset, limit)
	if s.cache != nil {
		if cached, ok := s.cache.Get(cacheKey); ok {
			if res, ok := cached.(struct {
				results []SearchResult
				total   int
			}); ok {
				return res.results, res.total, nil
			}
		}
	}

	searchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	q.idfMap, _ = s.getSearchIDFMap(searchCtx, q.tokens)

	// --- Phase 1: Retrieval ---
	candidates, total, err := s.retrieveSearchCandidates(searchCtx, q, lang, limit, offset)
	if err != nil {
		return nil, 0, err
	}

	// --- Phase 2: Scoring ---
	reranked := s.rerankSearchCandidates(candidates, q, lang)

	// --- Phase 3: Semantic re-ranking ---
	reranked = s.applySemanticReranking(searchCtx, reranked, q)

	// --- Phase 4: Post-processing ---
	if lang != "" {
		reranked = promoteLanguagePerDomain(reranked, lang, s.scoringCfg.SecondaryLanguage)
	}

	reranked = filterAndDiversifyCandidates(reranked, domain, schemaType)

	if domain != "" || schemaType != "" {
		total = len(reranked)
	} else if total < len(reranked) {
		total = len(reranked)
	}

	if offset >= len(reranked) {
		return []SearchResult{}, total, nil
	}

	end := min(offset+limit, len(reranked))

	// Load body previews for final results
	if err := s.loadResultBodies(searchCtx, reranked[offset:end]); err != nil {
		s.logger.Warn("failed to load result bodies", "error", err)
	}

	results := make([]SearchResult, 0, end-offset)
	for _, candidate := range reranked[offset:end] {
		results = append(results, candidate.SearchResult)
	}

	if s.cache != nil {
		s.cache.SetWithTTL(cacheKey, struct {
			results []SearchResult
			total   int
		}{results, total}, 10, 5*time.Minute)
	}

	return results, total, nil
}

func (s *SQLiteStorage) retrieveSearchCandidates(searchCtx context.Context, q searchQuery, lang string, limit int, offset int) ([]searchCandidate, int, error) {
	candidateLimit := searchCandidateLimit(limit, offset)

	var candidates []searchCandidate
	var total int

	plans := []struct {
		mode     searchMode
		ftsQuery string
	}{
		{mode: searchModeFTSPhrase, ftsQuery: buildFTSPhraseQuery(q.tokens)},
		{mode: searchModeFTSExact, ftsQuery: buildFTSExactQuery(q.tokens)},
		{mode: searchModeFTSCore, ftsQuery: buildFTSCoreQuery(q)},
		{mode: searchModeFTSRelaxed, ftsQuery: buildFTSRelaxedQuery(q)},
	}

	const perPlanTimeout = 2500 * time.Millisecond

	for _, plan := range plans {
		if searchCtx.Err() != nil {
			break
		}
		if plan.ftsQuery == "" {
			continue
		}

		planCtx, planCancel := context.WithTimeout(searchCtx, perPlanTimeout)

		count, err := s.countFTSCandidates(planCtx, plan.ftsQuery)
		if err != nil {
			planCancel()
			if searchCtx.Err() != nil {
				break
			}
			if planCtx.Err() != nil {
				s.logger.Debug("FTS plan timed out, skipping", "mode", plan.mode, "query", q.raw)
				continue
			}
			return nil, 0, fmt.Errorf("counting %s results for %q: %w", plan.mode, q.raw, err)
		}
		if count == 0 {
			planCancel()
			continue
		}

		results, err := s.searchFTSCandidates(planCtx, plan.mode, plan.ftsQuery, q, lang, candidateLimit)
		planCancel()
		if err != nil {
			if searchCtx.Err() != nil {
				break
			}
			if planCtx.Err() != nil {
				s.logger.Debug("FTS plan timed out during search, skipping", "mode", plan.mode, "query", q.raw)
				continue
			}
			return nil, 0, fmt.Errorf("searching %s candidates for %q: %w", plan.mode, q.raw, err)
		}
		if len(results) == 0 {
			continue
		}

		candidates = mergeSearchCandidates(candidates, results)
		total += count

		// For navigational queries, early exit once we have enough.
		if q.intent == intentNavigational && len(candidates) >= candidateLimit {
			break
		}
	}

	// --- Navigational fallback ---
	if q.intent == intentNavigational && len(candidates) < 30 {
		navCandidates, err := s.searchNavigationalCandidates(searchCtx, q, lang, min(candidateLimit, 60))
		if err == nil {
			candidates = mergeSearchCandidates(candidates, navCandidates)
		}
	}

	// --- Platform diversity injection ---
	if q.platformIntent != "" && searchCtx.Err() == nil {
		diversityLimit := min(candidateLimit/3, 100)
		if diversityLimit < 30 {
			diversityLimit = 30
		}

		// Use the best FTS query that produced results
		bestFTS := ""
		var bestMode searchMode
		for _, plan := range plans {
			if plan.ftsQuery != "" {
				bestFTS = plan.ftsQuery
				bestMode = plan.mode
				break
			}
		}

		if bestFTS != "" {
			platformDomain := q.platformIntent + ".com"
			if q.domainLike != "" {
				platformDomain = q.domainLike
			}

			diverseCtx, diverseCancel := context.WithTimeout(searchCtx, perPlanTimeout)
			diverseCandidates, err := s.searchFTSCandidatesExcludingDomain(diverseCtx, bestMode, bestFTS, q, lang, diversityLimit, platformDomain)
			diverseCancel()
			if err == nil && len(diverseCandidates) > 0 {
				candidates = mergeSearchCandidates(candidates, diverseCandidates)
			}
		}
	}

	// --- Domain-match retrieval for informational queries ---
	if q.intent == intentInformational && len(q.tokens) >= 2 {
		domainCandidates, err := s.searchDomainMatchCandidates(searchCtx, q, lang, min(candidateLimit, 30))
		if err == nil {
			candidates = mergeSearchCandidates(candidates, domainCandidates)
		}
	}

	if err := searchCtx.Err(); err != nil && len(candidates) == 0 {
		return nil, 0, fmt.Errorf("search timed out before collecting candidates for %q: %w", q.raw, err)
	}

	return candidates, total, nil
}

func (s *SQLiteStorage) applySemanticReranking(searchCtx context.Context, reranked []searchCandidate, q searchQuery) []searchCandidate {
	if len(reranked) == 0 || s.embedder == nil {
		return reranked
	}

	semanticCtx, semanticCancel := context.WithTimeout(searchCtx, 1200*time.Millisecond)
	queryEmbedding, err := s.embedder.GenerateEmbeddingContext(semanticCtx, q.raw)
	semanticCancel()
	if err == nil && len(queryEmbedding) > 0 {
		topN := min(len(reranked), 100)
		pageIDs := make([]int64, 0, topN)
		for i := 0; i < topN; i++ {
			pageIDs = append(pageIDs, reranked[i].ID)
		}

		embs, err := s.GetPageEmbeddings(searchCtx, pageIDs)
		if err == nil && len(embs) > 0 {
			for i := 0; i < topN; i++ {
				emb, ok := embs[reranked[i].ID]
				if !ok || len(emb.Embedding) == 0 {
					continue
				}
				sim := cosineSimilarity(queryEmbedding, emb.Embedding)
				semanticScore := sim * 10000.0
				oldRank := reranked[i].Rank
				reranked[i].Rank = oldRank*0.75 + semanticScore*0.25
			}
			sort.SliceStable(reranked, func(i, j int) bool {
				return reranked[i].Rank > reranked[j].Rank
			})
		}
	}
	return reranked
}

func filterAndDiversifyCandidates(reranked []searchCandidate, domain string, schemaType string) []searchCandidate {
	if len(reranked) == 0 {
		return reranked
	}

	// Domain filter
	if domain != "" {
		filtered := make([]searchCandidate, 0, len(reranked))
		for _, c := range reranked {
			if strings.EqualFold(c.Domain, domain) {
				filtered = append(filtered, c)
			}
		}
		reranked = filtered
	}

	// Schema type filter
	if schemaType != "" {
		filtered := make([]searchCandidate, 0, len(reranked))
		for _, c := range reranked {
			if strings.EqualFold(c.SchemaType, schemaType) {
				filtered = append(filtered, c)
			}
		}
		reranked = filtered
	}

	// Domain diversity (only when not searching within a single domain)
	if domain == "" {
		reranked = diversifyByDomain(reranked)
	}

	return reranked
}

// ---------------------------------------------------------------------------
// Query parsing & intent classification
// ---------------------------------------------------------------------------

func (s *SQLiteStorage) newSearchQuery(query string) searchQuery {
	normalized := normalizeSearchText(query)
	allTokens := strings.Fields(normalized)
	domainLike := normalizeDomainLikeQuery(query)

	// Implicit gTLD detection
	if domainLike == "" && len(allTokens) == 2 {
		candidate := allTokens[0] + "." + allTokens[1]
		if etld, err := publicsuffix.EffectiveTLDPlusOne(candidate); err == nil && etld == candidate {
			domainLike = candidate
		}
	}

	navTerm := ""
	if domainLike != "" {
		navTerm = strings.ReplaceAll(normalizeSearchText(classifySearchDomain(domainLike).rootLabel), " ", "")
	}

	// Detect site-type and platform intent
	var foundPlatforms []string
	var foundSiteTypes []string

	isEdge := func(i int) bool {
		return i == 0 || i == len(allTokens)-1
	}

	for i, tok := range allTokens {
		if s.platformDomains != nil {
			if _, ok := s.platformDomains[tok]; ok && isEdge(i) {
				foundPlatforms = append(foundPlatforms, tok)
				continue
			}
		}
		if s.siteTypes != nil {
			if _, ok := s.siteTypes[tok]; ok && isEdge(i) {
				foundSiteTypes = append(foundSiteTypes, tok)
			}
		}
	}

	platformIntent := ""
	if len(foundPlatforms) == 1 {
		platformIntent = foundPlatforms[0]
	}

	siteTypeIntent := ""
	if len(foundSiteTypes) == 1 && platformIntent == "" {
		siteTypeIntent = foundSiteTypes[0]
	}

	// Platform names act as navigational terms
	if platformIntent != "" {
		navTerm = platformIntent
		if domainLike != "" && strings.HasSuffix(domainLike, "."+platformIntent) {
			domainLike = ""
		}
	}

	// Filter stopwords — tokens used for scoring are stopword-filtered.
	// Context-aware: when the query contains media keywords (serie, tv, pelicula,
	// film, movie, show, anime, manga), preserve short stopwords that could be
	// titles (e.g. "from" in "from serie tv" — the TV series "From").
	mediaContext := false
	mediaKeywords := map[string]struct{}{
		"serie": {}, "series": {}, "tv": {}, "pelicula": {}, "peliculas": {},
		"film": {}, "films": {}, "movie": {}, "movies": {}, "show": {}, "shows": {},
		"anime": {}, "manga": {}, "dorama": {}, "novela": {}, "novelas": {},
		"documental": {}, "documentales": {}, "documentary": {},
	}
	for _, tok := range allTokens {
		if _, ok := mediaKeywords[tok]; ok {
			mediaContext = true
			break
		}
	}
	allTokensFiltered := filterStopwordsContext(allTokens, mediaContext)

	// Filter out the platform and site-type names from search tokens.
	// The platform name is used for domain filtering and boost, not for
	// content matching (e.g. "warframe reddit" → search for "warframe"
	// within reddit.com, not for pages containing the word "reddit").
	searchTokens := make([]string, 0, len(allTokensFiltered))
	for _, tok := range allTokensFiltered {
		if tok == platformIntent {
			continue
		}
		if tok == siteTypeIntent {
			continue
		}
		searchTokens = append(searchTokens, tok)
	}
	if len(searchTokens) == 0 {
		// Query was only a platform or site-type word (e.g. "reddit" or "wiki")
		searchTokens = allTokensFiltered
	}

	// Compound word splitting: "tiendainglesa" → "tienda" + "inglesa"
	// For tokens ≥ 7 chars, try all 2-way splits and validate against FTS vocab.
	searchTokens = s.splitCompoundTokens(searchTokens)

	// Intent classification
	intent := classifyQueryIntent(query, searchTokens, domainLike, navTerm, platformIntent)

	return searchQuery{
		raw:            query,
		lowered:        strings.ToLower(strings.TrimSpace(query)),
		normalized:     normalized,
		tokens:         searchTokens,
		ftsTokens:      searchTokens,
		navTerm:        navTerm,
		domainLike:     domainLike,
		intent:         intent,
		siteTypeIntent: siteTypeIntent,
		platformIntent: platformIntent,
	}
}

// classifyQueryIntent determines whether a query is navigational or informational.
// Navigational: user wants a specific website (1-2 tokens, matches a known domain/brand).
// Informational: user wants an answer/topic (3+ tokens, question-like, or topic phrases).
func classifyQueryIntent(query string, tokens []string, domainLike, navTerm, platformIntent string) queryIntent {
	// Explicit domain-like query → navigational
	if domainLike != "" {
		return intentNavigational
	}

	// Platform intent → navigational (user wants that platform)
	if platformIntent != "" {
		return intentNavigational
	}

	// Single token → likely navigational (brand name, domain root)
	if len(tokens) == 1 {
		return intentNavigational
	}

	// 2 tokens with a navTerm → navigational
	if len(tokens) == 2 && navTerm != "" {
		return intentNavigational
	}

	// 3+ tokens → informational (user is asking a question or describing a topic)
	if len(tokens) >= 3 {
		return intentInformational
	}

	// Default to informational for anything ambiguous
	return intentInformational
}

func searchCandidateLimit(limit, offset int) int {
	want := offset + (limit * 8) + 30
	// BM25 ordering can saturate the pool with a single domain's
	// boilerplate-heavy pages (e.g. Reddit templates). A higher floor
	// ensures the Go re-ranking pipeline always has enough diversity.
	if want < 300 {
		want = 300
	}
	if want > 500 {
		want = 500
	}
	return want
}

// ---------------------------------------------------------------------------
// IDF computation
// ---------------------------------------------------------------------------

func (s *SQLiteStorage) getSearchIDFMap(ctx context.Context, tokens []string) (map[string]float64, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	var totalDocs float64
	if err := s.searchContentDB.QueryRowContext(ctx, `SELECT MAX(rowid) FROM pages_fts`).Scan(&totalDocs); err != nil {
		totalDocs = 10000.0
	}
	if totalDocs <= 0 {
		totalDocs = 1.0
	}

	idfMap := make(map[string]float64, len(tokens))
	defaultIDF := math.Log(totalDocs / 1.0)

	var missingTokens []string

	for _, t := range tokens {
		if s.cache != nil {
			if cachedIDF, ok := s.cache.Get("idf:" + t); ok {
				idfMap[t] = cachedIDF.(float64)
				continue
			}
		}
		missingTokens = append(missingTokens, t)
	}

	if len(missingTokens) == 0 {
		return idfMap, nil
	}

	for _, t := range missingTokens {
		idfMap[t] = defaultIDF
	}

	placeholders := make([]string, len(missingTokens))
	args := make([]any, len(missingTokens))
	for i, t := range missingTokens {
		placeholders[i] = "?"
		args[i] = t
	}

	query := fmt.Sprintf(`SELECT term, doc FROM pages_fts_vocab WHERE term IN (%s)`, strings.Join(placeholders, ","))
	rows, err := s.searchContentDB.QueryContext(ctx, query, args...)
	if err != nil {
		s.logger.Warn("failed to fetch IDF map", "error", err)
		return idfMap, nil
	}
	defer rows.Close()

	for rows.Next() {
		var term string
		var docCount float64
		if err := rows.Scan(&term, &docCount); err == nil && docCount > 0 {
			val := math.Log(totalDocs / docCount)
			idfMap[term] = val
			if s.cache != nil {
				s.cache.SetWithTTL("idf:"+term, val, 1, 24*time.Hour)
			}
		}
	}

	// Cache default IDF for tokens that weren't found to avoid re-querying
	for _, t := range missingTokens {
		if val, ok := idfMap[t]; ok && val == defaultIDF {
			if s.cache != nil {
				s.cache.SetWithTTL("idf:"+t, val, 1, 24*time.Hour)
			}
		}
	}

	return idfMap, nil
}

// ---------------------------------------------------------------------------
// FTS retrieval
// ---------------------------------------------------------------------------

func (s *SQLiteStorage) countFTSCandidates(ctx context.Context, ftsQuery string) (int, error) {
	var total int
	if err := s.searchContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_fts WHERE pages_fts MATCH ?`, ftsQuery).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *SQLiteStorage) searchFTSCandidates(ctx context.Context, mode searchMode, matchQuery string, query searchQuery, lang string, limit int) ([]searchCandidate, error) {
	domainExact := query.domainLike
	if domainExact == "" {
		domainExact = query.navTerm
	}

	domainPrefix := ""
	if domainExact != "" {
		domainPrefix = domainExact + ".%"
	}

	// BM25 column weights for pages_fts:
	//   title=10, h1=8, description=5, schema_title=6, schema_description=4, content=1
	// bm25() returns negative values; more negative = more relevant.
	// We negate it to produce a positive score for the Go re-ranking pipeline.
	//
	// Only a domain boost is applied at SQL level to ensure navigational targets
	// survive the LIMIT cutoff. All other scoring (title, URL, homepage, language,
	// freshness) is handled by the Go re-ranking pipeline — duplicating it here
	// would add expensive per-row LOWER/LIKE scans with no benefit.
	searchSQL := `
		SELECT
			p.id, p.url, p.title, p.h1, p.h2, p.description,
			'' AS snippet, p.domain, p.language, p.region,
			p.crawled_at, p.published_at, p.updated_at,
			(-bm25(pages_fts, 10.0, 8.0, 5.0, 6.0, 4.0, 1.0)) AS bm25_score,
			LENGTH(p.content) AS content_len,
			p.schema_type, p.schema_image, p.schema_author,
			p.schema_keywords, p.schema_rating,
			p.referring_domains, COALESCE(p.pagerank, 1.0) AS pagerank,
			COALESCE(sc.clicks, 0) AS clicks
		FROM pages_fts
		JOIN pages p ON p.id = pages_fts.rowid
		LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
		WHERE pages_fts MATCH ?
		ORDER BY (
			bm25(pages_fts, 10.0, 8.0, 5.0, 6.0, 4.0, 1.0)
			- CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 30.0 ELSE 0.0 END
			- CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 15.0 ELSE 0.0 END
		) ASC
		LIMIT ?
	`
	args := []any{query.raw, matchQuery, domainExact, domainExact, domainPrefix, domainPrefix, limit}

	rows, err := s.searchContentDB.QueryContext(ctx, searchSQL, args...)
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
			snippetText    sql.NullString
			clicksCount    int
			schemaRating   sql.NullFloat64
			schemaType     sql.NullString
			schemaImage    sql.NullString
			schemaAuthor   sql.NullString
			schemaKeywords sql.NullString
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.URL, &candidate.Title, &candidate.H1, &candidate.H2,
			&candidate.Description, &snippetText, &candidate.Domain, &candidate.Language,
			&candidate.Region, &crawledAt, &publishedAtStr, &updatedAtStr, &candidate.sqlScore,
			&candidate.contentLen, &schemaType, &schemaImage, &schemaAuthor, &schemaKeywords,
			&schemaRating, &candidate.ReferringDomains, &candidate.PageRank, &clicksCount,
		); err != nil {
			return nil, fmt.Errorf("scanning FTS candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 50.0

		candidate.Snippet = snippetText.String
		if candidate.Snippet == "" {
			candidate.Snippet = candidate.Description
		}
		candidate.mode = mode
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
		if schemaType.Valid {
			candidate.SchemaType = schemaType.String
		}
		if schemaImage.Valid {
			candidate.SchemaImage = schemaImage.String
		}
		if schemaAuthor.Valid {
			candidate.SchemaAuthor = schemaAuthor.String
		}
		if schemaKeywords.Valid {
			candidate.SchemaKeywords = schemaKeywords.String
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating FTS candidates: %w", err)
	}

	return candidates, nil
}

// searchFTSCandidatesExcludingDomain performs the same FTS retrieval as searchFTSCandidates
// but excludes results from a specific domain (and its subdomains). This is used to inject
// diversity into the candidate pool when a platform domain monopolizes BM25 results.
func (s *SQLiteStorage) searchFTSCandidatesExcludingDomain(ctx context.Context, mode searchMode, matchQuery string, query searchQuery, lang string, limit int, excludeDomain string) ([]searchCandidate, error) {
	excludeLike := "%" + excludeDomain

	searchSQL := `
		SELECT
			p.id, p.url, p.title, p.h1, p.h2, p.description,
			'' AS snippet, p.domain, p.language, p.region,
			p.crawled_at, p.published_at, p.updated_at,
			(-bm25(pages_fts, 10.0, 8.0, 5.0, 6.0, 4.0, 1.0)) AS bm25_score,
			LENGTH(p.content) AS content_len,
			p.schema_type, p.schema_image, p.schema_author,
			p.schema_keywords, p.schema_rating,
			p.referring_domains, COALESCE(p.pagerank, 1.0) AS pagerank,
			COALESCE(sc.clicks, 0) AS clicks
		FROM pages_fts
		JOIN pages p ON p.id = pages_fts.rowid
		LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
		WHERE pages_fts MATCH ?
		  AND LOWER(p.domain) != ?
		  AND LOWER(p.domain) NOT LIKE ?
		ORDER BY bm25(pages_fts, 10.0, 8.0, 5.0, 6.0, 4.0, 1.0) ASC
		LIMIT ?
	`
	args := []any{query.raw, matchQuery, excludeDomain, excludeLike, limit}

	rows, err := s.searchContentDB.QueryContext(ctx, searchSQL, args...)
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
			snippetText    sql.NullString
			clicksCount    int
			schemaRating   sql.NullFloat64
			schemaType     sql.NullString
			schemaImage    sql.NullString
			schemaAuthor   sql.NullString
			schemaKeywords sql.NullString
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.URL, &candidate.Title, &candidate.H1, &candidate.H2,
			&candidate.Description, &snippetText, &candidate.Domain, &candidate.Language,
			&candidate.Region, &crawledAt, &publishedAtStr, &updatedAtStr, &candidate.sqlScore,
			&candidate.contentLen, &schemaType, &schemaImage, &schemaAuthor, &schemaKeywords,
			&schemaRating, &candidate.ReferringDomains, &candidate.PageRank, &clicksCount,
		); err != nil {
			return nil, fmt.Errorf("scanning FTS diversity candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 50.0

		candidate.Snippet = snippetText.String
		if candidate.Snippet == "" {
			candidate.Snippet = candidate.Description
		}
		candidate.mode = mode
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
		if schemaType.Valid {
			candidate.SchemaType = schemaType.String
		}
		if schemaImage.Valid {
			candidate.SchemaImage = schemaImage.String
		}
		if schemaAuthor.Valid {
			candidate.SchemaAuthor = schemaAuthor.String
		}
		if schemaKeywords.Valid {
			candidate.SchemaKeywords = schemaKeywords.String
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating FTS diversity candidates: %w", err)
	}

	return candidates, nil
}

// buildSQLScoreExpr builds the SQL scoring expression based on query intent.
// For navigational queries: strong domain/homepage signals.
// For informational queries: content matching signals, URL path relevance, minimal structural boosts.
func buildSQLScoreExpr(query searchQuery, domainExact, domainPrefix, titleExact, titlePrefix, titleContains, lang string) string {
	base := "0.0"

	// Title matching — strong signal for navigational, moderate for informational
	if query.intent == intentNavigational {
		base += " + CASE WHEN LOWER(p.title) = ? THEN 2000.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.title) LIKE ? THEN 1200.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.title) LIKE ? THEN 800.0 ELSE 0 END"
	} else {
		base += " + CASE WHEN LOWER(p.title) = ? THEN 500.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.title) LIKE ? THEN 300.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.title) LIKE ? THEN 180.0 ELSE 0 END"
	}

	if query.intent == intentNavigational {
		// Navigational: strong domain and homepage signals
		base += " + CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 1500.0 ELSE 0 END"
		base += " + CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 800.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.url) = 'https://' || LOWER(p.domain) || '/' THEN 2000.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.url) GLOB LOWER('https://' || p.domain || '/[a-z][a-z]') THEN 1200.0 ELSE 0 END"
	} else {
		// Informational: moderate domain signal, no massive homepage boost
		base += " + CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 100.0 ELSE 0 END"
		base += " + CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 50.0 ELSE 0 END"
		base += " + CASE WHEN LOWER(p.domain) LIKE '%.gub.%' OR LOWER(p.domain) LIKE '%.gov.%' THEN 80.0 ELSE 0 END"
	}

	base += " + CASE WHEN LOWER(p.url) LIKE ? THEN 100.0 ELSE 0 END"

	// Freshness (same for both)
	base += " + MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))"

	return base
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

	navScoreExpr := fmt.Sprintf(`
		(
			CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 1500.0 ELSE 0 END
			+ CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 800.0 ELSE 0 END
			+ CASE WHEN LOWER(p.url) = 'https://' || LOWER(p.domain) || '/' THEN 2000.0 ELSE 0 END
			+ CASE WHEN LOWER(p.url) GLOB LOWER('https://' || p.domain || '/[a-z][a-z]') THEN 1200.0 ELSE 0 END
			+ CASE WHEN LOWER(p.url) LIKE ? THEN 220.0 ELSE 0 END
			+ CASE WHEN LOWER(p.title) LIKE ? THEN 160.0 ELSE 0 END
			+ CASE WHEN LOWER(p.h1) LIKE ? THEN 120.0 ELSE 0 END
			+ CASE WHEN LOWER(p.h2) LIKE ? THEN 100.0 ELSE 0 END
			+ CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 3 THEN 500.0 ELSE 0 END
			+ MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))
			+ CASE WHEN ? != '' AND p.language = ? THEN %.1f ELSE 0 END
		) AS sql_score`, s.scoringCfg.LanguageBoost*0.1)

	querySQL := fmt.Sprintf(`SELECT
		p.id, p.url, p.title, p.h1, p.h2, p.description,
		CASE WHEN p.description != '' THEN SUBSTR(p.description, 1, 240) ELSE '' END AS snippet,
		p.domain, p.language, p.region, p.crawled_at, p.published_at, p.updated_at,
		%s, LENGTH(p.content) AS content_len,
		p.schema_type, p.schema_image, p.schema_author, p.schema_keywords,
		p.schema_rating, p.referring_domains, COALESCE(p.pagerank, 1.0) AS pagerank,
		COALESCE(sc.clicks, 0) AS clicks
	FROM pages p
	LEFT JOIN search_clicks sc ON sc.url = p.url AND sc.query = ?
	WHERE
		(? != '' AND LOWER(p.domain) = ?)
		OR (? != '' AND LOWER(p.domain) LIKE ?)
		OR LOWER(p.url) LIKE ?
		OR LOWER(p.title) LIKE ?
		OR LOWER(p.h1) LIKE ?
		OR LOWER(p.h2) LIKE ?
	ORDER BY (sql_score + COALESCE(sc.clicks, 0) * 50.0) DESC, p.crawled_at DESC
	LIMIT ?
	`, navScoreExpr)

	args := []any{
		domainExact, domainExact, navTerm, navLike, navLike,
		titleLike, titleLike, titleLike, lang, lang,
		query.raw, domainExact, domainExact, navTerm, navLike, navLike,
		titleLike, titleLike, titleLike, limit,
	}

	rows, err := s.searchContentDB.QueryContext(ctx, querySQL, args...)
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
			snippetText    sql.NullString
			clicksCount    int
			schemaRating   sql.NullFloat64
			schemaType     sql.NullString
			schemaImage    sql.NullString
			schemaAuthor   sql.NullString
			schemaKeywords sql.NullString
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.URL, &candidate.Title, &candidate.H1, &candidate.H2,
			&candidate.Description, &snippetText, &candidate.Domain, &candidate.Language,
			&candidate.Region, &crawledAt, &publishedAtStr, &updatedAtStr, &candidate.sqlScore,
			&candidate.contentLen, &schemaType, &schemaImage, &schemaAuthor, &schemaKeywords,
			&schemaRating, &candidate.ReferringDomains, &candidate.PageRank, &clicksCount,
		); err != nil {
			return nil, fmt.Errorf("scanning navigational candidate: %w", err)
		}
		candidate.sqlScore += float64(clicksCount) * 50.0
		candidate.Snippet = snippetText.String
		candidate.mode = searchModeNavigational
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
		if schemaType.Valid {
			candidate.SchemaType = schemaType.String
		}
		if schemaImage.Valid {
			candidate.SchemaImage = schemaImage.String
		}
		if schemaAuthor.Valid {
			candidate.SchemaAuthor = schemaAuthor.String
		}
		if schemaKeywords.Valid {
			candidate.SchemaKeywords = schemaKeywords.String
		}
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating navigational candidates: %w", err)
	}

	return candidates, nil
}

// searchDomainMatchCandidates finds pages whose domain root label contains query tokens.
// This ensures domain-matching pages (like tiendainglesa.com.uy for "tienda inglesa")
// are in the candidate set even if the FTS AND query didn't match them.
func (s *SQLiteStorage) searchDomainMatchCandidates(ctx context.Context, query searchQuery, lang string, limit int) ([]searchCandidate, error) {
	if limit <= 0 || len(query.tokens) < 2 {
		return nil, nil
	}

	// Build CASE expressions to count how many tokens match the domain.
	// Include siteTypeIntent token (e.g. "tienda") since it was removed from
	// query.tokens but is part of the brand/domain name we're looking for.
	domainMatchTokens := make([]string, 0, len(query.tokens)+1)
	domainMatchTokens = append(domainMatchTokens, query.tokens...)
	if query.siteTypeIntent != "" && len(query.siteTypeIntent) >= 3 {
		domainMatchTokens = append(domainMatchTokens, query.siteTypeIntent)
	}

	var caseClauses []string
	var args []any
	for _, token := range domainMatchTokens {
		if len(token) >= 3 {
			caseClauses = append(caseClauses, "CASE WHEN LOWER(p.domain) LIKE ? THEN 1 ELSE 0 END")
			args = append(args, "%"+token+"%")
		}
	}
	if len(caseClauses) < 2 {
		return nil, nil
	}

	// Require at least 2 tokens to match the domain
	matchCountExpr := "(" + strings.Join(caseClauses, " + ") + ")"
	whereExpr := matchCountExpr + " >= 2"

	// Use a subquery to avoid duplicating CASE expressions and their args
	querySQL := fmt.Sprintf(`SELECT id, url, title, h1, h2, description, snippet, domain, language, region, crawled_at, published_at, updated_at, sql_score, content_len, schema_type, schema_image, schema_author, schema_keywords, schema_rating, referring_domains, pagerank, clicks
FROM (
	SELECT p.id, p.url, p.title, p.h1, p.h2, p.description,
		CASE WHEN p.description != '' THEN SUBSTR(p.description, 1, 240) ELSE '' END AS snippet,
		p.domain, p.language, p.region, p.crawled_at, p.published_at, p.updated_at,
		500.0 AS sql_score, LENGTH(p.content) AS content_len,
		p.schema_type, p.schema_image, p.schema_author, p.schema_keywords,
		p.schema_rating, p.referring_domains, COALESCE(p.pagerank, 1.0) AS pagerank,
		0 AS clicks,
		%s AS match_count
	FROM pages p
	WHERE %s
)
ORDER BY match_count DESC, crawled_at DESC
LIMIT ?`, matchCountExpr, whereExpr)

	// Duplicate args: matchCountExpr appears in both SELECT and WHERE inside the subquery
	doubledArgs := make([]any, 0, len(args)*2+1)
	doubledArgs = append(doubledArgs, args...) // for SELECT match_count
	doubledArgs = append(doubledArgs, args...) // for WHERE match_count >= 2
	doubledArgs = append(doubledArgs, limit)

	rows, err := s.searchContentDB.QueryContext(ctx, querySQL, doubledArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying domain-match candidates: %w", err)
	}
	defer rows.Close()

	var candidates []searchCandidate
	for rows.Next() {
		var (
			candidate      searchCandidate
			crawledAt      sql.NullTime
			publishedAtStr sql.NullString
			updatedAtStr   sql.NullString
			snippetText    sql.NullString
			schemaRating   sql.NullFloat64
			schemaType     sql.NullString
			schemaImage    sql.NullString
			schemaAuthor   sql.NullString
			schemaKeywords sql.NullString
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.URL, &candidate.Title, &candidate.H1, &candidate.H2,
			&candidate.Description, &snippetText, &candidate.Domain, &candidate.Language,
			&candidate.Region, &crawledAt, &publishedAtStr, &updatedAtStr, &candidate.sqlScore,
			&candidate.contentLen, &schemaType, &schemaImage, &schemaAuthor, &schemaKeywords,
			&schemaRating, &candidate.ReferringDomains, &candidate.PageRank, new(int),
		); err != nil {
			return nil, fmt.Errorf("scanning domain-match candidate: %w", err)
		}
		candidate.Snippet = snippetText.String
		candidate.mode = searchModeFTSRelaxed
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
		if schemaType.Valid {
			candidate.SchemaType = schemaType.String
		}
		if schemaImage.Valid {
			candidate.SchemaImage = schemaImage.String
		}
		if schemaAuthor.Valid {
			candidate.SchemaAuthor = schemaAuthor.String
		}
		if schemaKeywords.Valid {
			candidate.SchemaKeywords = schemaKeywords.String
		}
		candidates = append(candidates, candidate)
	}

	return candidates, rows.Err()
}

// ---------------------------------------------------------------------------
// Go-side re-ranking (the core scoring engine)
// ---------------------------------------------------------------------------

func (s *SQLiteStorage) rerankSearchCandidates(candidates []searchCandidate, query searchQuery, lang string) []searchCandidate {
	reranked := make([]searchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		score, keep := s.scoreSearchCandidate(candidate, query, lang)
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

func (s *SQLiteStorage) scoreSearchCandidate(candidate searchCandidate, query searchQuery, lang string) (float64, bool) {
	// Normalize fields
	titleNorm := normalizeSearchText(candidate.Title)
	h1Norm := normalizeSearchText(candidate.H1)
	h2Norm := normalizeSearchText(candidate.H2)
	descNorm := normalizeSearchText(candidate.Description)
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

	// Tokenize fields
	titleTokens := uniqueStrings(strings.Fields(titleNorm))
	h1Tokens := uniqueStrings(strings.Fields(h1Norm))
	h2Tokens := uniqueStrings(strings.Fields(h2Norm))
	descTokens := uniqueStrings(strings.Fields(descNorm))
	urlTokens := uniqueStrings(strings.Fields(urlNorm))
	domainTokens := uniqueStrings(append(strings.Fields(domainNorm), strings.Fields(effectiveDomainNorm)...))
	domainTokens = uniqueStrings(append(domainTokens, strings.Fields(rootLabelNorm)...))

	// --- Component 1: Content Relevance ---
	contentScore := scoreContentRelevance(query, titleNorm, titleTokens, h1Norm, h1Tokens, h2Norm, h2Tokens, descNorm, descTokens, urlNorm, urlTokens, domainNorm, domainTokens, effectiveDomainNorm, rootLabelNorm, urlDomain)

	// --- Component 2: Authority (saturated PageRank) ---
	authorityScore := scoreAuthority(candidate.PageRank, candidate.ReferringDomains)

	// --- Component 3: URL/Domain Signal (intent-aware) ---
	urlScore := scoreURLSignal(query, candidate.URL, domainInfo, urlDomain)

	// --- Component 4: Freshness ---
	freshnessScore := scoreFreshness(candidate.CrawledAt, candidate.publishedAtTime)

	// --- Component 5: User Signals (capped clicks) ---
	userScore := scoreUserSignals(candidate.sqlScore)

	// --- Intent-aware weighting ---
	var totalScore float64
	if query.intent == intentNavigational {
		// Navigational: domain/homepage signals matter most
		totalScore = contentScore*0.30 + authorityScore*0.20 + urlScore*0.35 + freshnessScore*0.05 + userScore*0.10
	} else {
		// Informational: content relevance is king
		totalScore = contentScore*0.50 + authorityScore*0.15 + urlScore*0.10 + freshnessScore*0.15 + userScore*0.10
	}

	// --- Add BM25 score as a baseline ---
	// Native bm25 scores (negated) range ~5-16; scale to match other components.
	// Old SQL CASE/LIKE scoring produced ~1000-3000 for good matches.
	// BM25 of ~10 * 200 * 0.20 = ~400, comparable to old contribution.
	totalScore += candidate.sqlScore * 200.0 * 0.20

	// --- Mode bonus ---
	totalScore += searchModeBonus(candidate.mode)

	// --- Language boost ---
	if lang != "" && candidate.Language == lang {
		totalScore += s.scoringCfg.LanguageBoost
	} else if lang != "" && s.scoringCfg.SecondaryLanguageBoost > 0 && lang != s.scoringCfg.SecondaryLanguage && candidate.Language == s.scoringCfg.SecondaryLanguage {
		totalScore += s.scoringCfg.SecondaryLanguageBoost
	}

	isHomepage := isSearchHomepage(candidate.URL)
	urlLower := strings.ToLower(candidate.URL)
	isProfilePage := strings.Contains(urlLower, "/user/") ||
		strings.Contains(urlLower, "/profile/") ||
		strings.Contains(urlLower, "/u/") ||
		strings.Contains(urlLower, "/people/") ||
		strings.Contains(urlLower, "/member/") ||
		strings.Contains(urlLower, "/author/") ||
		strings.Contains(urlLower, "/accounts/")

	// --- Navigational absolute boosts ---
	if query.intent == intentNavigational {
		totalScore += applyNavigationalBoosts(candidate, query, domainInfo, domainNorm, domainTokens, isProfilePage, isHomepage)
	}

	// --- Platform intent boost (hub-aware via sitehints) ---
	if query.platformIntent != "" {
		totalScore += applyPlatformIntentBoosts(candidate, query, domainInfo, urlTokens, isProfilePage, isHomepage)
	}

	// --- Site-type intent boost ---
	if query.siteTypeIntent != "" && isHomepage {
		typeInDomain := strings.Contains(candidate.Domain, query.siteTypeIntent) ||
			strings.Contains(domainInfo.effectiveDomain, query.siteTypeIntent) ||
			strings.Contains(domainInfo.rootLabel, query.siteTypeIntent)
		if typeInDomain {
			totalScore += 12000.0
		}
	}

	// --- Authoritative source boost for informational queries ---
	if query.intent == intentInformational {
		totalScore += applyGovernmentSourceBoosts(candidate, query, urlDomain, isHomepage)
	}

	// --- Domain brand match boost for informational queries ---
	if query.intent == intentInformational {
		totalScore += applyBrandMatchBoosts(query, domainInfo)
	}

	return totalScore, true
}

func applyNavigationalBoosts(candidate searchCandidate, query searchQuery, domainInfo searchDomainInfo, domainNorm string, domainTokens []string, isProfilePage bool, isHomepage bool) float64 {
	var boost float64
	domainPhrase := bestFieldPhraseScore(query.normalized, query.tokens, domainNorm, domainTokens)

	if query.domainLike != "" || query.navTerm != "" {
		candidateDomain := strings.TrimPrefix(candidate.Domain, "www.")
		matchDomain := query.domainLike
		if matchDomain == "" {
			matchDomain = query.navTerm
		}
		if candidateDomain == matchDomain {
			if !isProfilePage {
				boost += 8000.0
				if isHomepage {
					boost += 6000.0
				}
			}
		} else if domainInfo.isRootDomain && domainInfo.rootLabel == matchDomain {
			if !isProfilePage {
				boost += 6000.0
				if isHomepage {
					boost += 5000.0
				}
			}
		}
	}

	if isHomepage && domainPhrase >= 0.75 && !isProfilePage {
		if domainInfo.isRootDomain {
			boost += 15000.0
		} else {
			boost += 8000.0
		}
	}
	return boost
}

func applyPlatformIntentBoosts(candidate searchCandidate, query searchQuery, domainInfo searchDomainInfo, urlTokens []string, isProfilePage bool, isHomepage bool) float64 {
	var boost float64
	if domainInfo.rootLabel == query.platformIntent {
		if !isProfilePage {
			// Use sitehints to classify the URL as hub or sub-page.
			hint := sitehints.Classify(candidate.URL, query.platformIntent)

			if hint.IsHub {
				boost += 16000.0 + hint.HubBoost
				if hint.HubName != "" {
					for _, tok := range query.tokens {
						if strings.EqualFold(tok, hint.HubName) {
							boost += 30000.0
							break
						}
					}
				}
			} else {
				basePlatformBoost := 16000.0 - hint.DepthPenalty
				if basePlatformBoost < 10000.0 {
					basePlatformBoost = 10000.0
				}
				boost += basePlatformBoost
			}

			if isHomepage {
				boost += 4000.0
			}

			if len(query.tokens) > 0 {
				urlAvg, _, _ := searchTokenCoverage(query.tokens, urlTokens)
				if urlAvg > 0 {
					boost += 3000.0 * urlAvg
				}
			}
		} else {
			boost += 2000.0
		}
	}
	return boost
}

func applyGovernmentSourceBoosts(candidate searchCandidate, query searchQuery, urlDomain string, isHomepage bool) float64 {
	var boost float64
	isGovOrEdu := strings.Contains(urlDomain, ".gub.") ||
		strings.Contains(urlDomain, ".gov.") ||
		strings.Contains(urlDomain, ".edu.")

	if isGovOrEdu {
		pathLower := strings.ToLower(candidate.URL)
		pathMatchCount := 0
		for _, token := range query.tokens {
			if len(token) >= 3 && !isNumericToken(token) && strings.Contains(pathLower, token) {
				pathMatchCount++
			}
		}

		primaryToken := ""
		if len(query.tokens) > 0 {
			ordered := sortTokensByRelevance(query.tokens, query.idfMap)
			if len(ordered) > 0 {
				primaryToken = ordered[0]
			}
		}
		hasPrimaryInPath := primaryToken != "" && len(primaryToken) >= 3 && !isNumericToken(primaryToken) && strings.Contains(pathLower, primaryToken)

		primaryIsSpecific := true
		if hasPrimaryInPath && len(query.tokens) >= 2 && len(query.idfMap) >= 2 {
			ordered := sortTokensByRelevance(query.tokens, query.idfMap)
			if len(ordered) >= 2 {
				primaryIDF := query.idfMap[ordered[0]]
				secondIDF := query.idfMap[ordered[1]]
				if primaryIDF < secondIDF*1.5 {
					primaryIsSpecific = false
				}
			}
		}

		if hasPrimaryInPath && pathMatchCount >= 2 && primaryIsSpecific {
			boost += 8000.0 + float64(pathMatchCount)*800.0
			if isHomepage {
				boost += 2000.0
			}
		} else if hasPrimaryInPath && primaryIsSpecific {
			boost += 6000.0 + float64(pathMatchCount)*800.0
			if isHomepage {
				boost += 2000.0
			}
		} else if hasPrimaryInPath && pathMatchCount >= 2 {
			boost += 2000.0 + float64(pathMatchCount)*400.0
		} else if hasPrimaryInPath {
			boost += 800.0
		} else if pathMatchCount >= 2 {
			boost += 1000.0 + float64(pathMatchCount)*400.0
		} else if pathMatchCount >= 1 {
			boost += 400.0
		}
	}
	return boost
}

func applyBrandMatchBoosts(query searchQuery, domainInfo searchDomainInfo) float64 {
	var boost float64
	if len(query.tokens) > 0 && domainInfo.rootLabel != "" {
		rootLower := strings.ToLower(domainInfo.rootLabel)
		rootLen := len([]rune(rootLower))
		brandTokens := make([]string, 0, len(query.tokens)+1)
		brandTokens = append(brandTokens, query.tokens...)
		if query.siteTypeIntent != "" && len(query.siteTypeIntent) >= 3 {
			brandTokens = append(brandTokens, query.siteTypeIntent)
		}
		matchedTokens := 0
		for _, token := range brandTokens {
			tokenLen := len([]rune(token))
			if tokenLen >= 3 && strings.Contains(rootLower, token) && tokenLen*100/rootLen >= 40 {
				matchedTokens++
			}
		}
		if matchedTokens >= 2 {
			boost += float64(matchedTokens) * 2000.0
			if matchedTokens == len(brandTokens) {
				boost += 5000.0
			}
		}
	}
	return boost
}

// scoreContentRelevance computes how well the page content matches the query.
// Uses phrase matching, token coverage, and term frequency across title, h1, h2, description, and URL.
func scoreContentRelevance(query searchQuery, titleNorm string, titleTokens []string, h1Norm string, h1Tokens []string, h2Norm string, h2Tokens []string, descNorm string, descTokens []string, urlNorm string, urlTokens []string, domainNorm string, domainTokens []string, effectiveDomainNorm string, rootLabelNorm string, urlDomain string) float64 {
	var score float64

	// Phrase scores (exact phrase match in each field)
	titlePhrase := bestFieldPhraseScore(query.normalized, query.tokens, titleNorm, titleTokens)
	h1Phrase := bestFieldPhraseScore(query.normalized, query.tokens, h1Norm, h1Tokens)
	h2Phrase := bestFieldPhraseScore(query.normalized, query.tokens, h2Norm, h2Tokens)
	descPhrase := bestFieldPhraseScore(query.normalized, query.tokens, descNorm, descTokens)
	urlPhrase := bestFieldPhraseScore(query.normalized, query.tokens, urlNorm, urlTokens)
	domainPhrase := max(
		bestFieldPhraseScore(query.normalized, query.tokens, domainNorm, domainTokens),
		bestFieldPhraseScore(query.normalized, query.tokens, effectiveDomainNorm, strings.Fields(effectiveDomainNorm)),
		bestFieldPhraseScore(query.normalized, query.tokens, rootLabelNorm, strings.Fields(rootLabelNorm)),
	)

	score += 560.0*titlePhrase + 360.0*h1Phrase + 360.0*h2Phrase + 240.0*descPhrase
	score += 260.0*domainPhrase + 90.0*urlPhrase

	// Token coverage (what fraction of query tokens appear in each field)
	titleAvg, titleCoverage, titleExact := searchTokenCoverage(query.tokens, titleTokens)
	h1Avg, h1Coverage, _ := searchTokenCoverage(query.tokens, h1Tokens)
	h2Avg, h2Coverage, _ := searchTokenCoverage(query.tokens, h2Tokens)
	descAvg, descCoverage, _ := searchTokenCoverage(query.tokens, descTokens)
	domainAvg, domainCoverage, _ := searchTokenCoverage(query.tokens, domainTokens)
	urlAvg, urlCoverage, _ := searchTokenCoverage(query.tokens, urlTokens)

	// Weighted average across fields (title matters most)
	weightedFieldAvg := (3.0*titleAvg + 2.0*h1Avg + 2.0*h2Avg + 2.0*descAvg) / 9.0
	weightedFieldCoverage := (3.0*titleCoverage + 2.0*h1Coverage + 2.0*h2Coverage + 2.0*descCoverage) / 9.0

	score += 430.0*weightedFieldAvg + 180.0*weightedFieldCoverage
	score += 200.0*domainAvg + 60.0*domainCoverage
	score += 130.0*urlAvg + 40.0*urlCoverage

	// Term frequency with IDF weighting
	fixedTF := fixedWeightedTermFrequency(query.tokens, query.idfMap, titleNorm, h1Norm, h2Norm, descNorm)
	score += 55.0 * fixedTF

	// Exact title match bonus
	if len(query.tokens) > 0 && titleExact == len(query.tokens) {
		score += 180.0
	}

	// Content coverage penalty: if the page only matches 1 out of N query tokens
	// in title+h1+h2+description combined, heavily penalize it.
	// This prevents pages that only match "uruguay" from ranking for "patente uruguay".
	if len(query.tokens) >= 2 {
		totalMatched := 0
		for _, qt := range query.tokens {
			matched := false
			for _, tt := range titleTokens {
				if searchTokenSimilarity(qt, tt) >= 0.72 {
					matched = true
					break
				}
			}
			if !matched {
				for _, tt := range h1Tokens {
					if searchTokenSimilarity(qt, tt) >= 0.72 {
						matched = true
						break
					}
				}
			}
			if !matched {
				for _, tt := range h2Tokens {
					if searchTokenSimilarity(qt, tt) >= 0.72 {
						matched = true
						break
					}
				}
			}
			if !matched {
				for _, tt := range descTokens {
					if searchTokenSimilarity(qt, tt) >= 0.72 {
						matched = true
						break
					}
				}
			}
			if !matched {
				for _, tt := range urlTokens {
					if searchTokenSimilarity(qt, tt) >= 0.72 {
						matched = true
						break
					}
				}
			}
			if matched {
				totalMatched++
			}
		}

		coverageRatio := float64(totalMatched) / float64(len(query.tokens))
		if coverageRatio < 0.5 {
			// For government/educational domains, apply a softer penalty —
			// their body content is often relevant even if metadata is minimal.
			isGovOrEdu := strings.HasSuffix(urlDomain, ".gub.uy") ||
				strings.HasSuffix(urlDomain, ".gov.uy") ||
				strings.Contains(urlDomain, ".gub.") ||
				strings.Contains(urlDomain, ".gov.")

			if isGovOrEdu {
				// Softer penalty for gov domains
				score *= 0.75 + coverageRatio*0.25 // e.g., 0.33 coverage → score *= 0.83
			} else {
				// Heavy penalty for non-gov domains
				score *= coverageRatio * 2.0 // e.g., 0.25 coverage → score *= 0.5
			}
		}
	}

	return score
}

// scoreAuthority computes a saturated authority score from PageRank and referring domains.
// Uses min(log1p(pr), cap) to prevent high-PR sites from dominating.
func scoreAuthority(pageRank float64, referringDomains int) float64 {
	if pageRank > 0 {
		return math.Min(math.Log1p(pageRank), 4.0) * 100.0
	}
	if referringDomains > 0 {
		return math.Min(math.Log1p(float64(referringDomains)), 3.0) * 70.0
	}
	return 0
}

// scoreURLSignal computes URL-based relevance signals.
// For navigational queries: homepage and domain match matter.
// For informational queries: URL path relevance to query tokens is the primary signal.
func scoreURLSignal(query searchQuery, rawURL string, domainInfo searchDomainInfo, urlDomain string) float64 {
	var score float64

	isHomepage := isSearchHomepage(rawURL)
	urlLower := strings.ToLower(rawURL)

	// For informational queries: URL path token matches are very strong signals
	if query.intent == intentInformational && len(query.tokens) > 0 {
		// Check each query token against the URL path
		pathTokens := strings.FieldsFunc(strings.Trim(urlLower, "/"), func(r rune) bool {
			return r == '/' || r == '-' || r == '_' || r == '.' || r == '?' || r == '=' || r == '&'
		})

		matchedTokens := 0
		for _, token := range query.tokens {
			if len(token) < 3 {
				continue
			}
			for _, pathToken := range pathTokens {
				if pathToken == token || strings.Contains(pathToken, token) || strings.Contains(token, pathToken) {
					matchedTokens++
					break
				}
			}
		}

		if matchedTokens > 0 {
			// Strong boost: each matched query token in URL path
			score += float64(matchedTokens) * 200.0
			// Bonus if ALL query tokens are in the URL
			if matchedTokens == len(query.tokens) {
				score += 300.0
			}
		}

		// URL depth preference for informational (shallow = more authoritative)
		slashCount := strings.Count(rawURL, "/") - 2
		if slashCount <= 1 {
			score += 60.0
		} else if slashCount <= 2 {
			score += 30.0
		}
	}

	// Government domain authority boost for informational queries
	if query.intent == intentInformational {
		if strings.HasSuffix(urlDomain, ".gub.uy") || strings.HasSuffix(urlDomain, ".gov.uy") {
			score += 150.0
		} else if strings.Contains(urlDomain, ".gub.") || strings.Contains(urlDomain, ".gov.") {
			score += 80.0
		}
	}

	// Domain root label matching: "eldorado.com.uy" → rootLabel "eldorado" matches token "dorado"
	// This catches navigational queries where the brand name is in the domain.
	// Requires token to be ≥60% of root label length to avoid false matches
	// (e.g. "supermercadosenuruguay" should NOT match "supermercado").
	if len(query.tokens) > 0 && domainInfo.rootLabel != "" {
		rootLower := strings.ToLower(domainInfo.rootLabel)
		rootLen := len([]rune(rootLower))
		urlBrandTokens := make([]string, 0, len(query.tokens)+1)
		urlBrandTokens = append(urlBrandTokens, query.tokens...)
		if query.siteTypeIntent != "" && len(query.siteTypeIntent) >= 3 {
			urlBrandTokens = append(urlBrandTokens, query.siteTypeIntent)
		}
		matchedInDomain := 0
		for _, token := range urlBrandTokens {
			tokenLen := len([]rune(token))
			if tokenLen >= 3 && strings.Contains(rootLower, token) && tokenLen*100/rootLen >= 40 {
				matchedInDomain++
			}
		}
		if matchedInDomain > 0 {
			score += float64(matchedInDomain) * 400.0
			if matchedInDomain >= 2 {
				score += 800.0
			}
		}
	}

	// For navigational queries: domain match and homepage bonus
	if query.intent == intentNavigational {
		if isHomepage && domainInfo.isRootDomain {
			score += 200.0
		} else if isHomepage {
			score += 100.0
		}
	}

	return score
}

// scoreFreshness computes a freshness score based on crawl and publish dates.
func scoreFreshness(crawledAt time.Time, publishedAt time.Time) float64 {
	bestTime := crawledAt
	if !publishedAt.IsZero() {
		bestTime = publishedAt
	}
	if bestTime.IsZero() {
		return 0
	}
	days := time.Since(bestTime).Hours() / 24
	return max(0.0, 50.0-days*0.15)
}

// scoreUserSignals computes a score from SQL-side signals (clicks, etc.).
// Capped to prevent popularity bias from dominating.
func scoreUserSignals(sqlScore float64) float64 {
	// SQL score already includes click signals; cap the contribution
	return math.Min(sqlScore, 500.0) * 0.10
}

// ---------------------------------------------------------------------------
// Snippet extraction
// ---------------------------------------------------------------------------

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

	query := fmt.Sprintf(`SELECT id, SUBSTR(content, 1, 4000) FROM pages WHERE id IN (%s)`, strings.Join(ids, ","))

	rows, err := s.searchContentDB.QueryContext(ctx, query, args...)
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
				} else {
					runes := []rune(body)
					if len(runes) > 240 {
						candidates[i].Snippet = string(runes[:240])
					} else {
						candidates[i].Snippet = body
					}
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Domain diversity & language promotion
// ---------------------------------------------------------------------------

func promoteLanguagePerDomain(candidates []searchCandidate, lang string, secondaryLang string) []searchCandidate {
	if len(candidates) <= 1 {
		return candidates
	}

	domainGroups := make(map[string][]int)
	for i, c := range candidates {
		info := classifySearchDomain(c.Domain)
		d := info.effectiveDomain
		if d == "" {
			d = c.Domain
		}
		domainGroups[d] = append(domainGroups[d], i)
	}

	for _, indices := range domainGroups {
		if len(indices) <= 1 {
			continue
		}

		bestIdx := -1
		bestScore := 0

		for _, idx := range indices {
			if candidates[idx].Language == lang {
				if bestScore < 2 {
					bestScore = 2
					bestIdx = idx
				}
			} else if bestScore < 2 && secondaryLang != "" && candidates[idx].Language == secondaryLang {
				if bestScore < 1 {
					bestScore = 1
					bestIdx = idx
				}
			}
		}

		if bestIdx >= 0 && bestIdx != indices[0] {
			best := candidates[bestIdx]
			candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)

			insertPos := indices[0]
			if bestIdx < indices[0] {
				insertPos = indices[0] - 1
			}

			candidates = append(candidates[:insertPos], append([]searchCandidate{best}, candidates[insertPos:]...)...)
		}
	}

	return candidates
}

func diversifyByDomain(candidates []searchCandidate) []searchCandidate {
	if len(candidates) <= 3 {
		return candidates
	}

	var result []searchCandidate
	domainCount := make(map[string]int)

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
			result = append(result, deferred...)
			break
		}
	}

	return result
}

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

// ---------------------------------------------------------------------------
// Mode bonus
// ---------------------------------------------------------------------------

func searchModeBonus(mode searchMode) float64 {
	switch mode {
	case searchModeNavigational:
		return 120.0
	case searchModeFTSPhrase:
		return 180.0
	case searchModeFTSExact:
		return 140.0
	case searchModeFTSCore:
		return 100.0
	case searchModeFTSRelaxed:
		return 60.0
	default:
		return 0.0
	}
}

// ---------------------------------------------------------------------------
// Text normalization & token utilities
// ---------------------------------------------------------------------------

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

// searchStopwords contains common function words in Spanish and English that
// should be removed from FTS queries to avoid massive AND intersections.
var searchStopwords = map[string]struct{}{
	// --- Spanish ---
	"a": {}, "al": {}, "con": {}, "de": {}, "del": {}, "el": {}, "en": {},
	"es": {}, "la": {}, "las": {}, "lo": {}, "los": {}, "le": {}, "les": {},
	"me": {}, "mi": {}, "no": {}, "nos": {}, "o": {}, "por": {}, "que": {},
	"se": {}, "si": {}, "sin": {}, "su": {}, "sus": {}, "te": {}, "tu": {},
	"tus": {}, "un": {}, "una": {}, "y": {}, "ya": {},
	"como": {}, "cual": {}, "donde": {}, "este": {}, "esta": {}, "esto": {},
	"ese": {}, "esa": {}, "eso": {}, "para": {}, "pero": {},
	"mas": {}, "muy": {}, "fue": {}, "hay": {}, "ser": {},
	"cuando": {}, "cuanto": {}, "quien": {}, "quienes": {}, "cuyo": {},
	"sobre": {}, "entre": {}, "hasta": {}, "desde": {}, "tras": {},
	// --- English ---
	"an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {}, "but": {},
	"by": {}, "do": {}, "for": {}, "from": {}, "had": {}, "has": {}, "have": {},
	"he": {}, "her": {}, "him": {}, "his": {}, "how": {}, "if": {}, "in": {},
	"is": {}, "it": {}, "its": {}, "my": {}, "not": {}, "of": {}, "on": {},
	"or": {}, "our": {}, "she": {}, "so": {}, "the": {}, "to": {}, "up": {},
	"us": {}, "was": {}, "we": {}, "who": {}, "you": {},
	"when": {}, "what": {}, "where": {}, "which": {}, "why": {},
	"with": {}, "about": {}, "into": {}, "through": {}, "during": {},
	"before": {}, "after": {}, "above": {}, "below": {}, "than": {},
}

func filterStopwords(tokens []string) []string {
	return filterStopwordsContext(tokens, false)
}

func filterStopwordsContext(tokens []string, mediaContext bool) []string {
	if len(tokens) <= 1 {
		return tokens
	}

	// In media context, preserve stopwords that are commonly used as
	// movie/TV show titles (e.g. "From", "The", "It", "You", "We").
	mediaTitleStopwords := map[string]struct{}{
		"from": {}, "the": {}, "it": {}, "you": {}, "we": {},
		"her": {}, "him": {}, "us": {}, "me": {}, "my": {},
		"up": {}, "in": {}, "on": {}, "el": {}, "la": {},
		"lo": {}, "los": {}, "las": {}, "un": {}, "una": {},
		"su": {}, "si": {}, "no": {}, "ya": {},
	}

	filtered := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, stop := searchStopwords[t]; stop {
			if mediaContext {
				if _, ok := mediaTitleStopwords[t]; ok {
					filtered = append(filtered, t)
					continue
				}
			}
			continue
		}
		filtered = append(filtered, t)
	}

	if len(filtered) == 0 {
		return tokens
	}
	return filtered
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
	if parsed.Path == "" || parsed.Path == "/" {
		return true
	}
	path := strings.Trim(parsed.Path, "/")
	if !strings.Contains(path, "/") && isLanguagePathSegment(path) {
		return true
	}
	return false
}

var langPathPattern = regexp.MustCompile(`^[a-z]{2}(-[a-zA-Z0-9]{2,4})?$`)

func isLanguagePathSegment(segment string) bool {
	return langPathPattern.MatchString(segment)
}

// ---------------------------------------------------------------------------
// Phrase & token scoring utilities
// ---------------------------------------------------------------------------

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

func fixedWeightedTermFrequency(queryTokens []string, idfMap map[string]float64, titleNorm, h1Norm, h2Norm, descNorm string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}

	queryTokens = uniqueStrings(queryTokens)
	titleFreq := tokenFrequencyMap(strings.Fields(titleNorm))
	h1Freq := tokenFrequencyMap(strings.Fields(h1Norm))
	h2Freq := tokenFrequencyMap(strings.Fields(h2Norm))
	descFreq := tokenFrequencyMap(strings.Fields(descNorm))

	return weightedTokenFrequency(queryTokens, idfMap, titleFreq, 3.0, 4) +
		weightedTokenFrequency(queryTokens, idfMap, h1Freq, 2.0, 5) +
		weightedTokenFrequency(queryTokens, idfMap, h2Freq, 2.0, 5) +
		weightedTokenFrequency(queryTokens, idfMap, descFreq, 2.0, 5)
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

// ---------------------------------------------------------------------------
// FTS query builders (simplified to 4 plans)
// ---------------------------------------------------------------------------

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

func buildFTSCoreQuery(q searchQuery) string {
	tokens := q.ftsTokens
	if len(tokens) == 0 {
		return ""
	}

	ordered := sortTokensByRelevance(tokens, q.idfMap)
	core := ordered
	if len(core) > 2 {
		core = core[:2]
	}

	parts := make([]string, 0, len(core))
	for _, token := range core {
		parts = append(parts, quoteFTS5Token(token))
	}
	return strings.Join(parts, " AND ")
}

func buildFTSRelaxedQuery(q searchQuery) string {
	tokens := q.ftsTokens
	if len(tokens) == 0 {
		return ""
	}

	ordered := sortTokensByRelevance(tokens, q.idfMap)
	significant := ordered
	if len(significant) > 4 {
		significant = significant[:4]
	}

	var parts []string
	seen := make(map[string]struct{}, len(significant)*2)
	for _, token := range significant {
		exact := quoteFTS5Token(token)
		if _, ok := seen[exact]; !ok {
			seen[exact] = struct{}{}
			parts = append(parts, exact)
		}
		if len([]rune(token)) >= 4 {
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

func isNumericToken(token string) bool {
	for _, r := range token {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(token) > 0
}

func sortTokensByRelevance(tokens []string, idfMap map[string]float64) []string {
	if len(tokens) == 0 {
		return tokens
	}
	ordered := append([]string(nil), tokens...)
	sort.SliceStable(ordered, func(i, j int) bool {
		idfI := idfMap[ordered[i]]
		idfJ := idfMap[ordered[j]]
		if math.Abs(idfI-idfJ) > 0.001 {
			return idfI > idfJ
		}
		return len([]rune(ordered[i])) > len([]rune(ordered[j]))
	})
	return ordered
}

// splitCompoundTokens splits compound words like "tiendainglesa" into "tienda" + "inglesa"
// by validating splits against the FTS5 vocabulary. Only applies to tokens ≥ 12 chars
// to avoid splitting normal words like "inglesa" or "montevideo".
func (s *SQLiteStorage) splitCompoundTokens(tokens []string) []string {
	// Collect tokens that are long enough to be compound words
	candidates := make([]string, 0)
	for _, tok := range tokens {
		if len([]rune(tok)) >= 12 {
			candidates = append(candidates, tok)
		}
	}
	if len(candidates) == 0 {
		return tokens
	}

	// Generate all possible subwords from 2-way splits
	type split struct {
		left, right string
	}
	tokenSplits := make(map[string][]split) // original token → possible splits
	subwordSet := make(map[string]struct{})
	for _, tok := range candidates {
		runes := []rune(tok)
		for i := 3; i <= len(runes)-3; i++ {
			left := string(runes[:i])
			right := string(runes[i:])
			tokenSplits[tok] = append(tokenSplits[tok], split{left, right})
			subwordSet[left] = struct{}{}
			subwordSet[right] = struct{}{}
		}
	}

	// Query FTS vocab to check which subwords actually exist in the index.
	// Also check if the original compound tokens themselves are real words —
	// if so, we should NOT split them (e.g. "supermercado" is a real word).
	subwords := make([]string, 0, len(subwordSet)+len(candidates))
	for w := range subwordSet {
		subwords = append(subwords, w)
	}
	for _, tok := range candidates {
		subwords = append(subwords, tok)
	}

	knownWords := make(map[string]struct{})
	if len(subwords) > 0 {
		// Build batch query
		placeholders := make([]string, len(subwords))
		args := make([]any, len(subwords))
		for i, w := range subwords {
			placeholders[i] = "?"
			args[i] = w
		}
		query := fmt.Sprintf(`SELECT term FROM pages_fts_vocab WHERE term IN (%s)`, strings.Join(placeholders, ","))
		rows, err := s.searchContentDB.QueryContext(context.Background(), query, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var term string
				if err := rows.Scan(&term); err == nil {
					knownWords[term] = struct{}{}
				}
			}
		}
	}

	// Identify compound tokens that are real words themselves — skip splitting them
	skipSplit := make(map[string]bool)
	for _, tok := range candidates {
		if _, ok := knownWords[tok]; ok {
			skipSplit[tok] = true
		}
	}

	// Replace compound tokens with their split parts if both halves are known
	result := make([]string, 0, len(tokens)+4)
	for _, tok := range tokens {
		splits, ok := tokenSplits[tok]
		if !ok || skipSplit[tok] {
			result = append(result, tok)
			continue
		}
		// Find best split: prefer longer left part (greedy left-to-right)
		bestLeft, bestRight := "", ""
		for _, sp := range splits {
			_, lok := knownWords[sp.left]
			_, rok := knownWords[sp.right]
			if lok && rok {
				if len([]rune(sp.left)) > len([]rune(bestLeft)) {
					bestLeft = sp.left
					bestRight = sp.right
				}
			}
		}
		if bestLeft != "" && bestRight != "" {
			result = append(result, bestLeft, bestRight)
		} else {
			result = append(result, tok)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Merge, utility
// ---------------------------------------------------------------------------

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

func (s *SQLiteStorage) TestFTSQuery(ctx context.Context, query string) (int, error) {
	return s.countFTSCandidates(ctx, query)
}

func (s *SQLiteStorage) RecordClick(ctx context.Context, query string, url string) error {
	normalizedQuery := normalizeSearchText(query)
	if normalizedQuery == "" || url == "" {
		return nil
	}

	_, err := s.writeContentDB.ExecContext(ctx, `
		INSERT INTO search_clicks (query, url, clicks)
		VALUES (?, ?, 1)
		ON CONFLICT(query, url) DO UPDATE SET clicks = clicks + 1
	`, normalizedQuery, url)
	return err
}

func (s *SQLiteStorage) SearchImages(ctx context.Context, query string, limit, offset int) ([]ImageSearchResult, int, error) {
	q := s.newSearchQuery(query)
	if q.normalized == "" || len(q.tokens) == 0 {
		return nil, 0, nil
	}

	ftsQuery := buildFTSRelaxedQuery(q)
	if ftsQuery == "" {
		return nil, 0, nil
	}

	var total int
	countQuery := `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH ?`
	if err := s.searchContentDB.QueryRowContext(ctx, countQuery, ftsQuery).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting image results for %q: %w", query, err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	searchQuery := `
	SELECT i.id, i.url, i.page_url, i.domain, i.alt_text, i.title, i.context,
		i.width, i.height, rank
	FROM images_fts
	JOIN images i ON i.id = images_fts.rowid
	WHERE images_fts MATCH ?
	ORDER BY rank
	LIMIT ? OFFSET ?
	`

	rows, err := s.searchContentDB.QueryContext(ctx, searchQuery, ftsQuery, limit, offset)
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

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}

	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
