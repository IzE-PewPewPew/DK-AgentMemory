package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// Audit actions. Kept as constants so a query for "who deleted things" does not
// depend on remembering whether the verb was recorded as delete or deleted.
const (
	ActionMemoryCreate    = "memory.create"
	ActionMemoryUpdate    = "memory.update"
	ActionMemoryDelete    = "memory.delete"
	ActionMemorySupersede = "memory.supersede"
	ActionMemoryShare     = "memory.share"
	ActionSessionCreate   = "session.create"
	ActionSessionEnd      = "session.end"
	ActionObservationAdd  = "observation.add"
	ActionImport          = "import"
	ActionExport          = "export"
	ActionGraphRebuild    = "graph.rebuild"
	ActionTeamCreate      = "admin.team.create"
	ActionUserCreate      = "admin.user.create"
	ActionKeyIssue        = "admin.key.issue"
	ActionKeyRevoke       = "admin.key.revoke"
)

// Audit records one mutation.
//
// Best effort by design: it is called after the mutation has already committed,
// and a failure to write the audit row must not undo the operation the user
// asked for. The error is returned so a caller can log it; no caller aborts on
// it.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if !s.cfg.Security.AuditEnabled {
		return nil
	}
	detail, err := jsonObjectOrEmpty(e.Detail)
	if err != nil {
		return err
	}
	if e.ID == "" {
		e.ID = ulid.New()
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO audit_log (id, team_id, user_id, key_id, action, target, detail, request_id, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, e.ID, nullIfEmpty(e.TeamID), nullIfEmpty(e.UserID), nullIfEmpty(e.KeyID),
		e.Action, nullIfEmpty(e.Target), detail, nullIfEmpty(e.RequestID), nullIfEmpty(e.IP))
	return err
}

// AuditFilter narrows an audit query.
type AuditFilter struct {
	User   string
	Action string
	Since  *time.Time
	Limit  int
}

// ListAudit returns audit entries for a team, newest first.
func (s *Store) ListAudit(ctx context.Context, teamID string, f AuditFilter) ([]AuditEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	a := &argList{}
	var q strings.Builder
	q.WriteString(`SELECT id, team_id, user_id, key_id, action, target, detail, request_id, ip, created_at
	               FROM audit_log WHERE team_id = ` + a.add(teamID))
	if f.User != "" {
		fmt.Fprintf(&q, " AND user_id = %s", a.add(f.User))
	}
	if f.Action != "" {
		fmt.Fprintf(&q, " AND action = %s", a.add(f.Action))
	}
	if f.Since != nil {
		fmt.Fprintf(&q, " AND created_at >= %s", a.add(*f.Since))
	}
	fmt.Fprintf(&q, " ORDER BY created_at DESC LIMIT %s", a.add(limit))

	rows, err := s.pool.Query(ctx, q.String(), a.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e                       AuditEntry
			team, user, key, target *string
			requestID, ip           *string
			detail                  []byte
		)
		if err := rows.Scan(&e.ID, &team, &user, &key, &e.Action, &target, &detail, &requestID, &ip, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.TeamID, e.UserID, e.KeyID = deref(team), deref(user), deref(key)
		e.Target, e.RequestID, e.IP = deref(target), deref(requestID), deref(ip)
		if len(detail) > 0 {
			_ = unmarshalMap(detail, &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
