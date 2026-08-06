package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one numbered SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Applied describes a migration already recorded in the database.
type Applied struct {
	Version  int
	Name     string
	Checksum string
}

// LoadMigrations reads the embedded migrations, substituting the configured
// embedding dimension.
//
// File names are `NNNN_name.sql`. The number is the version and it is the only
// ordering that matters; alphabetical ordering of names is not relied on.
func LoadMigrations(embeddingDim int) ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numStr, rest, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q is not named NNNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %q is not a version number", e.Name(), numStr)
		}

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sqlText := strings.ReplaceAll(string(body), "{{EMBEDDING_DIM}}", strconv.Itoa(embeddingDim))

		sum := sha256.Sum256([]byte(sqlText))
		out = append(out, Migration{
			Version:  version,
			Name:     rest,
			SQL:      sqlText,
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("two migrations share version %d (%s and %s)", out[i].Version, out[i-1].Name, out[i].Name)
		}
	}
	return out, nil
}

// MigrateResult reports what a migration run did.
type MigrateResult struct {
	Applied []Migration
	Skipped int
	Current int
}

// Migrate brings the database up to the latest schema version.
//
// Forward-only and idempotent. Each migration runs inside its own transaction
// together with the row recording it, so a failure halfway through leaves the
// database at the last complete version rather than in a state no migration
// describes. Running it twice does nothing the second time.
func Migrate(ctx context.Context, pool *pgxpool.Pool, embeddingDim int) (*MigrateResult, error) {
	migrations, err := LoadMigrations(embeddingDim)
	if err != nil {
		return nil, err
	}

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer PRIMARY KEY,
			name        text        NOT NULL,
			checksum    text        NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	rows, err := pool.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	applied := map[int]Applied{}
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			rows.Close()
			return nil, err
		}
		applied[a.Version] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res := &MigrateResult{}
	for _, m := range migrations {
		if prev, ok := applied[m.Version]; ok {
			// A changed checksum means someone edited a migration that has
			// already run somewhere. Refuse: the database and the file no
			// longer describe the same schema, and applying the difference is
			// not something this runner can safely infer.
			if prev.Checksum != m.Checksum {
				return nil, fmt.Errorf(
					"migration %04d_%s has changed since it was applied\n"+
						"  recorded checksum: %s\n"+
						"  file checksum:     %s\n"+
						"Migrations are forward-only. Add a new migration instead of editing this one.\n"+
						"(If embedding.dimensions changed, that is a re-embed, not a config edit.)",
					m.Version, m.Name, prev.Checksum[:12], m.Checksum[:12])
			}
			res.Skipped++
			res.Current = m.Version
			continue
		}

		if err := applyOne(ctx, pool, m); err != nil {
			return nil, err
		}
		res.Applied = append(res.Applied, m)
		res.Current = m.Version
	}

	return res, nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m Migration) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("recording migration %04d_%s: %w", m.Version, m.Name, err)
	}
	return tx.Commit(ctx)
}

// SchemaVersion returns the highest applied migration version, or 0 when the
// database has never been migrated.
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var v *int
	err := pool.QueryRow(ctx, `
		SELECT max(version) FROM schema_migrations
	`).Scan(&v)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return 0, nil
		}
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

// LatestVersion is the highest version shipped in this binary.
func LatestVersion() int {
	ms, err := LoadMigrations(384)
	if err != nil || len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}
