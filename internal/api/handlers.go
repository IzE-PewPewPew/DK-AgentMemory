package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/logx"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

func requestIDOf(r *http.Request) string { return logx.RequestID(r.Context()) }

// embedText produces a query or document vector with a short deadline.
//
// Two seconds, not twenty. This runs on the write and search paths, where the
// caller is an agent waiting on a response. Losing the vector costs semantic
// recall on one row; making the user wait costs them the whole feature.
func (s *Server) embedText(ctx context.Context, text string) []float32 {
	if s.embedder == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil {
		s.log.Debug("embedding unavailable, continuing without a vector", "error", err)
		return nil
	}
	if len(vecs) == 0 {
		return nil
	}
	return vecs[0]
}

// --- health ----------------------------------------------------------------

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// HealthReport is the body of /v1/healthz.
type HealthReport struct {
	OK       bool           `json:"ok"`
	Version  string         `json:"version"`
	Uptime   string         `json:"uptime"`
	Database ComponentState `json:"database"`
	Embedder ComponentState `json:"embedder"`
	Worker   ComponentState `json:"worker"`
	Stats    *store.Stats   `json:"stats,omitempty"`
	// Caller echoes who the presented key resolved to. `dkm doctor` needs it,
	// and answering "which account am I actually using" without a second
	// endpoint saves a round trip on every health check.
	Caller CallerInfo `json:"caller"`
}

// CallerInfo identifies the authenticated caller.
type CallerInfo struct {
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Team    string `json:"team"`
	IsAdmin bool   `json:"is_admin"`
}

// ComponentState reports one dependency.
type ComponentState struct {
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
	Latency string `json:"latency,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	report := HealthReport{
		OK:      true,
		Version: version.Short(),
		Uptime:  time.Since(s.startedAt).Truncate(time.Second).String(),
		Caller:  CallerInfo{User: id.UserID, Name: id.UserName, Team: id.TeamID, IsAdmin: id.IsAdmin},
	}

	start := time.Now()
	if err := s.store.Ping(r.Context()); err != nil {
		report.Database = ComponentState{OK: false, Error: err.Error()}
		report.OK = false
	} else {
		report.Database = ComponentState{OK: true, Latency: time.Since(start).Truncate(time.Millisecond).String()}
		if stats, err := s.store.Stats(r.Context()); err == nil {
			report.Stats = stats
		}
	}

	if s.embedder == nil {
		report.Embedder = ComponentState{OK: true, Detail: "disabled"}
	} else {
		ectx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		start = time.Now()
		err := s.embedder.Health(ectx)
		cancel()
		if err != nil {
			// A degraded embedder is reported but does not make the service
			// unhealthy: keyword search still works, and writes still land.
			// Marking the whole service down here would trigger a rollback for
			// a condition that a restart of one sidecar fixes.
			report.Embedder = ComponentState{OK: false, Detail: s.embedder.Name(), Error: err.Error()}
		} else {
			report.Embedder = ComponentState{
				OK: true, Detail: s.embedder.Name(),
				Latency: time.Since(start).Truncate(time.Millisecond).String(),
			}
		}
	}

	if !s.cfg.Consolidation.Enabled {
		report.Worker = ComponentState{OK: true, Detail: "disabled"}
	} else {
		report.Worker = ComponentState{OK: true, Detail: "scheduled"}
		if runs, err := s.store.ListRuns(r.Context(), 1); err == nil && len(runs) > 0 {
			report.Worker.Detail = "last run " + runs[0].StartedAt.UTC().Format(time.RFC3339)
			if runs[0].Error != nil {
				report.Worker.OK = false
				report.Worker.Error = *runs[0].Error
			}
		}
	}

	status := http.StatusOK
	if !report.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
	return nil
}

// --- sessions --------------------------------------------------------------

type createSessionRequest struct {
	Project string         `json:"project"`
	Agent   string         `json:"agent"`
	Meta    map[string]any `json:"meta,omitempty"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) error {
	var req createSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	id := identityFrom(r.Context())

	sess, err := s.store.CreateSession(r.Context(), id, req.Project, req.Agent, req.Meta)
	if err != nil {
		return fromStore(err, "session")
	}

	s.audit(r, id, store.ActionSessionCreate, sess.ID, map[string]any{"project": sess.Project, "agent": sess.Agent})
	s.events.publish("session.created", sess)
	writeJSON(w, http.StatusCreated, sess)
	return nil
}

