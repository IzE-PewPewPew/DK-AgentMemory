package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

func unmarshalMap(b []byte, dst *map[string]any) error { return json.Unmarshal(b, dst) }

// RebuildGraph derives nodes and edges from co-occurrence already present in the
// corpus.
//
// Nothing here is inferred by a model. A file node exists because a memory
// names that file; an edge exists because two files were named by the same
// memory or touched in the same session. The consequence is that a small corpus
// produces a small graph and an empty corpus produces an empty one -- which is
// correct, and better than a populated-looking graph of relationships nobody
// can trace back to anything.
func (s *Store) RebuildGraph(ctx context.Context, teamID, project string) (nodes, edges int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Rebuild is idempotent: clear this project's derived rows first, so a
	// second run produces the same graph rather than doubled weights.
	if _, err := tx.Exec(ctx,
		`DELETE FROM graph_nodes WHERE team_id = $1 AND project = $2`, teamID, project); err != nil {
		return 0, 0, err
	}

	// File nodes, weighted by how many memories mention each file.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_nodes (id, team_id, project, kind, label, weight)
		SELECT encode(sha256(convert_to($1 || $2 || 'file' || f, 'UTF8')), 'hex'),
		       $1, $2, 'file', f, count(*)::real
		FROM memories m, unnest(m.files) AS f
		WHERE m.team_id = $1 AND m.project = $2 AND m.deleted_at IS NULL AND f <> ''
		GROUP BY f
		ON CONFLICT (team_id, project, kind, label) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building file nodes: %w", err)
	}

	// Memory nodes, but only for memories that connect to something. An
	// isolated memory adds a dot to the picture and no information.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_nodes (id, team_id, project, kind, label, weight)
		SELECT encode(sha256(convert_to($1 || $2 || 'memory' || m.id, 'UTF8')), 'hex'),
		       $1, $2, 'memory', m.id, GREATEST(m.strength, 0.1)
		FROM memories m
		WHERE m.team_id = $1 AND m.project = $2 AND m.deleted_at IS NULL
		  AND (array_length(m.files, 1) > 0 OR m.session_id IS NOT NULL)
		ON CONFLICT (team_id, project, kind, label) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building memory nodes: %w", err)
	}

	// memory -> file: the memory names the file.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_edges (id, team_id, project, src, dst, rel, weight)
		SELECT encode(sha256(convert_to(mn.id || fn.id || 'touches', 'UTF8')), 'hex'),
		       $1, $2, mn.id, fn.id, 'touches', 1
		FROM memories m
		JOIN graph_nodes mn ON mn.team_id = $1 AND mn.project = $2 AND mn.kind = 'memory' AND mn.label = m.id
		CROSS JOIN LATERAL unnest(m.files) AS f
		JOIN graph_nodes fn ON fn.team_id = $1 AND fn.project = $2 AND fn.kind = 'file' AND fn.label = f
		WHERE m.team_id = $1 AND m.project = $2 AND m.deleted_at IS NULL
		ON CONFLICT (src, dst, rel) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building touches edges: %w", err)
	}

	// file <-> file: both named by the same memory. Weight is how many memories
	// name both, which is the number of times the pair actually co-occurred.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_edges (id, team_id, project, src, dst, rel, weight)
		SELECT encode(sha256(convert_to(a.id || b.id || 'co_occurs', 'UTF8')), 'hex'),
		       $1, $2, a.id, b.id, 'co_occurs', count(*)::real
		FROM memories m
		CROSS JOIN LATERAL unnest(m.files) AS f1
		CROSS JOIN LATERAL unnest(m.files) AS f2
		JOIN graph_nodes a ON a.team_id = $1 AND a.project = $2 AND a.kind = 'file' AND a.label = f1
		JOIN graph_nodes b ON b.team_id = $1 AND b.project = $2 AND b.kind = 'file' AND b.label = f2
		WHERE m.team_id = $1 AND m.project = $2 AND m.deleted_at IS NULL AND f1 < f2
		GROUP BY a.id, b.id
		ON CONFLICT (src, dst, rel) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building co-occurrence edges: %w", err)
	}

	// memory <-> memory: produced in the same session.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_edges (id, team_id, project, src, dst, rel, weight)
		SELECT encode(sha256(convert_to(a.id || b.id || 'same_session', 'UTF8')), 'hex'),
		       $1, $2, a.id, b.id, 'same_session', 1
		FROM memories m1
		JOIN memories m2 ON m2.session_id = m1.session_id AND m2.id > m1.id
		JOIN graph_nodes a ON a.team_id = $1 AND a.project = $2 AND a.kind = 'memory' AND a.label = m1.id
		JOIN graph_nodes b ON b.team_id = $1 AND b.project = $2 AND b.kind = 'memory' AND b.label = m2.id
		WHERE m1.team_id = $1 AND m1.project = $2 AND m1.session_id IS NOT NULL
		  AND m1.deleted_at IS NULL AND m2.deleted_at IS NULL
		ON CONFLICT (src, dst, rel) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building session edges: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM graph_nodes WHERE team_id = $1 AND project = $2`,
		teamID, project).Scan(&nodes); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM graph_edges WHERE team_id = $1 AND project = $2`,
		teamID, project).Scan(&edges); err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return nodes, edges, nil
}

