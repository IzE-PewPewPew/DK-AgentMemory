package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cmdConsolidate runs the distillation pipeline on demand.
func cmdConsolidate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("consolidate", flag.ContinueOnError)
	fs.SetOutput(Err)
	tierList := fs.String("tiers", "", "comma-separated tiers to run: 1 summaries, 2 facts, 3 lessons. Default all three, in order.")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	var tiers []int
	for _, t := range splitList(*tierList) {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > 3 {
			return fail("--tiers takes 1, 2 or 3")
		}
		tiers = append(tiers, n)
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	fmt.Fprintln(Out, "Consolidating. This calls your LLM provider and may take a minute.")

	var out struct {
		Ran      bool   `json:"ran"`
		Reason   string `json:"reason"`
		Fix      string `json:"fix"`
		Provider string `json:"provider"`
		Report   struct {
			Tiers              []string `json:"tiers"`
			Duration           string   `json:"duration"`
			Errors             []string `json:"errors"`
			SessionsSummarised int      `json:"sessions_summarised"`
			FactsProduced      int      `json:"facts_produced"`
			LessonsProduced    int      `json:"lessons_produced"`
			Deduped            int      `json:"deduped"`
			InputTokens        int      `json:"input_tokens"`
			OutputTokens       int      `json:"output_tokens"`
		} `json:"report"`
	}

	body := map[string]any{}
	if len(tiers) > 0 {
		body["tiers"] = tiers
	}
	if err := c.Admin(ctx, http.MethodPost, "/v1/admin/consolidate", body, &out); err != nil {
		return failErr(err)
	}

	if !out.Ran {
		fmt.Fprintf(Err, "\nDid not run: %s\n", out.Reason)
		if out.Fix != "" {
			fmt.Fprintf(Err, "\n%s\n", out.Fix)
		}
		return 1
	}

	r := out.Report
	fmt.Fprintf(Out, "\nRan %s via %s in %s.\n\n", strings.Join(r.Tiers, ", "), out.Provider, r.Duration)
	table(Out, [][]string{
		{"Sessions summarised", strconv.Itoa(r.SessionsSummarised)},
		{"Facts extracted", strconv.Itoa(r.FactsProduced)},
		{"Lessons synthesised", strconv.Itoa(r.LessonsProduced)},
		{"Deduplicated", strconv.Itoa(r.Deduped)},
		{"Tokens in / out", fmt.Sprintf("%d / %d", r.InputTokens, r.OutputTokens)},
	})

	for _, e := range r.Errors {
		fmt.Fprintf(Err, "\nerror: %s\n", e)
	}
	if len(r.Errors) > 0 {
		return 1
	}

	switch {
	case r.LessonsProduced > 0:
		fmt.Fprintf(Out, "\nSee them with `dkm lessons`.\n")
	case r.FactsProduced > 0:
		fmt.Fprintf(Out, "\nLessons need recurring patterns across at least five facts; run this again after more work.\n")
	case r.SessionsSummarised == 0 && r.FactsProduced == 0:
		// Zero is a legitimate outcome, and saying why prevents it reading as
		// a failure.
		fmt.Fprintf(Out, "\nNothing to distil. Tier 1 needs closed sessions, tier 2 needs summaries,\n"+
			"and tier 3 needs at least five facts in a project before it will infer a rule.\n")
	}
	return 0
}

// cmdActivity shows what each connected tool has actually contributed.
func cmdActivity(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	var out struct {
		Agents []struct {
			Agent        string     `json:"agent"`
			Sessions     int64      `json:"sessions"`
			Open         int64      `json:"open_sessions"`
			Observations int64      `json:"observations"`
			Memories     int64      `json:"memories"`
			Summarised   int64      `json:"summarised"`
			LastSeen     *time.Time `json:"last_seen"`
		} `json:"agents"`
		Consolidation struct {
			Enabled  bool   `json:"enabled"`
			Provider string `json:"provider"`
		} `json:"consolidation"`
	}
	if err := c.Admin(ctx, http.MethodGet, "/v1/activity", nil, &out); err != nil {
		return failErr(err)
	}

	if len(out.Agents) == 0 {
		fmt.Fprintln(Out, "No sessions recorded yet.")
		fmt.Fprintln(Out, "Only Claude Code captures automatically; other hosts record a session when the")
		fmt.Fprintln(Out, "agent decides to call a memory tool.")
		return 0
	}

	rows := [][]string{{"AGENT", "SESSIONS", "OPEN", "OBSERVATIONS", "MEMORIES", "SUMMARISED", "LAST SEEN"}}
	for _, a := range out.Agents {
		last := "—"
		if a.LastSeen != nil {
			last = a.LastSeen.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, []string{
			a.Agent,
			fmt.Sprint(a.Sessions), fmt.Sprint(a.Open),
			fmt.Sprint(a.Observations), fmt.Sprint(a.Memories),
			fmt.Sprint(a.Summarised), last,
		})
	}
	table(Out, rows)

	fmt.Fprintf(Out, "\nConsolidation: ")
	if out.Consolidation.Enabled {
		fmt.Fprintf(Out, "enabled via %s. Run it now with `dkm consolidate`.\n", out.Consolidation.Provider)
	} else {
		fmt.Fprintf(Out, "disabled. Memories are stored and searchable but never distilled into lessons.\n")
	}
	return 0
}
