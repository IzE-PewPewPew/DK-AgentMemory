-- Indexes for building the graph from observations rather than memories alone.
--
-- No schema change is needed: graph_nodes' kind CHECK already permits every
-- kind used, and graph_edges has no CHECK on rel, so the new 'co_edited'
-- relation is legal as-is.
--
-- These are the three scans that a per-project rebuild performs. Without the
-- first, every rebuild sequentially scans all 36,185 observations, which is why
-- mid-size projects sat at a flat ~350ms floor regardless of their own size.

-- Per-project observation lookup, for file nodes and co-editing windows.
CREATE INDEX IF NOT EXISTS observations_team_project_idx
    ON observations (team_id, project);

-- Within-session ordering. The window function partitions by session_id and
-- orders by id, so the index carries both and the sort is free.
CREATE INDEX IF NOT EXISTS observations_session_id_idx
    ON observations (session_id, id);

-- graph_edges had indexes on src, dst and (src,dst,rel) but none on
-- (team_id, project) -- so counting a project's edges after a rebuild was a
-- full scan. Harmless at 17 rows, not at 120,000.
CREATE INDEX IF NOT EXISTS graph_edges_team_project_idx
    ON graph_edges (team_id, project);
