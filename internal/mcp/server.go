package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Server implements the MCP protocol over a Backend.
type Server struct {
	backend Backend
	log     *slog.Logger
	name    string
}

// NewServer builds an MCP server.
//
// The logger must not write to stdout. In stdio mode stdout is the transport,
// and a single stray log line makes the host disconnect with a parse error --
// which surfaces to the user as "the memory server has no tools" and gives no
// clue why.
func NewServer(backend Backend, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Server{backend: backend, log: log, name: "dkm"}
}

// handle dispatches one request and returns the response, or nil for a
// notification (a request with no ID, which must not be answered).
func (s *Server) handle(ctx context.Context, req *request) *response {
	isNotification := len(req.ID) == 0

	reply := func(result any) *response {
		if isNotification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string, data any) *response {
		if isNotification {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg, Data: data}}
	}

	switch req.Method {
	case "initialize":
		// Echo the client's protocol version when it sends one. Hosts pin
		// different revisions, and refusing to speak a version that differs
		// only in date is how a working server looks broken.
		negotiated := ProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion != "" {
				negotiated = p.ProtocolVersion
			}
		}
		return reply(map[string]any{
			"protocolVersion": negotiated,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    s.name,
				"title":   "DevKuong Memories",
				"version": version.Short(),
			},
			"instructions": Instructions,
		})

	case "notifications/initialized", "initialized":
		return nil

	case "ping":
		return reply(map[string]any{})

	case "tools/list":
		return reply(map[string]any{"tools": Tools})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(codeInvalidParams, "params must be an object with name and arguments", err.Error())
		}
		if _, ok := ToolByName(p.Name); !ok {
			return fail(codeMethodNotFound, "unknown tool "+p.Name, map[string]any{"available": toolNames()})
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}

		result, err := s.backend.Call(ctx, p.Name, p.Arguments)
		if err != nil {
			// A failing tool is reported as a successful JSON-RPC response
			// carrying isError, not as a protocol error. The distinction
			// matters: a protocol error tells the host the server is broken,
			// while isError tells the model its call did not work and it may
			// try something else.
			s.log.Debug("tool call failed", "tool", p.Name, "error", err)
			return reply(toolResult(err.Error(), nil, true))
		}
		return reply(toolResult(renderText(p.Name, result), result, false))

	case "resources/list":
		return reply(map[string]any{"resources": []any{}})
	case "prompts/list":
		return reply(map[string]any{"prompts": []any{}})

	default:
		return fail(codeMethodNotFound, "unknown method "+req.Method, nil)
	}
}

func toolNames() []string {
	out := make([]string, len(Tools))
	for i, t := range Tools {
		out[i] = t.Name
	}
	return out
}

// toolResult builds the content block MCP expects.
//
// Both a human-readable text block and the structured payload are returned.
// Hosts that understand structuredContent get real data; hosts that do not
// still show the model something it can read, rather than a JSON blob it has to
// parse in-context.
func toolResult(text string, structured any, isError bool) map[string]any {
	out := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
	if structured != nil && !isError {
		out["structuredContent"] = structured
	}
	return out
}

// --- stdio transport -------------------------------------------------------

// ServeStdio runs the protocol over newline-delimited JSON on stdin/stdout.
//
// This is how every local MCP host launches a server: spawn the process, write
// requests to its stdin, read responses from its stdout. Nothing else may be
// written to stdout for the lifetime of the process.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	writer := bufio.NewWriter(out)
	var mu sync.Mutex

	send := func(resp *response) error {
		if resp == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewEncoder(writer).Encode(resp); err != nil {
			return err
		}
		return writer.Flush()
	}

	for {
		if ctx.Err() != nil {
			return nil
		}

		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the host closed the pipe; a normal exit
			}
			return err
		}
		line = trimJSONLine(line)
		if len(line) == 0 {
			if err != nil {
				return nil
			}
			continue
		}

		var req request
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			if sendErr := send(&response{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: codeParseError, Message: "invalid JSON: " + jsonErr.Error()},
			}); sendErr != nil {
				return sendErr
			}
			continue
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			if sendErr := send(&response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: codeInvalidRequest, Message: "jsonrpc must be \"2.0\""},
			}); sendErr != nil {
				return sendErr
			}
			continue
		}

		if sendErr := send(s.handle(ctx, &req)); sendErr != nil {
			return sendErr
		}
		if err != nil {
			return nil
		}
	}
}

