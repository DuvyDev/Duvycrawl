package crawler

import (
	"bytes"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
)

// ImageMeta holds metadata about an image found on a page.
type ImageMeta struct {
	URL     string // Absolute URL of the image
	Alt     string // Alt text
	Title   string // Title attribute
	Context string // Surrounding text for search context
	Width   int    // Width in pixels (0 if unknown)
	Height  int    // Height in pixels (0 if unknown)
}

// ParseResult contains the structured data extracted from an HTML page.
type ParseResult struct {
	Title       string
	Description string
	Content     string       // Visible text content, stripped of HTML
	Links       []string     // Absolute URLs found in <a> tags
	Canonical   string       // Canonical URL if specified
	Language    string       // Detected language code (e.g. "es", "en")
	Images      []ImageMeta  // Images found on the page
}

// Parser extracts structured data from HTML documents.
type Parser struct{}

// NewParser creates a new HTML parser.
func NewParser() *Parser {
	return &Parser{}
}

// Parse takes raw HTML bytes, the Content-Type header, and the page's base URL,
// and extracts the title, description, visible text content, and outbound links.
// It automatically detects and converts non-UTF-8 encodings (e.g. ISO-8859-1,
// Windows-1252) to UTF-8 before parsing.
func (p *Parser) Parse(htmlBody []byte, contentType string, baseURL string) (*ParseResult, error) {
	// Create a charset-aware reader that converts to UTF-8.
	// This inspects both the Content-Type header and the HTML <meta> tags
	// to determine the source encoding.
	utf8Reader, err := charset.NewReader(bytes.NewReader(htmlBody), contentType)
	if err != nil {
		// Fallback: try parsing as-is (assume UTF-8).
		utf8Reader = bytes.NewReader(htmlBody)
	}

	doc, err := goquery.NewDocumentFromReader(utf8Reader)
	if err != nil {
		return nil, err
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	result := &ParseResult{}

	// --- Language detection ---
	// Priority: <html lang="..."> > <meta http-equiv="Content-Language"> > <meta name="language">
	if lang, exists := doc.Find("html").Attr("lang"); exists {
		result.Language = normalizeLanguage(lang)
	}
	if result.Language == "" {
		doc.Find(`meta[http-equiv="Content-Language"]`).Each(func(_ int, s *goquery.Selection) {
			if content, exists := s.Attr("content"); exists {
				result.Language = normalizeLanguage(content)
			}
		})
	}
	if result.Language == "" {
		doc.Find(`meta[name="language"]`).Each(func(_ int, s *goquery.Selection) {
			if content, exists := s.Attr("content"); exists {
				result.Language = normalizeLanguage(content)
			}
		})
	}

	// Extract <title>.
	result.Title = strings.TrimSpace(doc.Find("title").First().Text())

	// Extract <meta name="description">.
	doc.Find(`meta[name="description"]`).Each(func(_ int, s *goquery.Selection) {
		if content, exists := s.Attr("content"); exists {
			result.Description = strings.TrimSpace(content)
		}
	})

	// If no description meta tag, try og:description.
	if result.Description == "" {
		doc.Find(`meta[property="og:description"]`).Each(func(_ int, s *goquery.Selection) {
			if content, exists := s.Attr("content"); exists {
				result.Description = strings.TrimSpace(content)
			}
		})
	}

	// Extract canonical URL.
	doc.Find(`link[rel="canonical"]`).Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists {
			canonical := resolveURL(base, strings.TrimSpace(href))
			if canonical != "" {
				result.Canonical = canonical
			}
		}
	})

	// --- Extract images (before removing elements) ---
	result.Images = extractImages(doc, base)

	// Extract visible text content.
	// Remove script, style, noscript, nav, footer, header elements first.
	doc.Find("script, style, noscript, nav, footer, header, iframe, svg").Remove()

	// Get the text from the main content area.
	// Try <main>, <article>, then fall back to <body>.
	var contentText string
	mainContent := doc.Find("main, article, [role=main]").First()
	if mainContent.Length() > 0 {
		contentText = extractText(mainContent)
	} else {
		contentText = extractText(doc.Find("body"))
	}

	result.Content = normalizeWhitespace(contentText)

	// Truncate content to a reasonable size (100KB of text).
	if len(result.Content) > 100*1024 {
		result.Content = result.Content[:100*1024]
	}

	// Extract links.
	seen := make(map[string]bool)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}

		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
			return
		}

		// Check for nofollow.
		if rel, _ := s.Attr("rel"); strings.Contains(rel, "nofollow") {
			return
		}

		// Resolve relative URLs.
		resolved := resolveURL(base, href)
		if resolved == "" {
			return
		}

		// Filter out non-HTML resources by extension.
		if isBinaryExtension(resolved) {
			return
		}

		// Filter out disallowed paths.
		parsedResolved, err := url.Parse(resolved)
		if err == nil && isDisallowedPath(parsedResolved.Path) {
			return
		}

		// Deduplicate.
		if !seen[resolved] {
			seen[resolved] = true
			result.Links = append(result.Links, resolved)
		}
	})

	return result, nil
}

