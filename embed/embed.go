// Package embed provides a lightweight client for obtaining text embeddings
// from an Ollama-compatible /api/embed endpoint, plus cosine-similarity helpers
// used by downstream consumers (e.g. the group package).
//
// # Minimal design
//
//   - Client holds only a base URL and an HTTP client; no global state.
//   - Embed() batches all texts in a single HTTP round-trip using Ollama's
//     multi-input request format.
//   - CosineSimilarity() and PairwiseSimilarity() are pure functions — no
//     state, no allocations beyond the output slice.
//
// # Ollama API contract
//
//	POST /api/embed
//	  Request:  {"model":"nomic-embed-text","input":["text1","text2",...]}
//	  Response: {"embeddings":[[0.1,0.2,...],[0.3,0.4,...],...]}
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// DefaultModel is the Ollama embedding model used when Client.Model is empty.
const DefaultModel = "nomic-embed-text"

// DefaultBaseURL is the Ollama server address used when Client.BaseURL is empty.
const DefaultBaseURL = "http://localhost:11434"

// Client calls Ollama's /api/embed endpoint to obtain dense vector embeddings.
//
// Zero value is usable: it defaults to DefaultBaseURL and DefaultModel.
//
//	c := &embed.Client{}
//	vecs, err := c.Embed(ctx, []string{"hello", "world"})
type Client struct {
	// BaseURL is the Ollama root URL, e.g. "http://localhost:11434".
	// Defaults to DefaultBaseURL when empty.
	BaseURL string

	// Model is the Ollama embedding model name, e.g. "nomic-embed-text".
	// Defaults to DefaultModel when empty.
	Model string

	// HTTPClient is the HTTP client used for requests.
	// Defaults to a client with a 60-second timeout when nil.
	HTTPClient *http.Client
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) model() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultModel
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Embed returns one embedding vector per input text, in the same order.
//
// All texts are sent in a single POST /api/embed request.
// Returns an error if the HTTP call fails, the response is malformed, or the
// number of returned vectors does not match len(texts).
// Returns nil, nil when texts is empty.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{
		Model: c.model(),
		Input: texts,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	url := c.baseURL() + "/api/embed"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embed: HTTP POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			return nil, fmt.Errorf("embed: ollama %s: %s", resp.Status, errBody.Error)
		}
		return nil, fmt.Errorf("embed: ollama %s", resp.Status)
	}

	var respBody struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(respBody.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(respBody.Embeddings), len(texts))
	}
	return respBody.Embeddings, nil
}

// CosineSimilarity returns the cosine similarity between two vectors in [−1, 1].
// Returns 0 when either vector has zero magnitude (avoids division by zero).
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// PairwiseSimilarity returns an n×n similarity matrix for the given vectors.
// matrix[i][j] == CosineSimilarity(vecs[i], vecs[j]).
// The diagonal is always 1.0; the matrix is symmetric.
func PairwiseSimilarity(vecs [][]float64) [][]float64 {
	n := len(vecs)
	matrix := make([][]float64, n)
	for i := range matrix {
		matrix[i] = make([]float64, n)
		matrix[i][i] = 1.0
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			s := CosineSimilarity(vecs[i], vecs[j])
			matrix[i][j] = s
			matrix[j][i] = s
		}
	}
	return matrix
}
