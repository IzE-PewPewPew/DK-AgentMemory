// Package connect detects installed AI coding tools and wires them to dkm.
//
// This is the product's first impression, and the bar it has to clear is that
// nobody edits JSON by hand. Ten hosts, four config schemas, three operating
// systems, and every one of them a place where a misplaced comma means "the
// agent has no memory tools" with no error anywhere.
//
// Two rules govern every writer here:
//
//   - Merge, never overwrite. Users have other MCP servers configured. A writer
//     that replaces the file is a writer that silently deletes someone's setup.
//   - No credentials in agent config. Agents get a command to run; the key
//     lives in ~/.dkm/config.yaml alone, so rotating it touches one file rather
//     than ten.
package connect

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Format is the config schema a host uses.
type Format int

const (
	// FormatMCPServers is the common shape: {"mcpServers": {"dkm": {...}}}
	FormatMCPServers Format = iota
	// FormatOpenCode is OpenCode's: {"mcp": {"dkm": {"type":"local","command":[...]}}}
	FormatOpenCode
	// FormatCodexTOML is Codex CLI's config.toml.
	FormatCodexTOML
	// FormatGemini is Gemini CLI's settings.json, same key as MCPServers but in
	// a file that carries unrelated settings.
	FormatGemini
)

// Agent describes one host.
type Agent struct {
	ID   string
	Name string

	// ConfigCandidates are checked in order. The first that exists is used; if
	// none exist, the first is created.
	ConfigCandidates []string

	// HookPath is where lifecycle hooks are written, when the host has them.
	HookPath string

	Format Format

	// SupportsHooks marks the two hosts that expose lifecycle events.
	//
	// Only Claude Code and Codex CLI do. Every other host is MCP-only, which
	// means it saves when the model decides to call a tool -- in practice, when
	// the user says "remember this", and often not otherwise. No configuration
	// changes that, and claiming otherwise for those hosts would be claiming
	// something the host does not expose.
	SupportsHooks bool

	// SupportsSkills marks hosts that read a SKILL.md.
	SupportsSkills bool
	SkillPath      string

	// ExtraDetect finds an installation that has no config file yet -- a tool
	// installed but never configured still counts as installed.
	ExtraDetect func() bool
}

// Status is the result of looking for one agent.
type Status struct {
	Agent      Agent
	Installed  bool
	ConfigPath string
	Connected  bool
	Detail     string
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func hpath(parts ...string) string {
	h := home()
	if h == "" {
		return ""
	}
	return filepath.Join(append([]string{h}, parts...)...)
}

func appData(parts ...string) string {
	base := os.Getenv("APPDATA")
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func anyOnPath(names ...string) func() bool {
	return func() bool {
		for _, n := range names {
			if onPath(n) {
				return true
			}
		}
		return false
	}
}

// claudeDesktopConfig returns the platform-specific config path.
//
// Three different conventions for the same application is exactly the kind of
// detail that makes people give up on manual setup.
func claudeDesktopConfig() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{hpath("Library", "Application Support", "Claude", "claude_desktop_config.json")}
	case "windows":
		return []string{appData("Claude", "claude_desktop_config.json")}
	default:
		return []string{
			hpath(".config", "Claude", "claude_desktop_config.json"),
			hpath(".config", "claude", "claude_desktop_config.json"),
		}
	}
}

func windsurfConfig() []string {
	if runtime.GOOS == "windows" {
		return []string{hpath(".codeium", "windsurf", "mcp_config.json")}
	}
	return []string{hpath(".codeium", "windsurf", "mcp_config.json")}
}

func clineConfig() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{hpath("Library", "Application Support", "Code", "User", "globalStorage",
			"saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")}
	case "windows":
		return []string{appData("Code", "User", "globalStorage",
			"saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")}
	default:
		return []string{hpath(".config", "Code", "User", "globalStorage",
			"saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")}
	}
}

