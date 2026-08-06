package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/importers"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

func cmdStatus(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	wd, _ := os.Getwd()
	project := c.Project(wd)

	fmt.Fprintf(Out, "Server    %s\n", c.Config().Server)
	fmt.Fprintf(Out, "Project   %s  (%s)\n", orDash(project.ID), project.Source)

	health, herr := c.Health(ctx)
	if herr != nil {
		fmt.Fprintf(Out, "State     offline — %s\n", firstLine(herr.Error()))
	} else {
		fmt.Fprintf(Out, "State     online, v%s\n", health.Version)
	}

	m := c.Mirror()
	if m == nil {
		fmt.Fprintln(Out, "Mirror    disabled (sync.enabled is false)")
		return 0
	}

	cached, _ := m.Memories()
	queue, _ := m.Queue()
	cursor := m.Cursor()

	fmt.Fprintf(Out, "Mirror    %s\n", m.Dir())
	fmt.Fprintf(Out, "Cached    %d memories\n", len(cached))
	if cursor == "" {
		fmt.Fprintf(Out, "Synced    never — run `dkm pull`\n")
	} else if _, id, ok := strings.Cut(cursor, "|"); ok {
		fmt.Fprintf(Out, "Cursor    %s\n", id)
	}

	if len(queue) == 0 {
		fmt.Fprintln(Out, "Queued    0 writes")
		return 0
	}

	fmt.Fprintf(Out, "Queued    %d write%s waiting for `dkm push`\n", len(queue), plural(len(queue)))
	for i, w := range queue {
		if i >= 5 {
			fmt.Fprintf(Out, "          … and %d more\n", len(queue)-5)
			break
		}
		fmt.Fprintf(Out, "          %s  %s\n", w.QueuedAt.Local().Format("01-02 15:04"), truncate(w.Title, 60))
	}
	return 0
}

func cmdSync(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	res, err := c.Sync(ctx)
	if res != nil && res.Skipped {
		fmt.Fprintln(Out, "Sync is disabled in the client config (sync.enabled is false).")
		return 0
	}
	if err != nil {
		if client.IsOffline(err) {
			fmt.Fprintf(Out, "Still offline. %d write%s remain queued.\n", res.Queued, plural(res.Queued))
			return 1
		}
		return failErr(err)
	}

	fmt.Fprintf(Out, "Pushed %d, pulled %d.\n", res.Pushed, res.Pulled)
	if res.Failed > 0 {
		fmt.Fprintf(Out, "%d write%s were rejected by the server and remain queued. Run `dkm status` to see them.\n",
			res.Failed, plural(res.Failed))
		return 1
	}
	return 0
}

func cmdPush(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	pushed, failed, err := c.Push(ctx)
	if err != nil && !client.IsOffline(err) {
		return failErr(err)
	}

	switch {
	case err != nil:
		fmt.Fprintf(Out, "Server unreachable. Pushed %d before stopping; the rest stay queued.\n", pushed)
		return 1
	case pushed == 0 && failed == 0:
		fmt.Fprintln(Out, "Nothing queued.")
	default:
		fmt.Fprintf(Out, "Pushed %d write%s.\n", pushed, plural(pushed))
	}
	if failed > 0 {
		fmt.Fprintf(Out, "%d were rejected and remain queued.\n", failed)
		return 1
	}
	return 0
}

func cmdPull(ctx context.Context, args []string) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	pulled, _, err := c.Pull(ctx)
	if err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Pulled %d change%s into the local mirror.\n", pulled, plural(pulled))
	return 0
}

// --- export ----------------------------------------------------------------

