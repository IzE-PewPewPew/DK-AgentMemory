package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

func cmdSave(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(Err)
	kind := fs.String("kind", "fact", "fact, decision, lesson or preference")
	project := fs.String("project", "", "project identity; detected from the working directory when omitted")
	body := fs.String("body", "", "longer detail, especially the reason")
	team := fs.Bool("team", false, "share with the team immediately")
	files := fs.String("files", "", "comma-separated file paths this relates to")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	title := strings.TrimSpace(strings.Join(rest, " "))
	if title == "" {
		return fail(`usage: dkm save "the thing to remember" [--kind decision] [--team]`)
	}
	if !store.ValidKind(*kind) {
		return fail("--kind must be one of fact, decision, lesson, preference")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	visibility := store.VisibilityPrivate
	if *team {
		visibility = store.VisibilityTeam
	}

	res, err := c.Save(ctx, store.MemoryInput{
		Kind:       *kind,
		Title:      title,
		Body:       firstNonEmptyStr(*body, title),
		Project:    resolveProject(c, *project),
		Files:      splitList(*files),
		Visibility: visibility,
	})
	if err != nil {
		return failErr(err)
	}

	if res.Queued {
		fmt.Fprintf(Out, "Queued locally as %s — the server is unreachable.\n", res.Memory.ID)
		fmt.Fprintf(Out, "It will be sent by the next `dkm sync`, and cannot be duplicated by a retry.\n")
		return 0
	}
	fmt.Fprintf(Out, "Saved %s  [%s, %s]\n", res.Memory.ID, res.Memory.Kind, res.Memory.Visibility)
	if res.Memory.Redacted {
		fmt.Fprintf(Out, "Credentials were detected and removed before storage.\n")
	}
	return 0
}

func cmdSearch(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "restrict to one project")
	allProjects := fs.Bool("all", false, "search every project, not just this one")
	kind := fs.String("kind", "", "comma-separated kinds to include")
	limit := fs.Int("limit", 8, "maximum results")
	full := fs.Bool("full", false, "print full bodies instead of one line each")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	query := strings.TrimSpace(strings.Join(rest, " "))
	if query == "" {
		return fail(`usage: dkm search "what you want to know"`)
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	scope := resolveProject(c, *project)
	if *allProjects {
		scope = ""
	}

	res, err := c.Search(ctx, query, scope, splitList(*kind), *limit)
	if err != nil {
		return failErr(err)
	}

	if res.Local {
		fmt.Fprintf(Err, "The server is unreachable. These are keyword-only results from the local mirror and may be stale.\n\n")
	}
	if len(res.Results) == 0 {
		fmt.Fprintln(Out, "Nothing found.")
		if scope != "" {
			fmt.Fprintf(Out, "Searched project %s only — add --all to search everything.\n", scope)
		}
		return 0
	}

	for i, r := range res.Results {
		fmt.Fprintf(Out, "%d. [%s] %s\n", i+1, r.Kind, r.Title)
		if *full && r.Body != "" && r.Body != r.Title {
			for _, line := range strings.Split(strings.TrimSpace(r.Body), "\n") {
				fmt.Fprintf(Out, "     %s\n", line)
			}
		} else if r.Body != "" && r.Body != r.Title {
			fmt.Fprintf(Out, "     %s\n", truncate(r.Body, 100))
		}

		meta := []string{r.ID}
		if r.Project != "" {
			meta = append(meta, r.Project)
		}
		if r.Visibility == store.VisibilityTeam {
			meta = append(meta, "team")
		}
		meta = append(meta, fmt.Sprintf("score %.4f", r.Score))
		fmt.Fprintf(Out, "     %s\n\n", strings.Join(meta, "  ·  "))
	}

	fmt.Fprintf(Out, "%d result%s (%s search)\n", len(res.Results), plural(len(res.Results)), res.Mode)
	if res.Mode == "keyword" && !res.Local {
		fmt.Fprintln(Out, "The embedder was unavailable, so this was keyword-only. Paraphrases will not have matched.")
	}
	return 0
}

func cmdLesson(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("lesson", flag.ContinueOnError)
	fs.SetOutput(Err)
	why := fs.String("why", "", "the incident or reasoning behind the rule")
	project := fs.String("project", "", "project identity; omit for a rule that applies everywhere")
	global := fs.Bool("global", false, "store without a project, so it applies to all work")
	team := fs.Bool("team", false, "share with the team immediately")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	lesson := strings.TrimSpace(strings.Join(rest, " "))
	if lesson == "" {
		return fail(`usage: dkm lesson "always X because Y" [--why "what happened"]`)
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	scope := resolveProject(c, *project)
	if *global {
		scope = ""
	}
	visibility := store.VisibilityPrivate
	if *team {
		visibility = store.VisibilityTeam
	}

	res, err := c.SaveLesson(ctx, lesson, *why, scope, nil, visibility)
	if err != nil {
		return failErr(err)
	}
	if res.Queued {
		fmt.Fprintf(Out, "Queued locally as %s — the server is unreachable.\n", res.Memory.ID)
		return 0
	}
	fmt.Fprintf(Out, "Lesson saved %s\n", res.Memory.ID)
	if *why == "" {
		// A rule whose reason is lost is a rule nobody can safely retire.
		fmt.Fprintln(Out, "No reason recorded. Add one with --why so this can be revisited later.")
	}
	return 0
}

func cmdLessons(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("lessons", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "restrict to one project")
	allProjects := fs.Bool("all", false, "list lessons from every project")
	limit := fs.Int("limit", 50, "maximum lessons")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	scope := resolveProject(c, *project)
	if *allProjects {
		scope = ""
	}

	lessons, local, err := c.Lessons(ctx, scope, *limit)
	if err != nil {
		return failErr(err)
	}
	if local {
		fmt.Fprintln(Err, "The server is unreachable; showing the local mirror.")
	}
	if len(lessons) == 0 {
		fmt.Fprintln(Out, "No lessons yet.")
		fmt.Fprintln(Out, "They accumulate from use, or you can write one: dkm lesson \"always X because Y\"")
		return 0
	}

	for _, l := range lessons {
		marker := " "
		if l.Source == store.SourceConsolidation {
			// Worth distinguishing: a lesson nobody typed is the pipeline
			// working, and people are right to want to know which is which.
			marker = "*"
		}
		fmt.Fprintf(Out, "%s %s\n", marker, l.Title)
		if l.Body != "" && l.Body != l.Title {
			fmt.Fprintf(Out, "    %s\n", truncate(l.Body, 100))
		}
		fmt.Fprintf(Out, "    %s  ·  strength %.2f  ·  %d use%s\n\n", l.ID, l.Strength, l.Hits, plural(l.Hits))
	}
	fmt.Fprintf(Out, "%d lesson%s.  * = synthesised by the consolidation pipeline.\n", len(lessons), plural(len(lessons)))
	return 0
}

func cmdContext(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "project identity")
	budget := fs.Int("budget", 0, "token budget; the server default is used when 0")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	payload, err := c.Context(ctx, resolveProject(c, *project), *budget)
	if err != nil {
		return failErr(err)
	}
	if payload.Text == "" {
		fmt.Fprintln(Out, "No stored context for this project yet.")
		return 0
	}

	fmt.Fprintln(Out, payload.Text)
	fmt.Fprintf(Err, "\n(%d tokens", payload.Tokens)
	if payload.Truncated {
		fmt.Fprintf(Err, ", truncated to fit the budget")
	}
	fmt.Fprintln(Err, ")")
	return 0
}

func cmdShare(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return fail("usage: dkm share <memory-id>")
	}
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	mem, err := c.Share(ctx, args[0])
	if err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Shared with the team: %s\n", mem.Title)
	return 0
}

