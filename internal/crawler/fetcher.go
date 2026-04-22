package crawler

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// FetchResult contains the raw result of an HTTP fetch operation.
type FetchResult struct {
	StatusCode  int
	Body        []byte
	ContentType string
	FinalURL    string // After redirects
	Duration    time.Duration
}

// Fetcher handles HTTP requests for downloading web pages.
// It is configured with timeouts, size limits, and user agent settings.
type Fetcher struct {
	client        *http.Client
	userAgent     string
	maxBodySizeKB int
	logger        *slog.Logger
}

// NewFetcher creates a new HTTP fetcher with the given configuration.
func NewFetcher(userAgent string, timeout time.Duration, maxBodySizeKB int, logger *slog.Logger) *Fetcher {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: false},
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    false,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}

	return &Fetcher{
		client:        client,
		userAgent:     userAgent,
		maxBodySizeKB: maxBodySizeKB,
		logger:        logger.With("component", "fetcher"),
	}
}

// Fetch downloads the content at the given URL.
// It validates the Content-Type and enforces size limits.
// Returns a FetchResult or an error.
func (f *Fetcher) Fetch(ctx context.Context, targetURL string) (*FetchResult, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %q: %w", targetURL, err)
	}

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,es;q=0.8")
	req.Header.Set("Connection", "keep-alive")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %q: %w", targetURL, err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	// Check Content-Type: we only process HTML pages.
	contentType := resp.Header.Get("Content-Type")
	if !isHTMLContentType(contentType) {
		return &FetchResult{
			StatusCode:  resp.StatusCode,
			ContentType: contentType,
			FinalURL:    resp.Request.URL.String(),
			Duration:    duration,
		}, fmt.Errorf("non-HTML content type: %s", contentType)
	}

	// Read body with size limit to prevent downloading huge files.
	maxBytes := int64(f.maxBodySizeKB) * 1024
	limitedReader := io.LimitReader(resp.Body, maxBytes+1)

	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("reading body from %q: %w", targetURL, err)
	}

	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("page %q exceeds max size of %d KB", targetURL, f.maxBodySizeKB)
	}

	return &FetchResult{
		StatusCode:  resp.StatusCode,
		Body:        body,
		ContentType: contentType,
		FinalURL:    resp.Request.URL.String(),
		Duration:    duration,
	}, nil
}

// isHTMLContentType returns true if the Content-Type indicates an HTML document.
func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml")
}
