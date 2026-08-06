package store

import (
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
)

// Memory kinds. These are a closed set enforced by a CHECK constraint, because
// an open vocabulary here becomes forty synonyms for "note" within a month.
const (
	KindFact       = "fact"
	KindDecision   = "decision"
	KindLesson     = "lesson"
	KindPreference = "preference"
)

// Visibility values.
const (
	VisibilityPrivate = "private"
	VisibilityTeam    = "team"
)

// Memory sources, recording how a row came to exist.
const (
	SourceManual        = "manual"
	SourceConsolidation = "consolidation"
	SourceImport        = "import"
	SourceHook          = "hook"
)

// ValidKind reports whether k is a permitted memory kind.
func ValidKind(k string) bool {
	switch k {
	case KindFact, KindDecision, KindLesson, KindPreference:
		return true
	}
	return false
}

// ValidVisibility reports whether v is a permitted visibility.
func ValidVisibility(v string) bool {
	return v == VisibilityPrivate || v == VisibilityTeam
}

// Team is a namespace. Every row in the system belongs to exactly one.
type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// User belongs to one team and owns keys, sessions, and memories.
type User struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Name      string    `json:"name"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey is the metadata of an issued key. The secret is never in this struct;
// it exists only in the response to the issuing call.
type APIKey struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Prefix     string     `json:"prefix"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Revoked reports whether the key has been revoked.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// Identity is the authenticated caller, resolved once per request and passed
// down. Every store call that touches tenant data takes one of these rather
// than a bare team ID, so there is no way to call a query without having
// established who is asking.
type Identity struct {
	KeyID    string `json:"key_id"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	TeamID   string `json:"team_id"`
	IsAdmin  bool   `json:"is_admin"`
}

// Session is one agent run.
type Session struct {
	ID               string         `json:"id"`
	TeamID           string         `json:"team_id"`
	UserID           string         `json:"user_id"`
	Project          string         `json:"project"`
	Agent            string         `json:"agent"`
	StartedAt        time.Time      `json:"started_at"`
	EndedAt          *time.Time     `json:"ended_at,omitempty"`
	Summary          *string        `json:"summary,omitempty"`
	SummarisedAt     *time.Time     `json:"summarised_at,omitempty"`
	FactsExtractedAt *time.Time     `json:"facts_extracted_at,omitempty"`
	Meta             map[string]any `json:"meta,omitempty"`
}

// Observation is one raw tier-0 event: a prompt, a tool call, a file edit.
type Observation struct {
	ID         string           `json:"id"`
	SessionID  string           `json:"session_id"`
	TeamID     string           `json:"team_id"`
	UserID     string           `json:"user_id"`
	Project    string           `json:"project"`
	Kind       string           `json:"kind"`
	Content    string           `json:"content"`
	Files      []string         `json:"files,omitempty"`
	Redacted   bool             `json:"redacted"`
	Redactions []redact.Finding `json:"redactions,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
}

// Memory is a durable fact, decision, lesson, or preference.
type Memory struct {
	ID         string           `json:"id"`
	TeamID     string           `json:"team_id"`
	UserID     string           `json:"user_id"`
	Project    string           `json:"project"`
	Kind       string           `json:"kind"`
	Title      string           `json:"title"`
	Body       string           `json:"body"`
	Files      []string         `json:"files,omitempty"`
	Visibility string           `json:"visibility"`
	Strength   float64          `json:"strength"`
	Hits       int              `json:"hits"`
	Source     string           `json:"source"`
	SessionID  *string          `json:"session_id,omitempty"`
	Redacted   bool             `json:"redacted"`
	Redactions []redact.Finding `json:"redactions,omitempty"`

	SupersededBy *string    `json:"superseded_by,omitempty"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// SearchResult is a memory with its fused retrieval score and the ranks that
// produced it. The ranks are returned because "why did this come first" is the
// question asked of every search system, and answering it from a log line is
// cheaper than reproducing the query.
type SearchResult struct {
	Memory
	Score    float64 `json:"score"`
	BM25Rank int     `json:"bm25_rank,omitempty"`
	VecRank  int     `json:"vector_rank,omitempty"`
}

// GraphNode is an entity extracted from real co-occurrence in the corpus.
type GraphNode struct {
	ID      string  `json:"id"`
	TeamID  string  `json:"team_id"`
	Project string  `json:"project"`
	Kind    string  `json:"kind"`
	Label   string  `json:"label"`
	Weight  float64 `json:"weight"`
}

// GraphEdge connects two nodes.
type GraphEdge struct {
	ID     string  `json:"id"`
	Src    string  `json:"src"`
	Dst    string  `json:"dst"`
	Rel    string  `json:"rel"`
	Weight float64 `json:"weight"`
}

// Graph is a connected subgraph for one project.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// AuditEntry records one mutation.
type AuditEntry struct {
	ID        string         `json:"id"`
	TeamID    string         `json:"team_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	KeyID     string         `json:"key_id,omitempty"`
	Action    string         `json:"action"`
	Target    string         `json:"target,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	IP        string         `json:"ip,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// ConsolidationRun records one pass of the consolidation pipeline, including
// its token spend. Cost that is not measured is cost that surprises someone.
type ConsolidationRun struct {
	ID           string     `json:"id"`
	Tier         int        `json:"tier"`
	TeamID       string     `json:"team_id,omitempty"`
	Project      string     `json:"project,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Items        int        `json:"items"`
	Produced     int        `json:"produced"`
	Deduped      int        `json:"deduped"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	Error        *string    `json:"error,omitempty"`
}