func cmdFeed(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("feed", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "restrict to one project")
	limit := fs.Int("limit", 20, "maximum items")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	mems, err := c.Feed(ctx, *project, *limit)
	if err != nil {
		return failErr(err)
	}
	if len(mems) == 0 {
		fmt.Fprintln(Out, "Nothing shared by teammates yet.")
		return 0
	}
	for _, m := range mems {
		fmt.Fprintf(Out, "[%s] %s\n", m.Kind, m.Title)
		if m.Body != "" && m.Body != m.Title {
			fmt.Fprintf(Out, "    %s\n", truncate(m.Body, 100))
		}
		fmt.Fprintf(Out, "    %s  ·  %s\n\n", m.ID, orDash(m.Project))
	}
	return 0
}

func cmdForget(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	fs.SetOutput(Err)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		return fail("usage: dkm forget <memory-id> [--yes]")
	}
	id := rest[0]

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	if !*yes {
		fmt.Fprintf(Out, "Delete memory %s? This cannot be undone from the CLI.\n", id)
		fmt.Fprintf(Out, "If it is merely out of date, `dkm supersede` keeps the history instead.\n")
		fmt.Fprintf(Out, "Type yes to confirm: ")
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			fmt.Fprintln(Out, "Cancelled.")
			return 0
		}
	}

	if err := c.Forget(ctx, id); err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Deleted %s\n", id)
	return 0
}

