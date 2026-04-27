package storage

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// --------------------------------------------------------------------------
// Adaptive Scoring / Interest Profile
// --------------------------------------------------------------------------

// RecordSearchQuery stores a normalized query and increments term weights.
func (s *SQLiteStorage) RecordSearchQuery(ctx context.Context, query, normalizedQuery, lang string) error {
	_, err := s.writeContentDB.ExecContext(ctx, `
		INSERT INTO search_queries (query, normalized_query, lang)
		VALUES (?, ?, ?)
	`, query, normalizedQuery, lang)
	if err != nil {
		return fmt.Errorf("recording search query: %w", err)
	}

	// Update interest terms from query tokens.
	tokens := normalizeInterestTokens(normalizedQuery)
	for _, tok := range tokens {
		_, err := s.writeContentDB.ExecContext(ctx, `
			INSERT INTO interest_terms (term, weight, source, lang, last_seen)
			VALUES (?, 1.0, 'query', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(term, source) DO UPDATE SET
				weight = weight + 1.0,
				last_seen = CURRENT_TIMESTAMP
		`, tok, lang)
		if err != nil {
			s.logger.Warn("failed to update interest term", "term", tok, "error", err)
		}
	}
	return nil
}

// GetInterestProfile returns the accumulated term weights.
func (s *SQLiteStorage) GetInterestProfile(ctx context.Context) (map[string]float64, error) {
	rows, err := s.readContentDB.QueryContext(ctx, `
		SELECT term, weight FROM interest_terms
	`)
	if err != nil {
		return nil, fmt.Errorf("querying interest terms: %w", err)
	}
	defer rows.Close()

	terms := make(map[string]float64)
	for rows.Next() {
		var term string
		var weight float64
		if err := rows.Scan(&term, &weight); err != nil {
			return nil, fmt.Errorf("scanning interest term: %w", err)
		}
		terms[term] = weight
	}
	return terms, rows.Err()
}

// GetDomainReputations returns the reputation score per domain.
func (s *SQLiteStorage) GetDomainReputations(ctx context.Context) (map[string]float64, error) {
	rows, err := s.readContentDB.QueryContext(ctx, `
		SELECT domain, score FROM domain_relevance
	`)
	if err != nil {
		return nil, fmt.Errorf("querying domain reputations: %w", err)
	}
	defer rows.Close()

	reps := make(map[string]float64)
	for rows.Next() {
		var domain string
		var score float64
		if err := rows.Scan(&domain, &score); err != nil {
			return nil, fmt.Errorf("scanning domain reputation: %w", err)
		}
		reps[domain] = score
	}
	return reps, rows.Err()
}

// GetSearchQueryCount returns the total number of recorded queries.
func (s *SQLiteStorage) GetSearchQueryCount(ctx context.Context) (int, error) {
	var count int
	if err := s.readContentDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM search_queries`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting search queries: %w", err)
	}
	return count, nil
}

// UpdateDomainReputation adjusts the relevance score for a domain.
func (s *SQLiteStorage) UpdateDomainReputation(ctx context.Context, domain string, relevanceDelta float64) error {
	_, err := s.writeContentDB.ExecContext(ctx, `
		INSERT INTO domain_relevance (domain, score, pages_count, last_updated)
		VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(domain) DO UPDATE SET
			score = score + ?,
			pages_count = pages_count + 1,
			last_updated = CURRENT_TIMESTAMP
	`, domain, relevanceDelta, relevanceDelta)
	if err != nil {
		return fmt.Errorf("updating domain reputation for %q: %w", domain, err)
	}
	return nil
}

// BoostTermsFromClick increases term weights based on a clicked result's content.
func (s *SQLiteStorage) BoostTermsFromClick(ctx context.Context, query string, pageURL string, clickWeight float64) error {
	// Fetch the page to extract title, description, schema keywords.
	page, err := s.GetPageByURL(ctx, pageURL)
	if err != nil {
		return fmt.Errorf("fetching page for click boost: %w", err)
	}
	if page == nil {
		return nil // page not yet indexed, skip
	}

	// Combine searchable text.
	text := page.Title + " " + page.Description + " " + page.SchemaKeywords + " " + page.H1
	tokens := normalizeInterestTokens(text)

	for _, tok := range tokens {
		_, err := s.writeContentDB.ExecContext(ctx, `
			INSERT INTO interest_terms (term, weight, source, lang, last_seen)
			VALUES (?, ?, 'click', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(term, source) DO UPDATE SET
				weight = weight + ?,
				last_seen = CURRENT_TIMESTAMP
		`, tok, clickWeight, page.Language, clickWeight)
		if err != nil {
			s.logger.Warn("failed to boost term from click", "term", tok, "error", err)
		}
	}

	// Also boost domain reputation for the clicked page.
	if err := s.UpdateDomainReputation(ctx, page.Domain, clickWeight*0.5); err != nil {
		s.logger.Warn("failed to boost domain reputation from click", "domain", page.Domain, "error", err)
	}

	return nil
}

// normalizeInterestTokens lowercases and splits text into simple tokens.
func normalizeInterestTokens(s string) []string {
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
