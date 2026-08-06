# Build plan

Internal. Not shipped to users.

Six milestones, 38 tasks. Each task has an ID, dependencies, and a binary acceptance test. **A task is done when its acceptance test passes, not when the code looks finished.**

Estimated 6–8 weeks part-time for one developer with agent assistance.

---

## Rules for the whole build

1. **Never start a milestone until the previous one's acceptance passes.** Scope creep is the primary failure mode.
2. **Schema before handlers.** Migrations are frozen at the end of M1. Changing them later costs more than getting them right once.
3. **Integration tests over unit tests.** Real Postgres in Docker, real HTTP. The bugs that hurt in this domain are integration bugs.
4. **Every endpoint needs a golden-path test and an auth-failure test.** A 401 must always be distinguishable from a 404.
5. **No feature flags.** If it ships, it works.
6. **No secrets in the repo, ever.** Pre-commit hook with `gitleaks` from day one.

---

## M1 — Foundation & server core
*Target: 2 weeks. Exit: two different AI tools share a memory through the server.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T1.1 | Repo scaffold, Go module, Makefile, `.gitignore`, Apache-2.0 | – | `make build` produces `dkm` |
| T1.2 | CI: test + lint + build matrix (linux/darwin/windows × amd64/arm64) | T1.1 | Green on PR |
| T1.3 | `gitleaks` pre-commit + CI scan | T1.1 | Planted fake key fails CI |
| T1.4 | Config loader, strict validation, env override | T1.1 | Unknown key → fatal naming the key; missing required → fatal naming the key |
| T1.5 | Migration runner, forward-only, idempotent | T1.4 | `dkm migrate` twice is a no-op the second time |
| T1.6 | Full schema (see [docs/SCHEMA.md](../SCHEMA.md)) | T1.5 | All tables, indexes, extensions created |
| T1.7 | pgx store layer, no ORM, hand-written SQL | T1.6 | Store unit tests against Dockerised PG |
| T1.8 | API key auth: argon2id, prefix lookup, revocation | T1.7 | Valid → 200, revoked → 401, absent → 401, all with JSON bodies |
| T1.9 | HTTP scaffold, JSON error envelope, request ID, structured logs | T1.4 | Every response has a body, including 404 and 500 |
| T1.10 | `/v1/livez`, `/v1/healthz` | T1.9 | livez unauthenticated; healthz auth + reports db/embed status |
| T1.11 | Sessions CRUD | T1.7 | Create, end, list by project |
| T1.12 | Observations batch ingest | T1.11 | 100 observations in one call, files array indexed |
| T1.13 | Memories CRUD + supersede chains | T1.7 | Create, list, supersede; superseded excluded from default list |
| T1.14 | Embedding sidecar (FastAPI + fastembed) + Go client | T1.4 | 384-dim vector, <50ms p95 local |
| T1.15 | Hybrid search: BM25 + vector + RRF | T1.13, T1.14 | "serverless token signing" finds a jose/jsonwebtoken memory with no shared terms |
| T1.16 | Redaction at ingest | T1.12 | AWS key, `sk-` token, JWT, PEM block all stored as `redacted=true`, value absent from DB |
| T1.17 | OpenAPI generated from handlers, served at `/v1/openapi.json` | T1.9 | Spec validates; every route present |
| T1.18 | MCP server: 12 tools, stdio + streamable HTTP | T1.15 | `dkm mcp` passes MCP Inspector |
| T1.19 | Docker Compose: dkm + postgres + embed, admin key on first boot | T1.14 | `docker compose up -d` → healthz 200, key printed once |

**M1 acceptance:** on a clean machine, `docker compose up -d`, issue a key, `dkm login`, save a memory from Claude Desktop, retrieve it in Claude Code. Under five minutes, zero manual JSON editing.

---

## M2 — Client & agent integration
*Target: 1.5 weeks. Exit: one command wires every installed tool.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T2.1 | `dkm login` — key paste + device-code flow | T1.8 | Writes `~/.dkm/config.yaml` mode 600 |
| T2.2 | Project resolution: `.dkm/project` → git remote → folder → CWD | – | Same repo cloned to two different paths yields one project ID |
| T2.3 | Agent detection for 10 hosts | – | Correctly identifies installed vs absent on macOS/Linux/Windows |
| T2.4 | Config writers: merge, never overwrite; `.bak` first; idempotent | T2.3 | Existing MCP servers preserved; re-run changes nothing |
| T2.5 | Claude Code hooks: SessionStart, UserPromptSubmit, PostToolUse, SessionEnd | T2.4, T1.12 | Real session produces observations without user action |
| T2.6 | Codex CLI hooks | T2.5 | Same |
| T2.7 | `dkm connect --all` / `--list` / `<agent>` / `disconnect` | T2.4 | Output matches docs/INSTALL.md |
| T2.8 | `dkm doctor` + `--verbose` (format in [docs/INSTALL.md](../INSTALL.md)) | T2.7 | Every failure names a file and a fix |
| T2.9 | Client verbs: save, search, lesson, share, feed, export | T1.15 | Each works offline-degraded with a clear message |
| T2.10 | `SKILL.md` describing when to use each tool | T1.18 | Installed by connect where supported |

