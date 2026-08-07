# Extracting memories from a conversation

Paste-ready prompts for pulling what an agent has learned into dkm, using the
MCP tools it already has.

This is often better than importing transcripts. `dkm import claude-code` reads
raw session events — every prompt, every tool call — and leaves the judgement to
the consolidation pipeline later. An agent that is still holding the
conversation in context can judge *now*, and it knows which of the forty things
it did actually mattered.

Works in any host with the twelve tools: Claude Desktop, Claude Code, OpenCode,
Cursor, Kimi, Windsurf, or anything else speaking MCP.

---

## The main prompt

Paste this at the end of a conversation worth keeping.

```
Review this entire conversation and save what is worth remembering into my
memory server, using your memory tools.

Save only things that will still be true and useful in three months:
- Decisions, and the reason for each. A decision without its reason cannot be
  revisited later, so if you cannot state why, drop it.
- Constraints that are not visible in the code ("staging has no pgvector",
  "this host runs cloudflared under PM2, not systemd").
- Workarounds, and what made them necessary.
- Conventions someone new to this project would not infer.

Do NOT save:
- Anything the code already states plainly
- File contents, or summaries of file contents
- What we did in what order — that is transcript, not memory
- Things that were true only during this conversation
- Any credential, token, or key, even one that appeared in this chat

Use memory_save for facts, decisions and preferences. Set kind correctly:
  kind="decision"   we chose X over Y, and why
  kind="fact"       something true about this system
  kind="preference" how this project likes things done

Use memory_lesson_save for rules learned from something going wrong. Phrase
them imperatively and keep them to one line: "always X because Y" or
"never X because Y". Put the incident in the body.

Set project to the repository this work belongs to, in the form
github.com/owner/repo. If you cannot determine it, leave it unset rather than
guessing.

Before saving each item, call memory_search first to check whether it is
already there. If it is, do not save a duplicate — call memory_reinforce on the
existing one instead.

When you are done, list what you saved and what you deliberately skipped, so I
can see your judgement.
```

**Why the search-first instruction matters.** Without it, running this on ten
conversations about the same project stores the same three facts ten times.
Exact duplicates are caught by a unique index server-side, but a rephrasing is
not — dedup by meaning happens in the consolidation pipeline, which may not have
run yet.

**Why "list what you skipped".** It is the only way to tell an agent that
correctly found nothing from one that did not really look. Both otherwise
produce silence.

---

## Shorter variants

**One specific thing**, mid-conversation:

```
Save that to my memory as a decision, with the reason we just discussed.
```

**Start of a session**, to load context rather than store it:

```
Before we start: call memory_context for this project, then tell me what you
already know about it.
```

**A rule, right after being corrected:**

```
Save that correction as a lesson so you do not repeat it in a future session.
```

---

## After a batch

Distil the saved memories into lessons:

```bash
dkm consolidate
dkm lessons
```

Lessons marked `*` were synthesised by the pipeline rather than typed by anyone.
Tier 3 needs at least five facts in a project before it will infer a rule, so
run it after several conversations rather than after each one.

---

## Which hosts do this by themselves

| | Extraction prompt | Automatic |
|---|---|---|
| Claude Code | works | ✅ hooks capture and inject with no prompting |
| Claude Desktop | works | ❌ ask it |
| OpenCode, Cursor, Kimi, Windsurf | works | ❌ ask it |

Only Claude Code exposes lifecycle hooks, so only Claude Code captures without
being asked. For every other host this prompt *is* the capture mechanism, and
there is no configuration that changes that — it is a limit of what those hosts
expose to MCP servers.

---

## Checking it worked

```bash
dkm search "something from that conversation"
dkm activity          # which agent contributed what
```

Or open the viewer at `/viewer` and watch the Memories tab; it updates live over
SSE as memories arrive.
