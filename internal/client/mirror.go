package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// Mirror is the local half of offline operation: a read cache and a write queue.
//
// The design rests on one property. Memories are append-only and corrections
// supersede rather than edit, so there is nothing to merge when a machine comes
// back online. Two people correcting the same fact while both offline produce
// two memories and one supersede chain, not a conflict. Every hard part of
// offline sync is a consequence of in-place mutation, and this store does not
// have any.
//
// Storage is NDJSON in a directory rather than an embedded database. It is
// inspectable with `cat`, repairable with a text editor, and adds no cgo
// dependency to a binary whose main promise is that it runs anywhere. The
// corpora involved -- tens of thousands of rows -- do not need a query planner.
type Mirror struct {
	dir      string
	queueMax int
	mu       sync.Mutex
}

const (
	memoriesFile = "memories.ndjson"
	queueFile    = "queue.ndjson"
	cursorFile   = "cursor"
)

// NewMirror opens (and creates) a mirror directory.
func NewMirror(dir string, queueMax int) (*Mirror, error) {
	if dir == "" {
		return nil, fmt.Errorf("mirror path is empty")
	}
	if queueMax < 1 {
		queueMax = 1000
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating mirror directory %s: %w", dir, err)
	}
	return &Mirror{dir: dir, queueMax: queueMax}, nil
}

// Dir returns the mirror location, for `dkm doctor`.
func (m *Mirror) Dir() string { return m.dir }

func (m *Mirror) path(name string) string { return filepath.Join(m.dir, name) }

// --- cached memories -------------------------------------------------------

// Memories reads the whole local mirror.
func (m *Mirror) Memories() ([]store.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readMemories()
}

func (m *Mirror) readMemories() ([]store.Memory, error) {
	f, err := os.Open(m.path(memoriesFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128<<10), 8<<20)

	var out []store.Memory
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var mem store.Memory
		if err := json.Unmarshal([]byte(line), &mem); err != nil {
			// A corrupt line is skipped rather than fatal. The mirror is a
			// cache: losing one row costs a re-sync, while refusing to start
			// costs the user their offline reads entirely.
			continue
		}
		out = append(out, mem)
	}
	return out, sc.Err()
}

// Apply merges a batch of changes into the mirror.
//
// Deletions remove the row rather than tombstoning it. The server remains the
// authority on what exists; a local tombstone would only matter if the mirror
// were also a source of truth, and it deliberately is not.
func (m *Mirror) Apply(changes []store.Change) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.readMemories()
	if err != nil {
		return err
	}

	index := make(map[string]int, len(existing))
	for i, mem := range existing {
		index[mem.ID] = i
	}

	for _, ch := range changes {
		switch {
		case ch.Deleted:
			if i, ok := index[ch.ID]; ok {
				existing[i].ID = "" // marked for removal in the compaction below
			}
		default:
			if i, ok := index[ch.ID]; ok {
				existing[i] = ch.Memory
			} else {
				existing = append(existing, ch.Memory)
				index[ch.ID] = len(existing) - 1
			}
		}
	}

	kept := existing[:0]
	for _, mem := range existing {
		if mem.ID != "" {
			kept = append(kept, mem)
		}
	}
	return m.writeMemories(kept)
}

