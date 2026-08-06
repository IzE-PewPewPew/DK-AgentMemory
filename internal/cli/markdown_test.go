package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/importers"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// The round trip is the whole contract of the Markdown format: a person exports
// a corpus, edits it in a text editor, and imports it back. If a field is lost
// on the way, the edit silently becomes a data-loss event, and the person who
// finds out is the one whose lesson turned back into an untyped fact.
func TestMarkdownRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	project := "github.com/devkuong/launcher"

	records := []mdRecord{
		{
			Type: "memory", ID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Kind: "lesson",
			Title:      "always use full paths with pkill on multi-service hosts",
			Body:       "pkill cloudflared killed an unrelated service that had cloudflared in its argv.",
			Project:    project,
			Files:      []string{"deploy/ecosystem.config.js", "docs/runbook with space.md"},
			Visibility: "team", Source: "manual", CreatedAt: "2026-08-01T00:00:00Z",
		},
		{
			Type: "memory", ID: "01JBBBBBBBBBBBBBBBBBBBBBBB", Kind: "decision",
			Title:      "we chose jose over jsonwebtoken",
			Body:       "Edge runtime compatibility.\n\n## Alternatives considered\n\njsonwebtoken needs Node crypto.",
			Project:    project,
			Visibility: "private", Source: "manual", CreatedAt: "2026-08-02T00:00:00Z",
		},
		{
			Type: "memory", ID: "01JCCCCCCCCCCCCCCCCCCCCCCC", Kind: "fact",
			Title:   "deploys need Node 20, not 22",
			Body:    "The build fails on 22 because of a native module.",
			Project: project, Visibility: "private", CreatedAt: "2026-08-03T00:00:00Z",
		},
	}

	doc := renderMarkdown(project, records, now)

	dir := t.TempDir()
	path := filepath.Join(dir, "memories.md")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := importers.ScanMarkdown([]string{path}, project, "private")
	if err != nil {
		t.Fatalf("ScanMarkdown: %v", err)
	}

	if len(preview.Memories) != len(records) {
		var got []string
		for _, m := range preview.Memories {
			got = append(got, m.Kind+": "+m.Title)
		}
		t.Fatalf("round trip produced %d memories, want %d:\n  %s\n\n--- document ---\n%s",
			len(preview.Memories), len(records), strings.Join(got, "\n  "), doc)
	}

	byTitle := map[string]store.MemoryInput{}
	for _, m := range preview.Memories {
		byTitle[m.Title] = m
	}

	for _, want := range records {
		got, ok := byTitle[want.Title]
		if !ok {
			t.Errorf("memory %q missing after round trip", want.Title)
			continue
		}
		if got.Kind != want.Kind {
			t.Errorf("%q kind: got %q, want %q", want.Title, got.Kind, want.Kind)
		}
		if got.Visibility != want.Visibility {
			t.Errorf("%q visibility: got %q, want %q", want.Title, got.Visibility, want.Visibility)
		}
		if len(want.Files) > 0 {
			if strings.Join(got.Files, ",") != strings.Join(want.Files, ",") {
				t.Errorf("%q files: got %v, want %v", want.Title, got.Files, want.Files)
			}
		}
		// The metadata comment must not survive into the body a human reads.
		if strings.Contains(got.Body, "<!--") {
			t.Errorf("%q body still contains a comment:\n%s", want.Title, got.Body)
		}
	}
}

// A file path containing a space is the reason the metadata block is one field
// per line rather than space-separated.
func TestMarkdownRoundTripPreservesPathsWithSpaces(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := mdRecord{
		Type: "memory", Kind: "fact", Title: "a fact", Body: "a body",
		Files: []string{"docs/my notes.md", "src/a.ts"}, Visibility: "team",
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	if err := os.WriteFile(path, []byte(renderMarkdown("p", []mdRecord{rec}, now)), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := importers.ScanMarkdown([]string{path}, "p", "private")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Memories) != 1 {
		t.Fatalf("got %d memories, want 1", len(preview.Memories))
	}
	if got := preview.Memories[0].Files; strings.Join(got, "|") != "docs/my notes.md|src/a.ts" {
		t.Fatalf("files: got %v", got)
	}
}

// A body containing its own headings must not split itself into extra memories
// on the next export/import cycle.
func TestMarkdownHeadingsInBodyDoNotSplit(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := mdRecord{
		Type: "memory", Kind: "fact", Title: "outer",
		Body: "intro\n\n## inner heading\n\nmore text\n\n### deeper\n\ntail",
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	if err := os.WriteFile(path, []byte(renderMarkdown("p", []mdRecord{rec}, now)), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := importers.ScanMarkdown([]string{path}, "p", "private")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Memories) != 1 {
		var titles []string
		for _, m := range preview.Memories {
			titles = append(titles, m.Title)
		}
		t.Fatalf("body headings split the memory into %d: %v", len(preview.Memories), titles)
	}
	if !strings.Contains(preview.Memories[0].Body, "inner heading") {
		t.Error("inner heading text was lost")
	}
}

// Fenced code containing a `#` comment is not a heading.
func TestMarkdownCodeFenceIsNotAHeading(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	body := "run it:\n\n```bash\n# comment that looks like a heading\npkill -f dkm\n```\n"
	rec := mdRecord{Type: "memory", Kind: "fact", Title: "how to restart", Body: body}

	doc := renderMarkdown("p", []mdRecord{rec}, now)
	if strings.Contains(doc, "### comment that looks like a heading") {
		t.Fatalf("a comment inside a code fence was demoted as if it were a heading:\n%s", doc)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := importers.ScanMarkdown([]string{path}, "p", "private")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Memories) != 1 {
		t.Fatalf("got %d memories, want 1", len(preview.Memories))
	}
	if !strings.Contains(preview.Memories[0].Body, "pkill -f dkm") {
		t.Error("code fence content was lost")
	}
}

func TestProjectFilename(t *testing.T) {
	cases := map[string]string{
		"github.com/devkuong/launcher": "github.com__devkuong__launcher.md",
		"":                             "team-wide.md",
		"a:b*c?d":                      "a__b__c__d.md",
	}
	for in, want := range cases {
		if got := projectFilename(in); got != want {
			t.Errorf("projectFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
