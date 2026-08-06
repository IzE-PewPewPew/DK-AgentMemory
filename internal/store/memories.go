package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// memoryCols is the projection every memory read uses. One list, so a column
// added to the table cannot be silently missing from half the reads.
const memoryCols = `m.id, m.team_id, m.user_id, m.project, m.kind, m.title, m.body,
	m.files, m.visibility, m.strength, m.hits, m.source, m.session_id,
	m.redacted, m.redactions, m.superseded_by, m.deleted_at,
	m.created_at, m.updated_at, m.last_used_at`

// visibleClause is the tenancy predicate.
//
// It is a string constant spliced into every memory query rather than a helper
// the caller may forget to invoke. $1 is the team, $2 the user. A memory is
// visible if it belongs to the caller's team AND is either theirs or shared
// with the team. Cross-team reads are not "checked and rejected" here, they are
// unexpressible.
const visibleClause = `m.team_id = $1 AND (m.user_id = $2 OR m.visibility = 'team')`

// MemoryInput is a request to store a memory.
type MemoryInput struct {
	// ID, when set, must be a valid ULID. Offline clients generate it so the
	// same queued write flushed twice lands on the same primary key and the
	// second attempt is a no-op rather than a duplicate.
	ID string `json:"id,omitempty"`

	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Project    string   `json:"project,omitempty"`
	Files      []string `json:"files,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	Source     string   `json:"source,omitempty"`
	SessionID  *string  `json:"session_id,omitempty"`

	// Embedding is optional. A memory without one is still searchable by BM25,
	// so an embedder outage degrades recall rather than rejecting writes.
	Embedding []float32 `json:"-"`
}

// Validate checks an input before it reaches SQL.
func (in *MemoryInput) Validate() error {
	if in.ID != "" {
		if err := ulid.Validate(in.ID); err != nil {
			return fmt.Errorf("id: %w", err)
		}
	}
	if in.Kind == "" {
		in.Kind = KindFact
	}
	if !ValidKind(in.Kind) {
		return fmt.Errorf("kind: %q is not one of fact, decision, lesson, preference", in.Kind)
	}
	if in.Visibility == "" {
		in.Visibility = VisibilityPrivate
	}
	if !ValidVisibility(in.Visibility) {
		return fmt.Errorf("visibility: %q is not private or team", in.Visibility)
	}
	if in.Source == "" {
		in.Source = SourceManual
	}
	if strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Body) == "" {
		return errors.New("a memory needs a title or a body")
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = firstLine(in.Body, 120)
	}
	return nil
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		// Cut on a word boundary where one is close, so titles do not end
		// mid-identifier.
		cut := strings.LastIndexByte(s[:max], ' ')
		if cut < max/2 {
			cut = max
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

// CreateMemory stores a memory and reports whether it was newly created.
//
// created=false means an identical memory already existed. That is the normal
// outcome of re-importing a transcript or flushing a queue twice, so it is a
// success with a flag rather than an error: callers that care can react, and
// callers that do not are still correct.
func (s *Store) CreateMemory(ctx context.Context, id Identity, in MemoryInput) (mem *Memory, created bool, err error) {
	if err := in.Validate(); err != nil {
		return nil, false, err
	}

	title, body := in.Title, in.Body
	var findings []redact.Finding
	if s.cfg.Security.RedactionEnabled {
		var tf, bf []redact.Finding
		title, tf = redact.Apply(title)
		body, bf = redact.Apply(body)
		findings = append(append(findings, tf...), bf...)
	}
	findingsJSON, err := jsonOrEmpty(findings)
	if err != nil {
		return nil, false, err
	}

	memID := in.ID
	if memID == "" {
		memID = ulid.New()
	}
	files := in.Files
	if files == nil {
		files = []string{}
	}
	hash := ContentHash(title, body)

	var embedding *string
	if len(in.Embedding) > 0 {
		lit := vectorLiteral(in.Embedding)
		embedding = &lit
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO memories (
			id, team_id, user_id, project, kind, title, body, files,
			visibility, source, session_id, content_hash, redacted, redactions, embedding
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::vector)
		ON CONFLICT DO NOTHING
		RETURNING `+strings.ReplaceAll(memoryCols, "m.", "")+`
	`, memID, id.TeamID, id.UserID, in.Project, in.Kind, title, body, files,
		in.Visibility, in.Source, in.SessionID, hash, len(findings) > 0, findingsJSON, embedding)

	mem, err = scanMemory(row)
	if err == nil {
		return mem, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	// Conflict. Either this exact ID already landed (a re-flushed offline
	// write) or an identical memory exists (a re-import).
	if existing, err := s.getMemoryUnscoped(ctx, memID); err == nil {
		return existing, false, nil
	}
	existing, err := s.findByContentHash(ctx, id, in.Project, hash)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (s *Store) getMemoryUnscoped(ctx context.Context, memID string) (*Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx,
		`SELECT `+memoryCols+` FROM memories m WHERE m.id = $1`, memID))
}

