package urlfilter

import (
	"net/url"
	"path"
	"strings"
)

var binaryExtensions = map[string]struct{}{
	".pdf": {}, ".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {}, ".ppt": {}, ".pptx": {},
	".zip": {}, ".tar": {}, ".gz": {}, ".rar": {}, ".7z": {}, ".bz2": {}, ".xz": {},
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".bmp": {}, ".svg": {}, ".webp": {}, ".ico": {},
	".tif": {}, ".tiff": {}, ".heic": {}, ".heif": {}, ".avif": {}, ".apng": {},
	".mp3": {}, ".mp4": {}, ".avi": {}, ".mkv": {}, ".mov": {}, ".wmv": {}, ".flv": {}, ".wav": {},
	".webm": {}, ".m4a": {}, ".ogg": {}, ".ogv": {}, ".flac": {},
	".exe": {}, ".msi": {}, ".dmg": {}, ".apk": {}, ".deb": {}, ".rpm": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".eot": {}, ".otf": {},
	".wasm": {}, ".swf": {}, ".map": {}, ".mjs": {},
	".webmanifest": {}, ".iso": {}, ".bin": {}, ".img": {},
}

var discoveryExtensions = map[string]struct{}{
	".css": {}, ".js": {}, ".json": {}, ".xml": {}, ".rss": {}, ".atom": {},
}

var chromeBlockedExtensions = map[string]struct{}{
	".pdf": {}, ".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {}, ".ppt": {}, ".pptx": {},
	".zip": {}, ".tar": {}, ".gz": {}, ".rar": {}, ".7z": {}, ".bz2": {}, ".xz": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".eot": {}, ".otf": {},
	".mp3": {}, ".mp4": {}, ".avi": {}, ".mkv": {}, ".mov": {}, ".wmv": {}, ".flv": {}, ".wav": {},
	".webm": {}, ".m4a": {}, ".ogg": {}, ".ogv": {}, ".flac": {},
	".wasm": {}, ".swf": {}, ".map": {}, ".webmanifest": {},
	".exe": {}, ".msi": {}, ".dmg": {}, ".apk": {}, ".deb": {}, ".rpm": {},
	".iso": {}, ".bin": {}, ".img": {},
}

var imageExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".bmp": {}, ".svg": {}, ".webp": {}, ".ico": {},
	".tif": {}, ".tiff": {}, ".heic": {}, ".heif": {}, ".avif": {}, ".apng": {},
}

var disallowedPathPrefixes = []string{
	"/wp-admin",
	"/wp-login",
	"/admin",
	"/login",
	"/logout",
	"/signup",
	"/register",
	"/cart",
	"/checkout",
	"/account",
	"/api/",
	"/cgi-bin/",
	"/wiki/special:",
	"/wiki/talk:",
	"/wiki/user:",
	"/wiki/user_talk:",
	"/wiki/wikipedia:",
	"/wiki/file:",
	"/wiki/mediawiki:",
	"/wiki/template:",
	"/wiki/help:",
	"/wiki/category:",
	"/wiki/portal:",
	"/wiki/draft:",
	"/wiki/module:",
}

var challengePathMarkers = []string{
	"/cdn-cgi/",
	"/challenge-platform/",
	"/captcha/",
	"/recaptcha/",
	"/turnstile/",
	"/__cf_chl_",
	"/cf-challenge",
}

// IsBinaryExtension reports whether the URL path points to a non-crawlable file.
func IsBinaryExtension(rawURL string) bool {
	_, ok := binaryExtensions[extension(rawURL)]
	return ok
}

// IsAssetExtension reports whether the URL is a lightweight discovery-only asset.
func IsAssetExtension(rawURL string) bool {
	_, ok := discoveryExtensions[extension(rawURL)]
	return ok
}

// IsImageExtension reports whether the URL path looks like an image. Extensionless
// URLs are accepted because many CDNs serve images without file extensions.
func IsImageExtension(rawURL string) bool {
	ext := extension(rawURL)
	if ext == "" {
		return true
	}
	_, ok := imageExtensions[ext]
	return ok
}

// IsNonIndexableDocumentURL reports whether a URL must never be persisted as a page.
func IsNonIndexableDocumentURL(rawURL string) bool {
	if IsBinaryExtension(rawURL) || IsAssetExtension(rawURL) {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	return IsDisallowedPath(parsed.Path)
}

// IsDisallowedPath catches common crawl traps and non-content paths.
func IsDisallowedPath(rawPath string) bool {
	p := strings.ToLower(rawPath)
	for _, prefix := range disallowedPathPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return IsChallengePath(p)
}

// IsChallengePath catches WAF/challenge internals that should never be followed.
func IsChallengePath(rawPath string) bool {
	p := strings.ToLower(rawPath)
	for _, marker := range challengePathMarkers {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return false
}

// BrowserBlockedURLPatterns returns CDP URL patterns for resources that do not
// help page rendering and should not be downloaded by Chromium.
func BrowserBlockedURLPatterns(blockImages bool) []string {
	exts := make([]string, 0, len(chromeBlockedExtensions)+len(imageExtensions))
	for ext := range chromeBlockedExtensions {
		exts = append(exts, ext)
	}
	if blockImages {
		for ext := range imageExtensions {
			exts = append(exts, ext)
		}
	}

	patterns := make([]string, 0, len(exts)*4)
	for _, ext := range exts {
		patterns = append(patterns,
			"*://*/*"+ext,
			"*://*/*"+ext+"?*",
			"*://*:*/*"+ext,
			"*://*:*/*"+ext+"?*",
		)
	}
	return patterns
}

func extension(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(path.Ext(parsed.Path))
}
