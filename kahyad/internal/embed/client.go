// Package embed implements kahyad's client for the local MLX embedding
// service (mlx/embed/server.py, W12-11 §4 model_ver rule + supervision
// contract): Client.EmbedQuery/EmbedBatch call POST /v1/embeddings through
// a kahyad-supervised child process (lazily started via the Supervisor
// interface - kahyad/internal/mlxsup.Supervisor satisfies it), and
// Backfiller (backfill.go) drives post-reindex chunk-vector backfill using
// this client. Nothing outside this package ever talks to the embed HTTP
// service directly.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// modelName is the fixed "model" field POST /v1/embeddings expects (mlx/
// embed/server.py's own MODEL_NAME constant) - distinct from
// cfg.ActiveEmbedModelVer, which additionally encodes the output
// dimension and a schema-version suffix for the model_ver column
// (HANDOFF §4 ⚑: "qwen3-embedding-0.6b:512:v1").
const modelName = "qwen3-embedding-0.6b"

// MaxBatch mirrors mlx/embed/server.py's own per-request cap (W12-11 step
// 1: "batch <= 64 inputs per request"). EmbedBatch rejects anything
// larger locally, before ever making the HTTP call.
const MaxBatch = 64

// Supervisor is the lazy-start dependency Client needs (satisfied by
// kahyad/internal/mlxsup.Supervisor without any adapter code). A narrow
// interface so this package's tests can fake it without spawning a real
// process.
type Supervisor interface {
	// EnsureRunning lazily starts the embed service on first use and
	// blocks until it is healthy, ctx is done, or its own startup grace
	// elapses - see kahyad/internal/mlxsup.Supervisor.EnsureRunning.
	EnsureRunning(ctx context.Context) error
}

// Client talks to the local embed service's HTTP API.
type Client struct {
	baseURL string // e.g. "http://127.0.0.1:8092"
	sup     Supervisor
	http    *http.Client
}

// New constructs a Client. baseURL is the embed service's own origin
// (scheme + host + port, no trailing slash) - kahyad/main.go derives this
// from cfg.EmbedPort, always 127.0.0.1 per HANDOFF §4 ⚑ ("127.0.0.1'e
// bağlı TCP HTTP sunucusu").
func New(baseURL string, sup Supervisor) *Client {
	return &Client{baseURL: baseURL, sup: sup, http: &http.Client{}}
}

// embeddingsRequest/Response mirror mlx/embed/server.py's OpenAI-shaped
// POST /v1/embeddings contract (W12-11 step 1).
type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingsResponse struct {
	Data []embeddingDatum `json:"data"`
}

type embedErrorBody struct {
	Error string `json:"error"`
}

// EmbedBatch embeds 1-64 texts in a single POST /v1/embeddings call,
// lazily starting the embed service first (via Supervisor.EnsureRunning)
// if it is not already running. The returned slice has exactly
// len(texts) entries, in the SAME order as texts (server-reported
// "index" values are used to reorder the response, not assumed to already
// be in order). An empty texts returns (nil, nil) without any network
// call.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > MaxBatch {
		return nil, fmt.Errorf("embed: batch of %d exceeds max %d (caller must chunk itself)", len(texts), MaxBatch)
	}

	if err := c.sup.EnsureRunning(ctx); err != nil {
		return nil, fmt.Errorf("embed: service unavailable: %w", err)
	}

	body, err := json.Marshal(embeddingsRequest{Model: modelName, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var eb embedErrorBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error != "" {
			return nil, fmt.Errorf("embed: service returned %d: %s", resp.StatusCode, eb.Error)
		}
		return nil, fmt.Errorf("embed: service returned status %d", resp.StatusCode)
	}

	var parsed embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embed: response has %d vectors, want %d", len(parsed.Data), len(texts))
	}

	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embed: response index %d out of range [0,%d)", d.Index, len(out))
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embed: response missing vector for index %d", i)
		}
	}
	return out, nil
}

// EmbedQuery embeds exactly one text - the shape search.Searcher's KNN
// leg needs ("embed the query (1 call)", W12-11 step 4).
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
