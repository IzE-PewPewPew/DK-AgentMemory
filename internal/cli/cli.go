// Package cli implements every dkm subcommand.
//
// One binary, three roles: the server (`serve`, `migrate`), the MCP shim
// (`mcp`, `hook`), and the human-facing client (everything else). Shipping them
// together means the version an agent talks through and the version a user
// types are the same build, and there is no second artefact to keep in step.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// Out and Err are the output streams, replaceable in tests.
var (
	Out io.Writer = os.Stdout
	Err io.Writer = os.Stderr
)

type command struct {
	name    string
	group   string
	summary string
	usage   string
	run     func(ctx context.Context, args []string) int
}

func commands() []command {
	return []command{
		{"login", "setup", "Store a server URL and API key", "dkm login <server-url> [--key <key>]", cmdLogin},
		{"connect", "setup", "Wire installed AI tools to this server", "dkm connect [--all | --list | <agent>]", cmdConnect},
		{"disconnect", "setup", "Remove dkm from agent configs", "dkm disconnect [--all | <agent>]", cmdDisconnect},
		{"doctor", "setup", "Check the whole setup and say what to fix", "dkm doctor [--verbose]", cmdDoctor},

		{"save", "memory", "Store a fact, decision, or preference", `dkm save "text" [--kind fact] [--team]`, cmdSave},
		{"search", "memory", "Hybrid search across your memories", `dkm search "query" [--project P] [--limit N]`, cmdSearch},
		{"lesson", "memory", "Store a durable rule", `dkm lesson "always X because Y"`, cmdLesson},
		{"lessons", "memory", "List active lessons", "dkm lessons [--project P]", cmdLessons},
		{"context", "memory", "Print the project briefing an agent would receive", "dkm context [--project P]", cmdContext},
		{"share", "memory", "Promote a private memory to the team", "dkm share <id>", cmdShare},
		{"feed", "memory", "Show what teammates shared recently", "dkm feed [--limit N]", cmdFeed},
		{"forget", "memory", "Delete a memory", "dkm forget <id> [--yes]", cmdForget},
		{"supersede", "memory", "Mark a memory replaced by a newer one", "dkm supersede <old-id> <new-id>", cmdSupersede},
		{"projects", "memory", "List projects with memory counts", "dkm projects", cmdProjects},
		{"graph", "memory", "Show related entities for a project", "dkm graph [--project P] [--node F]", cmdGraph},

		{"status", "sync", "Show sync state and the offline queue", "dkm status", cmdStatus},
		{"sync", "sync", "Push queued writes, then pull changes", "dkm sync", cmdSync},
		{"push", "sync", "Flush the offline write queue", "dkm push", cmdPush},
		{"pull", "sync", "Refresh the local mirror", "dkm pull", cmdPull},

		{"import", "transfer", "Import history from an agent or from markdown", "dkm import <claude-code|codex|markdown|ndjson> [--dry-run]", cmdImport},
		{"export", "transfer", "Stream your corpus as NDJSON", "dkm export [--scope me|team] [-o file]", cmdExport},

		{"serve", "server", "Run the API, viewer, and consolidation worker", "dkm serve [--config path]", cmdServe},
		{"migrate", "server", "Apply pending database migrations", "dkm migrate [--config path]", cmdMigrate},
		{"admin", "server", "Manage teams, users, and keys", "dkm admin <team|user|key> ...", cmdAdmin},

		{"mcp", "agent", "Run the MCP server on stdio", "dkm mcp", cmdMCP},
		{"hook", "agent", "Lifecycle hook entry point, called by agents", "dkm hook <event>", cmdHook},

		{"version", "other", "Print build information", "dkm version", cmdVersion},
		{"help", "other", "Show this help", "dkm help [command]", nil},
	}
}

// Main runs the CLI and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	name := args[0]
	rest := args[1:]

	switch name {
	case "-h", "--help", "help":
		if len(rest) > 0 {
			return helpFor(rest[0])
		}
		usage()
		return 0
	case "-v", "--version":
		return cmdVersion(context.Background(), nil)
	}

	for _, c := range commands() {
		if c.name != name || c.run == nil {
			continue
		}

		// Ctrl-C cancels the command's context rather than killing the process
		// outright, so an interrupted sync flushes what it has and an
		// interrupted serve drains its connections.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return c.run(ctx, rest)
	}

	fmt.Fprintf(Err, "dkm: unknown command %q\n", name)
	if suggestion := nearestCommand(name); suggestion != "" {
		fmt.Fprintf(Err, "Did you mean `dkm %s`?\n", suggestion)
	}
	fmt.Fprintln(Err, "Run `dkm help` for the full list.")
	return 2
}

