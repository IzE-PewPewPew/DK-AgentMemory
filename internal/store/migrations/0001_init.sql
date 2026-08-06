-- 0001_init: the whole schema.
--
-- Frozen at the end of M1. Later migrations add, they do not rewrite: a
-- forward-only history is what makes a failed deploy resumable rather than a
-- restore-from-backup event.
--
-- {{EMBEDDING_DIM}} is substituted by the migration runner from
-- embedding.dimensions. Changing that value after data exists requires a
-- re-embed and a new migration; it is not a config-only change.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- --------------------------------------------------------------------------
-- Identity
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS teams (
    id          text PRIMARY KEY,
    name        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id          text PRIMARY KEY,
    team_id     text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name        text        NOT NULL,
    is_admin    boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS users_team_idx ON users (team_id);

-- API keys are `pmk_<prefix>_<secret>`. The prefix is stored in the clear for
-- an indexed single-row lookup and for identifying a key in logs and in
-- `dkm doctor`; the whole key is argon2id-hashed. The plaintext is returned
-- once at issue and is not recoverable afterwards.
CREATE TABLE IF NOT EXISTS api_keys (
    id            text PRIMARY KEY,
    user_id       text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    prefix        text        NOT NULL UNIQUE,
    hash          text        NOT NULL,
    label         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys (user_id);

-- --------------------------------------------------------------------------
-- Tier 0: sessions and observations
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS sessions (
    id                  text PRIMARY KEY,
    team_id             text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id             text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project             text        NOT NULL DEFAULT '',
    agent               text        NOT NULL DEFAULT '',
    started_at          timestamptz NOT NULL DEFAULT now(),
    ended_at            timestamptz,
    summary             text,
    summarised_at       timestamptz,
    facts_extracted_at  timestamptz,
    meta                jsonb       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS sessions_team_project_idx ON sessions (team_id, project, started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id, started_at DESC);

-- The consolidation worker's work queue. A partial index keeps it proportional
-- to the backlog rather than to the total number of sessions ever recorded.
CREATE INDEX IF NOT EXISTS sessions_awaiting_summary_idx
    ON sessions (ended_at)
    WHERE ended_at IS NOT NULL AND summarised_at IS NULL;

CREATE TABLE IF NOT EXISTS observations (
    id           text PRIMARY KEY,
    session_id   text        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    team_id      text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id      text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project      text        NOT NULL DEFAULT '',
    kind         text        NOT NULL,
    content      text        NOT NULL,
    files        text[]      NOT NULL DEFAULT '{}',
    redacted     boolean     NOT NULL DEFAULT false,
    redactions   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS observations_session_idx ON observations (session_id, created_at);
CREATE INDEX IF NOT EXISTS observations_files_idx   ON observations USING gin (files);
CREATE INDEX IF NOT EXISTS observations_created_idx ON observations (created_at);

-- --------------------------------------------------------------------------
-- Tiers 1-3: memories
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS memories (
    id             text PRIMARY KEY,
    team_id        text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id        text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project        text        NOT NULL DEFAULT '',
    kind           text        NOT NULL CHECK (kind IN ('fact', 'decision', 'lesson', 'preference')),
    title          text        NOT NULL,
    body           text        NOT NULL,
    files          text[]      NOT NULL DEFAULT '{}',
    visibility     text        NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'team')),

    -- strength rises on reinforcement and decays with disuse; it multiplies the
    -- fused retrieval score.
    strength       real        NOT NULL DEFAULT 1.0 CHECK (strength >= 0),
    hits           integer     NOT NULL DEFAULT 0,

    source         text        NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'consolidation', 'import', 'hook')),
    session_id     text        REFERENCES sessions(id) ON DELETE SET NULL,

    -- sha256 over normalised title+body. Exact-duplicate defence; near
    -- duplicates are caught by vector similarity at write time instead.
    content_hash   text        NOT NULL,

    redacted       boolean     NOT NULL DEFAULT false,
    redactions     jsonb       NOT NULL DEFAULT '[]'::jsonb,

    -- Corrections supersede rather than edit, so history stays intact and two
    -- people correcting the same fact produces a chain rather than a conflict.
    superseded_by  text        REFERENCES memories(id) ON DELETE SET NULL,

    deleted_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz,

    embedding      vector({{EMBEDDING_DIM}}),

    tsv tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(body,  '')), 'B')
    ) STORED
);

