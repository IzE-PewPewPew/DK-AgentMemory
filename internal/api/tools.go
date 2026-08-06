package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/mcp"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// ExecuteTool runs one MCP tool against the store.
//
// This is the server-side half of the twelve tools. The client-side half in
// internal/client maps the same names onto REST calls that land in the handlers
// above, so both paths converge on the same store code, the same visibility
// predicate, and the same redaction.
func ExecuteTool(ctx context.Context, s *Server, id store.Identity, tool string, args map[string]any) (any, error) {
	switch tool {

	case "memory_search":
		query := mcp.ArgString(args, "query")
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("query is required")
		}
		results, err := s.store.Search(ctx, id, store.SearchQuery{
			Query:   query,
			Project: mcp.ArgString(args, "project"),
			Kinds:   mcp.ArgStrings(args, "kinds"),
			Limit:   mcp.ArgInt(args, "limit", 0),
			Vector:  s.embedText(ctx, query),
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": results, "count": len(results)}, nil

	case "memory_save":
		in := store.MemoryInput{
			Kind:       orDefault(mcp.ArgString(args, "kind"), store.KindFact),
			Title:      mcp.ArgString(args, "title"),
			Body:       mcp.ArgString(args, "body"),
			Project:    mcp.ArgString(args, "project"),
			Files:      mcp.ArgStrings(args, "files"),
			Visibility: mcp.ArgString(args, "visibility"),
			Source:     store.SourceManual,
		}
		if in.Body == "" {
			in.Body = in.Title
		}
		in.Embedding = s.embedText(ctx, in.Title+"\n"+in.Body)

		mem, created, err := s.store.CreateMemory(ctx, id, in)
		if err != nil {
			return nil, err
		}
		if created {
			s.events.publish("memory.created", mem)
		}
		return map[string]any{
			"id": mem.ID, "kind": mem.Kind, "title": mem.Title,
			"visibility": mem.Visibility, "project": mem.Project,
			"created": created,
			// Saying so explicitly stops an agent re-saving the same fact after
			// reading a response it interpreted as a failure.
			"text": savedText(mem, created),
		}, nil

	case "memory_lesson_save":
		lesson := mcp.ArgString(args, "lesson")
		if strings.TrimSpace(lesson) == "" {
			return nil, fmt.Errorf("lesson is required")
		}
		in := store.MemoryInput{
			Kind:       store.KindLesson,
			Title:      lesson,
			Body:       orDefault(mcp.ArgString(args, "body"), lesson),
			Project:    mcp.ArgString(args, "project"),
			Files:      mcp.ArgStrings(args, "files"),
			Visibility: mcp.ArgString(args, "visibility"),
			Source:     store.SourceManual,
		}
		in.Embedding = s.embedText(ctx, in.Title+"\n"+in.Body)

		mem, created, err := s.store.CreateMemory(ctx, id, in)
		if err != nil {
			return nil, err
		}
		if created {
			s.events.publish("lesson.created", mem)
		}
		return map[string]any{"id": mem.ID, "lesson": mem.Title, "created": created,
			"text": savedText(mem, created)}, nil

	case "memory_lesson_list":
		lessons, err := s.store.Lessons(ctx, id, mcp.ArgString(args, "project"), mcp.ArgInt(args, "limit", 50))
		if err != nil {
			return nil, err
		}
		return map[string]any{"lessons": lessons, "count": len(lessons)}, nil

	case "memory_context":
		payload, err := s.store.BuildContext(ctx, id,
			mcp.ArgString(args, "project"), mcp.ArgInt(args, "budget_tokens", 0), nil)
		if err != nil {
			return nil, err
		}
		if payload.Text == "" {
			payload.Text = "No stored context for this project yet."
		}
		return payload, nil

	case "memory_session_history":
		sessions, err := s.store.ListSessions(ctx, id,
			mcp.ArgString(args, "project"), mcp.ArgInt(args, "limit", 10))
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessions": sessions, "count": len(sessions)}, nil

	case "memory_forget":
		memID := mcp.ArgString(args, "id")
		if memID == "" {
			return nil, fmt.Errorf("id is required")
		}
		if !mcp.ArgBool(args, "confirm") {
			// The confirm flag is not ceremony. Deletion is the one
			// irreversible tool here, and requiring a second explicit field
			// means a malformed or hallucinated call cannot destroy anything.
			return nil, fmt.Errorf("confirm must be true to delete a memory; use memory_supersede if it is merely out of date")
		}
		if err := s.store.DeleteMemory(ctx, id, memID); err != nil {
			return nil, err
		}
		s.events.publish("memory.deleted", map[string]any{"id": memID})
		return map[string]any{"deleted": memID, "text": "Deleted memory " + memID + "."}, nil

	case "memory_share":
		memID := mcp.ArgString(args, "id")
		if memID == "" {
			return nil, fmt.Errorf("id is required")
		}
		mem, err := s.store.Share(ctx, id, memID)
		if err != nil {
			return nil, err
		}
		s.events.publish("memory.shared", mem)
		return map[string]any{"id": mem.ID, "visibility": mem.Visibility,
			"text": "Shared with the team: " + mem.Title}, nil

	case "memory_feed":
		mems, err := s.store.Feed(ctx, id, mcp.ArgString(args, "project"), mcp.ArgInt(args, "limit", 20))
		if err != nil {
			return nil, err
		}
		return map[string]any{"memories": mems, "count": len(mems)}, nil

	case "memory_graph":
		g, err := s.store.GetGraph(ctx, id,
			mcp.ArgString(args, "project"), mcp.ArgString(args, "node"),
			mcp.ArgInt(args, "depth", 2), 200)
		if err != nil {
			return nil, err
		}
		text := fmt.Sprintf("%d nodes, %d edges.", len(g.Nodes), len(g.Edges))
		if len(g.Nodes) == 0 {
			text = "No graph for this project yet. Nodes come from real co-occurrence, so an empty graph means nothing has co-occurred, not that the lookup failed."
		}
		return map[string]any{"nodes": g.Nodes, "edges": g.Edges, "text": text}, nil

	case "memory_supersede":
		oldID, newID := mcp.ArgString(args, "old_id"), mcp.ArgString(args, "new_id")
		if oldID == "" || newID == "" {
			return nil, fmt.Errorf("old_id and new_id are both required")
		}
		if err := s.store.Supersede(ctx, id, oldID, newID); err != nil {
			return nil, err
		}
		s.events.publish("memory.superseded", map[string]any{"id": oldID, "new_id": newID})
		return map[string]any{"superseded": oldID, "by": newID,
			"text": "Marked " + oldID + " as superseded by " + newID + ". Both remain readable; only the newer one appears in search."}, nil

	case "memory_reinforce":
		memID := mcp.ArgString(args, "id")
		if memID == "" {
			return nil, fmt.Errorf("id is required")
		}
		mem, err := s.store.Reinforce(ctx, id, memID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": mem.ID, "strength": mem.Strength, "hits": mem.Hits,
			"text": fmt.Sprintf("Reinforced. Strength is now %.2f after %d uses.", mem.Strength, mem.Hits)}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
}

func savedText(mem *store.Memory, created bool) string {
	if created {
		return "Saved as " + mem.ID + " (" + mem.Kind + ", " + mem.Visibility + ")."
	}
	return "An identical memory already existed: " + mem.ID + ". Nothing was duplicated."
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