func cmdSupersede(ctx context.Context, args []string) int {
	if len(args) < 2 {
		return fail("usage: dkm supersede <old-id> <new-id>")
	}
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	if err := c.Supersede(ctx, args[0], args[1]); err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "%s is now superseded by %s.\n", args[0], args[1])
	fmt.Fprintln(Out, "Both remain readable by ID; only the newer one appears in search.")
	return 0
}

func cmdProjects(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	projects, err := c.Projects(ctx)
	if err != nil {
		return failErr(err)
	}
	if len(projects) == 0 {
		fmt.Fprintln(Out, "No projects yet.")
		return 0
	}

	// Every tier, so a project mid-pipeline reads as "imported, not yet
	// consolidated" rather than as an empty row.
	rows := [][]string{{"PROJECT", "SESSIONS", "OBSERVATIONS", "SUMMARISED", "MEMORIES", "LAST ACTIVITY"}}
	var pending int64
	for _, p := range projects {
		pending += p.Sessions - p.Summarised
		rows = append(rows, []string{
			orDash(p.Project),
			fmt.Sprint(p.Sessions),
			fmt.Sprint(p.Observations),
			fmt.Sprintf("%d/%d", p.Summarised, p.Sessions),
			fmt.Sprint(p.Memories),
			p.LastSeen.Local().Format("2006-01-02 15:04"),
		})
	}
	table(Out, rows)
	if pending > 0 {
		fmt.Fprintf(Out, "\n%d sessions awaiting consolidation. Run `dkm consolidate` to distil them into memories.\n", pending)
	}
	return 0
}

func cmdGraph(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "project identity")
	node := fs.String("node", "", "seed label, usually a file path")
	depth := fs.Int("depth", 2, "hops from the seed")
	rebuild := fs.Bool("rebuild", false, "re-derive nodes and edges before reading; idempotent")
	all := fs.Bool("all", false, "with --rebuild, rebuild every project instead of just this one")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	if *rebuild {
		targets := []string{resolveProject(c, *project)}
		if *all {
			projects, err := c.Projects(ctx)
			if err != nil {
				return failErr(err)
			}
			targets = targets[:0]
			for _, p := range projects {
				if p.Project != "" {
					targets = append(targets, p.Project)
				}
			}
		}
		for _, t := range targets {
			res, err := c.RebuildGraph(ctx, t)
			if err != nil {
				fmt.Fprintf(Err, "  %-46s failed: %v\n", truncate(t, 46), err)
				continue
			}
			fmt.Fprintf(Out, "  %-46s %5d nodes, %6d edges\n", truncate(t, 46), res.Nodes, res.Edges)
		}
		if *all {
			return 0
		}
		fmt.Fprintln(Out)
	}

	g, err := c.Graph(ctx, resolveProject(c, *project), *node, *depth)
	if err != nil {
		return failErr(err)
	}

	if len(g.Nodes) == 0 {
		fmt.Fprintln(Out, "No graph for this project yet.")
		fmt.Fprintln(Out, "Nodes come from files named by stored memories and observations, and edges")
		fmt.Fprintln(Out, "from files worked on together. If this project has been imported but never")
		fmt.Fprintln(Out, "drawn, run `dkm graph --rebuild`.")
		return 0
	}

	labels := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		labels[n.ID] = n.Label
	}

	fmt.Fprintf(Out, "%d node%s, %d edge%s\n\n", len(g.Nodes), plural(len(g.Nodes)), len(g.Edges), plural(len(g.Edges)))
	for _, e := range g.Edges {
		src, dst := labels[e.Src], labels[e.Dst]
		if src == "" || dst == "" {
			continue
		}
		fmt.Fprintf(Out, "  %s  --%s-->  %s   (weight %.0f)\n", truncate(src, 40), e.Rel, truncate(dst, 40), e.Weight)
	}
	return 0
}

// --- helpers ---------------------------------------------------------------

// resolveProject uses the explicit project when given, otherwise detects it
// from the working directory.
//
// Detection by default is what makes `dkm search "why jose"` inside a repo
// return that repo's memories rather than everything the user has ever stored.
func resolveProject(c *client.Client, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return c.Project(wd).ID
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
