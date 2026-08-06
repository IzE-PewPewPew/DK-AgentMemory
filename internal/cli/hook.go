package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// Hooks are the only part of this system that runs inside someone else's
// process, on their critical path, on every prompt and every tool call.
//
// Three rules follow from that, and they override everything else here:
//
//  1. Bounded. Two seconds, enforced by a context deadline the whole command
//     runs under.
//  2. Silent on failure. Every error path exits 0. A memory system that breaks
//     the user's editor gets uninstalled the first time it happens, and it will
//     not get a second chance to be useful.
//  3. Batched. Observations accumulate in a local file and are flushed in
//     groups. One HTTP call per tool use would put network latency between the
//     agent and every edit it makes.

const hookBudget = 2 * time.Second

// hookFlushAt is how many buffered observations trigger a send.
const hookFlushAt = 20

// hookPayload is the JSON an agent host writes to the hook's stdin.
//
// Field names differ between hosts and between versions of the same host, so
// several spellings are accepted for each. A hook that stops working after the
// host updates is worse than one that never worked, because nobody notices.
type hookPayload struct {
	SessionID     string          `json:"session_id"`
	SessionIDAlt  string          `json:"sessionId"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	Prompt        string          `json:"prompt"`
	ToolName      string          `json:"tool_name"`
	ToolNameAlt   string          `json:"toolName"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolInputAlt  json.RawMessage `json:"toolInput"`
	Source        string          `json:"source"`
	Reason        string          `json:"reason"`
}

func (p *hookPayload) sessionID() string {
	if p.SessionID != "" {
		return p.SessionID
	}
	return p.SessionIDAlt
}

func (p *hookPayload) toolName() string {
	if p.ToolName != "" {
		return p.ToolName
	}
	return p.ToolNameAlt
}

func (p *hookPayload) toolInput() json.RawMessage {
	if len(p.ToolInput) > 0 {
		return p.ToolInput
	}
	return p.ToolInputAlt
}

// hookState maps a host's session to the dkm session it opened.
type hookState struct {
	DKMSession string `json:"dkm_session"`
	Project    string `json:"project"`
	Agent      string `json:"agent"`
	StartedAt  string `json:"started_at"`
}

func cmdHook(ctx context.Context, args []string) int {
	// Whatever happens below, this command exits 0. The host is waiting.
	defer func() { _ = recover() }()

	if len(args) == 0 {
		return 0
	}

	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()

	payload := readHookPayload()
	event := args[0]

	switch event {
	case "session-start":
		runHook(ctx, func(c *client.Client) { hookSessionStart(ctx, c, payload) })
	case "prompt":
		runHook(ctx, func(c *client.Client) { hookPrompt(ctx, c, payload) })
	case "tool":
		hookTool(payload)
		runHook(ctx, func(c *client.Client) { flushBuffer(ctx, c, payload, false) })
	case "session-end":
		runHook(ctx, func(c *client.Client) { hookSessionEnd(ctx, c, payload) })
	}
	return 0
}

// runHook builds a client and runs fn, swallowing everything.
func runHook(ctx context.Context, fn func(*client.Client)) {
	defer func() { _ = recover() }()

	// Warnings must not reach stdout: for SessionStart and UserPromptSubmit,
	// stdout is a structured channel the host parses.
	client.SetWarningOutput(io.Discard)

	c, err := client.New()
	if err != nil {
		hookLog("client unavailable: %v", err)
		return
	}
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(done) }()
		fn(c)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// Out of budget. Whatever was buffered stays on disk and goes out with
		// the next hook.
		hookLog("hook exceeded its %s budget", hookBudget)
	}
}

func readHookPayload() *hookPayload {
	p := &hookPayload{}

	// Read with a deadline of its own: a host that opens stdin and writes
	// nothing must not hold the hook for the whole budget.
	type result struct{ data []byte }
	ch := make(chan result, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		ch <- result{data}
	}()

	select {
	case r := <-ch:
		if len(r.data) > 0 {
			_ = json.Unmarshal(r.data, p)
		}
	case <-time.After(300 * time.Millisecond):
	}

	if p.CWD == "" {
		p.CWD, _ = os.Getwd()
	}
	return p
}

// --- events ----------------------------------------------------------------

func hookSessionStart(ctx context.Context, c *client.Client, p *hookPayload) {
	project := c.Project(p.CWD)

	sess, err := c.CreateSession(ctx, project.ID, "claude-code", map[string]any{
		"host_session_id": p.sessionID(),
		"cwd":             p.CWD,
		"source":          p.Source,
	})
	if err != nil {
		hookLog("could not open a session: %v", err)
	} else {
		writeHookState(p.sessionID(), hookState{
			DKMSession: sess.ID,
			Project:    project.ID,
			Agent:      "claude-code",
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		})
	}

	payload, err := c.Context(ctx, project.ID, 0)
	if err != nil || payload.Text == "" {
		return
	}
	emitContext("SessionStart", "Project memory for "+project.ID+":\n\n"+payload.Text)
}

