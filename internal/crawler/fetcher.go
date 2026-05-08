package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrUnsupportedContentType is returned when the fetched URL has a content type we don't want to process.
var ErrUnsupportedContentType = errors.New("unsupported content type")

// FetchResult contains the raw result of an HTTP fetch operation.
type FetchResult struct {
	StatusCode   int
	Body         []byte
	ContentType  string
	FinalURL     string // After redirects
	Duration     time.Duration
	Truncated    bool // True if body exceeded max size and was truncated
	Mode         string
	RenderReason string
}

// Fetcher handles HTTP requests for downloading web pages.
// It is configured with timeouts, size limits, cookie handling, retry logic,
// adaptive per-host backoff for throttled responses, and an optimized
// connection pool for high-throughput crawling.
type Fetcher struct {
	proxyManager  *ProxyManager
	userAgent     string
	maxBodySizeKB int
	maxRetries    int
	logger        *slog.Logger

	hostBackoffMu sync.Mutex
	hostBackoff   map[string]*hostBackoffEntry
}

type hostBackoffEntry struct {
	consecutive4xx int
	nextAllowed    time.Time
}

const (
	backoffBase   = 2 * time.Second
	backoffMax    = 60 * time.Second
	backoffJitter = 500 * time.Millisecond
)

func initHostBackoff() map[string]*hostBackoffEntry {
	return make(map[string]*hostBackoffEntry)
}

func (f *Fetcher) getHostBackoff(host string) *hostBackoffEntry {
	f.hostBackoffMu.Lock()
	defer f.hostBackoffMu.Unlock()
	entry, ok := f.hostBackoff[host]
	if !ok {
		entry = &hostBackoffEntry{}
		f.hostBackoff[host] = entry
	}
	return entry
}

func (f *Fetcher) recordSuccess(host string) {
	f.hostBackoffMu.Lock()
	defer f.hostBackoffMu.Unlock()
	if entry, ok := f.hostBackoff[host]; ok {
		entry.consecutive4xx = 0
	}
}

func (f *Fetcher) recordThrottle(host string) {
	f.hostBackoffMu.Lock()
	defer f.hostBackoffMu.Unlock()
	entry, ok := f.hostBackoff[host]
	if !ok {
		entry = &hostBackoffEntry{}
		f.hostBackoff[host] = entry
	}
	entry.consecutive4xx++
	jitter := time.Duration(rand.Int63n(int64(2*backoffJitter))) - backoffJitter
	delay := backoffBase * time.Duration(1<<min(entry.consecutive4xx, 5))
	delay += jitter
	if delay > backoffMax {
		delay = backoffMax
	}
	entry.nextAllowed = time.Now().Add(delay)
}

// NewFetcher creates a new HTTP fetcher with the given configuration.
func NewFetcher(userAgent string, maxBodySizeKB, maxRetries int, proxyManager *ProxyManager, logger *slog.Logger) *Fetcher {
	return &Fetcher{
		proxyManager:  proxyManager,
		userAgent:     userAgent,
		maxBodySizeKB: maxBodySizeKB,
		maxRetries:    maxRetries,
		logger:        logger.With("component", "fetcher"),
		hostBackoff:   initHostBackoff(),
	}
}

// Close releases resources held by the fetcher.
func (f *Fetcher) Close() error {
	// ProxyManager handles closing DNS caches.
	return nil
}

