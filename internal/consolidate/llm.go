package consolidate

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

// Provider is a chat-completion backend.
//
// Behind an interface because the choice is a running cost, not an architecture
// decision. Someone self-hosting wants an OpenAI-compatible endpoint pointed at
// their own machine; someone on a laptop wants a hosted small model. Neither
// should require touching a line of code, so the provider is a config key.
type Provider interface {
	Complete(ctx context.Context, req Request) (*Response, error)
	Name() string
}

// Request is one completion.
type Request struct {
	System    string
	Prompt    string
	MaxTokens int
	// JSON asks the model to reply with JSON only. Enforced by prompt rather
	// than by a provider-specific structured-output feature, because those
	// differ across the four providers and this is the one thing all of them do
	// the same way.
	JSON bool
}

// Response carries the text and what it cost.
//
// Token counts are returned even when a provider does not report them, in which
// case they are estimated. A cost you cannot see is a cost you find out about
// on an invoice.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	Estimated    bool
}

// New builds the configured provider.
func New(cfg *config.Config) (Provider, error) {
	llm := cfg.Consolidation.LLM
	key := cfg.LLMAPIKey()

	client := &http.Client{Timeout: llm.Timeout.Duration()}
	base := common{client: client, model: llm.Model, apiKey: key, maxTokens: llm.MaxTokens}

	switch llm.Provider {
	case "anthropic":
		base.endpoint = orDefault(llm.BaseURL, "https://api.anthropic.com")
		return &anthropic{common: base}, nil
	case "openai":
		base.endpoint = orDefault(llm.BaseURL, "https://api.openai.com")
		return &openAI{common: base}, nil
	case "openai-compatible":
		if llm.BaseURL == "" {
			return nil, fmt.Errorf("consolidation.llm.base_url is required for the openai-compatible provider")
		}
		base.endpoint = llm.BaseURL
		return &openAI{common: base}, nil
	case "google":
		base.endpoint = orDefault(llm.BaseURL, "https://generativelanguage.googleapis.com")
		return &google{common: base}, nil
	default:
		return nil, fmt.Errorf("consolidation.llm.provider %q is not supported", llm.Provider)
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimRight(v, "/")
}

type common struct {
	client    *http.Client
	endpoint  string
	model     string
	apiKey    string
	maxTokens int
}

func (c common) post(ctx context.Context, url string, headers map[string]string, body, out any) error {
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
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c common) tokens() int {
	if c.maxTokens > 0 {
		return c.maxTokens
	}
	return 2000
}

// estimate approximates a token count when a provider does not report one.
func estimate(s string) int { return len(s)/4 + 1 }

// --- anthropic -------------------------------------------------------------

type anthropic struct{ common }

func (a *anthropic) Name() string { return "anthropic:" + a.model }

func (a *anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	if a.apiKey == "" {
		return nil, fmt.Errorf("no API key: set the environment variable named by consolidation.llm.api_key_env")
	}

	body := map[string]any{
		"model":      a.model,
		"max_tokens": pick(req.MaxTokens, a.tokens()),
		"messages":   []any{map[string]any{"role": "user", "content": req.Prompt}},
	}
	if req.System != "" {
		body["system"] = req.System
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	err := a.post(ctx, a.endpoint+"/v1/messages", map[string]string{
		"x-api-key":         a.apiKey,
		"anthropic-version": "2023-06-01",
	}, body, &out)
	if err != nil {
		return nil, err
	}

	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return &Response{
		Text:         text.String(),
		InputTokens:  out.Usage.InputTokens,
		OutputTokens: out.Usage.OutputTokens,
	}, nil
}

// --- openai and compatible -------------------------------------------------

type openAI struct{ common }

func (o *openAI) Name() string { return "openai:" + o.model }

func (o *openAI) Complete(ctx context.Context, req Request) (*Response, error) {
	messages := []any{}
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Prompt})

	body := map[string]any{
		"model":                 o.model,
		"messages":              messages,
		"max_completion_tokens": pick(req.MaxTokens, o.tokens()),
	}
	if req.JSON {
		body["response_format"] = map[string]any{"type": "json_object"}
	}

	headers := map[string]string{}
	if o.apiKey != "" {
		headers["Authorization"] = "Bearer " + o.apiKey
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	if err := o.post(ctx, o.endpoint+"/v1/chat/completions", headers, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("provider returned no choices")
	}
	return &Response{
		Text:         out.Choices[0].Message.Content,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}, nil
}

// --- google ----------------------------------------------------------------

type google struct{ common }

func (g *google) Name() string { return "google:" + g.model }

func (g *google) Complete(ctx context.Context, req Request) (*Response, error) {
	if g.apiKey == "" {
		return nil, fmt.Errorf("no API key: set the environment variable named by consolidation.llm.api_key_env")
	}

	body := map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": req.Prompt}},
		}},
		"generationConfig": map[string]any{
			"maxOutputTokens": pick(req.MaxTokens, g.tokens()),
		},
	}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": req.System}},
		}
	}
	if req.JSON {
		body["generationConfig"].(map[string]any)["responseMimeType"] = "application/json"
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.endpoint, g.model)
	if err := g.post(ctx, url, map[string]string{"x-goog-api-key": g.apiKey}, body, &out); err != nil {
		return nil, err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("provider returned no candidates")
	}

	var text strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	return &Response{
		Text:         text.String(),
		InputTokens:  out.UsageMetadata.PromptTokenCount,
		OutputTokens: out.UsageMetadata.CandidatesTokenCount,
	}, nil
}

func pick(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

// completeWithRetry runs a completion with a small bounded retry.
//
// Two attempts, not five. Rate limits and transient 5xx are worth one retry;
// beyond that the run is abandoned and picked up on the next tick, because the
// work is idempotent and there is no deadline. A long retry loop inside a
// scheduled job is how one bad hour becomes a queue nothing drains.
func completeWithRetry(ctx context.Context, p Provider, req Request) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		resp, err := p.Complete(ctx, req)
		if err == nil {
			if resp.InputTokens == 0 && resp.OutputTokens == 0 {
				resp.InputTokens = estimate(req.System + req.Prompt)
				resp.OutputTokens = estimate(resp.Text)
				resp.Estimated = true
			}
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// extractJSON pulls a JSON value out of a model response.
//
// Models wrap JSON in prose and fences however they are asked not to. Failing
// the whole run over a ```json fence would make the pipeline's reliability a
// function of the model's mood.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}

	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return s
	}
	open := s[start]
	closeCh := byte(']')
	if open == '{' {
		closeCh = '}'
	}

	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == open:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
