// Package config handles loading and validating application configuration
// from YAML files and environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the complete application configuration.
type Config struct {
	Crawler       CrawlerConfig       `yaml:"crawler"`
	Storage       StorageConfig       `yaml:"storage"`
	API           APIConfig           `yaml:"api"`
	Logging       LoggingConfig       `yaml:"logging"`
	Seeds         []SeedURLConfig     `yaml:"seeds"`
	SearchIntents SearchIntentsConfig `yaml:"search_intents"`
	Embedder      EmbedderConfig      `yaml:"embedder"`
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
	// ProxyURL is an optional HTTP/HTTPS proxy URL.
	ProxyURL string `yaml:"proxy_url"`
	// DomainStatsFlushInterval controls how often per-domain crawl statistics
	// are flushed from memory to SQLite. Shorter intervals = more writes but
	// fresher stats. Longer intervals = fewer writes but possible data loss on
	// crash. Default: 30s.
	DomainStatsFlushInterval time.Duration `yaml:"domain_stats_flush_interval"`
	// AutoStart controls whether the crawler starts automatically on launch.
	// When false, you must start it via the API: POST /api/v1/crawler/start
	AutoStart bool `yaml:"auto_start"`
	// ScoringStrategy selects the frontier scoring algorithm:
	// "static"  — legacy priority-based behaviour (default)
	// "adaptive" — A*-like best-first that learns from searches and clicks
	ScoringStrategy string `yaml:"scoring_strategy"`
	// Adaptive holds the parameters for the adaptive scorer.
	Adaptive AdaptiveConfig `yaml:"adaptive"`
	// Scheduler holds the parameters for the re-crawl scheduler.
	Scheduler SchedulerConfig `yaml:"scheduler"`
}

// SeedURLConfig represents a seed URL with its own re-crawl interval.
type SeedURLConfig struct {
	// URL is the seed URL to crawl periodically.
	URL string `yaml:"url"`
	// RecrawlInterval is how often this URL should be re-crawled.
	// If zero, the scheduler's SeedRecrawlInterval is used.
	RecrawlInterval time.Duration `yaml:"recrawl_interval"`
}

// SchedulerConfig holds tunable parameters for the re-crawl scheduler.
type SchedulerConfig struct {
	// TickInterval is how often the scheduler checks for stale seeds.
	TickInterval time.Duration `yaml:"tick_interval"`
	// SeedRecrawlInterval is the default re-crawl interval for seed URLs
	// that do not specify their own.
	SeedRecrawlInterval time.Duration `yaml:"seed_recrawl_interval"`
}

// InterestConfig represents a manually declared interest term with its weight.
type InterestConfig struct {
	Term   string  `yaml:"term"`
	Weight float64 `yaml:"weight"`
}

// AdaptiveConfig holds tunable parameters for the adaptive (A*-like) scorer.
type AdaptiveConfig struct {
	// MinQueriesBeforeBoost is the number of search queries that must be
	// recorded before the adaptive heuristic is activated. Until then the
	// scorer behaves almost like the static strategy.
	MinQueriesBeforeBoost int `yaml:"min_queries_before_boost"`
	// DepthPenaltyK controls the sub-linear depth penalty:
	//   penalty = k * ln(1 + depth)
	DepthPenaltyK float64 `yaml:"depth_penalty_k"`
	// ProfileRefreshInterval is how often the in-memory interest profile is
	// reloaded from SQLite.
	ProfileRefreshInterval time.Duration `yaml:"profile_refresh_interval"`
	// SeedBonus is added to the score of seed-domain URLs (depth == 0).
	SeedBonus float64 `yaml:"seed_bonus"`
	// AnchorWeight is the multiplier for term matches in link anchor text.
	AnchorWeight float64 `yaml:"anchor_weight"`
	// URLPathWeight is the multiplier for term matches in the URL path.
	URLPathWeight float64 `yaml:"url_path_weight"`
	// SourceTitleWeight is the multiplier for term matches in the parent page title.
	SourceTitleWeight float64 `yaml:"source_title_weight"`
	// DomainReputationWeight is the multiplier for the domain's reputation score.
	DomainReputationWeight float64 `yaml:"domain_reputation_weight"`
	// LanguageMatchBonus is added when a link's language matches the profile.
	LanguageMatchBonus float64 `yaml:"language_match_bonus"`
	// Interests is a list of manually declared interest terms that boost
	// the adaptive scorer from startup, without requiring prior search queries.
	Interests []InterestConfig `yaml:"interests"`
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

// SearchIntentsConfig holds dictionaries for navigational intent detection.
type SearchIntentsConfig struct {
	SiteTypes       []string `yaml:"site_types"`
	PlatformDomains []string `yaml:"platform_domains"`
}

// EmbedderConfig controls the Ollama semantic embeddings client.
type EmbedderConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	Model   string `yaml:"model"`
	Workers int    `yaml:"workers"`
}