// extractText recursively extracts visible text from a goquery selection,
// adding appropriate spacing between block elements.
func extractText(s *goquery.Selection) string {
	var buf strings.Builder
	s.Contents().Each(func(_ int, child *goquery.Selection) {
		if goquery.NodeName(child) == "#text" {
			text := child.Text()
			buf.WriteString(text)
		} else {
			// Add space before block elements.
			tag := goquery.NodeName(child)
			if isBlockElement(tag) {
				buf.WriteString(" ")
			}
			buf.WriteString(extractText(child))
			if isBlockElement(tag) {
				buf.WriteString(" ")
			}
		}
	})
	return buf.String()
}

// resolveURL resolves a potentially relative URL against a base URL.
// Returns an empty string if the URL is invalid or not HTTP(S).
func resolveURL(base *url.URL, href string) string {
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}

	resolved := base.ResolveReference(parsed)

	// Only keep HTTP(S) URLs.
	scheme := strings.ToLower(resolved.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}

	// Remove fragment.
	resolved.Fragment = ""

	return resolved.String()
}

// normalizeWhitespace collapses multiple spaces and newlines into single spaces.
func normalizeWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// isBlockElement returns true for HTML tags that are typically block-level.
func isBlockElement(tag string) bool {
	switch tag {
	case "div", "p", "br", "h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "table", "tr", "td", "th",
		"blockquote", "pre", "section", "article", "aside",
		"details", "summary", "figure", "figcaption",
		"address", "hr", "dd", "dt", "dl":
		return true
	}
	return false
}

// isBinaryExtension returns true if the URL path ends with a known binary file extension.
func isBinaryExtension(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	ext := strings.ToLower(path.Ext(parsed.Path))
	switch ext {
	case ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".zip", ".tar", ".gz", ".rar", ".7z",
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".webp", ".ico",
		".mp3", ".mp4", ".avi", ".mkv", ".mov", ".wmv", ".flv", ".wav",
		".exe", ".msi", ".dmg", ".apk", ".deb", ".rpm",
		".css", ".js", ".json", ".xml", ".rss", ".atom",
		".woff", ".woff2", ".ttf", ".eot", ".otf",
		".iso", ".bin", ".img":
		return true
	}
	return false
}

// normalizeLanguage extracts a clean 2-letter ISO 639-1 language code
// from various formats like "es", "es-UY", "en-US", "pt-BR".
func normalizeLanguage(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	// Take only the primary language tag (before - or _).
	if i := strings.IndexAny(raw, "-_"); i > 0 {
		raw = raw[:i]
	}
	// Validate it looks like a 2-3 letter language code.
	if len(raw) < 2 || len(raw) > 3 {
		return ""
	}
	return raw
}

// extractImages finds all <img> and og:image tags in the document and
// returns their metadata. It filters out tracking pixels, icons, and
// data URIs.
func extractImages(doc *goquery.Document, base *url.URL) []ImageMeta {
	var images []ImageMeta
	seen := make(map[string]bool)

	// Extract og:image (article thumbnails — high value).
	doc.Find(`meta[property="og:image"]`).Each(func(_ int, s *goquery.Selection) {
		if content, exists := s.Attr("content"); exists {
			imgURL := resolveURL(base, strings.TrimSpace(content))
			if imgURL != "" && !seen[imgURL] {
				seen[imgURL] = true
				// Use page title as context for og:image.
				title := strings.TrimSpace(doc.Find("title").First().Text())
				images = append(images, ImageMeta{
					URL:     imgURL,
					Alt:     title,
					Context: title,
				})
			}
		}
	})

	// Extract <img> tags.
	doc.Find("img[src]").Each(func(_ int, s *goquery.Selection) {
		src, exists := s.Attr("src")
		if !exists {
			return
		}

		src = strings.TrimSpace(src)
		// Skip data URIs and empty srcs.
		if src == "" || strings.HasPrefix(src, "data:") {
			return
		}

		imgURL := resolveURL(base, src)
		if imgURL == "" || seen[imgURL] {
			return
		}

		// Only keep actual image files.
		if !isImageExtension(imgURL) {
			return
		}

		// Parse dimensions — filter out tiny images (tracking pixels, icons).
		width, _ := strconv.Atoi(s.AttrOr("width", "0"))
		height, _ := strconv.Atoi(s.AttrOr("height", "0"))
		if (width > 0 && width < 50) || (height > 0 && height < 50) {
			return
		}

		alt := strings.TrimSpace(s.AttrOr("alt", ""))
		title := strings.TrimSpace(s.AttrOr("title", ""))

		// Extract surrounding text as context (parent's text, truncated).
		context := ""
		parent := s.Parent()
		if parent.Length() > 0 {
			context = normalizeWhitespace(parent.Text())
			if len(context) > 200 {
				context = context[:200]
			}
		}

		seen[imgURL] = true
		images = append(images, ImageMeta{
			URL:     imgURL,
			Alt:     alt,
			Title:   title,
			Context: context,
			Width:   width,
			Height:  height,
		})
	})

	// Cap at 50 images per page.
	if len(images) > 50 {
		images = images[:50]
	}

	return images
}

// isImageExtension returns true if the URL looks like an image file.
func isImageExtension(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ext := strings.ToLower(path.Ext(parsed.Path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".avif":
		return true
	case "":
		// URLs without extension might still be images (CDN URLs).
		return true
	}
	return false
}
