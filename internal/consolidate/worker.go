// Package consolidate distils raw session activity into durable memory.
//
// Four tiers, each cheaper per unit of value than the one below it:
//
//	Tier 0  observations      raw, high volume, written by hooks
//	Tier 1  session summaries one LLM call per closed session
//	Tier 2  facts / decisions extracted per project, deduped
//	Tier 3  lessons           recurring patterns become rules
//
// Cost discipline is the requirement, not a nice-to-have. Every batch boundary
// here exists because the obvious alternative -- one LLM call per observation --
// is how a memory system becomes expensive without becoming better. A single
// active developer generates hundreds of observations an hour; summarising each
// one costs real money to produce text nobody reads.
package consolidate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/embed"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// Worker runs the scheduled pipeline in-process.
//
// In-process rather than a separate service. One binary to deploy, one process
// to supervise, one log to read. The work is small and idempotent; a queue and
// a worker fleet would be more machinery than the job needs, and every piece of
// that machinery is something that can be down at 2am.
type Worker struct {
	cfg      *config.Config
	store    *store.Store
	embedder embed.Embedder
	llm      Provider
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]bool
}

// NewWorker builds the consolidation worker.
func NewWorker(cfg *config.Config, st *store.Store, embedder embed.Embedder, log *slog.Logger) (*Worker, error) {
	w := &Worker{
		cfg:      cfg,
		store:    st,
		embedder: embedder,
		log:      log,
		running:  map[string]bool{},
	}

	if cfg.Consolidation.Enabled {
		llm, err := New(cfg)
		if err != nil {
			return nil, err
		}
		w.llm = llm
	}
	return w, nil
}

// Run starts every scheduled job and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup

	// The embedding backfill runs even with consolidation disabled: it needs no
	// LLM, and without it a memory written during an embedder outage would stay
	// keyword-only for ever.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.everyTick(ctx, 2*time.Minute, "embed-backfill", w.backfillEmbeddings)
	}()

	// Maintenance: decay, retention, graph. Daily, and offset from the top of
	// the hour so a fleet of self-hosted instances does not all wake at once.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.everyTick(ctx, 6*time.Hour, "maintenance", w.maintenance)
	}()

	if w.cfg.Consolidation.Enabled && w.llm != nil {
		w.log.Info("consolidation enabled",
			"provider", w.llm.Name(),
			"tier1_interval", w.cfg.Consolidation.SessionSummaryInterval.String(),
			"tier2_cron", w.cfg.Consolidation.FactExtractionCron,
			"tier3_cron", w.cfg.Consolidation.LessonSynthesisCron)

		wg.Add(1)
		go func() {
			defer wg.Done()
			w.everyTick(ctx, w.cfg.Consolidation.SessionSummaryInterval.Duration(), "tier1", w.tier1SessionSummaries)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			w.everyCron(ctx, w.cfg.Consolidation.FactSchedule(), "tier2", w.tier2ExtractFacts)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			w.everyCron(ctx, w.cfg.Consolidation.LessonSchedule(), "tier3", w.tier3SynthesiseLessons)
		}()
	} else {
		w.log.Info("consolidation disabled; memories are stored and searchable but never distilled into lessons")
	}

	wg.Wait()
}

// everyTick runs fn on an interval.
func (w *Worker) everyTick(ctx context.Context, interval time.Duration, name string, fn func(context.Context) error) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.guard(ctx, name, fn)
		}
	}
}

// everyCron runs fn on a cron schedule.
//
// Recomputing the next firing time after each run, rather than sleeping a fixed
// interval, is what makes "0 2 * * *" mean 2am rather than "every 24 hours from
// whenever the process started".
func (w *Worker) everyCron(ctx context.Context, schedule interface{ Next(time.Time) time.Time }, name string, fn func(context.Context) error) {
	if schedule == nil {
		return
	}
	for {
		next := schedule.Next(time.Now())
		if next.IsZero() {
			w.log.Warn("schedule will never fire; check the cron expression", "job", name)
			return
		}

		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			w.guard(ctx, name, fn)
		}
	}
}

