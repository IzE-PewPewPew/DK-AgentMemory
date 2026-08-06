# Requirements

## Client

Nothing. `dkm` is a single static binary with no runtime dependencies.

| | Minimum |
|---|---|
| OS | macOS 12+, Linux (glibc 2.31+ / musl), Windows 10+ |
| Arch | amd64, arm64 |
| Disk | 200 MB (binary + local mirror) |
| Network | HTTPS to your server |

Optional: `git` improves project detection. Without it, projects fall back to folder name and won't match across machines.

## Server

### Docker (recommended)

| | Minimum | Comfortable |
|---|---|---|
| vCPU | 1 | 2 |
| RAM | 1 GB | 2 GB |
| Disk | 10 GB | 40 GB |
| OS | any with Docker 24+ | |

The embedding model uses ~400 MB RAM. Below 1 GB total, disable local embeddings and use a hosted provider.

### Native

- Go 1.25+ (build only — the released binary needs no Go at all)
- PostgreSQL 16+ with `pgvector` 0.7+ and `pg_trgm`
- Python 3.10+ if running the local embedding sidecar

`pgvector` 0.7 is the floor because the schema builds an HNSW index, which
earlier versions do not have. The Go floor comes from the `pgx` and
`golang.org/x/crypto` releases this depends on.

### Capacity

Measured on 1 vCPU / 2 GB with local embeddings:

| Team | Memories | Search p95 | Disk/yr |
|---|---|---|---|
| 1–5 | ~50k | 40 ms | ~2 GB |
| 5–20 | ~250k | 80 ms | ~10 GB |
| 20–50 | ~1M | 200 ms | ~40 GB |

Beyond ~1M memories, move Postgres to its own host.

## Embedding providers

| Provider | Cost | Quality | Notes |
|---|---|---|---|
| `local` (bge-small-en) | free | good | Default. 400 MB RAM, CPU only |
| `ollama` | free | varies | Reuses an existing Ollama install |
| `openai` | ~$0.02/1M tokens | best | `text-embedding-3-small` |
| `voyage` | paid | best for code | `voyage-code-3` |
| `none` | free | BM25 only | Keyword search works; no semantic recall |

## LLM provider (optional)

Only needed for the consolidation pipeline — session summaries, fact extraction, lesson synthesis. Without one, memories are stored and searchable but never distilled into lessons.

Any Anthropic, OpenAI, Google, or OpenAI-compatible endpoint. Consolidation batches at session boundaries, so a 5-person team typically costs cents per day on a small model.

## Network

Server needs one inbound HTTPS route. Cloudflare Tunnel, Tailscale, or a reverse proxy all work — the server binds loopback by default and expects something in front of it.

Outbound is only required for hosted embedding or LLM providers. Fully air-gapped operation works with `local` embeddings and consolidation disabled.
