// Package config loads and validates the server and client configuration.
//
// Two rules govern everything here, and both exist because the alternative is a
// process that starts, reports healthy, and does something other than what the
// operator wrote down:
//
//   - An unknown key is fatal, and the error names the key and its line.
//   - A missing required key is fatal, and the error names the key.
//
// Validation happens once, at boot, before any port is bound or any connection
// is opened. Nothing downstream re-checks these values.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/cron"
)

// EnvPrefix is prepended to every environment override: server.bind is
// overridden by DKM_SERVER_BIND.
const EnvPrefix = "DKM"

// Config is the whole server configuration surface. Every field is documented
// in docs/CONFIGURATION.md; the two must not drift.
type Config struct {
	Server        Server        `yaml:"server"`
	Database      Database      `yaml:"database"`
	Embedding     Embedding     `yaml:"embedding"`
	Search        Search        `yaml:"search"`
	Consolidation Consolidation `yaml:"consolidation"`
	Injection     Injection     `yaml:"injection"`
	Security      Security      `yaml:"security"`
	Retention     Retention     `yaml:"retention"`
	Log           Log           `yaml:"log"`

	// path is where this config was read from, for error messages.
	path string `yaml:"-"`
}

type Server struct {
	Bind          string `yaml:"bind"`
	PublicURL     string `yaml:"public_url"`
	ViewerEnabled bool   `yaml:"viewer_enabled"`
	ViewerPath    string `yaml:"viewer_path"`
	ReadTimeout   Dur    `yaml:"read_timeout"`
	WriteTimeout  Dur    `yaml:"write_timeout"`
	ShutdownGrace Dur    `yaml:"shutdown_grace"`
}

type Database struct {
	URL            string `yaml:"url"`
	MaxConns       int    `yaml:"max_conns"`
	MigrateOnStart bool   `yaml:"migrate_on_start"`
}

type Embedding struct {
	Provider   string `yaml:"provider"`
	Endpoint   string `yaml:"endpoint"`
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
	APIKeyEnv  string `yaml:"api_key_env"`
	BatchSize  int    `yaml:"batch_size"`
	Timeout    Dur    `yaml:"timeout"`

	// QueryInstruction is prepended to search queries but never to stored
	// documents.
	//
	// BGE and E5 are asymmetric retrievers: they are trained with an
	// instruction on the query side only, and omitting it measurably degrades
	// ranking. The failure is quiet — search still returns results, they are
	// simply the wrong ones — which is far worse than an error, because the
	// system looks like it works.
	//
	// Empty disables it. Left unset, a sensible default is chosen from the
	// model name.
	QueryInstruction string `yaml:"query_instruction"`
}

// QueryPrefix returns the instruction to prepend to search queries.
//
// Defaulted from the model name rather than required, because getting this
// wrong is invisible and getting it right is mechanical.
func (e Embedding) QueryPrefix() string {
	if e.QueryInstruction != "" {
		if e.QueryInstruction == "none" {
			return ""
		}
		return e.QueryInstruction
	}
	model := strings.ToLower(e.Model)
	switch {
	case strings.Contains(model, "bge") && strings.Contains(model, "en"):
		return "Represent this sentence for searching relevant passages: "
	case strings.Contains(model, "bge"):
		return "为这个句子生成表示以用于检索相关文章："
	case strings.Contains(model, "e5"):
		return "query: "
	default:
		return ""
	}
}

type Search struct {
	DefaultLimit        int     `yaml:"default_limit"`
	RRFK                int     `yaml:"rrf_k"`
	CandidateLimit      int     `yaml:"candidate_limit"`
	RecencyHalfLifeDays float64 `yaml:"recency_half_life_days"`
	DedupThreshold      float64 `yaml:"dedup_threshold"`
}

type Consolidation struct {
	Enabled                bool   `yaml:"enabled"`
	SessionSummaryInterval Dur    `yaml:"session_summary_interval"`
	FactExtractionCron     string `yaml:"fact_extraction_cron"`
	LessonSynthesisCron    string `yaml:"lesson_synthesis_cron"`
	LLM                    LLM    `yaml:"llm"`

	factSchedule   *cron.Schedule `yaml:"-"`
	lessonSchedule *cron.Schedule `yaml:"-"`
}

// FactSchedule and LessonSchedule return the compiled cron expressions. They
// are parsed during Load, so a malformed expression stops the server at boot
// rather than never firing.
func (c Consolidation) FactSchedule() *cron.Schedule   { return c.factSchedule }
func (c Consolidation) LessonSchedule() *cron.Schedule { return c.lessonSchedule }

