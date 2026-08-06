# Installing the client

## macOS / Linux

```bash
curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh | sh
```

Installs to `~/.local/bin/dkm` and tells you if that is not on your PATH. It does
not edit shell profiles. To inspect it first — it is deliberately short enough
to read:

```bash
curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh -o install.sh
less install.sh
sh install.sh
```

Homebrew, once a release is tagged:
```bash
brew install IzE-PewPewPew/tap/dkm
```

## Windows

```powershell
irm https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.ps1 | iex
```

Installs to `%LOCALAPPDATA%\Programs\dkm` and adds it to your user PATH. Scoop,
once a release is tagged:
```powershell
scoop bucket add IzE-PewPewPew https://github.com/IzE-PewPewPew/scoop-bucket
scoop install dkm
```

## From source

```bash
go install github.com/IzE-PewPewPew/DK-AgentMemory/cmd/dkm@latest
```

Needs Go 1.25+. This is the path that works before the first tagged release.

## Manual

Download from [releases](https://github.com/IzE-PewPewPew/DK-AgentMemory/releases), verify, place on PATH:

```bash
sha256sum -c dkm_checksums.txt --ignore-missing
tar xzf dkm_linux_amd64.tar.gz
sudo mv dkm /usr/local/bin/
dkm version
```

Releases are signed with cosign:
```bash
cosign verify-blob --certificate dkm_linux_amd64.tar.gz.pem \
  --signature dkm_linux_amd64.tar.gz.sig dkm_linux_amd64.tar.gz
```

---

## Connect

```bash
dkm login https://memories.example.com
```

Prompts for an API key, or opens a browser for device-code flow if the server supports it. Writes `~/.dkm/config.yaml` with mode 600.

```bash
dkm connect --all
```

Detects installed AI tools and wires each:

```
Scanning for AI tools...
  ✓ Claude Code       ~/.claude/settings.json                    wired (+ hooks)
  ✓ Claude Desktop    ~/Library/.../claude_desktop_config.json   wired
  ✓ OpenCode          ~/.config/opencode/opencode.json           wired
  – Cursor            not installed

3 tools connected. Restart Claude Desktop to activate.
```

Existing config is merged, never replaced, and backed up to `.bak` first. Re-running is safe.

Single tool:
```bash
dkm connect claude-desktop
dkm connect --list
```

## Verify

```bash
dkm doctor
```

```
Server      https://memories.example.com             reachable, v0.1.0
Auth        kuong · acme (pmk_a3f2…)                 valid
Project     github.com/devkuong/launcher             via git remote
Memories    412 in store                             388 embedded, 24 awaiting the embedding backfill
Tools       Claude Code ✓  Claude Desktop ✓  OpenCode ✓
Sync        412 cached locally                       up to date
```

Any failure names the file, the command, or the config key that fixes it.
`dkm doctor --verbose` additionally prints every path checked.

The exit code is 0 when nothing is wrong and 1 otherwise, so it works in a
setup script.

## First memory

```bash
dkm save "deploys need Node 20, not 22"
dkm search "node version"
```

Then ask a different tool the same question. If it answers, you're done.

## Upgrade / uninstall

```bash
# Upgrade: re-run the installer, then re-wire so agent configs point at the
# new binary path if it moved.
curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh | sh
dkm connect --all

dkm disconnect --all        # remove from agent configs, leaving others intact
rm -rf ~/.dkm               # remove config, local mirror, and queued writes
```

`rm -rf ~/.dkm` deletes any writes still queued offline. Run `dkm status` first
if you have been working without a connection.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `dkm: command not found` | PATH not reloaded — open a new shell |
| Agent shows 0 memory tools | Config not reloaded. Quit fully (tray icon, not window) |
| `401 unauthorized` | Key revoked or wrong server. Run `dkm login` again |
| `project: unknown` | Not in a git repo, or no `origin` remote |
| Memories don't match teammate's | Different project ID — compare `dkm doctor` output |

`dkm doctor --verbose` prints every path checked and every decision made.
