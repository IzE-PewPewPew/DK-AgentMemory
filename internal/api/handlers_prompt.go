package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/compose"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// promptRequest is what the composer takes.
//
// decodeJSON rejects unknown fields, so this struct and the viewer's payload
// have to agree exactly.
type promptRequest struct {
	Description string   `json:"description"`
	Project     string   `json:"project,omitempty"`
	Target      string   `json:"target,omitempty"`
	Emphases    []string `json:"emphases,omitempty"`
	Mode        string   `json:"mode,omitempty"`
}

type promptCost struct {
	InputTokens       int  `json:"input_tokens"`
	OutputTokens      int  `json:"output_tokens"`
	IncludesReasoning bool `json:"includes_reasoning"`
	Estimated         bool `json:"estimated"`
}

// handlePrompt turns a rough description into a finished prompt.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) error {
	req, err := decodePromptRequest(w, r)
	if err != nil {
		return err
	}
	id := identityFrom(r.Context())

	if s.consolidator == nil || !s.consolidator.Enabled() {
		// 200 with a reason, matching the consolidate route. Nothing is broken
		// -- the provider is not configured -- and the caller needs to be told
		// which knob turns it on rather than left to read a 500.
		reason := "no LLM provider is configured"
		if s.consolidator != nil {
			if dr := s.consolidator.DisabledReason(); dr != "" {
				reason = dr
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"generated": false,
			"reason":    reason,
			"fix": "set consolidation.enabled: true, configure consolidation.llm, " +
				"export the API key named by consolidation.llm.api_key_env in the shell " +
				"that starts the server, and restart",
		})
		return nil
	}

	// Narrowed to what will actually reach the model, so the response reports
	// the grounding rather than the retrieval.
	memories := compose.Select(s.groundingFor(r, id, req))
	in := compose.Input{
		Task:     req.Description,
		Project:  req.Project,
		Target:   req.Target,
		Mode:     req.Mode,
		Emphases: req.Emphases,
		Memories: memories,
	}

	resp, err := s.consolidator.Complete(r.Context(), compose.System, compose.Build(in))
	if err != nil {
		// A reasoning model that spends its whole budget thinking produces
		// nothing and is not worth retrying as-asked -- the next attempt fails
		// identically and bills identically. Retry once with strictly less to
		// think about instead: the brief form, and half the memories.
		if isTokenBudget(err) {
			in.Mode = "brief"
			if len(in.Memories) > 3 {
				in.Memories = in.Memories[:3]
			}
			resp, err = s.consolidator.Complete(r.Context(), compose.System, compose.Build(in))
		}
		if err != nil {
			return &APIError{
				Status: http.StatusServiceUnavailable, Code: CodeUnavailable,
				Message: err.Error(),
				Hint: "check consolidation.llm.base_url, that the API key environment " +
					"variable is set, and that the key still has quota",
			}
		}
	}

	out := compose.Normalise(resp.Text)
	if out.Prompt == "" {
		return &APIError{
			Status: http.StatusServiceUnavailable, Code: CodeUnavailable,
			Message: "the model returned an empty prompt",
			Hint:    "raise consolidation.llm.max_tokens; a reasoning model spends it before writing",
		}
	}

	s.audit(r, id, "prompt.generate", s.consolidator.Provider(), map[string]any{
		"project":       req.Project,
		"grounded":      len(in.Memories),
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"generated":   true,
		"prompt":      out.Prompt,
		"understood":  out.Understood,
		"assumptions": orEmpty(out.Assumptions),
		"grounded":    len(in.Memories) > 0,
		"memories":    in.Memories,
		"provider":    s.consolidator.Provider(),
		"cost": promptCost{
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			// On a reasoning model the reported completion count includes the
			// tokens spent thinking. Labelling it plainly matters: unlabelled,
			// the number reads as several times smaller than the bill.
			IncludesReasoning: true,
			Estimated:         resp.Estimated,
		},
	})
	return nil
}

// handlePromptPreview shows what would ground the prompt, without spending
// anything at the provider.
//
// Worth its own route because the grounding is the part the user cannot
// predict, and finding out what the corpus knows should not cost a generation.
func (s *Server) handlePromptPreview(w http.ResponseWriter, r *http.Request) error {
	req, err := decodePromptRequest(w, r)
	if err != nil {
		return err
	}
	id := identityFrom(r.Context())

	// Narrowed to what will actually reach the model, so the response reports
	// the grounding rather than the retrieval.
	memories := compose.Select(s.groundingFor(r, id, req))
	in := compose.Input{
		Task:     req.Description,
		Project:  req.Project,
		Target:   req.Target,
		Mode:     req.Mode,
		Emphases: req.Emphases,
		Memories: memories,
	}
	user := compose.Build(in)

	writeJSON(w, http.StatusOK, map[string]any{
		"memories": memories,
		"grounded": len(memories) > 0,
		"mode":     modeOf(user),
		// Roughly four characters to the token. An estimate, labelled as one,
		// is more useful here than nothing: it tells the caller whether this
		// request is a cheap one before they pay for it.
		"estimated_input_tokens": (len(compose.System) + len(user)) / 4,
	})
	return nil
}

