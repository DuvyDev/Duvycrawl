package render

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

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
func NewBrowserRenderer(cfg config.RenderingConfig, userAgent string, logger *slog.Logger) (*BrowserRenderer, error) {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}

	baseCtx := context.Background()
	var allocCtx context.Context
	var cancel context.CancelFunc

	if cfg.BrowserWSURL != "" {
		allocCtx, cancel = chromedp.NewRemoteAllocator(baseCtx, cfg.BrowserWSURL)
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
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	timeout := r.cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
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

	blocked := []*network.BlockPattern{}
	if r.cfg.BlockImages {
		blocked = []*network.BlockPattern{
			{URLPattern: "*://*:*/*.png", Block: true},
			{URLPattern: "*://*:*/*.jpg", Block: true},
			{URLPattern: "*://*:*/*.jpeg", Block: true},
			{URLPattern: "*://*:*/*.gif", Block: true},
			{URLPattern: "*://*:*/*.webp", Block: true},
			{URLPattern: "*://*:*/*.avif", Block: true},
			{URLPattern: "*://*:*/*.svg", Block: true},
			{URLPattern: "*://*:*/*.ico", Block: true},
		}
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