// Addr returns the full address string (host:port) for the API server.
func (a APIConfig) Addr() string {
	return fmt.Sprintf("%s:%d", a.Host, a.Port)
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Crawler: CrawlerConfig{
			Workers:                  100,
			MaxDepth:                 3,
			RequestTimeout:           15 * time.Second,
			PolitenessDelay:          1 * time.Second,
			RandomDelay:              0,
			MaxRetries:               3,
			UserAgent:                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36",
			MaxPageSizeKB:            512,
			RespectRobots:            false,
			ParallelismPerDomain:     4,
			DisableCookies:           false,
			MaxIdleConnsPerHost:      100,
			ProxyURL:                 "",
			DomainStatsFlushInterval: 30 * time.Second,
			AutoStart:                true,
			ScoringStrategy:          "adaptive",
			Adaptive: AdaptiveConfig{
				MinQueriesBeforeBoost:  3,
				DepthPenaltyK:          12.0,
				ProfileRefreshInterval: 60 * time.Second,
				SeedBonus:              50.0,
				AnchorWeight:           40.0,
				URLPathWeight:          30.0,
				SourceTitleWeight:      20.0,
				DomainReputationWeight: 15.0,
				LanguageMatchBonus:     35.0,
			},
			Scheduler: SchedulerConfig{
				TickInterval:        10 * time.Minute,
				SeedRecrawlInterval: 24 * time.Hour,
			},
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
		// Seeds default to empty — they must be defined in the config.
		Seeds: nil,
		SearchIntents: SearchIntentsConfig{
			SiteTypes: []string{
				"wiki", "blog", "docs", "forum", "forums", "foro", "tienda", "store",
				"info", "guia", "guias", "guide", "guides", "noticias", "news",
				"comunidad", "community", "api", "documentacion", "documentation",
				"soporte", "support", "ayuda", "help", "portal", "hub", "status",
			},
			PlatformDomains: []string{
				"reddit", "youtube", "github", "twitter", "x", "facebook", "instagram",
				"tiktok", "linkedin", "pinterest", "twitch", "discord", "netflix",
				"spotify", "steam", "epicgames", "medium", "substack", "patreon",
				"kickstarter", "indiegogo", "vimeo", "dailymotion", "tumblr", "quora",
				"stackexchange", "stackoverflow", "gitlab", "bitbucket", "sourceforge",
				"itchio", "deviantart", "artstation", "behance", "dribbble", "soundcloud",
				"bandcamp", "mixcloud", "audiomack", "goodreads", "wattpad", "fanfiction",
				"ao3", "imgur", "gfycat", "tenor", "giphy", "pastebin", "hastebin",
				"jsfiddle", "codepen", "replit", "glitch", "heroku", "vercel", "netlify",
				"wordpress", "blogger", "wix", "squarespace", "weebly", "shopify",
				"etsy", "amazon", "aliexpress", "ebay", "mercadolibre", "newegg",
				"bestbuy", "walmart", "target",
			},
		},
		Embedder: EmbedderConfig{
			Enabled: false,
			URL:     "http://localhost:11434",
			Model:   "all-minilm:l6-v2",
			Workers: 2,
		},
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

	for i, ic := range c.Crawler.Adaptive.Interests {
		if strings.TrimSpace(ic.Term) == "" {
			return fmt.Errorf("crawler.adaptive.interests[%d].term must not be empty", i)
		}
		if ic.Weight <= 0 {
			return fmt.Errorf("crawler.adaptive.interests[%d].weight must be > 0, got %f", i, ic.Weight)
		}
	}

	if c.Crawler.Scheduler.TickInterval < 1*time.Minute {
		return fmt.Errorf("crawler.scheduler.tick_interval must be >= 1m, got %s", c.Crawler.Scheduler.TickInterval)
	}
	if c.Crawler.Scheduler.SeedRecrawlInterval < 1*time.Minute {
		return fmt.Errorf("crawler.scheduler.seed_recrawl_interval must be >= 1m, got %s", c.Crawler.Scheduler.SeedRecrawlInterval)
	}

	for i, s := range c.Seeds {
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("seeds[%d].url must not be empty", i)
		}
		if s.RecrawlInterval < 0 {
			return fmt.Errorf("seeds[%d].recrawl_interval must be >= 0, got %s", i, s.RecrawlInterval)
		}
	}

	return nil
}
