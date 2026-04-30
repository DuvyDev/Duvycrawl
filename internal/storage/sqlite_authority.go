package storage

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// --------------------------------------------------------------------------
// Domain Authority — Tranco ranking + corpus page-count caches
// --------------------------------------------------------------------------

// authorityCache holds in-memory caches for domain authority signals so
// that search-time scoring does not hit the database per-candidate.
type authorityCache struct {
	trancoMu   sync.RWMutex
	trancoRank map[string]int // domain → tranco rank (1 = best)

	pageCntMu sync.RWMutex
	pageCnt   map[string]int // domain → number of indexed pages

	// Configurable weights (set via SetAuthorityWeights).
	trancoWeight      float64
	corpusCountWeight float64
}

// initAuthorityCache creates and populates the authority caches.
// Called once during storage initialisation.
func (s *SQLiteStorage) initAuthorityCache(ctx context.Context) {
	s.authority = &authorityCache{
		trancoRank: make(map[string]int),
		pageCnt:    make(map[string]int),
	}
	s.refreshTrancoCache(ctx)
	s.refreshPageCountCache(ctx)
}

// refreshTrancoCache loads the entire domain_authority table into memory.
func (s *SQLiteStorage) refreshTrancoCache(ctx context.Context) {
	rows, err := s.readContentDB.QueryContext(ctx, `SELECT domain, tranco_rank FROM domain_authority`)
	if err != nil {
		s.logger.Warn("failed to load tranco cache", "error", err)
		return
	}
	defer rows.Close()

	m := make(map[string]int, 100_000)
	for rows.Next() {
		var domain string
		var rank int
		if err := rows.Scan(&domain, &rank); err == nil && rank > 0 {
			m[domain] = rank
		}
	}

	s.authority.trancoMu.Lock()
	s.authority.trancoRank = m
	s.authority.trancoMu.Unlock()

	if len(m) > 0 {
		s.logger.Info("tranco authority cache loaded", "domains", len(m))
	}
}

// refreshPageCountCache loads per-domain page counts from content.db.
func (s *SQLiteStorage) refreshPageCountCache(ctx context.Context) {
	rows, err := s.readContentDB.QueryContext(ctx, `SELECT domain, COUNT(*) FROM pages GROUP BY domain`)
	if err != nil {
		s.logger.Warn("failed to load page count cache", "error", err)
		return
	}
	defer rows.Close()

	m := make(map[string]int, 10_000)
	for rows.Next() {
		var domain string
		var cnt int
		if err := rows.Scan(&domain, &cnt); err == nil {
			m[domain] += cnt
			// Also accumulate the count for the root domain (eTLD+1)
			if root, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && root != domain {
				m[root] += cnt
			}
		}
	}

	s.authority.pageCntMu.Lock()
	s.authority.pageCnt = m
	s.authority.pageCntMu.Unlock()

	s.logger.Debug("page count cache loaded", "domains", len(m))
}

// GetTrancoRank returns the Tranco rank for a domain (0 = not found).
func (s *SQLiteStorage) GetTrancoRank(domain string) int {
	if s.authority == nil {
		return 0
	}
	// Normalise: strip "www." prefix to match Tranco format.
	domain = strings.TrimPrefix(domain, "www.")

	s.authority.trancoMu.RLock()
	rank := s.authority.trancoRank[domain]
	s.authority.trancoMu.RUnlock()

	// If the exact subdomain isn't found, fallback to the root domain (e.g. es.wikipedia.org -> wikipedia.org)
	if rank == 0 {
		if root, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && root != domain {
			s.authority.trancoMu.RLock()
			rank = s.authority.trancoRank[root]
			s.authority.trancoMu.RUnlock()
		}
	}

	return rank
}

// GetCorpusPageCount returns the number of indexed pages for a domain.
func (s *SQLiteStorage) GetCorpusPageCount(domain string) int {
	if s.authority == nil {
		return 0
	}
	s.authority.pageCntMu.RLock()
	cnt := s.authority.pageCnt[domain]
	s.authority.pageCntMu.RUnlock()

	// If the exact subdomain has no pages, fallback to the root domain's aggregate count.
	// Note: refreshPageCountCache already accumulates subdomain counts into the root domain.
	if cnt == 0 {
		if root, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && root != domain {
			s.authority.pageCntMu.RLock()
			cnt = s.authority.pageCnt[root]
			s.authority.pageCntMu.RUnlock()
		}
	}

	return cnt
}

