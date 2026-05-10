package cache

import (
	"log/slog"
	"time"

	"github.com/dgraph-io/ristretto"
)

// Cache is a high-performance in-memory cache wrapper around Ristretto.
type Cache struct {
	rCache *ristretto.Cache
	logger *slog.Logger
}

// Config defines the caching limits.
type Config struct {
	NumCounters int64 // Usually 10x the max number of items you expect to keep.
	MaxCost     int64 // Maximum cost of cache (e.g. in bytes).
	BufferItems int64 // Number of keys per Get buffer (default 64).
}

// NewCache creates a new Ristretto cache instance.
func NewCache(cfg Config, logger *slog.Logger) (*Cache, error) {
	if cfg.BufferItems == 0 {
		cfg.BufferItems = 64
	}

	rc, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: cfg.NumCounters,
		MaxCost:     cfg.MaxCost,
		BufferItems: cfg.BufferItems,
		Metrics:     false,
	})
	if err != nil {
		return nil, err
	}

	return &Cache{
		rCache: rc,
		logger: logger.With("component", "ristretto_cache"),
	}, nil
}

// Get retrieves an item from the cache.
func (c *Cache) Get(key string) (interface{}, bool) {
	return c.rCache.Get(key)
}

// Set adds an item to the cache with a cost of 1.
func (c *Cache) Set(key string, value interface{}, cost int64) bool {
	return c.rCache.Set(key, value, cost)
}

// SetWithTTL adds an item to the cache with a specified time-to-live.
func (c *Cache) SetWithTTL(key string, value interface{}, cost int64, ttl time.Duration) bool {
	return c.rCache.SetWithTTL(key, value, cost, ttl)
}

// Del removes an item from the cache.
func (c *Cache) Del(key string) {
	c.rCache.Del(key)
}

// Clear removes all items from the cache.
func (c *Cache) Clear() {
	c.rCache.Clear()
}

// Close closes the cache.
func (c *Cache) Close() {
	c.rCache.Close()
}
