package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnknownKey is one key present in a config file that the struct has no field
// for.
type UnknownKey struct {
	Path       string // dotted path, e.g. "embedding.dimensons"
	Line       int    // 1-based line in the source file
	Suggestion string // nearest known key, if one is close enough
}

// UnknownKeysError reports every unrecognised key at once.
//
// Reporting all of them matters: fixing a typo, restarting, and hitting the
// next typo is how a ten-minute deploy becomes an hour. The server refuses to
// start on any of these, because the failure this prevents is subtle. A comment
// on the same line as a value, a key at the wrong indentation, a setting
// renamed between versions -- each produces a server that starts, looks
// healthy, and quietly ignores what the operator asked for.
type UnknownKeysError struct {
	File string
	Keys []UnknownKey
}

func (e *UnknownKeysError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d unknown key", e.File, len(e.Keys))
	if len(e.Keys) != 1 {
		b.WriteString("s")
	}
	for _, k := range e.Keys {
		fmt.Fprintf(&b, "\n  line %d: %s", k.Line, k.Path)
		if k.Suggestion != "" {
			fmt.Fprintf(&b, "  (did you mean %s?)", k.Suggestion)
		}
	}
	b.WriteString("\n\nRefusing to start: an unrecognised key means the setting you intended is not being applied.")
	return b.String()
}

// checkUnknownKeys walks a decoded YAML tree against the target struct type and
// records every key with no corresponding field.
func checkUnknownKeys(node *yaml.Node, typ reflect.Type, path string, out *[]UnknownKey) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) > 0 {
			checkUnknownKeys(node.Content[0], typ, path, out)
		}
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	switch typ.Kind() {
	case reflect.Map:
		// A map accepts arbitrary keys by definition; nothing to check.
		return
	case reflect.Struct:
	default:
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}

	fields := yamlFields(typ)
	known := make([]string, 0, len(fields))
	for name := range fields {
		known = append(known, name)
	}
	sort.Strings(known)

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		key := keyNode.Value

		field, ok := fields[key]
		if !ok {
			full := key
			if path != "" {
				full = path + "." + key
			}
			*out = append(*out, UnknownKey{
				Path:       full,
				Line:       keyNode.Line,
				Suggestion: suggest(key, known, path),
			})
			continue
		}

		child := path
		if child == "" {
			child = key
		} else {
			child += "." + key
		}
		checkUnknownKeys(valNode, field.Type, child, out)
	}
}

// yamlFields maps yaml tag names to struct fields, including fields promoted
// from embedded structs.
func yamlFields(typ reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && (name == "" || strings.Contains(opts, "inline")) {
			t := f.Type
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t.Kind() == reflect.Struct {
				for k, v := range yamlFields(t) {
					out[k] = v
				}
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

// suggest returns the closest known key, or "" when nothing is close enough to
// be worth guessing. A wrong suggestion is worse than none.
func suggest(got string, known []string, path string) string {
	best, bestDist := "", 1<<30
	for _, k := range known {
		d := levenshtein(strings.ToLower(got), strings.ToLower(k))
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	// Allow roughly one edit per four characters, with a floor of one, so
	// "bnd" -> "bind" is offered but "colour" -> "bind" is not.
	limit := len(best)/4 + 1
	if bestDist > limit {
		return ""
	}
	if path != "" {
		return path + "." + best
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

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
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
