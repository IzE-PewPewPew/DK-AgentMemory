# Agent setup

`dkm connect --all` handles everything here. This page documents what it does and how to do it manually.

## Capability matrix

| Agent | Read | Auto-capture | Injection | Config |
|---|:--:|:--:|:--:|---|
| Claude Code | ✅ | ✅ | ✅ | `~/.claude.json` + `~/.claude/settings.json` |
| Codex CLI | ✅ | ⚠️ not yet | ❌ | `~/.codex/config.toml` |
| Claude Desktop | ✅ | ⚠️ | ❌ | `claude_desktop_config.json` |
| OpenCode | ✅ | ⚠️ | ❌ | `opencode.json` |
| Cursor | ✅ | ⚠️ | ❌ | `~/.cursor/mcp.json` |
| Kimi Code | ✅ | ⚠️ | ❌ | `~/.kimi-code/mcp.json` |
| Kimi CLI | ✅ | ⚠️ | ❌ | `~/.kimi/mcp.json` |
| Gemini CLI | ✅ | ⚠️ | ❌ | `~/.gemini/settings.json` |
| Windsurf | ✅ | ⚠️ | ❌ | `~/.codeium/windsurf/mcp_config.json` |
| Cline / Roo | ✅ | ⚠️ | ❌ | VS Code `globalStorage` settings |

Any agent that speaks MCP over streamable HTTP can also connect straight to the
server at `https://your-server/mcp` with the same bearer key, no local binary
involved. It is the same twelve tools and the same permission model — the
handler is mounted behind the API's own auth middleware.

## What ⚠️ means

**Only Claude Code has automatic capture in this build.** It exposes lifecycle
hooks, so every session, tool call, and file edit becomes an observation without
anyone asking.

Every other host here is MCP-only. The agent saves when it *decides* to call a
memory tool — in practice, when you say "remember this", and often not
otherwise. That is a limitation of what those hosts expose to MCP servers. No
configuration changes it, and any project claiming otherwise for them is
overstating what is possible.

**Codex CLI is the exception in the other direction.** It does expose hooks, and
the plan called for using them, but this build only writes its MCP server entry
— see [docs/dev/STATUS.md](dev/STATUS.md), task T2.6. Reading works today;
automatic capture there does not. This row will move back to ✅ when that lands
rather than before.

**Practical consequence:** if you want real automatic capture today, do project
work in Claude Code. Use Desktop, Cursor, Codex, and OpenCode as readers — they
see everything Claude Code captured.

## Claude Code

```bash
dkm connect claude-code
```

Writes the MCP server entry plus hooks:

| Hook | Action |
|---|---|
| SessionStart | Inject project context, token-budgeted |
| UserPromptSubmit | Retrieve against the prompt, inject above a relevance floor |
| PostToolUse | Capture file edits and commands into a local buffer |
| SessionEnd | Flush the buffer, close the session for consolidation |

Observations are buffered in `~/.dkm/hooks/` and sent in batches of twenty, or
whenever the session ends. One HTTP call per tool use would put network latency
between the agent and every edit it makes.

Every hook runs under a hard 2 s deadline and exits 0 regardless of outcome.
Failures are appended to `~/.dkm/hooks.log`, never printed — one of those
streams is a protocol channel and the other is the user's terminal.

Verify: `/mcp` inside Claude Code should list `dkm`.

## Claude Desktop

```bash
dkm connect claude-desktop
```

**Quit fully from the tray icon.** Closing the window leaves the process running with the old config — the most common reason a correct config appears not to work.

Verify: hammer icon in the chat box, or Settings → Developer.

## OpenCode

```bash
dkm connect opencode
```

Different schema from the others — top-level `mcp` key, command as an array. `dkm connect` handles it.

## Kimi Code

```bash
dkm connect kimi-code
```

Or interactively: run `/mcp-config` in the TUI, check with `/mcp`.

## Manual setup

Any MCP client:

```json
{
  "mcpServers": {
    "dkm": {
      "command": "dkm",
      "args": ["mcp"]
    }
  }
}
```

No credentials in agent config — `dkm mcp` reads `~/.dkm/config.yaml`. One file holds the key, so rotation touches one place.

On Windows use the absolute path with escaped backslashes:
```json
"command": "C:\\Users\\you\\AppData\\Local\\Programs\\dkm\\dkm.exe"
```

## MCP tools

| Tool | Purpose |
|---|---|
| `memory_search` | Hybrid search |
| `memory_save` | Store a fact or decision |
| `memory_lesson_save` | Store a durable rule |
| `memory_lesson_list` | Applicable lessons |
| `memory_context` | Project context for session start |
| `memory_session_history` | Recent sessions |
| `memory_forget` | Delete, with confirmation |
| `memory_share` | Private → team |
| `memory_feed` | Team-shared items |
| `memory_graph` | Related entities |
| `memory_supersede` | Mark outdated |
| `memory_reinforce` | Boost a useful memory |

Twelve tools an agent will use, rather than fifty it won't. `SKILL.md` ships alongside so agents know *when* to reach for them.

## Verifying

```bash
dkm doctor
```

Or ask the agent directly: *"What memory tools do you have?"* Twelve means connected.

| Result | Cause |
|---|---|
| 0 tools | MCP server didn't start — check binary path, restart the app |
| 12 tools, empty search | Connected but the store is empty, or wrong project |
| 401 in logs | Key revoked or wrong server |
