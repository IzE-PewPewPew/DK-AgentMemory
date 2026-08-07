// Package client is the human- and agent-facing half of dkm: an HTTP client for
// the server, project identity resolution, and the offline mirror behind both.
//
// Every read falls back to the local mirror when the server is unreachable and
// every write falls back to a queue, so losing the network degrades what the
// tool knows rather than whether it works. Callers are always told which
// happened — a stale answer presented as a fresh one is worse than an error.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// Client talks to a dkm server, with a local mirror behind it.
type Client struct {
	cfg    *config.Client
	http   *http.Client
	mirror *Mirror

	// offline is set once a request fails to connect, so the remaining commands
	// in a single invocation do not each wait out the same timeout.
	offline bool
}

// ErrOffline means the server could not be reached.
var ErrOffline = errors.New("server unreachable")

// stderr is where the client writes warnings. A variable rather than a direct
// os.Stderr reference so `dkm mcp` can redirect it: in stdio mode stdout is the
// JSON-RPC transport, and anything printed to the wrong stream corrupts the
// protocol.
var stderr io.Writer = os.Stderr

// SetWarningOutput redirects client warnings.
func SetWarningOutput(w io.Writer) { stderr = w }

// APIError is a structured error returned by the server.
type APIError struct {
	Status    int    `json:"-"`
	Code      string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if e.Hint != "" {
		msg += "\n  " + e.Hint
	}
	if e.RequestID != "" && e.Status >= 500 {
		msg += "\n  request id: " + e.RequestID
	}
	return msg
}

// New builds a client from the on-disk configuration.
func New() (*Client, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	return NewWithConfig(cfg)
}

// NewWithConfig builds a client from an explicit configuration.
func NewWithConfig(cfg *config.Client) (*Client, error) {
	c := &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				MaxIdleConns:        4,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}

	if cfg.Sync.Enabled {
		m, err := NewMirror(cfg.Sync.MirrorPath, cfg.Sync.QueueMax)
		if err != nil {
			// A broken mirror must not stop the client working online. It is a
			// cache; losing it costs offline reads, not the whole tool.
			fmt.Fprintf(stderr, "dkm: local mirror unavailable (%v); running online-only\n", err)
		} else {
			c.mirror = m
		}
	}
	return c, nil
}

// Config exposes the loaded client configuration.
func (c *Client) Config() *config.Client { return c.cfg }

// Mirror returns the local mirror, or nil when sync is disabled.
func (c *Client) Mirror() *Mirror { return c.mirror }

// Offline reports whether the last attempt found the server unreachable.
func (c *Client) Offline() bool { return c.offline }

// Project resolves the project identity for a directory.
func (c *Client) Project(dir string) Project {
	if c.cfg.Project.Strategy == "explicit" && c.cfg.Project.Explicit != "" {
		return Project{ID: c.cfg.Project.Explicit, Source: SourceConfig}
	}
	p := ResolveProject(dir)
	if c.cfg.Project.Strategy == "folder" && p.Source == SourceRemote {
		// The operator asked for folder naming explicitly; honour it, warning
		// that it will not match across machines.
		local := ResolveProject(dir)
		local.Source = SourceFolder
		local.Warning = "project.strategy is folder, so this identity will not match a teammate's checkout"
		return local
	}
	if !c.cfg.Project.FallbackWarn {
		p.Warning = ""
	}
	return p
}

// --- transport -------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.doStatus(ctx, method, path, body, out)
	return err
}

// doStatus is do, plus the HTTP status code.
//
// The distinction matters on writes: this API answers 201 for a memory it
// created and 200 for one that already existed, and a client that only checks
// for "no error" reports every re-import as a fresh import. That is a quiet
// false success — the operation was correct, the summary was a lie, and the
// only way to notice is to count rows in the database.
// maxRateLimitRetries bounds the backoff loop. High enough that a bulk import
// rides out a full budget window, low enough that a genuinely stuck client
// gives up rather than hanging for ever.
const maxRateLimitRetries = 30

// retryAfter reads the server's requested delay, with a floor and a ceiling.
//
// The floor stops a malformed or zero value turning backoff into a spin. The
// ceiling stops a hostile or mistaken header parking the client for an hour.
func retryAfter(resp *http.Response) time.Duration {
	wait := 2 * time.Second
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	if wait > 60*time.Second {
		wait = 60 * time.Second
	}
	return wait
}

