package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Flags after positionals is the form people actually type, and the form the
// README documents. Go's flag package stops at the first positional, so without
// permutation `dkm import markdown ./notes --apply` treats `--apply` as a path,
// runs a dry run, and reports success — a command that did nothing and said so
// in a way that reads like an empty corpus.
func TestParseFlagsAcceptsFlagsAfterPositionals(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPos  []string
		wantBool bool
		wantStr  string
	}{
		{"flags last", []string{"./notes", "--apply", "--project", "p"}, []string{"./notes"}, true, "p"},
		{"flags first", []string{"--apply", "--project", "p", "./notes"}, []string{"./notes"}, true, "p"},
		{"interleaved", []string{"--project", "p", "./notes", "--apply"}, []string{"./notes"}, true, "p"},
		{"equals form", []string{"./notes", "--project=p", "--apply"}, []string{"./notes"}, true, "p"},
		{"single dash", []string{"./notes", "-apply", "-project", "p"}, []string{"./notes"}, true, "p"},
		{"no flags", []string{"./a", "./b"}, []string{"./a", "./b"}, false, ""},
		{"only flags", []string{"--apply"}, nil, true, ""},
		{"multi positional", []string{"./a", "--apply", "./b"}, []string{"./a", "./b"}, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			apply := fs.Bool("apply", false, "")
			project := fs.String("project", "", "")

			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}
			if *apply != tc.wantBool {
				t.Errorf("apply: got %v, want %v", *apply, tc.wantBool)
			}
			if *project != tc.wantStr {
				t.Errorf("project: got %q, want %q", *project, tc.wantStr)
			}
			if strings.Join(pos, "|") != strings.Join(tc.wantPos, "|") {
				t.Errorf("positional: got %v, want %v", pos, tc.wantPos)
			}
		})
	}
}

// A file genuinely named like a flag is reachable through the conventional
// `--` separator.
func TestParseFlagsDoubleDashEndsFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "")

	pos, err := parseFlags(fs, []string{"--apply", "--", "--not-a-flag", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !*apply {
		t.Error("apply should have been set before the separator")
	}
	if strings.Join(pos, "|") != "--not-a-flag|-x" {
		t.Errorf("positional: got %v", pos)
	}
}

func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("apply", false, "")

	if _, err := parseFlags(fs, []string{"./x", "--nonsense"}); err == nil {
		t.Fatal("an unknown flag must be an error, not a filename")
	}
}

// A search query is several positionals joined together; a quoted query is one.
// Both must survive alongside flags.
func TestParseFlagsMultiWordQuery(t *testing.T) {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 8, "")

	pos, err := parseFlags(fs, []string{"serverless", "token", "signing", "--limit", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if *limit != 3 {
		t.Errorf("limit: got %d, want 3", *limit)
	}
	if got := strings.Join(pos, " "); got != "serverless token signing" {
		t.Errorf("query: got %q", got)
	}
}