type LLM struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
	MaxTokens int    `yaml:"max_tokens"`
	Timeout   Dur    `yaml:"timeout"`
}

type Injection struct {
	Enabled      bool     `yaml:"enabled"`
	BudgetTokens int      `yaml:"budget_tokens"`
	Include      []string `yaml:"include"`
}

type Security struct {
	RequireHTTPS          bool `yaml:"require_https"`
	RateLimitWritesPerMin int  `yaml:"rate_limit_writes_per_min"`
	RateLimitReadsPerMin  int  `yaml:"rate_limit_reads_per_min"`
	RedactionEnabled      bool `yaml:"redaction_enabled"`
	AuditEnabled          bool `yaml:"audit_enabled"`
}

type Retention struct {
	ObservationDays   int  `yaml:"observation_days"`
	DecayEnabled      bool `yaml:"decay_enabled"`
	DecayHalfLifeDays int  `yaml:"decay_half_life_days"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Defaults returns the configuration the server runs with when a file specifies
// only the three required keys.
func Defaults() *Config {
	return &Config{
		Server: Server{
			Bind:          "127.0.0.1:8090",
			ViewerEnabled: true,
			ViewerPath:    "/viewer",
			ReadTimeout:   Dur(30 * time.Second),
			WriteTimeout:  Dur(60 * time.Second),
			ShutdownGrace: Dur(15 * time.Second),
		},
		Database: Database{
			MaxConns:       20,
			MigrateOnStart: false,
		},
		Embedding: Embedding{
			Provider:   "local",
			Endpoint:   "http://127.0.0.1:8091",
			Model:      "BAAI/bge-small-en-v1.5",
			Dimensions: 384,
			APIKeyEnv:  "DKM_EMBED_API_KEY",
			BatchSize:  32,
			Timeout:    Dur(20 * time.Second),
		},
		Search: Search{
			DefaultLimit:        8,
			RRFK:                60,
			CandidateLimit:      50,
			RecencyHalfLifeDays: 90,
			DedupThreshold:      0.92,
		},
		Consolidation: Consolidation{
			Enabled:                true,
			SessionSummaryInterval: Dur(15 * time.Minute),
			FactExtractionCron:     "0 2 * * *",
			LessonSynthesisCron:    "0 3 * * 0",
			LLM: LLM{
				Provider:  "anthropic",
				Model:     "claude-haiku-4-5",
				APIKeyEnv: "DKM_LLM_API_KEY",
				MaxTokens: 2000,
				Timeout:   Dur(2 * time.Minute),
			},
		},
		Injection: Injection{
			Enabled:      true,
			BudgetTokens: 1500,
			Include:      []string{"lessons", "decisions", "session_summaries"},
		},
		Security: Security{
			RequireHTTPS:          true,
			RateLimitWritesPerMin: 100,
			RateLimitReadsPerMin:  300,
			RedactionEnabled:      true,
			AuditEnabled:          true,
		},
		Retention: Retention{
			ObservationDays:   90,
			DecayEnabled:      true,
			DecayHalfLifeDays: 180,
		},
		Log: Log{Level: "info", Format: "json"},
	}
}

// Load reads, validates, and returns the server configuration.
//
// A missing file is not automatically an error. Container deployments
// configure entirely through DKM_* environment variables, and requiring a file
// there means baking a config into an image or mounting one that only repeats
// what the environment already says. So an absent file falls back to defaults
// plus the environment, and only fails if that combination is still incomplete
// -- in which case the error names the missing key and both ways to supply it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return Parse(raw, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	cfg, envErr := Parse(nil, path)
	if envErr == nil {
		return cfg, nil
	}
	return nil, fmt.Errorf(
		"no config file at %s, and the environment does not supply everything needed:\n\n%v\n\n"+
			"Either write a config file based on config.example.yaml and pass --config <path>,\n"+
			"or set DKM_SERVER_BIND, DKM_SERVER_PUBLIC_URL and DKM_DATABASE_URL.",
		path, envErr)
}

// Parse validates configuration already in memory. Load is the normal entry
// point; this exists so tests do not need a temporary file for every case.
func Parse(raw []byte, path string) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := Defaults()
	cfg.path = path

	// Unknown keys are checked before decoding. Decoding first would let a
	// valid-but-misspelled section silently apply defaults, which is exactly
	// the outcome this check exists to prevent.
	var unknown []UnknownKey
	checkUnknownKeys(&doc, reflect.TypeOf(Config{}), "", &unknown)
	if len(unknown) > 0 {
		return nil, &UnknownKeysError{File: path, Keys: unknown}
	}

	if len(doc.Content) > 0 {
		if err := doc.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	if err := applyEnv(reflect.ValueOf(cfg).Elem(), EnvPrefix, os.LookupEnv); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// MissingKeyError names a required key that was not set.
type MissingKeyError struct {
	Key  string
	Why  string
	File string
}

func (e *MissingKeyError) Error() string {
	msg := fmt.Sprintf("%s: required key %q is not set", e.File, e.Key)
	if e.Why != "" {
		msg += "\n  " + e.Why
	}
	msg += fmt.Sprintf("\n  Set it in the file, or export %s.", envName(EnvPrefix, e.Key))
	return msg
}

// InvalidKeyError names a key whose value is present but unusable.
type InvalidKeyError struct {
	Key  string
	Got  string
	Want string
	File string
}

func (e *InvalidKeyError) Error() string {
	return fmt.Sprintf("%s: key %q has value %q, but %s", e.File, e.Key, e.Got, e.Want)
}

// Validate enforces required keys and value constraints.
func (c *Config) Validate() error {
	file := c.path
	if file == "" {
		file = "config"
	}
	missing := func(key, why string) error { return &MissingKeyError{Key: key, Why: why, File: file} }
	invalid := func(key, got, want string) error {
		return &InvalidKeyError{Key: key, Got: got, Want: want, File: file}
	}

	// --- required ---------------------------------------------------------
	if c.Server.Bind == "" {
		return missing("server.bind", "the address the API listens on, e.g. 127.0.0.1:8090")
	}
	if c.Server.PublicURL == "" {
		return missing("server.public_url", "the URL clients reach this server on; used in the device-code login flow")
	}
	if c.Database.URL == "" {
		return missing("database.url", "the Postgres connection string, e.g. postgres://dkm:pw@127.0.0.1:5432/dkm?sslmode=disable")
	}

	// --- server -----------------------------------------------------------
	// SplitHostPort rather than a substring check. `bind: 127.0.0.1:8090 # loopback`
	// is valid YAML whose value silently includes the comment, and a check that
	// only looks for a colon would accept it and then fail at listen time with
	// an error naming a port that appears nowhere in the file.
	host, port, err := net.SplitHostPort(c.Server.Bind)
	if err != nil {
		return invalid("server.bind", c.Server.Bind, "it must be host:port, e.g. 127.0.0.1:8090")
	}
	if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
		return invalid("server.bind", c.Server.Bind, "the port must be a number between 1 and 65535 (quote the value if it contains a trailing comment)")
	}
	if strings.ContainsAny(host, " \t#") {
		return invalid("server.bind", c.Server.Bind, "the host contains whitespace or a '#' — a trailing YAML comment inside a quoted value becomes part of it")
	}
	pub, err := url.Parse(c.Server.PublicURL)
	if err != nil || pub.Scheme == "" || pub.Host == "" {
		return invalid("server.public_url", c.Server.PublicURL, "it must be an absolute URL, e.g. https://memories.example.com")
	}
	if pub.Scheme != "http" && pub.Scheme != "https" {
		return invalid("server.public_url", c.Server.PublicURL, "the scheme must be http or https")
	}
	if c.Server.ViewerEnabled && !strings.HasPrefix(c.Server.ViewerPath, "/") {
		return invalid("server.viewer_path", c.Server.ViewerPath, "it must be an absolute path beginning with /")
	}

	// --- database ---------------------------------------------------------
	if !strings.HasPrefix(c.Database.URL, "postgres://") && !strings.HasPrefix(c.Database.URL, "postgresql://") {
		return invalid("database.url", truncate(c.Database.URL), "it must be a postgres:// connection string")
	}
	if c.Database.MaxConns < 1 {
		return invalid("database.max_conns", strconv.Itoa(c.Database.MaxConns), "it must be at least 1")
	}

	// --- embedding --------------------------------------------------------
	switch c.Embedding.Provider {
	case "local", "ollama", "openai", "voyage", "none":
	default:
		return invalid("embedding.provider", c.Embedding.Provider, "it must be one of local, ollama, openai, voyage, none")
	}
	if c.Embedding.Provider != "none" {
		if c.Embedding.Dimensions < 1 {
			return invalid("embedding.dimensions", strconv.Itoa(c.Embedding.Dimensions), "it must be a positive integer matching the schema (default 384)")
		}
		if c.Embedding.BatchSize < 1 {
			return invalid("embedding.batch_size", strconv.Itoa(c.Embedding.BatchSize), "it must be at least 1")
		}
		if (c.Embedding.Provider == "local" || c.Embedding.Provider == "ollama") && c.Embedding.Endpoint == "" {
			return missing("embedding.endpoint", "the "+c.Embedding.Provider+" provider needs an endpoint, e.g. http://127.0.0.1:8091")
		}
		if c.Embedding.Provider == "openai" || c.Embedding.Provider == "voyage" {
			if c.Embedding.APIKeyEnv == "" {
				return missing("embedding.api_key_env", "hosted embedding providers need the name of an environment variable holding the key")
			}
			if os.Getenv(c.Embedding.APIKeyEnv) == "" {
				return invalid("embedding.api_key_env", c.Embedding.APIKeyEnv,
					"that environment variable is empty; export it before starting, or set embedding.provider to local")
			}
		}
	}

	// --- search -----------------------------------------------------------
	if c.Search.DefaultLimit < 1 {
		return invalid("search.default_limit", strconv.Itoa(c.Search.DefaultLimit), "it must be at least 1")
	}
	if c.Search.RRFK < 1 {
		return invalid("search.rrf_k", strconv.Itoa(c.Search.RRFK), "it must be at least 1; 60 is the value from the RRF paper")
	}
	if c.Search.CandidateLimit < c.Search.DefaultLimit {
		return invalid("search.candidate_limit", strconv.Itoa(c.Search.CandidateLimit),
			"it must be at least search.default_limit; it is how many rows each retriever contributes before fusion")
	}
	if c.Search.RecencyHalfLifeDays <= 0 {
		return invalid("search.recency_half_life_days", fmt.Sprint(c.Search.RecencyHalfLifeDays), "it must be greater than 0")
	}
	if c.Search.DedupThreshold <= 0 || c.Search.DedupThreshold > 1 {
		return invalid("search.dedup_threshold", fmt.Sprint(c.Search.DedupThreshold), "it is a cosine similarity and must be in (0, 1]")
	}

	// --- consolidation ----------------------------------------------------
	if c.Consolidation.Enabled {
		switch c.Consolidation.LLM.Provider {
		case "anthropic", "openai", "google", "openai-compatible":
		default:
			return invalid("consolidation.llm.provider", c.Consolidation.LLM.Provider,
				"it must be one of anthropic, openai, google, openai-compatible")
		}
		if c.Consolidation.LLM.Model == "" {
			return missing("consolidation.llm.model", "the model name to send to the provider")
		}
		if c.Consolidation.LLM.Provider == "openai-compatible" && c.Consolidation.LLM.BaseURL == "" {
			return missing("consolidation.llm.base_url", "the openai-compatible provider needs the endpoint to talk to")
		}
		// The key is deliberately NOT required here. Consolidation is on by
		// default, so demanding a credential at parse time would stop the
		// server from starting for everyone who only wants memory storage —
		// refusing to serve because an optional background job cannot run.
		// The worker reports the missing key instead, and stays disabled.
		if c.Consolidation.LLM.APIKeyEnv == "" {
			return missing("consolidation.llm.api_key_env",
				"the name of an environment variable holding the provider key; the key itself never goes in this file")
		}
		if c.Consolidation.LLM.MaxTokens < 1 {
			return invalid("consolidation.llm.max_tokens", strconv.Itoa(c.Consolidation.LLM.MaxTokens), "it must be at least 1")
		}
		if c.Consolidation.SessionSummaryInterval.Duration() < time.Minute {
			return invalid("consolidation.session_summary_interval", c.Consolidation.SessionSummaryInterval.String(),
				"it must be at least 1m; consolidation batches at session boundaries and a tighter loop costs money without improving recall")
		}
		fs, err := cron.Parse(c.Consolidation.FactExtractionCron)
		if err != nil {
			return invalid("consolidation.fact_extraction_cron", c.Consolidation.FactExtractionCron, err.Error())
		}
		ls, err := cron.Parse(c.Consolidation.LessonSynthesisCron)
		if err != nil {
			return invalid("consolidation.lesson_synthesis_cron", c.Consolidation.LessonSynthesisCron, err.Error())
		}
		c.Consolidation.factSchedule, c.Consolidation.lessonSchedule = fs, ls
	}

	// --- injection --------------------------------------------------------
	if c.Injection.Enabled {
		if c.Injection.BudgetTokens < 1 {
			return invalid("injection.budget_tokens", strconv.Itoa(c.Injection.BudgetTokens), "it must be at least 1")
		}
		for _, in := range c.Injection.Include {
			switch in {
			case "lessons", "decisions", "session_summaries", "facts", "preferences":
			default:
				return invalid("injection.include", in,
					"each entry must be one of lessons, decisions, facts, preferences, session_summaries")
			}
		}
	}

	// --- security ---------------------------------------------------------
	if c.Security.RateLimitWritesPerMin < 0 || c.Security.RateLimitReadsPerMin < 0 {
		return invalid("security.rate_limit_*_per_min", "negative", "limits must be 0 (unlimited) or positive")
	}
	if !c.Security.RedactionEnabled {
		// Permitted, because an air-gapped single-user install may genuinely
		// want it off, but never silently.
		fmt.Fprintln(os.Stderr, "dkm: WARNING security.redaction_enabled is false — credentials read during a session will be stored verbatim")
	}

	// --- retention --------------------------------------------------------
	if c.Retention.ObservationDays < 1 {
		return invalid("retention.observation_days", strconv.Itoa(c.Retention.ObservationDays), "it must be at least 1")
	}
	if c.Retention.DecayEnabled && c.Retention.DecayHalfLifeDays < 1 {
		return invalid("retention.decay_half_life_days", strconv.Itoa(c.Retention.DecayHalfLifeDays), "it must be at least 1 when decay is enabled")
	}

	// --- log --------------------------------------------------------------
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return invalid("log.level", c.Log.Level, "it must be one of debug, info, warn, error")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return invalid("log.format", c.Log.Format, "it must be json or text")
	}

	return nil
}

