package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Server-sent events, not WebSocket.
//
// SSE is plain HTTP with a long-lived response body. It traverses Cloudflare
// Tunnel, nginx, Caddy, and corporate proxies with no upgrade handshake to
// misroute and no per-hop configuration. It reconnects on its own -- the
// EventSource contract, not something to implement. And it is one-directional,
// which is exactly what a read-only viewer needs.
//
// A WebSocket would add a handshake that proxies rewrite, a heartbeat to write,
// a reconnect loop to write, and a second failure mode where the page loads but
// never updates. For pushing "a memory appeared" to a dashboard, none of that
// buys anything.

// event is one published message.
type event struct {
	Name string
	Data []byte
	ID   string
}

type subscriber struct {
	ch     chan event
	closed bool
}

// eventHub fans published events out to connected viewers.
type eventHub struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	closed bool
	seq    uint64
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[*subscriber]struct{})}
}

// publish sends an event to every subscriber.
//
// Never blocks. A subscriber whose buffer is full is skipped rather than waited
// on: a viewer left open on a laptop that went to sleep must not be able to
// stall the request that created a memory.
func (h *eventHub) publish(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.seq++
	ev := event{Name: name, Data: data, ID: fmt.Sprint(h.seq)}
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
		}
	}
}

func (h *eventHub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan event, 64)}
	h.mu.Lock()
	if !h.closed {
		h.subs[s] = struct{}{}
	}
	h.mu.Unlock()
	return s
}

func (h *eventHub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
}

func (h *eventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
	h.subs = make(map[*subscriber]struct{})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return unavailable("this server build cannot stream events", nil)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tells nginx not to buffer the stream. Without it the page connects
	// successfully and then shows nothing, which is the hardest version of this
	// bug to diagnose.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.subscribe()
	defer s.events.unsubscribe(sub)

	// retry tells EventSource how long to wait before reconnecting. Set
	// explicitly rather than left to the browser default so a server restart
	// brings viewers back within a known window.
	fmt.Fprintf(w, "retry: 3000\n\n")
	fmt.Fprintf(w, "event: hello\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	// Comment frames every 25 seconds. Idle timeouts in proxies are commonly 30
	// or 60 seconds, and a stream with nothing to say looks identical to a dead
	// one until something is written.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case ev, open := <-sub.ch:
			if !open {
				return nil
			}
			if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, ev.Data); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func (s *Server) subscribe() *subscriber { return s.events.subscribe() }
