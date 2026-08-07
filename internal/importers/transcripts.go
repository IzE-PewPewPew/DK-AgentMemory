// Package importers reads existing agent history into dkm.
//
// Importing is where a new install stops being empty. The value is not the raw
// transcripts -- it is that the consolidation pipeline can run over years of
// history the same evening you install, so `dkm lesson list` has something in
// it on day one rather than in a month.
//
// Everything here is dry-run first. People are importing history that may
// contain credentials they happened to read during a session, and finding that
// out afterwards is not recoverable.
package importers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// Transcript is one parsed session file.
type Transcript struct {
	Path      string
	Agent     string
	SessionID string
	CWD       string

	Project       string
	ProjectSource client.Source
	ProjectWarn   string

	StartedAt time.Time
	EndedAt   time.Time

	Observations []store.ObservationInput
	Secrets      []SecretHit
}

// SecretHit is a credential found in a transcript, located by file and line and
// never carrying the value.
type SecretHit struct {
	File string      `json:"file"`
	Line int         `json:"line"`
	Kind redact.Kind `json:"kind"`
}

// Preview summarises what an import would do.
type Preview struct {
	Transcripts  []Transcript
	Files        int
	Observations int
	Skipped      int
	ByProject    map[string]int
	Secrets      []SecretHit
	SecretCounts map[redact.Kind]int
	Warnings     []string
}

// DefaultClaudeCodeRoot is where Claude Code keeps transcripts.
func DefaultClaudeCodeRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// DefaultCodexRoot is where Codex CLI keeps session rollouts.
func DefaultCodexRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// Progress reports scanning advancement. Called on the scanning goroutine, so
// implementations must not block.
type Progress func(scanned, total int, current string)

// ScanJSONL walks a directory of .jsonl transcripts and parses each one.
//
// Project identity is resolved per transcript from the working directory the
// session recorded, not from the directory name Claude Code encodes in its
// folder names. Those folder names are path-derived
// (-Users-me-dev-api), so grouping by them would reproduce exactly the
// path-based identity that stops one person's memories reaching a teammate.
//
// progress may be nil. It exists because scanning a real history is not fast:
// several hundred megabytes, with fifteen credential patterns run over every
// observation, takes tens of minutes. A command that prints nothing for half an
// hour has not communicated "working" — it has communicated "hung", and the
// reasonable response to that is Ctrl-C.
func ScanJSONL(root, agent string, progress Progress) (*Preview, error) {
	if root == "" {
		return nil, fmt.Errorf("no transcript directory given")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root, err)
	}

	var paths []string
	if !info.IsDir() {
		paths = []string{root}
	} else {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // an unreadable subdirectory should not abort the walk
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)

	preview := &Preview{
		ByProject:    map[string]int{},
		SecretCounts: map[redact.Kind]int{},
	}

	// Project resolution shells out to git, so cache per working directory: a
	// hundred transcripts from one repo should not mean a hundred git calls.
	projectCache := map[string]client.Project{}

	for i, path := range paths {
		if progress != nil {
			progress(i+1, len(paths), filepath.Base(path))
		}

		t, err := parseJSONL(path, agent, projectCache)
		if err != nil {
			preview.Skipped++
			preview.Warnings = append(preview.Warnings,
				fmt.Sprintf("%s: %v", filepath.Base(path), err))
			continue
		}
		if len(t.Observations) == 0 {
			preview.Skipped++
			continue
		}

		preview.Files++
		preview.Observations += len(t.Observations)
		preview.ByProject[t.Project] += len(t.Observations)
		preview.Secrets = append(preview.Secrets, t.Secrets...)
		for _, s := range t.Secrets {
			preview.SecretCounts[s.Kind]++
		}
		preview.Transcripts = append(preview.Transcripts, *t)
	}

	if n := preview.ByProject[""]; n > 0 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"%d observations have no resolvable project and will be team-global", n))
	}
	for _, t := range preview.Transcripts {
		if t.ProjectWarn != "" {
			preview.Warnings = append(preview.Warnings,
				fmt.Sprintf("%s: %s", filepath.Base(t.Path), t.ProjectWarn))
			break // one example is enough; the same warning repeats per file
		}
	}

	return preview, nil
}

// jsonlEntry is the union of the fields the supported transcript formats use.
//
// Deliberately permissive. These formats are not specified anywhere, they
// change between releases, and an importer that fails on an unknown field is an
// importer that stops working after the host updates.
type jsonlEntry struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Timestamp string          `json:"timestamp"`
	CWD       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
	Message   json.RawMessage `json:"message"`
	Summary   string          `json:"summary"`
	Content   json.RawMessage `json:"content"`
	// Codex rollout files nest the interesting part under `payload`.
	Payload json.RawMessage `json:"payload"`
}