// EmbeddingAPIKey resolves the embedding provider credential from the
// environment. The key itself is never read from the config file.
func (c *Config) EmbeddingAPIKey() string {
	if c.Embedding.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Embedding.APIKeyEnv)
}

// LLMAPIKey resolves the consolidation LLM credential from the environment.
func (c *Config) LLMAPIKey() string {
	if c.Consolidation.LLM.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Consolidation.LLM.APIKeyEnv)
}

// PublicHTTPS reports whether public_url is https, which decides whether
// bearer tokens may cross the wire when require_https is on.
func (c *Config) PublicHTTPS() bool {
	return strings.HasPrefix(strings.ToLower(c.Server.PublicURL), "https://")
}

func truncate(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:24] + "..."
}

// --- environment overrides -------------------------------------------------

// applyEnv walks the config struct and overrides any field whose derived
// environment variable is set. server.bind becomes DKM_SERVER_BIND,
// consolidation.llm.model becomes DKM_CONSOLIDATION_LLM_MODEL.
func applyEnv(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" || name == "" {
			continue
		}
		env := prefix + "_" + strings.ToUpper(name)
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(Dur(0)) {
			if err := applyEnv(fv, env, lookup); err != nil {
				return err
			}
			continue
		}

		raw, ok := lookup(env)
		if !ok {
			continue
		}
		if err := setFromString(fv, raw); err != nil {
			return fmt.Errorf("%s: %w", env, err)
		}
	}
	return nil
}