func (c *Client) doStatus(ctx context.Context, method, path string, body, out any) (int, error) {
	return c.doAttempt(ctx, method, path, body, out, 0)
}

func (c *Client) doAttempt(ctx context.Context, method, path string, body, out any, attempt int) (int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Server+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.offline = true
		return 0, fmt.Errorf("%w: %s: %v", ErrOffline, c.cfg.Server, unwrapNetError(err))
	}
	defer resp.Body.Close()

	// A 429 is not a failure, it is an instruction. The server states exactly
	// how long to wait in Retry-After; honouring it is the entire contract.
	//
	// Not doing so made a bulk import fail 531 of 567 transcripts against its
	// own server: the importer legitimately writes three times per transcript,
	// the default budget is 100 writes a minute, and every request past that
	// was reported to the user as an error rather than as backpressure. The
	// rate limiter exists to stop a runaway hook loop, not to stop the
	// documented import path.
	if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
		wait := retryAfter(resp)
		// Drained and closed before sleeping so the connection returns to the
		// pool; a retry loop that leaks a connection per attempt exhausts the
		// pool exactly when it is retrying hardest. The deferred Close above
		// runs too, and closing twice is harmless.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		select {
		case <-ctx.Done():
			return resp.StatusCode, ctx.Err()
		case <-time.After(wait):
		}
		return c.doAttempt(ctx, method, path, body, out, attempt+1)
	}

	if resp.StatusCode >= 400 {
		return resp.StatusCode, parseAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

func parseAPIError(resp *http.Response) error {
	var apiErr APIError
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &apiErr); err != nil || apiErr.Code == "" {
		// Every dkm response has a JSON body, so a non-JSON error means
		// something in front of the server answered -- a proxy, a tunnel, a
		// login page. Saying so is more useful than printing the HTML.
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return &APIError{
			Status:  resp.StatusCode,
			Code:    "unexpected_response",
			Message: fmt.Sprintf("%s returned a non-JSON body, so it probably did not come from dkm: %s", resp.Status, snippet),
			Hint:    "check that the server URL points at dkm and not at a proxy error page",
		}
	}
	apiErr.Status = resp.StatusCode
	return &apiErr
}

func unwrapNetError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// IsOffline reports whether an error means the server was unreachable.
func IsOffline(err error) bool { return errors.Is(err, ErrOffline) }

// IsUnauthorized reports whether an error is a 401.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// --- health ----------------------------------------------------------------

// Health is the subset of /v1/healthz the client displays.
type Health struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Uptime   string `json:"uptime"`
	Database struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	} `json:"database"`
	Embedder struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
		Error  string `json:"error,omitempty"`
	} `json:"embedder"`
	Worker struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
	} `json:"worker"`
	Stats  *store.Stats `json:"stats,omitempty"`
	Caller struct {
		User    string `json:"user"`
		Name    string `json:"name"`
		Team    string `json:"team"`
		IsAdmin bool   `json:"is_admin"`
	} `json:"caller"`
}

// Health fetches the server's deep health report.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	if err := c.do(ctx, http.MethodGet, "/v1/healthz", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// --- memories --------------------------------------------------------------

// SaveResult reports where a memory ended up.
//
// Queued means the server was unreachable and the write is waiting locally; the
// ID is already final, so the eventual flush cannot produce a second row.
type SaveResult struct {
	Memory  *store.Memory
	Queued  bool
	Created bool
}

// Save writes a memory.
func (c *Client) Save(ctx context.Context, in store.MemoryInput) (*SaveResult, error) {
	var mem store.Memory
	status, err := c.doStatus(ctx, http.MethodPost, "/v1/memories", in, &mem)
	if err == nil {
		// 201 created, 200 already existed. Reporting the difference is what
		// makes "imported 0, skipped 7" possible, and that line is the only
		// visible evidence that dedup is working.
		return &SaveResult{Memory: &mem, Created: status == http.StatusCreated}, nil
	}
	if !IsOffline(err) || c.mirror == nil {
		return nil, err
	}

	queued, qErr := c.mirror.Enqueue(QueuedWrite{
		ID: in.ID, Kind: in.Kind, Title: in.Title, Body: in.Body,
		Project: in.Project, Files: in.Files, Visibility: in.Visibility,
	})
	if qErr != nil {
		return nil, fmt.Errorf("server unreachable and the write could not be queued: %w", qErr)
	}
	return &SaveResult{
		Queued: true,
		Memory: &store.Memory{
			ID: queued.ID, Kind: queued.Kind, Title: queued.Title, Body: queued.Body,
			Project: queued.Project, Visibility: queued.Visibility, CreatedAt: queued.QueuedAt,
		},
	}, nil
}

