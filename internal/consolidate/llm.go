package consolidate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// ErrTokenBudget means the model spent its completion budget without producing
// an answer. Deterministic, so callers must not retry it.
var ErrTokenBudget = errors.New("model produced no answer")

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
		return &httpError{
			Status:     resp.StatusCode,
			URL:        url,
			Body:       strings.TrimSpace(string(snippet)),
			RetryAfter: retryAfter(resp),
		}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpError carries the provider's status code so the retry logic can tell a
// busy provider from one that is refusing us.
//
// Previously every non-2xx collapsed into a formatted string, which made 503
// "system cpu overloaded" and 401 "invalid token" indistinguishable to the
// caller — so both got the same two attempts three seconds apart, which is too
// many for the second and nowhere near enough for the first.
type httpError struct {
	Status     int
	URL        string
	Body       string
	RetryAfter time.Duration // from the Retry-After header; zero when absent
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s returned %d %s: %s", e.URL, e.Status, http.StatusText(e.Status), e.Body)
}

// Transient reports whether waiting and trying again could plausibly work.
//
// 429 and the 5xx family are the provider saying "not now". Everything else —
// 400 malformed, 401 bad key, 403 forbidden, 404 wrong model — says "not ever,
// as asked", and repeating the request only spends the quota faster.
func (e *httpError) Transient() bool {
	if e.Status == http.StatusTooManyRequests {
		return true
	}
	return e.Status >= 500 && e.Status <= 599
}

// retryAfter reads the header, accepting both the delay-seconds and HTTP-date
// forms the RFC allows. Zero when absent or unparseable.
func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
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
	// Same guard the anthropic and google providers have. Without it an unset
	// environment variable becomes an empty Bearer token, and the operator's
	// first clue is a 401 from a third party — which reads as "your key is
	// wrong" when the key is fine and simply was not in the server's
	// environment. Say which variable, in the language of the config file.
	if o.apiKey == "" {
		return nil, fmt.Errorf("no API key: set the environment variable named by consolidation.llm.api_key_env")
	}

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
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
				// Reasoning models return their working here and the answer in
				// Content. Read only for diagnostics — never fed downstream.
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}

	if err := o.post(ctx, o.endpoint+"/v1/chat/completions", headers, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("provider returned no choices")
	}

	choice := out.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" {
		// Empty content with a length finish is a reasoning model that spent
		// its whole budget thinking. "no JSON returned" sends the operator
		// looking at the prompt; naming the real cause sends them to the one
		// setting that fixes it.
		if choice.FinishReason == "length" {
			return nil, fmt.Errorf(
				"%w: hit the %d-token limit after %d reasoning tokens. "+
					"Raise consolidation.llm.max_tokens — reasoning models spend it before writing anything",
				ErrTokenBudget, pick(req.MaxTokens, o.tokens()),
				out.Usage.CompletionTokensDetails.ReasoningTokens)
		}
		if choice.Message.ReasoningContent != "" {
			return nil, fmt.Errorf("model returned reasoning but no answer (finish_reason %q)", choice.FinishReason)
		}
	}

	return &Response{
		Text:         choice.Message.Content,
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

// maxAttempts is how many times one completion is tried before giving up.
//
// Five, with exponential backoff, and only for errors that could plausibly
// clear. It used to be two attempts three seconds apart for everything, which
// abandoned a 449-session drain the first time the provider answered
// "system cpu overloaded (current: 99.1%)" — a condition that resolves in
// seconds. Backing off to roughly a minute of patience costs nothing when the
// provider is healthy and saves the whole run when it is briefly not.
const maxAttempts = 5

// backoffBase is the first retry delay. A variable rather than a constant so
// tests can exercise the loop without waiting two real minutes for it.
var backoffBase = 3 * time.Second

// backoff returns how long to wait before the given retry.
//
// Exponential from 3s: 3, 9, 27, 81. A busy provider needs more than a fixed
// three seconds, and a fixed short delay from every client at once is how a
// struggling endpoint is kept struggling.
func backoff(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt; i++ {
		d *= 3
	}
	return d
}

// completeWithRetry runs a completion, retrying only what is worth retrying.
//
// The work is idempotent and has no deadline, so abandoning a run is survivable
// — but only for errors that will recur. A 401 fails identically on attempt
// five; an exhausted token budget is deterministic and bills again each time.
// Those return immediately. Overload and rate limiting get patience.
func completeWithRetry(ctx context.Context, p Provider, req Request) (*Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			wait := backoff(attempt - 1)
			// A provider that says how long to wait knows better than we do.
			var he *httpError
			if errors.As(lastErr, &he) && he.RetryAfter > wait {
				wait = he.RetryAfter
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
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

		// An exhausted completion budget is deterministic: the next attempt
		// fails identically and bills identically. Retrying it doubles the
		// cost of a configuration mistake for no chance of success.
		if errors.Is(err, ErrTokenBudget) {
			return nil, err
		}
		// A refusal we can read is final. Only a busy provider is worth waiting
		// for; a wrong key or an unknown model is not.
		var he *httpError
		if errors.As(err, &he) && !he.Transient() {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// decodeList unmarshals a list of items from a model response that may be
// either a bare JSON array or an object wrapping one.
//
// This exists because the two things we ask for are in tension. Setting
// response_format to json_object is what makes a provider reliably emit valid
// JSON — but that mode requires the top level to be an *object*, so a prompt
// asking for a bare array is asking for something the mode forbids. A
// well-behaved model resolves the contradiction by wrapping the array, and a
// parser expecting only `[...]` then rejects a perfectly good answer on every
// single call.
//
// Providers also disagree about the wrapper's name, so known names are tried
// first and any array-valued property second.
func decodeList[T any](raw string, out *[]T) error {
	trimmed := strings.TrimSpace(extractJSON(raw))
	if trimmed == "" {
		return fmt.Errorf("model returned no JSON")
	}

	if trimmed[0] == '[' {
		return json.Unmarshal([]byte(trimmed), out)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return fmt.Errorf("model returned neither an array nor an object: %w", err)
	}

	for _, key := range []string{"facts", "lessons", "items", "results", "memories", "data", "output", "list"} {
		if v, ok := obj[key]; ok {
			if err := json.Unmarshal(v, out); err == nil {
				return nil
			}
		}
	}
	for _, v := range obj {
		if len(bytes.TrimSpace(v)) > 0 && bytes.TrimSpace(v)[0] == '[' {
			if err := json.Unmarshal(v, out); err == nil {
				return nil
			}
		}
	}

	// An object that is itself a single item, which is what a model returns
	// when it finds exactly one thing and forgets it was asked for a list.
	//
	// The zero-value check is the whole point of this branch. Go's decoder
	// happily unmarshals {"status":"ok"} into any struct, ignoring every
	// unknown key and setting no fields — so without it, a status envelope
	// becomes one blank memory that gets written to the database. Silently
	// storing an empty record is worse than reporting that the reply made no
	// sense.
	var single, zero T
	if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
		if !reflect.DeepEqual(single, zero) {
			*out = []T{single}
			return nil
		}
	}

	return fmt.Errorf("model returned a JSON object with no array in it")
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