// ImportTrancoCSV reads a Tranco CSV (format: "rank,domain") and bulk-inserts
// into domain_authority. Returns the number of rows imported.
func (s *SQLiteStorage) ImportTrancoCSV(ctx context.Context, r io.Reader) (int, error) {
	tx, err := s.writeContentDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tranco import tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO domain_authority (domain, tranco_rank, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare tranco insert: %w", err)
	}
	defer stmt.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256), 1024)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		rank, err := strconv.Atoi(parts[0])
		if err != nil || rank <= 0 {
			continue
		}
		domain := strings.TrimSpace(parts[1])
		if domain == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, domain, rank); err != nil {
			s.logger.Warn("tranco insert failed", "domain", domain, "error", err)
			continue
		}
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("scanning tranco CSV: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing tranco import: %w", err)
	}

	// Refresh in-memory cache after import.
	s.refreshTrancoCache(ctx)

	return count, nil
}

// TrancoCacheSize returns the number of entries in the authority cache.
func (s *SQLiteStorage) TrancoCacheSize() int {
	if s.authority == nil {
		return 0
	}
	s.authority.trancoMu.RLock()
	defer s.authority.trancoMu.RUnlock()
	return len(s.authority.trancoRank)
}

// --------------------------------------------------------------------------
// Tranco CSV download + extraction
// --------------------------------------------------------------------------

// EnsureTrancoFile downloads and extracts the Tranco CSV into dataDir if the
// CSV does not already exist. The ZIP is deleted after extraction.
// Returns the path to the CSV file and whether it was freshly downloaded.
func EnsureTrancoFile(ctx context.Context, dataDir, downloadURL string, logger *slog.Logger) (string, bool, error) {
	csvPath := filepath.Join(dataDir, "top-1m.csv")
	if _, err := os.Stat(csvPath); err == nil {
		// Already exists.
		return csvPath, false, nil
	}

	logger.Info("downloading Tranco domain ranking list", "url", downloadURL)
	start := time.Now()

	zipPath := filepath.Join(dataDir, "top-1m.csv.zip")
	if err := downloadFile(ctx, downloadURL, zipPath); err != nil {
		return "", false, fmt.Errorf("downloading tranco zip: %w", err)
	}

	// Extract CSV from ZIP.
	if err := extractFirstCSV(zipPath, csvPath); err != nil {
		os.Remove(zipPath)
		return "", false, fmt.Errorf("extracting tranco csv: %w", err)
	}

	// Clean up ZIP.
	os.Remove(zipPath)

	logger.Info("tranco list downloaded and extracted",
		"path", csvPath,
		"duration", time.Since(start).Round(time.Millisecond),
	)

	return csvPath, true, nil
}

// LoadTrancoIntoStorage reads the CSV file and imports it into the database,
// but only if the domain_authority table is empty (first run).
func LoadTrancoIntoStorage(ctx context.Context, store *SQLiteStorage, csvPath string, logger *slog.Logger) error {
	// Check if already populated.
	if store.TrancoCacheSize() > 0 {
		return nil
	}

	f, err := os.Open(csvPath)
	if err != nil {
		return fmt.Errorf("opening tranco csv %q: %w", csvPath, err)
	}
	defer f.Close()

	start := time.Now()
	count, err := store.ImportTrancoCSV(ctx, f)
	if err != nil {
		return fmt.Errorf("importing tranco csv: %w", err)
	}

	logger.Info("tranco domain authority imported",
		"domains", count,
		"duration", time.Since(start).Round(time.Millisecond),
	)
	return nil
}

// downloadFile fetches url and writes it to dst.
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// extractFirstCSV opens a ZIP file and extracts the first .csv file to dst.
func extractFirstCSV(zipPath, dst string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening %q in zip: %w", f.Name, err)
		}
		defer rc.Close()

		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, rc); err != nil {
			os.Remove(dst)
			return err
		}
		return nil
	}

	return fmt.Errorf("no .csv file found in zip")
}

// SetAuthorityWeights configures the scoring multipliers for authority signals.
func (s *SQLiteStorage) SetAuthorityWeights(trancoWeight, corpusCountWeight float64) {
	if s.authority == nil {
		return
	}
	s.authority.trancoWeight = trancoWeight
	s.authority.corpusCountWeight = corpusCountWeight
	s.logger.Info("authority weights configured",
		"tranco_weight", trancoWeight,
		"corpus_count_weight", corpusCountWeight,
	)
}

// enrichAuthorityCandidates populates trancoRank and corpusPages on each
// candidate from the in-memory caches. Must be called before reranking.
func (s *SQLiteStorage) enrichAuthorityCandidates(candidates []searchCandidate) {
	if s.authority == nil {
		return
	}
	for i := range candidates {
		candidates[i].trancoRank = s.GetTrancoRank(candidates[i].Domain)
		candidates[i].corpusPages = s.GetCorpusPageCount(candidates[i].Domain)
	}
}

// DataDir returns the storage data directory path.
func (s *SQLiteStorage) DataDir() string {
	return s.dataDir
}
