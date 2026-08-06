package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/mcp"
)

// ServerName is the key dkm registers itself under in every host config.
const ServerName = "dkm"

// Result describes what a connect or disconnect did.
type Result struct {
	Agent      string
	ConfigPath string
	HookPath   string
	Changed    bool
	Backup     string
	Hooks      bool
	Skill      string
	Note       string
}

// Connect wires one agent to dkm.
//
// Idempotent: running it twice leaves the file byte-identical the second time
// and reports Changed=false. That property is what makes it safe to put in a
// setup script, and it is checked by comparing the rendered bytes rather than
// assumed from the code path.
func Connect(a Agent, binary string) (*Result, error) {
	res := &Result{Agent: a.Name, ConfigPath: a.ConfigPath()}
	if res.ConfigPath == "" {
		return nil, fmt.Errorf("no known config location for %s on this platform", a.Name)
	}

	var err error
	switch a.Format {
	case FormatCodexTOML:
		res.Changed, res.Backup, err = writeCodexTOML(res.ConfigPath, binary)
	case FormatOpenCode:
		res.Changed, res.Backup, err = mergeJSON(res.ConfigPath, func(root map[string]any) {
			mcpSection := childObject(root, "mcp")
			mcpSection[ServerName] = map[string]any{
				"type":    "local",
				"command": []any{binary, "mcp"},
				"enabled": true,
			}
		})
	default:
		res.Changed, res.Backup, err = mergeJSON(res.ConfigPath, func(root map[string]any) {
			servers := childObject(root, "mcpServers")
			servers[ServerName] = map[string]any{
				"command": binary,
				"args":    []any{"mcp"},
			}
		})
	}
	if err != nil {
		return nil, err
	}

	if a.SupportsHooks && a.Format != FormatCodexTOML {
		hookChanged, hookBackup, err := writeClaudeHooks(a.HookPath, binary)
		if err != nil {
			return nil, err
		}
		res.Hooks = true
		res.HookPath = a.HookPath
		res.Changed = res.Changed || hookChanged
		if res.Backup == "" {
			res.Backup = hookBackup
		}
	}

	if a.SupportsSkills && a.SkillPath != "" {
		if err := writeSkill(a.SkillPath); err == nil {
			res.Skill = a.SkillPath
		}
	}

	return res, nil
}

// Disconnect removes dkm from an agent's config, leaving everything else alone.
func Disconnect(a Agent) (*Result, error) {
	res := &Result{Agent: a.Name, ConfigPath: a.ConfigPath()}
	if res.ConfigPath == "" || !fileExists(res.ConfigPath) {
		return res, nil
	}

	var err error
	switch a.Format {
	case FormatCodexTOML:
		res.Changed, res.Backup, err = removeCodexTOML(res.ConfigPath)
	case FormatOpenCode:
		res.Changed, res.Backup, err = mergeJSON(res.ConfigPath, func(root map[string]any) {
			if section, ok := root["mcp"].(map[string]any); ok {
				delete(section, ServerName)
			}
		})
	default:
		res.Changed, res.Backup, err = mergeJSON(res.ConfigPath, func(root map[string]any) {
			if servers, ok := root["mcpServers"].(map[string]any); ok {
				delete(servers, ServerName)
			}
		})
	}
	if err != nil {
		return nil, err
	}

	if a.SupportsHooks && a.Format != FormatCodexTOML && fileExists(a.HookPath) {
		changed, _, err := mergeJSON(a.HookPath, func(root map[string]any) {
			hooks, ok := root["hooks"].(map[string]any)
			if !ok {
				return
			}
			for event := range hooks {
				entries, ok := hooks[event].([]any)
				if !ok {
					continue
				}
				kept := make([]any, 0, len(entries))
				for _, e := range entries {
					if !entryMentionsDKM(e) {
						kept = append(kept, e)
					}
				}
				if len(kept) == 0 {
					delete(hooks, event)
				} else {
					hooks[event] = kept
				}
			}
			if len(hooks) == 0 {
				delete(root, "hooks")
			}
		})
		if err != nil {
			return nil, err
		}
		res.Changed = res.Changed || changed
	}

	return res, nil
}