func cmdExport(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(Err)
	scope := fs.String("scope", "me", "me or team")
	project := fs.String("project", "", "restrict to one project")
	outPath := fs.String("o", "", "write to a file instead of stdout")
	format := fs.String("format", "ndjson", "ndjson for machines, md for people")
	outDir := fs.String("out", "", "directory for --format md; one file per project")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch *format {
	case "ndjson", "md", "markdown":
	default:
		return fail("--format must be ndjson or md")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	if *format == "ndjson" {
		w := Out
		if *outPath != "" {
			f, err := os.Create(*outPath)
			if err != nil {
				return fail("%v", err)
			}
			defer f.Close()
			w = f
		}

		// Streamed straight through. A real corpus does not fit comfortably in
		// memory on either end, and buffering it would also mean nothing is
		// written until everything is read.
		n, err := c.Export(ctx, *scope, *project, w)
		if err != nil {
			return failErr(err)
		}
		if *outPath != "" {
			fmt.Fprintf(Err, "Wrote %s (%d bytes).\n", *outPath, n)
		}
		return 0
	}

	// Markdown is rendered client-side from the same NDJSON stream. The server
	// keeps one export format; presentation belongs here, where it can change
	// without a redeploy.
	//
	// This one does buffer, because grouping by project and ordering by kind
	// cannot be done in a single forward pass. Markdown export is for reading,
	// and a corpus too large to hold in memory is also too large to read.
	var buf bytes.Buffer
	if _, err := c.Export(ctx, *scope, *project, &buf); err != nil {
		return failErr(err)
	}

	byProject := map[string][]mdRecord{}
	total, malformed := 0, 0

	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 0, 256<<10), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec mdRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			malformed++
			continue
		}
		if rec.Type != "" && rec.Type != "memory" {
			continue
		}
		byProject[rec.Project] = append(byProject[rec.Project], rec)
		total++
	}
	if err := sc.Err(); err != nil {
		return fail("reading the export stream: %v", err)
	}

	if total == 0 {
		fmt.Fprintln(Err, "Nothing to export.")
		return 0
	}

	now := time.Now()

	// A single file when the destination is one, a directory otherwise.
	if *outDir == "" {
		w := Out
		if *outPath != "" {
			f, err := os.Create(*outPath)
			if err != nil {
				return fail("%v", err)
			}
			defer f.Close()
			w = f
		}
		projects := make([]string, 0, len(byProject))
		for p := range byProject {
			projects = append(projects, p)
		}
		sort.Strings(projects)
		for i, p := range projects {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprint(w, renderMarkdown(p, byProject[p], now))
		}
		if *outPath != "" {
			fmt.Fprintf(Err, "Wrote %s — %d memor%s across %d project%s.\n",
				*outPath, total, map[bool]string{true: "y", false: "ies"}[total == 1],
				len(projects), plural(len(projects)))
		}
		return 0
	}

	written, err := writeMarkdownExport(*outDir, byProject, now)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(Out, "Exported %d memor%s across %d project%s to %s\n\n",
		total, map[bool]string{true: "y", false: "ies"}[total == 1],
		len(byProject), plural(len(byProject)), *outDir)
	for _, p := range written {
		fmt.Fprintf(Out, "  %s\n", p)
	}
	if malformed > 0 {
		fmt.Fprintf(Err, "\n%d malformed line%s skipped.\n", malformed, plural(malformed))
	}
	fmt.Fprintf(Out, "\nRe-import any of these with:\n  dkm import markdown %s --apply\n", *outDir)
	return 0
}

// --- import ----------------------------------------------------------------

func cmdImport(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(Err, "usage: dkm import <source> [flags]")
		fmt.Fprintln(Err, "\nSources:")
		fmt.Fprintln(Err, "  claude-code   ~/.claude/projects transcripts")
		fmt.Fprintln(Err, "  codex         ~/.codex/sessions rollouts")
		fmt.Fprintln(Err, "  markdown      CLAUDE.md, ADRs, runbooks — one memory per section")
		fmt.Fprintln(Err, "  ndjson        a file produced by `dkm export`")
		fmt.Fprintln(Err, "\nEvery source defaults to a dry run. Pass --apply to write.")
		return 2
	}

	source := args[0]
	rest := args[1:]

	switch source {
	case "claude-code":
		return importTranscripts(ctx, rest, importers.DefaultClaudeCodeRoot(), "claude-code")
	case "codex", "codex-cli":
		return importTranscripts(ctx, rest, importers.DefaultCodexRoot(), "codex-cli")
	case "markdown", "md":
		return importMarkdown(ctx, rest)
	case "ndjson":
		return importNDJSON(ctx, rest)
	case "cursor":
		// Honest rather than half-working: Cursor keeps chat history in a
		// SQLite database whose schema is undocumented and changes between
		// releases. An importer built on that would break silently.
		fmt.Fprintln(Err, "Cursor stores chat history in an internal database with no stable format.")
		fmt.Fprintln(Err, "Export the chats you want to Markdown from Cursor, then:")
		fmt.Fprintln(Err, "\n  dkm import markdown ./exported-chats --apply")
		return 2
	default:
		return fail("unknown import source %q — try claude-code, codex, markdown or ndjson", source)
	}
}