// handleProgress reports how far the consolidation pipeline has got.
//
// authUser, not authAdmin: /v1/admin/runs is admin-only, and the progress
// indicator has to work for an ordinary read key or it is absent exactly when
// someone is wondering whether anything is happening.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) error {
	p, err := s.store.Progress(r.Context(), identityFrom(r.Context()))
	if err != nil {
		return fromStore(err, "progress")
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func decodePromptRequest(w http.ResponseWriter, r *http.Request) (promptRequest, error) {
	var req promptRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return req, err
	}

	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		return req, badRequest("description is required; say what you want to build")
	}
	// A one-word description is legal and must stay legal. It is how this gets
	// used in practice, and the memory corpus is what makes it answerable.
	if len([]rune(req.Description)) > compose.MaxTask {
		return req, badRequest("description is %d characters; the limit is %d",
			len([]rune(req.Description)), compose.MaxTask)
	}
	for _, e := range req.Emphases {
		if !compose.ValidEmphasis(strings.ToLower(strings.TrimSpace(e))) {
			return req, badRequest("unknown emphasis %q; valid values are %s",
				e, strings.Join(compose.Emphases, ", "))
		}
	}
	if req.Mode != "" && req.Mode != "brief" && req.Mode != "full" {
		return req, badRequest("mode must be brief or full")
	}
	return req, nil
}

// groundingFor retrieves the memories that will inform the prompt.
//
// Two passes, merged. Standing conventions come from the project context and
// are true whatever the task is; task-specific lessons come from hybrid search
// and are the ones that make a generated prompt worth more than a generic one.
// Neither alone is enough -- context cannot know the task, and search over a
// two-line description in imperfect English misses conventions that always
// hold.
//
// Retrieval never fails the request. A prompt grounded in nothing is still a
// usable prompt, and it says so; a 500 because the embedder was slow is not.
// BriefTitle marks the one memory per project that the user wrote by hand.
//
// A brief works the day it is written. Everything else in the grounding has to
// wait for hundreds of sessions to be read and distilled, which costs tokens
// and hours -- so on a fresh project the corpus knows nothing and the composer
// honestly says so. Three sentences typed by the person who owns the codebase
// closes that gap immediately.
const BriefTitle = "Project brief"

func (s *Server) groundingFor(r *http.Request, id store.Identity, req promptRequest) []compose.Memory {
	var out []compose.Memory

	// The brief goes first and is never displaced. Ranked against the task like
	// anything else it would score poorly -- it is general by design -- and be
	// pushed out by a specific but less important match.
	if req.Project != "" {
		if b := s.projectBrief(r, id, req.Project); b != nil {
			out = append(out, *b)
		}
	}

	if ctx, err := s.store.BuildContext(r.Context(), id, req.Project, 700,
		[]string{"lessons", "decisions", "preferences"}); err == nil && ctx != nil {
		// Facts and session summaries are deliberately absent from the standing
		// block. A session summary is narrative and cannot become a constraint;
		// a fact as a standing line is where "this project uses Postgres" gets
		// paid for on every paste without changing anything.
		out = append(out, flatten(ctx.Lessons, "context")...)
		out = append(out, flatten(ctx.Decisions, "context")...)
		out = append(out, flatten(ctx.Preferences, "context")...)
	}

	hits, err := s.store.Search(r.Context(), id, store.SearchQuery{
		Query:   req.Description,
		Project: req.Project,
		Kinds:   []string{store.KindLesson, store.KindDecision, store.KindPreference, store.KindFact},
		Limit:   12,
		Vector:  s.embedQuery(r.Context(), req.Description),
	})
	if err == nil {
		for _, h := range hits {
			// Facts earn their place when ranked against the task even though
			// they do not as a standing block: "Tauri v2, not Electron" is a
			// fact, and it is the most load-bearing line a generated prompt for
			// this project can carry.
			out = append(out, compose.Memory{
				ID: h.ID, Kind: h.Kind, Title: h.Title, Body: h.Body,
				Project: h.Project, Score: h.Score, Source: "search",
			})
		}
	}

	return compose.Rank(compose.Dedupe(out))
}

// projectBrief returns the hand-written brief for a project, if there is one.
func (s *Server) projectBrief(r *http.Request, id store.Identity, project string) *compose.Memory {
	ms, _, err := s.store.ListMemories(r.Context(), id, store.ListFilter{
		Project: project,
		Kinds:   []string{store.KindPreference},
		Limit:   50,
	})
	if err != nil {
		return nil
	}
	for _, m := range ms {
		if m.Title == BriefTitle {
			return &compose.Memory{
				ID: m.ID, Kind: m.Kind, Title: m.Title, Body: m.Body,
				Project: m.Project, Source: compose.SourceBrief,
			}
		}
	}
	return nil
}

func flatten(ms []store.Memory, source string) []compose.Memory {
	out := make([]compose.Memory, 0, len(ms))
	for _, m := range ms {
		out = append(out, compose.Memory{
			ID: m.ID, Kind: m.Kind, Title: m.Title, Body: m.Body,
			Project: m.Project, Source: source,
		})
	}
	return out
}

func isTokenBudget(err error) bool {
	// Matched on the message rather than the sentinel: internal/api does not
	// import internal/consolidate, deliberately, and widening the Consolidator
	// interface for one comparison would be a worse trade than this.
	var e interface{ Error() string }
	return errors.As(err, &e) && strings.Contains(err.Error(), "model produced no answer")
}

func modeOf(userMessage string) string {
	if strings.Contains(userMessage, "MODE: brief") {
		return "brief"
	}
	return "full"
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
