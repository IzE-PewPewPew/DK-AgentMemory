package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) error {
	keys, err := s.store.ListKeys(r.Context(), identityFrom(r.Context()).TeamID)
	if err != nil {
		return fromStore(err, "keys")
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "count": len(keys)})
	return nil
}

type issueKeyRequest struct {
	UserID string `json:"user_id"`
	Label  string `json:"label,omitempty"`
}

func (s *Server) handleIssueKey(w http.ResponseWriter, r *http.Request) error {
	var req issueKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.UserID == "" {
		return badRequest("user_id is required")
	}
	admin := identityFrom(r.Context())

	// An admin may only issue keys inside their own team. Without this check an
	// admin key is a global key, and the team boundary stops being a boundary.
	target, err := s.store.GetUser(r.Context(), req.UserID)
	if err != nil {
		return fromStore(err, "user "+req.UserID)
	}
	if target.TeamID != admin.TeamID {
		return forbidden("user " + req.UserID + " belongs to another team")
	}

	issued, err := s.store.IssueKey(r.Context(), req.UserID, req.Label)
	if err != nil {
		return fromStore(err, "user")
	}

	s.audit(r, admin, store.ActionKeyIssue, issued.ID, map[string]any{
		"user_id": req.UserID, "label": req.Label, "prefix": issued.Prefix,
	})
	s.log.Info("api key issued", "key_id", issued.ID, "prefix", issued.Prefix,
		"user", req.UserID, "by", admin.UserID)

	// The plaintext appears here and nowhere else, ever. It is not logged, not
	// stored, and not recoverable.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      issued.ID,
		"user_id": issued.UserID,
		"prefix":  issued.Prefix,
		"label":   issued.Label,
		"key":     issued.Plaintext,
		"notice":  "this key is shown once and cannot be recovered; store it now",
	})
	return nil
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) error {
	admin := identityFrom(r.Context())
	keyID := r.PathValue("id")

	if err := s.store.RevokeKey(r.Context(), admin.TeamID, keyID); err != nil {
		return fromStore(err, "key")
	}

	s.audit(r, admin, store.ActionKeyRevoke, keyID, nil)
	s.log.Info("api key revoked", "key_id", keyID, "by", admin.UserID)

	writeJSON(w, http.StatusOK, map[string]any{
		"revoked": keyID,
		"effect":  "the next request using this key is rejected; there is no cache to wait out",
	})
	return nil
}

type createTeamRequest struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) error {
	var req createTeamRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.ID) == "" {
		return badRequest("id is required")
	}

	team, err := s.store.CreateTeam(r.Context(), req.ID, req.Name)
	if err != nil {
		return fromStore(err, "team")
	}

	s.audit(r, identityFrom(r.Context()), store.ActionTeamCreate, team.ID, nil)
	writeJSON(w, http.StatusCreated, team)
	return nil
}

type createUserRequest struct {
	ID      string `json:"id"`
	TeamID  string `json:"team_id"`
	Name    string `json:"name,omitempty"`
	IsAdmin bool   `json:"is_admin,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) error {
	var req createUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.ID) == "" {
		return badRequest("id is required")
	}
	admin := identityFrom(r.Context())
	if req.TeamID == "" {
		req.TeamID = admin.TeamID
	}
	if req.TeamID != admin.TeamID {
		return forbidden("an admin can only create users in their own team")
	}

	user, err := s.store.CreateUser(r.Context(), req.ID, req.TeamID, req.Name, req.IsAdmin)
	if err != nil {
		return fromStore(err, "team "+req.TeamID)
	}

	s.audit(r, admin, store.ActionUserCreate, user.ID, map[string]any{"team_id": user.TeamID, "is_admin": user.IsAdmin})
	writeJSON(w, http.StatusCreated, user)
	return nil
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) error {
	users, err := s.store.ListUsers(r.Context(), identityFrom(r.Context()).TeamID)
	if err != nil {
		return fromStore(err, "users")
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "count": len(users)})
	return nil
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) error {
	f := store.AuditFilter{
		User:   r.URL.Query().Get("user"),
		Action: r.URL.Query().Get("action"),
		Limit:  queryInt(r, "limit", 100),
	}
	if since := r.URL.Query().Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return badRequest("since must be an RFC3339 timestamp, e.g. 2026-08-01T00:00:00Z")
		}
		f.Since = &t
	}

	entries, err := s.store.ListAudit(r.Context(), identityFrom(r.Context()).TeamID, f)
	if err != nil {
		return fromStore(err, "audit log")
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "count": len(entries)})
	return nil
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) error {
	runs, err := s.store.ListRuns(r.Context(), queryInt(r, "limit", 20))
	if err != nil {
		return fromStore(err, "consolidation runs")
	}

	since := time.Now().AddDate(0, 0, -30)
	inTok, outTok, err := s.store.TokenSpend(r.Context(), since)
	if err != nil {
		return fromStore(err, "token spend")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"runs": runs,
		"spend_last_30d": map[string]any{
			"input_tokens":  inTok,
			"output_tokens": outTok,
		},
	})
	return nil
}
