package crawler

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/saintfish/chardet"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/htmlindex"
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

// LinkAnchor represents a hyperlink with its anchor text and surrounding context.
type LinkAnchor struct {
	URL    string // Absolute URL
	Anchor string // Anchor text of the link
}

// ParseResult contains the structured data extracted from an HTML page.
type ParseResult struct {
	Title       string
	H1          string
	H2          string
	Description string
	Content     string       // Visible text content, stripped of HTML
	Links       []string     // Absolute URLs found in <a> tags
	Anchors     []LinkAnchor // Links with anchor text for backlink indexing
	Canonical   string       // Canonical URL if specified
	Language    string       // Detected language code (e.g. "es", "en")
	Images      []ImageMeta  // Images found on the page
	PublishedAt time.Time    // Publication date extracted from meta/JSON-LD

	// Schema.org (JSON-LD) structured data
	SchemaType        string
	SchemaTitle       string
	SchemaDescription string
	SchemaImage       string
	SchemaAuthor      string
	SchemaKeywords    string
	SchemaRating      float64
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
	// Convert the raw body to UTF-8 using a robust 3-tier strategy.
	// 1. DetermineEncoding (Content-Type header + HTML meta tags)
	// 2. chardet heuristic fallback
	// 3. Return as-is if everything fails
	htmlBody = toUTF8(htmlBody, contentType)

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(htmlBody))
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

	// Fallback/override for generic titles (e.g., Reddit SPA "Reddit - ...")
	if result.Title == "" || strings.HasPrefix(result.Title, "Reddit -") {
		shredditTitle := doc.Find("shreddit-title").AttrOr("title", "")
		if shredditTitle == "" {
			shredditTitle = doc.Find("shreddit-post").AttrOr("post-title", "")
		}

		if shredditTitle != "" {
			result.Title = strings.TrimSpace(shredditTitle)
		} else {
			// Try og:title
			doc.Find(`meta[property="og:title"]`).Each(func(_ int, s *goquery.Selection) {
				if content, exists := s.Attr("content"); exists {
					result.Title = strings.TrimSpace(content)
				}
			})
		}
	}

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

	// --- Extract publication date ---
	result.PublishedAt = extractPublishedAt(doc)

	// --- Extract schema.org JSON-LD structured data ---
	schema := extractJSONLD(doc, base)
	result.SchemaType = schema.Type
	result.SchemaTitle = schema.Title
	result.SchemaDescription = schema.Description
	result.SchemaImage = schema.Image
	result.SchemaAuthor = schema.Author
	result.SchemaKeywords = schema.Keywords
	result.SchemaRating = schema.Rating
	if result.PublishedAt.IsZero() && !schema.DatePublished.IsZero() {
		result.PublishedAt = schema.DatePublished
	}

	// --- Extract images (before removing elements) ---
	result.Images = extractImages(doc, base)

	// Extract resource links (script, link, iframe) before removing elements.
	seen := make(map[string]bool)
	doc.Find("script[src], link[href], iframe[src]").Each(func(_ int, s *goquery.Selection) {
		var href string
		var ok bool
		switch goquery.NodeName(s) {
		case "script":
			href, ok = s.Attr("src")
		case "link":
			href, ok = s.Attr("href")
		case "iframe":
			href, ok = s.Attr("src")
		}
		if !ok {
			return
		}
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "data:") || strings.HasPrefix(href, "#") {
			return
		}
		resolved := resolveURL(base, href)
		if resolved == "" {
			return
		}
		if isBinaryExtension(resolved) {
			return
		}
		if parsedResolved, err := url.Parse(resolved); err == nil && isDisallowedPath(parsedResolved.Path) {
			return
		}
		if !seen[resolved] {
			seen[resolved] = true
			result.Links = append(result.Links, resolved)
			result.Anchors = append(result.Anchors, LinkAnchor{
				URL:    resolved,
				Anchor: "",
			})
		}
	})

	// Extract visible text content.
	// Remove non-content elements: scripts, styles, navigation, chrome, ads, etc.
	doc.Find("script, style, noscript, nav, footer, header, iframe, svg, aside, form, button, input, select, textarea, label, fieldset, legend, dialog, menu, menuitem, template, slot, canvas, video, audio, source, track, map, area, object, embed, applet").Remove()

	// Remove common content noise: ads, sidebars, comments, widgets, code blocks, etc.
	doc.Find(".ad, .ads, .advertisement, .ad-container, .google-ad, .sidebar, #sidebar, .comments, .comment-list, .comment-section, .related-posts, .newsletter, .cookie-banner, .popup, .modal, .social-share, .share-buttons, .author-bio, .toc, .table-of-contents, pre, code").Remove()

	// Get the text from the main content area.
	// Try <main>, <article>, then fall back to <body>.
	contentRoot := doc.Find("main, article, [role=main]").First()
	if contentRoot.Length() == 0 {
		contentRoot = doc.Find("body").First()
	}

	result.H1 = extractHeadingText(contentRoot, "h1", 1200)
	result.H2 = extractHeadingText(contentRoot, "h2", 2000)

	var contentText string
	shredditPost := doc.Find("shreddit-post").First()
	if shredditPost.Length() > 0 {
		textBody := shredditPost.Find("[slot='text-body']")
		if textBody.Length() > 0 {
			contentText = extractText(textBody)
		} else {
			contentText = extractText(shredditPost)
		}
	} else {
		contentText = extractText(contentRoot)
	}

	result.Content = normalizeWhitespace(contentText)

	// Fallback for SPAs where dynamic content is empty but meta/schema data exists.
	// Priority: og:description > schema_description > h1 + h2 headings.
	if result.Content == "" {
		if result.Description != "" {
			result.Content = result.Description
		} else if result.SchemaDescription != "" {
			result.Content = result.SchemaDescription
		} else if result.H1 != "" {
			parts := []string{result.H1}
			if result.H2 != "" {
				parts = append(parts, result.H2)
			}
			result.Content = strings.Join(parts, " ")
		}
	}

	// Truncate content to a reasonable size (100KB of text).
	if len(result.Content) > 100*1024 {
		result.Content = result.Content[:100*1024]
	}

	// Extract links.
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
			anchorText := strings.TrimSpace(s.Text())
			if len(anchorText) > 500 {
				anchorText = anchorText[:500]
			}
			result.Links = append(result.Links, resolved)
			result.Anchors = append(result.Anchors, LinkAnchor{
				URL:    resolved,
				Anchor: anchorText,
			})
		}
	})

	return result, nil
}