type endSessionRequest struct {
	EndedAt *time.Time `json:"ended_at,omitempty"`
	Summary *string    `json:"summary,omitempty"`
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) error {
	var req endSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	id := identityFrom(r.Context())

	sess, err := s.store.EndSession(r.Context(), id, r.PathValue("id"), req.EndedAt, req.Summary)
	if err != nil {
		return fromStore(err, "session")
	}

	s.audit(r, id, store.ActionSessionEnd, sess.ID, nil)
	s.events.publish("session.ended", sess)
	writeJSON(w, http.StatusOK, sess)
	return nil
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) error {
	sess, err := s.store.GetSession(r.Context(), identityFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		return fromStore(err, "session")
	}
	writeJSON(w, http.StatusOK, sess)
	return nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) error {
	sessions, err := s.store.ListSessions(r.Context(), identityFrom(r.Context()),
		r.URL.Query().Get("project"), queryInt(r, "limit", 50))
	if err != nil {
		return fromStore(err, "sessions")
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "count": len(sessions)})
	return nil
}

func (s *Server) handleListObservations(w http.ResponseWriter, r *http.Request) error {
	obs, err := s.store.ListObservations(r.Context(), identityFrom(r.Context()),
		r.PathValue("id"), queryInt(r, "limit", 500))
	if err != nil {
		return fromStore(err, "session")
	}
	writeJSON(w, http.StatusOK, map[string]any{"observations": obs, "count": len(obs)})
	return nil
}

type addObservationsRequest struct {
	SessionID string                   `json:"session_id"`
	Items     []store.ObservationInput `json:"items"`
}

func (s *Server) handleAddObservations(w http.ResponseWriter, r *http.Request) error {
	var req addObservationsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.SessionID == "" {
		return badRequest("session_id is required")
	}
	if len(req.Items) == 0 {
		return badRequest("items must contain at least one observation")
	}
	if len(req.Items) > 1000 {
		return badRequest("items contains %d observations; the per-call maximum is 1000", len(req.Items))
	}

	id := identityFrom(r.Context())
	obs, err := s.store.AddObservations(r.Context(), id, req.SessionID, req.Items)
	if err != nil {
		return fromStore(err, "session")
	}

	redacted := 0
	for _, o := range obs {
		if o.Redacted {
			redacted++
		}
	}
	if redacted > 0 {
		s.log.Info("redacted credentials at ingest",
			"session", req.SessionID, "observations", len(obs), "redacted", redacted)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"count":    len(obs),
		"redacted": redacted,
		"ids":      idsOf(obs),
	})
	return nil
}

func idsOf(obs []store.Observation) []string {
	out := make([]string, len(obs))
	for i, o := range obs {
		out[i] = o.ID
	}
	return out
}

// --- memories --------------------------------------------------------------

func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) error {
	var in store.MemoryInput
	if err := decodeJSON(w, r, &in); err != nil {
		return err
	}
	id := identityFrom(r.Context())

	in.Embedding = s.embedText(r.Context(), in.Title+"\n"+in.Body)

	mem, created, err := s.store.CreateMemory(r.Context(), id, in)
	if err != nil {
		if strings.Contains(err.Error(), "kind:") || strings.Contains(err.Error(), "visibility:") ||
			strings.Contains(err.Error(), "id:") || strings.Contains(err.Error(), "needs a title") {
			return badRequest("%v", err)
		}
		return fromStore(err, "memory")
	}

	if created {
		s.audit(r, id, store.ActionMemoryCreate, mem.ID, map[string]any{"kind": mem.Kind, "project": mem.Project})
		s.events.publish("memory.created", mem)
		writeJSON(w, http.StatusCreated, mem)
		return nil
	}

	// 200 rather than 409. An identical memory already existing is the expected
	// outcome of a re-import or a re-flushed offline queue, and returning the
	// row the caller wanted is more useful than making them handle an error to
	// discover they already have it.
	writeJSON(w, http.StatusOK, mem)
	return nil
}

func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) error {
	mem, err := s.store.GetMemory(r.Context(), identityFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		return fromStore(err, "memory")
	}
	writeJSON(w, http.StatusOK, mem)
	return nil
}