// IsConnected reports whether dkm appears in an agent's config.
func IsConnected(a Agent) bool {
	path := a.ConfigPath()
	if path == "" || !fileExists(path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	if a.Format == FormatCodexTOML {
		return strings.Contains(string(data), "[mcp_servers."+ServerName+"]")
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	key := "mcpServers"
	if a.Format == FormatOpenCode {
		key = "mcp"
	}
	servers, ok := root[key].(map[string]any)
	if !ok {
		return false
	}
	_, present := servers[ServerName]
	return present
}

// --- JSON merging ----------------------------------------------------------

// mergeJSON reads a JSON file, applies a mutation, and writes it back.
//
// The whole document is decoded into map[string]any and re-encoded, so keys the
// mutation does not touch survive verbatim. Formatting and key order do change
// on the first write -- which is why the original is backed up to .bak before
// anything is replaced.
func mergeJSON(path string, mutate func(map[string]any)) (changed bool, backup string, err error) {
	original, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, "", fmt.Errorf("reading %s: %w", path, readErr)
	}

	root := map[string]any{}
	if len(bytes.TrimSpace(original)) > 0 {
		if err := json.Unmarshal(original, &root); err != nil {
			return false, "", fmt.Errorf(
				"%s is not valid JSON, so it will not be modified: %w\n"+
					"  Fix the file or move it aside, then run `dkm connect` again", path, err)
		}
	}

	mutate(root)

	rendered, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, "", err
	}
	rendered = append(rendered, '\n')

	if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(rendered)) {
		return false, "", nil
	}

	if len(original) > 0 {
		backup = path + ".bak"
		if err := os.WriteFile(backup, original, 0o600); err != nil {
			return false, "", fmt.Errorf("backing up %s: %w", path, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, backup, err
	}
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		return false, backup, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, backup, nil
}

func childObject(root map[string]any, key string) map[string]any {
	if existing, ok := root[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	root[key] = created
	return created
}

// --- Claude Code hooks -----------------------------------------------------

// hookEvents are the four lifecycle points that make automatic capture work.
var hookEvents = []struct {
	Event   string
	Command string
	Comment string
}{
	{"SessionStart", "session-start", "inject project context"},
	{"UserPromptSubmit", "prompt", "retrieve against the prompt"},
	{"PostToolUse", "tool", "capture edits and commands"},
	{"SessionEnd", "session-end", "close the session for consolidation"},
}

func writeClaudeHooks(path, binary string) (bool, string, error) {
	if path == "" {
		return false, "", nil
	}
	return mergeJSON(path, func(root map[string]any) {
		hooks := childObject(root, "hooks")

		for _, h := range hookEvents {
			entries, _ := hooks[h.Event].([]any)

			// Drop any previous dkm entry before adding the current one, so an
			// upgrade that changes the command does not leave two hooks racing
			// each other.
			kept := make([]any, 0, len(entries)+1)
			for _, e := range entries {
				if !entryMentionsDKM(e) {
					kept = append(kept, e)
				}
			}

			kept = append(kept, map[string]any{
				"matcher": "",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": quoteIfNeeded(binary) + " hook " + h.Command,
					// Two seconds, and the hook itself exits 0 whatever
					// happens. A memory system that stalls or breaks the
					// user's editor is worse than no memory system, and it
					// only has to happen once for the tool to be uninstalled.
					"timeout": 2,
				}},
			})
			hooks[h.Event] = kept
		}
	})
}

func entryMentionsDKM(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, "dkm hook") {
			return true
		}
	}
	return false
}

func quoteIfNeeded(binary string) string {
	if strings.ContainsAny(binary, " \t") {
		return `"` + binary + `"`
	}
	return binary
}

// --- Codex TOML ------------------------------------------------------------

// codexHeader is the section header dkm owns in config.toml.
var codexHeader = "[mcp_servers." + ServerName + "]"

// splitCodexSection locates dkm's section by scanning lines.
//
// Not a regex. The obvious pattern -- header followed by everything up to the
// next '[' -- terminates early on `args = ["mcp"]`, because a bracket inside a
// value looks exactly like the start of the next section. A line-based scan
// asks the right question: where does the next section *header* begin.
func splitCodexSection(text string) (before, after string, found bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == codexHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return text, "", false
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}

	before = strings.Join(lines[:start], "\n")
	after = strings.Join(lines[end:], "\n")
	return before, after, true
}

// writeCodexTOML edits config.toml as text.
//
// Deliberately not a TOML round-trip. Re-encoding would discard comments and
// reorder a file the user maintains by hand; replacing exactly one section
// leaves everything else byte-identical, which also makes the idempotency check
// meaningful.
func writeCodexTOML(path, binary string) (bool, string, error) {
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, "", fmt.Errorf("reading %s: %w", path, err)
	}

	section := fmt.Sprintf(
		"[mcp_servers.%s]\ncommand = %q\nargs = [\"mcp\"]\nstartup_timeout_sec = 20\n",
		ServerName, binary)

	var updated string
	if before, after, found := splitCodexSection(string(original)); found {
		updated = strings.TrimRight(before, "\n")
		if updated != "" {
			updated += "\n\n"
		}
		updated += section
		if trimmed := strings.TrimLeft(after, "\n"); trimmed != "" {
			updated += "\n" + trimmed
		}
	} else if len(bytes.TrimSpace(original)) == 0 {
		updated = section
	} else {
		updated = strings.TrimRight(string(original), "\n") + "\n\n" + section
	}

	if strings.TrimSpace(updated) == strings.TrimSpace(string(original)) {
		return false, "", nil
	}

	var backup string
	if len(original) > 0 {
		backup = path + ".bak"
		if err := os.WriteFile(backup, original, 0o600); err != nil {
			return false, "", fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, backup, err
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return false, backup, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, backup, nil
}

func removeCodexTOML(path string) (bool, string, error) {
	original, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	before, after, found := splitCodexSection(string(original))
	if !found {
		return false, "", nil
	}

	updated := strings.TrimRight(before, "\n")
	if trimmed := strings.TrimLeft(after, "\n"); trimmed != "" {
		if updated != "" {
			updated += "\n\n"
		}
		updated += trimmed
	}
	updated = strings.TrimRight(updated, "\n") + "\n"

	backup := path + ".bak"
	if err := os.WriteFile(backup, original, 0o600); err != nil {
		return false, "", err
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return false, backup, err
	}
	return true, backup, nil
}

// --- skills ----------------------------------------------------------------

// writeSkill installs SKILL.md for hosts that read one.
//
// Twelve tools tell an agent what it can do; the skill tells it when. Without
// it, agents search rarely and save either nothing or everything.
func writeSkill(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == mcp.Skill {
		return nil
	}
	return os.WriteFile(path, []byte(mcp.Skill), 0o644)
}

// BinaryPath returns the absolute path of the running executable, for writing
// into agent configs.
//
// Absolute, because a GUI application does not inherit the shell's PATH. A
// config that says `dkm` works when tested from a terminal and fails silently
// when Claude Desktop launches from the dock -- and the symptom is zero tools
// with no error.
func BinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "dkm"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}