// toUTF8 converts raw HTML bytes to UTF-8 using a robust multi-tier strategy.
//
// The single most important rule: if the body is already valid UTF-8, we NEVER
// touch it. This prevents over-eager detectors from “converting" UTF-8 into
// garbage (a common problem when a page declares one encoding in the header
// but actually serves UTF-8).
//
// Tiers:
//  1. Fast path — utf8.Valid check.
//  2. charset.DetermineEncoding — inspects HTTP Content-Type + HTML meta tags.
//  3. chardet heuristic — byte-distribution analysis when meta tags lie.
//  4. Brute force — try common legacy encodings (windows-1252, iso-8859-1, …).
//  5. Return raw bytes and hope for the best.
//
// Every conversion is verified with utf8.Valid before being accepted.
func toUTF8(body []byte, contentType string) []byte {
	if len(body) == 0 {
		return body
	}

	// Tier 1 — Fast path. Most of the modern web is already UTF-8.
	if utf8.Valid(body) {
		return body
	}

	// The body is NOT valid UTF-8. We MUST convert it.
	// Tier 2 — golang's detector (headers + HTML meta tags).
	e, name, _ := charset.DetermineEncoding(body, contentType)
	if e != nil && strings.ToLower(name) != "utf-8" {
		if decoded, err := e.NewDecoder().Bytes(body); err == nil && utf8.Valid(decoded) {
			return decoded
		}
	}

	// Tier 3 — chardet heuristic fallback.
	detector := chardet.NewTextDetector()
	if result, err := detector.DetectBest(body); err == nil && result != nil {
		enc, err := htmlindex.Get(result.Charset)
		if err == nil && enc != nil {
			if decoded, err := enc.NewDecoder().Bytes(body); err == nil && utf8.Valid(decoded) {
				return decoded
			}
		}
	}

	// Tier 4 — brute-force common legacy encodings.
	for _, encName := range []string{"windows-1252", "iso-8859-1", "iso-8859-15", "gbk", "euc-jp", "shift_jis"} {
		if enc, err := htmlindex.Get(encName); err == nil && enc != nil {
			if decoded, err := enc.NewDecoder().Bytes(body); err == nil && utf8.Valid(decoded) {
				return decoded
			}
		}
	}

	// Tier 5 — nothing worked; return raw bytes and hope goquery can cope.
	return body
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

func extractHeadingText(root *goquery.Selection, selector string, maxLen int) string {
	if root == nil || root.Length() == 0 {
		return ""
	}

	parts := make([]string, 0, 4)
	seen := make(map[string]struct{})
	root.Find(selector).Each(func(_ int, s *goquery.Selection) {
		text := normalizeWhitespace(strings.TrimSpace(s.Text()))
		if text == "" {
			return
		}
		if _, exists := seen[text]; exists {
			return
		}
		seen[text] = struct{}{}
		parts = append(parts, text)
	})

	joined := strings.Join(parts, " ")
	if maxLen > 0 && len(joined) > maxLen {
		joined = joined[:maxLen]
	}
	return joined
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

// extractPublishedAt tries multiple strategies to find the publication date
// of an HTML document. It checks meta tags, <time> elements, and JSON-LD
// in order of reliability.
func extractPublishedAt(doc *goquery.Document) time.Time {
	// Strategy 1: Explicit publication meta tags (most reliable).
	metaCandidates := []string{
		`meta[property="article:published_time"]`,
		`meta[name="article:published_time"]`,
		`meta[property="og:article:published_time"]`,
		`meta[name="date"]`,
		`meta[name="pubdate"]`,
		`meta[name="publish_date"]`,
		`meta[name="published_date"]`,
		`meta[name="publication_date"]`,
		`meta[name="DC.date"]`,
		`meta[name="dc.date"]`,
		`meta[property="og:published_time"]`,
	}
	for _, sel := range metaCandidates {
		doc.Find(sel).Each(func(_ int, s *goquery.Selection) {
			if t := parseDateAttribute(s); !t.IsZero() {
				return
			}
		})
	}

	// Try extracting from meta candidates — return first valid date found.
	for _, sel := range metaCandidates {
		var found time.Time
		doc.Find(sel).Each(func(_ int, s *goquery.Selection) {
			if t := parseDateAttribute(s); !t.IsZero() && found.IsZero() {
				found = t
			}
		})
		if !found.IsZero() {
			return found
		}
	}

	// Strategy 2: <time> element with datetime attribute or content.
	if t := extractTimeElement(doc); !t.IsZero() {
		return t
	}

	// Strategy 3: JSON-LD (schema.org) datePublished.
	if schema := extractJSONLD(doc, nil); !schema.DatePublished.IsZero() {
		return schema.DatePublished
	}

	return time.Time{}
}

func parseDateAttribute(s *goquery.Selection) time.Time {
	content, exists := s.Attr("content")
	if !exists {
		return time.Time{}
	}
	return parseDateString(content)
}

func extractTimeElement(doc *goquery.Document) time.Time {
	var best time.Time
	doc.Find("time").Each(func(_ int, s *goquery.Selection) {
		if datetime, exists := s.Attr("datetime"); exists {
			if t := parseDateString(datetime); !t.IsZero() {
				if best.IsZero() || t.Before(best) {
					best = t
				}
			}
		}
	})
	return best
}

// SchemaData holds extracted schema.org JSON-LD fields.
type SchemaData struct {
	Type          string
	Title         string
	Description   string
	Image         string
	Author        string
	Keywords      string
	Rating        float64
	DatePublished time.Time
}

// extractJSONLD parses all application/ld+json script blocks and extracts
// relevant schema.org fields. It handles both single objects and @graph arrays.
func extractJSONLD(doc *goquery.Document, base *url.URL) SchemaData {
	var best SchemaData
	doc.Find(`script[type="application/ld+json"]`).Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text == "" {
			return
		}

		var raw interface{}
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return
		}

		// JSON-LD can be a single object or an array of objects.
		var objects []map[string]interface{}
		switch v := raw.(type) {
		case map[string]interface{}:
			// Single object, possibly with @graph.
			if graph, ok := v["@graph"].([]interface{}); ok {
				for _, item := range graph {
					if m, ok := item.(map[string]interface{}); ok {
						objects = append(objects, m)
					}
				}
			} else {
				objects = append(objects, v)
			}
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					objects = append(objects, m)
				}
			}
		}

		for _, obj := range objects {
			schemaType := stringFromJSONLD(obj["@type"])
			// Skip types that are not useful for search (e.g. BreadcrumbList, WebSite).
			if schemaType == "BreadcrumbList" || schemaType == "WebSite" || schemaType == "Organization" {
				continue
			}

			if best.Type == "" {
				best.Type = schemaType
			}
			if best.Title == "" {
				best.Title = stringFromJSONLD(obj["headline"])
				if best.Title == "" {
					best.Title = stringFromJSONLD(obj["name"])
				}
			}
			if best.Description == "" {
				best.Description = stringFromJSONLD(obj["description"])
			}
			if best.Image == "" {
				imgURL := extractImageURL(obj["image"])
				if imgURL != "" {
					if base != nil {
						best.Image = resolveURL(base, imgURL)
					} else {
						best.Image = imgURL
					}
				}
				if best.Image == "" {
					imgURL = extractImageURL(obj["thumbnailUrl"])
					if imgURL != "" {
						if base != nil {
							best.Image = resolveURL(base, imgURL)
						} else {
							best.Image = imgURL
						}
					}
				}
			}
			if best.Author == "" {
				if author, ok := obj["author"].(map[string]interface{}); ok {
					best.Author = stringFromJSONLD(author["name"])
				} else if a, ok := obj["author"].(string); ok {
					best.Author = a
				}
			}
			if best.Keywords == "" {
				best.Keywords = stringFromJSONLD(obj["keywords"])
				if best.Keywords == "" {
					if sec, ok := obj["articleSection"].(string); ok {
						best.Keywords = sec
					}
				}
			}
			if best.Rating == 0 {
				if ar, ok := obj["aggregateRating"].(map[string]interface{}); ok {
					if rv, ok := ar["ratingValue"].(float64); ok {
						best.Rating = rv
					} else if rvStr, ok := ar["ratingValue"].(string); ok {
						if rv, err := strconv.ParseFloat(rvStr, 64); err == nil {
							best.Rating = rv
						}
					}
				}
			}
			if best.DatePublished.IsZero() {
				if dp, ok := obj["datePublished"].(string); ok && dp != "" {
					best.DatePublished = parseDateString(dp)
				}
				if best.DatePublished.IsZero() {
					if dc, ok := obj["dateCreated"].(string); ok && dc != "" {
						best.DatePublished = parseDateString(dc)
					}
				}
			}
		}
	})
	return best
}