// guard runs a job, preventing overlap and converting a panic into a log line.
//
// The overlap check matters on a slow database: a tier-2 pass that takes longer
// than its interval would otherwise start a second copy competing with the
// first for the same rows.
func (w *Worker) guard(ctx context.Context, name string, fn func(context.Context) error) {
	w.mu.Lock()
	if w.running[name] {
		w.mu.Unlock()
		w.log.Warn("skipping run; the previous one is still going", "job", name)
		return
	}
	w.running[name] = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running[name] = false
		w.mu.Unlock()
		if rec := recover(); rec != nil {
			w.log.Error("consolidation job panicked", "job", name, "panic", rec)
		}
	}()

	start := time.Now()
	if err := fn(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.log.Error("consolidation job failed", "job", name, "error", err, "duration", time.Since(start).String())
		return
	}
	w.log.Debug("consolidation job finished", "job", name, "duration", time.Since(start).String())
}

// --- tier 1: session summaries ---------------------------------------------

func (w *Worker) tier1SessionSummaries(ctx context.Context) error {
	sessions, err := w.store.SessionsAwaitingSummary(ctx, 25)
	if err != nil || len(sessions) == 0 {
		return err
	}

	runID, err := w.store.StartRun(ctx, 1, "", "")
	if err != nil {
		return err
	}

	var produced, inTok, outTok int
	var runErr error

	for _, sess := range sessions {
		if ctx.Err() != nil {
			break
		}

		obs, err := w.store.SessionObservations(ctx, sess.ID, 400)
		if err != nil {
			runErr = err
			break
		}
		if len(obs) == 0 {
			// Nothing happened. Mark it done rather than reconsidering this
			// session on every tick for the rest of time.
			_ = w.store.MarkSummarised(ctx, sess.ID, "")
			continue
		}

		resp, err := completeWithRetry(ctx, w.llm, Request{
			System:    tier1System,
			Prompt:    renderObservations(sess, obs),
			MaxTokens: 400,
		})
		if err != nil {
			runErr = err
			break
		}

		inTok += resp.InputTokens
		outTok += resp.OutputTokens

		summary := strings.TrimSpace(resp.Text)
		if err := w.store.MarkSummarised(ctx, sess.ID, summary); err != nil {
			runErr = err
			break
		}
		if summary != "" {
			produced++
		}
	}

	if inTok+outTok > 0 {
		w.log.Info("tier 1 session summaries",
			"sessions", len(sessions), "summaries", produced,
			"input_tokens", inTok, "output_tokens", outTok)
	}
	return w.store.FinishRun(ctx, runID, len(sessions), produced, 0, inTok, outTok, runErr)
}

const tier1System = `You summarise one coding session for a memory system that other AI agents will read later.

Write two to four sentences of plain prose. Cover what was worked on, what changed, and anything that went wrong and why.

Rules:
- State facts, not narrative. "Fixed the auth redirect by …" not "The developer then decided to …".
- Include specifics: file names, commands, error messages, versions.
- Omit anything routine: reading files, running tests that passed, formatting.
- If nothing of consequence happened, reply with exactly: (nothing notable)
- Never invent detail that is not in the transcript.`

func renderObservations(sess store.Session, obs []store.Observation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\nAgent: %s\nStarted: %s\n\n",
		orNone(sess.Project), orNone(sess.Agent), sess.StartedAt.Format(time.RFC3339))

	// A budget on the prompt, not just on the reply. A pathological session with
	// ten thousand observations must not turn one summary into a large bill.
	const budget = 24000
	used := 0

	for _, o := range obs {
		line := fmt.Sprintf("[%s] %s", o.Kind, collapse(o.Content, 500))
		if len(o.Files) > 0 {
			line += " (files: " + strings.Join(o.Files, ", ") + ")"
		}
		if used+len(line) > budget {
			fmt.Fprintf(&b, "\n… %d further observations omitted for length.\n", len(obs)-used)
			break
		}
		b.WriteString(line + "\n")
		used += len(line)
	}
	return b.String()
}

