package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// --- sync ------------------------------------------------------------------

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) error {
	changes, next, err := s.store.Changes(r.Context(), identityFrom(r.Context()),
		r.URL.Query().Get("since"), queryInt(r, "limit", 200))
	if err != nil {
		if strings.Contains(err.Error(), "cursor:") {
			return badRequest("%v", err)
		}
		return fromStore(err, "sync")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"changes": changes,
		"count":   len(changes),
		"cursor":  next,
		// has_more lets a client loop until drained without guessing from the
		// page size, which is wrong the moment the limit changes.
		"has_more": len(changes) == queryInt(r, "limit", 200),
	})
	return nil
}

// --- export ----------------------------------------------------------------

// TransferRecord is one line of the NDJSON transfer format.
//
// One record per line, streamed. A single JSON array would have to be built in
// memory on the writing side and parsed in memory on the reading side, and a
// real corpus exceeds both the proxy body limits in front of the server and the
// patience of whoever is waiting.
type TransferRecord struct {
	Type       string   `json:"type"`
	ID         string   `json:"id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Title      string   `json:"title,omitempty"`
	Body       string   `json:"body,omitempty"`
	Project    string   `json:"project,omitempty"`
	Files      []string `json:"files,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	Source     string   `json:"source,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "me"
	}
	if scope != "me" && scope != "team" {
		return badRequest("scope must be me or team")
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="dkm-export.ndjson"`)
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriterSize(w, 64<<10)
	// The status line has already been sent, so a failed flush cannot become an
	// error response. The client sees a short stream; nothing else is possible
	// at this point.
	defer func() { _ = bw.Flush() }()
	enc := json.NewEncoder(bw)

	flusher, _ := w.(http.Flusher)
	cursor := ""
	written := 0

	for {
		mems, next, err := s.store.ListMemories(r.Context(), id, store.ListFilter{
			Project:           r.URL.Query().Get("project"),
			MineOnly:          scope == "me",
			IncludeSuperseded: true,
			Limit:             500,
			Cursor:            cursor,
		})
		if err != nil {
			// The status line is already sent, so this cannot become a JSON
			// error response. Emit an error record so the reader sees a
			// truncated stream rather than a silently short one.
			_ = enc.Encode(map[string]string{"type": "error", "message": err.Error()})
			return nil
		}

		for _, m := range mems {
			if err := enc.Encode(TransferRecord{
				Type: "memory", ID: m.ID, Kind: m.Kind, Title: m.Title, Body: m.Body,
				Project: m.Project, Files: m.Files, Visibility: m.Visibility,
				Source: m.Source, CreatedAt: m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			}); err != nil {
				return nil
			}
			written++
		}

		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
		if next == "" {
			break
		}
		cursor = next
	}

	s.audit(r, id, store.ActionExport, scope, map[string]any{"records": written})
	return nil
}

// --- import ----------------------------------------------------------------

// ndjsonScanner returns a scanner sized for long lines.
//
// bufio.Scanner defaults to 64 KiB per line, and a memory body holding a
// stack trace or a config file exceeds that. Hitting the default produces
// "token too long" halfway through an import, which reads like corruption.
func ndjsonScanner(r *http.Request) *bufio.Scanner {
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 0, 256<<10), 8<<20)
	return sc
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	sc := ndjsonScanner(r)

	var (
		line      int
		imported  int
		duplicate int
		skipped   int
		failures  []string
	)

	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}

		var rec TransferRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			skipped++
			if len(failures) < 20 {
				failures = append(failures, fmt.Sprintf("line %d: malformed JSON", line))
			}
			continue
		}
		if rec.Type != "" && rec.Type != "memory" {
			skipped++
			continue
		}

		in := store.MemoryInput{
			Kind: rec.Kind, Title: rec.Title, Body: rec.Body, Project: rec.Project,
			Files: rec.Files, Visibility: rec.Visibility, Source: store.SourceImport,
		}
		// The incoming ID is deliberately not reused. Two servers can each hold
		// a memory with the same ULID from different corpora, and honouring a
		// foreign ID would make an import able to collide with, or silently
		// no-op against, an unrelated local row.
		in.Embedding = s.embedText(r.Context(), in.Title+"\n"+in.Body)

		_, created, err := s.store.CreateMemory(r.Context(), id, in)
		switch {
		case err != nil:
			skipped++
			if len(failures) < 20 {
				failures = append(failures, fmt.Sprintf("line %d: %v", line, err))
			}
		case created:
			imported++
		default:
			duplicate++
		}
	}
	if err := sc.Err(); err != nil {
		return badRequest("reading the NDJSON stream: %v", err)
	}

	s.audit(r, id, store.ActionImport, "ndjson", map[string]any{
		"imported": imported, "duplicate": duplicate, "skipped": skipped,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"lines":     line,
		"imported":  imported,
		"duplicate": duplicate,
		"skipped":   skipped,
		"failures":  failures,
	})
	return nil
}

// ImportPreview is the dry-run report.
//
// Dry run is the default posture for import because people import years of
// history that may contain credentials they happened to read during a session.
// Finding that out afterwards is not recoverable -- the value is in the
// database, its backups, and its replicas by then.
type ImportPreview struct {
	Records   int                 `json:"records"`
	Memories  int                 `json:"memories"`
	Malformed int                 `json:"malformed"`
	Projects  []ProjectGrouping   `json:"projects"`
	Secrets   []SecretFinding     `json:"secrets"`
	Summary   map[redact.Kind]int `json:"secret_summary,omitempty"`
	Warnings  []string            `json:"warnings,omitempty"`
}

// ProjectGrouping shows how records will be grouped once imported.
type ProjectGrouping struct {
	Project string `json:"project"`
	Records int    `json:"records"`
}

// SecretFinding locates a credential in the input. It carries a line number and
// a kind, never the value.
type SecretFinding struct {
	Line  int         `json:"line"`
	Field string      `json:"field"`
	Kind  redact.Kind `json:"kind"`
	// Project is included so a reader can tell which repo the credential came
	// from without opening the file.
	Project string `json:"project,omitempty"`
}

func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) error {
	sc := ndjsonScanner(r)

	preview := ImportPreview{Projects: []ProjectGrouping{}, Secrets: []SecretFinding{}}
	byProject := map[string]int{}
	var allFindings []redact.Finding
	line := 0

	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		preview.Records++

		var rec TransferRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			preview.Malformed++
			continue
		}
		if rec.Type != "" && rec.Type != "memory" {
			continue
		}
		preview.Memories++
		byProject[rec.Project]++

		for field, text := range map[string]string{"title": rec.Title, "body": rec.Body} {
			for _, f := range redact.Scan(text) {
				allFindings = append(allFindings, f)
				if len(preview.Secrets) < 500 {
					preview.Secrets = append(preview.Secrets, SecretFinding{
						Line: line, Field: field, Kind: f.Kind, Project: rec.Project,
					})
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return badRequest("reading the NDJSON stream: %v", err)
	}

	for project, n := range byProject {
		preview.Projects = append(preview.Projects, ProjectGrouping{Project: project, Records: n})
	}
	sort.Slice(preview.Projects, func(i, j int) bool {
		return preview.Projects[i].Records > preview.Projects[j].Records
	})
	preview.Summary = redact.Summary(allFindings)

	if n := byProject[""]; n > 0 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"%d records have no project and will be team-global; they will not match a teammate's project-scoped search", n))
	}
	if len(allFindings) > 0 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"%d credential-shaped strings found; they will be replaced with [redacted:kind] markers on import", len(allFindings)))
	}
	if len(preview.Secrets) == 500 {
		preview.Warnings = append(preview.Warnings,
			"the secret list is truncated at 500 entries; the summary counts are complete")
	}

	writeJSON(w, http.StatusOK, preview)
	return nil
}
