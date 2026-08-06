# Architecture

## Overview

```
┌─ Developer machine ──────────────────────────────┐
│  Claude Code · Claude Desktop · OpenCode         │
│  Cursor · Kimi · Codex · any MCP client          │
│           │ stdio (MCP)                          │
│      dkm mcp   ← same binary, shim mode          │
│           │                                      │
│      ~/.dkm/cache.db  (local mirror + queue)     │
└───────────┼──────────────────────────────────────┘
            │ HTTPS + per-user bearer
            ▼
┌─ Server ─────────────────────────────────────────┐
│  tunnel / reverse proxy → 127.0.0.1:8090         │
│                                                  │
│  dkm serve (single Go binary)                    │
│    ├─ REST API (/v1) + generated OpenAPI         │
│    ├─ MCP over streamable HTTP                   │
│    ├─ Viewer (go:embed, read-only, /viewer)      │
│    ├─ SSE events (/v1/events)                    │
│    └─ Consolidation worker (in-process ticker)   │
│                    │                             │
│  Postgres 16 + pgvector    127.0.0.1:5432        │
│  Embedding sidecar         127.0.0.1:8091        │
└──────────────────────────────────────────────────┘
```

One binary serves the API, MCP, and viewer. There is no separate engine process to supervise, version-pin, or orphan.

## Why this stack

**Go.** Static binary, no runtime, no package manager at deploy time. Cross-compiles to every platform from one CI job.

**Postgres + pgvector.** Full-text search, vector similarity, JSONB, and transactions in one engine. `pg_dump` is the backup — no ambiguity about what the state store is.

**Embedding sidecar.** Isolated so a model crash can't take the API down, and swappable for a hosted provider by changing one config key.

**SSE, not WebSocket.** Live viewer updates over plain HTTP. Tunnels without special routing, reconnects on its own, no upgrade handshake to misroute.

## Memory tiers

```
Tier 0  observations      raw session activity, high volume
   │    every 15 min, per closed session
   ▼
Tier 1  session summary   1 LLM call per session
   │    nightly, per project
   ▼
Tier 2  facts / decisions extracted, deduped against existing
   │    weekly
   ▼
Tier 3  lessons           durable rules: "always X because Y"
```

Consolidation batches at session boundaries, never per-observation. Per-observation LLM calls are how memory systems become expensive without becoming better.

**Deduplication is what keeps it useful.** Before writing a fact, vector-search existing ones above 0.92 cosine. On a hit, reinforce or supersede — never write a near-duplicate. Skip this and the store degrades into noise within a month.

## Retrieval

Hybrid, fused. BM25 alone misses paraphrase; vectors alone miss exact identifiers like `pprp-wallet-api`.

```
1. BM25 top-50        tsv @@ websearch_to_tsquery
2. Vector top-50      embedding <=> query_vec
3. Reciprocal Rank Fusion:  score = Σ 1/(60 + rank_i)
4. × strength, × recency half-life (~90d)
5. Filter: own private + team-visible
6. Top-K (default 8)
```

RRF needs no score normalisation between the two systems, which is where naive hybrid ranking goes wrong.

## Project identity

Derived from the git remote, normalised:

```
git@github.com:devkuong/launcher.git
https://github.com/devkuong/launcher.git
   both → github.com/devkuong/launcher
```

Identical on every machine and OS regardless of checkout path. This is what lets one person's memories reach a teammate working on the same repo.

Resolution order:
1. `.dkm/project` in repo root — explicit, committable
2. Normalised git remote `origin`
3. Git root folder name, with a warning
4. CWD basename, with a louder warning

## Offline

- **Reads** hit `~/.dkm/cache.db` first, refresh in background
- **Writes** append to a local queue with a client-generated ULID, flush on reconnect
- **No conflicts by construction** — memories are append-only; corrections supersede rather than edit

Two people correcting the same fact produces two memories and one supersede chain. There is nothing to merge.

## Security model

**Per-user keys**, argon2id-hashed, revocable individually. Never one shared secret — rotating a shared secret means re-provisioning everyone.

**Redaction at ingest**, before persistence. Auto-capture means anything read during a session is a candidate for storage. Patterns matched: AWS keys, `sk-` prefixes, JWTs, PEM blocks, `password=`, connection strings. Stores `redacted=true` and offset, never the value.

**Scoping enforced in SQL.** Every query carries `team_id` and a visibility predicate, making cross-team leakage structurally impossible rather than a code-review concern.

**Viewer is read-only** and carries no write endpoints.

**Audit log** on every mutation.

## Failure design

Each rule below exists because its absence caused a real production incident in a comparable system:

| Rule | Prevents |
|---|---|
| Config validated at boot; unknown keys fatal | Silent misconfiguration (comments parsed into values) |
| Every response has a JSON body, including errors | Bare 404s indistinguishable from routing failures |
| One auth path for HTTP, CLI, and MCP | CLI 401ing while curl succeeds |
| No feature flags off by default | 503s with no indication why |
| OpenAPI generated from handler definitions | Guessing endpoint names |
| Migrations forward-only and idempotent | Half-applied schema after a failed deploy |