// --- tier 2: fact extraction -----------------------------------------------

type extractedFact struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Files []string `json:"files,omitempty"`
}

func (w *Worker) tier2ExtractFacts(ctx context.Context) error {
	sessions, err := w.store.SessionsAwaitingFacts(ctx, 100)
	if err != nil || len(sessions) == 0 {
		return err
	}

	// Group by team and project: facts are about a project, and a prompt that
	// mixes two codebases produces facts that belong to neither.
	groups := map[[2]string][]store.Session{}
	for _, s := range sessions {
		key := [2]string{s.TeamID, s.Project}
		groups[key] = append(groups[key], s)
	}

	for key, group := range groups {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.extractForProject(ctx, key[0], key[1], group); err != nil {
			w.log.Error("fact extraction failed", "team", key[0], "project", key[1], "error", err)
		}
	}
	return nil
}

func (w *Worker) extractForProject(ctx context.Context, teamID, project string, sessions []store.Session) error {
	runID, err := w.store.StartRun(ctx, 2, teamID, project)
	if err != nil {
		return err
	}

	var produced, deduped, inTok, outTok int
	var runErr error

	defer func() {
		_ = w.store.FinishRun(ctx, runID, len(sessions), produced, deduped, inTok, outTok, runErr)
	}()

	identity, err := w.store.AnyUserInTeam(ctx, teamID)
	if err != nil {
		runErr = err
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\nSession summaries:\n", orNone(project))
	for _, s := range sessions {
		if s.Summary != nil && *s.Summary != "" && *s.Summary != "(nothing notable)" {
			fmt.Fprintf(&b, "- [%s] %s\n", s.StartedAt.Format("2006-01-02"), *s.Summary)
		}
	}

	resp, err := completeWithRetry(ctx, w.llm, Request{
		System:    tier2System,
		Prompt:    b.String(),
		MaxTokens: w.cfg.Consolidation.LLM.MaxTokens,
		JSON:      true,
	})
	if err != nil {
		runErr = err
		return err
	}
	inTok, outTok = resp.InputTokens, resp.OutputTokens

	var facts []extractedFact
	if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &facts); err != nil {
		runErr = fmt.Errorf("model did not return a JSON array: %w", err)
		return runErr
	}

	for _, f := range facts {
		if strings.TrimSpace(f.Title) == "" {
			continue
		}
		if f.Kind != store.KindDecision && f.Kind != store.KindPreference {
			f.Kind = store.KindFact
		}

		text := f.Title + "\n" + f.Body
		vec := w.embedOne(ctx, text)

		// Dedup before write, always. Vector-search what exists and reinforce
		// on a near match rather than inserting. Without this every scheduled
		// run re-derives the same three facts from overlapping summaries, and
		// within a month search returns fifteen phrasings of one thing.
		if len(vec) > 0 {
			similar, err := w.store.SimilarByVector(ctx, *identity, project, vec, 3)
			if err == nil && len(similar) > 0 && similar[0].Score >= w.cfg.Search.DedupThreshold {
				if _, err := w.store.Reinforce(ctx, *identity, similar[0].ID); err == nil {
					deduped++
					continue
				}
			}
		}

		mem, created, err := w.store.CreateMemory(ctx, *identity, store.MemoryInput{
			Kind: f.Kind, Title: f.Title, Body: orFallback(f.Body, f.Title),
			Project: project, Files: f.Files,
			Visibility: store.VisibilityTeam, // consolidation output is for the team
			Source:     store.SourceConsolidation,
			Embedding:  vec,
		})
		switch {
		case err != nil:
			w.log.Warn("could not store extracted fact", "error", err)
		case created:
			produced++
			_ = mem
		default:
			deduped++
		}
	}

	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	if err := w.store.MarkFactsExtracted(ctx, ids); err != nil {
		runErr = err
		return err
	}

	w.log.Info("tier 2 fact extraction",
		"project", project, "sessions", len(sessions),
		"facts", produced, "deduped", deduped,
		"input_tokens", inTok, "output_tokens", outTok)
	return nil
}

