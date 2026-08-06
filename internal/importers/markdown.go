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
			if strings.TrimSpace(sec.Body) == "" {
				continue
			}

			title := sec.Title
			if kind == store.KindDecision {
				if m := adrTitleRE.FindStringSubmatch(title); m != nil && m[1] != "" {
					title = m[1]
				}
			}
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			}

			for _, hit := range redact.Scan(sec.Body) {
				out.Secrets = append(out.Secrets, SecretHit{File: path, Line: sec.Line + hit.Line - 1, Kind: hit.Kind})
				out.SecretCounts[hit.Kind]++
			}

			out.Memories = append(out.Memories, store.MemoryInput{
				Kind:       kind,
				Title:      collapse(title, 200),
				Body:       strings.TrimSpace(sec.Body),
				Project:    project,
				Files:      []string{filepath.ToSlash(path)},
				Visibility: visibility,
				Source:     store.SourceImport,
			})
		}
	}

	return out, nil
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

	minLevel := 7
	for _, line := range lines {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			if lvl := len(m[1]); lvl < minLevel {
				minLevel = lvl
			}
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
	for _, line := range lines {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			if lvl := len(m[1]); lvl == minLevel+1 {
				splitLevel = minLevel + 1
				break
			}
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
		if m := headingRE.FindStringSubmatch(line); m != nil && len(m[1]) <= splitLevel {
			flush()
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