// Fetch downloads the content at the given URL with automatic retries
// and adaptive per-host backoff for throttled responses (429, 503).
func (f *Fetcher) Fetch(ctx context.Context, targetURL string) (*FetchResult, error) {
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL %q: %w", targetURL, err)
	}
	host := parsedURL.Hostname()

	var lastErr error
	var lastResult *FetchResult

	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		if attempt > 0 {
			backoffEntry := f.getHostBackoff(host)
			f.hostBackoffMu.Lock()
			waitUntil := backoffEntry.nextAllowed
			f.hostBackoffMu.Unlock()
			if !waitUntil.IsZero() {
				wait := time.Until(waitUntil)
				if wait > 0 {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(wait):
					}
				}
			}

			backoff := time.Duration(attempt) * time.Second
			f.logger.Debug("retrying request",
				"url", targetURL,
				"attempt", attempt,
				"backoff", backoff,
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		pClient := f.proxyManager.GetClient()
		result, err := f.fetchOnce(ctx, pClient.Client, targetURL)
		lastErr = err
		if result != nil {
			lastResult = result
		}

		if err == nil {
			if result.StatusCode == http.StatusTooManyRequests || result.StatusCode == http.StatusServiceUnavailable {
				if attempt == f.maxRetries {
					f.proxyManager.ResetFailures(pClient)
					return result, nil
				}
				f.logger.Warn("throttled response, backing off",
					"url", targetURL,
					"status", result.StatusCode,
				)
				f.recordThrottle(host)
				continue
			}
			f.proxyManager.ResetFailures(pClient)
			f.recordSuccess(host)
			return result, nil
		}

		if errors.Is(err, ErrUnsupportedContentType) {
			// The proxy worked perfectly and the server responded.
			// This is not a fetch failure, but we shouldn't index it and shouldn't retry.
			f.proxyManager.ResetFailures(pClient)
			f.recordSuccess(host)
			return nil, err
		}

		if result != nil && result.StatusCode >= 400 && result.StatusCode < 500 {
			if result.StatusCode != http.StatusTooManyRequests {
				return nil, err
			}
			f.recordThrottle(host)
		} else if err != nil {
			// Mark proxy as down if there's a network-level error (e.g. connection refused)
			// Don't mark down for timeouts, cancellations, or target-specific drops (EOF, connection reset)
			// because they are often site-specific WAF/rate-limits and not the proxy's fault.
			isTargetDrop := errors.Is(err, io.EOF) || 
				errors.Is(err, io.ErrUnexpectedEOF) || 
				errors.Is(err, syscall.ECONNRESET) ||
				strings.HasSuffix(err.Error(), "EOF") ||
				strings.Contains(err.Error(), "connection reset by peer")

			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !isTargetDrop {
				f.proxyManager.MarkDown(pClient)
			}
		}
	}
	if lastResult != nil && lastErr == nil {
		return lastResult, nil
	}

	return nil, fmt.Errorf("after %d attempts: %w", f.maxRetries+1, lastErr)
}

// fetchOnce performs a single HTTP GET request using the provided client.
func (f *Fetcher) fetchOnce(ctx context.Context, client *http.Client, targetURL string) (*FetchResult, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %q: %w", targetURL, err)
	}

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// NOTE: Do NOT manually set Accept-Encoding. Go's http.Transport with
	// DisableCompression=false automatically adds "Accept-Encoding: gzip" and
	// transparently decompresses the response body. Manually setting it disables
	// automatic decompression, leaving us with binary gzip data that goquery
	// cannot parse (resulting in empty titles and zero links/images).
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	// Browser-like client hints and sec-fetch headers help bypass basic WAF
	// checks (e.g. Cloudflare "Just a moment..." interstitials).
	// These MUST stay consistent with the configured User-Agent (platform + version).
	req.Header.Set("Sec-CH-UA", `"Chromium";v="147", "Not:A-Brand";v="2", "Google Chrome";v="147"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %q: %w", targetURL, err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	// Check Content-Type: allow HTML and selected discovery-only types.
	contentType := resp.Header.Get("Content-Type")
	isHTML, isDiscovery := isAllowedContentType(contentType)
	if !isHTML && !isDiscovery {
		return &FetchResult{
			StatusCode:  resp.StatusCode,
			ContentType: contentType,
			FinalURL:    resp.Request.URL.String(),
			Duration:    duration,
			Mode:        "http",
		}, fmt.Errorf("%w: %s", ErrUnsupportedContentType, contentType)
	}

	// Read body with size limit to prevent downloading huge files.
	// For non-HTML discovery assets use a tighter cap to protect memory.
	maxBytes := int64(f.maxBodySizeKB) * 1024
	if !isHTML {
		const maxDiscoveryKB = 256
		if maxBytes > maxDiscoveryKB*1024 {
			maxBytes = maxDiscoveryKB * 1024
		}
	}
	limitedReader := io.LimitReader(resp.Body, maxBytes)

	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("reading body from %q: %w", targetURL, err)
	}

	// Drain the rest of the body to reuse the connection, but discard it.
	discarded, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	truncated := discarded > 0

	return &FetchResult{
		StatusCode:  resp.StatusCode,
		Body:        body,
		ContentType: contentType,
		FinalURL:    resp.Request.URL.String(),
		Duration:    duration,
		Truncated:   truncated,
		Mode:        "http",
	}, nil
}

// isHTMLContentType returns true if the Content-Type indicates an HTML document.
func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml")
}

// isAllowedContentType returns whether a Content-Type is HTML indexable and/or
// allowed for discovery-only crawling.
func isAllowedContentType(ct string) (isHTML bool, isDiscovery bool) {
	ct = strings.ToLower(ct)
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml") {
		return true, true
	}
	discoveryTypes := []string{
		"text/css",
		"application/javascript", "text/javascript", "application/x-javascript", "application/ecmascript",
		"application/json", "application/ld+json",
		"application/xml", "text/xml", "application/rss+xml", "application/atom+xml",
	}
	for _, d := range discoveryTypes {
		if strings.Contains(ct, d) {
			return false, true
		}
	}
	return false, false
}
