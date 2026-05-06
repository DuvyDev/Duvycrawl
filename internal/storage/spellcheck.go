package storage

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// SpellChecker provides fast, in-memory fuzzy spell checking using a trigram index.
// It loads common terms from the FTS vocabulary at startup to allow sub-millisecond
// typo correction for search queries.
type SpellChecker struct {
	words    []string
	trigrams map[string][]uint32
	docs     []int
	ready    bool
	mu       sync.RWMutex
	logger   *slog.Logger
}

// NewSpellChecker creates a new uninitialized SpellChecker.
func NewSpellChecker(logger *slog.Logger) *SpellChecker {
	return &SpellChecker{
		trigrams: make(map[string][]uint32),
		logger:   logger.With("component", "spellchecker"),
	}
}

// LoadVocabulary queries the FTS vocabulary from the database and builds the
// trigram index in memory.
func (sc *SpellChecker) LoadVocabulary(db *sql.DB) {
	start := time.Now()

	// We only load terms that appear in at least 3 documents to avoid learning
	// typos from spam pages. We also ignore very short terms.
	query := `SELECT term, doc FROM pages_fts_vocab WHERE doc >= 3 AND LENGTH(term) >= 4`
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		sc.logger.Error("failed to query vocab for spell checker", "error", err)
		return
	}
	defer rows.Close()

	var tempWords []string
	var tempDocs []int
	tempTrigrams := make(map[string][]uint32)

	var idx uint32 = 0
	for rows.Next() {
		var term string
		var doc int
		if err := rows.Scan(&term, &doc); err != nil {
			continue
		}

		tempWords = append(tempWords, term)
		tempDocs = append(tempDocs, doc)

		// Generate trigrams and map them to the word's index
		runes := []rune(term)
		for i := 0; i+3 <= len(runes); i++ {
			tg := string(runes[i : i+3])
			tempTrigrams[tg] = append(tempTrigrams[tg], idx)
		}
		idx++
	}

	if err := rows.Err(); err != nil {
		sc.logger.Error("error iterating vocab rows", "error", err)
		return
	}

	sc.mu.Lock()
	sc.words = tempWords
	sc.docs = tempDocs
	sc.trigrams = tempTrigrams
	sc.ready = true
	sc.mu.Unlock()

	sc.logger.Info("spell checker index loaded",
		"words", len(tempWords),
		"trigrams", len(tempTrigrams),
		"duration", time.Since(start),
	)
}

// StartPeriodicReload starts a background goroutine that reloads the vocabulary
// periodically so that newly crawled words are included in the spell checker
// without needing to restart the server.
func (sc *SpellChecker) StartPeriodicReload(ctx context.Context, db *sql.DB, interval time.Duration) {
	// Initial load
	sc.LoadVocabulary(db)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sc.LoadVocabulary(db)
			}
		}
	}()
}

// Suggest finds the closest vocabulary word to the given input using the
// trigram index to quickly find candidates, and Levenshtein distance to
// accurately rank them. Returns empty string if no good match is found.
func (sc *SpellChecker) Suggest(word string) string {
	sc.mu.RLock()
	if !sc.ready {
		sc.mu.RUnlock()
		return ""
	}
	sc.mu.RUnlock()

	word = strings.ToLower(strings.TrimSpace(word))
	runes := []rune(word)
	if len(runes) < 4 {
		return ""
	}

	// 1. Generate trigrams of the input word
	var inputTrigrams []string
	for i := 0; i+3 <= len(runes); i++ {
		inputTrigrams = append(inputTrigrams, string(runes[i:i+3]))
	}
	if len(inputTrigrams) == 0 {
		return ""
	}

	// 2. Find candidates sharing the most trigrams
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	matches := make(map[uint32]int)
	for _, tg := range inputTrigrams {
		if ids, ok := sc.trigrams[tg]; ok {
			for _, id := range ids {
				matches[id]++
			}
		}
	}

	if len(matches) == 0 {
		return ""
	}

	// 3. Keep top N candidates to avoid slow Levenshtein calculations on thousands of words
	type candidate struct {
		id    uint32
		count int
	}
	var topCandidates []candidate
	for id, count := range matches {
		topCandidates = append(topCandidates, candidate{id, count})
	}

	sort.Slice(topCandidates, func(i, j int) bool {
		return topCandidates[i].count > topCandidates[j].count
	})

	limit := 100
	if len(topCandidates) > limit {
		topCandidates = topCandidates[:limit]
	}

	// 4. Calculate exact Levenshtein distance on top candidates
	var bestTerm string
	var bestScore float64 = -1.0
	minDist := 100

	maxAllowedDist := 2
	if len(runes) >= 7 {
		maxAllowedDist = 3
	}

	for _, cand := range topCandidates {
		term := sc.words[cand.id]
		doc := sc.docs[cand.id]

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

// levenshtein calculates the Levenshtein distance between two strings.
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