// SearchResults carries results plus how they were obtained.
type SearchResults struct {
	Results []store.SearchResult `json:"results"`
	Count   int                  `json:"count"`
	Mode    string               `json:"mode"`
	// Local is true when the results came from the mirror because the server
	// was unreachable. Surfaced so a user is never quietly shown a stale,
	// keyword-only answer while believing it came from the server.
	Local bool `json:"local"`
}

// Search queries the server, falling back to the mirror when offline.
func (c *Client) Search(ctx context.Context, query, project string, kinds []string, limit int) (*SearchResults, error) {
	var out SearchResults
	err := c.do(ctx, http.MethodPost, "/v1/search", map[string]any{
		"query": query, "project": project, "kinds": kinds, "limit": limit,
	}, &out)
	if err == nil {
		return &out, nil
	}
	if !IsOffline(err) || c.mirror == nil {
		return nil, err
	}

	local, lerr := c.mirror.Search(query, project, limit)
	if lerr != nil {
		return nil, err
	}
	return &SearchResults{Results: local, Count: len(local), Mode: "keyword", Local: true}, nil
}

// Lessons lists lessons, falling back to the mirror.
func (c *Client) Lessons(ctx context.Context, project string, limit int) ([]store.Memory, bool, error) {
	var out struct {
		Lessons []store.Memory `json:"lessons"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/lessons?"+q("project", project, "limit", itoa(limit)), nil, &out)
	if err == nil {
		return out.Lessons, false, nil
	}
	if !IsOffline(err) || c.mirror == nil {
		return nil, false, err
	}

	mems, lerr := c.mirror.Memories()
	if lerr != nil {
		return nil, false, err
	}
	var lessons []store.Memory
	for _, m := range mems {
		if m.Kind == store.KindLesson && m.DeletedAt == nil && m.SupersededBy == nil {
			if project == "" || m.Project == project || m.Project == "" {
				lessons = append(lessons, m)
			}
		}
	}
	return lessons, true, nil
}

// SaveLesson stores a lesson.
func (c *Client) SaveLesson(ctx context.Context, lesson, body, project string, files []string, visibility string) (*SaveResult, error) {
	return c.Save(ctx, store.MemoryInput{
		Kind: store.KindLesson, Title: lesson,
		Body:       firstNonEmpty(body, lesson),
		Project:    project,
		Files:      files,
		Visibility: visibility,
	})
}

// Share promotes a memory to team visibility.
func (c *Client) Share(ctx context.Context, id string) (*store.Memory, error) {
	var mem store.Memory
	if err := c.do(ctx, http.MethodPost, "/v1/share/"+url.PathEscape(id), map[string]any{}, &mem); err != nil {
		return nil, err
	}
	return &mem, nil
}

// Feed lists what teammates shared recently.
func (c *Client) Feed(ctx context.Context, project string, limit int) ([]store.Memory, error) {
	var out struct {
		Memories []store.Memory `json:"memories"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/feed?"+q("project", project, "limit", itoa(limit)), nil, &out)
	return out.Memories, err
}

// Memories lists memories.
func (c *Client) Memories(ctx context.Context, project string, kinds []string, limit int, cursor string) ([]store.Memory, string, error) {
	var out struct {
		Memories   []store.Memory `json:"memories"`
		NextCursor string         `json:"next_cursor"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/memories?"+q(
		"project", project, "kind", strings.Join(kinds, ","), "limit", itoa(limit), "cursor", cursor,
	), nil, &out)
	return out.Memories, out.NextCursor, err
}

// Forget deletes a memory.
func (c *Client) Forget(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/memories/"+url.PathEscape(id), nil, nil)
}

// Supersede marks a memory replaced.
func (c *Client) Supersede(ctx context.Context, oldID, newID string) error {
	return c.do(ctx, http.MethodPost, "/v1/memories/"+url.PathEscape(oldID)+"/supersede",
		map[string]any{"new_id": newID}, nil)
}

// Reinforce raises a memory's strength.
func (c *Client) Reinforce(ctx context.Context, id string) (*store.Memory, error) {
	var mem store.Memory
	err := c.do(ctx, http.MethodPost, "/v1/memories/"+url.PathEscape(id)+"/reinforce", map[string]any{}, &mem)
	return &mem, err
}

// Context fetches the project briefing.
func (c *Client) Context(ctx context.Context, project string, budget int) (*store.ContextPayload, error) {
	var out store.ContextPayload
	err := c.do(ctx, http.MethodPost, "/v1/context",
		map[string]any{"project": project, "budget_tokens": budget}, &out)
	return &out, err
}

// Projects lists visible projects.
func (c *Client) Projects(ctx context.Context) ([]store.ProjectSummary, error) {
	var out struct {
		Projects []store.ProjectSummary `json:"projects"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/projects", nil, &out)
	return out.Projects, err
}

// Graph fetches the project graph.
func (c *Client) Graph(ctx context.Context, project, node string, depth int) (*store.Graph, error) {
	var g store.Graph
	err := c.do(ctx, http.MethodGet, "/v1/graph?"+q(
		"project", project, "node", node, "depth", itoa(depth)), nil, &g)
	return &g, err
}

// --- sessions --------------------------------------------------------------

// CreateSession opens a session.
func (c *Client) CreateSession(ctx context.Context, project, agent string, meta map[string]any) (*store.Session, error) {
	var sess store.Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions",
		map[string]any{"project": project, "agent": agent, "meta": meta}, &sess)
	return &sess, err
}

// EndSession closes a session.
func (c *Client) EndSession(ctx context.Context, id string, summary string) error {
	body := map[string]any{}
	if summary != "" {
		body["summary"] = summary
	}
	return c.do(ctx, http.MethodPatch, "/v1/sessions/"+url.PathEscape(id), body, nil)
}

// Sessions lists recent sessions.
func (c *Client) Sessions(ctx context.Context, project string, limit int) ([]store.Session, error) {
	var out struct {
		Sessions []store.Session `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/sessions?"+q("project", project, "limit", itoa(limit)), nil, &out)
	return out.Sessions, err
}

// AddObservations sends a batch of observations.
func (c *Client) AddObservations(ctx context.Context, sessionID string, items []store.ObservationInput) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/observations",
		map[string]any{"session_id": sessionID, "items": items}, &out)
	return out.Count, err
}

// --- sync ------------------------------------------------------------------

// SyncResult reports one sync pass.
type SyncResult struct {
	Pulled  int
	Pushed  int
	Failed  int
	Queued  int
	Cursor  string
	Skipped bool
}

// Sync pushes queued writes, then pulls changes into the mirror.
//
// Push before pull, so a memory written offline is on the server before the
// pull that would otherwise not contain it -- and so the mirror ends the pass
// consistent with the server rather than one round behind.
func (c *Client) Sync(ctx context.Context) (*SyncResult, error) {
	res := &SyncResult{}
	if c.mirror == nil {
		res.Skipped = true
		return res, nil
	}

	pushed, failed, err := c.Push(ctx)
	res.Pushed, res.Failed = pushed, failed
	if err != nil {
		return res, err
	}

	pulled, cursor, err := c.Pull(ctx)
	res.Pulled, res.Cursor = pulled, cursor
	if err != nil {
		return res, err
	}

	if queue, qErr := c.mirror.Queue(); qErr == nil {
		res.Queued = len(queue)
	}
	return res, nil
}

// Push flushes the offline write queue.
//
// Each queued write carries the ULID generated when it was made, and the server
// upserts on that primary key. Flushing the same queue twice therefore produces
// the same rows, not twice as many -- which is what makes an interrupted push
// safe to simply repeat.
func (c *Client) Push(ctx context.Context) (pushed, failed int, err error) {
	if c.mirror == nil {
		return 0, 0, nil
	}
	queue, err := c.mirror.Queue()
	if err != nil || len(queue) == 0 {
		return 0, 0, err
	}

	var done []string
	for _, w := range queue {
		in := store.MemoryInput{
			ID: w.ID, Kind: w.Kind, Title: w.Title, Body: w.Body,
			Project: w.Project, Files: w.Files, Visibility: w.Visibility,
		}
		var mem store.Memory
		if err := c.do(ctx, http.MethodPost, "/v1/memories", in, &mem); err != nil {
			if IsOffline(err) {
				// Still offline. Stop rather than burn the rest of the queue
				// against a server that is not there.
				return pushed, failed, err
			}
			failed++
			continue
		}
		pushed++
		done = append(done, w.ID)
	}

	if err := c.mirror.Dequeue(done); err != nil {
		return pushed, failed, err
	}
	return pushed, failed, nil
}

// Pull fetches changes into the mirror.
func (c *Client) Pull(ctx context.Context) (int, string, error) {
	if c.mirror == nil {
		return 0, "", nil
	}

	cursor := c.mirror.Cursor()
	total := 0

	for {
		var out struct {
			Changes []store.Change `json:"changes"`
			Cursor  string         `json:"cursor"`
			HasMore bool           `json:"has_more"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/sync?"+q("since", cursor, "limit", "200"), nil, &out); err != nil {
			return total, cursor, err
		}
		if len(out.Changes) == 0 {
			break
		}
		if err := c.mirror.Apply(out.Changes); err != nil {
			return total, cursor, err
		}
		total += len(out.Changes)
		cursor = out.Cursor
		if err := c.mirror.SetCursor(cursor); err != nil {
			return total, cursor, err
		}
		if !out.HasMore {
			break
		}
	}
	return total, cursor, nil
}

