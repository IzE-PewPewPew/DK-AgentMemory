// Package embed turns text into vectors.
//
// The provider is behind an interface for one operational reason: the default
// runs as a separate process. A model that segfaults or exhausts memory takes
// down a sidecar, not the API, and swapping to a hosted provider is a config
// change rather than a redeploy.
//
// Every method degrades rather than fails. A memory written while the embedder
// is unreachable is stored without a vector and picked up by the backfill pass
// later; search with no query vector falls back to BM25. An embedder outage
// costs recall quality, never writes.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// Embedder produces vectors.
type Embedder interface {
	// Embed returns one vector per input, in order. A nil slice at position i
	// means that input could not be embedded.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions is the vector width this provider emits.
	Dimensions() int
	// Name identifies the provider in logs and health output.
	Name() string
	// Health reports whether the provider is currently usable.
	Health(ctx context.Context) error
}

// New builds the configured embedder.
func New(cfg *config.Config) (Embedder, error) {
	client := &http.Client{Timeout: cfg.Embedding.Timeout.Duration()}
	base := common{
		client:    client,
		model:     cfg.Embedding.Model,
		dims:      cfg.Embedding.Dimensions,
		batchSize: cfg.Embedding.BatchSize,
		apiKey:    cfg.EmbeddingAPIKey(),
	}

	switch cfg.Embedding.Provider {
	case "none":
		return &noop{dims: cfg.Embedding.Dimensions}, nil
	case "local":
		base.endpoint = strings.TrimRight(cfg.Embedding.Endpoint, "/")
		return &local{common: base}, nil
	case "ollama":
		base.endpoint = strings.TrimRight(cfg.Embedding.Endpoint, "/")
		return &ollama{common: base}, nil
	case "openai":
		base.endpoint = strings.TrimRight(orDefault(cfg.Embedding.Endpoint, "https://api.openai.com"), "/")
		return &openAI{common: base}, nil
	case "voyage":
		base.endpoint = strings.TrimRight(orDefault(cfg.Embedding.Endpoint, "https://api.voyageai.com"), "/")
		return &voyage{common: base}, nil
	default:
		return nil, fmt.Errorf("embedding.provider %q is not supported", cfg.Embedding.Provider)
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

type common struct {
	client    *http.Client
	endpoint  string
	model     string
	dims      int
	batchSize int
	apiKey    string
}

func (c common) Dimensions() int { return c.dims }

// post sends a JSON request and decodes a JSON response.
func (c common) post(ctx context.Context, url string, body any, headers map[string]string, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Cap the body: an HTML error page from a misrouted proxy is a common
		// failure here and pasting a whole page into a log helps nobody.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// batched splits texts into provider-sized batches and concatenates results.
func batched(ctx context.Context, texts []string, size int, fn func(context.Context, []string) ([][]float32, error)) ([][]float32, error) {
	if size < 1 {
		size = 32
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		end := start + size
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := fn(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("provider returned %d vectors for %d inputs", len(vecs), end-start)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// --- none ------------------------------------------------------------------

// noop is the `none` provider: keyword search only, no outbound calls, no
// sidecar. A legitimate choice for an air-gapped single-user install.
type noop struct{ dims int }

func (n *noop) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (n *noop) Dimensions() int                                      { return n.dims }
func (n *noop) Name() string                                         { return "none" }
func (n *noop) Health(context.Context) error                         { return nil }

// --- local sidecar ---------------------------------------------------------

// local talks to the bundled FastAPI sidecar in deploy/embed.
type local struct{ common }

func (l *local) Name() string { return "local:" + l.model }

type localRequest struct {
	Texts []string `json:"texts"`
	Model string   `json:"model,omitempty"`
}

type localResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Dimensions int         `json:"dimensions"`
}

func (l *local) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return batched(ctx, texts, l.batchSize, func(ctx context.Context, chunk []string) ([][]float32, error) {
		var resp localResponse
		if err := l.post(ctx, l.endpoint+"/embed", localRequest{Texts: chunk, Model: l.model}, nil, &resp); err != nil {
			return nil, err
		}
		if err := checkDims(resp.Embeddings, l.dims); err != nil {
			return nil, err
		}
		return resp.Embeddings, nil
	})
}

func (l *local) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("embedding sidecar unreachable at %s: %w", l.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding sidecar at %s returned %s", l.endpoint, resp.Status)
	}
	return nil
}

// --- ollama ----------------------------------------------------------------

type ollama struct{ common }

func (o *ollama) Name() string { return "ollama:" + o.model }

func (o *ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return batched(ctx, texts, o.batchSize, func(ctx context.Context, chunk []string) ([][]float32, error) {
		var resp struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		body := map[string]any{"model": o.model, "input": chunk}
		if err := o.post(ctx, o.endpoint+"/api/embed", body, nil, &resp); err != nil {
			return nil, err
		}
		if err := checkDims(resp.Embeddings, o.dims); err != nil {
			return nil, err
		}
		return resp.Embeddings, nil
	})
}

func (o *ollama) Health(ctx context.Context) error {
	_, err := o.Embed(ctx, []string{"health"})
	return err
}

// --- openai ----------------------------------------------------------------

type openAI struct{ common }

func (o *openAI) Name() string { return "openai:" + o.model }

func (o *openAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.apiKey == "" {
		return nil, fmt.Errorf("openai embeddings need an API key; set the variable named by embedding.api_key_env")
	}
	return batched(ctx, texts, o.batchSize, func(ctx context.Context, chunk []string) ([][]float32, error) {
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		body := map[string]any{"model": o.model, "input": chunk, "dimensions": o.dims}
		headers := map[string]string{"Authorization": "Bearer " + o.apiKey}
		if err := o.post(ctx, o.endpoint+"/v1/embeddings", body, headers, &resp); err != nil {
			return nil, err
		}

		// The API documents that results may be out of order, so they are
		// placed by index rather than appended in arrival order.
		out := make([][]float32, len(chunk))
		for _, d := range resp.Data {
			if d.Index < 0 || d.Index >= len(out) {
				return nil, fmt.Errorf("provider returned index %d for a batch of %d", d.Index, len(out))
			}
			out[d.Index] = d.Embedding
		}
		if err := checkDims(out, o.dims); err != nil {
			return nil, err
		}
		return out, nil
	})
}

func (o *openAI) Health(ctx context.Context) error {
	if o.apiKey == "" {
		return fmt.Errorf("no API key configured")
	}
	_, err := o.Embed(ctx, []string{"health"})
	return err
}

// --- voyage ----------------------------------------------------------------

type voyage struct{ common }

func (v *voyage) Name() string { return "voyage:" + v.model }

func (v *voyage) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if v.apiKey == "" {
		return nil, fmt.Errorf("voyage embeddings need an API key; set the variable named by embedding.api_key_env")
	}
	return batched(ctx, texts, v.batchSize, func(ctx context.Context, chunk []string) ([][]float32, error) {
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		body := map[string]any{"model": v.model, "input": chunk, "input_type": "document"}
		headers := map[string]string{"Authorization": "Bearer " + v.apiKey}
		if err := v.post(ctx, v.endpoint+"/v1/embeddings", body, headers, &resp); err != nil {
			return nil, err
		}
		out := make([][]float32, len(chunk))
		for _, d := range resp.Data {
			if d.Index < 0 || d.Index >= len(out) {
				return nil, fmt.Errorf("provider returned index %d for a batch of %d", d.Index, len(out))
			}
			out[d.Index] = d.Embedding
		}
		if err := checkDims(out, v.dims); err != nil {
			return nil, err
		}
		return out, nil
	})
}

func (v *voyage) Health(ctx context.Context) error {
	if v.apiKey == "" {
		return fmt.Errorf("no API key configured")
	}
	_, err := v.Embed(ctx, []string{"health"})
	return err
}

// --- shared ----------------------------------------------------------------

// checkDims fails loudly on a width mismatch.
//
// A provider returning 1536 dimensions into a vector(384) column produces a
// Postgres error on every insert, at write time, far from the cause. Catching
// it here names the actual problem: the model and the schema disagree.
func checkDims(vecs [][]float32, want int) error {
	for i, v := range vecs {
		if len(v) == 0 {
			return fmt.Errorf("provider returned an empty vector at position %d", i)
		}
		if len(v) != want {
			return fmt.Errorf(
				"provider returned %d-dimensional vectors but embedding.dimensions is %d; "+
					"they must match, and changing dimensions requires re-embedding the corpus",
				len(v), want)
		}
	}
	return nil
}

// One is a convenience for the single-text case, which is most of the calls.
func One(ctx context.Context, e Embedder, text string) []float32 {
	if e == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	vecs, err := e.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		// Deliberately swallowed. Callers use this on the write and query
		// paths, where the correct response to an embedder problem is reduced
		// recall rather than a failed request. The health endpoint is where an
		// embedder outage is meant to become visible.
		return nil
	}
	return vecs[0]
}
