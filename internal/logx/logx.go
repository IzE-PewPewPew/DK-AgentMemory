// Package logx builds the process logger and carries request identity through
// context.
//
// Every log line and every error response carries the same request ID, so a
// user reporting "I got a 500" hands over one string that finds the exact
// request in the server log.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

type ctxKey int

const requestIDKey ctxKey = iota

// New builds a logger. Level is one of debug|info|warn|error and format is
// json|text; both are validated by the config loader before reaching here, so
// an invalid value is a programming error rather than user input.
func New(level, format string, w io.Writer) (*slog.Logger, error) {
	if w == nil {
		w = os.Stderr
	}

	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info", "":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("log.level: unknown level %q (want debug, info, warn or error)", level)
	}

	opts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	switch strings.ToLower(format) {
	case "json", "":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("log.format: unknown format %q (want json or text)", format)
	}

	return slog.New(h).With("service", "dkm", "version", version.Short()), nil
}

// Discard returns a logger that writes nothing. Used by tests and by `dkm mcp`,
// where stdout is the JSON-RPC transport and stray output corrupts the protocol.
func Discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// WithRequestID attaches a request ID to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID attached to ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// From returns a logger tagged with the context's request ID.
func From(ctx context.Context, base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	if id := RequestID(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}
