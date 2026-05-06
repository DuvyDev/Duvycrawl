package crawler

import (
	"net/url"
	"regexp"
	"strings"
)

// ExtractResourceLinks extracts URLs from non-HTML resource bodies (CSS, JS, JSON, XML/RSS/Atom).
func ExtractResourceLinks(body []byte, contentType, baseURL string) []string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/css"):
		return extractFromCSS(body, baseURL)
	case strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript") || strings.Contains(ct, "json"):
		return extractFromText(body, baseURL)
	case strings.Contains(ct, "xml") || strings.Contains(ct, "rss") || strings.Contains(ct, "atom"):
		return extractFromXML(body, baseURL)
	default:
		return nil
	}
}

var (
	cssURLRe       = regexp.MustCompile(`(?:url|@import)\s*\(\s*['"]?([^'"()]+)['"]?\s*\)`)
	quotedURLRe    = regexp.MustCompile(`["']((https?://|//)[^"']+)["']`)
	rootRelativeRe = regexp.MustCompile(`["'](/[^"']+)["']`)
	xmlAttrRe      = regexp.MustCompile(`\b(?:href|src|url|loc)\s*=\s*["']([^"']+)["']`)
)

func extractFromCSS(body []byte, baseURL string) []string {
	return extractWithRegexes(body, baseURL, cssURLRe)
}

func extractFromText(body []byte, baseURL string) []string {
	return extractWithRegexes(body, baseURL, quotedURLRe, rootRelativeRe)
}

func extractFromXML(body []byte, baseURL string) []string {
	return extractWithRegexes(body, baseURL, xmlAttrRe)
}

func extractWithRegexes(body []byte, baseURL string, regexes ...*regexp.Regexp) []string {
	base, _ := url.Parse(baseURL)
	seen := make(map[string]struct{})
	var out []string
	text := string(body)
	for _, re := range regexes {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			raw := strings.TrimSpace(m[1])
			if raw == "" {
				continue
			}
			resolved := resolveURL(base, raw)
			if resolved == "" {
				continue
			}
			if _, ok := seen[resolved]; ok {
				continue
			}
			if isBinaryExtension(resolved) {
				continue
			}
			seen[resolved] = struct{}{}
			out = append(out, resolved)
		}
	}
	return out
}

// isAssetExtension returns true if the URL path ends with a known asset extension.
func isAssetExtension(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ext := strings.ToLower(parsed.Path)
	for _, e := range []string{".css", ".js", ".json", ".xml", ".rss", ".atom"} {
		if strings.HasSuffix(ext, e) {
			return true
		}
	}
	return false
}
