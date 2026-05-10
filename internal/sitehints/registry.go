package sitehints

import (
	"net/url"
	"strings"
)

// Boost constants. These are additive adjustments applied on top of the
// base platform boost (~16000) that already exists in the scoring pipeline.
const (
	// hubBoostLarge is for platforms where the hub page is the clear
	// primary destination (subreddit root, repo root, channel page).
	hubBoostLarge = 6000.0

	// hubBoostMedium is for platforms where the hub is important but
	// content pages are also commonly the target (Wikipedia articles,
	// SO questions).
	hubBoostMedium = 4000.0

	// depthPenaltyLight penalizes pages one level below the hub
	// (e.g. a GitHub repo's "issues" tab).
	depthPenaltyLight = 1000.0

	// depthPenaltyMedium penalizes pages two or more levels deep
	// (e.g. a specific GitHub issue, a Reddit comment thread).
	depthPenaltyMedium = 2500.0

	// depthPenaltyHeavy penalizes very deep/ephemeral pages
	// (e.g. release tags, commit SHAs, individual clips).
	depthPenaltyHeavy = 4000.0
)

func init() {
	for _, rule := range platformRules {
		registry[rule.name] = rule
	}
}

// platformRules defines the classification logic for each supported platform.
var platformRules = []*platformRule{

	// -------------------------------------------------------------------------
	// Reddit
	// Hub: /r/{subreddit}  (the subreddit landing page)
	// Non-hub: /r/{subreddit}/comments/...  /r/{subreddit}/wiki/...
	// -------------------------------------------------------------------------
	{
		name:    "reddit",
		domains: []string{"reddit.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "reddit"}

			// /r/{subreddit} with exactly 2 segments → hub
			if len(parts) == 2 && strings.EqualFold(parts[0], "r") {
				m.IsHub = true
				m.HubName = strings.ToLower(parts[1])
				m.HubBoost = hubBoostLarge
				return m
			}

			// /r/{subreddit}/... → non-hub, penalize by depth
			if len(parts) > 2 && strings.EqualFold(parts[0], "r") {
				if strings.EqualFold(parts[2], "comments") {
					m.DepthPenalty = depthPenaltyMedium
				} else {
					m.DepthPenalty = depthPenaltyLight
				}
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// GitHub
	// Hub: /{owner}/{repo}  (exactly 2 path segments)
	// Non-hub: /{owner}/{repo}/issues/...  /releases/...  /tree/...
	// -------------------------------------------------------------------------
	{
		name:    "github",
		domains: []string{"github.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "github"}

			if len(parts) == 0 {
				return m
			}

			// Skip reserved top-level paths (not repos).
			reserved := map[string]bool{
				"features": true, "marketplace": true, "explore": true,
				"topics": true, "trending": true, "collections": true,
				"sponsors": true, "settings": true, "notifications": true,
				"login": true, "signup": true, "join": true, "about": true,
				"pricing": true, "enterprise": true, "team": true,
				"orgs": true, "organizations": true,
			}
			if reserved[strings.ToLower(parts[0])] {
				return m
			}

			// /{owner}/{repo} exactly → hub (repo root)
			if len(parts) == 2 {
				m.IsHub = true
				m.HubName = strings.ToLower(parts[1])
				m.HubBoost = hubBoostLarge
				return m
			}

			// /{owner} exactly → user/org profile, slight hub
			if len(parts) == 1 {
				m.IsHub = true
				m.HubName = strings.ToLower(parts[0])
				m.HubBoost = hubBoostMedium
				return m
			}

			// /{owner}/{repo}/{section}/... → non-hub
			if len(parts) >= 3 {
				section := strings.ToLower(parts[2])
				switch section {
				case "releases", "tags":
					m.DepthPenalty = depthPenaltyHeavy
				case "issues", "pull", "discussions":
					if len(parts) > 3 {
						m.DepthPenalty = depthPenaltyMedium // specific issue
					} else {
						m.DepthPenalty = depthPenaltyLight // issues list
					}
				case "tree", "blob", "commit", "commits":
					m.DepthPenalty = depthPenaltyMedium
				case "wiki":
					m.DepthPenalty = depthPenaltyLight
				default:
					m.DepthPenalty = depthPenaltyLight
				}
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// YouTube
	// Hub: /@{channel}  /c/{channel}  /channel/{id}  /user/{name}
	// Non-hub: /watch?v=...  /shorts/...  /playlist?...
	// -------------------------------------------------------------------------
	{
		name:    "youtube",
		domains: []string{"youtube.com", "youtu.be"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "youtube"}

			if len(parts) == 0 {
				return m
			}

			first := parts[0]

			// /@channel or /c/channel or /channel/id or /user/name → hub
			if strings.HasPrefix(first, "@") && len(parts) <= 2 {
				m.IsHub = true
				m.HubName = strings.ToLower(strings.TrimPrefix(first, "@"))
				m.HubBoost = hubBoostLarge
				return m
			}
			if (strings.EqualFold(first, "c") || strings.EqualFold(first, "channel") || strings.EqualFold(first, "user")) && len(parts) >= 2 {
				if len(parts) <= 3 { // /c/name or /c/name/videos
					m.IsHub = true
					m.HubName = strings.ToLower(parts[1])
					m.HubBoost = hubBoostLarge
					return m
				}
			}

			// /watch → individual video
			if strings.EqualFold(first, "watch") {
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			// /shorts/ → short video
			if strings.EqualFold(first, "shorts") {
				m.DepthPenalty = depthPenaltyHeavy
				return m
			}

			// /playlist → playlist page
			if strings.EqualFold(first, "playlist") {
				m.DepthPenalty = depthPenaltyLight
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Wikipedia
	// Hub: /wiki/{Article} (single article, the main content)
	// Note: Wikipedia articles ARE the hub — they are usually what users want.
	// Sub-pages like /wiki/Article/History are less relevant.
	// -------------------------------------------------------------------------
	{
		name:    "wikipedia",
		domains: []string{"wikipedia.org"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "wikipedia"}

			// /wiki/{Article} → hub
			if len(parts) == 2 && strings.EqualFold(parts[0], "wiki") {
				// Skip meta pages
				article := parts[1]
				if strings.HasPrefix(article, "Special:") ||
					strings.HasPrefix(article, "Talk:") ||
					strings.HasPrefix(article, "User:") ||
					strings.HasPrefix(article, "Wikipedia:") ||
					strings.HasPrefix(article, "Help:") ||
					strings.HasPrefix(article, "Category:") ||
					strings.HasPrefix(article, "File:") ||
					strings.HasPrefix(article, "Template:") {
					m.DepthPenalty = depthPenaltyHeavy
					return m
				}
				m.IsHub = true
				m.HubBoost = hubBoostMedium
				return m
			}

			// /wiki/{Article}/{SubPage} → non-hub
			if len(parts) > 2 && strings.EqualFold(parts[0], "wiki") {
				m.DepthPenalty = depthPenaltyLight
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Stack Overflow / Stack Exchange
	// Hub: /questions/{id}/{slug} (a specific Q&A thread)
	// Non-hub: /questions/tagged/...  /users/...
	// -------------------------------------------------------------------------
	{
		name:    "stackoverflow",
		domains: []string{"stackoverflow.com", "stackexchange.com", "superuser.com", "serverfault.com", "askubuntu.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "stackoverflow"}

			if len(parts) >= 2 && strings.EqualFold(parts[0], "questions") {
				// /questions/tagged/... → tag listing, not a specific question
				if strings.EqualFold(parts[1], "tagged") {
					m.DepthPenalty = depthPenaltyLight
					return m
				}
				// /questions/{id}/... → the actual Q&A (this IS the hub for SO)
				m.IsHub = true
				m.HubBoost = hubBoostMedium
				return m
			}

			if len(parts) >= 1 && strings.EqualFold(parts[0], "users") {
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// GitLab
	// Hub: /{owner}/{repo} (exactly 2 segments, no leading -)
	// Non-hub: /{owner}/{repo}/-/issues/...  /-/merge_requests/...
	// -------------------------------------------------------------------------
	{
		name:    "gitlab",
		domains: []string{"gitlab.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "gitlab"}

			if len(parts) == 0 {
				return m
			}

			reserved := map[string]bool{
				"explore": true, "help": true, "admin": true,
				"users": true, "groups": true, "dashboard": true,
			}
			if reserved[strings.ToLower(parts[0])] {
				return m
			}

			// /{owner}/{repo} → hub
			if len(parts) == 2 {
				m.IsHub = true
				m.HubName = strings.ToLower(parts[1])
				m.HubBoost = hubBoostLarge
				return m
			}

			// /{owner}/{repo}/-/{section}/... → non-hub
			if len(parts) >= 4 && parts[2] == "-" {
				section := strings.ToLower(parts[3])
				switch section {
				case "releases", "tags":
					m.DepthPenalty = depthPenaltyHeavy
				case "issues", "merge_requests":
					if len(parts) > 4 {
						m.DepthPenalty = depthPenaltyMedium
					} else {
						m.DepthPenalty = depthPenaltyLight
					}
				default:
					m.DepthPenalty = depthPenaltyLight
				}
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// npm
	// Hub: /package/{name} or /package/@{scope}/{name}
	// Non-hub: /package/{name}/v/{version}
	// -------------------------------------------------------------------------
	{
		name:    "npm",
		domains: []string{"npmjs.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "npm"}

			if len(parts) >= 2 && strings.EqualFold(parts[0], "package") {
				// /package/{name} or /package/@scope/name → hub
				isScoped := strings.HasPrefix(parts[1], "@")
				hubLen := 2
				if isScoped {
					hubLen = 3
				}
				if len(parts) == hubLen {
					m.IsHub = true
					if isScoped {
						m.HubName = strings.ToLower(parts[1] + "/" + parts[2])
					} else {
						m.HubName = strings.ToLower(parts[1])
					}
					m.HubBoost = hubBoostLarge
					return m
				}
				// Deeper → version page, etc.
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// PyPI
	// Hub: /project/{name}
	// Non-hub: /project/{name}/{version}
	// -------------------------------------------------------------------------
	{
		name:    "pypi",
		domains: []string{"pypi.org"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "pypi"}

			if len(parts) >= 2 && strings.EqualFold(parts[0], "project") {
				if len(parts) == 2 {
					m.IsHub = true
					m.HubName = strings.ToLower(parts[1])
					m.HubBoost = hubBoostLarge
					return m
				}
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Docker Hub
	// Hub: /r/{publisher}/{image} or /_/{image}
	// Non-hub: .../tags  .../builds
	// -------------------------------------------------------------------------
	{
		name:    "docker",
		domains: []string{"hub.docker.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "docker"}

			// /r/{publisher}/{image} → hub
			if len(parts) == 3 && (strings.EqualFold(parts[0], "r") || strings.EqualFold(parts[0], "_")) {
				m.IsHub = true
				m.HubBoost = hubBoostMedium
				return m
			}
			// /_/{image} (official images)
			if len(parts) == 2 && strings.EqualFold(parts[0], "_") {
				m.IsHub = true
				m.HubBoost = hubBoostMedium
				return m
			}

			if len(parts) > 3 {
				m.DepthPenalty = depthPenaltyLight
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Twitch
	// Hub: /{channel} (single segment, not a reserved path)
	// Non-hub: /{channel}/clips  /{channel}/videos
	// -------------------------------------------------------------------------
	{
		name:    "twitch",
		domains: []string{"twitch.tv"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "twitch"}

			if len(parts) == 0 {
				return m
			}

			reserved := map[string]bool{
				"directory": true, "downloads": true, "jobs": true,
				"turbo": true, "settings": true, "friends": true,
				"subscriptions": true, "inventory": true, "wallet": true,
				"search": true, "team": true, "p": true, "videos": true,
			}

			first := strings.ToLower(parts[0])
			if reserved[first] {
				return m
			}

			// /{channel} → hub (streamer page)
			if len(parts) == 1 {
				m.IsHub = true
				m.HubName = strings.ToLower(parts[0])
				m.HubBoost = hubBoostLarge
				return m
			}

			// /{channel}/clip/...  /{channel}/videos → non-hub
			if len(parts) >= 2 {
				section := strings.ToLower(parts[1])
				switch section {
				case "clip", "clips":
					m.DepthPenalty = depthPenaltyMedium
				case "videos", "schedule", "about":
					m.DepthPenalty = depthPenaltyLight
				default:
					m.DepthPenalty = depthPenaltyLight
				}
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Steam
	// Hub: /app/{id}/{name} (a game's store page)
	// Non-hub: /app/{id}/{name}/reviews  /app/{id}/{name}/discussions
	// -------------------------------------------------------------------------
	{
		name:    "steam",
		domains: []string{"store.steampowered.com", "steampowered.com", "steamcommunity.com"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "steam"}

			// /app/{id} or /app/{id}/{name} → hub (game store page)
			if len(parts) >= 2 && strings.EqualFold(parts[0], "app") {
				if len(parts) <= 3 {
					m.IsHub = true
					m.HubBoost = hubBoostMedium
					return m
				}
				m.DepthPenalty = depthPenaltyLight
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// MercadoLibre
	// No hub concept — products are individual. But listing pages (/MLU-...)
	// are more relevant than review/question sub-pages.
	// -------------------------------------------------------------------------
	{
		name:    "mercadolibre",
		domains: []string{"mercadolibre.com.uy", "mercadolibre.com.ar", "mercadolibre.com.mx", "mercadolibre.com.br", "mercadolibre.com.co", "mercadolibre.com.ve", "mercadolibre.cl", "mercadolibre.com.pe", "mercadolibre.com.ec"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "mercadolibre"}

			// Product listings are typically at the root or 1-2 segments deep.
			// No specific hub concept, but avoid penalizing product pages.
			if len(parts) > 3 {
				m.DepthPenalty = depthPenaltyLight
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// NuGet (.NET package registry)
	// Hub: /packages/{name}
	// Non-hub: /packages/{name}/{version}
	// -------------------------------------------------------------------------
	{
		name:    "nuget",
		domains: []string{"nuget.org"},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "nuget"}

			if len(parts) >= 2 && strings.EqualFold(parts[0], "packages") {
				if len(parts) == 2 {
					m.IsHub = true
					m.HubName = strings.ToLower(parts[1])
					m.HubBoost = hubBoostLarge
					return m
				}
				// /packages/{name}/{version} → version page
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			return m
		},
	},

	// -------------------------------------------------------------------------
	// Amazon
	// Hub: /dp/{ASIN}  /gp/product/{ASIN}  (product detail page)
	// Non-hub: /dp/{ASIN}/ref=...  /product-reviews/...  /offer-listing/...
	// -------------------------------------------------------------------------
	{
		name:    "amazon",
		domains: []string{
			"amazon.com", "amazon.co.uk", "amazon.de", "amazon.fr",
			"amazon.es", "amazon.it", "amazon.co.jp", "amazon.ca",
			"amazon.com.au", "amazon.com.br", "amazon.com.mx",
			"amazon.in", "amazon.nl", "amazon.sg", "amazon.ae",
			"amazon.sa", "amazon.pl", "amazon.se", "amazon.com.tr",
		},
		classify: func(u *url.URL) HubMatch {
			parts := cleanPath(u)
			m := HubMatch{Platform: "amazon"}

			if len(parts) == 0 {
				return m
			}

			first := strings.ToLower(parts[0])

			// /dp/{ASIN} → product detail page (hub)
			if first == "dp" && len(parts) >= 2 {
				if len(parts) <= 3 {
					m.IsHub = true
					m.HubBoost = hubBoostMedium
					return m
				}
				m.DepthPenalty = depthPenaltyLight
				return m
			}

			// /gp/product/{ASIN} → product detail page (hub)
			if first == "gp" && len(parts) >= 3 && strings.EqualFold(parts[1], "product") {
				if len(parts) <= 4 {
					m.IsHub = true
					m.HubBoost = hubBoostMedium
					return m
				}
				m.DepthPenalty = depthPenaltyLight
				return m
			}

			// /product-reviews/{ASIN} → reviews page (non-hub)
			if first == "product-reviews" {
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			// /offer-listing/{ASIN} → sellers list (non-hub)
			if first == "offer-listing" {
				m.DepthPenalty = depthPenaltyMedium
				return m
			}

			// /s?k=... → search results (non-hub, ephemeral)
			if first == "s" {
				m.DepthPenalty = depthPenaltyHeavy
				return m
			}

			return m
		},
	},
}
