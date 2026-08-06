package importers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/redact"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
)

// MarkdownPreview is what a markdown import would produce.
type MarkdownPreview struct {
	Files        int
	Memories     []store.MemoryInput
	Secrets      []SecretHit
	SecretCounts map[redact.Kind]int
	Warnings     []string
}

var headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// adrTitleRE recognises the conventional ADR filename and heading forms, so
// architecture decisions arrive as decisions rather than as undifferentiated
// facts. The distinction matters at retrieval: a decision carries a reason, and
// the context builder ranks it above a plain fact.
var adrTitleRE = regexp.MustCompile(`(?i)^(?:adr[- _]?\d+|\d{4})[-_. ]*(.*)$`)

// ScanMarkdown converts documentation into memories.
//
// The unit is a section, not a file. A CLAUDE.md is a list of independent
// statements, and importing it as one memory produces one search hit that
// contains everything and answers nothing.
func ScanMarkdown(paths []string, project string, visibility string) (*MarkdownPreview, error) {
	out := &MarkdownPreview{SecretCounts: map[redact.Kind]int{}}

	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				// Skip directories that are large, generated, or not ours.
				switch d.Name() {
				case ".git", "node_modules", "vendor", "dist", "build", ".next", "target":
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", p, err))
		}
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		out.Files++

		kind := store.KindFact
		lower := strings.ToLower(filepath.Base(path))
		if strings.Contains(filepath.ToSlash(strings.ToLower(path)), "/adr") ||
			strings.HasPrefix(lower, "adr") || strings.Contains(lower, "decision") {
			kind = store.KindDecision
		}

		for _, sec := range splitSections(string(data)) {
			// The banner an export writes under the document's H1 — counts, a
			// date, a horizontal rule. Skipped explicitly rather than by
			// guessing from its shape, because "looks like a header" is not a
			// property anything should depend on.
			if exportHeaderRE.MatchString(sec.Body) {
				continue
			}

			// Metadata written by `dkm export --format md` is read back before
			// the comment is stripped, so a round trip preserves kind,
			// visibility, and file associations rather than re-deriving them
			// from the filename.
			meta := parseDKMMeta(sec.Body)
			body := strings.TrimSpace(stripHTMLComments(sec.Body))

			// Empty after stripping means the section held nothing but a
			// comment — the export header, for instance. Importing that would
			// create a memory titled after the project containing nothing.
			if body == "" {
				continue
			}

			title := sec.Title
			sectionKind := kind
			if k := meta["kind"]; store.ValidKind(k) {
				sectionKind = k
			}
			if sectionKind == store.KindDecision {
				if m := adrTitleRE.FindStringSubmatch(title); m != nil && m[1] != "" {
					title = m[1]
				}
			}
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			}

			sectionVisibility := visibility
			if v := meta["visibility"]; store.ValidVisibility(v) {
				sectionVisibility = v
			}

			// A memory that named source files keeps naming them. Falling back
			// to the containing document is right for hand-written notes, and
			// wrong for a re-import, where it would replace real file
			// associations with the path of the export.
			files := []string{filepath.ToSlash(path)}
			if raw := meta["files"]; raw != "" {
				files = nil
				for _, f := range strings.Split(raw, ",") {
					if f = strings.TrimSpace(f); f != "" {
						files = append(files, f)
					}
				}
			}

			for _, hit := range redact.Scan(body) {
				out.Secrets = append(out.Secrets, SecretHit{File: path, Line: sec.Line + hit.Line - 1, Kind: hit.Kind})
				out.SecretCounts[hit.Kind]++
			}

			out.Memories = append(out.Memories, store.MemoryInput{
				Kind:       sectionKind,
				Title:      collapse(title, 200),
				Body:       body,
				Project:    project,
				Files:      files,
				Visibility: sectionVisibility,
				Source:     store.SourceImport,
			})
		}
	}

	return out, nil
}

var (
	htmlCommentRE  = regexp.MustCompile(`(?s)<!--.*?-->`)
	dkmMetaRE      = regexp.MustCompile(`(?s)<!--\s*dkm\s(.*?)-->`)
	exportHeaderRE = regexp.MustCompile(`<!--\s*dkm-export\b`)
	fenceRE        = regexp.MustCompile("^\\s*(```|~~~)")
)

// fencedLines marks which lines sit inside a fenced code block.
//
// Without this, a shell comment in an example — `# restart the service` — is
// indistinguishable from an H1, and a document that merely documents a command
// splits itself into extra memories on import. The heading scan below consults
// this for every line rather than pattern-matching harder.
func fencedLines(lines []string) []bool {
	out := make([]bool, len(lines))
	inFence := false
	for i, line := range lines {
		if fenceRE.MatchString(line) {
			inFence = !inFence
			out[i] = true // the fence marker itself is never a heading
			continue
		}
		out[i] = inFence
	}
	return out
}

// parseDKMMeta reads the metadata block that `dkm export --format md` writes.
//
// One `key=value` per line, so a value may contain spaces and commas without
// any quoting rules for the two sides to disagree about. Unknown keys are
// ignored rather than rejected: a document written by a newer version should
// still import into an older one, minus the fields it does not understand.
func parseDKMMeta(body string) map[string]string {
	m := dkmMetaRE.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(m[1], "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if key = strings.TrimSpace(key); key != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

// stripHTMLComments removes comments so they do not become memory bodies.
func stripHTMLComments(s string) string {
	return htmlCommentRE.ReplaceAllString(s, "")
}

type section struct {
	Title string
	Body  string
	Line  int
}

// splitSections breaks a document at headings.
//
// Sections are split at the shallowest heading level the document actually
// uses, so a file whose structure starts at ## is not treated as one section
// because it has no #.
func splitSections(doc string) []section {
	lines := strings.Split(doc, "\n")
	fenced := fencedLines(lines)

	// heading returns the level of a real heading on line i, or 0.
	heading := func(i int) int {
		if fenced[i] {
			return 0
		}
		if m := headingRE.FindStringSubmatch(lines[i]); m != nil {
			return len(m[1])
		}
		return 0
	}

	minLevel := 7
	for i := range lines {
		if lvl := heading(i); lvl > 0 && lvl < minLevel {
			minLevel = lvl
		}
	}
	if minLevel == 7 {
		// No headings at all: the whole document is one memory, titled by its
		// first non-empty line.
		body := strings.TrimSpace(doc)
		if body == "" {
			return nil
		}
		title := body
		if i := strings.IndexByte(title, '\n'); i >= 0 {
			title = title[:i]
		}
		return []section{{Title: strings.TrimSpace(title), Body: body, Line: 1}}
	}

	// Split one level below the top when there is one, so a document with a
	// single H1 and many H2s yields one memory per H2 rather than one in total.
	splitLevel := minLevel
	for i := range lines {
		if heading(i) == minLevel+1 {
			splitLevel = minLevel + 1
			break
		}
	}

	var out []section
	var cur *section
	var body strings.Builder

	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(body.String())
			if cur.Body != "" || cur.Title != "" {
				out = append(out, *cur)
			}
		}
		body.Reset()
	}

	for i, line := range lines {
		if lvl := heading(i); lvl > 0 && lvl <= splitLevel {
			flush()
			m := headingRE.FindStringSubmatch(line)
			cur = &section{Title: strings.TrimSpace(m[2]), Line: i + 1}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteString("\n")
		} else if strings.TrimSpace(line) != "" {
			// Content above the first heading: keep it rather than drop it.
			cur = &section{Title: "", Line: i + 1}
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	flush()

	return out
}