func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) error {
	f := store.ListFilter{
		Project:           r.URL.Query().Get("project"),
		Kinds:             queryList(r, "kind"),
		IncludeSuperseded: queryBool(r, "include_superseded"),
		MineOnly:          queryBool(r, "mine"),
		Limit:             queryInt(r, "limit", 50),
		Cursor:            r.URL.Query().Get("cursor"),
	}
	for _, k := range f.Kinds {
		if !store.ValidKind(k) {
			return badRequest("kind %q is not one of fact, decision, lesson, preference", k)
		}
	}

	mems, next, err := s.store.ListMemories(r.Context(), identityFrom(r.Context()), f)
	if err != nil {
		if strings.Contains(err.Error(), "cursor:") {
			return badRequest("%v", err)
		}
		return fromStore(err, "memories")
	}

	body := map[string]any{"memories": mems, "count": len(mems)}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

type updateMemoryRequest struct {
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Visibility *string `json:"visibility,omitempty"`
}

func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) error {
	var req updateMemoryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.Title == nil && req.Body == nil && req.Visibility == nil {
		return badRequest("nothing to update: send title, body, or visibility")
	}
	id := identityFrom(r.Context())

	mem, err := s.store.UpdateMemory(r.Context(), id, r.PathValue("id"), req.Title, req.Body, req.Visibility)
	if err != nil {
		return fromStore(err, "memory")
	}

	// Re-embed straight away when the text changed, so search does not go
	// stale for the window until the backfill pass runs.
	if req.Body != nil || req.Title != nil {
		if vec := s.embedText(r.Context(), mem.Title+"\n"+mem.Body); len(vec) > 0 {
			_ = s.store.SetEmbedding(r.Context(), mem.ID, vec)
		}
	}

	s.audit(r, id, store.ActionMemoryUpdate, mem.ID, nil)
	s.events.publish("memory.updated", mem)
	writeJSON(w, http.StatusOK, mem)
	return nil
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	memID := r.PathValue("id")

	if err := s.store.DeleteMemory(r.Context(), id, memID); err != nil {
		return fromStore(err, "memory")
	}

	s.audit(r, id, store.ActionMemoryDelete, memID, nil)
	s.events.publish("memory.deleted", map[string]any{"id": memID})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": memID})
	return nil
}

type supersedeRequest struct {
	NewID string `json:"new_id"`
}

func (s *Server) handleSupersede(w http.ResponseWriter, r *http.Request) error {
	var req supersedeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.NewID == "" {
		return badRequest("new_id is required")
	}
	id := identityFrom(r.Context())
	oldID := r.PathValue("id")

	if err := s.store.Supersede(r.Context(), id, oldID, req.NewID); err != nil {
		if strings.Contains(err.Error(), "cannot supersede itself") {
			return badRequest("%v", err)
		}
		return fromStore(err, "memory")
	}

	s.audit(r, id, store.ActionMemorySupersede, oldID, map[string]any{"new_id": req.NewID})
	s.events.publish("memory.superseded", map[string]any{"id": oldID, "new_id": req.NewID})
	writeJSON(w, http.StatusOK, map[string]any{"superseded": oldID, "by": req.NewID})
	return nil
}

func (s *Server) handleReinforce(w http.ResponseWriter, r *http.Request) error {
	mem, err := s.store.Reinforce(r.Context(), identityFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		return fromStore(err, "memory")
	}
	writeJSON(w, http.StatusOK, mem)
	return nil
}

// --- search ----------------------------------------------------------------