func (s *Store) findByContentHash(ctx context.Context, id Identity, project, hash string) (*Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE m.team_id = $1 AND m.user_id = $2 AND m.project = $3
		  AND m.content_hash = $4 AND m.deleted_at IS NULL
	`, id.TeamID, id.UserID, project, hash))
}

// GetMemory returns one memory the caller may see.
func (s *Store) GetMemory(ctx context.Context, id Identity, memID string) (*Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE `+visibleClause+` AND m.id = $3 AND m.deleted_at IS NULL
	`, id.TeamID, id.UserID, memID))
}

// ListFilter narrows a memory listing.
type ListFilter struct {
	Project string
	Kinds   []string
	// IncludeSuperseded returns rows that a newer memory has replaced. Off by
	// default: superseded rows are history, and history in a default listing is
	// how a store looks wrong.
	IncludeSuperseded bool
	// MineOnly excludes team-shared memories belonging to other users.
	MineOnly bool
	Limit    int
	// Cursor is the last ID of the previous page. Ordering is by ID descending,
	// and IDs are ULIDs, so this is both stable and index-backed. Offset
	// pagination is not offered: it skips rows when the set changes underneath.
	Cursor string
}

// ListMemories returns a page of memories plus the cursor for the next page.
func (s *Store) ListMemories(ctx context.Context, id Identity, f ListFilter) ([]Memory, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	q := strings.Builder{}
	q.WriteString(`SELECT ` + memoryCols + ` FROM memories m WHERE ` + visibleClause + ` AND m.deleted_at IS NULL`)
	args := []any{id.TeamID, id.UserID}

	if f.Project != "" {
		args = append(args, f.Project)
		fmt.Fprintf(&q, " AND m.project = $%d", len(args))
	}
	if len(f.Kinds) > 0 {
		args = append(args, f.Kinds)
		fmt.Fprintf(&q, " AND m.kind = ANY($%d)", len(args))
	}
	if !f.IncludeSuperseded {
		q.WriteString(" AND m.superseded_by IS NULL")
	}
	if f.MineOnly {
		q.WriteString(" AND m.user_id = $2")
	}
	if f.Cursor != "" {
		if err := ulid.Validate(f.Cursor); err != nil {
			return nil, "", fmt.Errorf("cursor: %w", err)
		}
		args = append(args, f.Cursor)
		fmt.Fprintf(&q, " AND m.id < $%d", len(args))
	}

	args = append(args, limit+1) // one extra row tells us whether more exist
	fmt.Fprintf(&q, " ORDER BY m.id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out, err := scanMemories(rows)
	if err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// UpdateMemory changes the mutable fields of a memory the caller owns.
//
// Only the owner may edit. A teammate who disagrees supersedes instead, which
// keeps both statements and the order they were made in.
func (s *Store) UpdateMemory(ctx context.Context, id Identity, memID string, title, body, visibility *string) (*Memory, error) {
	if visibility != nil && !ValidVisibility(*visibility) {
		return nil, fmt.Errorf("visibility: %q is not private or team", *visibility)
	}

	// Read first, then write. The content hash must be computed by the same
	// code that computes it on insert -- reproducing the normalisation in SQL
	// would be a second implementation of the dedup key, and the two would
	// drift on the first edge case either one handled differently.
	current, err := s.GetMemory(ctx, id, memID)
	if err != nil {
		return nil, err
	}
	if current.UserID != id.UserID {
		return nil, ErrForbidden
	}

	newTitle, newBody := current.Title, current.Body
	if title != nil {
		newTitle = *title
		if s.cfg.Security.RedactionEnabled {
			newTitle = redact.Clean(newTitle)
		}
	}
	if body != nil {
		newBody = *body
		if s.cfg.Security.RedactionEnabled {
			newBody = redact.Clean(newBody)
		}
	}
	newVisibility := current.Visibility
	if visibility != nil {
		newVisibility = *visibility
	}

	// Dropping the embedding when the body changes queues the row for re-embed
	// on the next backfill pass. Leaving the old vector in place would keep
	// serving semantic hits for text that no longer exists.
	bodyChanged := newBody != current.Body

	row := s.pool.QueryRow(ctx, `
		UPDATE memories m SET
			title        = $4,
			body         = $5,
			visibility   = $6,
			content_hash = $7,
			updated_at   = now(),
			embedding    = CASE WHEN $8 THEN NULL ELSE m.embedding END
		WHERE m.id = $3 AND m.team_id = $1 AND m.user_id = $2 AND m.deleted_at IS NULL
		RETURNING `+strings.ReplaceAll(memoryCols, "m.", "")+`
	`, id.TeamID, id.UserID, memID, newTitle, newBody, newVisibility,
		ContentHash(newTitle, newBody), bodyChanged)

	mem, err := scanMemory(row)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: an identical memory already exists in this project", ErrConflict)
	}
	return mem, err
}

// DeleteMemory soft-deletes a memory the caller owns.
//
// Soft, because a hard delete of a row another memory supersedes would break
// the chain, and because "I did not mean to forget that" is a recoverable
// mistake only while the row still exists.
func (s *Store) DeleteMemory(ctx context.Context, id Identity, memID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND team_id = $2 AND user_id = $3 AND deleted_at IS NULL
	`, memID, id.TeamID, id.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Supersede marks oldID as replaced by newID.
//
// Both must be visible to the caller and in the same team. The old row stays
// readable through its ID and disappears from default listings and search.
func (s *Store) Supersede(ctx context.Context, id Identity, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("a memory cannot supersede itself")
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT true FROM memories m WHERE `+visibleClause+` AND m.id = $3 AND m.deleted_at IS NULL
	`, id.TeamID, id.UserID, newID).Scan(&exists); err != nil {
		return fmt.Errorf("replacement memory %s: %w", newID, wrapNotFound(err))
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE memories SET superseded_by = $1, updated_at = now()
		WHERE id = $2 AND team_id = $3 AND user_id = $4 AND deleted_at IS NULL
	`, newID, oldID, id.TeamID, id.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Reinforce raises a memory's strength and records the use.
//
// Growth is sub-linear: strength approaches a ceiling of 5 rather than
// compounding. A memory retrieved a thousand times should outrank one retrieved
// twice, but not by a factor that makes every other result unreachable.
func (s *Store) Reinforce(ctx context.Context, id Identity, memID string) (*Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx, `
		UPDATE memories m SET
			strength     = LEAST(5.0, m.strength + (5.0 - m.strength) * 0.15),
			hits         = m.hits + 1,
			last_used_at = now(),
			updated_at   = now()
		WHERE `+visibleClause+` AND m.id = $3 AND m.deleted_at IS NULL
		RETURNING `+strings.ReplaceAll(memoryCols, "m.", "")+`
	`, id.TeamID, id.UserID, memID))
}

// Share promotes a private memory to team visibility.
func (s *Store) Share(ctx context.Context, id Identity, memID string) (*Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx, `
		UPDATE memories m SET visibility = 'team', updated_at = now()
		WHERE m.id = $3 AND m.team_id = $1 AND m.user_id = $2 AND m.deleted_at IS NULL
		RETURNING `+strings.ReplaceAll(memoryCols, "m.", "")+`
	`, id.TeamID, id.UserID, memID))
}

// Feed returns recently shared team memories, excluding the caller's own.
func (s *Store) Feed(ctx context.Context, id Identity, project string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE m.team_id = $1 AND m.visibility = 'team' AND m.user_id <> $2
		  AND m.deleted_at IS NULL AND m.superseded_by IS NULL
		  AND ($3 = '' OR m.project = $3)
		ORDER BY m.updated_at DESC
		LIMIT $4
	`, id.TeamID, id.UserID, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// Lessons returns active lessons, strongest first.
func (s *Store) Lessons(ctx context.Context, id Identity, project string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE `+visibleClause+` AND m.kind = 'lesson'
		  AND m.deleted_at IS NULL AND m.superseded_by IS NULL
		  AND ($3 = '' OR m.project = $3 OR m.project = '')
		ORDER BY m.strength DESC, m.updated_at DESC
		LIMIT $4
	`, id.TeamID, id.UserID, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// Projects lists the distinct projects visible to the caller with counts.
type ProjectSummary struct {
	Project  string    `json:"project"`
	Memories int64     `json:"memories"`
	Sessions int64     `json:"sessions"`
	LastSeen time.Time `json:"last_seen"`
}

// Projects returns per-project counts, for `dkm doctor` and the viewer.
func (s *Store) Projects(ctx context.Context, id Identity) ([]ProjectSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.project,
		       count(*) AS memories,
		       (SELECT count(*) FROM sessions sx WHERE sx.team_id = $1 AND sx.project = m.project),
		       max(m.updated_at)
		FROM memories m
		WHERE `+visibleClause+` AND m.deleted_at IS NULL
		GROUP BY m.project
		ORDER BY max(m.updated_at) DESC
	`, id.TeamID, id.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.Project, &p.Memories, &p.Sessions, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountMemories returns visible totals split by ownership, for `dkm doctor`.
func (s *Store) CountMemories(ctx context.Context, id Identity) (total, team, private int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE m.visibility = 'team'),
		       count(*) FILTER (WHERE m.visibility = 'private')
		FROM memories m
		WHERE `+visibleClause+` AND m.deleted_at IS NULL
	`, id.TeamID, id.UserID).Scan(&total, &team, &private)
	return
}

// --- embeddings ------------------------------------------------------------

// SetEmbedding attaches a vector to a memory.
func (s *Store) SetEmbedding(ctx context.Context, memID string, vec []float32) error {
	if len(vec) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE memories SET embedding = $2::vector WHERE id = $1`, memID, vectorLiteral(vec))
	return err
}

// PendingEmbeddings returns memories with no vector yet.
//
// Writes never block on the embedder. Anything it could not embed at write time
// is picked up here, so an embedder that was down for an hour costs an hour of
// degraded semantic recall rather than an hour of rejected writes.
func (s *Store) PendingEmbeddings(ctx context.Context, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE m.embedding IS NULL AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// --- decay -----------------------------------------------------------------

// Decay reduces the strength of memories that have not been retrieved.
//
// Applied on a schedule with a half-life measured in months. Without it, an
// early wrong guess outranks a later correction forever, because the only thing
// that ever raised a score was having existed first.
func (s *Store) Decay(ctx context.Context, halfLifeDays int) (int64, error) {
	if halfLifeDays < 1 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories SET strength = GREATEST(0.1, strength * power(0.5, 1.0 / $1))
		WHERE deleted_at IS NULL
		  AND COALESCE(last_used_at, created_at) < now() - interval '1 day'
	`, float64(halfLifeDays))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- sync ------------------------------------------------------------------

// Change is one row in the incremental sync stream.
type Change struct {
	Memory
	Deleted bool `json:"deleted"`
}

// Changes returns memories updated after the given cursor.
//
// The cursor is "<RFC3339Nano>|<id>", compared as a row so ties on the timestamp
// break deterministically by ID. A cursor made of a timestamp alone loses rows
// written in the same microsecond.
func (s *Store) Changes(ctx context.Context, id Identity, cursor string, limit int) ([]Change, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	since := time.Unix(0, 0).UTC()
	sinceID := ""
	if cursor != "" {
		tsPart, idPart, ok := strings.Cut(cursor, "|")
		if !ok {
			return nil, "", fmt.Errorf("cursor: expected <timestamp>|<id>, got %q", cursor)
		}
		t, err := time.Parse(time.RFC3339Nano, tsPart)
		if err != nil {
			return nil, "", fmt.Errorf("cursor: %q is not an RFC3339 timestamp", tsPart)
		}
		since, sinceID = t, idPart
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+` FROM memories m
		WHERE `+visibleClause+`
		  AND (m.updated_at, m.id) > ($3, $4)
		ORDER BY m.updated_at, m.id
		LIMIT $5
	`, id.TeamID, id.UserID, since, sinceID, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	mems, err := scanMemories(rows)
	if err != nil {
		return nil, "", err
	}

	out := make([]Change, 0, len(mems))
	for _, m := range mems {
		out = append(out, Change{Memory: m, Deleted: m.DeletedAt != nil})
	}

	next := cursor
	if len(mems) > 0 {
		last := mems[len(mems)-1]
		next = last.UpdatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return out, next, nil
}

// --- scanning --------------------------------------------------------------

type singleRow interface{ Scan(dest ...any) error }

func scanMemory(row singleRow) (*Memory, error) {
	var (
		m          Memory
		redactions []byte
	)
	err := row.Scan(&m.ID, &m.TeamID, &m.UserID, &m.Project, &m.Kind, &m.Title, &m.Body,
		&m.Files, &m.Visibility, &m.Strength, &m.Hits, &m.Source, &m.SessionID,
		&m.Redacted, &redactions, &m.SupersededBy, &m.DeletedAt,
		&m.CreatedAt, &m.UpdatedAt, &m.LastUsedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(redactions) > 0 {
		_ = json.Unmarshal(redactions, &m.Redactions)
	}
	return &m, nil
}

func scanMemories(rows rowScanner) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
