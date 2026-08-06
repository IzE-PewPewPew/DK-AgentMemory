package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/connect"
)

func cmdLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(Err)
	key := fs.String("key", "", "API key; prompted for when omitted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		return fail("usage: dkm login <server-url> [--key <key>]")
	}

	server := strings.TrimRight(fs.Arg(0), "/")
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		server = "https://" + server
	}

	plaintext := *key
	if plaintext == "" {
		plaintext = os.Getenv("DKM_KEY")
	}
	if plaintext == "" {
		var err error
		plaintext, err = promptForKey(server)
		if err != nil {
			return fail("%v", err)
		}
	}
	if strings.TrimSpace(plaintext) == "" {
		return fail("no key given")
	}

	cfg := config.ClientDefaults()
	cfg.Server = server
	cfg.Key = strings.TrimSpace(plaintext)
	if err := cfg.Validate(); err != nil {
		return fail("%v", err)
	}

	// Verify before writing. A config file containing a key that does not work
	// turns the next command's failure into a puzzle rather than an answer.
	probe, err := client.NewWithConfig(cfg)
	if err != nil {
		return failErr(err)
	}
	health, err := probe.Health(ctx)
	if err != nil {
		return failErr(err)
	}

	cfg.User = health.Caller.User
	cfg.Team = health.Caller.Team
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(Out, "Logged in to %s (v%s)\n", server, health.Version)
	fmt.Fprintf(Out, "  user    %s\n", orDash(health.Caller.User))
	fmt.Fprintf(Out, "  team    %s\n", orDash(health.Caller.Team))
	fmt.Fprintf(Out, "  key     %s…\n", cfg.KeyPrefix())
	fmt.Fprintf(Out, "  config  %s\n\n", cfg.Path())
	fmt.Fprintf(Out, "Next:\n  dkm connect --all\n")
	return 0
}

// promptForKey reads a key from the terminal without echoing it.
func promptForKey(server string) (string, error) {
	fmt.Fprintf(Out, "API key for %s\n", server)
	fmt.Fprintf(Out, "(issued with `dkm admin key issue`; input is hidden): ")

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(Out)
		if err != nil {
			return "", fmt.Errorf("reading the key: %w", err)
		}
		return string(raw), nil
	}

	// Not a terminal: read a line, so `echo $KEY | dkm login …` works in CI.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func cmdConnect(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(Err)
	all := fs.Bool("all", false, "wire every detected tool")
	list := fs.Bool("list", false, "list known tools and whether they are installed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	statuses := connect.Detect()

	if *list {
		rows := [][]string{{"AGENT", "INSTALLED", "CONNECTED", "CONFIG"}}
		for _, st := range statuses {
			rows = append(rows, []string{
				st.Agent.Name,
				yesNo(st.Installed),
				yesNo(st.Connected),
				shorten(st.ConfigPath),
			})
		}
		table(Out, rows)
		return 0
	}

	var targets []connect.Status
	switch {
	case *all:
		for _, st := range statuses {
			if st.Installed {
				targets = append(targets, st)
			}
		}
	case fs.NArg() >= 1:
		for _, name := range fs.Args() {
			agent, ok := connect.ByID(name)
			if !ok {
				fmt.Fprintf(Err, "dkm: unknown agent %q. Known agents:\n", name)
				for _, st := range statuses {
					fmt.Fprintf(Err, "  %s\n", st.Agent.ID)
				}
				return 2
			}
			targets = append(targets, connect.Status{Agent: agent, Installed: agent.Installed(), ConfigPath: agent.ConfigPath()})
		}
	default:
		return fail("usage: dkm connect [--all | --list | <agent>...]")
	}

	if len(targets) == 0 {
		fmt.Fprintln(Out, "No supported AI tools found on this machine.")
		fmt.Fprintln(Out, "Run `dkm connect --list` to see what is looked for, or name one explicitly.")
		return 0
	}

	binary := connect.BinaryPath()
	fmt.Fprintln(Out, "Scanning for AI tools...")

	connected, unchanged := 0, 0
	restartNeeded := false

	for _, st := range targets {
		if !st.Installed {
			fmt.Fprintf(Out, "  – %-18s not installed\n", st.Agent.Name)
			continue
		}
		res, err := connect.Connect(st.Agent, binary)
		if err != nil {
			fmt.Fprintf(Out, "  ✗ %-18s %v\n", st.Agent.Name, err)
			continue
		}

		note := "wired"
		if res.Hooks {
			note += " (+ hooks)"
		}
		if !res.Changed {
			note = "already wired"
			unchanged++
		} else {
			connected++
		}
		if st.Agent.ID == "claude-desktop" || st.Agent.ID == "cursor" || st.Agent.ID == "windsurf" {
			restartNeeded = true
		}
		fmt.Fprintf(Out, "  ✓ %-18s %-45s %s\n", st.Agent.Name, shorten(res.ConfigPath), note)
	}

	fmt.Fprintln(Out)
	switch {
	case connected > 0:
		fmt.Fprintf(Out, "%d tool%s connected", connected, plural(connected))
		if unchanged > 0 {
			fmt.Fprintf(Out, ", %d already up to date", unchanged)
		}
		fmt.Fprintln(Out, ".")
	case unchanged > 0:
		fmt.Fprintf(Out, "Everything was already up to date (%d tool%s).\n", unchanged, plural(unchanged))
	}

	if restartNeeded {
		// The single most common "it did not work": quitting the window rather
		// than the application leaves the old config loaded.
		fmt.Fprintln(Out, "\nRestart any GUI app you just wired. Quit it fully from the tray or menu bar —")
		fmt.Fprintln(Out, "closing the window leaves the process running with the previous config.")
	}
	fmt.Fprintln(Out, "\nVerify with `dkm doctor`, or ask the agent: \"what memory tools do you have?\" (twelve means connected).")
	return 0
}

