# Schema

The full schema lives in
[`internal/store/migrations/0001_init.sql`](../internal/store/migrations/0001_init.sql).
This page explains the parts whose reasoning is not obvious from the DDL.

Migrations are forward-only and idempotent. Each runs in its own transaction
together with the row recording it, so a failure halfway leaves the database at
the last complete version rather than in a state no migration describes. A
migration that has already been applied is verified by checksum: editing one
after it has run anywhere is refused, because at that point the file and the
database no longer describe the same schema.

`dkm migrate` twice is a no-op the second time, and says so.

## Tables

| Table | Holds |
|---|---|
| `teams`, `users` | Identity. Every row in the system belongs to exactly one team. |
| `api_keys` | argon2id hashes plus an indexed non-secret prefix. |
| `sessions` | One agent run, with its tier-1 summary and consolidation watermarks. |
| `observations` | Tier 0: raw prompts, tool calls, edits. High volume, expires. |
| `memories` | Tiers 1–3: facts, decisions, lessons, preferences. |
| `graph_nodes`, `graph_edges` | Derived from co-occurrence. Rebuildable, never authoritative. |
| `audit_log` | Every mutation: who, what, when, from where. |
| `consolidation_runs` | What each pipeline pass did, and what it cost in tokens. |
| `schema_migrations` | Applied version and checksum. |

## Decisions worth explaining

**`project` is a text column, not a foreign key.** A project is a string derived
from a git remote (`github.com/org/repo`). Making it a table would add a join to
every query and an insert to every write, to enforce a constraint nothing needs:
projects have no attributes of their own, and a typo'd project ID is visible
immediately in `dkm doctor` rather than silently accepted by an FK that would
have created the row anyway.

**`tsv` is a generated column, not a trigger.** `GENERATED ALWAYS AS … STORED`
cannot drift from the row it indexes. A trigger can be dropped, can fail, and
can be forgotten in a bulk load — and the symptom is search silently missing
rows that are visibly present in the table.

**The dedup index is `(team_id, user_id, project, content_hash)` and partial on
`deleted_at IS NULL`.** Per-user, so two people independently recording the same
fact each keep their own copy, while one person importing the same transcript
twice does not create a second row. Partial, so deleting a memory and writing it
again is allowed. The hash is computed in Go over case-folded,
whitespace-collapsed text; reproducing that normalisation in SQL would be a
second implementation of the dedup key, and the two would disagree on the first
edge case either handled differently.

**`embedding` is nullable.** Writes never block on the embedder. A memory
written while the sidecar is down is stored without a vector, is immediately
findable by keyword, and is picked up by the backfill pass. The alternative —
rejecting the write — trades an hour of degraded recall for an hour of lost
work.

**`superseded_by` rather than an `UPDATE`.** Corrections add a row and link it.
Both statements survive in order, search shows only the newer one, and two
people correcting the same fact while offline produce two memories and one chain
instead of a conflict. Every hard part of offline sync is a consequence of
in-place mutation; this schema does not have any.

**`memories_updated_idx` on `(updated_at, id)`.** The sync cursor is a row
comparison `(updated_at, id) > ($1, $2)`, so ties on the timestamp break
deterministically. A cursor made of a timestamp alone loses rows written in the
same microsecond.

**`sessions_awaiting_summary_idx` is partial.** The consolidation worker's queue
is `ended_at IS NOT NULL AND summarised_at IS NULL`. Indexing exactly that
predicate keeps the scan proportional to the backlog rather than to every
session ever recorded.

**`strength` is bounded and decays.** Reinforcement approaches a ceiling of 5
rather than compounding, so a memory retrieved a thousand times outranks one
retrieved twice without making everything else unreachable. Decay runs on a
half-life measured in months; without it, an early wrong guess outranks its own
correction for ever, because the only thing that ever raised its score was
existing first.

## Vector dimensions

`vector({{EMBEDDING_DIM}})` is substituted from `embedding.dimensions` when the
migration runs. Changing that value after data exists is **not** a config edit:
the stored vectors are the wrong width, and the migration checksum changes,
which the runner refuses. Re-embedding is a deliberate operation — export,
change the dimension, re-import.

## Extensions

`vector` (pgvector 0.7+, for the HNSW index) and `pg_trgm`. Both are created by
the first migration with `IF NOT EXISTS`, which needs a role that may create
extensions — on a managed Postgres, create them once as superuser first.
