package crawler

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"log/slog"
)

// DNSCache caches DNS lookups with a TTL to reduce repeated resolver queries
// when crawling the same domains heavily. It is safe for concurrent use.
type DNSCache struct {
	ttl       time.Duration
	dialer    *net.Dialer
	logger    *slog.Logger
	stop      chan struct{}
	closeOnce sync.Once

	mu      sync.RWMutex
	entries map[string]*dnsEntry
}

type dnsEntry struct {
	ips      []string
	expires  time.Time
	lookupMu sync.Mutex // serializes concurrent lookups for the same host
}

// NewDNSCache creates a new DNS cache with the given TTL and underlying dialer.
func NewDNSCache(ttl time.Duration, dialer *net.Dialer, logger *slog.Logger) *DNSCache {
	c := &DNSCache{
		ttl:     ttl,
		dialer:  dialer,
		logger:  logger.With("component", "dnscache"),
		entries: make(map[string]*dnsEntry),
		stop:    make(chan struct{}),
	}
	go c.janitor()
	return c
}

// Lookup resolves a host using the cache when possible.
func (c *DNSCache) Lookup(ctx context.Context, host string) ([]string, error) {
	c.mu.RLock()
	e, ok := c.entries[host]
	c.mu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		ips := make([]string, len(e.ips))
		copy(ips, e.ips)
		return ips, nil
	}

	// Miss or expired — need to fetch. Ensure entry exists and use its
	// per-host mutex to collapse concurrent lookups.
	if !ok {
		c.mu.Lock()
		e, ok = c.entries[host]
		if !ok {
			e = &dnsEntry{}
			c.entries[host] = e
		}
		c.mu.Unlock()
	}

	e.lookupMu.Lock()
	defer e.lookupMu.Unlock()

	// Double-check after acquiring lock.
	if time.Now().Before(e.expires) {
		ips := make([]string, len(e.ips))
		copy(ips, e.ips)
		return ips, nil
	}

	resolver := &net.Resolver{}
	ips, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	e.ips = ips
	e.expires = time.Now().Add(c.ttl)
	c.mu.Unlock()

	c.logger.Debug("dns resolved", "host", host, "ips", len(ips))
	return ips, nil
}

// DialContext can be used as http.Transport.DialContext. It resolves the host
// via the cache and then dials the IP directly, preserving the original host
// for TLS SNI and HTTP Host header.
func (c *DNSCache) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return c.dialer.DialContext(ctx, network, addr)
	}

	ips, err := c.Lookup(ctx, host)
	if err != nil {
		return nil, err
	}

	for _, ip := range ips {
		conn, err := c.dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no reachable IPs for %s", host)
}

// janitor runs a background goroutine that prunes expired entries.
func (c *DNSCache) janitor() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.prune()
		case <-c.stop:
			return
		}
	}
}

func (c *DNSCache) prune() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for host, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, host)
		}
	}
}

// Close stops the background janitor. Safe to call multiple times.
func (c *DNSCache) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
}
