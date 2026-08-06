package store

import (
	"context"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// SessionsAwaitingSummary returns closed sessions that have not been summarised.
//
// The work queue is a partial index on exactly this predicate, so the cost is
// proportional to the backlog rather than to every session ever recorded.
func (s *Store) SessionsAwaitingSummary(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, user_id, project, agent, started_at, ended_at,
		       summary, summarised_at, facts_extracted_at, meta
		FROM sessions
		WHERE ended_at IS NOT NULL AND summarised_at IS NULL
		ORDER BY ended_at
		LIMIT $1
	`, limit)
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
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SessionObservations returns a session's raw observations for the summariser,
// bypassing the per-caller visibility check because the worker runs as the
// system rather than as a user.
func (s *Store) SessionObservations(ctx context.Context, sessionID string, limit int) ([]Observation, error) {
	if limit <= 0 || limit > 2000 {
		limit = 400
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, team_id, user_id, project, kind, content, files, redacted, redactions, created_at
		FROM observations
		WHERE session_id = $1
		ORDER BY created_at
		LIMIT $2
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanObservations(rows)
}

// MarkSummarised records a tier-1 summary.
//
// summarised_at is set even when the summary is empty, so a session with
// nothing worth saying is not retried on every tick forever.
func (s *Store) MarkSummarised(ctx context.Context, sessionID, summary string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET summary = $2, summarised_at = now() WHERE id = $1
	`, sessionID, summary)
	return err
}

// SessionsAwaitingFacts returns summarised sessions whose facts have not been
// extracted, grouped by project by the caller.
func (s *Store) SessionsAwaitingFacts(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, user_id, project, agent, started_at, ended_at,
		       summary, summarised_at, facts_extracted_at, meta
		FROM sessions
		WHERE summarised_at IS NOT NULL AND facts_extracted_at IS NULL
		  AND summary IS NOT NULL AND summary <> ''
		ORDER BY team_id, project, ended_at
		LIMIT $1
	`, limit)
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
		out = append(out, sess)
	}
	return out, rows.Err()
}

// MarkFactsExtracted closes out tier 2 for a set of sessions.
func (s *Store) MarkFactsExtracted(ctx context.Context, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET facts_extracted_at = now() WHERE id = ANY($1)`, sessionIDs)
	return err
}

// RecentFacts returns facts and decisions for tier-3 lesson synthesis.
func (s *Store) RecentFacts(ctx context.Context, teamID, project string, since time.Time, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE m.team_id = $1 AND m.project = $2
		  AND m.kind IN ('fact', 'decision')
		  AND m.deleted_at IS NULL AND m.superseded_by IS NULL
		  AND m.created_at >= $3
		ORDER BY m.created_at DESC
		LIMIT $4
	`, teamID, project, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// AnyUserInTeam returns a user to attribute system-generated memories to.
//
// Consolidation output belongs to someone: an unowned row would be invisible to
// the visibility predicate, which requires either ownership or team visibility.
// The team's admin is preferred, falling back to the earliest member.
func (s *Store) AnyUserInTeam(ctx context.Context, teamID string) (*Identity, error) {
	var id Identity
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, team_id, is_admin FROM users
		WHERE team_id = $1
		ORDER BY is_admin DESC, created_at
		LIMIT 1
	`, teamID).Scan(&id.UserID, &id.UserName, &id.TeamID, &id.IsAdmin)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	return &id, nil
}

// StartRun opens a consolidation run record.
func (s *Store) StartRun(ctx context.Context, tier int, teamID, project string) (string, error) {
	id := ulid.New()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO consolidation_runs (id, tier, team_id, project) VALUES ($1, $2, $3, $4)
	`, id, tier, nullIfEmpty(teamID), nullIfEmpty(project))
	return id, err
}

// FinishRun closes a run and records what it cost.
//
// Token counts are stored rather than only logged, because "why did the bill go
// up" is answerable from a table and not from yesterday's rotated logs.
func (s *Store) FinishRun(ctx context.Context, runID string, items, produced, deduped, inTok, outTok int, runErr error) error {
	var errStr *string
	if runErr != nil {
		msg := runErr.Error()
		errStr = &msg
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE consolidation_runs
		SET finished_at = now(), items = $2, produced = $3, deduped = $4,
		    input_tokens = $5, output_tokens = $6, error = $7
		WHERE id = $1
	`, runID, items, produced, deduped, inTok, outTok, errStr)
	return err
}

// ListRuns returns recent consolidation runs for the health endpoint and viewer.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]ConsolidationRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, tier, team_id, project, started_at, finished_at,
		       items, produced, deduped, input_tokens, output_tokens, error
		FROM consolidation_runs
		ORDER BY started_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConsolidationRun
	for rows.Next() {
		var (
			r             ConsolidationRun
			team, project *string
		)
		if err := rows.Scan(&r.ID, &r.Tier, &team, &project, &r.StartedAt, &r.FinishedAt,
			&r.Items, &r.Produced, &r.Deduped, &r.InputTokens, &r.OutputTokens, &r.Error); err != nil {
			return nil, err
		}
		r.TeamID, r.Project = deref(team), deref(project)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TokenSpend totals consolidation token usage since a point in time.
func (s *Store) TokenSpend(ctx context.Context, since time.Time) (in, out int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0)
		FROM consolidation_runs WHERE started_at >= $1
	`, since).Scan(&in, &out)
	return
}
