# Status

Internal. An honest accounting of the 38 tasks in [BUILD-PLAN.md](BUILD-PLAN.md)
against what is actually in the tree.

**Legend**
`done` — implemented, and its acceptance criterion is checkable.
`untested` — implemented, but the acceptance test has not been run here.
`partial` / `not done` — exactly what it says.

The distinction between `done` and `untested` matters and is the reason this
page exists. The build machine had Go and git but **no Docker and no Postgres**,
so nothing that needs a live database has been executed. CI runs those against a
real `pgvector/pgvector:pg16` service; until it has run green on a push, treat
every database-dependent claim as unverified.

---

## What has actually been run

```
go build ./...        clean, on all six release platforms
go vet ./...          clean
golangci-lint run     0 issues (v2.12.2, config in .golangci.yml)
gitleaks detect       no leaks (v8, config in .gitleaks.toml)
go test ./...         green — config, cron, redact, ulid, connect, client
go test -race         green
dkm mcp               initialize + tools/list return exactly 12 tools
dkm help              renders
```

**CI is green** — all ten jobs on `main`: secret scan, lint, test, the MCP
protocol check, and six build targets.

One caveat about the `test` job, because a green tick is easy to over-read: it
starts a real `pgvector/pgvector:pg16` service and runs
`go test -tags=integration`, but **no test carries that build tag yet**, so the
database sits idle and the step re-runs the same unit tests. The harness is in
place; the tests that would use it are not written. See the gap under T1.7.

Not run: the Docker Compose stack, the install scripts, and the release
pipeline.

---

## M1 — Foundation and server core

| ID | Task | State | Note |
|---|---|---|---|
| T1.1 | Repo scaffold, Makefile, Apache-2.0 | done | `make build` produces `bin/dkm` |
| T1.2 | CI matrix | **done** | Green on `main`. All actions pinned to commit SHAs |
| T1.3 | gitleaks pre-commit + CI | **done** | `.githooks/pre-commit`, `.gitleaks.toml`, CI job, all three verified. Enable the hook with `git config core.hooksPath .githooks` |
| T1.4 | Strict config validation | **done** | Tested: unknown key fatal with line and suggestion, missing required key fatal by name, comment-suffixed value rejected |
| T1.5 | Migration runner | untested | Forward-only, checksummed, per-migration transaction. Needs a database to prove the second run is a no-op |
| T1.6 | Full schema | untested | `0001_init.sql`, documented in [docs/SCHEMA.md](../SCHEMA.md) |
| T1.7 | pgx store, no ORM | partial | Hand-written SQL throughout. **Store-layer tests against a Dockerised Postgres are not written** — the largest gap in this build |
| T1.8 | argon2id keys, prefix lookup, revocation | untested | Revocation is read from the database on every request, so there is no cache to wait out |
| T1.9 | HTTP scaffold, JSON envelope, request IDs | done | Catch-all route means 404s carry a body too |
| T1.10 | livez / healthz | untested | livez unauthenticated; healthz reports db, embedder, worker, caller |
| T1.11 | Sessions CRUD | untested | |
| T1.12 | Batch observation ingest | untested | Up to 1000 per call, `files` GIN-indexed |
| T1.13 | Memories CRUD + supersede | untested | |
| T1.14 | Embedding sidecar + Go client | untested | FastAPI sidecar in `deploy/embed`, five providers behind one interface |
| T1.15 | Hybrid BM25 + vector + RRF | untested | Implemented as specified: rank fusion, no score normalisation. **The "serverless token signing" acceptance query has not been run** |
| T1.16 | Redaction at ingest | partial | Fifteen credential classes, unit-tested including a "does ordinary prose survive" case. **The test that greps the database after ingest is not written** |
| T1.17 | OpenAPI from handlers | done | Generated from the same route table the mux is built from |
| T1.18 | MCP: 12 tools, stdio + HTTP | **done** | Verified by hand and by a CI job that asserts the count is 12 |
| T1.19 | Docker Compose | untested | No Docker on the build machine |

## M2 — Client and agent integration

| ID | Task | State | Note |
|---|---|---|---|
| T2.1 | `dkm login` | untested | Key is verified against the server before the config is written |
| T2.2 | Project resolution | **done** | Tested: SSH and HTTPS remotes normalise to one identity; folder fallback warns |
| T2.3 | Agent detection, 10 hosts | untested | Windows, macOS and Linux paths are in the registry; only Windows has been exercised |
| T2.4 | Merge-never-overwrite config writers | **done** | Tested: other MCP servers survive, re-running is byte-identical, malformed JSON is refused rather than replaced, no key is written into agent config |
| T2.5 | Claude Code hooks | untested | Four events, 2 s budget, exit 0 always, observations batched at 20 |
| T2.6 | Codex CLI hooks | **not done** | Codex gets the MCP server entry only. Its config is TOML with no hook schema this build knows; auto-capture there does not work yet |
| T2.7 | connect / disconnect / list | untested | |
| T2.8 | `dkm doctor` | untested | Every failure line names a file, a command, or a config key |
| T2.9 | Client verbs | untested | Offline degradation implemented on search, lessons, and save |
| T2.10 | SKILL.md | done | Embedded and installed by connect for hosts that read one |

## M3 — Import and project identity

