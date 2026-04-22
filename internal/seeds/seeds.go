// Package seeds provides the default list of priority domains that are
// automatically seeded into the crawler's queue on first startup.
package seeds

// SeedDomain represents a domain to be pre-loaded into the crawler
// with a specific crawl priority.
type SeedDomain struct {
	Domain   string
	Priority int
	// StartURLs are the initial pages to crawl for this domain.
	// If empty, "https://<domain>/" is used by default.
	StartURLs []string
}

// DefaultSeeds returns the built-in list of seed domains.
// These domains are added with high priority so the crawler
// processes them before any discovered URLs.
func DefaultSeeds() []SeedDomain {
	return []SeedDomain{
		// ----- Social & Community -----
		{
			Domain:   "reddit.com",
			Priority: 100,
			StartURLs: []string{
				"https://www.reddit.com/",
				"https://www.reddit.com/r/popular/",
			},
		},
		{
			Domain:    "discord.com",
			Priority:  100,
			StartURLs: []string{"https://discord.com/"},
		},
		{
			Domain:    "twitter.com",
			Priority:  100,
			StartURLs: []string{"https://twitter.com/", "https://x.com/"},
		},

		// ----- Development -----
		{
			Domain:   "github.com",
			Priority: 100,
			StartURLs: []string{
				"https://github.com/",
				"https://github.com/trending",
				"https://github.com/explore",
			},
		},
		{
			Domain:   "stackoverflow.com",
			Priority: 100,
			StartURLs: []string{
				"https://stackoverflow.com/",
				"https://stackoverflow.com/questions",
			},
		},
		{
			Domain:    "dev.to",
			Priority:  90,
			StartURLs: []string{"https://dev.to/"},
		},
		{
			Domain:    "go.dev",
			Priority:  90,
			StartURLs: []string{"https://go.dev/", "https://go.dev/doc/"},
		},
		{
			Domain:    "docs.microsoft.com",
			Priority:  80,
			StartURLs: []string{"https://learn.microsoft.com/en-us/"},
		},

		// ----- Entertainment -----
		{
			Domain:    "twitch.tv",
			Priority:  100,
			StartURLs: []string{"https://www.twitch.tv/"},
		},
		{
			Domain:    "youtube.com",
			Priority:  100,
			StartURLs: []string{"https://www.youtube.com/"},
		},

		// ----- Knowledge -----
		{
			Domain:   "en.wikipedia.org",
			Priority: 100,
			StartURLs: []string{
				"https://en.wikipedia.org/wiki/Main_Page",
				"https://en.wikipedia.org/wiki/Portal:Current_events",
			},
		},
		{
			Domain:    "es.wikipedia.org",
			Priority:  100,
			StartURLs: []string{"https://es.wikipedia.org/wiki/Wikipedia:Portada"},
		},
		{
			Domain:    "medium.com",
			Priority:  80,
			StartURLs: []string{"https://medium.com/"},
		},

		// ----- News & Tech -----
		{
			Domain:    "news.ycombinator.com",
			Priority:  90,
			StartURLs: []string{"https://news.ycombinator.com/"},
		},
		{
			Domain:    "techcrunch.com",
			Priority:  80,
			StartURLs: []string{"https://techcrunch.com/"},
		},
		{
			Domain:    "arstechnica.com",
			Priority:  80,
			StartURLs: []string{"https://arstechnica.com/"},
		},

		// ----- Productivity & Reference -----
		{
			Domain:    "notion.so",
			Priority:  70,
			StartURLs: []string{"https://www.notion.so/"},
		},
		{
			Domain:    "gitlab.com",
			Priority:  70,
			StartURLs: []string{"https://gitlab.com/explore"},
		},
	}
}