func (m *Mirror) writeMemories(mems []store.Memory) error {
	return writeAtomic(m.path(memoriesFile), func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		for _, mem := range mems {
			if err := enc.Encode(mem); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- local search ----------------------------------------------------------

// Search scores the local mirror when the server is unreachable.
//
// A deliberately simple BM25 variant, not an attempt to reproduce the server's
// hybrid ranking. There is no embedder offline, so semantic recall is gone
// whatever this does; what remains is finding the memory whose words match. The
// results carry Score so a caller can tell offline results from online ones.
func (m *Mirror) Search(query, project string, limit int) ([]store.SearchResult, error) {
	mems, err := m.Memories()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 8
	}

	terms := tokenise(query)
	if len(terms) == 0 {
		return nil, nil
	}

	// Document frequency across the corpus, for the idf term.
	df := map[string]int{}
	docs := make([][]string, len(mems))
	for i, mem := range mems {
		docs[i] = tokenise(mem.Title + " " + mem.Body + " " + strings.Join(mem.Files, " "))
		seen := map[string]bool{}
		for _, t := range docs[i] {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}

	const (
		k1 = 1.2
		b  = 0.75
	)
	avgLen := 0.0
	for _, d := range docs {
		avgLen += float64(len(d))
	}
	if len(docs) > 0 {
		avgLen /= float64(len(docs))
	}
	if avgLen == 0 {
		avgLen = 1
	}

	N := float64(len(mems))
	var scored []store.SearchResult

	for i, mem := range mems {
		if mem.DeletedAt != nil || mem.SupersededBy != nil {
			continue
		}
		if project != "" && mem.Project != "" && mem.Project != project {
			continue
		}

		tf := map[string]int{}
		for _, t := range docs[i] {
			tf[t]++
		}
		titleTerms := map[string]bool{}
		for _, t := range tokenise(mem.Title) {
			titleTerms[t] = true
		}

		score := 0.0
		for _, term := range terms {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (N-float64(df[term])+0.5)/(float64(df[term])+0.5))
			norm := f * (k1 + 1) / (f + k1*(1-b+b*float64(len(docs[i]))/avgLen))
			weight := 1.0
			if titleTerms[term] {
				// A term in the title is a stronger signal than the same term
				// buried in a body, matching the server's tsvector weighting.
				weight = 2.0
			}
			score += idf * norm * weight
		}
		if score <= 0 {
			continue
		}

		score *= math.Max(mem.Strength, 0.05)
		scored = append(scored, store.SearchResult{Memory: mem, Score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].ID > scored[j].ID
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// isTokenRune reports whether a character belongs inside a search token.
//
// Dots and slashes are included so `src/auth.ts` and `pprp-wallet-api` survive
// as single tokens. Splitting on them would turn the identifiers people
// actually search for into fragments that match everything.
func isTokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= '0' && r <= '9' ||
		r == '-' || r == '_' || r == '.' || r == '/'
}

func tokenise(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !isTokenRune(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-_./")
		if len(f) > 1 && !stopWords[f] {
			out = append(out, f)
		}
	}
	return out
}

// stopWords are dropped from local scoring. Short and English-only on purpose:
// an aggressive list would strip terms that are meaningful in code, and "go"
// or "if" being a stop word is exactly the sort of thing that makes a search
// look broken.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true,
	"you": true, "all": true, "can": true, "her": true, "was": true, "one": true,
	"our": true, "out": true, "his": true, "has": true, "had": true, "were": true,
	"they": true, "this": true, "that": true, "with": true, "from": true, "have": true,
	"been": true, "will": true, "would": true, "there": true, "their": true, "what": true,
	"about": true, "which": true, "when": true, "into": true,
}

// --- write queue -----------------------------------------------------------

// QueuedWrite is a memory created while the server was unreachable.
//
// The ID is generated here, on the client, at the moment the user ran the
// command. That is what makes flushing idempotent: the row's primary key exists
// before the server has ever heard of it, so a flush interrupted halfway and
// retried inserts the same key and the second attempt is a no-op. Without a
// client-generated ID, the only way to avoid duplicates would be an
// acknowledgement the client never received.
type QueuedWrite struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Project    string    `json:"project,omitempty"`
	Files      []string  `json:"files,omitempty"`
	Visibility string    `json:"visibility,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
}

// Enqueue appends a write to the queue, assigning an ID if there is none.
func (m *Mirror) Enqueue(w QueuedWrite) (QueuedWrite, error) {
	if w.ID == "" {
		w.ID = ulid.New()
	}
	if w.QueuedAt.IsZero() {
		w.QueuedAt = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	queued, err := m.readQueue()
	if err != nil {
		return w, err
	}
	if len(queued) >= m.queueMax {
		return w, fmt.Errorf(
			"offline queue is full (%d writes, sync.queue_max)\n"+
				"  Reconnect and run `dkm push`, or raise sync.queue_max in %s",
			len(queued), config.ClientPath())
	}

	f, err := os.OpenFile(m.path(queueFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return w, err
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(w); err != nil {
		return w, err
	}
	return w, f.Sync()
}

// Queue returns everything waiting to be flushed.
func (m *Mirror) Queue() ([]QueuedWrite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readQueue()
}

func (m *Mirror) readQueue() ([]QueuedWrite, error) {
	f, err := os.Open(m.path(queueFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128<<10), 8<<20)

	var out []QueuedWrite
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var w QueuedWrite
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			continue
		}
		out = append(out, w)
	}
	return out, sc.Err()
}

// Dequeue removes the given IDs from the queue.
//
// Called only after the server has accepted each write. An entry that failed
// stays queued and is retried, and because the ID travelled with it, the retry
// cannot produce a second row.
func (m *Mirror) Dequeue(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	done := make(map[string]bool, len(ids))
	for _, id := range ids {
		done[id] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	queued, err := m.readQueue()
	if err != nil {
		return err
	}

	remaining := queued[:0]
	for _, w := range queued {
		if !done[w.ID] {
			remaining = append(remaining, w)
		}
	}

	if len(remaining) == 0 {
		return os.Remove(m.path(queueFile))
	}
	return writeAtomic(m.path(queueFile), func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		for _, q := range remaining {
			if err := enc.Encode(q); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- cursor ----------------------------------------------------------------

// Cursor returns the sync position, or "" when nothing has been synced.
func (m *Mirror) Cursor() string {
	data, err := os.ReadFile(m.path(cursorFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// SetCursor records the sync position.
func (m *Mirror) SetCursor(c string) error {
	if c == "" {
		return nil
	}
	return os.WriteFile(m.path(cursorFile), []byte(c+"\n"), 0o600)
}

// --- helpers ---------------------------------------------------------------

// writeAtomic replaces a file via a temporary file and a rename.
//
// A partially written mirror after a laptop lid closes is worse than a stale
// one: the stale file is correct as of its timestamp, while a truncated file is
// correct as of nothing.
func writeAtomic(path string, fn func(*bufio.Writer) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removed on every path that does not reach the rename below, so a failed
	// write leaves the previous file intact and no debris beside it.
	defer func() { _ = os.Remove(tmpName) }()

	bw := bufio.NewWriterSize(tmp, 64<<10)
	if err := fn(bw); err != nil {
		// The close error is dropped in each of these: the failure already in
		// hand is the one worth reporting, and the deferred Remove cleans up
		// regardless of how the close went.
		_ = tmp.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
