// Package api serves the REST surface, the read-only viewer, and the SSE event
// stream from a single origin.
//
// One origin matters more than it sounds. Serving the viewer from a second
// hostname means CORS, a second TLS certificate, and a tunnel config with two
// ingress rules -- and every one of those is a thing that can be wrong at 2am
// while the API itself is fine.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/embed"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// authLevel describes what a route requires.
type authLevel int

const (
	authNone authLevel = iota
	authUser
	authAdmin
)

// route is a single endpoint.
//
// The OpenAPI document is generated from this table, and the mux is built from
// the same table. They cannot drift, because there is only one list: adding a
// route without documenting it is not possible, and documenting one that does
// not exist is not either.
type route struct {
	Method   string
	Pattern  string
	Summary  string
	Tag      string
	Auth     authLevel
	Handler  http.HandlerFunc
	Request  string
	Response string
}

// Server holds everything the HTTP layer needs.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	embedder embed.Embedder
	log      *slog.Logger
	limiter  *limiter
	events   *eventHub
	mcp      http.Handler

	consolidator Consolidator

	routes    []route
	mux       *http.ServeMux
	startedAt time.Time
}

// Options configures New.
type Options struct {
	Config   *config.Config
	Store    *store.Store
	Embedder embed.Embedder
	Logger   *slog.Logger
	// DisableMCP leaves /mcp unmounted. Only useful for tests that want to
	// assert the REST surface in isolation.
	DisableMCP bool

	// Consolidator lets an operator run the pipeline on demand. Optional: when
	// nil, the endpoint reports that consolidation is not available rather than
	// disappearing, so the answer to "why is nothing being distilled" is in the
	// response instead of in a 404.
	Consolidator Consolidator
}

// Consolidator runs the consolidation tiers immediately.
//
// An interface rather than the concrete worker, because internal/consolidate
// imports internal/store and internal/embed, and the API importing it directly
// would make the dependency graph a cycle waiting to happen.
type Consolidator interface {
	RunNow(ctx context.Context, tiers []int) (any, error)
	Enabled() bool
	Provider() string
}

// New builds the server and registers every route.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	s := &Server{
		cfg:      opts.Config,
		store:    opts.Store,
		embedder: opts.Embedder,
		log:      log,
		limiter: newLimiter(
			opts.Config.Security.RateLimitWritesPerMin,
			opts.Config.Security.RateLimitReadsPerMin,
		),
		events:       newEventHub(),
		consolidator: opts.Consolidator,
		startedAt:    time.Now(),
	}

	// The streamable-HTTP MCP transport is served by this same process, from
	// this same store, behind this same bearer middleware. Building it here
	// rather than passing it in keeps that single-auth-path property visible.
	if !opts.DisableMCP {
		s.mcp = s.MCPHandler()
	}

	s.routes = s.buildRoutes()
	s.mux = s.buildMux()
	return s
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return withRequestID(s.withRecovery(s.withLogging(s.mux)))
}

// Events exposes the SSE hub so writers elsewhere can publish.
func (s *Server) Events() *eventHub { return s.events }

// Close releases server-held resources.
func (s *Server) Close() { s.events.close() }

// handle adapts an error-returning handler to http.HandlerFunc.
//
// Handlers return errors rather than writing them, so every error path goes
// through exactly one renderer and no handler can accidentally emit a bare
// status with no body.
func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

