package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// SearchQuery describes one retrieval.
type SearchQuery struct {
	Query   string
	Project string
	Kinds   []string
	Limit   int

	// Vector is the query embedding. When empty the search degrades to BM25
	// only, which is the correct behaviour when the embedder is down: keyword
	// recall still works and nothing 500s.
	Vector []float32

	IncludeSuperseded bool
	MineOnly          bool
}

// argList allocates positional placeholders in order.
type argList struct{ vals []any }

func (a *argList) add(v any) string {
	a.vals = append(a.vals, v)
	return "$" + strconv.Itoa(len(a.vals))
}

// Search runs hybrid retrieval: BM25 and vector similarity, fused with
// Reciprocal Rank Fusion, then weighted by strength and recency.
//
// Why fuse ranks rather than scores. BM25 returns a relevance figure on an
// unbounded scale that depends on corpus statistics; pgvector returns a cosine
// distance in [0,2]. There is no principled way to add those, and normalising
// them ("divide by the max in this result set") makes a result's score depend
// on what else happened to be returned, so adding one irrelevant document
// reorders everything above it. RRF ignores the magnitudes entirely and sums
// 1/(k+rank), which needs no normalisation and is what makes hybrid search work
// rather than merely combine.
//
// Why both retrievers are needed. BM25 alone cannot match "serverless token
// signing" against a memory that says "we chose jose over jsonwebtoken for Edge
// runtime compatibility" -- they share no terms. Vectors alone are unreliable
// on exact identifiers like `pprp-wallet-api`, where the embedding of a rare
// token carries little signal and a lexical match is exactly right.
func (s *Store) Search(ctx context.Context, id Identity, q SearchQuery) ([]SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = s.cfg.Search.DefaultLimit
	}
	if limit > 200 {
		limit = 200
	}
	candidates := s.cfg.Search.CandidateLimit
	if candidates < limit {
		candidates = limit
	}

	a := &argList{}
	pTeam := a.add(id.TeamID)
	pUser := a.add(id.UserID)

	filter := strings.Builder{}
	fmt.Fprintf(&filter, "m.team_id = %s AND (m.user_id = %s OR m.visibility = 'team')", pTeam, pUser)
	filter.WriteString(" AND m.deleted_at IS NULL")
	if !q.IncludeSuperseded {
		filter.WriteString(" AND m.superseded_by IS NULL")
	}
	if q.MineOnly {
		fmt.Fprintf(&filter, " AND m.user_id = %s", pUser)
	}
	if q.Project != "" {
		p := a.add(q.Project)
		// Memories with an empty project are global to the team -- a lesson
		// about the deployment host applies whichever repo you are in.
		fmt.Fprintf(&filter, " AND (m.project = %s OR m.project = '')", p)
	}
	if len(q.Kinds) > 0 {
		p := a.add(q.Kinds)
		fmt.Fprintf(&filter, " AND m.kind = ANY(%s)", p)
	}
	where := filter.String()

	pQuery := a.add(q.Query)
	pCand := a.add(candidates)

	var sb strings.Builder
	sb.WriteString(`
WITH bm25 AS (
    SELECT id, row_number() OVER (ORDER BY r DESC, id) AS rank
    FROM (
        SELECT m.id, ts_rank_cd(m.tsv, websearch_to_tsquery('english', ` + pQuery + `)) AS r
        FROM memories m
        WHERE ` + where + `
          AND m.tsv @@ websearch_to_tsquery('english', ` + pQuery + `)
        ORDER BY r DESC, m.id
        LIMIT ` + pCand + `
    ) x
)`)

	useVector := len(q.Vector) > 0
	if useVector {
		pVec := a.add(vectorLiteral(q.Vector))
		sb.WriteString(`,
vec AS (
    SELECT id, row_number() OVER (ORDER BY d ASC, id) AS rank
    FROM (
        SELECT m.id, m.embedding <=> ` + pVec + `::vector AS d
        FROM memories m
        WHERE ` + where + `
          AND m.embedding IS NOT NULL
        ORDER BY d ASC, m.id
        LIMIT ` + pCand + `
    ) x
)`)
	} else {
		// An empty CTE keeps one query shape rather than two. The planner
		// discards it and the FULL OUTER JOIN below degenerates to bm25 alone.
		sb.WriteString(`,
vec AS (SELECT NULL::text AS id, NULL::bigint AS rank WHERE false)`)
	}

	pK := a.add(float64(s.cfg.Search.RRFK))
	pHalf := a.add(s.cfg.Search.RecencyHalfLifeDays)
	pLimit := a.add(limit)

	// Strength and recency are bounded to a 15% band each, deliberately.
	//
	// RRF compresses ranks into a narrow range: with k=60, rank 1 scores
	// 1/61 and rank 3 scores 1/63 — a 3% spread. Multiplying that by raw
	// strength, which runs from 0.1 to 5.0, lets a reinforced memory outrank a
	// far more relevant one by fifty positions. That is precisely the failure
	// RRF exists to prevent, reintroduced one line after using it: a
	// magnitude on an unrelated scale swamping a rank signal.
	//
	// Measured here on a real corpus. A lesson at strength 1.6 sitting at
	// vector rank 3 (cosine 0.48) beat the correct answer at rank 1 (cosine
	// 0.82), and did so for every query, because 1.6x beats 3%.
	//
	// So both act as tiebreakers now. Each can move a result by roughly the
	// distance of a few rank positions and no further: enough that a proven
	// memory wins between near-equals, never enough to override relevance.
	sb.WriteString(`,
fused AS (
    SELECT COALESCE(b.id, v.id) AS id,
           COALESCE(1.0 / (` + pK + ` + b.rank), 0) + COALESCE(1.0 / (` + pK + ` + v.rank), 0) AS rrf,
           b.rank AS bm25_rank,
           v.rank AS vec_rank
    FROM bm25 b
    FULL OUTER JOIN vec v ON v.id = b.id
)
SELECT ` + memoryCols + `,
       (f.rrf
         * (0.85 + 0.15 * LEAST(GREATEST(m.strength, 0) / 5.0, 1.0))
         * (0.85 + 0.15 * power(0.5, EXTRACT(EPOCH FROM (now() - m.updated_at)) / 86400.0 / ` + pHalf + `))
       ) AS score,
       COALESCE(f.bm25_rank, 0) AS bm25_rank,
       COALESCE(f.vec_rank, 0)  AS vec_rank
FROM fused f
JOIN memories m ON m.id = f.id
ORDER BY score DESC, m.id DESC
LIMIT ` + pLimit)

	rows, err := s.pool.Query(ctx, sb.String(), a.vals...)
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r          SearchResult
			redactions []byte
			bmRank     int64
			vecRank    int64
		)
		if err := rows.Scan(&r.ID, &r.TeamID, &r.UserID, &r.Project, &r.Kind, &r.Title, &r.Body,
			&r.Files, &r.Visibility, &r.Strength, &r.Hits, &r.Source, &r.SessionID,
			&r.Redacted, &redactions, &r.SupersededBy, &r.DeletedAt,
			&r.CreatedAt, &r.UpdatedAt, &r.LastUsedAt,
			&r.Score, &bmRank, &vecRank); err != nil {
			return nil, err
		}
		if len(redactions) > 0 {
			_ = json.Unmarshal(redactions, &r.Redactions)
		}
		r.BM25Rank, r.VecRank = int(bmRank), int(vecRank)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SimilarByVector returns the nearest memories to a vector, with their cosine
