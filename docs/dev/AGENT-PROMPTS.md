# Agent prompts

Paste-ready prompts for building DevKuong Memories with a coding agent (Claude Code recommended — it has hooks, so it can eat its own dog food once M2 lands).

## How to use these

**One milestone per session.** Long sessions drift. Start fresh at each milestone boundary with the bootstrap prompt plus the milestone prompt.

**Give the agent these files:** `docs/dev/BUILD-PLAN.md`, `docs/ARCHITECTURE.md`, `docs/CONFIGURATION.md`, plus the two spec documents.

**Verify acceptance yourself.** Do not take "done" as evidence. Run the acceptance test.

---

## Bootstrap — paste once per session

```
You are building DevKuong Memories, an open-source self-hosted memory service
for AI coding agents. Repo: github.com/devkuong/memories. License Apache-2.0.

Read these before writing any code:
  docs/dev/BUILD-PLAN.md   — tasks and acceptance criteria
  docs/ARCHITECTURE.md     — design and rationale
  docs/CONFIGURATION.md    — config surface

TECH CONSTRAINTS — non-negotiable:
- Go 1.22+. No ORM: pgx v5 with hand-written SQL.
- Postgres 16 + pgvector. No other datastore server-side.
- Single binary: cmd/dkm, subcommands serve | mcp | migrate | client verbs.
- Layout: internal/{api,store,embed,mcp,consolidate,redact,connect,importers,sync}
- Config: YAML, strict validation at boot. Unknown key = fatal naming the key.
  Missing required key = fatal naming the key. Never start half-configured.
- Every HTTP response has a JSON body, including 404 and 500.
- One auth path shared by HTTP, CLI, and MCP. No separate code path.
- No feature flags. If it ships, it works.

TESTING — integration over unit:
- Real Postgres via testcontainers. Real HTTP calls.
- Every endpoint: one golden-path test, one auth-failure test.
- A 401 must always be distinguishable from a 404.

RULES:
- Work only on the assigned milestone. If you notice something belonging to a
  later milestone, write it in NOTES.md and move on. Do not implement it.
- Schema is frozen after M1. Flag any change as a blocker, do not proceed.
- Never commit secrets, real hostnames, or example keys that look real.
- Small commits, conventional commit messages.
- When a task's acceptance criterion is ambiguous, ask before implementing.

Confirm you have read the plan and state which tasks you will do first.
```

---

## M1 — Foundation & server core

```
Implement Milestone M1, tasks T1.1 through T1.19, in dependency order.

Start with T1.1–T1.6 (scaffold, CI, config, migrations, schema) and stop.
Show me the schema and config validation before continuing to T1.7.

Notes on specific tasks:

T1.4  Config validation is a headline feature, not boilerplate. Unknown keys
      must be fatal — a mistyped or comment-suffixed value silently becoming
      part of the value is the exact failure this prevents. Test it.

T1.8  Keys are `pmk_<prefix>_<random>`. argon2id-hash the whole key. Store an
      8-char prefix separately for O(1) lookup and log identification. Plaintext
      returned once at issue and never again.

T1.15 Hybrid search is the core of the product. BM25 top-50 via tsvector, vector
      top-50 via pgvector cosine, fuse with RRF (k=60), then multiply by strength
      and a 90-day recency half-life. Do NOT normalise scores between the two
      systems — that is what RRF exists to avoid.
      Acceptance: store "we chose jose over jsonwebtoken for Edge runtime
      compatibility", query "serverless token signing" — zero shared terms —
      and it must rank first.

T1.16 Redaction runs before persistence, not after. Patterns: AWS access keys,
      `sk-` prefixed tokens, JWTs, PEM private key blocks, `password=`,
      postgres/mysql/mongo connection strings. Store redacted=true plus the
      offset. The raw value must never touch the database. Write a test that
      greps the DB after ingest to prove absence.

T1.18 Exactly 12 MCP tools, listed in docs/AGENTS.md. Do not add more. Support
      both stdio and streamable HTTP from one implementation.

Deliver working `dkm serve`, `dkm mcp`, and `dkm migrate` before anything else.
```

---

## M2 — Client & agent integration

```
Implement Milestone M2, tasks T2.1 through T2.10.

This milestone is the product's first impression. Polish matters here more
than anywhere else in the codebase.

T2.2  Project identity is the most important design decision in the project.
      Resolve in this order:
        1. .dkm/project file in repo root
        2. normalised git remote origin
        3. git root folder name  (warn: will not match across machines)
        4. CWD basename          (warn loudly)
      Normalise git@github.com:org/repo.git and https://github.com/org/repo.git
      to the same string: github.com/org/repo. Strip .git, lowercase the host,
      preserve case in org/repo.
      Acceptance: same repo cloned to /home/a/x and C:\dev\y yields one project ID.

T2.4  Config writers must MERGE, never overwrite. Users have other MCP servers
      installed. Back up to .bak before touching anything. Running connect twice
      must produce a byte-identical file the second time.
      Do NOT write the API key into agent configs. `dkm mcp` reads
      ~/.dkm/config.yaml. One file holds the credential so rotation touches
      one place.

T2.5  Claude Code hooks post to /v1/observations. Hooks must never block or
      crash the host agent — timeout at 2s, fail silent, log locally.
      A memory system that breaks the user's editor is worse than no memory
      system.

T2.8  `dkm doctor` output is in docs/INSTALL.md — match it. Every failure line
      must name a file path and a concrete fix. "Something went wrong" is a bug.

Test agent detection on all three OSes. Windows paths are where this breaks.
```