**M2 acceptance:** fresh Windows laptop → install → login → `connect --all` → all installed tools show 12 memory tools. No hand-edited JSON.

---

## M3 — Import & project identity
*Target: 1 week. Exit: real history imported, correctly grouped, secrets flagged.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T3.1 | Claude Code JSONL importer | T1.12, T2.2 | Parses `~/.claude/projects`, groups by git remote per transcript |
| T3.2 | `--dry-run` preview with secret report | T3.1, T1.16 | Counts, project grouping, and every secret by file and line |
| T3.3 | Markdown importer (CLAUDE.md, ADRs, runbooks) | T1.13 | Headings become memory titles, bodies become content |
| T3.4 | Cursor + Codex importers | T3.1 | Same pipeline |
| T3.5 | NDJSON export/import, streaming both directions | T1.13 | 500 MB round-trips without loading into memory |
| T3.6 | Dedup on import | T1.15 | Importing the same transcript twice creates no duplicates |

**M3 acceptance:** import a real `~/.claude/projects` with 40+ transcripts. Projects group correctly across two machines with different checkout paths. Planted secret is flagged in dry-run and redacted on import.

---

## M4 — Team & sharing
*Target: 1 week. Exit: private stays private; revocation is immediate.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T4.1 | Visibility enforced in SQL, every query | T1.13 | Test attempts cross-team read at store layer and fails |
| T4.2 | `POST /v1/share/{id}`, `GET /v1/feed` | T4.1 | Private invisible to teammate; shared visible |
| T4.3 | Admin: team/user create, key issue/revoke/list | T1.8 | Revoked key rejected on next request |
| T4.4 | Audit log on every mutation + `/v1/admin/audit` | T4.3 | Who/what/when queryable |
| T4.5 | Rate limiting per key | T1.9 | 429 with `Retry-After` past threshold |
| T4.6 | Read-only viewer, `go:embed`, served at `/viewer` | T1.15 | Memories, sessions, search, lessons. No write endpoints |
| T4.7 | SSE `/v1/events` for live viewer updates | T4.6 | Reconnects automatically; works through a tunnel |

**M4 acceptance:** two users, two keys. A saves privately — B cannot see it. A shares — B can. Revoke B's key — next request 401. Viewer works through Cloudflare Tunnel with no path rewriting.

---

## M5 — Consolidation, graph, offline
*Target: 2 weeks. Exit: lessons appear that nobody typed.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T5.1 | In-process scheduler | T1.9 | Survives restart, no duplicate runs |
| T5.2 | LLM provider abstraction (anthropic/openai/google/compatible) | T1.4 | Swap provider via config, no code change |
| T5.3 | Tier 1: session summaries | T5.1, T5.2 | One call per closed session, batched |
| T5.4 | Tier 2: fact extraction with dedup ≥0.92 | T5.3, T1.15 | Near-duplicates reinforce, never insert |
| T5.5 | Tier 3: lesson synthesis | T5.4 | Recurring patterns become lessons |
| T5.6 | Strength reinforcement + decay | T1.13 | Retrieved memories strengthen; unused decay |
| T5.7 | Graph extraction in the consolidation pass | T5.4 | Nodes/edges from real co-occurrence |
| T5.8 | `GET /v1/graph` with depth traversal | T5.7 | Returns connected subgraph for a project |
| T5.9 | Local mirror (SQLite) + `GET /v1/sync?since=` | T2.9 | Search works with the server unreachable |
| T5.10 | Offline write queue, ULID client IDs, idempotent flush | T5.9 | 3 offline writes flush exactly once on reconnect |
| T5.11 | `dkm status` / `push` / `pull` / `sync` | T5.10 | Queue depth, cursor, and mirror size all visible |

**M5 acceptance:** after one week of real team use, `dkm lesson list` shows rules nobody typed. Pull the network cable — search still works, writes queue, and flush on reconnect creates no duplicates.

---

## M6 — Release & launch
*Target: 1 week.*

| ID | Task | Deps | Acceptance |
|---|---|---|---|
| T6.1 | GoReleaser: binaries, checksums, cosign signatures | T1.2 | Tag produces a verifiable release |
| T6.2 | Container to ghcr.io, multi-arch | T6.1 | `docker pull` works on arm64 |
| T6.3 | Install scripts (sh + ps1) hosted | T6.1 | One-line install on all three OSes |
| T6.4 | Homebrew tap + Scoop manifest | T6.1 | `brew install` works |
| T6.5 | Docs site from `docs/` | – | Published, searchable |
| T6.6 | `SECURITY.md`, issue templates, `CONTRIBUTING.md` | – | Present and accurate |
| T6.7 | Git history scrubbed of every secret and internal hostname | – | `gitleaks detect --log-opts="--all"` clean |
| T6.8 | 30-second demo GIF | – | In README |

**M6 acceptance:** a stranger with no context installs, self-hosts, and connects one agent in under ten minutes using only the docs.

---

## Deferred — do not build in v1

Orchestration primitives (actions, leases, routines, signals, checkpoints, sentinels, sketches, crystals, facets), mesh/P2P sync, multi-tenant SaaS mode, fine-tuning, browser extension, mobile client.

Every unbuilt feature is one that can't break at 2am.
