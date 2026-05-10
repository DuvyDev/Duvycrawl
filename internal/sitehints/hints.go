// Package sitehints provides platform-specific URL classification heuristics
// for search result ranking. When a user searches "warframe reddit" or
// "chromium github", the system needs to distinguish between hub pages
// (subreddit root, repo root) and deep sub-pages (individual posts, releases).
//
// Each platform registers patterns that identify hub pages, and the Classify
// function returns scoring adjustments that the search engine applies during
// re-ranking. This keeps platform-specific knowledge out of the monolithic
// search scoring code and makes it trivial to add new platforms.
//
// All functions are pure (no I/O, no database access) and safe for concurrent use.
package sitehints

import (
	"net/url"
	"strings"
)

// HubMatch is the result of classifying a URL against known platform patterns.
type HubMatch struct {
	// Platform is the canonical platform name (e.g. "reddit", "github").
	Platform string

	// IsHub is true when the URL points to a hub/landing page for a resource
	// within the platform (e.g. a subreddit root, a GitHub repo root).
	IsHub bool

	// HubName is the platform-specific identifier of the hub resource,
	// lowercased for comparison. For Reddit this is the subreddit name
	// (e.g. "warframe"), for GitHub the repo name (e.g. "chromium"), etc.
	// Empty for non-hub pages.
	HubName string

	// HubBoost is the additional score to apply when IsHub is true.
	// Hub pages should rank above individual content pages from the same platform.
	HubBoost float64

	// DepthPenalty is subtracted from the base platform boost for non-hub pages.
	// Deeper pages (releases, individual comments) get a larger penalty.
	DepthPenalty float64
}

// platformRule defines how to classify URLs for a single platform.
type platformRule struct {
	// name is the canonical platform identifier (must match the config's
	// platformDomains keys, e.g. "reddit", "github").
	name string

	// domains lists all domains (without "www.") that belong to this platform.
	// Checked with HasSuffix to support subdomains (e.g. "old.reddit.com").
	domains []string

	// classify inspects a parsed URL and returns a HubMatch.
	// It is only called when the domain has already been matched.
	classify func(u *url.URL) HubMatch
}

// registry holds all registered platform rules, keyed by domain root label.
// Populated by init() in registry.go.
var registry = map[string]*platformRule{}

// Classify checks a raw URL against all registered platform rules.
// If the URL's domain matches a known platform, it returns a HubMatch
// with the appropriate scoring adjustments. If no platform matches,
// it returns a zero-value HubMatch (IsHub=false, boosts=0).
//
// The platform parameter is the detected platformIntent from the query
// parser (e.g. "reddit"). If empty, all registered platforms are checked.
// If non-empty, only the matching platform is evaluated.
func Classify(rawURL string, platform string) HubMatch {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return HubMatch{}
	}

	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimPrefix(host, "www.")

	if platform != "" {
		// Fast path: only check the specific platform.
		if rule, ok := registry[platform]; ok {
			if matchesDomain(host, rule.domains) {
				return rule.classify(parsed)
			}
		}
		return HubMatch{}
	}

	// Slow path: check all platforms.
	for _, rule := range registry {
		if matchesDomain(host, rule.domains) {
			return rule.classify(parsed)
		}
	}

	return HubMatch{}
}

// matchesDomain checks if a hostname belongs to any of the platform's domains.
func matchesDomain(host string, domains []string) bool {
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// cleanPath returns the URL path trimmed of leading/trailing slashes
// and split into non-empty segments.
func cleanPath(u *url.URL) []string {
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	var clean []string
	for _, p := range parts {
		if p != "" {
			clean = append(clean, p)
		}
	}
	return clean
}
