---
name: memory
description: Shared, persistent memory across every AI tool on this machine and across the team. Use it to recall past decisions and conventions before answering, and to record durable facts, decisions, and lessons after learning something that will still matter next week.
---

# Memory

You have a memory that outlives this session, is shared with every other AI tool
on this machine, and — for anything marked team — with the rest of the team.

## Recall before you guess

Search before answering a question about how this project works, why something
is built the way it is, or what was decided before. It is faster than reading
the code and it surfaces reasons the code cannot contain.

```
memory_search   query: "why do we use jose instead of jsonwebtoken"
```

The search is hybrid, so a paraphrase finds the memory even when it shares no
words with it. Do not restrict yourself to keywords you expect to appear
verbatim.

Starting work in an unfamiliar project:

```
memory_context  project: "github.com/org/repo"
```

That returns lessons first, then decisions, then recent session summaries, inside
a token budget.

## Save what will still be true next week

After a decision, a discovery, or a non-obvious constraint:

```
memory_save     title: "cloudflared runs under PM2 here, not systemd"
                body:  "systemctl restart cloudflared exits 0 and changes nothing.
                        The real process is PM2 id 3."
                kind:  "decision"
```

**Always record the reason.** A decision without its reason cannot be revisited
later; all anyone can do is re-litigate it from scratch.

Worth saving:
- Decisions and the reasoning behind them
- Constraints that are not visible in the code ("the staging DB has no pgvector")
- Workarounds and what made them necessary
- Conventions the team follows that a newcomer would not infer

Not worth saving:
- Anything the code already says
- File contents, or summaries of file contents
- Transient state: what you were about to do, what is currently failing
- Restatements of the user's request

If you are unsure, ask whether someone would benefit from this in three months.

## Lessons are rules, not notes

When something went wrong and you worked out the general rule:

```
memory_lesson_save  lesson: "always use full paths with pkill on multi-service hosts"
                    body:   "pkill cloudflared killed an unrelated service that had
                             cloudflared in its argv."
```

Lessons are injected into future sessions, so keep them short and imperative.
A lesson that reads like a paragraph will be skimmed past.

## Correct, do not overwrite

When something was true and no longer is, save the new memory and then:

```
memory_supersede  old_id: "01J..."  new_id: "01J..."
```

Both survive, in order, and only the newer one appears in search. Use
`memory_forget` only when the user explicitly asks to delete something.

## Close the loop

When a retrieved memory actually answered the question, call `memory_reinforce`
with its ID. Ranking depends on it: memories that prove useful rise, and
memories nobody ever uses fade. Skipping this is why a memory store slowly
becomes a store of whatever happened to be saved first.

## Privacy

Memories are private by default. `visibility: "team"` shares at write time, and
`memory_share` promotes an existing one.

Credentials are stripped automatically before anything is stored, but do not
rely on that as permission: never deliberately save a secret, and never paste
one into a memory body to "keep it handy".
