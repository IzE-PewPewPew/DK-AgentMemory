package store

import (
	"context"
	"encoding/json"
	"fmt"
)

func unmarshalMap(b []byte, dst *map[string]any) error { return json.Unmarshal(b, dst) }

// coEditWindow is how many consecutive file-touching observations count as
// "worked on together".
//
// The unit has to be smaller than a session. Whole-session co-occurrence makes
// every file a session touched adjacent to every other, which on one project
// here means 683 files at 85% density -- a near-complete graph, which carries
// no information and cannot be drawn. A window of three turns that into 3,657
// edges at an average degree of 10.8, and the strongest pairs are real working
// sets: a component and its stylesheet, a loader and the obfuscator it feeds.
//
// Three, not two, because an edit rarely alternates strictly between two files;
// and not ten, because by then the window spans unrelated work.
const coEditWindow = 3

// graphNodeCap bounds file nodes per project. Non-binding on this corpus -- the
// largest project has 683 distinct files -- and present only so that a
// pathologically large repository degrades to its most-touched files instead of
// trying to draw everything.
const graphNodeCap = 1500

// RebuildGraph derives nodes and edges from co-occurrence already present in the
// corpus.
//
// Nothing here is inferred by a model. A file node exists because a memory or an
// observation names that file; an edge exists because two files were named by
// the same memory, or were touched within a few steps of each other inside one
// session. The consequence is that a small corpus produces a small graph and an
// empty corpus produces an empty one -- which is correct, and better than a
// populated-looking graph of relationships nobody can trace back to anything.
//
// Observations are the important source. Memories only exist after the
// consolidation pipeline has run, so a project that has been imported but not
// yet consolidated has thousands of observations and no memories -- and drawing
// only from memories rendered eight of ten real projects as an empty canvas.
func (s *Store) RebuildGraph(ctx context.Context, teamID, project string) (nodes, edges int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise rebuilds of the same project. The scheduled worker and the
	// viewer's rebuild button can both land here; while a rebuild took 50ms
	// they never collided, but reading observations makes it hundreds of
	// milliseconds and the race becomes routine. Released on commit or
	// rollback, so there is nothing to clean up.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1 || '/' || $2))`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("locking the project: %w", err)
	}

	// Rebuild is idempotent: clear this project's derived rows first, so a
	// second run produces the same graph rather than doubled weights. Edges go
	// with them, by ON DELETE CASCADE from graph_nodes -- deleting them
	// separately is redundant and slower.
	if _, err := tx.Exec(ctx,
		`DELETE FROM graph_nodes WHERE team_id = $1 AND project = $2`, teamID, project); err != nil {
		return 0, 0, err
	}

	// File nodes from BOTH sources in one statement, so the weights sum.
	//
	// Written as a single UNION ALL rather than two inserts because the second
	// insert would have to reconcile with the first through
	// ON CONFLICT DO NOTHING -- and that silently keeps whichever weight landed
	// first. A file named by 2 memories and 307 observations would carry weight
	// 2, and since the renderer takes the heaviest nodes, the most important
	// files in the project would be the ones dropped.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_nodes (id, team_id, project, kind, label, weight)
		SELECT encode(sha256(convert_to($1 || $2 || 'file' || label, 'UTF8')), 'hex'),
		       $1, $2, 'file', label, w
		FROM (
			SELECT label, sum(w)::real AS w
			FROM (
				SELECT f AS label, count(*)::real AS w
				FROM memories m
				CROSS JOIN LATERAL unnest(m.files) AS f
				WHERE m.team_id = $1 AND m.project = $2
				  AND m.deleted_at IS NULL AND f <> ''
				GROUP BY f
				UNION ALL
				SELECT uf.f, count(*)::real
				FROM observations o
				CROSS JOIN LATERAL unnest(o.files) AS uf(f)
				WHERE o.team_id = $1 AND o.project = $2 AND uf.f <> ''
				GROUP BY uf.f
			) src
			GROUP BY label
			ORDER BY sum(w) DESC, label
			LIMIT $3
		) top_files
		ON CONFLICT (team_id, project, kind, label) DO NOTHING
	`, teamID, project, graphNodeCap); err != nil {
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
	//
	// The team/project predicate on m2 is explicit rather than implied. It was
	// previously enforced only by the graph_nodes joins below, which made those
	// joins load-bearing for tenant isolation without saying so.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_edges (id, team_id, project, src, dst, rel, weight)
		SELECT encode(sha256(convert_to(a.id || b.id || 'same_session', 'UTF8')), 'hex'),
		       $1, $2, a.id, b.id, 'same_session', 1
		FROM memories m1
		JOIN memories m2 ON m2.session_id = m1.session_id AND m2.id > m1.id
		                AND m2.team_id = $1 AND m2.project = $2
		JOIN graph_nodes a ON a.team_id = $1 AND a.project = $2 AND a.kind = 'memory' AND a.label = m1.id
		JOIN graph_nodes b ON b.team_id = $1 AND b.project = $2 AND b.kind = 'memory' AND b.label = m2.id
		WHERE m1.team_id = $1 AND m1.project = $2 AND m1.session_id IS NOT NULL
		  AND m1.deleted_at IS NULL AND m2.deleted_at IS NULL
		ON CONFLICT (src, dst, rel) DO NOTHING
	`, teamID, project); err != nil {
		return 0, 0, fmt.Errorf("building session edges: %w", err)
	}

	// file <-> file: touched within a few steps of each other in one session.
	//
	// This is the statement that draws an imported-but-not-yet-consolidated
	// project. It orders by observations.id, NOT created_at: created_at is the
	// import timestamp and is degenerate here -- only 19 of 568 sessions have
	// more than one distinct value, so ordering by it would shuffle the whole
	// session into one undifferentiated blob. The id is a ULID, which sorts in
	// creation order and therefore preserves the true within-session sequence.
	//
	// Kept on its own relation rather than merged into 'co_occurs': the two
	// weights measure different things (memories naming both, versus adjacent
	// edits), and UNIQUE (src, dst, rel) would let whichever statement ran
	// first silently own the number.
	//
	// Endpoints join graph_nodes by label rather than recomputing the hash, so
	// a file dropped by the node cap simply loses its edges instead of
	// violating the foreign key.
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_edges (id, team_id, project, src, dst, rel, weight)
		WITH seq AS (
			SELECT o.session_id AS sid,
			       uf.f         AS lab,
			       row_number() OVER (PARTITION BY o.session_id
			                          ORDER BY o.id, uf.f) AS rn
			FROM observations o
			CROSS JOIN LATERAL unnest(o.files) AS uf(f)
			WHERE o.team_id = $1 AND o.project = $2 AND uf.f <> ''
		),
		pairs AS (
			SELECT LEAST(a.lab, b.lab)    AS f1,
			       GREATEST(a.lab, b.lab) AS f2,
			       count(*)::real         AS w
			FROM seq a
			JOIN seq b
			  ON b.sid = a.sid
			 AND b.rn > a.rn
			 AND b.rn - a.rn <= $3
			 AND a.lab <> b.lab
			GROUP BY 1, 2
		)
		SELECT encode(sha256(convert_to(a.id || b.id || 'co_edited', 'UTF8')), 'hex'),
		       $1, $2, a.id, b.id, 'co_edited', p.w
		FROM pairs p
		JOIN graph_nodes a ON a.team_id = $1 AND a.project = $2
		                  AND a.kind = 'file' AND a.label = p.f1
		JOIN graph_nodes b ON b.team_id = $1 AND b.project = $2
		                  AND b.kind = 'file' AND b.label = p.f2
		ON CONFLICT (src, dst, rel) DO NOTHING
	`, teamID, project, coEditWindow); err != nil {
		return 0, 0, fmt.Errorf("building co-editing edges: %w", err)
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
		// Ordered by weight relative to the heaviest node OF THE SAME KIND,
		// not by raw weight.
		//
		// The scales are not comparable: a file's weight counts mentions and
		// now reaches 307, while a memory's is its strength and tops out near
		// 2. Ordering by the raw number put every file ahead of every memory,
		// so the top-N slice contained no memory nodes at all -- the legend
		// entry for them became permanently dead and their edges pointed at
		// nodes that were never returned.
		rows, err := s.pool.Query(ctx, `
			SELECT id, team_id, project, kind, label, weight
			FROM (
				SELECT *,
				       weight / NULLIF(max(weight) OVER (PARTITION BY kind), 0) AS rel
				FROM graph_nodes
				WHERE team_id = $1 AND project = $2
			) ranked
			ORDER BY rel DESC NULLS LAST, weight DESC, label
			LIMIT $3
		`, id.TeamID, project, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		nodeRows = rows
	} else {
		// Breadth-limited traversal, walked here rather than in SQL.
		//
		// The recursive CTE this replaces carried (id, hops) in its working
		// set, and UNION deduplicates whole rows -- so a node reachable at two
		// different distances was two distinct rows and got re-expanded once
		// per distance. That was survivable while the graph was 23 sparse
		// nodes. At the density observations produce (average degree ~11) it
		// does not return. Deduplicating on id alone fixes the blowup but
		// there is then no way to enforce depth, because the hop count cannot
		// participate in the duplicate check.
		//
		// A project's edges are bounded by graphNodeCap, so loading them and
		// walking in Go costs one indexed query and a few thousand map
		// operations -- and the traversal is ordinary code that can be tested
		// without a database.
		reachable, err := s.reachableFrom(ctx, id.TeamID, project, seed, depth, limit)
		if err != nil {
			return nil, err
		}
		if len(reachable) == 0 {
			return g, nil
		}
		rows, err := s.pool.Query(ctx, `
			SELECT id, team_id, project, kind, label, weight
			FROM graph_nodes
			WHERE team_id = $1 AND project = $2 AND id = ANY($3)
			ORDER BY weight DESC, label
			LIMIT $4
		`, id.TeamID, project, reachable, limit)
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

// reachableFrom returns the node ids within `depth` hops of the seed label.
func (s *Store) reachableFrom(ctx context.Context, teamID, project, seed string, depth, limit int) ([]string, error) {
	var seedID string
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM graph_nodes
		WHERE team_id = $1 AND project = $2 AND label = $3
	`, teamID, project, seed).Scan(&seedID)
	if err != nil {
		// An unknown seed is an empty neighbourhood, not a failure. The label
		// comes from a URL the user can edit and from a graph that is rebuilt
		// on a schedule, so a stale one is ordinary.
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT src, dst FROM graph_edges WHERE team_id = $1 AND project = $2
	`, teamID, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	adj := map[string][]string{}
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, err
		}
		// Both directions: co-occurrence and co-editing have no natural one.
		adj[src] = append(adj[src], dst)
		adj[dst] = append(adj[dst], src)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bfs(adj, seedID, depth, limit), nil
}

// bfs walks outward from start, visiting each node once, and stops at `depth`
// hops or `limit` nodes -- whichever comes first.
//
// Level order matters: the limit has to fall on the far edge of the
// neighbourhood rather than truncating the near one, or a seeded view could
// omit the seed's immediate neighbours while including distant nodes.
func bfs(adj map[string][]string, start string, depth, limit int) []string {
	if start == "" || limit <= 0 {
		return nil
	}
	seen := map[string]bool{start: true}
	out := []string{start}
	frontier := []string{start}

	for hop := 0; hop < depth && len(out) < limit; hop++ {
		var next []string
		for _, id := range frontier {
			for _, nb := range adj[id] {
				if seen[nb] {
					continue
				}
				seen[nb] = true
				out = append(out, nb)
				next = append(next, nb)
				if len(out) >= limit {
					return out
				}
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	return out
}

// ProjectsForGraph lists every team/project pair the graph can draw, so the
// scheduled rebuild knows what to walk.
//
// All three sources, not just memories. This is the reason a freshly imported
// project stayed empty no matter what RebuildGraph learned to read: a project
// with 234 sessions and 21,174 observations but no memories yet was not in this
// list, so the rebuild was never called for it at all.
func (s *Store) ProjectsForGraph(ctx context.Context) ([][2]string, error) {
	return s.projectPairs(ctx, `
		SELECT DISTINCT team_id, project FROM (
			SELECT team_id, project FROM memories
			  WHERE deleted_at IS NULL AND project <> ''
			UNION
			SELECT team_id, project FROM sessions     WHERE project <> ''
			UNION
			SELECT team_id, project FROM observations WHERE project <> ''
		) p
	`)
}

// ProjectsWithMemories lists team/project pairs that have at least one memory.
//
// Split from ProjectsForGraph rather than sharing it. Lesson synthesis walks
// this list, and it has nothing to say about a project whose memories have not
// been extracted yet -- widening it in place would have quietly taken tier 3
// from two projects to ten, spending an LLM call on each to produce nothing.
func (s *Store) ProjectsWithMemories(ctx context.Context) ([][2]string, error) {
	return s.projectPairs(ctx, `
		SELECT DISTINCT team_id, project FROM memories
		  WHERE deleted_at IS NULL AND project <> ''
	`)
}

func (s *Store) projectPairs(ctx context.Context, query string) ([][2]string, error) {
	rows, err := s.pool.Query(ctx, query)
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