CREATE INDEX IF NOT EXISTS memories_tsv_idx           ON memories USING gin (tsv);
CREATE INDEX IF NOT EXISTS memories_files_idx         ON memories USING gin (files);
CREATE INDEX IF NOT EXISTS memories_team_project_idx  ON memories (team_id, project);
CREATE INDEX IF NOT EXISTS memories_team_kind_idx     ON memories (team_id, kind);
CREATE INDEX IF NOT EXISTS memories_user_idx          ON memories (user_id);
CREATE INDEX IF NOT EXISTS memories_session_idx       ON memories (session_id);
CREATE INDEX IF NOT EXISTS memories_created_idx       ON memories (created_at DESC);

-- The sync cursor. Row comparison against (updated_at, id) needs this exact
-- composite ordering to use an index rather than sorting the table.
CREATE INDEX IF NOT EXISTS memories_updated_idx       ON memories (updated_at, id);

-- Exact-duplicate guard, scoped per user so two people independently recording
-- the same fact each keep their own copy, while one person importing the same
-- transcript twice does not.
CREATE UNIQUE INDEX IF NOT EXISTS memories_dedup_idx
    ON memories (team_id, user_id, project, content_hash)
    WHERE deleted_at IS NULL;

-- HNSW over cosine distance. Built after the table so a restore populates rows
-- first and pays for one index build rather than one per row.
CREATE INDEX IF NOT EXISTS memories_embedding_idx
    ON memories USING hnsw (embedding vector_cosine_ops);

-- --------------------------------------------------------------------------
-- Graph
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS graph_nodes (
    id          text PRIMARY KEY,
    team_id     text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project     text        NOT NULL DEFAULT '',
    kind        text        NOT NULL CHECK (kind IN ('file', 'concept', 'memory', 'session', 'project')),
    label       text        NOT NULL,
    weight      real        NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, project, kind, label)
);
CREATE INDEX IF NOT EXISTS graph_nodes_team_project_idx ON graph_nodes (team_id, project);

CREATE TABLE IF NOT EXISTS graph_edges (
    id       text PRIMARY KEY,
    team_id  text        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project  text        NOT NULL DEFAULT '',
    src      text        NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,
    dst      text        NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,
    rel      text        NOT NULL,
    weight   real        NOT NULL DEFAULT 1,
    UNIQUE (src, dst, rel)
);
CREATE INDEX IF NOT EXISTS graph_edges_src_idx ON graph_edges (src);
CREATE INDEX IF NOT EXISTS graph_edges_dst_idx ON graph_edges (dst);

-- --------------------------------------------------------------------------
-- Audit
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_log (
    id          text PRIMARY KEY,
    team_id     text,
    user_id     text,
    key_id      text,
    action      text        NOT NULL,
    target      text,
    detail      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    request_id  text,
    ip          text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_team_idx   ON audit_log (team_id, created_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_user_idx   ON audit_log (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_action_idx ON audit_log (action, created_at DESC);

-- --------------------------------------------------------------------------
-- Consolidation bookkeeping
-- --------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS consolidation_runs (
    id             text PRIMARY KEY,
    tier           smallint    NOT NULL,
    team_id        text,
    project        text,
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    items          integer     NOT NULL DEFAULT 0,
    produced       integer     NOT NULL DEFAULT 0,
    deduped        integer     NOT NULL DEFAULT 0,
    input_tokens   integer     NOT NULL DEFAULT 0,
    output_tokens  integer     NOT NULL DEFAULT 0,
    error          text
);
CREATE INDEX IF NOT EXISTS consolidation_runs_started_idx ON consolidation_runs (started_at DESC);
