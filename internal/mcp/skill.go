package mcp

import _ "embed"

// Skill is the SKILL.md shipped alongside the tools.
//
// Twelve tools tell an agent what it *can* do. This tells it *when* — which is
// the part that decides whether a memory system gets used or ignored. Without
// it, agents search rarely and save either nothing or everything, and both
// failure modes look like the tools not working.
//
// `dkm connect` installs it for hosts that support skills, and it is returned
// in the MCP initialize response as `instructions` for hosts that do not.
//
//go:embed SKILL.md
var Skill string

// Instructions is the shorter form sent during initialize.
//
// Deliberately not the whole SKILL.md: initialize instructions are prepended to
// the host's system prompt and paid for on every single turn, so this is the
// irreducible version.
const Instructions = `This server gives you memory that persists across sessions and is shared with every other AI tool on this machine.

Before answering a question about how this project works, why something was built a certain way, or what was decided previously, call memory_search. It is hybrid search, so paraphrases match.

After learning something that will still be true next week — a decision and its reason, a constraint the code does not show, a workaround and why it was needed — call memory_save. Always record the reason; a decision without one cannot be revisited.

When something goes wrong and you work out the general rule, call memory_lesson_save. Lessons are injected into future sessions, so keep them short and imperative.

When a retrieved memory answered the question, call memory_reinforce with its ID. Ranking depends on that signal.

Do not store file contents, transient state, or anything the code already says. Correct outdated memories with memory_supersede rather than memory_forget, so the history survives.`
