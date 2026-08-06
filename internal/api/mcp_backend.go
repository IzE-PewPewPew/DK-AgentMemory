package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/mcp"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// mcpBackend serves MCP tool calls directly from the store, for agents
// connecting to the server over streamable HTTP rather than through a local
// `dkm mcp` process.
//
// It shares the store, the redaction path, and the identity resolved by the
// same bearer middleware every other route uses. A remote agent therefore sees
// exactly what a local one does -- no second permission model, no second set of
// defaults.
type mcpBackend struct{ s *Server }

// MCPHandler returns an http.Handler serving MCP over streamable HTTP.
//
// It is not mounted by New; the caller passes it in as Options.MCPHandler,
// which keeps the wiring visible in cmd/dkm rather than hidden here.
func (s *Server) MCPHandler() http.Handler {
	return mcp.NewServer(&mcpBackend{s: s}, s.log).HTTPHandler()
}

func (b *mcpBackend) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	id, ok := ctx.Value(identityKey).(store.Identity)
	if !ok {
		return nil, fmt.Errorf("not authenticated")
	}
	return ExecuteTool(ctx, b.s, id, tool, args)
}
