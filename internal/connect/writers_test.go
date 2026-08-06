package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T2.4 acceptance: existing MCP servers are preserved, and re-running connect
// changes nothing.
func TestMergePreservesExistingServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	existing := `{
  "mcpServers": {
    "filesystem": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"] },
    "github":     { "command": "gh-mcp" }
  },
  "theme": "dark",
  "unrelatedSetting": { "nested": [1, 2, 3] }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := Agent{ID: "test", Name: "Test", ConfigCandidates: []string{path}, Format: FormatMCPServers}

	res, err := Connect(agent, "/usr/local/bin/dkm")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !res.Changed {
		t.Fatal("first connect should have changed the file")
	}
	if res.Backup == "" {
		t.Fatal("an existing file must be backed up before it is replaced")
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	root := readJSON(t, path)
	servers, _ := root["mcpServers"].(map[string]any)
	for _, name := range []string{"filesystem", "github", "dkm"} {
		if _, ok := servers[name]; !ok {
			t.Errorf("server %q missing after connect; merge deleted an existing entry", name)
		}
	}
	if root["theme"] != "dark" {
		t.Error("unrelated top-level key was lost")
	}
	if _, ok := root["unrelatedSetting"]; !ok {
		t.Error("unrelated nested key was lost")
	}

	// No credential in agent config. The key lives in ~/.dkm/config.yaml alone.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "pmk_") {
		t.Error("an API key was written into agent config")
	}
}

func TestConnectIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	agent := Agent{ID: "test", Name: "Test", ConfigCandidates: []string{path}, Format: FormatMCPServers}

	if _, err := Connect(agent, "/usr/local/bin/dkm"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Connect(agent, "/usr/local/bin/dkm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("second connect reported a change; it must be a no-op")
	}

	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("second connect produced different bytes:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestOpenCodeSchema(t *testing.T) {
	// OpenCode uses a different shape: a top-level `mcp` key and the command as
	// an array. Writing the common shape here produces a config that loads and
	// does nothing.
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	agent := Agent{ID: "opencode", Name: "OpenCode", ConfigCandidates: []string{path}, Format: FormatOpenCode}

	if _, err := Connect(agent, "dkm"); err != nil {
		t.Fatal(err)
	}

	root := readJSON(t, path)
	section, ok := root["mcp"].(map[string]any)
	if !ok {
		t.Fatal(`expected a top-level "mcp" key`)
	}
	entry, ok := section["dkm"].(map[string]any)
	if !ok {
		t.Fatal("dkm entry missing")
	}
	if entry["type"] != "local" {
		t.Errorf(`type: got %v, want "local"`, entry["type"])
	}
	cmd, ok := entry["command"].([]any)
	if !ok || len(cmd) != 2 {
		t.Fatalf("command should be a two-element array, got %v", entry["command"])
	}
}

func TestDisconnectLeavesOtherServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{ID: "test", Name: "Test", ConfigCandidates: []string{path}, Format: FormatMCPServers}

	if _, err := Connect(agent, "dkm"); err != nil {
		t.Fatal(err)
	}
	if !IsConnected(agent) {
		t.Fatal("IsConnected should be true after connect")
	}

	if _, err := Disconnect(agent); err != nil {
		t.Fatal(err)
	}
	if IsConnected(agent) {
		t.Fatal("IsConnected should be false after disconnect")
	}

	root := readJSON(t, path)
	servers, _ := root["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("disconnect removed an unrelated server")
	}
}

func TestMalformedConfigIsRefusedNotOverwritten(t *testing.T) {
	// A user's hand-edited config with a trailing comma must not be silently
	// replaced with a fresh one: that would delete every other MCP server they
	// have, and the error is what tells them to fix the comma.
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	broken := `{"mcpServers": {"a": {"command": "x"},}}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{ID: "test", Name: "Test", ConfigCandidates: []string{path}, Format: FormatMCPServers}

	if _, err := Connect(agent, "dkm"); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	after, _ := os.ReadFile(path)
	if string(after) != broken {
		t.Error("a malformed config was modified; it must be left untouched")
	}
}

func TestCodexTOMLMergeAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := "model = \"o3\"\n\n[mcp_servers.other]\ncommand = \"other-mcp\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{ID: "codex-cli", Name: "Codex CLI", ConfigCandidates: []string{path}, Format: FormatCodexTOML}

	if _, err := Connect(agent, "/usr/local/bin/dkm"); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	text := string(body)
	if !strings.Contains(text, "[mcp_servers.dkm]") {
		t.Error("dkm section missing")
	}
	if !strings.Contains(text, "[mcp_servers.other]") {
		t.Error("existing section was removed")
	}
	if !strings.Contains(text, `model = "o3"`) {
		t.Error("unrelated top-level key was removed")
	}

	first, _ := os.ReadFile(path)
	res, err := Connect(agent, "/usr/local/bin/dkm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("second connect should be a no-op")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Error("second connect changed the file")
	}
}

func TestHooksAreWrittenOnceAndReplacedOnReconnect(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	config := filepath.Join(dir, ".claude.json")

	agent := Agent{
		ID: "claude-code", Name: "Claude Code",
		ConfigCandidates: []string{config},
		HookPath:         settings,
		Format:           FormatMCPServers,
		SupportsHooks:    true,
	}

	if _, err := Connect(agent, "/usr/local/bin/dkm"); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(agent, "/usr/local/bin/dkm"); err != nil {
		t.Fatal(err)
	}

	root := readJSON(t, settings)
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatal("no hooks written")
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "SessionEnd"} {
		entries, ok := hooks[event].([]any)
		if !ok || len(entries) == 0 {
			t.Fatalf("hook %s missing", event)
		}
		// Reconnecting must not stack duplicate hooks; two hooks posting the
		// same observation would double every capture.
		if len(entries) != 1 {
			t.Errorf("hook %s has %d entries after two connects, want 1", event, len(entries))
		}
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", path, err, data)
	}
	return root
}
