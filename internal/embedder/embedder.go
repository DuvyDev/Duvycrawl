// Package embedder provides a lightweight client for the Ollama embeddings API.
// It is used by the crawler to generate vector embeddings for crawled pages,
// enabling semantic search re-ranking.
package embedder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DuvyDev/Duvycrawl/internal/config"
)

const (
	defaultBaseURL = "http://localhost:11434"
	defaultModel   = "all-minilm:l6-v2"
	defaultWorkers = 2
	maxWorkers     = 16
	timeout        = 30 * time.Second
)

// Client is a thin HTTP client for Ollama's /api/embeddings endpoint.
type Client struct {
	baseURL string
	model   string
	workers int
	client  *http.Client
}

// NewClient creates an embedder client using the provided configuration.
func NewClient(cfg config.EmbedderConfig) *Client {
	baseURL := cfg.URL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	} else if workers > maxWorkers {
		workers = maxWorkers
	}

	// Reuse TCP connections aggressively — critical when Ollama is remote.
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	if strings.HasPrefix(baseURL, "https://") {
		transport.ForceAttemptHTTP2 = true
	}

	return &Client{
		baseURL: baseURL,
		model:   model,
		workers: workers,
		client:  &http.Client{Timeout: timeout, Transport: transport},
	}
}

// GenerateEmbedding sends text to Ollama and returns the embedding vector.
func (c *Client) GenerateEmbedding(text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("empty text")
	}

	payload := map[string]any{
		"model":  c.model,
		"prompt": text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling payload: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling ollama embeddings api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		bodyText := strings.TrimSpace(string(bodyBytes))
		if bodyText != "" {
			return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, bodyText)
		}
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var result struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding ollama response: %w", err)
	}

	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}

	// Convert float64 → float32 to halve memory usage.
	embedding := make([]float32, len(result.Embedding))
	for i, v := range result.Embedding {
		embedding[i] = float32(v)
	}
	return embedding, nil
}

// Model returns the configured embedding model name.
func (c *Client) Model() string {
	return c.model
}

// Workers returns the number of concurrent embedding workers to launch.
func (c *Client) Workers() int {
	return c.workers
}
