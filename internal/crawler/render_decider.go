package crawler

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/DuvyDev/Duvycrawl/internal/config"
)

func renderingEnabled(cfg config.RenderingConfig) bool {
	return cfg.Enabled && cfg.Mode != "disabled"
}

func shouldRenderStatus(cfg config.RenderingConfig, result *FetchResult) (bool, string) {
	if !renderingEnabled(cfg) || result == nil {
		return false, ""
	}
	if cfg.Mode == "force" && isHTMLContentType(result.ContentType) {
		return true, "force"
	}
	for _, code := range cfg.RenderOnStatusCodes {
		if result.StatusCode == code {
			return true, fmt.Sprintf("status_%d", result.StatusCode)
		}
	}
	return false, ""
}

func shouldRenderParsed(cfg config.RenderingConfig, result *FetchResult, parsed *ParseResult) (bool, string) {
	if !renderingEnabled(cfg) || result == nil || parsed == nil {
		return false, ""
	}
	if cfg.Mode == "force" {
		return true, "force"
	}

	body := lowerBodySample(result.Body)
	if containsJavaScriptRequiredMessage(body) {
		return true, "javascript_required_message"
	}
	if containsKnownInterstitial(body) {
		return true, "browser_interstitial"
	}
	if looksLikeEmptySPA(body, parsed) {
		return true, "empty_spa_shell"
	}

	contentChars := len(strings.TrimSpace(parsed.Content))
	links := len(parsed.Anchors)
	if contentChars < cfg.MinTextChars && links < cfg.MinLinks {
		return true, "weak_http_parse"
	}

	return false, ""
}

func lowerBodySample(body []byte) string {
	const maxSample = 128 * 1024
	if len(body) > maxSample {
		body = body[:maxSample]
	}
	body = bytes.ToLower(body)
	return string(body)
}

func containsJavaScriptRequiredMessage(body string) bool {
	markers := []string{
		"enable javascript",
		"javascript is required",
		"requires javascript",
		"please enable js",
		"please enable javascript",
		"necesitas activar javascript",
		"necesita activar javascript",
		"habilita javascript",
		"active javascript",
	}
	for _, marker := range markers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func containsKnownInterstitial(body string) bool {
	markers := []string{
		"just a moment",
		"checking your browser",
		"verify you are human",
		"ddos-guard",
		"cf-browser-verification",
		"cf-challenge",
	}
	for _, marker := range markers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func looksLikeEmptySPA(body string, parsed *ParseResult) bool {
	if len(strings.TrimSpace(parsed.Content)) >= 200 || len(parsed.Anchors) > 0 {
		return false
	}
	spaMarkers := []string{
		`id="root"`,
		`id="app"`,
		`id="__next"`,
		`id="__nuxt"`,
		"data-reactroot",
		"ng-version",
	}
	for _, marker := range spaMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}
