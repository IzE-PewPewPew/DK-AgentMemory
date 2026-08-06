package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/logx"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

type ctxKey int

const identityKey ctxKey = iota

// identityFrom returns the authenticated caller. It panics when called from a
// route that is not behind requireAuth, which is a programming error and better
// found by the first request in a test than by a nil dereference in production.
func identityFrom(ctx context.Context) store.Identity {
	id, ok := ctx.Value(identityKey).(store.Identity)
	if !ok {
		panic("api: identityFrom called on an unauthenticated route")
	}
	return id
}

// statusWriter records the status and byte count for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so SSE streams are not buffered.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withRequestID assigns an ID to every request and echoes it in a header.
//
// A client-supplied X-Request-ID is honoured so a trace survives a proxy hop,
// but only when it is a plausible identifier: an unbounded client-controlled
// string ends up in log fields and in error bodies.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if len(id) == 0 || len(id) > 64 || strings.ContainsAny(id, "\r\n \t") {
			id = ulid.New()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(logx.WithRequestID(r.Context(), id)))
	})
}

// withLogging emits one structured line per request.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		// livez is polled by uptime checks every few seconds. Logging it at
		// info turns the log into a heartbeat trace and buries everything else.
		level := "info"
		if r.URL.Path == "/v1/livez" {
			level = "debug"
		}

		log := logx.From(r.Context(), s.log)
		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r),
		}
		if id, ok := r.Context().Value(identityKey).(store.Identity); ok {
			args = append(args, "user", id.UserID, "team", id.TeamID)
		}
		if level == "debug" {
			log.Debug("request", args...)
		} else {
			log.Info("request", args...)
		}
	})
}

// withRecovery converts a panic into a 500 with a JSON body.
//
// A panic in one handler must not take the process down and must not leave the
// client holding a connection that closes with no response. The stack goes to
// the log; the client gets a request ID.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec) // the server's own abort signal; let it through
				}
				logx.From(r.Context(), s.log).Error("panic in handler",
					"panic", rec, "method", r.Method, "path", r.URL.Path)
				s.writeError(w, r, internal(errors.New("handler panicked")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireAuth resolves the bearer token to an identity.
//
// This is the only authentication path in the server. `dkm mcp` and the CLI
// reach the same code through the same HTTP surface, so a key that works with
// curl works everywhere -- the alternative, a second code path for the CLI, is
// how "curl works but the CLI 401s" happens.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, authErr := bearerToken(r)
		if authErr != nil {
			s.writeError(w, r, authErr)
			return
		}

		// Refuse to accept a bearer token over plaintext to anything other than
		// loopback. A token sent in the clear is a leaked token, and accepting
		// it once teaches the operator that the setup works.
		if s.cfg.Security.RequireHTTPS && !isSecureRequest(r) {
			s.writeError(w, r, &APIError{
				Status:  http.StatusUpgradeRequired,
				Code:    CodeForbidden,
				Message: "refusing a bearer token over plaintext HTTP",
				Hint:    "put TLS in front of the server, or set security.require_https to false for a loopback-only install",
			})
			return
		}

		id, err := s.store.Authenticate(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrKeyRevoked):
				s.writeError(w, r, &APIError{
					Status: http.StatusUnauthorized, Code: CodeUnauthorized,
					Message: "key revoked",
					Hint:    "ask an admin to issue a new key, then run `dkm login`",
				})
			case errors.Is(err, store.ErrKeyUnknown), errors.Is(err, store.ErrKeyMalformed):
				s.writeError(w, r, &APIError{
					Status: http.StatusUnauthorized, Code: CodeUnauthorized,
					Message: "invalid api key",
					Hint:    "run `dkm login <server-url>` to store a valid key",
				})
			default:
				s.writeError(w, r, internal(err))
			}
			return
		}

		if err := s.limiter.allow(id.KeyID, r.Method); err != nil {
			s.writeError(w, r, err)
			return
		}

		ctx := context.WithValue(r.Context(), identityKey, *id)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin additionally demands the admin flag.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !identityFrom(r.Context()).IsAdmin {
			// 403, not 404. The caller is authenticated, so hiding the route's
			// existence buys nothing and costs them an accurate error.
			s.writeError(w, r, forbidden("this endpoint requires an admin key"))
			return
		}
		next(w, r)
	})
}

func bearerToken(r *http.Request) (string, *APIError) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", &APIError{
			Status: http.StatusUnauthorized, Code: CodeUnauthorized,
			Message: "missing Authorization header",
			Hint:    "send `Authorization: Bearer <api key>`",
		}
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
		return "", &APIError{
			Status: http.StatusUnauthorized, Code: CodeUnauthorized,
			Message: "malformed Authorization header",
			Hint:    "the format is `Authorization: Bearer <api key>`",
		}
	}
	return strings.TrimSpace(token), nil
}

// isSecureRequest reports whether the connection is TLS, terminated by a proxy
// that said so, or loopback.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); strings.EqualFold(proto, "https") {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- rate limiting ---------------------------------------------------------

// limiter is a per-key token bucket, separated into read and write budgets.
//
// Split because the two have different costs and different abuse shapes: a
// runaway hook loop writes, a misconfigured dashboard reads. One shared budget
// would let either starve the other.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	writes  int
	reads   int
}

type bucket struct {
	writeTokens float64
	readTokens  float64
	last        time.Time
}

func newLimiter(writesPerMin, readsPerMin int) *limiter {
	return &limiter{
		buckets: make(map[string]*bucket),
		writes:  writesPerMin,
		reads:   readsPerMin,
	}
}

func (l *limiter) allow(keyID, method string) *APIError {
	isWrite := method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions

	limit := l.reads
	if isWrite {
		limit = l.writes
	}
	if limit <= 0 {
		return nil // 0 means unlimited
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[keyID]
	if !ok {
		b = &bucket{writeTokens: float64(l.writes), readTokens: float64(l.reads), last: now}
		l.buckets[keyID] = b
		if len(l.buckets) > 10000 {
			l.evictLocked(now)
		}
	}

	elapsed := now.Sub(b.last).Minutes()
	b.last = now
	b.writeTokens = minF(float64(l.writes), b.writeTokens+elapsed*float64(l.writes))
	b.readTokens = minF(float64(l.reads), b.readTokens+elapsed*float64(l.reads))

	tokens := &b.readTokens
	if isWrite {
		tokens = &b.writeTokens
	}
	if *tokens < 1 {
		// Retry-After is a real number of seconds until one token exists, not a
		// constant. A client that honours it should succeed on the next try.
		wait := int((1-*tokens)/float64(limit)*60) + 1
		return &APIError{
			Status:  http.StatusTooManyRequests,
			Code:    CodeRateLimited,
			Message: "rate limit exceeded",
			Hint:    "retry after " + strconv.Itoa(wait) + "s, or raise security.rate_limit_*_per_min",
			Headers: map[string]string{"Retry-After": strconv.Itoa(wait)},
		}
	}
	*tokens--
	return nil
}

func (l *limiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