// similarity.
//
// This is the dedup probe. Before writing an extracted fact the pipeline asks
// "is this already here", and a hit above the configured threshold reinforces
// the existing memory instead of inserting a near-duplicate. Skipping this step
// is the single reason memory stores degrade into noise: every session
// re-derives the same three facts, and within a month search returns fifteen
// phrasings of one thing.
func (s *Store) SimilarByVector(ctx context.Context, id Identity, project string, vec []float32, limit int) ([]SearchResult, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryCols+`,
		       1 - (m.embedding <=> $3::vector) AS similarity,
		       0::bigint, 0::bigint
		FROM memories m
		WHERE m.team_id = $1 AND m.user_id = $2
		  AND m.embedding IS NOT NULL
		  AND m.deleted_at IS NULL AND m.superseded_by IS NULL
		  AND ($4 = '' OR m.project = $4)
		ORDER BY m.embedding <=> $3::vector
		LIMIT $5
	`, id.TeamID, id.UserID, vectorLiteral(vec), project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r              SearchResult
			redactions     []byte
			ignoreA, ignoB int64
		)
		if err := rows.Scan(&r.ID, &r.TeamID, &r.UserID, &r.Project, &r.Kind, &r.Title, &r.Body,
			&r.Files, &r.Visibility, &r.Strength, &r.Hits, &r.Source, &r.SessionID,
			&r.Redacted, &redactions, &r.SupersededBy, &r.DeletedAt,
			&r.CreatedAt, &r.UpdatedAt, &r.LastUsedAt,
			&r.Score, &ignoreA, &ignoB); err != nil {
			return nil, err
		}
		if len(redactions) > 0 {
			_ = json.Unmarshal(redactions, &r.Redactions)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContextPayload is the token-budgeted injection returned at session start.
type ContextPayload struct {
	Project        string   `json:"project"`
	Lessons        []Memory `json:"lessons,omitempty"`
	Decisions      []Memory `json:"decisions,omitempty"`
	Facts          []Memory `json:"facts,omitempty"`
	Preferences    []Memory `json:"preferences,omitempty"`
	SessionSummary []string `json:"session_summaries,omitempty"`
	Text           string   `json:"text"`
	Tokens         int      `json:"tokens"`
	Truncated      bool     `json:"truncated"`
}

// BuildContext assembles the project context injected at session start.
//
// Ordering is deliberate: lessons first, then decisions, then recent session
// summaries. A budget that runs out should run out on "what happened last
// Tuesday", never on "always use full paths with pkill on this host".
func (s *Store) BuildContext(ctx context.Context, id Identity, project string, budgetTokens int, include []string) (*ContextPayload, error) {
	if budgetTokens <= 0 {
		budgetTokens = s.cfg.Injection.BudgetTokens
	}
	if len(include) == 0 {
		include = s.cfg.Injection.Include
	}
	want := map[string]bool{}
	for _, i := range include {
		want[i] = true
	}

	out := &ContextPayload{Project: project}
	var b strings.Builder
	budget := budgetTokens
	spend := func(s string) bool {
		t := estimateTokens(s)
		if t > budget {
			out.Truncated = true
			return false
		}
		budget -= t
		return true
	}

	if want["lessons"] {
		lessons, err := s.Lessons(ctx, id, project, 25)
		if err != nil {
			return nil, err
		}
		var section []string
		for _, l := range lessons {
			line := "- " + l.Title
			if l.Body != "" && l.Body != l.Title {
				line += ": " + l.Body
			}
			if !spend(line) {
				break
			}
			section = append(section, line)
			out.Lessons = append(out.Lessons, l)
		}
		if len(section) > 0 {
			b.WriteString("## Lessons\n" + strings.Join(section, "\n") + "\n\n")
		}
	}

	for _, kind := range []string{KindDecision, KindPreference, KindFact} {
		key := map[string]string{
			KindDecision:   "decisions",
			KindPreference: "preferences",
			KindFact:       "facts",
		}[kind]
		if !want[key] {
			continue
		}
		mems, _, err := s.ListMemories(ctx, id, ListFilter{Project: project, Kinds: []string{kind}, Limit: 25})
		if err != nil {
			return nil, err
		}
		var section []string
		for _, m := range mems {
			line := "- " + m.Title
			if m.Body != "" && m.Body != m.Title {
				line += ": " + m.Body
			}
			if !spend(line) {
				break
			}
			section = append(section, line)
			switch kind {
			case KindDecision:
				out.Decisions = append(out.Decisions, m)
			case KindPreference:
				out.Preferences = append(out.Preferences, m)
			default:
				out.Facts = append(out.Facts, m)
			}
		}
		if len(section) > 0 {
			b.WriteString("## " + strings.ToUpper(key[:1]) + key[1:] + "\n" + strings.Join(section, "\n") + "\n\n")
		}
	}

	if want["session_summaries"] {
		sessions, err := s.ListSessions(ctx, id, project, 5)
		if err != nil {
			return nil, err
		}
		var section []string
		for _, sess := range sessions {
			if sess.Summary == nil || *sess.Summary == "" {
				continue
			}
			line := "- " + sess.StartedAt.Format("2006-01-02") + ": " + *sess.Summary
			if !spend(line) {
				break
			}
			section = append(section, line)
			out.SessionSummary = append(out.SessionSummary, *sess.Summary)
		}
		if len(section) > 0 {
			b.WriteString("## Recent sessions\n" + strings.Join(section, "\n") + "\n")
		}
	}

	out.Text = strings.TrimSpace(b.String())
	out.Tokens = estimateTokens(out.Text)
	return out, nil
}

// estimateTokens approximates a token count from character length.
//
// Four characters per token is the usual English rule of thumb. A real
// tokeniser would be more accurate, but it would also mean shipping one
// vocabulary per model family and re-shipping it whenever a model changes.
// This budget only needs to be roughly right and always conservative.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/4 + 1
}
