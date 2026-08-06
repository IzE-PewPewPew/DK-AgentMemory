// Package store is the only package that speaks SQL.
//
// Two rules hold throughout:
//
//   - No ORM. Every statement is hand-written, so the query plan of the search
//     path is something a person can read and reason about.
//   - Tenant scoping lives in the SQL, not in the caller. Every query that
//     touches user data carries `team_id = $n` and a visibility predicate in
//     its WHERE clause. An application-layer check gets forgotten when someone
//     adds an endpoint six months from now; a predicate compiled into the only
//     query that can reach the table does not.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
)

// Store owns the connection pool.
type Store struct {
	pool *pgxpool.Pool
	cfg  *config.Config
}

// Sentinel errors. Handlers map these to status codes; nothing else in the
// codebase inspects driver error strings.
var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists")
	ErrForbidden = errors.New("forbidden")
)

// Open connects to Postgres and verifies the connection.
//
// A failure here stops the process. Starting with an unreachable database and
// serving 500s is worse than not starting: a health check that never goes green
// is a clearer signal than an endpoint that returns errors with a 200-shaped
// deployment behind it.
func Open(ctx context.Context, cfg *config.Config) (*Store, error) {
	pc, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("database.url is not a valid connection string: %w", err)
	}
	pc.MaxConns = int32(cfg.Database.MaxConns)
	pc.MaxConnLifetime = time.Hour
	pc.MaxConnIdleTime = 30 * time.Minute
	pc.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connecting to Postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("Postgres is unreachable at %s: %w", redactDSN(cfg.Database.URL), err)
	}

	return &Store{pool: pool, cfg: cfg}, nil
}

// NewWithPool wraps an existing pool. Tests use it; the server uses Open.
func NewWithPool(pool *pgxpool.Pool, cfg *config.Config) *Store {
	return &Store{pool: pool, cfg: cfg}
}

// Pool exposes the underlying pool for migrations and integration tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Config returns the configuration the store was opened with.
func (s *Store) Config() *config.Config { return s.cfg }

// Close releases every connection.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies pending migrations.
func (s *Store) Migrate(ctx context.Context) (*MigrateResult, error) {
	return Migrate(ctx, s.pool, s.cfg.Embedding.Dimensions)
}

// SchemaVersion returns the currently applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return SchemaVersion(ctx, s.pool)
}

// Stats is the deep health information reported by /v1/healthz.
type Stats struct {
	SchemaVersion int   `json:"schema_version"`
	Teams         int64 `json:"teams"`
	Users         int64 `json:"users"`
	Memories      int64 `json:"memories"`
	Sessions      int64 `json:"sessions"`
	Observations  int64 `json:"observations"`
	Embedded      int64 `json:"memories_embedded"`
}

// Stats collects row counts for the health endpoint.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM teams),
		       (SELECT count(*) FROM users),
		       (SELECT count(*) FROM memories WHERE deleted_at IS NULL),
		       (SELECT count(*) FROM sessions),
		       (SELECT count(*) FROM observations),
		       (SELECT count(*) FROM memories WHERE embedding IS NOT NULL AND deleted_at IS NULL)
	`).Scan(&st.Teams, &st.Users, &st.Memories, &st.Sessions, &st.Observations, &st.Embedded)
	if err != nil {
		return nil, err
	}
	if st.SchemaVersion, err = SchemaVersion(ctx, s.pool); err != nil {
		return nil, err
	}
	return &st, nil
}

// --- helpers ---------------------------------------------------------------

// ContentHash is the exact-duplicate key: sha256 over case-folded,
// whitespace-collapsed title and body.
//
// Normalising before hashing means "Use Node 20" and "use node 20 " collapse to
// one row. Hashing the raw text instead would let trivial reformatting defeat
// the unique index, which is how a store fills with the same fact fifteen times.
func ContentHash(title, body string) string {
	h := sha256.New()
	h.Write([]byte(normalise(title)))
	h.Write([]byte("\x00"))
	h.Write([]byte(normalise(body)))
	return hex.EncodeToString(h.Sum(nil))
}

func normalise(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether err is a Postgres FK error.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// wrapNotFound converts pgx's no-rows sentinel into the package one.
func wrapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// redactDSN removes the password from a connection string so it can appear in
// an error message. Connection failures are the errors most likely to be pasted
// into an issue.
func redactDSN(dsn string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return "the configured database"
	}
	creds, host, ok := strings.Cut(rest, "@")
	if !ok {
		return scheme + "://" + rest
	}
	user, _, hasPassword := strings.Cut(creds, ":")
	if !hasPassword {
		return scheme + "://" + creds + "@" + host
	}
	return scheme + "://" + user + ":***@" + host
}

// vectorLiteral renders a float slice as a pgvector literal. pgvector's text
// input is `[1,2,3]`; this avoids a driver-specific type registration for the
// two queries that need it.
func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}

func jsonOrEmpty(v any) ([]byte, error) {
	if v == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "null" {
		return []byte("[]"), nil
	}
	return b, nil
}

func jsonObjectOrEmpty(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "null" {
		return []byte("{}"), nil
	}
	return b, nil
}