---

## M3 — Import & project identity

```
Implement Milestone M3, tasks T3.1 through T3.6.

T3.2  Dry-run is the default posture for import. The preview must show:
      transcript count, project grouping, observation counts, and every
      detected secret with file and line.
      Users are importing years of history that may contain credentials they
      read during a session. Discovering that after the fact is unacceptable.
      Format is in the client spec §6 — match it.

T3.6  Dedup on import is mandatory. Re-importing the same transcript must
      create zero new memories. Use content hash for exact matches and vector
      similarity ≥0.92 for near-matches.

T3.5  Export streams NDJSON, one record per line. Do NOT build a single JSON
      array — a real corpus exceeds tunnel and proxy body limits, and it forces
      the whole export into memory on both ends.

Test against a real ~/.claude/projects directory with 40+ transcripts.
```

---

## M4 — Team & sharing

```
Implement Milestone M4, tasks T4.1 through T4.7.

T4.1  Visibility is enforced in SQL, not in application code. Every query
      carries team_id and a visibility predicate. Write a store-layer test that
      deliberately attempts a cross-team read and asserts it returns nothing.
      Application-layer checks get forgotten in new endpoints; SQL-layer checks
      cannot be.

T4.6  The viewer is READ-ONLY. No write endpoints, no delete buttons, no key
      display. Embed with go:embed and serve from the same origin as the API
      under /viewer — a second hostname means path rewriting and WebSocket
      routing problems that are entirely avoidable.

T4.7  SSE, not WebSocket. Plain HTTP, tunnels without special configuration,
      reconnects on its own. Do not use WebSocket for the viewer under any
      circumstances.

T4.3  Key revocation must take effect on the next request, not on a cache TTL.
```

---

## M5 — Consolidation, graph, offline

```
Implement Milestone M5, tasks T5.1 through T5.11.

T5.3-5.5  COST DISCIPLINE IS THE REQUIREMENT HERE. Batch at session boundaries
      and on a schedule. Never fire an LLM call per observation — that pattern
      makes memory systems expensive without making them better.
      Tier 1: one call per closed session, every 15 min.
      Tier 2: nightly per project.
      Tier 3: weekly across facts.
      Log estimated token spend per run.

T5.4  Dedup before write, always. Vector-search existing facts; on ≥0.92 cosine,
      reinforce the existing memory or supersede it. Never insert a near-
      duplicate. Skipping this is why memory systems degrade into noise.

T5.10 Offline queue uses client-generated ULIDs so writes are idempotent on
      flush. Server upserts on client_id. Test: queue 3 writes offline,
      reconnect, flush twice — exactly 3 memories exist.

T5.7  Graph nodes and edges come from real co-occurrence in consolidated
      memories: shared files, shared concepts, shared sessions. Do not invent
      relationships. An empty graph on an empty corpus is correct behaviour —
      do not fabricate nodes to make the UI look populated.
```

---

## M6 — Release & launch

```
Implement Milestone M6, tasks T6.1 through T6.8.

T6.7  Before the repo goes public, scrub git history for secrets, internal
      hostnames, and real API keys. Use git-filter-repo. Run
      `gitleaks detect --log-opts="--all"` and require zero findings.
      A force-push does not remove data from forks, caches, or the GitHub API.

T6.1  Pin all GitHub Actions to commit SHAs, not tags.

T6.3  The install script is the first code a stranger runs on their machine.
      It must be short enough to read in one screen, do nothing surprising,
      and print exactly what it changed.
```

---

## Debugging prompt

When something breaks mid-build:

```
A test is failing / behaviour is wrong. Before changing code:

1. State what you expected and what happened.
2. Identify which layer owns the failure: config, store, api, mcp, client.
3. Write a minimal failing test that isolates it.
4. Only then propose a fix.

Do not change more than one layer per attempt. Do not add retries, sleeps, or
error suppression to make a test pass — those hide bugs rather than fix them.
```

## Review prompt

At each milestone boundary:

```
Milestone review before I run acceptance:

1. List every task in this milestone and its status.
2. For each acceptance criterion, state the exact command I should run and
   the exact output I should see.
3. List anything you implemented that was NOT in the milestone scope.
4. List anything in NOTES.md deferred to later milestones.
5. Flag any place where you worked around a design constraint rather than
   satisfying it.

Be blunt about item 5. A workaround I know about is manageable; one I discover
in production is not.
```
