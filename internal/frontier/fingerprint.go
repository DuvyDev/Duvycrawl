package frontier

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// segmentPattern defines a regex pattern and its replacement placeholder
// for identifying variable path segments.
type segmentPattern struct {
	regex       *regexp.Regexp
	placeholder string
	validate    func(string) bool
}

// containsHexLetter returns true if the string has at least one a-f/A-F character.
func containsHexLetter(s string) bool {
	for _, c := range s {
		if (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			return true
		}
	}
	return false
}

// segmentPatterns are tested in order from most specific to most general.
// Each pattern is anchored (^...$) to match complete path segments only.
var segmentPatterns = []segmentPattern{
	{regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`), "{uuid}", nil},
	{regexp.MustCompile(`^[0-9a-fA-F]{64}$`), "{sha256}", containsHexLetter},
	{regexp.MustCompile(`^[0-9a-fA-F]{40}$`), "{sha1}", containsHexLetter},
	{regexp.MustCompile(`^[0-9a-fA-F]{32}$`), "{md5}", containsHexLetter},
	{regexp.MustCompile(`^[0-9a-fA-F]{24}$`), "{oid}", containsHexLetter},
	{regexp.MustCompile(`^[0-9a-fA-F]{8,}$`), "{hex}", containsHexLetter},
	{regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`), "{date}", nil},
	{regexp.MustCompile(`^\d{10}(\d{3})?$`), "{ts}", nil},
	{regexp.MustCompile(`^\d+$`), "{num}", nil},
}

// normalizeSegment checks a single path segment against heuristic patterns.
func normalizeSegment(segment string) (string, bool) {
	for _, p := range segmentPatterns {
		if p.regex.MatchString(segment) {
			if p.validate != nil && !p.validate(segment) {
				continue
			}
			return p.placeholder, true
		}
	}
	return segment, false
}

// FingerprintURL produces a structural fingerprint of the given URL by:
// 1. Replacing variable path segments (IDs, UUIDs, hashes, dates) with placeholders
// 2. Dropping query parameter values, keeping only sorted keys
//
// This allows deduplication of URLs that differ only in variable IDs
// or filter values, e.g.:
//
//	/user/123/profile  →  /user/{num}/profile
//	/user/456/profile  →  /user/{num}/profile (same fingerprint)
//	/search?q=foo      →  /search?q
//	/search?q=bar      →  /search?q (same fingerprint)
func FingerprintURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	path := u.Path
	if path == "" || path == "/" {
		return buildFingerprint(u, path)
	}

	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return buildFingerprint(u, "/")
	}
	segments := strings.Split(trimmed, "/")

	for i, seg := range segments {
		if placeholder, matched := normalizeSegment(seg); matched {
			segments[i] = placeholder
		}
	}

	fingerprintedPath := "/" + strings.Join(segments, "/")
	if strings.HasSuffix(path, "/") {
		fingerprintedPath += "/"
	}

	return buildFingerprint(u, fingerprintedPath)
}

// buildFingerprint reconstructs the URL with the fingerprinted path
// and sorted query keys (values dropped).
func buildFingerprint(u *url.URL, path string) string {
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	b.WriteString(u.Host)
	b.WriteString(path)

	if u.RawQuery != "" {
		keys := sortedQueryKeys(u.Query())
		if len(keys) > 0 {
			b.WriteByte('?')
			b.WriteString(strings.Join(keys, "&"))
		}
	}

	return b.String()
}

// sortedQueryKeys extracts and sorts query parameter keys.
func sortedQueryKeys(params url.Values) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ShouldDropParam reports whether a query parameter is purely cosmetic
// and should be ignored for deduplication purposes.
func ShouldDropParam(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return true
	}

	if strings.HasPrefix(k, "utm_") {
		return true
	}
	if strings.HasPrefix(k, "_hs") || strings.HasPrefix(k, "mkt_") {
		return true
	}
	if strings.Contains(k, "token") || strings.Contains(k, "session") || strings.Contains(k, "signature") {
		return true
	}
	if strings.HasPrefix(k, "x-amz-") {
		return true
	}

	switch k {
	case "fbclid", "gclid", "dclid", "msclkid", "yclid", "igshid",
		"mc_cid", "mc_eid", "zanpid", "_gl",
		"auth", "sid", "phpsessid", "jsessionid",
		"expires", "expiry", "exp":
		return true
	}

	return false
}