func usage() {
	fmt.Fprintf(Out, "dkm — DevKuong Memories, one shared memory for every AI coding tool.\n\n")
	fmt.Fprintf(Out, "Usage:\n  dkm <command> [flags]\n\n")

	groups := []struct{ id, title string }{
		{"setup", "Setup"},
		{"memory", "Memory"},
		{"sync", "Sync"},
		{"transfer", "Import and export"},
		{"server", "Server"},
		{"agent", "Agent integration"},
		{"other", ""},
	}

	all := commands()
	for _, g := range groups {
		var rows []command
		for _, c := range all {
			if c.group == g.id {
				rows = append(rows, c)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if g.title != "" {
			fmt.Fprintf(Out, "%s\n", g.title)
		}
		tw := tabwriter.NewWriter(Out, 0, 0, 2, ' ', 0)
		for _, c := range rows {
			fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
		}
		tw.Flush()
		fmt.Fprintln(Out)
	}

	fmt.Fprintf(Out, "First run:\n")
	fmt.Fprintf(Out, "  dkm login https://memories.example.com\n")
	fmt.Fprintf(Out, "  dkm connect --all\n")
	fmt.Fprintf(Out, "  dkm doctor\n\n")
	fmt.Fprintf(Out, "Docs: https://github.com/IzE-PewPewPew/DK-AgentMemory\n")
}

func helpFor(name string) int {
	for _, c := range commands() {
		if c.name == name {
			fmt.Fprintf(Out, "%s\n\n%s\n", c.summary, c.usage)
			return 0
		}
	}
	fmt.Fprintf(Err, "dkm: no such command %q\n", name)
	return 2
}

func nearestCommand(name string) string {
	best, bestDist := "", 1<<30
	for _, c := range commands() {
		d := editDistance(name, c.name)
		if d < bestDist {
			best, bestDist = c.name, d
		}
	}
	if bestDist > len(best)/2+1 {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := curr[j-1] + 1
			if prev[j]+1 < m {
				m = prev[j] + 1
			}
			if prev[j-1]+cost < m {
				m = prev[j-1] + cost
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func cmdVersion(context.Context, []string) int {
	fmt.Fprintln(Out, version.String())
	return 0
}

// --- shared helpers --------------------------------------------------------

// parseFlags parses a command's flags whether they appear before or after its
// positional arguments, and returns the positionals.
//
// Go's flag package stops at the first non-flag argument. That makes
// `dkm import markdown ./notes --apply` — the form written in the README —
// treat `--apply` as a second path, perform a dry run, and report success. The
// command did nothing and said so in a way that reads like an empty import
// rather than a misparse, which is the worst kind of wrong: quiet, plausible,
// and repeatable.
//
// Permuting the arguments before parsing is what every Unix CLI does and what
// anyone typing this expects.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after a bare `--` is positional, by convention. This is
		// how a file legitimately named `--apply` would be passed.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			// `--name value` consumes the next argument; `--name=value` and
			// boolean flags do not.
			if !strings.Contains(a, "=") {
				if f := fs.Lookup(strings.TrimLeft(a, "-")); f != nil && !isBoolFlag(f) && i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
			continue
		}
		positional = append(positional, a)
	}

	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return append(positional, fs.Args()...), nil
}

// isBoolFlag reports whether a flag takes no value.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// fail prints an error and returns exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(Err, "dkm: "+format+"\n", a...)
	return 1
}

// failErr renders an error, translating the ones with a known remedy into
// instructions rather than diagnostics.
func failErr(err error) int {
	switch {
	case errors.Is(err, config.ErrNotLoggedIn):
		fmt.Fprintf(Err, "dkm: not logged in.\n\n  dkm login https://your-server\n\nOr set DKM_SERVER and DKM_KEY.\n")
	case client.IsOffline(err):
		fmt.Fprintf(Err, "dkm: %v\n\nThe server is unreachable. Reads fall back to the local mirror and writes are queued;\nrun `dkm sync` once you are back online.\n", err)
	case client.IsUnauthorized(err):
		fmt.Fprintf(Err, "dkm: %v\n\nRun `dkm login <server-url>` with a current key.\n", err)
	default:
		fmt.Fprintf(Err, "dkm: %v\n", err)
	}
	return 1
}

// newClient builds a client, reporting setup problems in a way that says what
// to do next.
func newClient() (*client.Client, error) {
	c, err := client.New()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// table writes aligned columns.
func table(w io.Writer, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// truncate shortens a string for single-line display.
func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// plural returns "s" unless n is 1, so messages read as English.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