func parseJSONL(path, agent string, cache map[string]client.Project) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), 16<<20)

	t := &Transcript{Path: path, Agent: agent}
	lineNo := 0

	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}

		var e jsonlEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue // one malformed line should not lose the transcript
		}
		if len(e.Payload) > 0 {
			var inner jsonlEntry
			if err := json.Unmarshal(e.Payload, &inner); err == nil {
				if inner.Type != "" {
					e.Type = inner.Type
				}
				if inner.Role != "" {
					e.Role = inner.Role
				}
				if len(inner.Content) > 0 {
					e.Content = inner.Content
				}
				if len(inner.Message) > 0 {
					e.Message = inner.Message
				}
				if inner.CWD != "" {
					e.CWD = inner.CWD
				}
			}
		}

		if e.CWD != "" && t.CWD == "" {
			t.CWD = e.CWD
		}
		if e.SessionID != "" && t.SessionID == "" {
			t.SessionID = e.SessionID
		}
		if ts, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			if t.StartedAt.IsZero() || ts.Before(t.StartedAt) {
				t.StartedAt = ts
			}
			if ts.After(t.EndedAt) {
				t.EndedAt = ts
			}
		}

		kind, text, files := renderEntry(&e)
		if strings.TrimSpace(text) == "" {
			continue
		}
		text = collapse(text, 4000)

		for _, hit := range redact.Scan(text) {
			t.Secrets = append(t.Secrets, SecretHit{File: path, Line: lineNo, Kind: hit.Kind})
		}

		t.Observations = append(t.Observations, store.ObservationInput{
			Kind: kind, Content: text, Files: files,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	dir := t.CWD
	if dir == "" {
		dir = filepath.Dir(path)
	}
	if cached, ok := cache[dir]; ok {
		t.Project, t.ProjectSource, t.ProjectWarn = cached.ID, cached.Source, cached.Warning
	} else {
		p := client.ResolveProject(dir)
		cache[dir] = p
		t.Project, t.ProjectSource, t.ProjectWarn = p.ID, p.Source, p.Warning
	}

	// A transcript whose recorded cwd no longer exists resolves to a directory
	// name that means nothing. Better to import it unscoped than to attach it
	// to a project it is not from.
	if t.CWD != "" {
		if _, err := os.Stat(t.CWD); err != nil {
			t.Project = ""
			t.ProjectWarn = "the recorded working directory no longer exists, so this transcript has no project"
		}
	}

	return t, nil
}

// renderEntry converts one transcript line to an observation.
func renderEntry(e *jsonlEntry) (kind, text string, files []string) {
	if e.Summary != "" {
		return "summary", e.Summary, nil
	}

	role := e.Role
	body := e.Content
	if len(e.Message) > 0 {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(e.Message, &msg); err == nil {
			if msg.Role != "" {
				role = msg.Role
			}
			if len(msg.Content) > 0 {
				body = msg.Content
			}
		}
	}
	if role == "" {
		role = e.Type
	}

	switch role {
	case "user", "human":
		kind = "prompt"
	case "assistant", "model":
		kind = "response"
	default:
		kind = "note"
	}

	text, files = renderContent(body)
	return kind, text, files
}

// renderContent flattens the content field, which is a string in some formats
// and an array of typed blocks in others.
func renderContent(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}

	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil
	}

	var b strings.Builder
	var files []string
	seen := map[string]bool{}

	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
			b.WriteString("\n")
		case "tool_use":
			// Tool calls are where the file information is, and where the
			// commands that turn out to matter later live.
			fmt.Fprintf(&b, "[tool %s]", blk.Name)
			var input map[string]any
			if err := json.Unmarshal(blk.Input, &input); err == nil {
				for _, key := range []string{"file_path", "path", "notebook_path", "filePath"} {
					if v, ok := input[key].(string); ok && v != "" && !seen[v] {
						seen[v] = true
						files = append(files, relativise(v))
					}
				}
				if cmd, ok := input["command"].(string); ok && cmd != "" {
					fmt.Fprintf(&b, " %s", collapse(cmd, 400))
				}
				if desc, ok := input["description"].(string); ok && desc != "" {
					fmt.Fprintf(&b, " — %s", desc)
				}
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String()), files
}

// relativise trims an absolute path to something comparable across machines.
//
// /home/alice/dev/api/src/auth.ts and C:\work\api\src\auth.ts are the same file
// to two teammates, and storing the absolute form makes file-based graph edges
// and search filters machine-specific.
func relativise(p string) string {
	p = filepath.ToSlash(p)
	root := client.ResolveProject(filepath.Dir(p)).Root
	if root != "" {
		if rel, err := filepath.Rel(root, filepath.FromSlash(p)); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	// No repository root: keep the last few segments, which is still more
	// portable than a full home-directory path.
	parts := strings.Split(p, "/")
	if len(parts) > 3 {
		return strings.Join(parts[len(parts)-3:], "/")
	}
	return p
}

func collapse(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