func hookPrompt(ctx context.Context, c *client.Client, p *hookPayload) {
	if strings.TrimSpace(p.Prompt) == "" {
		return
	}

	state := readHookState(p.sessionID())
	bufferObservation(p.sessionID(), store.ObservationInput{
		Kind: "prompt", Content: truncate(p.Prompt, 4000),
	})

	project := state.Project
	if project == "" {
		project = c.Project(p.CWD).ID
	}

	res, err := c.Search(ctx, p.Prompt, project, nil, 4)
	if err != nil || len(res.Results) == 0 {
		return
	}

	// A relevance floor, so an unrelated prompt does not get four memories
	// stapled to it. Injecting weak matches trains the user to ignore the
	// injected block, which costs more than the occasional missed hit.
	const floor = 0.004
	var lines []string
	for _, r := range res.Results {
		if r.Score < floor {
			continue
		}
		line := "- " + r.Title
		if r.Body != "" && r.Body != r.Title {
			line += ": " + truncate(r.Body, 300)
		}
		lines = append(lines, line+"  (id: "+r.ID+")")
	}
	if len(lines) == 0 {
		return
	}

	emitContext("UserPromptSubmit",
		"Relevant stored memory:\n"+strings.Join(lines, "\n")+
			"\n\nIf one of these answered the question, call memory_reinforce with its id.")
}

func hookTool(p *hookPayload) {
	name := p.toolName()
	if name == "" {
		return
	}

	var input map[string]any
	_ = json.Unmarshal(p.toolInput(), &input)

	var files []string
	for _, key := range []string{"file_path", "path", "notebook_path", "filePath"} {
		if v, ok := input[key].(string); ok && v != "" {
			files = append(files, relativisePath(v, p.CWD))
		}
	}

	content := "[tool " + name + "]"
	if cmd, ok := input["command"].(string); ok && cmd != "" {
		content += " " + truncate(cmd, 500)
	}
	if desc, ok := input["description"].(string); ok && desc != "" {
		content += " — " + truncate(desc, 200)
	}
	if len(files) > 0 {
		content += " " + strings.Join(files, " ")
	}

	kind := "tool"
	switch name {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		kind = "edit"
	case "Bash", "BashOutput":
		kind = "command"
	}

	bufferObservation(p.sessionID(), store.ObservationInput{
		Kind: kind, Content: content, Files: files,
	})
}

func hookSessionEnd(ctx context.Context, c *client.Client, p *hookPayload) {
	flushBuffer(ctx, c, p, true)

	state := readHookState(p.sessionID())
	if state.DKMSession == "" {
		return
	}
	if err := c.EndSession(ctx, state.DKMSession, ""); err != nil {
		hookLog("could not close the session: %v", err)
		return
	}
	clearHookState(p.sessionID())
}

// --- buffering -------------------------------------------------------------

func hookDir() string {
	dir := filepath.Join(config.Home(), "hooks")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func safeName(sessionID string) string {
	if sessionID == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func bufferObservation(sessionID string, obs store.ObservationInput) {
	path := filepath.Join(hookDir(), safeName(sessionID)+".ndjson")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(obs)
}

// flushBuffer sends buffered observations, either when enough have accumulated
// or unconditionally at session end.
func flushBuffer(ctx context.Context, c *client.Client, p *hookPayload, force bool) {
	path := filepath.Join(hookDir(), safeName(p.sessionID())+".ndjson")

	f, err := os.Open(path)
	if err != nil {
		return
	}
	var items []store.ObservationInput
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var obs store.ObservationInput
		if err := json.Unmarshal(sc.Bytes(), &obs); err == nil {
			items = append(items, obs)
		}
	}
	// Closed explicitly rather than deferred: the file is removed further down
	// once its contents have been accepted, and on Windows a removal cannot
	// happen while a handle is open.
	_ = f.Close()

	if len(items) == 0 || (!force && len(items) < hookFlushAt) {
		return
	}

	state := readHookState(p.sessionID())
	if state.DKMSession == "" {
		// SessionStart never completed -- the server may have been down. Open a
		// session now rather than discarding everything captured since.
		sess, err := c.CreateSession(ctx, c.Project(p.CWD).ID, "claude-code",
			map[string]any{"host_session_id": p.sessionID(), "recovered": true})
		if err != nil {
			return
		}
		state.DKMSession = sess.ID
		state.Project = sess.Project
		writeHookState(p.sessionID(), state)
	}

	if _, err := c.AddObservations(ctx, state.DKMSession, items); err != nil {
		// Left on disk for the next flush. Losing observations is acceptable
		// only when the alternative is losing the user's patience.
		hookLog("could not send %d observations: %v", len(items), err)
		return
	}
	_ = os.Remove(path)
}

// --- state -----------------------------------------------------------------

func stateFile(sessionID string) string {
	return filepath.Join(hookDir(), safeName(sessionID)+".json")
}

func writeHookState(sessionID string, st hookState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(stateFile(sessionID), data, 0o600)
}

func readHookState(sessionID string) hookState {
	var st hookState
	data, err := os.ReadFile(stateFile(sessionID))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

func clearHookState(sessionID string) {
	_ = os.Remove(stateFile(sessionID))
	_ = os.Remove(filepath.Join(hookDir(), safeName(sessionID)+".ndjson"))
}

// --- output ----------------------------------------------------------------

// emitContext writes the structured block a host reads from a hook's stdout to
// inject text into the model's context.
func emitContext(event, text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": text,
		},
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(data))
}

// hookLog appends to a local log. Hooks never write diagnostics to stdout or
// stderr: one is a protocol channel and the other ends up in the user's
// terminal in the middle of their work.
func hookLog(format string, a ...any) {
	path := filepath.Join(config.Home(), "hooks.log")

	// Size-check before opening. Rotating an already-open handle means closing
	// and reopening inside the same function, which is how the earlier version
	// of this ended up closing one file twice.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		_ = os.Remove(path)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

func relativisePath(p, cwd string) string {
	if cwd == "" {
		return filepath.ToSlash(p)
	}
	if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}
