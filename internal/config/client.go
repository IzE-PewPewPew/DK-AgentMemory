package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Client is the per-machine client configuration, written by `dkm login` and
// read by every other client verb including `dkm mcp`.
//
// This file is the only place the API key lives. Agent configs get a command to
// run, never a credential, so rotating a key means editing one file rather than
// hunting through ten agent config formats.
type Client struct {
	Server string `yaml:"server"`
	Key    string `yaml:"key"`

	User string `yaml:"user"`
	Team string `yaml:"team"`

	Sync    ClientSync    `yaml:"sync"`
	Project ClientProject `yaml:"project"`
	Privacy ClientPrivacy `yaml:"privacy"`

	path string `yaml:"-"`
}

type ClientSync struct {
	Enabled         bool   `yaml:"enabled"`
	MirrorPath      string `yaml:"mirror_path"`
	RefreshInterval Dur    `yaml:"refresh_interval"`
	QueueMax        int    `yaml:"queue_max"`
}

type ClientProject struct {
	// Strategy is git-remote | folder | explicit.
	Strategy string `yaml:"strategy"`
	// Explicit pins the project ID for this machine, overriding detection.
	Explicit string `yaml:"explicit"`
	// FallbackWarn prints a warning when identity falls back to a folder name,
	// which will not match a teammate's checkout.
	FallbackWarn bool `yaml:"fallback_warn"`
}

type ClientPrivacy struct {
	DefaultVisibility string `yaml:"default_visibility"`
	RedactLocal       bool   `yaml:"redact_local"`
}

// ClientDefaults returns the configuration a fresh `dkm login` writes.
func ClientDefaults() *Client {
	return &Client{
		Sync: ClientSync{
			Enabled:         true,
			MirrorPath:      filepath.Join(Home(), "mirror"),
			RefreshInterval: Dur(5 * 60 * 1e9), // 5m
			QueueMax:        1000,
		},
		Project: ClientProject{
			Strategy:     "git-remote",
			FallbackWarn: true,
		},
		Privacy: ClientPrivacy{
			DefaultVisibility: "private",
			RedactLocal:       true,
		},
	}
}

// Home is the client state directory: $DKM_HOME, or ~/.dkm.
func Home() string {
	if h := os.Getenv("DKM_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory is recoverable: fall back to the working directory
		// so `dkm` still runs, rather than refusing to start.
		return ".dkm"
	}
	return filepath.Join(home, ".dkm")
}

// ClientPath is the client config file location.
func ClientPath() string { return filepath.Join(Home(), "config.yaml") }

// ErrNotLoggedIn is returned when no client config exists yet.
var ErrNotLoggedIn = errors.New("not logged in")

// LoadClient reads ~/.dkm/config.yaml, applies DKM_SERVER / DKM_KEY /
// DKM_PROJECT overrides, and validates the result.
//
// Environment overrides are applied even when the file is missing, so a CI job
// can run with nothing but two environment variables set.
func LoadClient() (*Client, error) {
	path := ClientPath()
	cfg := ClientDefaults()
	cfg.path = path

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var unknown []UnknownKey
		checkUnknownKeys(&doc, reflect.TypeOf(Client{}), "", &unknown)
		if len(unknown) > 0 {
			return nil, &UnknownKeysError{File: path, Keys: unknown}
		}
		if len(doc.Content) > 0 {
			if err := doc.Decode(cfg); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// Fall through: the environment may still supply everything needed.
	default:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if v := os.Getenv("DKM_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("DKM_KEY"); v != "" {
		cfg.Key = v
	}
	if v := os.Getenv("DKM_PROJECT"); v != "" {
		cfg.Project.Strategy = "explicit"
		cfg.Project.Explicit = v
	}

	if cfg.Server == "" || cfg.Key == "" {
		return nil, fmt.Errorf("%w: run `dkm login <server-url>`, or set DKM_SERVER and DKM_KEY", ErrNotLoggedIn)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the client configuration.
func (c *Client) Validate() error {
	file := c.path
	if file == "" {
		file = ClientPath()
	}
	if !strings.HasPrefix(c.Server, "http://") && !strings.HasPrefix(c.Server, "https://") {
		return &InvalidKeyError{Key: "server", Got: c.Server, File: file,
			Want: "it must be an absolute URL, e.g. https://memories.example.com"}
	}
	c.Server = strings.TrimRight(c.Server, "/")

	switch c.Project.Strategy {
	case "git-remote", "folder", "explicit":
	default:
		return &InvalidKeyError{Key: "project.strategy", Got: c.Project.Strategy, File: file,
			Want: "it must be one of git-remote, folder, explicit"}
	}
	if c.Project.Strategy == "explicit" && c.Project.Explicit == "" {
		return &MissingKeyError{Key: "project.explicit", File: file,
			Why: "strategy is explicit, so the project ID must be given"}
	}

	switch c.Privacy.DefaultVisibility {
	case "private", "team":
	default:
		return &InvalidKeyError{Key: "privacy.default_visibility", Got: c.Privacy.DefaultVisibility, File: file,
			Want: "it must be private or team"}
	}

	if c.Sync.QueueMax < 1 {
		return &InvalidKeyError{Key: "sync.queue_max", Got: fmt.Sprint(c.Sync.QueueMax), File: file,
			Want: "it must be at least 1"}
	}
	if c.Sync.MirrorPath == "" {
		c.Sync.MirrorPath = filepath.Join(Home(), "mirror")
	}
	c.Sync.MirrorPath = ExpandHome(c.Sync.MirrorPath)
	return nil
}

// Path returns the file this config came from.
func (c *Client) Path() string {
	if c.path == "" {
		return ClientPath()
	}
	return c.path
}

// Save writes the client config with owner-only permissions.
//
// The file holds an API key, so the mode is set explicitly on every write
// rather than relying on the umask, and the write goes through a temporary file
// so an interrupted save cannot leave a truncated config behind.
func (c *Client) Save() error {
	path := c.Path()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	body, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# dkm client configuration. This file contains an API key.\n" +
		"# Written by `dkm login`. Rotate with `dkm login` again.\n"
	body = append([]byte(header), body...)

	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("securing %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("securing %s: %w", path, err)
		}
	}
	return nil
}

// KeyPrefix returns the public, non-secret portion of the API key, for display
// in `dkm doctor` and in logs. A key is pmk_<prefix>_<secret>; only the prefix
// is ever shown.
func (c *Client) KeyPrefix() string {
	parts := strings.SplitN(c.Key, "_", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[0] + "_" + parts[1]
}

// ExpandHome resolves a leading ~ in a path.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