// GetGraph returns a project's graph, optionally limited to what is reachable
// from a seed label within `depth` hops.
func (s *Store) GetGraph(ctx context.Context, id Identity, project, seed string, depth, limit int) (*Graph, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	if depth <= 0 {
		depth = 2
	}
	if depth > 6 {
		depth = 6
	}

	g := &Graph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}

	var nodeRows rowScanner
	if seed == "" {
		rows, err := s.pool.Query(ctx, `
			SELECT id, team_id, project, kind, label, weight
			FROM graph_nodes
			WHERE team_id = $1 AND project = $2
			ORDER BY weight DESC
			LIMIT $3
		`, id.TeamID, project, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		nodeRows = rows
	} else {
		// Breadth-limited traversal. The recursive term walks edges in both
		// directions because co-occurrence has no natural direction.
		rows, err := s.pool.Query(ctx, `
			WITH RECURSIVE reachable(id, hops) AS (
				SELECT n.id, 0
				FROM graph_nodes n
				WHERE n.team_id = $1 AND n.project = $2 AND n.label = $3
				UNION
				SELECT CASE WHEN e.src = r.id THEN e.dst ELSE e.src END, r.hops + 1
				FROM reachable r
				JOIN graph_edges e ON (e.src = r.id OR e.dst = r.id)
				WHERE r.hops < $4
			)
			SELECT DISTINCT n.id, n.team_id, n.project, n.kind, n.label, n.weight
			FROM reachable r
			JOIN graph_nodes n ON n.id = r.id
			ORDER BY n.weight DESC
			LIMIT $5
		`, id.TeamID, project, seed, depth, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		nodeRows = rows
	}

	ids := make([]string, 0, limit)
	for nodeRows.Next() {
		var n GraphNode
		if err := nodeRows.Scan(&n.ID, &n.TeamID, &n.Project, &n.Kind, &n.Label, &n.Weight); err != nil {
			return nil, err
		}
		g.Nodes = append(g.Nodes, n)
		ids = append(ids, n.ID)
	}
	if err := nodeRows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return g, nil
	}

	edgeRows, err := s.pool.Query(ctx, `
		SELECT id, src, dst, rel, weight
		FROM graph_edges
		WHERE team_id = $1 AND project = $2 AND src = ANY($3) AND dst = ANY($3)
		ORDER BY weight DESC
		LIMIT $4
	`, id.TeamID, project, ids, limit*4)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var e GraphEdge
		if err := edgeRows.Scan(&e.ID, &e.Src, &e.Dst, &e.Rel, &e.Weight); err != nil {
			return nil, err
		}
		g.Edges = append(g.Edges, e)
	}
	return g, edgeRows.Err()
}

// ProjectsForGraph lists team/project pairs that have memories, so the
// scheduled rebuild knows what to walk.
func (s *Store) ProjectsForGraph(ctx context.Context) ([][2]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT team_id, project FROM memories WHERE deleted_at IS NULL AND project <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var team, project string
		if err := rows.Scan(&team, &project); err != nil {
			return nil, err
		}
		out = append(out, [2]string{team, project})
	}
	return out, rows.Err()
}

// newNodeID is used by importers that build nodes outside the SQL path.
func newNodeID() string { return ulid.New() }
