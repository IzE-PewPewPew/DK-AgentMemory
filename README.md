<h1 align="center">DevKuong Memories</h1>

<p align="center">
  <strong>One shared memory for every AI coding tool your team uses.</strong>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="docs/SELF-HOSTING.md">Self-host</a> ·
  <a href="docs/AGENTS.md">Supported agents</a> ·
  <a href="docs/ARCHITECTURE.md">How it works</a>
</p>

---

Your agents forget everything between sessions. You re-explain the same stack, the same conventions, the same three gotchas — to every tool, every day, and so does everyone else on your team.

DevKuong Memories is a self-hosted memory service. One command wires every AI tool on your machine to it. What one tool learns, the others know. What you learn, your team knows.

```bash
curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh | sh
dkm login https://memories.example.com
dkm connect --all
```

That's the setup. No per-tool JSON editing, no runtime to install, no database to configure.

## What it does

**Shared across tools.** Save a decision in Claude Desktop, recall it in Claude Code. Same store, same project, no sync step.

**Shared across machines.** Projects are identified by git remote, not folder path — so your `~/dev/api` and your teammate's `D:\work\api` are the same project.

**Learns over time.** A consolidation pipeline distils raw session activity into facts, then facts into durable lessons. Nobody writes those by hand.

**Works offline.** Local mirror serves reads; writes queue and flush on reconnect.

**Yours.** Single Go binary plus Postgres. Runs on a €4 VPS. No SaaS, no telemetry, no account.

## Quick start

### Client

```bash
# Install — macOS / Linux
curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh | sh

# Install — Windows
irm https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.ps1 | iex

# Or from source, if you have Go
go install github.com/IzE-PewPewPew/DK-AgentMemory/cmd/dkm@latest

# Connect to your server
dkm login https://memories.example.com

# Wire every AI tool found on this machine
dkm connect --all

# Confirm
dkm doctor
```

### Server

```bash
git clone https://github.com/IzE-PewPewPew/DK-AgentMemory
cd DK-AgentMemory/deploy
cp .env.example .env      # set POSTGRES_PASSWORD and DKM_PUBLIC_URL
docker compose up -d
docker compose logs dkm | grep -A2 'admin key'
```

Prints an admin key on first boot, once. Full guide: [docs/SELF-HOSTING.md](docs/SELF-HOSTING.md).

## Supported agents

| Agent | Read memory | Auto-capture | Context injection |
|---|:--:|:--:|:--:|
| Claude Code | ✅ | ✅ hooks | ✅ |
| Codex CLI | ✅ | ⚠️ not yet | ❌ |
| Claude Desktop | ✅ | ⚠️ on request | ❌ |
| OpenCode | ✅ | ⚠️ on request | ❌ |
| Cursor | ✅ | ⚠️ on request | ❌ |
| Kimi Code / CLI | ✅ | ⚠️ on request | ❌ |
| Gemini CLI | ✅ | ⚠️ on request | ❌ |
| Any MCP client | ✅ | ⚠️ on request | ❌ |

⚠️ means the agent saves only when it decides to call a memory tool. True automatic capture needs lifecycle hooks, which most hosts do not expose — a limitation of those hosts, not of this project. Codex CLI does expose them and this build has not used them yet. See [docs/AGENTS.md](docs/AGENTS.md).

## Usage

```bash
dkm save "cloudflared runs under PM2 here, not systemd"
dkm lesson "always use full paths with pkill on multi-service hosts"
dkm search "tunnel config"
dkm share <id>            # private → team
dkm feed                  # what the team shared
dkm import claude-code    # pull in existing transcripts
```

Inside an agent, use natural language or the bundled skills — `/recall`, `/remember`, `/lessons`.

## Requirements

Client: nothing. Single static binary.
Server: 1 vCPU, 1 GB RAM, Docker — or Go 1.25+, Postgres 16 + pgvector.

Details in [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md).

## Status

v0. Every feature described on this page is implemented and the test suite is
green, but this has not yet run a real team for a month — which is the bar the
project set for itself before calling anything stable. Expect to find rough
edges, and please report them.

What that means concretely:

- The schema is frozen; migrations are forward-only from here.
- Nothing is published to Homebrew, Scoop, or ghcr.io until the first tagged
  release, so the install commands above need that tag to exist first. Building
  from source works today.
- Database-backed integration tests run in CI against a real Postgres. They are
  skipped locally unless you set `DKM_TEST_DATABASE_URL`.

## Comparison

| | DevKuong Memories | agentmemory | mem0 | Letta |
|---|---|---|---|---|
| Self-hosted | ✅ | ✅ | ⚠️ hybrid | ✅ |
| Single binary | ✅ | ❌ Node + Rust engine | ❌ | ❌ |
| Team namespacing | ✅ | ⚠️ basic | ✅ | ❌ |
| Per-user revocable keys | ✅ | ❌ one shared secret | ✅ | ⚠️ |
| Offline writes | ✅ | ❌ | ❌ | ❌ |
| Project ID across machines | ✅ git remote | ❌ folder path | ⚠️ | ⚠️ |
| Auto lesson synthesis | ✅ | ✅ | ⚠️ | ✅ |

Honest note: agentmemory has far more features. This project deliberately has fewer, chosen for a smaller operational surface.

## Documentation

- [Requirements](docs/REQUIREMENTS.md)
- [Installation](docs/INSTALL.md)
- [Self-hosting](docs/SELF-HOSTING.md)
- [Configuration](docs/CONFIGURATION.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Schema](docs/SCHEMA.md)
- [Agent setup](docs/AGENTS.md)
- [API reference](docs/API.md)
- [Implementation status](docs/dev/STATUS.md) — what is verified and what is not

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

Apache-2.0
