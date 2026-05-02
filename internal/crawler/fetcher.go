package crawler

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"golang.org/x/sync/singleflight"
)

// FetchResult contains the raw result of an HTTP fetch operation.
type FetchResult struct {
	StatusCode  int
	Body        []byte
	ContentType string
	FinalURL    string // After redirects
	Duration    time.Duration
	Truncated   bool // True if body exceeded max size and was truncated
}

// Fetcher handles HTTP requests for downloading web pages.
// It is configured with timeouts, size limits, cookie handling, retry logic,
// adaptive per-host backoff for throttled responses, and an optimized
// connection pool for high-throughput crawling.
type Fetcher struct {
	client        *http.Client
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

// -----------------------------------------------------------------------
// DNS cache with TTL — avoids redundant lookups when 300 workers hit
// the same domains. Uses singleflight to deduplicate concurrent queries.
// -----------------------------------------------------------------------

const dnsCacheTTL = 5 * time.Minute

type dnsCacheEntry struct {
	addrs   []string
	expires time.Time
}

type dnsCache struct {
	entries sync.Map            // string -> *dnsCacheEntry
	group   singleflight.Group
}

func (c *dnsCache) lookup(ctx context.Context, host string) ([]string, error) {
	// Check cache.
	if v, ok := c.entries.Load(host); ok {
		entry := v.(*dnsCacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.addrs, nil
		}
		c.entries.Delete(host)
	}

	// Deduplicate concurrent lookups for the same host.
	result, err, _ := c.group.Do(host, func() (interface{}, error) {
		resolver := &net.Resolver{}
		addrs, err := resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
		c.entries.Store(host, &dnsCacheEntry{
			addrs:   addrs,
			expires: time.Now().Add(dnsCacheTTL),
		})
		return addrs, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]string), nil
}

// newCachingDialer wraps a net.Dialer with a DNS cache so that repeated
// connections to the same host reuse resolved addresses.
func newCachingDialer(baseDialer *net.Dialer, cache *dnsCache) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			// Fallback if addr doesn't have port.
			return baseDialer.DialContext(ctx, network, addr)
		}

		// Skip cache for IP literals.
		if net.ParseIP(host) != nil {
			return baseDialer.DialContext(ctx, network, addr)
		}

		addrs, err := cache.lookup(ctx, host)
		if err != nil || len(addrs) == 0 {
			// Fallback to standard resolution on error.
			return baseDialer.DialContext(ctx, network, addr)
		}

		// Try resolved addresses in order.
		var lastErr error
		for _, ip := range addrs {
			conn, err := baseDialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// NewFetcher creates a new HTTP fetcher with the given configuration.
func NewFetcher(userAgent string, timeout time.Duration, maxBodySizeKB, maxRetries, maxIdleConnsPerHost int, disableCookies bool, proxyURL string, logger *slog.Logger) *Fetcher {
	baseDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	cache := &dnsCache{}

	transport := &http.Transport{
		DialContext: newCachingDialer(baseDialer, cache),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			Renegotiation:      tls.RenegotiateOnceAsClient,
			MinVersion:         tls.VersionTLS10,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConnsPerHost:       maxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}

	if proxyURL != "" {
		parsedProxy, err := url.Parse(proxyURL)
		if err == nil {
			// SOCKS5 proxy (including socks5h:// for remote DNS resolution).
			if parsedProxy.Scheme == "socks5" || parsedProxy.Scheme == "socks5h" {
				dialer, err := proxy.FromURL(parsedProxy, proxy.Direct)
				if err == nil {
					transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
						return dialer.Dial(network, addr)
					}
					logger.Info("using socks5 proxy", "url", proxyURL)
				} else {
					logger.Warn("failed to create socks5 dialer, ignoring proxy", "url", proxyURL, "error", err)
				}
			} else {
				transport.Proxy = http.ProxyURL(parsedProxy)
				logger.Info("using http proxy", "url", proxyURL)
			}
		} else {
			logger.Warn("invalid proxy URL, ignoring", "url", proxyURL, "error", err)
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			// Preserve headers on redirect (like Colly does).
			if len(via) > 0 {
				lastReq := via[len(via)-1]
				for k, v := range lastReq.Header {
					// Skip headers that should not be blindly forwarded.
					if k == "Cookie" {
						continue
					}
					req.Header[k] = v
				}
				// Only copy Authorization if same host (security).
				if lastReq.URL.Host == req.URL.Host {
					if auth := lastReq.Header.Get("Authorization"); auth != "" {
						req.Header.Set("Authorization", auth)
					}
				}
			}
			return nil
		},
	}

	if !disableCookies {
		jar, _ := cookiejar.New(nil)
		client.Jar = jar
		logger.Info("cookie jar enabled")
	} else {
		logger.Info("cookie jar disabled")
	}

	return &Fetcher{
		client:        client,
		userAgent:     userAgent,
		maxBodySizeKB: maxBodySizeKB,
		maxRetries:    maxRetries,
		logger:        logger.With("component", "fetcher"),
		hostBackoff:   initHostBackoff(),
	}
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

		result, err := f.fetchOnce(ctx, targetURL)
		lastErr = err

		if err == nil {
			if result.StatusCode == http.StatusTooManyRequests || result.StatusCode == http.StatusServiceUnavailable {
				f.logger.Warn("throttled response, backing off",
					"url", targetURL,
					"status", result.StatusCode,
				)
				f.recordThrottle(host)
				continue
			}
			f.recordSuccess(host)
			return result, nil
		}

		if result != nil && result.StatusCode >= 400 && result.StatusCode < 500 {
			if result.StatusCode != http.StatusTooManyRequests {
				return nil, err
			}
			f.recordThrottle(host)
		}
	}

	return nil, fmt.Errorf("after %d attempts: %w", f.maxRetries+1, lastErr)
}