func importTranscripts(ctx context.Context, args []string, defaultRoot, agent string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(Err)
	root := fs.String("path", defaultRoot, "directory or file to import")
	apply := fs.Bool("apply", false, "actually import; without this it is a dry run")
	limit := fs.Int("limit", 0, "import at most this many transcripts")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	preview, err := importers.ScanJSONL(*root, agent)
	if err != nil {
		return fail("%v", err)
	}

	printTranscriptPreview(preview, *root)

	if !*apply {
		fmt.Fprintln(Out, "\nThis was a dry run. Nothing was sent.")
		fmt.Fprintf(Out, "Re-run with --apply to import.\n")
		return 0
	}
	if preview.Files == 0 {
		return 0
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	transcripts := preview.Transcripts
	if *limit > 0 && *limit < len(transcripts) {
		transcripts = transcripts[:*limit]
	}

	imported, failedCount := 0, 0
	for i, t := range transcripts {
		if ctx.Err() != nil {
			fmt.Fprintln(Out, "\nInterrupted. Already-imported transcripts are not duplicated on a re-run.")
			break
		}

		sess, err := c.CreateSession(ctx, t.Project, t.Agent, map[string]any{
			"imported_from": t.Path,
			"original_id":   t.SessionID,
		})
		if err != nil {
			failedCount++
			fmt.Fprintf(Err, "  ✗ %s: %v\n", shorten(t.Path), err)
			continue
		}

		// Batched. One HTTP call per observation would turn a 40-transcript
		// import into tens of thousands of round trips.
		const batch = 200
		sent := 0
		for start := 0; start < len(t.Observations); start += batch {
			end := start + batch
			if end > len(t.Observations) {
				end = len(t.Observations)
			}
			n, err := c.AddObservations(ctx, sess.ID, t.Observations[start:end])
			if err != nil {
				fmt.Fprintf(Err, "  ✗ %s: %v\n", shorten(t.Path), err)
				break
			}
			sent += n
		}

		if err := c.EndSession(ctx, sess.ID, ""); err != nil {
			fmt.Fprintf(Err, "  ! %s: could not close the session: %v\n", shorten(t.Path), err)
		}

		imported++
		fmt.Fprintf(Out, "  [%d/%d] %-50s %d observations → %s\n",
			i+1, len(transcripts), truncate(orDash(t.Project), 50), sent, sess.ID)
	}

	fmt.Fprintf(Out, "\nImported %d transcript%s.\n", imported, plural(imported))
	if failedCount > 0 {
		fmt.Fprintf(Out, "%d failed.\n", failedCount)
	}
	fmt.Fprintln(Out, "The consolidation pipeline will summarise these on its next run;")
	fmt.Fprintln(Out, "lessons appear after fact extraction and synthesis have both run.")
	return 0
}

func printTranscriptPreview(p *importers.Preview, root string) {
	fmt.Fprintf(Out, "Scanning %s\n\n", shorten(root))
	fmt.Fprintf(Out, "  transcripts   %d\n", p.Files)
	fmt.Fprintf(Out, "  observations  %d\n", p.Observations)
	if p.Skipped > 0 {
		fmt.Fprintf(Out, "  skipped       %d (empty or unreadable)\n", p.Skipped)
	}

	if len(p.ByProject) > 0 {
		fmt.Fprintf(Out, "\nGrouped by project:\n")
		keys := sortedKeys(p.ByProject)
		sort.Slice(keys, func(i, j int) bool { return p.ByProject[keys[i]] > p.ByProject[keys[j]] })
		for _, k := range keys {
			fmt.Fprintf(Out, "  %-52s %d observations\n", orDash(k), p.ByProject[k])
		}
	}

	if len(p.Secrets) > 0 {
		// The reason dry run is the default. Years of history can contain
		// credentials that were read during a session, and discovering that
		// after the import is not something anyone can undo.
		fmt.Fprintf(Out, "\nCredentials detected — %d occurrence%s:\n", len(p.Secrets), plural(len(p.Secrets)))
		for _, kind := range sortedKindKeys(p.SecretCounts) {
			fmt.Fprintf(Out, "  %-24s %d\n", kind, p.SecretCounts[redact.Kind(kind)])
		}
		fmt.Fprintf(Out, "\nFirst occurrences:\n")
		for i, s := range p.Secrets {
			if i >= 10 {
				fmt.Fprintf(Out, "  … and %d more\n", len(p.Secrets)-10)
				break
			}
			fmt.Fprintf(Out, "  %s:%d  %s\n", shorten(s.File), s.Line, s.Kind)
		}
		fmt.Fprintln(Out, "\nThese are replaced with [redacted:kind] markers before anything is stored.")
		fmt.Fprintln(Out, "The values never reach the database. Review the list before continuing.")
	}

	for _, w := range p.Warnings {
		fmt.Fprintf(Out, "\nwarning: %s\n", w)
	}
}

func importMarkdown(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("import markdown", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "project identity; detected from the working directory when omitted")
	apply := fs.Bool("apply", false, "actually import; without this it is a dry run")
	team := fs.Bool("team", false, "store as team-visible")
	paths, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	visibility := store.VisibilityPrivate
	if *team {
		visibility = store.VisibilityTeam
	}

	preview, err := importers.ScanMarkdown(paths, resolveProject(c, *project), visibility)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(Out, "  files     %d\n  memories  %d\n", preview.Files, len(preview.Memories))
	if len(preview.Secrets) > 0 {
		fmt.Fprintf(Out, "\nCredentials detected — %d occurrence%s:\n", len(preview.Secrets), plural(len(preview.Secrets)))
		for i, s := range preview.Secrets {
			if i >= 10 {
				fmt.Fprintf(Out, "  … and %d more\n", len(preview.Secrets)-10)
				break
			}
			fmt.Fprintf(Out, "  %s:%d  %s\n", shorten(s.File), s.Line, s.Kind)
		}
		fmt.Fprintln(Out, "\nThese are redacted before storage.")
	}
	for _, w := range preview.Warnings {
		fmt.Fprintf(Out, "warning: %s\n", w)
	}

	if !*apply {
		fmt.Fprintln(Out, "\nDry run. Re-run with --apply to import.")
		if len(preview.Memories) > 0 {
			fmt.Fprintln(Out, "\nFirst few:")
			for i, m := range preview.Memories {
				if i >= 5 {
					break
				}
				fmt.Fprintf(Out, "  [%s] %s\n", m.Kind, truncate(m.Title, 70))
			}
		}
		return 0
	}

	created, duplicate := 0, 0
	for _, m := range preview.Memories {
		res, err := c.Save(ctx, m)
		if err != nil {
			fmt.Fprintf(Err, "  ✗ %s: %v\n", truncate(m.Title, 50), err)
			continue
		}
		if res.Created {
			created++
		} else {
			duplicate++
		}
	}
	fmt.Fprintf(Out, "\nImported %d, skipped %d already present.\n", created, duplicate)
	return 0
}

func importNDJSON(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("import ndjson", flag.ContinueOnError)
	fs.SetOutput(Err)
	apply := fs.Bool("apply", false, "actually import; without this it is a dry run")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	var in io.Reader = os.Stdin
	if len(rest) > 0 {
		f, err := os.Open(rest[0])
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		in = f
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	if !*apply {
		report, err := c.ImportPreview(ctx, in)
		if err != nil {
			return failErr(err)
		}
		printJSONReport(report)
		fmt.Fprintln(Out, "\nDry run. Re-run with --apply to import.")
		return 0
	}

	res, err := c.Import(ctx, bufio.NewReaderSize(in, 256<<10))
	if err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "lines %d · imported %d · duplicate %d · skipped %d\n",
		res.Lines, res.Imported, res.Duplicate, res.Skipped)
	for _, f := range res.Failures {
		fmt.Fprintf(Err, "  %s\n", f)
	}
	return 0
}

// printJSONReport renders the server's dry-run report without this package
// needing the API's types.
func printJSONReport(report map[string]any) {
	keys := []string{"records", "memories", "malformed"}
	for _, k := range keys {
		if v, ok := report[k]; ok {
			fmt.Fprintf(Out, "  %-12s %v\n", k, v)
		}
	}

	if projects, ok := report["projects"].([]any); ok && len(projects) > 0 {
		fmt.Fprintln(Out, "\nGrouped by project:")
		for _, p := range projects {
			if pm, ok := p.(map[string]any); ok {
				fmt.Fprintf(Out, "  %-52s %v records\n", orDash(fmt.Sprint(pm["project"])), pm["records"])
			}
		}
	}

	if summary, ok := report["secret_summary"].(map[string]any); ok && len(summary) > 0 {
		fmt.Fprintln(Out, "\nCredentials detected:")
		for _, k := range sortedKeys(summary) {
			fmt.Fprintf(Out, "  %-24s %v\n", k, summary[k])
		}
	}
	if secrets, ok := report["secrets"].([]any); ok && len(secrets) > 0 {
		fmt.Fprintln(Out, "\nFirst occurrences:")
		for i, s := range secrets {
			if i >= 10 {
				fmt.Fprintf(Out, "  … and %d more\n", len(secrets)-10)
				break
			}
			if sm, ok := s.(map[string]any); ok {
				fmt.Fprintf(Out, "  line %v  %v  (%v)\n", sm["line"], sm["kind"], sm["field"])
			}
		}
	}

	if warnings, ok := report["warnings"].([]any); ok {
		for _, w := range warnings {
			fmt.Fprintf(Out, "\nwarning: %v\n", w)
		}
	}
}

func sortedKindKeys(m map[redact.Kind]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

var _ = json.Marshal