// extractImageURL extracts an image URL from a JSON-LD field that may be
// a string, an ImageObject with a "url" property, or an array of either.
func extractImageURL(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["url"].(string); ok {
			return strings.TrimSpace(s)
		}
		if s, ok := m["contentUrl"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
		for _, item := range arr {
			if s, ok := item.(string); ok && s != "" {
				return strings.TrimSpace(s)
			}
			if m, ok := item.(map[string]interface{}); ok {
				if s, ok := m["url"].(string); ok && s != "" {
					return strings.TrimSpace(s)
				}
				if s, ok := m["contentUrl"].(string); ok && s != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// stringFromJSONLD extracts a string value from a JSON-LD field that may be
// a string, an object with @value, or an array of strings.
func stringFromJSONLD(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["@value"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// dateFormats lists common date formats found in HTML meta tags and JSON-LD,
// ordered from most specific/common to least.
var dateFormats = []string{
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05-0700",
	"2006-01-02",
	"2006-01-02 15:04:05",
	"January 2, 2006",
	"Jan 2, 2006",
	"02 January 2006",
	"02 Jan 2006",
	"02/01/2006",
	"01/02/2006",
	"2 de January de 2006",
	"2 de Jan de 2006",
}

// parseDateString tries multiple date formats and returns the first successful parse.
func parseDateString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	for _, format := range dateFormats {
		if t, err := time.Parse(format, s); err == nil {
			// Sanity check: reject dates before 1990 or far in the future.
			if t.Year() >= 1990 && t.Year() <= time.Now().Year()+1 {
				return t
			}
		}
	}

	// Try stripping timezone suffix like "+00:00" or "Z" after the fact.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if t.Year() >= 1990 && t.Year() <= time.Now().Year()+1 {
			return t
		}
	}

	return time.Time{}
}