func trimJSONLine(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// --- streamable HTTP transport ---------------------------------------------

// HTTPHandler serves MCP over streamable HTTP, so a remote agent can use the
// same twelve tools without installing anything locally.
//
// Authentication is the caller's problem: this handler is mounted behind the
// API's own bearer middleware, so there is one auth path rather than a second
// one that drifts.
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		case http.MethodGet:
			// The spec allows a GET to open a server-to-client stream. Nothing
			// here pushes unsolicited messages, so saying so is more useful
			// than holding a connection open forever with nothing on it.
			w.Header().Set("Allow", "POST")
			writeRPCError(w, http.StatusMethodNotAllowed, codeInvalidRequest,
				"this server does not push unsolicited messages; POST requests to this endpoint")
			return
		case http.MethodDelete:
			// Session teardown. There is no server-side session state to tear
			// down, so this succeeds trivially.
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			w.Header().Set("Allow", "POST, DELETE")
			writeRPCError(w, http.StatusMethodNotAllowed, codeInvalidRequest, "method not allowed")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeRPCError(w, http.StatusBadRequest, codeParseError, "could not read the request body")
			return
		}

		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" {
			writeRPCError(w, http.StatusBadRequest, codeInvalidRequest, "empty request body")
			return
		}

		// A batch arrives as a JSON array. Each element gets its own response,
		// and notifications contribute nothing to the result.
		if trimmed[0] == '[' {
			var reqs []request
			if err := json.Unmarshal([]byte(trimmed), &reqs); err != nil {
				writeRPCError(w, http.StatusBadRequest, codeParseError, "invalid JSON batch: "+err.Error())
				return
			}
			out := make([]*response, 0, len(reqs))
			for i := range reqs {
				if resp := s.handle(r.Context(), &reqs[i]); resp != nil {
					out = append(out, resp)
				}
			}
			if len(out) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, http.StatusOK, out)
			return
		}

		var req request
		if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
			writeRPCError(w, http.StatusBadRequest, codeParseError, "invalid JSON: "+err.Error())
			return
		}

		resp := s.handle(r.Context(), &req)
		if resp == nil {
			// A notification has no response. 202 is the spec's answer.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRPCError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, response{
		JSONRPC: "2.0",
		Error:   &rpcError{Code: code, Message: msg},
	})
}

// renderText turns a structured result into something a model reads well.
//
// Handing a model raw JSON costs tokens and attention on syntax rather than
// content. A short list of titles is what the model needs to decide what to do
// next; the structured payload is still attached for hosts that use it.
func renderText(tool string, result any) string {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}

	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		return string(data)
	}

	for _, key := range []string{"results", "memories", "lessons", "sessions"} {
		items, ok := generic[key].([]any)
		if !ok {
			continue
		}
		if len(items) == 0 {
			return "No " + key + " found."
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d %s:\n", len(items), key)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			title, _ := m["title"].(string)
			if title == "" {
				title, _ = m["summary"].(string)
			}
			kind, _ := m["kind"].(string)
			id, _ := m["id"].(string)
			line := "- "
			if kind != "" {
				line += "[" + kind + "] "
			}
			line += title
			if body, _ := m["body"].(string); body != "" && body != title {
				line += "\n  " + firstLines(body, 3)
			}
			if id != "" {
				line += "\n  id: " + id
			}
			b.WriteString(line + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}

	if text, ok := generic["text"].(string); ok && text != "" {
		return text
	}
	return string(data)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n  ")
}