type searchRequest struct {
	Query             string   `json:"query"`
	Project           string   `json:"project,omitempty"`
	Kinds             []string `json:"kinds,omitempty"`
	Limit             int      `json:"limit,omitempty"`
	MineOnly          bool     `json:"mine_only,omitempty"`
	IncludeSuperseded bool     `json:"include_superseded,omitempty"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) error {
	var req searchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Query) == "" {
		return badRequest("query is required")
	}
	for _, k := range req.Kinds {
		if !store.ValidKind(k) {
			return badRequest("kind %q is not one of fact, decision, lesson, preference", k)
		}
	}

	id := identityFrom(r.Context())
	q := store.SearchQuery{
		Query:             req.Query,
		Project:           req.Project,
		Kinds:             req.Kinds,
		Limit:             req.Limit,
		MineOnly:          req.MineOnly,
		IncludeSuperseded: req.IncludeSuperseded,
		Vector:            s.embedText(r.Context(), req.Query),
	}

	results, err := s.store.Search(r.Context(), id, q)
	if err != nil {
		return fromStore(err, "search")
	}

	mode := "hybrid"
	if len(q.Vector) == 0 {
		mode = "keyword"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"count":   len(results),
		"mode":    mode,
	})
	return nil
}

type contextRequest struct {
	Project      string   `json:"project"`
	BudgetTokens int      `json:"budget_tokens,omitempty"`
	Include      []string `json:"include,omitempty"`
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) error {
	var req contextRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	payload, err := s.store.BuildContext(r.Context(), identityFrom(r.Context()),
		req.Project, req.BudgetTokens, req.Include)
	if err != nil {
		return fromStore(err, "context")
	}
	writeJSON(w, http.StatusOK, payload)
	return nil
}

// --- lessons ---------------------------------------------------------------

func (s *Server) handleListLessons(w http.ResponseWriter, r *http.Request) error {
	lessons, err := s.store.Lessons(r.Context(), identityFrom(r.Context()),
		r.URL.Query().Get("project"), queryInt(r, "limit", 50))
	if err != nil {
		return fromStore(err, "lessons")
	}
	writeJSON(w, http.StatusOK, map[string]any{"lessons": lessons, "count": len(lessons)})
	return nil
}

type createLessonRequest struct {
	Lesson     string   `json:"lesson"`
	Body       string   `json:"body,omitempty"`
	Project    string   `json:"project,omitempty"`
	Files      []string `json:"files,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
}

func (s *Server) handleCreateLesson(w http.ResponseWriter, r *http.Request) error {
	var req createLessonRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Lesson) == "" {
		return badRequest("lesson is required")
	}

	in := store.MemoryInput{
		Kind:       store.KindLesson,
		Title:      req.Lesson,
		Body:       req.Body,
		Project:    req.Project,
		Files:      req.Files,
		Visibility: req.Visibility,
	}
	if in.Body == "" {
		in.Body = req.Lesson
	}
	in.Embedding = s.embedText(r.Context(), in.Title+"\n"+in.Body)

	id := identityFrom(r.Context())
	mem, created, err := s.store.CreateMemory(r.Context(), id, in)
	if err != nil {
		return fromStore(err, "lesson")
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.audit(r, id, store.ActionMemoryCreate, mem.ID, map[string]any{"kind": "lesson"})
		s.events.publish("lesson.created", mem)
	}
	writeJSON(w, status, mem)
	return nil
}

// --- sharing ---------------------------------------------------------------

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	mem, err := s.store.Share(r.Context(), id, r.PathValue("id"))
	if err != nil {
		return fromStore(err, "memory")
	}

	s.audit(r, id, store.ActionMemoryShare, mem.ID, nil)
	s.events.publish("memory.shared", mem)
	writeJSON(w, http.StatusOK, mem)
	return nil
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) error {
	mems, err := s.store.Feed(r.Context(), identityFrom(r.Context()),
		r.URL.Query().Get("project"), queryInt(r, "limit", 20))
	if err != nil {
		return fromStore(err, "feed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": mems, "count": len(mems)})
	return nil
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) error {
	projects, err := s.store.Projects(r.Context(), identityFrom(r.Context()))
	if err != nil {
		return fromStore(err, "projects")
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects, "count": len(projects)})
	return nil
}

// --- graph -----------------------------------------------------------------

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	g, err := s.store.GetGraph(r.Context(), id,
		r.URL.Query().Get("project"),
		r.URL.Query().Get("node"),
		queryInt(r, "depth", 2),
		queryInt(r, "limit", 500))
	if err != nil {
		return fromStore(err, "graph")
	}

	// An empty graph on a small corpus is the correct answer, not a failure.
	// Nodes come from real co-occurrence; there is nothing to invent when
	// nothing has co-occurred yet.
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": g.Nodes, "edges": g.Edges,
		"node_count": len(g.Nodes), "edge_count": len(g.Edges),
	})
	return nil
}

func (s *Server) handleGraphRebuild(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r.Context())
	project := r.URL.Query().Get("project")
	if project == "" {
		return badRequest("project is required; rebuilding every project at once is a worker job, not a request")
	}

	nodes, edges, err := s.store.RebuildGraph(r.Context(), id.TeamID, project)
	if err != nil {
		return fromStore(err, "graph")
	}

	s.audit(r, id, store.ActionGraphRebuild, project, map[string]any{"nodes": nodes, "edges": edges})
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "nodes": nodes, "edges": edges})
	return nil
}