func cmdDisconnect(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	fs.SetOutput(Err)
	all := fs.Bool("all", false, "remove dkm from every detected tool")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var agents []connect.Agent
	if *all {
		for _, st := range connect.Detect() {
			if st.Installed {
				agents = append(agents, st.Agent)
			}
		}
	} else {
		for _, name := range fs.Args() {
			a, ok := connect.ByID(name)
			if !ok {
				return fail("unknown agent %q", name)
			}
			agents = append(agents, a)
		}
	}
	if len(agents) == 0 {
		return fail("usage: dkm disconnect [--all | <agent>...]")
	}

	for _, a := range agents {
		res, err := connect.Disconnect(a)
		if err != nil {
			fmt.Fprintf(Out, "  ✗ %-18s %v\n", a.Name, err)
			continue
		}
		state := "removed"
		if !res.Changed {
			state = "was not configured"
		}
		fmt.Fprintf(Out, "  ✓ %-18s %s\n", a.Name, state)
	}
	fmt.Fprintln(Out, "\n~/.dkm is untouched. Remove it to delete the stored key and local mirror.")
	return 0
}

func cmdDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(Err)
	verbose := fs.Bool("verbose", false, "print every path checked and every decision made")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	problems := 0
	report := func(label, value, detail string) {
		if detail != "" {
			fmt.Fprintf(Out, "%-11s %-40s %s\n", label, value, detail)
		} else {
			fmt.Fprintf(Out, "%-11s %s\n", label, value)
		}
	}

	// --- config ---
	cfg, err := config.LoadClient()
	if err != nil {
		report("Config", config.ClientPath(), "✗ "+firstLine(err.Error()))
		fmt.Fprintf(Err, "\nFix: run `dkm login <server-url>`.\n")
		return 1
	}
	if *verbose {
		report("Config", cfg.Path(), "loaded")
	}

	c, err := client.NewWithConfig(cfg)
	if err != nil {
		return failErr(err)
	}

	// --- server ---
	health, herr := c.Health(ctx)
	switch {
	case herr == nil:
		report("Server", cfg.Server, "reachable, v"+health.Version)
	case client.IsUnauthorized(herr):
		report("Server", cfg.Server, "reachable")
		report("Auth", cfg.KeyPrefix()+"…", "✗ rejected — run `dkm login` with a current key")
		problems++
	default:
		report("Server", cfg.Server, "✗ "+firstLine(herr.Error()))
		fmt.Fprintf(Out, "%-11s %s\n", "", "Reads fall back to the local mirror; writes queue until it returns.")
		problems++
	}

	if herr == nil {
		who := health.Caller.User
		if health.Caller.Team != "" {
			who += " · " + health.Caller.Team
		}
		report("Auth", fmt.Sprintf("%s (%s…)", orDash(who), cfg.KeyPrefix()), "valid")

		if !health.Database.OK {
			report("Database", "✗", health.Database.Error)
			problems++
		}
		if !health.Embedder.OK {
			report("Embedder", health.Embedder.Detail, "✗ "+firstLine(health.Embedder.Error))
			fmt.Fprintf(Out, "%-11s %s\n", "", "Search is keyword-only until this recovers. Writes are unaffected.")
			problems++
		} else if *verbose {
			report("Embedder", health.Embedder.Detail, "ok")
		}
	}

	// --- project ---
	wd, _ := os.Getwd()
	project := c.Project(wd)
	report("Project", orDash(project.ID), "via "+string(project.Source))
	if project.Warning != "" {
		problems++
		for _, line := range wrap(project.Warning, 78) {
			fmt.Fprintf(Out, "%-11s %s\n", "", line)
		}
		fmt.Fprintf(Out, "%-11s Fix: echo '%s' > .dkm/project and commit it.\n", "", orDash(project.ID))
	}

	// --- memories ---
	if herr == nil && health.Stats != nil {
		detail := fmt.Sprintf("%d embedded", health.Stats.Embedded)
		if health.Stats.Memories > 0 && health.Stats.Embedded < health.Stats.Memories {
			// Not a failure. The backfill pass vectorises these on its next
			// run; saying so stops it looking like data loss.
			detail += fmt.Sprintf(", %d awaiting the embedding backfill",
				health.Stats.Memories-health.Stats.Embedded)
		}
		report("Memories", fmt.Sprintf("%d in store", health.Stats.Memories), detail)
	}

	// --- tools ---
	var wired, missing []string
	for _, st := range connect.Detect() {
		if !st.Installed {
			if *verbose {
				missing = append(missing, st.Agent.Name)
			}
			continue
		}
		mark := "✓"
		if !st.Connected {
			mark = "✗"
			problems++
		}
		wired = append(wired, st.Agent.Name+" "+mark)
		if *verbose {
			fmt.Fprintf(Out, "%-11s %-40s %s\n", "", st.Agent.Name, shorten(st.ConfigPath))
		}
	}
	if len(wired) == 0 {
		report("Tools", "none detected", "run `dkm connect --list`")
	} else {
		report("Tools", strings.Join(wired, "  "), "")
	}
	for _, m := range missing {
		fmt.Fprintf(Out, "%-11s %-40s not installed\n", "", m)
	}

	// --- sync ---
	if m := c.Mirror(); m != nil {
		queue, _ := m.Queue()
		cached, _ := m.Memories()
		state := "up to date"
		if len(queue) > 0 {
			state = fmt.Sprintf("%d write%s queued — run `dkm push`", len(queue), plural(len(queue)))
		}
		report("Sync", fmt.Sprintf("%d cached locally", len(cached)), state)
		if *verbose {
			fmt.Fprintf(Out, "%-11s mirror %s\n", "", m.Dir())
		}
	} else {
		report("Sync", "disabled", "sync.enabled is false in "+cfg.Path())
	}

	fmt.Fprintln(Out)
	if problems == 0 {
		fmt.Fprintln(Out, "No problems found.")
		return 0
	}
	fmt.Fprintf(Out, "%d problem%s found. Each line above names the file or command that fixes it.\n", problems, plural(problems))
	return 1
}

// --- small helpers ---------------------------------------------------------

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// shorten replaces the home directory with ~ so paths fit on one line.
func shorten(p string) string {
	if p == "" {
		return "—"
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	return append(lines, cur)
}
