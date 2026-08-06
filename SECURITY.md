# Security policy

## Reporting

**Do not open a public issue for security vulnerabilities.**

Email **security@devkuong.dev** with:
- Description and impact
- Reproduction steps
- Affected version
- Any suggested fix

**Response:** acknowledgement within 72 hours, initial assessment within 7 days. Fix timeline depends on severity; we'll keep you informed. Credit in the advisory unless you'd rather not be named.

## Supported versions

The latest minor release receives security fixes. Older versions do not.

## Threat model

**Assumed:** the server holds sensitive data. Memories contain source code excerpts, architecture decisions, file paths, and prompt text. Treat the database as you would a source repository.

**Defended against:** unauthenticated access, cross-team data access, credential leakage into stored memory, key compromise (per-user revocation), replay of revoked keys, injection via memory content.

**Not defended against:** a compromised server host, a malicious team member with a valid key, a compromised client machine, or an LLM provider retaining consolidation prompts. If your threat model includes the last one, disable consolidation or use a local model.

## Deployment guidance

**Bind to loopback.** Default is `127.0.0.1`. Put a tunnel or reverse proxy in front. Verify with `ss -tlnp` after every deploy rather than trusting the default.

**One key per person.** Never share a key across users or machines. Revocation should affect one person.

**Rotate on departure.** Revoke the individual key. No team-wide rotation needed — that's the point of per-user keys.

**Leave redaction on.** `security.redaction_enabled: false` exists for debugging. Running it in production means credentials read during any captured session are stored in plaintext.

**Review imports.** `--dry-run` before importing historical transcripts. Years of sessions may contain credentials.

**Protect the viewer.** Read-only, but it renders every memory. Put authentication in front of it — Cloudflare Access, Tailscale, or basic auth at the proxy. Configure that *before* creating the DNS record.

**Back up encrypted.** `pg_dump` output contains everything. Encrypt at rest and in transit.

## Known limitations

**Redaction is pattern-based.** It catches common credential formats. It cannot catch every secret — a password in prose reads like prose. Use `.dkm/config.yaml` `exclude` patterns for files that should never be observed at all.

**MCP clients see all tools.** Any agent with a valid key can call any tool that key permits. There is no per-tool authorisation.

**Consolidation sends memory content to your configured LLM provider.** Use a local model or disable consolidation if that's unacceptable.
