// Package mcp implements the Model Context Protocol server: the twelve tools an
// agent sees, the JSON-RPC framing, and both transports.
//
// One implementation serves stdio and streamable HTTP. The transports differ
// only in how bytes arrive; the dispatch, the tool list, and the schemas are
// shared, so a tool cannot behave differently depending on how the agent
// connected.
package mcp

import (
	"context"
	"encoding/json"
)

// Tool is one callable exposed to agents.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Annotations describe side effects so a host can decide what needs
	// confirmation. A tool that deletes should never look like one that reads.
	Annotations map[string]any `json:"annotations,omitempty"`
}

// Backend executes a tool call.
//
// A single dispatch method rather than twelve interface methods, so the two
// implementations -- one talking to the store in-process, one talking to the
// REST API over HTTP -- stay small and the tool list has exactly one
// definition.
type Backend interface {
	Call(ctx context.Context, tool string, args map[string]any) (any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	} else {
		s["required"] = []string{}
	}
	// Reject unknown arguments. A model that invents `filters` should be told,
	// not silently ignored -- the silent case looks like the tool not working.
	s["additionalProperties"] = false
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func strArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// Tools is the complete list. Twelve, deliberately.
//
// An agent picks from this list on every turn, and a list of fifty is a list it
// reads past. Each of these answers a question an agent actually has: what do I
// already know, what did we decide, what did we learn, what happened last time.
// Anything that could be phrased as an argument to one of these is an argument,
// not a thirteenth tool.
var Tools = []Tool{
	{
		Name: "memory_search",
		Description: "Search stored memories with hybrid keyword and semantic retrieval. " +
			"Use this before answering questions about how this project works, why something was built a certain way, " +
			"or what was decided previously. Paraphrases work — the query does not need to share words with the memory.",
		InputSchema: obj(map[string]any{
			"query":   str("What you want to know, in natural language."),
			"project": str("Project identity such as github.com/org/repo. Omit to search everything visible."),
			"kinds":   strArray("Restrict to some of: fact, decision, lesson, preference."),
			"limit":   integer("Maximum results. Default 8."),
		}, "query"),
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		Name: "memory_save",
		Description: "Store a durable fact, decision, or preference. " +
			"Save things that will still be true next week: a decision and its reason, a non-obvious constraint, " +
			"a workaround and why it was needed. Do not save transient state, file contents, or anything the code already says.",
		InputSchema: obj(map[string]any{
			"title":      str("One line stating the fact or decision."),
			"body":       str("The detail, and above all the reason. A decision without its reason cannot be revisited."),
			"kind":       enum("What sort of memory this is. Default fact.", "fact", "decision", "preference"),
			"project":    str("Project identity. Omit to use the caller's current project."),
			"files":      strArray("Files this relates to, as repo-relative paths."),
			"visibility": enum("private keeps it to you; team shares it. Default private.", "private", "team"),
		}, "title"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
	{
		Name: "memory_lesson_save",
		Description: "Store a durable rule learned from experience, in the form \"always X because Y\" or \"never X because Y\". " +
			"Use this after something went wrong and you worked out the general rule. Lessons are injected into future sessions, " +
			"so they should be short, imperative, and worth reading every time.",
		InputSchema: obj(map[string]any{
			"lesson":     str("The rule, imperative and one line."),
			"body":       str("Why — the incident or reasoning behind it."),
			"project":    str("Project identity. Omit for a lesson that applies everywhere."),
			"files":      strArray("Files the lesson relates to."),
			"visibility": enum("private or team. Default private.", "private", "team"),
		}, "lesson"),
		Annotations: map[string]any{"readOnlyHint": false, "idempotentHint": true},
	},
	{
		Name: "memory_lesson_list",
		Description: "List the lessons that apply here, strongest first. " +
			"Worth calling at the start of work in an unfamiliar area.",
		InputSchema: obj(map[string]any{
			"project": str("Project identity. Omit for everything visible."),
			"limit":   integer("Maximum lessons. Default 50."),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "memory_context",
		Description: "Get a token-budgeted briefing for a project: active lessons, then decisions, then recent session summaries. " +
			"Call this once when starting work on a project you have no context for.",
		InputSchema: obj(map[string]any{
			"project":       str("Project identity."),
			"budget_tokens": integer("Roughly how many tokens to spend. Default 1500."),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "memory_session_history",
		Description: "List recent sessions and their summaries. Use this to answer \"what was I doing last time\" " +
			"or to find when a change was made.",
		InputSchema: obj(map[string]any{
			"project": str("Project identity."),
			"limit":   integer("Maximum sessions. Default 10."),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "memory_forget",
		Description: "Delete a memory. Use this only when the user explicitly asks to forget something. " +
			"If a memory is merely out of date, use memory_supersede instead, which keeps the history.",
		InputSchema: obj(map[string]any{
			"id":      str("The memory ID to delete."),
			"confirm": boolean("Must be true. Present so a deletion cannot happen through a malformed call."),
		}, "id", "confirm"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
	},
	{
		Name:        "memory_share",
		Description: "Promote one of your private memories to team visibility. Everyone on the team can then read it.",
		InputSchema: obj(map[string]any{
			"id": str("The memory ID to share."),
		}, "id"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name:        "memory_feed",
		Description: "Show what teammates have shared recently.",
		InputSchema: obj(map[string]any{
			"project": str("Project identity."),
			"limit":   integer("Maximum items. Default 20."),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "memory_graph",
		Description: "Show entities related to a file or memory, derived from real co-occurrence in the stored corpus. " +
			"An empty result means nothing has co-occurred yet, not that the lookup failed.",
		InputSchema: obj(map[string]any{
			"project": str("Project identity."),
			"node":    str("Seed label, usually a file path. Omit for the whole project graph."),
			"depth":   integer("How many hops from the seed. Default 2."),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "memory_supersede",
		Description: "Mark an old memory as replaced by a newer one. " +
			"This is the right tool when something was true and no longer is: both statements survive, in order, " +
			"and only the newer one appears in search.",
		InputSchema: obj(map[string]any{
			"old_id": str("The memory that is now out of date."),
			"new_id": str("The memory that replaces it."),
		}, "old_id", "new_id"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name: "memory_reinforce",
		Description: "Mark a memory as having been useful, raising how highly it ranks in future searches. " +
			"Call this when a retrieved memory actually answered the question.",
		InputSchema: obj(map[string]any{
			"id": str("The memory ID that proved useful."),
		}, "id"),
		Annotations: map[string]any{"readOnlyHint": false, "idempotentHint": false},
	},
}

// ToolByName finds a tool definition.
func ToolByName(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// The Arg* helpers read tool arguments defensively.
//
// Arguments come from a language model, and models are inconsistent about types
// in ways a strict decoder would reject: a limit arrives as 8, as 8.0, or as
// "8". Rejecting those produces a tool that works for one model and not
// another, so each is accepted and normalised here, once, rather than in twelve
// places.

// ArgString reads a string argument, or "" when absent.
func ArgString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// ArgInt reads a numeric argument, falling back to def.
func ArgInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		var n int
		if err := json.Unmarshal([]byte(v), &n); err == nil {
			return n
		}
	}
	return def
}

// ArgBool reads a boolean argument.
func ArgBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// ArgStrings reads an array-of-strings argument, ignoring non-string elements.
func ArgStrings(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		// A model sending a single string where an array is expected is
		// common enough to be worth accepting.
		if s, ok := args[key].(string); ok && s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
