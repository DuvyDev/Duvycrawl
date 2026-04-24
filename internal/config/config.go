// Package config handles loading and validating application configuration
// from YAML files and environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the complete application configuration.
type Config struct {
	Crawler CrawlerConfig `yaml:"crawler"`
	Storage StorageConfig `yaml:"storage"`
	API     APIConfig     `yaml:"api"`
	Logging LoggingConfig `yaml:"logging"`
	Seeds   []SeedConfig  `yaml:"seeds"`
}

// CrawlerConfig controls the crawling behavior.
type CrawlerConfig struct {
	// Workers is the number of concurrent crawl goroutines.
	Workers int `yaml:"workers"`
	// MaxDepth is the maximum link-follow depth from seed pages.
	MaxDepth int `yaml:"max_depth"`
	// RequestTimeout is the maximum duration for a single HTTP request.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// PolitenessDelay is the minimum wait between requests to the same domain.
	PolitenessDelay time.Duration `yaml:"politeness_delay"`
	// RandomDelay is the extra randomized duration added to PolitenessDelay.
	// This helps avoid predictable crawl patterns. Set to 0 to disable.
	RandomDelay time.Duration `yaml:"random_delay"`
	// MaxRetries is the maximum number of retry attempts for a failed request.
	MaxRetries int `yaml:"max_retries"`
	// UserAgent is the User-Agent header sent with every request.
	UserAgent string `yaml:"user_agent"`
	// MaxPageSizeKB is the maximum page size to download, in kilobytes.
	MaxPageSizeKB int `yaml:"max_page_size_kb"`
	// RespectRobots controls whether robots.txt directives are honored.
	RespectRobots bool `yaml:"respect_robots"`
	// SeedDomainsOnly when true restricts crawling to seed domains only.
	// Discovered links pointing to non-seed domains are discarded.
	// When false, the crawler follows links freely across the web.
	SeedDomainsOnly bool `yaml:"seed_domains_only"`
	// ParallelismPerDomain is the maximum number of concurrent requests
	// to the same domain. Higher values increase throughput for a single
	// domain but be careful not to overwhelm servers. Default: 2.
	ParallelismPerDomain int `yaml:"parallelism_per_domain"`
	// DisableCookies turns off cookie handling. Cookies are enabled by
	// default and are essential for crawling sites with session-based
	// protections (CSRF, login walls, etc.).
	DisableCookies bool `yaml:"disable_cookies"`
	// MaxIdleConnsPerHost controls the maximum number of idle (keep-alive)
	// connections to keep per host. Higher values improve throughput when
	// crawling the same domains repeatedly.
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`
	// ProxyURL is an optional HTTP/SOCKS5 proxy URL for all requests.
	// Example: "http://proxy.example.com:8080"
	ProxyURL string `yaml:"proxy_url"`
	// FallbackUserAgent is used when the primary User-Agent results in an
	// empty page or a bot-block response. A Googlebot-style UA is a good
	// default because some sites serve a simplified static version to bots.
	FallbackUserAgent string `yaml:"fallback_user_agent"`
	// MaxFallbackRetries is the maximum number of fallback attempts per URL
	// when the primary User-Agent is detected to have failed.
	MaxFallbackRetries int `yaml:"max_fallback_retries"`
	// DomainStatsFlushInterval controls how often per-domain crawl statistics
	// are flushed from memory to SQLite. Shorter intervals = more writes but
	// fresher stats. Longer intervals = fewer writes but possible data loss on
	// crash. Default: 30s.
	DomainStatsFlushInterval time.Duration `yaml:"domain_stats_flush_interval"`
}

// SeedConfig represents a seed domain defined in the configuration file.
type SeedConfig struct {
	// Domain is the domain name (e.g. "github.com").
	Domain string `yaml:"domain"`
	// Priority controls crawl order. Higher = crawled sooner. Default: 100.
	Priority int `yaml:"priority"`
	// StartURLs are the initial pages to crawl. If empty, "https://<domain>/" is used.
	StartURLs []string `yaml:"start_urls"`
}

// StorageConfig controls data persistence.
type StorageConfig struct {
	// DBPath is the filesystem path for the SQLite database file.
	DBPath string `yaml:"db_path"`
}

// APIConfig controls the REST API server.
type APIConfig struct {
	// Host is the address to bind the HTTP server to.
	Host string `yaml:"host"`
	// Port is the TCP port for the HTTP server.
	Port int `yaml:"port"`
}

// LoggingConfig controls log output.
type LoggingConfig struct {
	// Level is the minimum log level: debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is the log output format: "text" or "json".
	Format string `yaml:"format"`
}

// Addr returns the full address string (host:port) for the API server.
func (a APIConfig) Addr() string {
	return fmt.Sprintf("%s:%d", a.Host, a.Port)
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Crawler: CrawlerConfig{
			Workers:              10,
			MaxDepth:             3,
			RequestTimeout:       15 * time.Second,
			PolitenessDelay:      1 * time.Second,
			RandomDelay:          0,
			MaxRetries:           3,
			UserAgent:            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36",
			MaxPageSizeKB:        5120,
			RespectRobots:        true,
			SeedDomainsOnly:      false,
			ParallelismPerDomain: 2,
			DisableCookies:       false,
			MaxIdleConnsPerHost:  100,
			ProxyURL:                   "",
			FallbackUserAgent:          "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			MaxFallbackRetries:         1,
			DomainStatsFlushInterval:   30 * time.Second,
		},
		Storage: StorageConfig{
			DBPath: "./data/duvycrawl.db",
		},
		API: APIConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		// Seeds default to empty — the hardcoded defaults from internal/seeds
		// are used as fallback when no seeds are defined in the config.
		Seeds: nil,
	}
}

// Load reads a YAML configuration file and returns a Config.
// Missing fields are filled with defaults. After loading the main file,
// any YAML files in a "seeds" subdirectory next to the config are also
// loaded and their seeds are merged into the final config.
// The resulting config is validated before being returned.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	// Merge seeds from any YAML files in the <config-dir>/seeds/ folder.
	seedsDir := filepath.Join(filepath.Dir(path), "seeds")
	entries, err := os.ReadDir(seedsDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := strings.ToLower(entry.Name())
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			seedPath := filepath.Join(seedsDir, entry.Name())
			seedData, err := os.ReadFile(seedPath)
			if err != nil {
				continue // skip unreadable seed files
			}
			var partial struct {
				Seeds []SeedConfig `yaml:"seeds"`
			}
			if err := yaml.Unmarshal(seedData, &partial); err != nil {
				continue // skip malformed seed files
			}
			cfg.Seeds = append(cfg.Seeds, partial.Seeds...)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// validate checks that all configuration values are within acceptable bounds.
func (c *Config) validate() error {
	if c.Crawler.Workers < 1 {
		return fmt.Errorf("crawler.workers must be >= 1, got %d", c.Crawler.Workers)
	}
	if c.Crawler.Workers > 10000 {
		return fmt.Errorf("crawler.workers must be <= 10000, got %d", c.Crawler.Workers)
	}
	if c.Crawler.MaxDepth < 0 {
		return fmt.Errorf("crawler.max_depth must be >= 0, got %d", c.Crawler.MaxDepth)
	}
	if c.Crawler.RequestTimeout < 1*time.Second {
		return fmt.Errorf("crawler.request_timeout must be >= 1s, got %s", c.Crawler.RequestTimeout)
	}
	if c.Crawler.PolitenessDelay < 100*time.Millisecond {
		return fmt.Errorf("crawler.politeness_delay must be >= 100ms, got %s", c.Crawler.PolitenessDelay)
	}
	if c.Crawler.MaxRetries < 0 {
		return fmt.Errorf("crawler.max_retries must be >= 0, got %d", c.Crawler.MaxRetries)
	}
	if c.Crawler.UserAgent == "" {
		return fmt.Errorf("crawler.user_agent must not be empty")
	}
	if c.Crawler.MaxPageSizeKB < 1 {
		return fmt.Errorf("crawler.max_page_size_kb must be >= 1, got %d", c.Crawler.MaxPageSizeKB)
	}
	if c.Crawler.ParallelismPerDomain < 1 {
		return fmt.Errorf("crawler.parallelism_per_domain must be >= 1, got %d", c.Crawler.ParallelismPerDomain)
	}
	if c.Crawler.MaxIdleConnsPerHost < 1 {
		return fmt.Errorf("crawler.max_idle_conns_per_host must be >= 1, got %d", c.Crawler.MaxIdleConnsPerHost)
	}
	if c.Storage.DBPath == "" {
		return fmt.Errorf("storage.db_path must not be empty")
	}
	if c.API.Port < 1 || c.API.Port > 65535 {
		return fmt.Errorf("api.port must be between 1 and 65535, got %d", c.API.Port)
	}

	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level must be one of [debug, info, warn, error], got %q", c.Logging.Level)
	}

	validFormats := map[string]bool{"text": true, "json": true}
	if !validFormats[c.Logging.Format] {
		return fmt.Errorf("logging.format must be one of [text, json], got %q", c.Logging.Format)
	}

	return nil
}
