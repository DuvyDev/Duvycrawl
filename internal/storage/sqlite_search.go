package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/transform"
	_ "modernc.org/sqlite"
)

func (s *SQLiteStorage) SearchPages(ctx context.Context, query string, limit, offset int, lang string, domain string, schemaType string) ([]SearchResult, int, error) {
	q := s.newSearchQuery(query)
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
		{mode: searchModeFTSCore, ftsQuery: buildFTSCoreQuery(q.tokens)},
		{mode: searchModeFTSCore2, ftsQuery: buildFTSCore2Query(q.tokens)},
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

	// --- Fallback to Vocabulary Spell Correction ---
	// Only apply if the query is a single word and FTS found nothing.
	if len(candidates) == 0 && searchCtx.Err() == nil && len(q.tokens) == 1 {
		correctedTerm := s.suggestCorrectTerm(searchCtx, q.tokens[0])
		if correctedTerm != "" && correctedTerm != q.tokens[0] {
			s.logger.Info("auto-correcting typo", "original", q.tokens[0], "corrected", correctedTerm)
			
			// Re-create the query with the corrected term
			qCorrect := s.newSearchQuery(correctedTerm)
			if qCorrect.normalized != "" {
				qCorrect.idfMap, _ = s.getSearchIDFMap(searchCtx, qCorrect.tokens)
				
				// Re-run FTS Exact and Prefix searches for the corrected term
				fallbackPlans := []struct {
					mode     searchMode
					ftsQuery string
				}{
					{mode: searchModeFTSExact, ftsQuery: buildFTSExactQuery(qCorrect.tokens)},
					{mode: searchModeFTSPrefix, ftsQuery: buildFTSPrefixQuery(qCorrect.tokens)},
				}
				
				for _, plan := range fallbackPlans {
					if searchCtx.Err() != nil {
						break
					}
					if plan.ftsQuery == "" {
						continue
					}
					count, err := s.countFTSCandidates(searchCtx, plan.ftsQuery)
					if err != nil || count == 0 {
						continue
					}
					
					results, err := s.searchFTSCandidates(searchCtx, plan.mode, plan.ftsQuery, qCorrect, lang, candidateLimit)
					if err == nil && len(results) > 0 {
						candidates = mergeSearchCandidates(candidates, results)
						total += count
						if selectedMode == "" {
							selectedMode = plan.mode
						}
						break
					}
				}
				
				// Update q to qCorrect so re-ranking uses the correct token!
				if len(candidates) > 0 {
					q = qCorrect
				}
			}
		}
	}

	// --- Navigational boost when FTS/fuzzy gave few results ---
	// For platform intents (e.g. "reddit", "youtube") we always run this so
	// results from that domain are surfaced even if the platform name does not
	// appear in the indexed page content.
	if q.navigational && searchCtx.Err() == nil {
		if len(candidates) < 30 || q.platformIntent != "" {
			navigationCandidates, err := s.searchNavigationalCandidates(searchCtx, q, lang, min(candidateLimit, 60))
			if err == nil {
				candidates = mergeSearchCandidates(candidates, navigationCandidates)
			}
		}
	}

	reranked := rerankSearchCandidates(candidates, q, lang)

	// --- Semantic re-ranking via embeddings (if Ollama is available) ---
	if len(reranked) > 0 && s.embedder != nil {
		queryEmbedding, err := s.embedder.GenerateEmbedding(q.raw)
		if err == nil && len(queryEmbedding) > 0 {
			// Collect page IDs for the top candidates.
			topN := min(len(reranked), 100) // Only re-rank top 100 for speed.
			pageIDs := make([]int64, 0, topN)
			for i := 0; i < topN; i++ {
				pageIDs = append(pageIDs, reranked[i].ID)
			}

			embs, err := s.GetPageEmbeddings(searchCtx, pageIDs)
			if err == nil && len(embs) > 0 {
				semanticBoostWeight := 0.25 // 25% semantic, 75% lexical
				for i := 0; i < topN; i++ {
					emb, ok := embs[reranked[i].ID]
					if !ok || len(emb.Embedding) == 0 {
						continue
					}
					sim := cosineSimilarity(queryEmbedding, emb.Embedding)
					// Scale similarity (0..1) to the same magnitude as lexical scores.
					semanticScore := sim * 10000.0
					oldRank := reranked[i].Rank
					reranked[i].Rank = oldRank*(1.0-semanticBoostWeight) + semanticScore*semanticBoostWeight
				}
				// Re-sort after blending.
				sort.SliceStable(reranked, func(i, j int) bool {
					return reranked[i].Rank > reranked[j].Rank
				})
			}
		}
	}

	if len(reranked) == 0 {
		return nil, 0, nil
	}

	s.logger.Debug("search query debug",
		"raw", q.raw,
		"navTerm", q.navTerm,
		"domainLike", q.domainLike,
		"navigational", q.navigational,
		"tokens", strings.Join(q.tokens, ","),
	)

	// Debug: log top candidates with their computed scores.
	if len(reranked) > 0 {
		debugCount := min(5, len(reranked))
		for i := 0; i < debugCount; i++ {
			s.logger.Debug("search candidate",
				"rank", i+1,
				"url", reranked[i].URL,
				"score", reranked[i].Rank,
				"domain", reranked[i].Domain,
			)
		}
	}

	// --- Optional domain filter ---
	// Must run BEFORE diversifyByDomain so we don't artificially limit site-specific searches to 3 results.
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
	// Must run BEFORE diversifyByDomain so filtering out schemas doesn't leave empty slots.
	if schemaType != "" {
		filtered := make([]searchCandidate, 0, len(reranked))
		for _, c := range reranked {
			if strings.EqualFold(c.SchemaType, schemaType) {
				filtered = append(filtered, c)
			}
		}
		reranked = filtered
	}

	// --- Domain diversity: interleave results so one domain doesn't
	//     dominate the first page. First 3 positions = max 1 per domain,
	//     positions 4-10 = max 2, positions 11+ = max 3.
	// We only run this if we are not explicitly searching within a single domain!
	if domain == "" {
		reranked = diversifyByDomain(reranked)
	}

	if domain != "" || schemaType != "" {
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

	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
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

func (s *SQLiteStorage) newSearchQuery(query string) searchQuery {
	normalized := normalizeSearchText(query)
	tokens := strings.Fields(normalized)
	domainLike := normalizeDomainLikeQuery(query)

	// Implicit gTLD detection: for 2-token queries like "warframe market",
	// check if joining with "." forms a valid domain (e.g., warframe.market).
	// .market, .dev, .app, .blog, etc. are real gTLDs — if publicsuffix
	// validates the combination, treat the query as navigational so the
	// homepage gets the massive domain-match boosts it deserves.
	if domainLike == "" && len(tokens) == 2 {
		candidate := tokens[0] + "." + tokens[1]
		if etld, err := publicsuffix.EffectiveTLDPlusOne(candidate); err == nil && etld == candidate {
			domainLike = candidate
		}
	}

	navTerm := ""
	if domainLike != "" {
		navTerm = strings.ReplaceAll(normalizeSearchText(classifySearchDomain(domainLike).rootLabel), " ", "")
	}
	if navTerm == "" && len(tokens) >= 1 {
		if len(tokens) == 1 {
			navTerm = tokens[0]
		} else {
			// Compound domain detection: "uruguay concursa" -> "uruguayconcursa".
			// This catches domains like uruguayconcursa.gub.uy that concatenate
			// words without dots or hyphens.
			compactNav := strings.Join(tokens, "")
			if len(compactNav) >= 6 {
				navTerm = compactNav
			}
		}
	}

	// Detect site-type intent and platform intent.
	// We only extract these if they are at the edges of the query (first or last token).
	// If multiple platforms are detected (e.g. "youtube vs twitch"), we abort extraction
	// so FTS can search for both platform names in the text.
	var foundPlatforms []string
	var foundSiteTypes []string

	isEdge := func(i int) bool {
		return i == 0 || i == len(tokens)-1
	}

	for i, tok := range tokens {
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
	// Only set site type if no platform intent was extracted
	if len(foundSiteTypes) == 1 && platformIntent == "" {
		siteTypeIntent = foundSiteTypes[0]
	}

	var searchTokens []string
	for _, tok := range tokens {
		if tok == platformIntent {
			continue
		}
		if tok == siteTypeIntent {
			continue
		}
		searchTokens = append(searchTokens, tok)
	}

	if len(searchTokens) == 0 {
		// Query was only a site-type or platform word (e.g. "wiki" or "reddit") — keep original tokens.
		searchTokens = tokens
	}

	// Platform names act as navigational terms so domain filtering kicks in.
	// When a platform name is detected, it overrides any gTLD-based navTerm
	// because the user intent is clearly to filter by that platform's domain
	// (e.g. "warframe reddit" → find reddit.com results, NOT warframe.reddit).
	if platformIntent != "" {
		navTerm = platformIntent
		// If the gTLD detection produced a false domainLike using the platform
		// name as a TLD (e.g. "warframe.reddit"), discard it — it's not a real
		// domain the user is looking for.
		if domainLike != "" && strings.HasSuffix(domainLike, "."+platformIntent) {
			domainLike = ""
		}
	}

	s.logger.Debug("search query debug",
		"raw", query,
		"navTerm", navTerm,
		"domainLike", domainLike,
		"navigational", navTerm != "" || domainLike != "",
		"tokens", strings.Join(searchTokens, ","),
	)
	return searchQuery{
		raw:            query,
		lowered:        strings.ToLower(strings.TrimSpace(query)),
		normalized:     normalized,
		compact:        strings.ReplaceAll(normalized, " ", ""),
		tokens:         searchTokens,
		fragments:      buildSearchFragments(searchTokens),
		navTerm:        navTerm,
		domainLike:     domainLike,
		navigational:   navTerm != "" || domainLike != "",
		siteTypeIntent: siteTypeIntent,
		platformIntent: platformIntent,
	}
}

func searchCandidateLimit(limit, offset int) int {
	want := offset + (limit * 8) + 30
	if want > 500 {
		want = 500
	}
	return want
}

func (s *SQLiteStorage) getSearchIDFMap(ctx context.Context, tokens []string) (map[string]float64, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	var totalDocs float64
	if err := s.readContentDB.QueryRowContext(ctx, `SELECT MAX(rowid) FROM pages_fts`).Scan(&totalDocs); err != nil {
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
	rows, err := s.readContentDB.QueryContext(ctx, query, args...)
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
	if err := s.readContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_fts WHERE pages_fts MATCH ?`, ftsQuery).Scan(&total); err != nil {
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

	domainPrefix := ""
	if domainExact != "" {
		domainPrefix = domainExact + ".%"
	}

	titleExact := query.lowered
	titlePrefix := query.lowered + "%"
	titleContains := "%" + query.lowered + "%"

	scoreExpr := "0.0" +
		" + CASE WHEN LOWER(p.title) = ? THEN 500.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.title) LIKE ? THEN 300.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.title) LIKE ? THEN 180.0 ELSE 0 END" +
		" + CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 1500.0 ELSE 0 END" +
		" + CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 800.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.url) = 'https://' || LOWER(p.domain) || '/' THEN 2000.0 ELSE 0 END" +
		" + CASE WHEN LOWER(p.url) LIKE ? THEN 100.0 ELSE 0 END" +
		" + CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 1 THEN 500.0 ELSE 0 END" +
		" + CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) BETWEEN 2 AND 3 THEN 150.0 ELSE 0 END" +
		" + CASE WHEN p.is_seed = 1 THEN 40.0 ELSE 0 END" +
		" + MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))"

	urlContains := "%" + query.lowered + "%"
	args := []any{titleExact, titlePrefix, titleContains, domainExact, domainExact, domainPrefix, domainPrefix, urlContains}

	var scoreTokens []string
	for _, token := range query.tokens {
		if len([]rune(token)) > 3 {
			scoreTokens = append(scoreTokens, token)
		}
	}
	if len(scoreTokens) == 0 {
		scoreTokens = query.tokens
	}
	if len(scoreTokens) > 4 {
		scoreTokens = scoreTokens[:4]
	}

	for _, token := range scoreTokens {
		scoreExpr += " + CASE WHEN LOWER(p.title) LIKE ? THEN 40.0 ELSE 0 END"
		args = append(args, "%"+token+"%")
		scoreExpr += " + CASE WHEN LOWER(p.h1) LIKE ? THEN 25.0 ELSE 0 END"
		args = append(args, "%"+token+"%")
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
			'' AS snippet,
			p.domain,
			p.language,
			p.region,
			p.crawled_at,
			p.published_at,
			p.updated_at,
			m.sql_score,
			p.is_seed,
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
		
		WHERE pages_fts MATCH ?
		ORDER BY (m.sql_score + m.clicks * 150.0) DESC, m.crawled_at DESC
	`, scoreExpr)
	args = append(args, query.raw, matchQuery, limit, matchQuery)

	rows, err := s.readContentDB.QueryContext(ctx, searchSQL, args...)
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
				CASE WHEN ? != '' AND LOWER(p.domain) = ? THEN 1500.0 ELSE 0 END
				+ CASE WHEN ? != '' AND LOWER(p.domain) LIKE ? THEN 800.0 ELSE 0 END
				+ CASE WHEN LOWER(p.url) = 'https://' || LOWER(p.domain) || '/' THEN 2000.0 ELSE 0 END
				+ CASE WHEN LOWER(p.url) LIKE ? THEN 220.0 ELSE 0 END
				+ CASE WHEN LOWER(p.title) LIKE ? THEN 160.0 ELSE 0 END
				+ CASE WHEN LOWER(p.h1) LIKE ? THEN 120.0 ELSE 0 END
				+ CASE WHEN LOWER(p.h2) LIKE ? THEN 100.0 ELSE 0 END
				+ CASE WHEN LENGTH(p.url) - LENGTH(REPLACE(p.url, '/', '')) <= 3 THEN 500.0 ELSE 0 END
				+ CASE WHEN p.is_seed = 1 THEN 20.0 ELSE 0 END
				+ MAX(0.0, 20.0 - 0.2 * (JULIANDAY('now') - JULIANDAY(SUBSTR(p.crawled_at, 1, 10))))
				+ CASE WHEN ? != '' AND p.language = ? THEN 20.0 ELSE 0 END
			) AS sql_score,
			p.is_seed,
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

	rows, err := s.readContentDB.QueryContext(ctx, querySQL, args...)
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

	titleTokens := uniqueStrings(strings.Fields(titleNorm))
	h1Tokens := uniqueStrings(strings.Fields(h1Norm))
	h2Tokens := uniqueStrings(strings.Fields(h2Norm))
	descTokens := uniqueStrings(strings.Fields(descNorm))
	urlTokens := uniqueStrings(strings.Fields(urlNorm))
	domainTokens := uniqueStrings(append(strings.Fields(domainNorm), strings.Fields(effectiveDomainNorm)...))
	domainTokens = uniqueStrings(append(domainTokens, strings.Fields(rootLabelNorm)...))

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

	titleAvg, titleCoverage, titleExact := searchTokenCoverage(query.tokens, titleTokens)
	h1Avg, h1Coverage, _ := searchTokenCoverage(query.tokens, h1Tokens)
	h2Avg, h2Coverage, _ := searchTokenCoverage(query.tokens, h2Tokens)
	descAvg, descCoverage, _ := searchTokenCoverage(query.tokens, descTokens)
	domainAvg, domainCoverage, _ := searchTokenCoverage(query.tokens, domainTokens)
	urlAvg, urlCoverage, _ := searchTokenCoverage(query.tokens, urlTokens)

	weightedFieldAvg := (3.0*titleAvg + 2.0*h1Avg + 2.0*h2Avg + 2.0*descAvg) / 9.0
	weightedFieldCoverage := (3.0*titleCoverage + 2.0*h1Coverage + 2.0*h2Coverage + 2.0*descCoverage) / 9.0
	fixedTF := fixedWeightedTermFrequency(query.tokens, query.idfMap, titleNorm, h1Norm, h2Norm, descNorm)

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
	score += 560.0*titlePhrase + 360.0*h1Phrase + 360.0*h2Phrase + 240.0*descPhrase
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
			score += 8000.0
			if isHomepage {
				score += 6000.0
			}
		} else if domainInfo.isRootDomain && domainInfo.rootLabel == matchDomain {
			// e.g. query="reddit" → reddit.com, reddit.org, reddit.co.uk (any valid TLD)
			score += 6000.0
			if isHomepage {
				score += 5000.0
			}
		}
	}

	// --- Absolute homepage boost for navigational queries ---
	// When the user is clearly looking for a site (navigational query),
	// the root homepage should always win against any sub-page.
	// This compensates for SPAs where sub-pages may have richer titles.
	if query.navigational && isHomepage && domainPhrase >= 0.75 {
		if domainInfo.isRootDomain {
			score += 15000.0
		} else {
			score += 8000.0
		}
	}

	if query.navigational && domainInfo.isRootDomain && isHomepage && domainPhrase >= 0.95 {
		score += 1200.0
	}
	if query.navigational && !domainInfo.isRootDomain {
		score -= 300.0
	}

	// --- Site-type intent boost (e.g. "wiki warframe", "docs python") ---
	// When the query contains a site-type keyword, boost the homepage of
	// domains/subdomains that include that keyword.
	// Root domains and subdomains get the SAME massive boost so that
	// wiki.warframe.com/ can beat any internal wiki page or competitor.
	if query.siteTypeIntent != "" && isHomepage {
		typeInDomain := strings.Contains(candidate.Domain, query.siteTypeIntent)
		if !typeInDomain {
			typeInDomain = strings.Contains(domainInfo.effectiveDomain, query.siteTypeIntent) ||
				strings.Contains(domainInfo.rootLabel, query.siteTypeIntent)
		}
		if typeInDomain {
			score += 12000.0
		}
	}

	// --- Platform intent boost ---
	// When the query contains a platform name (e.g. "reddit", "youtube"),
	// strongly boost results from that platform's domain. This generalizes
	// the old hardcoded Reddit boost to work for any platform and ensures
	// the boost is strong enough to surface platform results above
	// high-scoring domain matches (e.g. wiki.warframe.com).
	if query.platformIntent != "" {
		if domainInfo.rootLabel == query.platformIntent {
			score += 16000.0
			if isHomepage {
				score += 4000.0
			}
			// Extra boost when the remaining search tokens appear in the URL
			// (e.g. "warframe reddit" → /r/Warframe/... contains "warframe").
			if len(query.tokens) > 0 && urlAvg > 0 {
				score += 3000.0 * urlAvg
			}
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
	case searchModeFTSCore:
		return 70.0
	case searchModeFTSCore2:
		return 50.0
	case searchModeFTSRelaxed:
		return 30.0
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
	if n < 3 || n > 6 {
		return "" // Majority fallback only makes sense for 3-6 words.
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

func buildFTSCoreQuery(tokens []string) string {
	if len(tokens) <= 3 {
		return "" // Only useful for queries with > 3 words
	}

	var filtered []string
	for _, t := range tokens {
		if len([]rune(t)) > 3 {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		filtered = tokens
	}

	ordered := append([]string(nil), filtered...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len([]rune(ordered[i])) > len([]rune(ordered[j]))
	})

	core := ordered
	if len(core) > 3 {
		core = core[:3] // Take the 3 longest words
	}

	parts := make([]string, 0, len(core))
	for _, token := range core {
		parts = append(parts, quoteFTS5Token(token))
	}
	// Implicit AND between bare words in FTS5
	return strings.Join(parts, " ")
}

func buildFTSCore2Query(tokens []string) string {
	if len(tokens) <= 2 {
		return "" // Only useful for queries with > 2 words
	}

	var filtered []string
	for _, t := range tokens {
		if len([]rune(t)) > 3 {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		filtered = tokens
	}

	ordered := append([]string(nil), filtered...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len([]rune(ordered[i])) > len([]rune(ordered[j]))
	})

	core := ordered
	if len(core) > 2 {
		core = core[:2] // Take the 2 longest words
	}

	parts := make([]string, 0, len(core))
	for _, token := range core {
		parts = append(parts, quoteFTS5Token(token))
	}
	// Implicit AND between bare words in FTS5
	return strings.Join(parts, " ")
}

func buildFTSRelaxedQuery(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}

	var filtered []string
	for _, t := range tokens {
		if len([]rune(t)) > 3 {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		filtered = tokens
	}

	ordered := append([]string(nil), filtered...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len([]rune(ordered[i])) > len([]rune(ordered[j]))
	})

	significant := ordered
	if len(significant) > 4 {
		significant = significant[:4] // Limit OR fallback to top 4 longest words
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

func (s *SQLiteStorage) suggestCorrectTerm(ctx context.Context, word string) string {
	word = strings.ToLower(strings.TrimSpace(word))
	runes := []rune(word)
	if len(runes) < 4 {
		return ""
	}

	// We take the first 3 characters as a prefix to search the vocabulary.
	// We can try first 4 characters, but 3 gives more tolerance for typos in 4th char.
	// But 3 chars prefix might match too many terms. Let's try 4 if len > 5.
	prefixLen := 3
	if len(runes) > 6 {
		prefixLen = 4
	}
	prefix := string(runes[:prefixLen])

	// Query vocabulary
	query := `SELECT term, doc FROM pages_fts_vocab WHERE term >= ? AND term < ? ORDER BY doc DESC LIMIT 200`
	endPrefix := prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)

	rows, err := s.readContentDB.QueryContext(ctx, query, prefix, endPrefix)
	if err != nil {
		s.logger.Warn("failed to query vocab for suggestion", "error", err)
		return ""
	}
	defer rows.Close()

	var bestTerm string
	var bestScore float64 = -1.0
	minDist := 100

	maxAllowedDist := 2
	if len(runes) > 8 {
		maxAllowedDist = 3
	}

	for rows.Next() {
		var term string
		var doc int
		if err := rows.Scan(&term, &doc); err != nil {
			continue
		}

		dist := levenshtein(word, term)
		if dist <= maxAllowedDist {
			// Score formula: favor smaller distance heavily, tie-break with doc frequency.
			// Lower distance is better. Higher doc count is better.
			score := 1000.0/float64(dist+1) + float64(doc)
			if dist < minDist || (dist == minDist && score > bestScore) {
				minDist = dist
				bestScore = score
				bestTerm = term
			}
		}
	}

	return bestTerm
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len([]rune(b))
	}
	if len(b) == 0 {
		return len([]rune(a))
	}

	s1 := []rune(a)
	s2 := []rune(b)

	lenS1 := len(s1)
	lenS2 := len(s2)

	d := make([][]int, lenS1+1)
	for i := range d {
		d[i] = make([]int, lenS2+1)
	}

	for i := 0; i <= lenS1; i++ {
		d[i][0] = i
	}
	for j := 0; j <= lenS2; j++ {
		d[0][j] = j
	}

	for i := 1; i <= lenS1; i++ {
		for j := 1; j <= lenS2; j++ {
			cost := 1
			if s1[i-1] == s2[j-1] {
				cost = 0
			}
			d[i][j] = min(
				d[i-1][j]+1,      // deletion
				d[i][j-1]+1,      // insertion
				d[i-1][j-1]+cost, // substitution
			)
		}
	}
	return d[lenS1][lenS2]
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
	if query == "" {
		return nil, 0, nil
	}

	// Count total matches.
	var total int
	countQuery := `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH ?`
	if err := s.readContentDB.QueryRowContext(ctx, countQuery, query).Scan(&total); err != nil {
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

	rows, err := s.readContentDB.QueryContext(ctx, searchQuery, query, limit, offset)
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
// Semantic Similarity
// --------------------------------------------------------------------------

// cosineSimilarity returns the cosine similarity between two vectors (range: -1..1).
// For normalized embeddings (e.g. from Ollama) the result is typically 0..1.
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

// --------------------------------------------------------------------------
// Link / Backlink Operations
// --------------------------------------------------------------------------