// --- transfer --------------------------------------------------------------

// Export streams the corpus as NDJSON into w.
func (c *Client) Export(ctx context.Context, scope, project string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.Server+"/v1/export?"+q("scope", scope, "project", project), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := c.http.Do(req)
	if err != nil {
		c.offline = true
		return 0, fmt.Errorf("%w: %v", ErrOffline, unwrapNetError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, parseAPIError(resp)
	}
	return io.Copy(w, resp.Body)
}

// ImportResult reports what an import did.
type ImportResult struct {
	Lines     int      `json:"lines"`
	Imported  int      `json:"imported"`
	Duplicate int      `json:"duplicate"`
	Skipped   int      `json:"skipped"`
	Failures  []string `json:"failures"`
}

// Import streams NDJSON to the server.
func (c *Client) Import(ctx context.Context, body io.Reader) (*ImportResult, error) {
	return c.streamNDJSON(ctx, "/v1/import", body, &ImportResult{})
}

func (c *Client) streamNDJSON(ctx context.Context, path string, body io.Reader, out *ImportResult) (*ImportResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Server+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", version.UserAgent())

	// No timeout: an import of years of history legitimately takes minutes, and
	// a client-side deadline mid-stream leaves a partial import with no report.
	httpClient := *c.http
	httpClient.Timeout = 0

	resp, err := httpClient.Do(req)
	if err != nil {
		c.offline = true
		return nil, fmt.Errorf("%w: %v", ErrOffline, unwrapNetError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ImportPreview runs a dry run and returns the server's report as raw JSON, so
// the CLI can render it without this package importing the API's types.
func (c *Client) ImportPreview(ctx context.Context, body io.Reader) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Server+"/v1/import/preview", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", version.UserAgent())

	httpClient := *c.http
	httpClient.Timeout = 0

	resp, err := httpClient.Do(req)
	if err != nil {
		c.offline = true
		return nil, fmt.Errorf("%w: %v", ErrOffline, unwrapNetError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp)
	}
	var out map[string]any
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// --- admin -----------------------------------------------------------------

// Admin issues an admin request and decodes the result.
func (c *Client) Admin(ctx context.Context, method, path string, body any, out any) error {
	return c.do(ctx, method, path, body, out)
}

// AdminLong is Admin without the client-side deadline.
//
// The default 30-second timeout is right for an API that answers from Postgres.
// It is wrong for consolidation, whose duration is set by an LLM provider: the
// server finishes the work and writes the results, while the client gives up
// waiting and reports "server unreachable" — then advises falling back to the
// mirror, which is advice for a completely different situation. The operation
// succeeded and the operator was told it failed.
//
// Cancellation comes from ctx instead, which is what Ctrl-C already uses.
func (c *Client) AdminLong(ctx context.Context, method, path string, body, out any) error {
	saved := c.http.Timeout
	c.http.Timeout = 0
	defer func() { c.http.Timeout = saved }()
	return c.do(ctx, method, path, body, out)
}

// --- helpers ---------------------------------------------------------------

// q builds a query string from alternating key/value pairs, dropping empties.
func q(pairs ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" && pairs[i+1] != "0" {
			v.Set(pairs[i], pairs[i+1])
		}
	}
	return v.Encode()
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