const tier2System = `You extract durable facts from session summaries for a shared engineering memory.

Reply with a JSON array and nothing else. Each element:
  {"kind": "fact" | "decision" | "preference", "title": "one line", "body": "the detail and the reason", "files": ["optional/paths"]}

Extract only what will still be true and useful in three months:
- Decisions and, crucially, the reason for them
- Constraints not visible in the code ("staging has no pgvector")
- Workarounds and what made them necessary
- Conventions a newcomer would not infer

Do not extract:
- Anything the code itself states
- What someone did on a particular day
- Transient state, or problems that were fixed within the session

A decision without its reason is not worth storing; either include the reason or drop the item.
If nothing qualifies, reply with exactly: []`

// --- tier 3: lesson synthesis ----------------------------------------------

type synthesisedLesson struct {
	Lesson string   `json:"lesson"`
	Why    string   `json:"why"`
	Files  []string `json:"files,omitempty"`
}

func (w *Worker) tier3SynthesiseLessons(ctx context.Context) error {
	pairs, err := w.store.ProjectsForGraph(ctx)
	if err != nil {
		return err
	}

	for _, pair := range pairs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.synthesiseForProject(ctx, pair[0], pair[1]); err != nil {
			w.log.Error("lesson synthesis failed", "team", pair[0], "project", pair[1], "error", err)
		}
	}
	return nil
}

func (w *Worker) synthesiseForProject(ctx context.Context, teamID, project string) error {
	since := time.Now().AddDate(0, 0, -30)
	facts, err := w.store.RecentFacts(ctx, teamID, project, since, 200)
	if err != nil {
		return err
	}
	// A handful of facts cannot show a recurring pattern, and asking a model to
	// find one anyway produces confident invention.
	if len(facts) < 5 {
		return nil
	}

	runID, err := w.store.StartRun(ctx, 3, teamID, project)
	if err != nil {
		return err
	}
	var produced, deduped, inTok, outTok int
	var runErr error
	defer func() {
		_ = w.store.FinishRun(ctx, runID, len(facts), produced, deduped, inTok, outTok, runErr)
	}()

	identity, err := w.store.AnyUserInTeam(ctx, teamID)
	if err != nil {
		runErr = err
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\nFacts and decisions recorded over the last 30 days:\n", orNone(project))
	for _, f := range facts {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", f.Kind, f.Title, collapse(f.Body, 300))
	}

	resp, err := completeWithRetry(ctx, w.llm, Request{
		System:    tier3System,
		Prompt:    b.String(),
		MaxTokens: w.cfg.Consolidation.LLM.MaxTokens,
		JSON:      true,
	})
	if err != nil {
		runErr = err
		return err
	}
	inTok, outTok = resp.InputTokens, resp.OutputTokens

	var lessons []synthesisedLesson
	if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &lessons); err != nil {
		runErr = fmt.Errorf("model did not return a JSON array: %w", err)
		return runErr
	}

	for _, l := range lessons {
		if strings.TrimSpace(l.Lesson) == "" {
			continue
		}
		vec := w.embedOne(ctx, l.Lesson+"\n"+l.Why)

		if len(vec) > 0 {
			similar, err := w.store.SimilarByVector(ctx, *identity, project, vec, 3)
			if err == nil && len(similar) > 0 && similar[0].Score >= w.cfg.Search.DedupThreshold {
				if _, err := w.store.Reinforce(ctx, *identity, similar[0].ID); err == nil {
					deduped++
					continue
				}
			}
		}

		_, created, err := w.store.CreateMemory(ctx, *identity, store.MemoryInput{
			Kind: store.KindLesson, Title: l.Lesson, Body: orFallback(l.Why, l.Lesson),
			Project: project, Files: l.Files,
			Visibility: store.VisibilityTeam,
			Source:     store.SourceConsolidation,
			Embedding:  vec,
		})
		switch {
		case err != nil:
			w.log.Warn("could not store lesson", "error", err)
		case created:
			produced++
		default:
			deduped++
		}
	}

	if produced > 0 {
		w.log.Info("tier 3 lessons synthesised",
			"project", project, "lessons", produced, "deduped", deduped,
			"input_tokens", inTok, "output_tokens", outTok)
	}
	return nil
}

