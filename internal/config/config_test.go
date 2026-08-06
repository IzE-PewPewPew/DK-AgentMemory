package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimal = `
server:
  bind: 127.0.0.1:8090
  public_url: https://memories.example.com
database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm?sslmode=disable
`

func TestMinimalConfigLoadsWithDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Search.RRFK != 60 {
		t.Errorf("search.rrf_k default: got %d, want 60", cfg.Search.RRFK)
	}
	if cfg.Embedding.Dimensions != 384 {
		t.Errorf("embedding.dimensions default: got %d, want 384", cfg.Embedding.Dimensions)
	}
	if !cfg.Security.RedactionEnabled {
		t.Error("redaction must default to on")
	}
	if cfg.Consolidation.FactSchedule() == nil {
		t.Error("cron expressions should be compiled during Load")
	}
}

// T1.4: an unknown key is fatal, and the error names the key.
func TestUnknownKeyIsFatalAndNamesTheKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"top level", minimal + "\nnonsense:\n  a: 1\n", "nonsense"},
		{"nested", minimal + "\nembedding:\n  dimensons: 384\n", "embedding.dimensons"},
		{"deeply nested", minimal + "\nconsolidation:\n  llm:\n    modell: x\n", "consolidation.llm.modell"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body), "test.yaml")
			if err == nil {
				t.Fatal("expected a fatal error, got none")
			}
			var uke *UnknownKeysError
			if !errors.As(err, &uke) {
				t.Fatalf("expected *UnknownKeysError, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the offending key %q:\n%s", tc.want, err)
			}
			if uke.Keys[0].Line == 0 {
				t.Error("error should carry the source line")
			}
		})
	}
}

func TestUnknownKeyOffersASuggestion(t *testing.T) {
	_, err := Parse([]byte(minimal+"\nembedding:\n  dimensons: 384\n"), "test.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "did you mean embedding.dimensions?") {
		t.Fatalf("expected a suggestion, got:\n%s", err)
	}
}

func TestAllUnknownKeysAreReportedAtOnce(t *testing.T) {
	// Fixing one typo, restarting, and hitting the next is the failure mode
	// this avoids.
	body := minimal + "\nfirst_typo: 1\nsecond_typo: 2\nthird_typo: 3\n"
	_, err := Parse([]byte(body), "test.yaml")
	var uke *UnknownKeysError
	if !errors.As(err, &uke) {
		t.Fatalf("expected *UnknownKeysError, got %v", err)
	}
	if len(uke.Keys) != 3 {
		t.Fatalf("expected 3 unknown keys, got %d: %v", len(uke.Keys), uke.Keys)
	}
}

// T1.4: a missing required key is fatal, and the error names the key.
func TestMissingRequiredKeyIsFatalAndNamesTheKey(t *testing.T) {
	cases := map[string]string{
		"server.bind": `
server:
  public_url: https://memories.example.com
database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm
`,
		"server.public_url": `
server:
  bind: 127.0.0.1:8090
database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm
`,
		"database.url": `
server:
  bind: 127.0.0.1:8090
  public_url: https://memories.example.com
`,
	}

	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			// server.bind has a default, so blank it explicitly to test the
			// required check rather than the default.
			if key == "server.bind" {
				body = strings.Replace(body, "server:\n", "server:\n  bind: \"\"\n", 1)
			}
			_, err := Parse([]byte(body), "test.yaml")
			if err == nil {
				t.Fatal("expected a fatal error, got none")
			}
			var mke *MissingKeyError
			if !errors.As(err, &mke) {
				t.Fatalf("expected *MissingKeyError, got %T: %v", err, err)
			}
			if mke.Key != key {
				t.Fatalf("error names %q, want %q", mke.Key, key)
			}
			if !strings.Contains(err.Error(), envName(EnvPrefix, key)) {
				t.Errorf("error should mention the environment override %s:\n%s", envName(EnvPrefix, key), err)
			}
		})
	}
}