// fetchOnce performs a single HTTP GET request.
func (f *Fetcher) fetchOnce(ctx context.Context, targetURL string) (*FetchResult, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %q: %w", targetURL, err)
	}

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// NOTE: Do NOT manually set Accept-Encoding. Go's http.Transport with
	// DisableCompression=false automatically adds "Accept-Encoding: gzip" and
	// transparently decompresses the response body. Manually setting it disables
	// automatic decompression, leaving us with binary gzip data that goquery
	// cannot parse (resulting in empty titles and zero links/images).
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %q: %w", targetURL, err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	// Check Content-Type: reject known binary types (images, audio, video,
	// fonts, archives) but accept HTML, XML, JSON, JS, CSS, and anything
	// text-based so the parser can discover URLs from non-HTML responses.
	contentType := resp.Header.Get("Content-Type")
	if isBinaryContentType(contentType) {
		return &FetchResult{
			StatusCode:  resp.StatusCode,
			ContentType: contentType,
			FinalURL:    resp.Request.URL.String(),
			Duration:    duration,
		}, fmt.Errorf("binary content type: %s", contentType)
	}

	// Read body with size limit to prevent downloading huge files.
	// If the page exceeds the limit, we keep what we have (truncated) so
	// that metadata, title, and outbound links can still be extracted.
	maxBytes := int64(f.maxBodySizeKB) * 1024
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
	}, nil
}

// isHTMLContentType returns true if the Content-Type indicates an HTML document.
func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml")
}

// isBinaryContentType returns true if the Content-Type indicates a binary
// resource that cannot contain useful links (images, audio, video, fonts,
// archives, executables). Text-based types (HTML, XML, JSON, JS, CSS,
// plain text) are allowed through so the parser can extract URLs from them.
func isBinaryContentType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, prefix := range []string{
		"image/", "audio/", "video/", "font/",
		"application/octet-stream", "application/pdf",
		"application/zip", "application/gzip",
		"application/x-tar", "application/x-rar",
		"application/x-7z", "application/vnd.ms-",
		"application/vnd.openxmlformats",
	} {
		if strings.HasPrefix(ct, prefix) || strings.Contains(ct, prefix) {
			return true
		}
	}
	return false
}
