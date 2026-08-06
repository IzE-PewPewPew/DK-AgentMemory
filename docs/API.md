# API reference

Base path `/v1`. Machine-readable spec at `GET /v1/openapi.json`, generated from handler definitions — it cannot drift from the implementation.

## Auth

```
Authorization: Bearer pmk_a3f2_xxxxxxxxxxxx
```

Every endpoint except `/v1/livez` requires it. Errors always carry a JSON body:

```json
{ "error": "unauthorized", "message": "key revoked", "request_id": "01J..." }
```

401 (bad or revoked key) is always distinguishable from 404 (no such route or resource).

## Health

```
GET  /v1/livez        no auth  → {"ok":true}
GET  /v1/healthz      auth     → db, embedder, worker, version, caller, stats
GET  /v1/openapi.json auth     → OpenAPI 3.1
```

`/v1/livez` is unauthenticated on purpose: an uptime check should not need a
credential, and a credential inside a monitoring system is a credential in one
more place.

`/v1/healthz` returns 503 when the database is unreachable and 200 when only the
embedder is down. A degraded embedder means keyword-only search, not an outage,
and a deploy should not roll back for it.

The `caller` block echoes which user and team the presented key resolved to,
which is what `dkm doctor` reports.

## Sessions & observations

```
POST   /v1/sessions                      {project, agent, meta?} → {id}
PATCH  /v1/sessions/{id}                 {ended_at?, summary?}
GET    /v1/sessions?project=&limit=
GET    /v1/sessions/{id}
GET    /v1/sessions/{id}/observations?limit=
POST   /v1/observations                  {session_id, items:[{kind, content, files[]}]}
```

`POST /v1/observations` is batched, up to 1000 items per call — hooks buffer and
send in groups rather than one call per event. Redaction runs here, before
persistence, and the response reports how many items were redacted.

## Memories

```
POST   /v1/memories              {kind, title, body, project?, files[]?, visibility?}
GET    /v1/memories?kind=&project=&limit=&cursor=
GET    /v1/memories/{id}
PATCH  /v1/memories/{id}         {title?, body?, visibility?}
DELETE /v1/memories/{id}         soft delete, audited
POST   /v1/memories/{id}/supersede   {new_id}
POST   /v1/memories/{id}/reinforce
```

`kind` is one of `fact`, `decision`, `lesson`, `preference`. `visibility` is `private` or `team`, defaulting to `private`.

`POST /v1/memories` accepts an optional client-generated ULID as `id`. Offline
clients set it so a queued write flushed twice lands on the same primary key.
Creating a memory that already exists returns **200** with the existing row
rather than 409 — a re-import or a re-flushed queue is a success, and making
callers handle an error to discover they already have the row helps nobody.
A new memory returns 201.

## Search

```
POST /v1/search    {query, project?, kinds[]?, limit?}
```

```json
{
  "results": [
    { "id": "01J...", "kind": "decision", "title": "...", "body": "...",
      "score": 0.87, "project": "github.com/devkuong/launcher",
      "files": ["src/auth.ts"], "created_at": "..." }
  ]
}
```

Hybrid BM25 + vector, RRF-fused, weighted by strength and recency. Scoped to the caller's private memories plus team-visible ones.

The response carries `mode`: `hybrid` when a query vector was available,
`keyword` when the embedder was unreachable. The per-result `bm25_rank` and
`vector_rank` show which retriever contributed, which answers "why did this come
first" without reproducing the query.

```
POST /v1/context   {project, budget_tokens?}
```

Returns a token-budgeted injection payload: active lessons, then project decisions, then recent session summaries.

## Lessons

```
GET  /v1/lessons?project=
POST /v1/lessons                 {lesson, project?, files[]?}
POST /v1/lessons/{id}/reinforce
```

Lessons are memories with `kind=lesson`; these routes are conveniences.

## Sharing

```
POST /v1/share/{memory_id}       private → team
GET  /v1/feed?limit=             recent team-visible
```

## Projects

```
GET  /v1/projects                projects visible to the caller, with counts
```

## Graph

```
GET  /v1/graph?project=&node=&depth=&limit=
POST /v1/graph/rebuild?project=
```

`node` seeds a traversal from a label, usually a file path. Omit it for the
whole project graph.

Rebuild is idempotent and safe to run any time. An empty graph on a small corpus is correct — nodes come from real co-occurrence, not fabrication.

## Sync

```
GET  /v1/sync?since=<cursor>     incremental changes for the local mirror
```

## Import / export

```
GET  /v1/export?scope=me|team&project=   streams NDJSON
POST /v1/import                          accepts an NDJSON stream
POST /v1/import/preview                  dry run: counts, grouping, secret report
```

Streaming NDJSON in both directions. No body-size ceiling, no whole-corpus buffering.

`dkm export --format md` renders Markdown **client-side** from this same NDJSON
stream. The server keeps one export format; presentation belongs in the client,
where it can change without a redeploy. That path does buffer, because grouping
by project and ordering by kind cannot be done in one forward pass — acceptable,
because a corpus too large to hold in memory is also too large to read.

Agent transcripts are parsed **client-side** by `dkm import claude-code` and
`dkm import codex`, which then post sessions and batched observations through
the ordinary endpoints. Parsing them on the server would mean uploading raw
history — including whatever credentials it contains — before anything has had a
chance to redact it, and it would mean the server needing to resolve a project
identity from a path that only exists on the client's machine.

## Admin

```
GET    /v1/admin/keys
POST   /v1/admin/keys            {user_id, label} → plaintext key, once
DELETE /v1/admin/keys/{id}       effective on next request
POST   /v1/admin/teams
POST   /v1/admin/users
GET    /v1/admin/users
GET    /v1/admin/audit?user=&action=&since=&limit=
GET    /v1/admin/runs            consolidation runs and 30-day token spend
```

Admin routes are scoped to the caller's own team. An admin key is not a global
key — issuing a key for a user in another team returns 403, because otherwise
the team boundary stops being a boundary.

## MCP

```
POST /mcp                        streamable HTTP transport, same bearer auth
```

The same twelve tools a local `dkm mcp` process exposes, for agents that speak
MCP over HTTP. Mounted behind the same auth middleware as everything else.

## Events

```
GET /v1/events                   SSE stream
```

Server-sent events for the viewer. Plain HTTP — no upgrade handshake, tunnels without special routing, reconnects on its own.

## Rate limits

Default 100 writes/min and 300 reads/min per key. Exceeding returns 429 with `Retry-After`.

## Pagination

Cursor-based. Responses include `next_cursor` when more results exist. Offset pagination is not supported.
