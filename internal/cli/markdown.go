package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Markdown export exists so a corpus can be read, reviewed, and edited by a
// person — and then imported back.
//
// The round trip is the constraint that shapes the format. `dkm import markdown`
// splits a document at one heading level and turns each section into a memory,
// so the export puts exactly one memory per `##` heading and nothing else at
// that level. Grouping headers would read more nicely and would silently break
// the round trip by collapsing every lesson into a single memory, so the kind
// is carried in the metadata comment instead and the ordering does the
// grouping work.
//
// Structured fields ride in an HTML comment: invisible in every Markdown
// renderer, unambiguous to parse, and impossible to confuse with prose.

// mdRecord is one exported memory, decoded from the NDJSON transfer stream.
type mdRecord struct {
	Type       string   `json:"type"`
	ID         string   `json:"id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Title      string   `json:"title,omitempty"`
	Body       string   `json:"body,omitempty"`
	Project    string   `json:"project,omitempty"`
	Files      []string `json:"files,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	Source     string   `json:"source,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
}

// kindOrder puts the most durable material first, so a reader who stops after
// the first screen has read the things most likely to matter.
var kindOrder = map[string]int{
	"lesson":     0,
	"decision":   1,
	"preference": 2,
	"fact":       3,
}

// renderMarkdown turns one project's memories into a document.
func renderMarkdown(project string, records []mdRecord, now time.Time) string {
	sorted := append([]mdRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ki, kj := kindOrder[sorted[i].Kind], kindOrder[sorted[j].Kind]
		if ki != kj {
			return ki < kj
		}
		return sorted[i].CreatedAt > sorted[j].CreatedAt // newest first within a kind
	})

	var b strings.Builder

	title := project
	if title == "" {
		title = "Team-wide memories"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	// The header is a comment so that re-importing does not turn it into a
	// memory. The importer strips comments and skips sections left empty.
	fmt.Fprintf(&b, "<!-- dkm-export project=%q count=%d exported=%s -->\n\n",
		project, len(sorted), now.UTC().Format(time.RFC3339))

	counts := map[string]int{}
	for _, r := range sorted {
		counts[r.Kind]++
	}
	var summary []string
	for _, k := range []string{"lesson", "decision", "preference", "fact"} {
		if counts[k] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s%s", counts[k], k, plural(counts[k])))
		}
	}
	if len(summary) > 0 {
		fmt.Fprintf(&b, "*%s. Exported %s.*\n\n",
			strings.Join(summary, ", "), now.Format("2 January 2006"))
	}
	b.WriteString("---\n\n")

	for _, r := range sorted {
		heading := strings.TrimSpace(r.Title)
		if heading == "" {
			heading = "(untitled)"
		}
		// A heading cannot contain a newline, and a `#` at the start of a
		// continuation line would open a new section on re-import.
		heading = strings.ReplaceAll(heading, "\n", " ")
		fmt.Fprintf(&b, "## %s\n\n", heading)

		body := strings.TrimSpace(r.Body)
		if body != "" && body != strings.TrimSpace(r.Title) {
			// Demote any heading inside a body. An imported memory whose text
			// contains "## Steps" would otherwise split itself in two on the
			// next round trip.
			b.WriteString(demoteHeadings(body))
			b.WriteString("\n\n")
		}

		// One field per line rather than space-separated on one. A file path
		// may contain a space, and a single-line form would either lose the
		// tail of that path or need quoting rules that the importer would then
		// have to reimplement exactly. It is inside a comment either way, so
		// nobody reading the rendered document pays for the extra lines.
		var meta []string
		if r.Kind != "" {
			meta = append(meta, "kind="+r.Kind)
		}
		if r.Visibility != "" {
			meta = append(meta, "visibility="+r.Visibility)
		}
		if len(r.Files) > 0 {
			meta = append(meta, "files="+strings.Join(r.Files, ","))
		}
		if r.Source != "" {
			meta = append(meta, "source="+r.Source)
		}
		if r.ID != "" {
			meta = append(meta, "id="+r.ID)
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "<!-- dkm\n%s\n-->\n\n", strings.Join(meta, "\n"))
		}
	}

	return b.String()
}

// demoteHeadings pushes every ATX heading in a body down two levels, keeping it
// below the `##` that separates memories.
func demoteHeadings(body string) string {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "#") {
			lines[i] = "##" + line
		}
	}
	return strings.Join(lines, "\n")
}

// projectFilename turns a project identity into a safe file name.
//
// `github.com/devkuong/launcher` becomes
// `github.com__devkuong__launcher.md` — reversible by eye, and containing no
// separator that any of the three supported operating systems objects to.
func projectFilename(project string) string {
	if strings.TrimSpace(project) == "" {
		return "team-wide.md"
	}
	name := project
	for _, bad := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, bad, "__")
	}
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "project"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// writeMarkdownExport writes one file per project and returns what it wrote.
func writeMarkdownExport(dir string, byProject map[string][]mdRecord, now time.Time) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	written := make([]string, 0, len(projects))
	for _, p := range projects {
		path := filepath.Join(dir, projectFilename(p))
		doc := renderMarkdown(p, byProject[p], now)
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			return written, fmt.Errorf("writing %s: %w", path, err)
		}
		written = append(written, path)
	}

	// An index, so a directory of exports is navigable without opening each
	// file to find out which project it holds.
	var idx strings.Builder
	idx.WriteString("# Exported memories\n\n")
	fmt.Fprintf(&idx, "*%d project%s, exported %s by dkm.*\n\n",
		len(projects), plural(len(projects)), now.Format("2 January 2006"))
	for _, p := range projects {
		label := p
		if label == "" {
			label = "Team-wide (no project)"
		}
		fmt.Fprintf(&idx, "- [%s](%s) — %d memor%s\n",
			label, projectFilename(p), len(byProject[p]),
			map[bool]string{true: "y", false: "ies"}[len(byProject[p]) == 1])
	}
	idx.WriteString("\nRe-import any of these with:\n\n```bash\ndkm import markdown <file> --apply\n```\n")

	indexPath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(indexPath, []byte(idx.String()), 0o644); err != nil {
		return written, err
	}
	return append(written, indexPath), nil
}