func (s *Server) buildRoutes() []route {
	r := func(method, pattern, tag, summary string, auth authLevel, h func(http.ResponseWriter, *http.Request) error) route {
		return route{
			Method: method, Pattern: pattern, Tag: tag, Summary: summary,
			Auth: auth, Handler: s.handle(h),
		}
	}

	routes := []route{
		// Health. livez is unauthenticated on purpose: an uptime check should
		// not need a credential, and a credential that lives in a monitoring
		// system is a credential in one more place.
		r("GET", "/v1/livez", "health", "Liveness probe, no authentication", authNone, s.handleLivez),
		r("GET", "/v1/healthz", "health", "Deep health: database, embedder, worker, version", authUser, s.handleHealthz),
		r("GET", "/v1/openapi.json", "health", "OpenAPI 3.1 document generated from the route table", authUser, s.handleOpenAPI),

		// Sessions and observations.
		r("POST", "/v1/sessions", "sessions", "Open a session", authUser, s.handleCreateSession),
		r("PATCH", "/v1/sessions/{id}", "sessions", "Close a session, optionally with a summary", authUser, s.handleEndSession),
		r("GET", "/v1/sessions", "sessions", "List recent sessions", authUser, s.handleListSessions),
		r("GET", "/v1/sessions/{id}", "sessions", "Fetch one session", authUser, s.handleGetSession),
		r("GET", "/v1/sessions/{id}/observations", "sessions", "List a session's observations", authUser, s.handleListObservations),
		r("POST", "/v1/observations", "sessions", "Batch-ingest observations", authUser, s.handleAddObservations),

		// Memories.
		r("POST", "/v1/memories", "memories", "Store a memory", authUser, s.handleCreateMemory),
		r("GET", "/v1/memories", "memories", "List memories, cursor-paginated", authUser, s.handleListMemories),
		r("GET", "/v1/memories/{id}", "memories", "Fetch one memory", authUser, s.handleGetMemory),
		r("PATCH", "/v1/memories/{id}", "memories", "Edit a memory you own", authUser, s.handleUpdateMemory),
		r("DELETE", "/v1/memories/{id}", "memories", "Soft-delete a memory you own", authUser, s.handleDeleteMemory),
		r("POST", "/v1/memories/{id}/supersede", "memories", "Mark a memory replaced by another", authUser, s.handleSupersede),
		r("POST", "/v1/memories/{id}/reinforce", "memories", "Raise a memory's strength", authUser, s.handleReinforce),

		// Retrieval.
		r("POST", "/v1/search", "search", "Hybrid BM25 + vector search, RRF-fused", authUser, s.handleSearch),
		r("POST", "/v1/context", "search", "Token-budgeted project context for session start", authUser, s.handleContext),

		// Lessons.
		r("GET", "/v1/lessons", "lessons", "List active lessons", authUser, s.handleListLessons),
		r("POST", "/v1/lessons", "lessons", "Store a lesson", authUser, s.handleCreateLesson),
		r("POST", "/v1/lessons/{id}/reinforce", "lessons", "Reinforce a lesson", authUser, s.handleReinforce),

		// Sharing.
		r("POST", "/v1/share/{id}", "sharing", "Promote a private memory to team visibility", authUser, s.handleShare),
		r("GET", "/v1/feed", "sharing", "Recently shared team memories", authUser, s.handleFeed),

		// Projects.
		r("GET", "/v1/projects", "projects", "Projects visible to the caller, with counts", authUser, s.handleProjects),

		// Graph.
		r("GET", "/v1/graph", "graph", "Project graph, optionally seeded and depth-limited", authUser, s.handleGraph),
		r("POST", "/v1/graph/rebuild", "graph", "Rebuild the graph from co-occurrence; idempotent", authUser, s.handleGraphRebuild),

		// Sync.
		r("GET", "/v1/sync", "sync", "Incremental changes for the local mirror", authUser, s.handleSync),

		// Import and export.
		r("GET", "/v1/export", "transfer", "Stream the corpus as NDJSON", authUser, s.handleExport),
		r("POST", "/v1/import", "transfer", "Ingest an NDJSON stream", authUser, s.handleImport),
		r("POST", "/v1/import/preview", "transfer", "Dry run: counts, grouping, and a secret report", authUser, s.handleImportPreview),

		// Events.
		r("GET", "/v1/events", "events", "Server-sent event stream for the viewer", authUser, s.handleEvents),

		// Admin.
		r("GET", "/v1/admin/keys", "admin", "List keys in the caller's team", authAdmin, s.handleListKeys),
		r("POST", "/v1/admin/keys", "admin", "Issue a key; the plaintext is returned once", authAdmin, s.handleIssueKey),
		r("DELETE", "/v1/admin/keys/{id}", "admin", "Revoke a key, effective on the next request", authAdmin, s.handleRevokeKey),
		r("POST", "/v1/admin/teams", "admin", "Create a team", authAdmin, s.handleCreateTeam),
		r("POST", "/v1/admin/users", "admin", "Create a user", authAdmin, s.handleCreateUser),
		r("GET", "/v1/admin/users", "admin", "List users in the caller's team", authAdmin, s.handleListUsers),
		r("GET", "/v1/admin/audit", "admin", "Query the audit log", authAdmin, s.handleAudit),
		r("GET", "/v1/admin/runs", "admin", "Recent consolidation runs and token spend", authAdmin, s.handleRuns),
		r("POST", "/v1/admin/consolidate", "admin", "Run the consolidation pipeline now instead of waiting for the schedule", authAdmin, s.handleConsolidate),

		// Activity: what each connected agent has actually captured. The
		// question "is Claude Desktop feeding this thing anything?" should be
		// answerable without reading the sessions table by hand.
		r("GET", "/v1/activity", "projects", "Capture activity grouped by agent", authUser, s.handleActivity),
	}

	return routes
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	for _, rt := range s.routes {
		h := rt.Handler
		switch rt.Auth {
		case authUser:
			h = s.requireAuth(h)
		case authAdmin:
			h = s.requireAdmin(h)
		}
		mux.HandleFunc(rt.Method+" "+rt.Pattern, h)
	}

	// MCP over streamable HTTP, so a remote agent can reach the same twelve
	// tools without a local binary.
	if s.mcp != nil {
		mux.Handle("/mcp", s.requireAuth(s.mcp.ServeHTTP))
		mux.Handle("/mcp/", s.requireAuth(s.mcp.ServeHTTP))
	}

	if s.cfg.Server.ViewerEnabled {
		mount := strings.TrimRight(s.cfg.Server.ViewerPath, "/")
		if mount == "" {
			mount = "/viewer"
		}
		// The viewer carries no write endpoints and no credential of its own.
		// It asks the operator for a key, keeps it in sessionStorage, and calls
		// the same API everything else does.
		mux.Handle(mount, http.RedirectHandler(mount+"/", http.StatusMovedPermanently))
		mux.Handle(mount+"/", http.StripPrefix(mount+"/", viewerHandler()))
	}

	// Catch-all. Without it the standard mux answers an unknown path with a
	// bare 404 and an empty body, which is indistinguishable from a proxy that
	// never reached this process at all.
	mux.HandleFunc("/", s.handle(func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/" {
			writeJSON(w, http.StatusOK, map[string]any{
				"service": "dkm",
				"version": version.Short(),
				"docs":    "/v1/openapi.json",
				"viewer":  s.viewerLink(),
			})
			return nil
		}
		return &APIError{
			Status: http.StatusNotFound, Code: CodeNotFound,
			Message: "no route for " + r.Method + " " + r.URL.Path,
			Hint:    "GET /v1/openapi.json lists every route this build serves",
		}
	}))

	return mux
}

