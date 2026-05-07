package render

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/DuvyDev/Duvycrawl/internal/urlfilter"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// ErrRendererBusy means all browser slots are occupied and the caller should
// keep crawling instead of tying up a crawler worker waiting for Chromium.
var ErrRendererBusy = errors.New("renderer busy")

// Result contains the raw HTML and metadata produced by a browser render.
type Result struct {
	StatusCode  int
	Body        []byte
	ContentType string
	FinalURL    string
	Duration    time.Duration
	Truncated   bool
}

// Renderer renders JavaScript-heavy pages with a real browser.
type Renderer interface {
	Render(ctx context.Context, targetURL string) (*Result, error)
	Close() error
}

// BrowserRenderer is a small CDP-backed renderer with a global tab semaphore.
type BrowserRenderer struct {
	cfg       config.RenderingConfig
	userAgent string
	logger    *slog.Logger
	sem       chan struct{}
	allocCtx  context.Context
	cancel    context.CancelFunc
}

// NewBrowserRenderer creates a CDP renderer. It does not launch Chrome until the
// first Render call, which keeps HTTP-only startup cheap.
func NewBrowserRenderer(cfg config.RenderingConfig, userAgent string, proxyURL string, logger *slog.Logger) (*BrowserRenderer, error) {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}

	baseCtx := context.Background()
	var allocCtx context.Context
	var cancel context.CancelFunc

	if cfg.BrowserWSURL != "" {
		wsURL := cfg.BrowserWSURL
		if proxyURL != "" {
			launchJSON, _ := json.Marshal(map[string]interface{}{
				"args": []string{"--proxy-server=" + proxyURL},
			})
			wsURL = fmt.Sprintf("%s?launch=%s", wsURL, base64.StdEncoding.EncodeToString(launchJSON))
		}
		allocCtx, cancel = chromedp.NewRemoteAllocator(baseCtx, wsURL, chromedp.NoModifyURL)
	} else {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-background-networking", true),
			chromedp.Flag("disable-sync", true),
			chromedp.Flag("mute-audio", true),
		)
		if proxyURL != "" {
			opts = append(opts, chromedp.Flag("proxy-server", proxyURL))
		}
		if cfg.BlockImages {
			opts = append(opts, chromedp.Flag("blink-settings", "imagesEnabled=false"))
		}
		if cfg.ExecutablePath != "" {
			opts = append(opts, chromedp.ExecPath(cfg.ExecutablePath))
		}
		allocCtx, cancel = chromedp.NewExecAllocator(baseCtx, opts...)
	}

	return &BrowserRenderer{
		cfg:       cfg,
		userAgent: userAgent,
		logger:    logger.With("component", "renderer"),
		sem:       make(chan struct{}, cfg.MaxConcurrency),
		allocCtx:  allocCtx,
		cancel:    cancel,
	}, nil
}

func (r *BrowserRenderer) Render(ctx context.Context, targetURL string) (*Result, error) {
	timeout := r.cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	acquireWait := r.cfg.AcquireTimeout
	if acquireWait > timeout {
		acquireWait = timeout
	}
	if acquireWait <= 0 {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, ErrRendererBusy
		}
	} else {
		acquireCtx, acquireCancel := context.WithTimeout(ctx, acquireWait)
		defer acquireCancel()

		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-acquireCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrRendererBusy
		}
	}

	tabCtx, tabCancel := chromedp.NewContext(r.allocCtx)
	defer tabCancel()
	renderCtx, cancel := context.WithTimeout(tabCtx, timeout)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-renderCtx.Done():
		}
	}()

	start := time.Now()
	var html string
	finalURL := targetURL
	statusCode := 0
	contentType := "text/html"
	var metaMu sync.Mutex

	chromedp.ListenTarget(tabCtx, func(ev any) {
		resp, ok := ev.(*network.EventResponseReceived)
		if !ok || resp.Type.String() != "Document" || resp.Response == nil {
			return
		}
		metaMu.Lock()
		statusCode = int(resp.Response.Status)
		if ct, ok := resp.Response.Headers["content-type"].(string); ok && ct != "" {
			contentType = ct
		} else if ct, ok := resp.Response.Headers["Content-Type"].(string); ok && ct != "" {
			contentType = ct
		}
		metaMu.Unlock()
	})

	blockedPatterns := urlfilter.BrowserBlockedURLPatterns(r.cfg.BlockImages)
	blocked := make([]*network.BlockPattern, 0, len(blockedPatterns))
	for _, pattern := range blockedPatterns {
		blocked = append(blocked, &network.BlockPattern{URLPattern: pattern, Block: true})
	}

	actions := []chromedp.Action{
		network.Enable(),
	}
	if r.userAgent != "" {
		actions = append(actions, emulation.SetUserAgentOverride(r.userAgent))
	}
	if len(blocked) > 0 {
		actions = append(actions, network.SetBlockedURLs().WithURLPatterns(blocked))
	}
	actions = append(actions,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	if r.cfg.WaitAfterLoad > 0 {
		actions = append(actions, chromedp.Sleep(r.cfg.WaitAfterLoad))
	}
	actions = append(actions,
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)

	if err := chromedp.Run(renderCtx, actions...); err != nil {
		return nil, fmt.Errorf("rendering %q: %w", targetURL, err)
	}

	body := []byte(html)
	truncated := false
	maxBytes := r.cfg.MaxHTMLSizeKB * 1024
	if maxBytes > 0 && len(body) > maxBytes {
		body = body[:maxBytes]
		truncated = true
	}

	metaMu.Lock()
	finalStatusCode := statusCode
	finalContentType := contentType
	if finalStatusCode == 0 {
		finalStatusCode = 200
	}
	if strings.TrimSpace(finalContentType) == "" {
		finalContentType = "text/html"
	}
	metaMu.Unlock()

	return &Result{
		StatusCode:  finalStatusCode,
		Body:        body,
		ContentType: finalContentType,
		FinalURL:    finalURL,
		Duration:    time.Since(start),
		Truncated:   truncated,
	}, nil
}

func (r *BrowserRenderer) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}
