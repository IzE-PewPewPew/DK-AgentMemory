package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// CreateSession opens a session for the calling identity.
func (s *Store) CreateSession(ctx context.Context, id Identity, project, agent string, meta map[string]any) (*Session, error) {
	metaJSON, err := jsonObjectOrEmpty(meta)
	if err != nil {
		return nil, err
	}

	sess := Session{
		ID:      ulid.New(),
		TeamID:  id.TeamID,
		UserID:  id.UserID,
		Project: project,
		Agent:   agent,
		Meta:    meta,
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO sessions (id, team_id, user_id, project, agent, meta)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING started_at
	`, sess.ID, sess.TeamID, sess.UserID, sess.Project, sess.Agent, metaJSON).Scan(&sess.StartedAt)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// EndSession closes a session and optionally records a summary.
//
// Scoped by team_id in the UPDATE itself: a caller cannot end another team's
// session even with a valid ULID, and the check is in the statement rather than
// in a preceding SELECT that a future refactor could drop.
func (s *Store) EndSession(ctx context.Context, id Identity, sessionID string, endedAt *time.Time, summary *string) (*Session, error) {
	when := time.Now().UTC()
	if endedAt != nil {
		when = *endedAt
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET ended_at = $1,
		    summary  = COALESCE($2, summary)
		WHERE id = $3 AND team_id = $4
	`, when, summary, sessionID, id.TeamID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.GetSession(ctx, id, sessionID)
}

// GetSession returns one session belonging to the caller's team.
func (s *Store) GetSession(ctx context.Context, id Identity, sessionID string) (*Session, error) {
	var (
		sess Session
		meta []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, team_id, user_id, project, agent, started_at, ended_at,
		       summary, summarised_at, facts_extracted_at, meta
		FROM sessions
		WHERE id = $1 AND team_id = $2
	`, sessionID, id.TeamID).Scan(
		&sess.ID, &sess.TeamID, &sess.UserID, &sess.Project, &sess.Agent,
		&sess.StartedAt, &sess.EndedAt, &sess.Summary, &sess.SummarisedAt,
		&sess.FactsExtractedAt, &meta)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &sess.Meta)
	}
	return &sess, nil
}

// ListSessions returns recent sessions for the caller's team, newest first.
func (s *Store) ListSessions(ctx context.Context, id Identity, project string, limit int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, user_id, project, agent, started_at, ended_at,
		       summary, summarised_at, facts_extracted_at, meta
		FROM sessions
		WHERE team_id = $1
		  AND ($2 = '' OR project = $2)
		ORDER BY started_at DESC
		LIMIT $3
	`, id.TeamID, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			sess Session
			meta []byte
		)
		if err := rows.Scan(&sess.ID, &sess.TeamID, &sess.UserID, &sess.Project, &sess.Agent,
			&sess.StartedAt, &sess.EndedAt, &sess.Summary, &sess.SummarisedAt,
			&sess.FactsExtractedAt, &meta); err != nil {
			return nil, err
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &sess.Meta)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// --- observations ----------------------------------------------------------

// ObservationInput is one item of a batch ingest.
type ObservationInput struct {
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	Files   []string `json:"files,omitempty"`
}

// AddObservations writes a batch of observations to one session.
//
// Batched by design: hooks buffer events and send them in groups. One HTTP
// round trip and one transaction per tool call would put the agent's latency on
// the memory system's critical path, and the first thing anyone does with a
// memory system that slows down their editor is uninstall it.
//
// Redaction happens here, before the INSERT, so a credential read during a
// session never reaches a row, a WAL segment, or a backup.
func (s *Store) AddObservations(ctx context.Context, id Identity, sessionID string, items []ObservationInput) ([]Observation, error) {
	if len(items) == 0 {
		return nil, nil
	}

	// Confirm the session belongs to this team before writing anything to it.
	var project string
	err := s.pool.QueryRow(ctx,
		`SELECT project FROM sessions WHERE id = $1 AND team_id = $2`,
		sessionID, id.TeamID).Scan(&project)
	if err != nil {
		return nil, wrapNotFound(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out := make([]Observation, 0, len(items))
	for _, it := range items {
		if it.Kind == "" {
			it.Kind = "note"
		}

		content := it.Content
		var findings []redact.Finding
		if s.cfg.Security.RedactionEnabled {
			content, findings = redact.Apply(content)
		}
		findingsJSON, err := jsonOrEmpty(findings)
		if err != nil {
			return nil, err
		}

		files := it.Files
		if files == nil {
			files = []string{}
		}

		obs := Observation{
			ID:         ulid.New(),
			SessionID:  sessionID,
			TeamID:     id.TeamID,
			UserID:     id.UserID,
			Project:    project,
			Kind:       it.Kind,
			Content:    content,
			Files:      files,
			Redacted:   len(findings) > 0,
			Redactions: findings,
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO observations (id, session_id, team_id, user_id, project, kind, content, files, redacted, redactions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING created_at
		`, obs.ID, obs.SessionID, obs.TeamID, obs.UserID, obs.Project, obs.Kind,
			obs.Content, obs.Files, obs.Redacted, findingsJSON).Scan(&obs.CreatedAt); err != nil {
			return nil, fmt.Errorf("inserting observation: %w", err)
		}
		out = append(out, obs)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// ListObservations returns a session's observations in chronological order.
func (s *Store) ListObservations(ctx context.Context, id Identity, sessionID string, limit int) ([]Observation, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.session_id, o.team_id, o.user_id, o.project, o.kind,
		       o.content, o.files, o.redacted, o.redactions, o.created_at
		FROM observations o
		WHERE o.session_id = $1 AND o.team_id = $2
		ORDER BY o.created_at
		LIMIT $3
	`, sessionID, id.TeamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanObservations(rows)
}

// PurgeObservations deletes tier-0 rows older than the retention window.
//
// Consolidated memories are not touched: the raw stream is the expensive part
// to keep and the least useful to read, while the distilled tiers above it are
// the opposite.
func (s *Store) PurgeObservations(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM observations WHERE created_at < now() - $1::interval`,
		fmt.Sprintf("%d hours", int(olderThan.Hours())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanObservations(rows rowScanner) ([]Observation, error) {
	var out []Observation
	for rows.Next() {
		var (
			obs        Observation
			redactions []byte
		)
		if err := rows.Scan(&obs.ID, &obs.SessionID, &obs.TeamID, &obs.UserID, &obs.Project,
			&obs.Kind, &obs.Content, &obs.Files, &obs.Redacted, &redactions, &obs.CreatedAt); err != nil {
			return nil, err
		}
		if len(redactions) > 0 {
			_ = json.Unmarshal(redactions, &obs.Redactions)
		}
		out = append(out, obs)
	}
	return out, rows.Err()
}