const tier3System = `You turn recorded facts into durable rules for an engineering team.

Reply with a JSON array and nothing else. Each element:
  {"lesson": "an imperative rule, one line", "why": "the incident or reasoning behind it", "files": ["optional/paths"]}

A lesson must:
- Be imperative and general: "always use full paths with pkill on multi-service hosts"
- Be supported by more than one of the facts below, or by one costly enough to be worth a standing rule
- Carry its reason, because a rule whose reason is forgotten is a rule nobody can safely retire

Do not produce:
- Restatements of a single fact
- Generic engineering advice that is not specific to this project
- Anything you cannot point at evidence for in the input

Most inputs justify zero or one lesson. If nothing qualifies, reply with exactly: []`

// --- maintenance -----------------------------------------------------------

func (w *Worker) maintenance(ctx context.Context) error {
	if w.cfg.Retention.DecayEnabled {
		n, err := w.store.Decay(ctx, w.cfg.Retention.DecayHalfLifeDays)
		if err != nil {
			return err
		}
		w.log.Debug("strength decay applied", "rows", n)
	}

	if w.cfg.Retention.ObservationDays > 0 {
		window := time.Duration(w.cfg.Retention.ObservationDays) * 24 * time.Hour
		n, err := w.store.PurgeObservations(ctx, window)
		if err != nil {
			return err
		}
		if n > 0 {
			w.log.Info("purged expired observations",
				"rows", n, "older_than_days", w.cfg.Retention.ObservationDays)
		}
	}

	pairs, err := w.store.ProjectsForGraph(ctx)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, _, err := w.store.RebuildGraph(ctx, pair[0], pair[1]); err != nil {
			w.log.Warn("graph rebuild failed", "team", pair[0], "project", pair[1], "error", err)
		}
	}
	return nil
}

// backfillEmbeddings vectorises memories written while the embedder was down.
func (w *Worker) backfillEmbeddings(ctx context.Context) error {
	if w.embedder == nil || w.cfg.Embedding.Provider == "none" {
		return nil
	}

	pending, err := w.store.PendingEmbeddings(ctx, 100)
	if err != nil || len(pending) == 0 {
		return err
	}

	texts := make([]string, len(pending))
	for i, m := range pending {
		texts[i] = m.Title + "\n" + m.Body
	}

	ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	vecs, err := w.embedder.Embed(ectx, texts)
	if err != nil {
		// Expected while the sidecar is restarting. Debug, not error: this runs
		// every two minutes and an error-level line each time would bury real
		// problems.
		w.log.Debug("embedding backfill deferred", "pending", len(pending), "error", err)
		return nil
	}

	done := 0
	for i, vec := range vecs {
		if i >= len(pending) || len(vec) == 0 {
			continue
		}
		if err := w.store.SetEmbedding(ctx, pending[i].ID, vec); err != nil {
			return err
		}
		done++
	}
	if done > 0 {
		w.log.Info("embedded pending memories", "count", done)
	}
	return nil
}

func (w *Worker) embedOne(ctx context.Context, text string) []float32 {
	if w.embedder == nil {
		return nil
	}
	ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	vecs, err := w.embedder.Embed(ectx, []string{text})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	return vecs[0]
}

// --- helpers ---------------------------------------------------------------

func collapse(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orFallback(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
