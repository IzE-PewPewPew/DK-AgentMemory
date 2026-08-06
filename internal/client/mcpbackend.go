package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/mcp"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// MCPBackend serves MCP tool calls by talking to the server over HTTP.
//
// This is what `dkm mcp` runs. The same twelve tools exist here and in the
// server's in-process backend, and both end up in the same handlers -- so a
// tool behaves identically whether the agent reached it through a local process
// or over the network.
//
// The one thing this half adds is offline behaviour. A search with the server
// unreachable returns mirror results marked as such, and a save is queued
// rather than lost, because an agent asking its memory a question during a
// flight should get an answer rather than an error.
type MCPBackend struct {
	client *Client
	// defaultProject is resolved once at startup from the directory the host
	// launched the process in. Agents frequently omit the project argument, and
	// falling back to "everything" quietly makes every search cross-project.
	defaultProject string
}

// NewMCPBackend builds the client-side MCP backend.
func NewMCPBackend(c *Client, dir string) *MCPBackend {
	return &MCPBackend{client: c, defaultProject: c.Project(dir).ID}
}

// Call dispatches one tool.
func (b *MCPBackend) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	project := mcp.ArgString(args, "project")
	if project == "" {
		project = b.defaultProject
	}

	switch tool {
	case "memory_search":
		query := mcp.ArgString(args, "query")
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("query is required")
		}
		res, err := b.client.Search(ctx, query, project, mcp.ArgStrings(args, "kinds"), mcp.ArgInt(args, "limit", 8))
		if err != nil {
			return nil, err
		}
		out := map[string]any{"results": res.Results, "count": res.Count, "mode": res.Mode}
		if res.Local {
			out["offline"] = true
			out["notice"] = "the server was unreachable; these are keyword-only results from the local mirror and may be stale"
		}
		return out, nil

	case "memory_save":
		title := mcp.ArgString(args, "title")
		if strings.TrimSpace(title) == "" {
			return nil, fmt.Errorf("title is required")
		}
		res, err := b.client.Save(ctx, store.MemoryInput{
			Kind:       firstNonEmpty(mcp.ArgString(args, "kind"), store.KindFact),
			Title:      title,
			Body:       firstNonEmpty(mcp.ArgString(args, "body"), title),
			Project:    project,
			Files:      mcp.ArgStrings(args, "files"),
			Visibility: mcp.ArgString(args, "visibility"),
		})
		if err != nil {
			return nil, err
		}
		return saveResponse(res), nil

	case "memory_lesson_save":
		lesson := mcp.ArgString(args, "lesson")
		if strings.TrimSpace(lesson) == "" {
			return nil, fmt.Errorf("lesson is required")
		}
		res, err := b.client.SaveLesson(ctx, lesson, mcp.ArgString(args, "body"), project,
			mcp.ArgStrings(args, "files"), mcp.ArgString(args, "visibility"))
		if err != nil {
			return nil, err
		}
		return saveResponse(res), nil

	case "memory_lesson_list":
		lessons, local, err := b.client.Lessons(ctx, project, mcp.ArgInt(args, "limit", 50))
		if err != nil {
			return nil, err
		}
		out := map[string]any{"lessons": lessons, "count": len(lessons)}
		if local {
			out["offline"] = true
		}
		return out, nil

	case "memory_context":
		payload, err := b.client.Context(ctx, project, mcp.ArgInt(args, "budget_tokens", 0))
		if err != nil {
			return nil, err
		}
		if payload.Text == "" {
			payload.Text = "No stored context for this project yet."
		}
		return payload, nil

	case "memory_session_history":
		sessions, err := b.client.Sessions(ctx, project, mcp.ArgInt(args, "limit", 10))
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessions": sessions, "count": len(sessions)}, nil

	case "memory_forget":
		id := mcp.ArgString(args, "id")
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if !mcp.ArgBool(args, "confirm") {
			return nil, fmt.Errorf("confirm must be true to delete a memory; use memory_supersede if it is merely out of date")
		}
		if err := b.client.Forget(ctx, id); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": id, "text": "Deleted memory " + id + "."}, nil

	case "memory_share":
		id := mcp.ArgString(args, "id")
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		mem, err := b.client.Share(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": mem.ID, "visibility": mem.Visibility,
			"text": "Shared with the team: " + mem.Title}, nil

	case "memory_feed":
		mems, err := b.client.Feed(ctx, project, mcp.ArgInt(args, "limit", 20))
		if err != nil {
			return nil, err
		}
		return map[string]any{"memories": mems, "count": len(mems)}, nil

	case "memory_graph":
		g, err := b.client.Graph(ctx, project, mcp.ArgString(args, "node"), mcp.ArgInt(args, "depth", 2))
		if err != nil {
			return nil, err
		}
		text := fmt.Sprintf("%d nodes, %d edges.", len(g.Nodes), len(g.Edges))
		if len(g.Nodes) == 0 {
			text = "No graph for this project yet. Nodes come from real co-occurrence, so an empty graph means nothing has co-occurred."
		}
		return map[string]any{"nodes": g.Nodes, "edges": g.Edges, "text": text}, nil

	case "memory_supersede":
		oldID, newID := mcp.ArgString(args, "old_id"), mcp.ArgString(args, "new_id")
		if oldID == "" || newID == "" {
			return nil, fmt.Errorf("old_id and new_id are both required")
		}
		if err := b.client.Supersede(ctx, oldID, newID); err != nil {
			return nil, err
		}
		return map[string]any{"superseded": oldID, "by": newID,
			"text": "Marked " + oldID + " as superseded by " + newID + "."}, nil

	case "memory_reinforce":
		id := mcp.ArgString(args, "id")
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		mem, err := b.client.Reinforce(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": mem.ID, "strength": mem.Strength, "hits": mem.Hits,
			"text": fmt.Sprintf("Reinforced. Strength is now %.2f after %d uses.", mem.Strength, mem.Hits)}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
}

func saveResponse(res *SaveResult) map[string]any {
	out := map[string]any{
		"id":      res.Memory.ID,
		"kind":    res.Memory.Kind,
		"title":   res.Memory.Title,
		"queued":  res.Queued,
		"created": res.Created,
	}
	switch {
	case res.Queued:
		out["text"] = "The server was unreachable, so this was queued locally as " + res.Memory.ID +
			" and will be sent on the next successful connection. It will not be duplicated."
	default:
		out["text"] = "Saved as " + res.Memory.ID + "."
	}
	return out
}
