package sitehints

import (
	"testing"
)

func TestReddit(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://www.reddit.com/r/Warframe", true, "subreddit root"},
		{"https://reddit.com/r/Warframe", true, "subreddit root no www"},
		{"https://reddit.com/r/Warframe/", true, "subreddit root trailing slash"},
		{"https://old.reddit.com/r/golang", true, "old.reddit subreddit"},
		{"https://reddit.com/r/Warframe/comments/abc123/some_post", false, "comment thread"},
		{"https://reddit.com/r/Warframe/wiki/faq", false, "wiki page"},
		{"https://reddit.com/r/Warframe/top", false, "sort page"},
		{"https://reddit.com/", false, "homepage"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "reddit")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
			if tt.isHub && m.HubName == "" {
				t.Errorf("Classify(%q): HubName should not be empty for hub pages", tt.url)
			}
			if tt.isHub && m.HubBoost <= 0 {
				t.Errorf("Classify(%q): hub should have positive boost, got %v", tt.url, m.HubBoost)
			}
			if !tt.isHub && m.Platform == "reddit" && m.DepthPenalty < 0 {
				t.Errorf("Classify(%q): depth penalty should be >= 0, got %v", tt.url, m.DepthPenalty)
			}
		})
	}
}

func TestGitHub(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://github.com/chromium/chromium", true, "repo root"},
		{"https://github.com/nicbarker/clay", true, "repo root"},
		{"https://github.com/nicbarker", true, "user profile"},
		{"https://github.com/chromium/chromium/releases/tag/150.0.7832.1", false, "release tag"},
		{"https://github.com/chromium/chromium/issues/123", false, "specific issue"},
		{"https://github.com/chromium/chromium/issues", false, "issues list"},
		{"https://github.com/chromium/chromium/tree/main/src", false, "source tree"},
		{"https://github.com/chromium/chromium/blob/main/README.md", false, "file blob"},
		{"https://github.com/chromium/chromium/pull/456", false, "pull request"},
		{"https://github.com/explore", false, "reserved path"},
		{"https://github.com/features", false, "reserved path"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "github")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
			if tt.isHub && m.HubName == "" {
				t.Errorf("Classify(%q): HubName should not be empty for hub pages", tt.url)
			}
			if tt.isHub && m.HubBoost <= 0 {
				t.Errorf("Classify(%q): hub should have positive boost, got %v", tt.url, m.HubBoost)
			}
		})
	}
}

func TestGitHubDepthPenalty(t *testing.T) {
	// Release tags should have a heavier penalty than issues lists.
	release := Classify("https://github.com/chromium/chromium/releases/tag/150.0.7832.1", "github")
	issuesList := Classify("https://github.com/chromium/chromium/issues", "github")
	specificIssue := Classify("https://github.com/chromium/chromium/issues/123", "github")

	if release.DepthPenalty <= specificIssue.DepthPenalty {
		t.Errorf("release penalty (%v) should be > specific issue penalty (%v)", release.DepthPenalty, specificIssue.DepthPenalty)
	}
	if specificIssue.DepthPenalty <= issuesList.DepthPenalty {
		t.Errorf("specific issue penalty (%v) should be > issues list penalty (%v)", specificIssue.DepthPenalty, issuesList.DepthPenalty)
	}
}

func TestYouTube(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://youtube.com/@mkbhd", true, "channel @handle"},
		{"https://youtube.com/c/mkbhd", true, "channel /c/ path"},
		{"https://youtube.com/channel/UC123abc", true, "channel /channel/ path"},
		{"https://youtube.com/user/mkbhd", true, "channel /user/ path"},
		{"https://youtube.com/@mkbhd/videos", true, "channel videos tab"},
		{"https://youtube.com/watch?v=abc123", false, "video"},
		{"https://youtube.com/shorts/abc123", false, "short"},
		{"https://youtube.com/playlist?list=abc123", false, "playlist"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "youtube")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestWikipedia(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://es.wikipedia.org/wiki/Uruguay", true, "article"},
		{"https://en.wikipedia.org/wiki/Golang", true, "EN article"},
		{"https://en.wikipedia.org/wiki/Special:Search", false, "special page"},
		{"https://en.wikipedia.org/wiki/Talk:Golang", false, "talk page"},
		{"https://en.wikipedia.org/wiki/User:Someone", false, "user page"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "wikipedia")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestStackOverflow(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://stackoverflow.com/questions/12345/how-to-do-something", true, "question"},
		{"https://stackoverflow.com/questions/tagged/go", false, "tag listing"},
		{"https://stackoverflow.com/users/12345/john", false, "user profile"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "stackoverflow")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestNpm(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://www.npmjs.com/package/react", true, "package root"},
		{"https://www.npmjs.com/package/@types/node", true, "scoped package root"},
		{"https://www.npmjs.com/package/react/v/18.0.0", false, "version page"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "npm")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestPyPI(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://pypi.org/project/django", true, "project root"},
		{"https://pypi.org/project/django/4.2.1", false, "version page"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "pypi")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestTwitch(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://twitch.tv/shroud", true, "streamer page"},
		{"https://twitch.tv/shroud/clips", false, "clips page"},
		{"https://twitch.tv/directory", false, "reserved path"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "twitch")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestSteam(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://store.steampowered.com/app/570/Dota_2", true, "game page"},
		{"https://store.steampowered.com/app/570", true, "game page no name"},
		{"https://store.steampowered.com/app/570/Dota_2/reviews", false, "reviews sub-page"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "steam")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestNoMatch(t *testing.T) {
	m := Classify("https://example.com/some/page", "reddit")
	if m.IsHub || m.HubBoost != 0 || m.DepthPenalty != 0 || m.Platform != "" {
		t.Errorf("unmatched URL should return zero HubMatch, got %+v", m)
	}
}

func TestEmptyPlatform(t *testing.T) {
	// With platform="" it should still detect GitHub from the URL.
	m := Classify("https://github.com/chromium/chromium", "")
	if !m.IsHub || m.Platform != "github" {
		t.Errorf("expected github hub match with empty platform, got %+v", m)
	}
}

func TestNuGet(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://www.nuget.org/packages/Newtonsoft.Json", true, "package root"},
		{"https://www.nuget.org/packages/Newtonsoft.Json/13.0.3", false, "version page"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "nuget")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}

func TestAmazon(t *testing.T) {
	tests := []struct {
		url   string
		isHub bool
		label string
	}{
		{"https://www.amazon.com/dp/B08N5WRWNW", true, "product dp short"},
		{"https://www.amazon.com/dp/B08N5WRWNW/Product-Name", true, "product dp with name"},
		{"https://www.amazon.com/gp/product/B08N5WRWNW", true, "product gp path"},
		{"https://www.amazon.co.uk/dp/B08N5WRWNW", true, "UK product"},
		{"https://www.amazon.com/product-reviews/B08N5WRWNW", false, "reviews page"},
		{"https://www.amazon.com/offer-listing/B08N5WRWNW", false, "offers page"},
		{"https://www.amazon.com/s?k=headphones", false, "search results"},
		{"https://www.amazon.com/", false, "homepage"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m := Classify(tt.url, "amazon")
			if m.IsHub != tt.isHub {
				t.Errorf("Classify(%q): IsHub = %v, want %v", tt.url, m.IsHub, tt.isHub)
			}
		})
	}
}