// Agents is the registry. Order is the order `dkm connect --all` reports.
func Agents() []Agent {
	return []Agent{
		{
			ID:   "claude-code",
			Name: "Claude Code",
			// MCP servers live in ~/.claude.json; hooks live in
			// ~/.claude/settings.json. Two files, one tool -- writing both is
			// precisely the tedium this command exists to remove.
			ConfigCandidates: []string{hpath(".claude.json")},
			HookPath:         hpath(".claude", "settings.json"),
			Format:           FormatMCPServers,
			SupportsHooks:    true,
			SupportsSkills:   true,
			SkillPath:        hpath(".claude", "skills", "memory", "SKILL.md"),
			ExtraDetect: func() bool {
				return dirExists(hpath(".claude")) || onPath("claude")
			},
		},
		{
			ID:               "codex-cli",
			Name:             "Codex CLI",
			ConfigCandidates: []string{hpath(".codex", "config.toml")},
			HookPath:         hpath(".codex", "config.toml"),
			Format:           FormatCodexTOML,
			SupportsHooks:    true,
			ExtraDetect: func() bool {
				return dirExists(hpath(".codex")) || onPath("codex")
			},
		},
		{
			ID:               "claude-desktop",
			Name:             "Claude Desktop",
			ConfigCandidates: claudeDesktopConfig(),
			Format:           FormatMCPServers,
			ExtraDetect:      claudeDesktopInstalled,
		},
		{
			ID:   "opencode",
			Name: "OpenCode",
			ConfigCandidates: []string{
				hpath(".config", "opencode", "opencode.json"),
				hpath(".opencode.json"),
			},
			Format:      FormatOpenCode,
			ExtraDetect: anyOnPath("opencode"),
		},
		{
			ID:               "cursor",
			Name:             "Cursor",
			ConfigCandidates: []string{hpath(".cursor", "mcp.json")},
			Format:           FormatMCPServers,
			ExtraDetect: func() bool {
				return dirExists(hpath(".cursor")) || onPath("cursor")
			},
		},
		{
			ID:               "kimi-code",
			Name:             "Kimi Code",
			ConfigCandidates: []string{hpath(".kimi-code", "mcp.json")},
			Format:           FormatMCPServers,
			ExtraDetect: func() bool {
				return dirExists(hpath(".kimi-code")) || onPath("kimi-code")
			},
		},
		{
			ID:               "kimi-cli",
			Name:             "Kimi CLI",
			ConfigCandidates: []string{hpath(".kimi", "mcp.json")},
			Format:           FormatMCPServers,
			ExtraDetect: func() bool {
				return dirExists(hpath(".kimi")) || onPath("kimi")
			},
		},
		{
			ID:               "gemini-cli",
			Name:             "Gemini CLI",
			ConfigCandidates: []string{hpath(".gemini", "settings.json")},
			Format:           FormatGemini,
			ExtraDetect: func() bool {
				return dirExists(hpath(".gemini")) || onPath("gemini")
			},
		},
		{
			ID:               "windsurf",
			Name:             "Windsurf",
			ConfigCandidates: windsurfConfig(),
			Format:           FormatMCPServers,
			ExtraDetect: func() bool {
				return dirExists(hpath(".codeium"))
			},
		},
		{
			ID:               "cline",
			Name:             "Cline / Roo",
			ConfigCandidates: clineConfig(),
			Format:           FormatMCPServers,
			ExtraDetect: func() bool {
				return fileExists(clineConfig()[0])
			},
		},
	}
}

// ByID finds an agent in the registry.
func ByID(id string) (Agent, bool) {
	for _, a := range Agents() {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

func claudeDesktopInstalled() bool {
	switch runtime.GOOS {
	case "darwin":
		return dirExists("/Applications/Claude.app") ||
			dirExists(hpath("Applications", "Claude.app")) ||
			dirExists(hpath("Library", "Application Support", "Claude"))
	case "windows":
		return dirExists(appData("Claude")) ||
			dirExists(filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude")) ||
			dirExists(filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Claude"))
	default:
		return dirExists(hpath(".config", "Claude")) || onPath("claude-desktop")
	}
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// ConfigPath returns the config file to use: the first candidate that exists,
// or the first candidate overall when none do.
func (a Agent) ConfigPath() string {
	for _, p := range a.ConfigCandidates {
		if p != "" && fileExists(p) {
			return p
		}
	}
	for _, p := range a.ConfigCandidates {
		if p != "" {
			return p
		}
	}
	return ""
}

// Installed reports whether the host appears to be present.
//
// A config file counts, and so does a binary on PATH or an application
// directory: a tool installed but never configured is still installed, and
// telling the user it is absent sends them to reinstall something they have.
func (a Agent) Installed() bool {
	for _, p := range a.ConfigCandidates {
		if fileExists(p) {
			return true
		}
	}
	if a.HookPath != "" && fileExists(a.HookPath) {
		return true
	}
	if a.ExtraDetect != nil {
		return a.ExtraDetect()
	}
	return false
}

// Detect returns the status of every known agent.
func Detect() []Status {
	agents := Agents()
	out := make([]Status, 0, len(agents))
	for _, a := range agents {
		st := Status{Agent: a, ConfigPath: a.ConfigPath()}
		st.Installed = a.Installed()
		if st.Installed {
			st.Connected = IsConnected(a)
		}
		out = append(out, st)
	}
	return out
}
