# Configuration

## Server

`config.yaml`, path via `--config`. Validated at boot: **unknown keys are fatal, missing required keys are fatal with the key named.** The server will not start half-configured.

Environment variables override file values using `DKM_` + underscore-joined path: `DKM_SERVER_BIND`, `DKM_DATABASE_URL`.

```yaml
server:
  bind: 127.0.0.1:8090          # required. keep on loopback
  public_url: https://memories.example.com   # required. used in device-code flow
  viewer_enabled: true
  viewer_path: /viewer
  read_timeout: 30s
  write_timeout: 60s
  shutdown_grace: 15s

database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm?sslmode=disable   # required
  max_conns: 20
  migrate_on_start: false       # prefer explicit `dkm migrate`

embedding:
  provider: local               # local | ollama | openai | voyage | none
  endpoint: http://127.0.0.1:8091
  model: BAAI/bge-small-en-v1.5
  dimensions: 384               # must match the schema; changing needs a reindex
  api_key_env: DKM_EMBED_API_KEY
  batch_size: 32
  timeout: 20s

search:
  default_limit: 8
  rrf_k: 60
  candidate_limit: 50           # rows each retriever contributes before fusion
  recency_half_life_days: 90
  dedup_threshold: 0.92

consolidation:
  enabled: true
  session_summary_interval: 15m
  fact_extraction_cron: "0 2 * * *"
  lesson_synthesis_cron: "0 3 * * 0"
  llm:
    provider: anthropic         # anthropic | openai | google | openai-compatible
    model: claude-haiku-4-5
    api_key_env: DKM_LLM_API_KEY
    base_url: ""
    max_tokens: 2000
    timeout: 2m

injection:
  enabled: true
  budget_tokens: 1500
  include: [lessons, decisions, session_summaries]

security:
  require_https: true           # refuse bearer over plaintext to non-loopback
  rate_limit_writes_per_min: 100
  rate_limit_reads_per_min: 300
  redaction_enabled: true       # do not disable
  audit_enabled: true

retention:
  observation_days: 90          # raw tier 0; consolidated memories are kept
  decay_enabled: true
  decay_half_life_days: 180

log:
  level: info                   # debug | info | warn | error
  format: json
```

### Required keys

`server.bind`, `server.public_url`, `database.url`. Everything else has a defensible default.

### No config file

A missing config file is not an error when the environment supplies the three
required keys. Container deployments configure entirely through `DKM_*`
variables, and mounting a file that only repeats them is one more thing that can
disagree with reality. If the file is absent *and* the environment is
incomplete, startup fails naming the missing key and both ways to supply it.

### Secrets

Never in `config.yaml`. Use `*_api_key_env` pointing at an environment variable. `chmod 600` the file regardless.

## Client

`~/.dkm/config.yaml`, written by `dkm login`. Mode 600.

```yaml
server: https://memories.example.com
key: pmk_a3f2_xxxxxxxxxxxx

user: kuong
team: acme

sync:
  enabled: true
  mirror_path: ~/.dkm/mirror    # a directory of NDJSON files, not a database
  refresh_interval: 5m
  queue_max: 1000

project:
  strategy: git-remote          # git-remote | folder | explicit
  explicit: ""                  # required when strategy is explicit
  fallback_warn: true

privacy:
  default_visibility: private   # private | team
  redact_local: true            # redact before sending
```

Env overrides: `DKM_SERVER`, `DKM_KEY`, `DKM_PROJECT`. `DKM_HOME` relocates the
whole state directory.

The mirror is a directory holding `memories.ndjson`, `queue.ndjson`, and a
`cursor` file — plain text, inspectable with `cat`, repairable with an editor,
and no cgo dependency in a binary whose main promise is that it runs anywhere.
Offline search scores that file with BM25; there is no embedder offline, so
semantic recall is unavailable rather than degraded silently. `dkm search`
says so when results came from the mirror.

### Per-project override

`.dkm/project` in repo root — a single line, committable, shared by the whole team:

```
github.com/devkuong/pprp/wallet-api
```

Useful for monorepos and for repos whose remote has changed.

### Per-project settings

`.dkm/config.yaml` in repo root:

```yaml
visibility: team          # everything in this repo defaults to team-visible
injection:
  budget_tokens: 3000
exclude:
  - "**/*.env"
  - "secrets/**"
```

`exclude` prevents matching paths from ever becoming observations. Independent of redaction — belt and braces.

## Agent config written by `dkm connect`

Reference only; `dkm connect` manages these.

**Claude Desktop** — `claude_desktop_config.json`:
```json
{
  "mcpServers": {
    "dkm": {
      "command": "/usr/local/bin/dkm",
      "args": ["mcp"],
      "env": { "DKM_SERVER": "https://memories.example.com" }
    }
  }
}
```

The key is not written into agent config — `dkm mcp` reads `~/.dkm/config.yaml`. Only one file holds the credential.

**OpenCode** — `opencode.json`:
```json
{
  "mcp": {
    "dkm": { "type": "local", "command": ["dkm", "mcp"], "enabled": true }
  }
}
```

**Claude Code** — two files, both written by `dkm connect claude-code`:

- `~/.claude.json` — the `mcpServers` entry
- `~/.claude/settings.json` — the four lifecycle hooks, each calling
  `dkm hook <event>` with a 2 s timeout

The hooks exit 0 whatever happens and never write to stdout except the
structured context block Claude Code reads. A memory system that stalls or
breaks the editor gets uninstalled the first time it happens.

`dkm connect` also installs `~/.claude/skills/memory/SKILL.md`, which is what
tells an agent *when* to search and save rather than only what it *can* call.