| ID | Task | State | Note |
|---|---|---|---|
| T3.1 | Claude Code JSONL importer | untested | Groups by git remote resolved from each transcript's recorded `cwd`, not from the path-derived folder name |
| T3.2 | Dry run with secret report | done | Dry run is the default; `--apply` is required to write |
| T3.3 | Markdown importer | untested | One memory per section; ADR paths become decisions |
| T3.4 | Cursor + Codex importers | partial | Codex rollouts parse through the same pipeline. **Cursor is deliberately not implemented** — its history lives in an undocumented SQLite schema; `dkm import cursor` explains the export-to-markdown route instead |
| T3.5 | Streaming NDJSON export/import | untested | Streamed both directions, no whole-corpus buffering |
| T3.6 | Dedup on import | partial | Exact content-hash dedup via a unique index. Near-duplicate vector dedup runs in consolidation, not on import |

## M4 — Team and sharing

| ID | Task | State | Note |
|---|---|---|---|
| T4.1 | Visibility enforced in SQL | partial | The predicate is a spliced constant in every memory query. **The store-layer test that deliberately attempts a cross-team read is not written** |
| T4.2 | Share and feed | untested | |
| T4.3 | Admin team/user/key | untested | Admins are scoped to their own team; cross-team key issue returns 403 |
| T4.4 | Audit log | untested | Written after commit, best effort, never fails the operation |
| T4.5 | Rate limiting | untested | Separate read and write buckets, real `Retry-After` |
| T4.6 | Read-only viewer | untested | `go:embed`, strict CSP, no write endpoint, key in sessionStorage only |
| T4.7 | SSE | untested | Keepalive every 25 s, `X-Accel-Buffering: no` for nginx |

## M5 — Consolidation, graph, offline

| ID | Task | State | Note |
|---|---|---|---|
| T5.1 | In-process scheduler | untested | Overlap guard; cron recomputed after each run so "0 2 * * *" means 2am |
| T5.2 | LLM provider abstraction | untested | anthropic / openai / google / openai-compatible |
| T5.3 | Tier 1 summaries | untested | One call per closed session, prompt budget capped |
| T5.4 | Tier 2 extraction with dedup | untested | Vector-searches before writing; reinforces above the threshold |
| T5.5 | Tier 3 lesson synthesis | untested | Requires ≥5 facts before it will run at all |
| T5.6 | Strength and decay | untested | Bounded reinforcement, half-life decay |
| T5.7 | Graph from co-occurrence | untested | Files, memories, shared sessions. Nothing invented |
| T5.8 | `GET /v1/graph` with traversal | untested | Recursive CTE, both edge directions |
| T5.9 | Local mirror | **deviation** | NDJSON directory, not SQLite. Documented in [CONFIGURATION.md](../CONFIGURATION.md) — see below |
| T5.10 | Offline queue, ULID, idempotent | untested | Client-generated ULIDs; the server upserts on the primary key |
| T5.11 | status / push / pull / sync | untested | |

## M6 — Release and launch

| ID | Task | State | Note |
|---|---|---|---|
| T6.1 | GoReleaser, checksums, cosign | untested | Keyless OIDC signing; **all GitHub Actions pinned to commit SHAs** |
| T6.2 | Multi-arch container to ghcr.io | untested | |
| T6.3 | Install scripts | untested | Both fit on a screen, verify checksums, print every change. Served from the repo rather than a domain |
| T6.4 | Homebrew tap + Scoop manifest | partial | GoReleaser config written; the `homebrew-tap` and `scoop-bucket` repositories do not exist yet |
| T6.5 | Docs site | **not done** | `docs/` is markdown in-repo, not published |
| T6.6 | SECURITY.md, templates, CONTRIBUTING | done | All present, updated to match the implementation |
| T6.7 | History scrubbed | n/a | No git history yet |
| T6.8 | Demo GIF | **not done** | |

---

## Deliberate deviations from the plan

**The local mirror is NDJSON, not SQLite.** The plan said `~/.dkm/cache.db`.
Pure-Go SQLite would add roughly 40 MB of generated code and a large build-time
cost; cgo SQLite would break the single-static-binary promise the whole project
rests on. The mirror stores tens of thousands of rows and needs one query
pattern, so it is a directory of NDJSON with a BM25 scorer in Go: inspectable
with `cat`, repairable with an editor, and no new dependency. `mirror_path` now
points at a directory.

**Transcript parsing is client-side.** The plan implied a
`POST /v1/import/jsonl` endpoint. Parsing on the server would mean uploading raw
history — credentials included — before anything had a chance to redact it, and
would require the server to resolve a project identity from a path that exists
only on the client. `dkm import claude-code` parses locally and posts through
the ordinary endpoints.

**Go 1.25, not 1.22.** Forced by the current `pgx` and `golang.org/x/crypto`
releases. Build-time only; the shipped binary needs no Go.

---

## The three things to do next, in order

1. **Write the store-layer integration tests.** Everything marked `untested`
   above is untested for one reason. Start with the cross-team read test from
   T4.1 and the post-ingest database grep from T1.16 — they are the two that
   protect properties nothing else can check.
2. **Run the M1 acceptance end to end.** `docker compose up -d`, issue a key,
   save from one tool, read from another. Under five minutes, no hand-edited
   JSON.
3. **Codex CLI hooks, or correct the documentation.** `docs/AGENTS.md` currently
   claims auto-capture for Codex. Either implement it or move Codex to the ⚠️
   row — the capability table being accurate is the one thing that page is for.
