package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// cmdPrompt turns a rough description into a finished prompt.
func cmdPrompt(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	fs.SetOutput(Err)
	project := fs.String("project", "", "project to ground the prompt in; detected from the working directory by default")
	target := fs.String("target", "", "model the prompt is written for (default \"Claude Opus 5\")")
	focus := fs.String("focus", "", "comma-separated: flow, structure, coding, ux, security, cost")
	mode := fs.String("mode", "", "brief or full; chosen from the request length by default")
	preview := fs.Bool("preview", false, "show which memories would ground the prompt, without calling the LLM")
	quiet := fs.Bool("quiet", false, "print only the prompt, for piping to the clipboard")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	description := strings.TrimSpace(strings.Join(rest, " "))
	if description == "" {
		return fail("say what you want to build:\n\n  dkm prompt \"settings page that saves the download folder\"")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	body := map[string]any{"description": description, "project": resolveProject(c, *project)}
	if *target != "" {
		body["target"] = *target
	}
	if *mode != "" {
		body["mode"] = *mode
	}
	if f := splitList(*focus); len(f) > 0 {
		body["emphases"] = f
	}

	if *preview {
		return promptPreview(ctx, c, body)
	}

	var out struct {
		Generated   bool     `json:"generated"`
		Reason      string   `json:"reason"`
		Fix         string   `json:"fix"`
		Prompt      string   `json:"prompt"`
		Understood  string   `json:"understood"`
		Assumptions []string `json:"assumptions"`
		Grounded    bool     `json:"grounded"`
		Provider    string   `json:"provider"`
		Memories    []struct {
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Source string `json:"source"`
		} `json:"memories"`
		Cost struct {
			InputTokens       int  `json:"input_tokens"`
			OutputTokens      int  `json:"output_tokens"`
			IncludesReasoning bool `json:"includes_reasoning"`
		} `json:"cost"`
	}

	if !*quiet {
		fmt.Fprintln(Err, "Composing. This calls your LLM provider once.")
	}

	// Generation is one reasoning-model call, which is tens of seconds rather
	// than the default 30-second budget for an answer from Postgres.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := c.AdminLong(ctx, http.MethodPost, "/v1/prompt", body, &out); err != nil {
		return failErr(err)
	}
	if !out.Generated {
		fmt.Fprintf(Err, "\nDid not run: %s\n", out.Reason)
		if out.Fix != "" {
			fmt.Fprintf(Err, "\n%s\n", out.Fix)
		}
		return 1
	}

	// The prompt goes to stdout alone, so `dkm prompt ... | clip` pastes clean.
	// Everything else is commentary and goes to stderr.
	fmt.Fprintln(Out, out.Prompt)
	if *quiet {
		return 0
	}

	fmt.Fprintln(Err)
	if out.Understood != "" {
		fmt.Fprintf(Err, "Understood as: %s\n", out.Understood)
	}
	if len(out.Memories) > 0 {
		fmt.Fprintf(Err, "\nGrounded in %d of your own memories:\n", len(out.Memories))
		for _, m := range out.Memories {
			fmt.Fprintf(Err, "  · %s (%s)\n", truncate(m.Title, 62), m.Kind)
		}
	} else {
		fmt.Fprintln(Err, "\nNothing in your corpus matched this task, so the prompt claims no")
		fmt.Fprintln(Err, "conventions. Consolidate more sessions to ground future prompts.")
	}

	note := ""
	if out.Cost.IncludesReasoning {
		note = " (output includes model thinking)"
	}
	fmt.Fprintf(Err, "\n%s · %d in / %d out%s\n",
		out.Provider, out.Cost.InputTokens, out.Cost.OutputTokens, note)
	return 0
}

func promptPreview(ctx context.Context, c clientLike, body map[string]any) int {
	var out struct {
		Grounded bool   `json:"grounded"`
		Mode     string `json:"mode"`
		Tokens   int    `json:"estimated_input_tokens"`
		Memories []struct {
			Kind  string  `json:"kind"`
			Title string  `json:"title"`
			Score float64 `json:"score"`
		} `json:"memories"`
	}
	if err := c.AdminLong(ctx, http.MethodPost, "/v1/prompt/preview", body, &out); err != nil {
		return failErr(err)
	}

	if len(out.Memories) == 0 {
		fmt.Fprintln(Out, "Nothing in your corpus matches this task yet.")
	} else {
		fmt.Fprintf(Out, "Would ground the prompt in %d memories:\n\n", len(out.Memories))
		rows := [][]string{{"KIND", "SCORE", "TITLE"}}
		for _, m := range out.Memories {
			rows = append(rows, []string{m.Kind, fmt.Sprintf("%.3f", m.Score), truncate(m.Title, 58)})
		}
		table(Out, rows)
	}
	fmt.Fprintf(Out, "\nMode %s · about %d input tokens. Nothing was sent to the provider.\n",
		out.Mode, out.Tokens)
	return 0
}

// clientLike is the slice of the client this command needs, so the preview
// helper can be exercised without a server.
type clientLike interface {
	AdminLong(ctx context.Context, method, path string, body, out any) error
}