// The exact failure that motivates strict parsing: a comment on the same line
// as a value. YAML folds it into the string, and a lenient loader would start
// with a bind address of "127.0.0.1:8090 # loopback".
func TestCommentSuffixedValueIsCaught(t *testing.T) {
	body := `
server:
  bind: "127.0.0.1:8090 # keep on loopback"
  public_url: https://memories.example.com
database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm
`
	cfg, err := Parse([]byte(body), "test.yaml")
	if err == nil {
		t.Fatalf("expected an error, got bind = %q", cfg.Server.Bind)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("DKM_SERVER_BIND", "0.0.0.0:9999")
	t.Setenv("DKM_SEARCH_RRF_K", "42")
	t.Setenv("DKM_CONSOLIDATION_LLM_MODEL", "claude-sonnet-5")
	t.Setenv("DKM_CONSOLIDATION_SESSION_SUMMARY_INTERVAL", "30m")
	t.Setenv("DKM_SECURITY_REQUIRE_HTTPS", "false")
	t.Setenv("DKM_INJECTION_INCLUDE", "lessons,decisions")

	cfg, err := Parse([]byte(minimal), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Bind != "0.0.0.0:9999" {
		t.Errorf("bind: got %q", cfg.Server.Bind)
	}
	if cfg.Search.RRFK != 42 {
		t.Errorf("rrf_k: got %d", cfg.Search.RRFK)
	}
	if cfg.Consolidation.LLM.Model != "claude-sonnet-5" {
		t.Errorf("llm.model: got %q", cfg.Consolidation.LLM.Model)
	}
	if cfg.Consolidation.SessionSummaryInterval.Duration() != 30*time.Minute {
		t.Errorf("interval: got %s", cfg.Consolidation.SessionSummaryInterval)
	}
	if cfg.Security.RequireHTTPS {
		t.Error("require_https should have been overridden to false")
	}
	if len(cfg.Injection.Include) != 2 {
		t.Errorf("include: got %v", cfg.Injection.Include)
	}
}

func TestEnvOverrideRejectsBadValue(t *testing.T) {
	t.Setenv("DKM_SEARCH_RRF_K", "sixty")
	_, err := Parse([]byte(minimal), "test.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "DKM_SEARCH_RRF_K") {
		t.Fatalf("error should name the variable:\n%v", err)
	}
}

func TestValueConstraints(t *testing.T) {
	cases := map[string]string{
		"embedding.provider":                 "\nembedding:\n  provider: pinecone\n",
		"search.dedup_threshold":             "\nsearch:\n  dedup_threshold: 1.5\n",
		"search.rrf_k":                       "\nsearch:\n  rrf_k: 0\n",
		"log.level":                          "\nlog:\n  level: verbose\n",
		"injection.include":                  "\ninjection:\n  include: [lessons, gossip]\n",
		"consolidation.llm.provider":         "\nconsolidation:\n  llm:\n    provider: mistral\n",
		"consolidation.fact_extraction_cron": "\nconsolidation:\n  fact_extraction_cron: \"0 2 * *\"\n",
	}

	for key, extra := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := Parse([]byte(minimal+extra), "test.yaml")
			if err == nil {
				t.Fatal("expected an error")
			}
			var ike *InvalidKeyError
			if !errors.As(err, &ike) {
				t.Fatalf("expected *InvalidKeyError, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error should name %q:\n%v", key, err)
			}
		})
	}
}

func TestPublicURLMustBeAbsolute(t *testing.T) {
	body := `
server:
  bind: 127.0.0.1:8090
  public_url: not-a-url
database:
  url: postgres://dkm:pw@127.0.0.1:5432/dkm
`
	_, err := Parse([]byte(body), "test.yaml")
	var ike *InvalidKeyError
	if !errors.As(err, &ike) || ike.Key != "server.public_url" {
		t.Fatalf("expected an InvalidKeyError for server.public_url, got %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := Parse([]byte(minimal+"\nconsolidation:\n  session_summary_interval: 45m\n"), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Consolidation.SessionSummaryInterval.Duration(); got != 45*time.Minute {
		t.Fatalf("got %s, want 45m", got)
	}
	if _, err := Parse([]byte(minimal+"\nconsolidation:\n  session_summary_interval: soon\n"), "test.yaml"); err == nil {
		t.Fatal("expected an error for a non-duration value")
	}
}

func TestClientValidate(t *testing.T) {
	c := ClientDefaults()
	c.Server = "https://memories.example.com/"
	c.Key = "pmk_a3f2_notarealkey000000"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Server != "https://memories.example.com" {
		t.Errorf("trailing slash should be trimmed, got %q", c.Server)
	}
	if got := c.KeyPrefix(); got != "pmk_a3f2" {
		t.Errorf("KeyPrefix: got %q, want pmk_a3f2", got)
	}

	c.Privacy.DefaultVisibility = "world"
	if err := c.Validate(); err == nil {
		t.Error("expected an error for an unknown visibility")
	}
}
