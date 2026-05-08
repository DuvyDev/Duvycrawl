package crawler

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyStatus represents the health of a proxy.
type ProxyStatus int32

const (
	StatusHealthy ProxyStatus = iota
	StatusDown
)

// ProxyClient holds a proxy URL, its associated HTTP client, and its current health status.
type ProxyClient struct {
	URL      string
	IsDirect bool
	Client   *http.Client
	Status   atomic.Int32
	dnsCache *DNSCache
}

// SetStatus updates the atomic status.
func (p *ProxyClient) SetStatus(status ProxyStatus) {
	p.Status.Store(int32(status))
}

// GetStatus reads the atomic status.
func (p *ProxyClient) GetStatus() ProxyStatus {
	return ProxyStatus(p.Status.Load())
}

// ProxyManager maintains a list of ProxyClients and a background monitor to check their health.
type ProxyManager struct {
	proxies       []*ProxyClient
	checkInterval time.Duration
	logger        *slog.Logger
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// NewProxyManager creates a ProxyManager. If proxyURLs is empty, it creates a single direct connection.
func NewProxyManager(
	proxyURLs []string,
	checkInterval time.Duration,
	timeout time.Duration,
	maxIdleConnsPerHost int,
	disableCookies bool,
	logger *slog.Logger,
) *ProxyManager {
	if len(proxyURLs) == 0 {
		proxyURLs = []string{"direct"}
	}

	pm := &ProxyManager{
		checkInterval: checkInterval,
		logger:        logger.With("component", "proxy_manager"),
	}

	baseDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	for _, pURL := range proxyURLs {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
				Renegotiation:      tls.RenegotiateOnceAsClient,
				MinVersion:         tls.VersionTLS12,
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

		client := &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				if len(via) > 0 {
					lastReq := via[len(via)-1]
					for k, v := range lastReq.Header {
						if k == "Cookie" {
							continue
						}
						req.Header[k] = v
					}
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
		}

		pClient := &ProxyClient{
			URL:    pURL,
			Client: client,
		}
		pClient.SetStatus(StatusHealthy) // Assume healthy until proven otherwise

		if pURL == "direct" || pURL == "" {
			pClient.IsDirect = true
			pClient.URL = "direct"
			pClient.dnsCache = NewDNSCache(5*time.Minute, baseDialer, logger)
			transport.DialContext = pClient.dnsCache.DialContext
			pm.logger.Info("configured direct connection fallback")
		} else {
			parsedProxy, err := url.Parse(pURL)
			if err != nil {
				pm.logger.Warn("invalid proxy URL, ignoring", "url", pURL, "error", err)
				continue
			}

			if parsedProxy.Scheme == "socks5" || parsedProxy.Scheme == "socks5h" {
				dialer, err := proxy.FromURL(parsedProxy, proxy.Direct)
				if err == nil {
					transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
						return dialer.Dial(network, addr)
					}
					pm.logger.Info("configured socks5 proxy", "url", pURL)
				} else {
					pm.logger.Warn("failed to create socks5 dialer, ignoring proxy", "url", pURL, "error", err)
					continue
				}
			} else {
				transport.Proxy = http.ProxyURL(parsedProxy)
				pm.logger.Info("configured http proxy", "url", pURL)
			}
		}

		pm.proxies = append(pm.proxies, pClient)
	}

	if len(pm.proxies) == 0 {
		// Fallback to a single direct connection if all proxies were invalid
		pm.logger.Warn("no valid proxies configured, falling back to direct connection")
		// Instead of copy-pasting, we recurse once.
		return NewProxyManager([]string{"direct"}, checkInterval, timeout, maxIdleConnsPerHost, disableCookies, logger)
	}

	return pm
}

// StartMonitor begins the background health checks.
func (pm *ProxyManager) StartMonitor(ctx context.Context) {
	if len(pm.proxies) <= 1 {
		// No need to monitor if there's only 1 proxy/direct connection,
		// since we can't failover to anything else anyway.
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	pm.cancel = cancel
	pm.wg.Add(1)

	go func() {
		defer pm.wg.Done()
		ticker := time.NewTicker(pm.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pm.checkAllProxies(ctx)
			}
		}
	}()
}

// StopMonitor shuts down the background health checks and closes DNS caches.
func (pm *ProxyManager) StopMonitor() {
	if pm.cancel != nil {
		pm.cancel()
	}
	pm.wg.Wait()

	for _, p := range pm.proxies {
		if p.dnsCache != nil {
			p.dnsCache.Close()
		}
	}
}

// checkAllProxies iterates through all configured proxies and pings them.
func (pm *ProxyManager) checkAllProxies(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range pm.proxies {
		wg.Add(1)
		go func(pClient *ProxyClient) {
			defer wg.Done()
			pm.checkProxy(ctx, pClient)
		}(p)
	}
	wg.Wait()
}

func (pm *ProxyManager) checkProxy(ctx context.Context, p *ProxyClient) {
	// A fast, reliable endpoint that returns 204 No Content.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://clients3.google.com/generate_204", nil)
	if err != nil {
		return
	}

	// We use a short timeout for the health check.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := p.Client.Do(req)
	oldStatus := p.GetStatus()

	if err != nil {
		if oldStatus == StatusHealthy {
			p.SetStatus(StatusDown)
			pm.logger.Warn("proxy went down", "url", p.URL, "error", err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 || resp.StatusCode == 200 {
		if oldStatus == StatusDown {
			p.SetStatus(StatusHealthy)
			pm.logger.Info("proxy recovered", "url", p.URL)
		}
	} else {
		// Unexpected status but connection worked. We'll mark it healthy since the proxy is routing traffic.
		if oldStatus == StatusDown {
			p.SetStatus(StatusHealthy)
			pm.logger.Info("proxy recovered (with non-204 status)", "url", p.URL, "status", resp.StatusCode)
		}
	}
}

// GetClient returns the highest priority ProxyClient that is currently Healthy.
// If all proxies are down, it returns the highest priority proxy anyway (as a last resort).
func (pm *ProxyManager) GetClient() *ProxyClient {
	for _, p := range pm.proxies {
		if p.GetStatus() == StatusHealthy {
			return p
		}
	}
	// Fallback to the first configured proxy if all are marked down
	return pm.proxies[0]
}

// MarkDown explicitly marks a proxy as down (e.g. if the fetcher detects a dial error).
func (pm *ProxyManager) MarkDown(p *ProxyClient) {
	if p.GetStatus() == StatusHealthy {
		p.SetStatus(StatusDown)
		pm.logger.Warn("proxy marked down by fetcher", "url", p.URL)
	}
}

// PrimaryProxyURL returns the URL of the highest priority proxy for use with chromedp.
func (pm *ProxyManager) PrimaryProxyURL() string {
	if len(pm.proxies) > 0 && !pm.proxies[0].IsDirect {
		return pm.proxies[0].URL
	}
	return ""
}