func setFromString(fv reflect.Value, raw string) error {
	if fv.Type() == reflect.TypeOf(Dur(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%q is not a duration (e.g. 15m, 2h)", raw)
		}
		fv.Set(reflect.ValueOf(Dur(d)))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%q is not a boolean (true or false)", raw)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not an integer", raw)
		}
		fv.SetInt(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", raw)
		}
		fv.SetFloat(n)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("cannot be set from an environment variable")
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		fv.Set(reflect.ValueOf(out))
	default:
		return fmt.Errorf("cannot be set from an environment variable")
	}
	return nil
}

func envName(prefix, dotted string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(dotted, ".", "_"))
}

// --- duration --------------------------------------------------------------

// Dur is a time.Duration that reads YAML scalars like `15m` and `2h30m`.
type Dur time.Duration

// Duration returns the underlying duration.
func (d Dur) Duration() time.Duration { return time.Duration(d) }

func (d Dur) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts a duration string or a plain number of seconds.
func (d *Dur) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		var secs int64
		if err2 := n.Decode(&secs); err2 != nil {
			return fmt.Errorf("line %d: %q is not a duration (e.g. 15m, 2h)", n.Line, n.Value)
		}
		*d = Dur(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (e.g. 15m, 2h)", n.Line, s)
	}
	*d = Dur(parsed)
	return nil
}

// MarshalYAML writes durations back as human-readable strings.
func (d Dur) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// DefaultConfigPath returns the conventional server config location for the
// current OS, used when --config is not given.
func DefaultConfigPath() string {
	if p := os.Getenv("DKM_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	if os.PathSeparator == '\\' {
		return filepath.Join(os.Getenv("ProgramData"), "dkm", "config.yaml")
	}
	return "/etc/dkm/config.yaml"
}