func (s *Server) viewerLink() string {
	if !s.cfg.Server.ViewerEnabled {
		return ""
	}
	return strings.TrimRight(s.cfg.Server.ViewerPath, "/") + "/"
}

// --- shared request helpers ------------------------------------------------

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}

func queryList(r *http.Request, name string) []string {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// audit records a mutation, logging rather than failing when the write fails.
func (s *Server) audit(r *http.Request, id store.Identity, action, target string, detail map[string]any) {
	entry := store.AuditEntry{
		TeamID:    id.TeamID,
		UserID:    id.UserID,
		KeyID:     id.KeyID,
		Action:    action,
		Target:    target,
		Detail:    detail,
		RequestID: requestIDOf(r),
		IP:        clientIP(r),
	}
	if err := s.store.Audit(r.Context(), entry); err != nil {
		s.log.Warn("audit write failed", "action", action, "error", err)
	}
}

// ListenAndServe runs the HTTP server until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Bind,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.cfg.Server.ReadTimeout.Duration(),
		// SSE streams live longer than any write timeout worth setting for
		// ordinary requests, so the write deadline is left off and slow-client
		// protection comes from the read header timeout and the connection cap.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening",
			"bind", s.cfg.Server.Bind,
			"public_url", s.cfg.Server.PublicURL,
			"viewer", s.viewerLink(),
			"version", version.Short())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	grace := s.cfg.Server.ShutdownGrace.Duration()
	s.log.Info("shutting down", "grace", grace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	s.Close()
	return srv.Shutdown(shutdownCtx)
}
