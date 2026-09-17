// Package config loads and saves mcptui profiles from config.toml.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/ideonate/mcptui/internal/paths"
)

// Auth modes.
const (
	AuthOAuth  = "oauth"
	AuthBearer = "bearer"
	AuthNone   = "none"
)

// Confirm modes.
const (
	ConfirmNever  = "never"
	ConfirmWrites = "writes"
	ConfirmAll    = "all"
)

// Profile is one server connection.
type Profile struct {
	Name string `toml:"-"`
	// Temporary profiles come from the command line and are not saved.
	Temporary bool `toml:"-"`

	Transport string            `toml:"transport,omitempty"` // "http" (default when url set) | "stdio"
	URL       string            `toml:"url,omitempty"`
	Command   []string          `toml:"command,omitempty"`
	Cwd       string            `toml:"cwd,omitempty"`
	Env       map[string]string `toml:"env,omitempty"`

	Auth    string            `toml:"auth,omitempty"` // oauth (default for http) | bearer | none
	Token   string            `toml:"token,omitempty"`
	Headers map[string]string `toml:"headers,omitempty"`

	ClientID     string `toml:"client_id,omitempty"`
	ClientSecret string `toml:"client_secret,omitempty"`
	Scope        string `toml:"scope,omitempty"`
	AuthServer   string `toml:"auth_server,omitempty"`
	CallbackPort int    `toml:"callback_port,omitempty"`

	EnvBadge string `toml:"env_badge,omitempty"` // dev | staging | prod
	Confirm  string `toml:"confirm,omitempty"`   // never | writes | all
	Timeout  string `toml:"timeout,omitempty"`   // Go duration; empty = none
}

// Config is the whole config file.
type Config struct {
	DefaultProfile string              `toml:"default_profile,omitempty"`
	History        *bool               `toml:"history,omitempty"` // persist history (default true)
	Profiles       map[string]*Profile `toml:"profiles,omitempty"`

	path string
}

// Load reads the config file at path (or the default). A missing file yields
// an empty config.
func Load(path string) (*Config, error) {
	if path == "" {
		path = paths.ConfigFile()
	}
	cfg := &Config{path: path, Profiles: map[string]*Profile{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	for name, p := range cfg.Profiles {
		if p == nil {
			p = &Profile{}
			cfg.Profiles[name] = p
		}
		p.Name = name
	}
	return cfg, nil
}

// Path is the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// PersistHistory reports whether history.jsonl should be written.
func (c *Config) PersistHistory() bool { return c.History == nil || *c.History }

// Save writes the config atomically.
func (c *Config) Save() error {
	if err := paths.EnsureDir(filepath.Dir(c.path)); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(c.path, b, 0o600)
}

// ProfileNames returns sorted profile names.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolve turns a CLI argument into a profile: a saved profile name, a URL
// (temporary profile), or "" for the default profile.
func (c *Config) Resolve(arg string) (*Profile, error) {
	if arg == "" {
		arg = c.DefaultProfile
		if arg == "" {
			if len(c.Profiles) == 1 {
				for _, p := range c.Profiles {
					return p.withDefaults(), nil
				}
			}
			return nil, fmt.Errorf("no profile given and no default_profile set in %s", c.path)
		}
	}
	if p, ok := c.Profiles[arg]; ok {
		return p.withDefaults(), nil
	}
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		// Reuse a saved profile with the same URL if there is one.
		for _, name := range c.ProfileNames() {
			if p := c.Profiles[name]; p.URL == arg {
				return p.withDefaults(), nil
			}
		}
		return TemporaryURL(arg), nil
	}
	return nil, fmt.Errorf("unknown profile %q (see `mcptui profiles`)", arg)
}

// TemporaryURL builds an unsaved HTTP profile.
func TemporaryURL(raw string) *Profile {
	name := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		name = u.Host
	}
	return (&Profile{Name: name, URL: raw, Temporary: true}).withDefaults()
}

// TemporaryCommand builds an unsaved stdio profile.
func TemporaryCommand(argv []string) *Profile {
	name := "stdio"
	if len(argv) > 0 {
		name = filepath.Base(argv[0])
	}
	return (&Profile{Name: name, Transport: "stdio", Command: argv, Temporary: true}).withDefaults()
}

func (p *Profile) withDefaults() *Profile {
	cp := *p
	if cp.Transport == "" {
		if len(cp.Command) > 0 && cp.URL == "" {
			cp.Transport = "stdio"
		} else {
			cp.Transport = "http"
		}
	}
	if cp.Auth == "" {
		switch {
		case cp.Transport == "stdio":
			cp.Auth = AuthNone
		case cp.Token != "":
			cp.Auth = AuthBearer
		default:
			cp.Auth = AuthOAuth
		}
	}
	return &cp
}

// Validate checks the profile is usable.
func (p *Profile) Validate() error {
	switch p.Transport {
	case "http":
		if p.URL == "" {
			return fmt.Errorf("profile %q: url is required", p.Name)
		}
		if _, err := url.Parse(p.URL); err != nil {
			return fmt.Errorf("profile %q: bad url: %w", p.Name, err)
		}
	case "stdio":
		if len(p.Command) == 0 {
			return fmt.Errorf("profile %q: command is required for stdio", p.Name)
		}
	default:
		return fmt.Errorf("profile %q: unknown transport %q", p.Name, p.Transport)
	}
	switch p.Auth {
	case AuthOAuth, AuthNone:
	case AuthBearer:
		if p.Token == "" {
			return fmt.Errorf("profile %q: auth = \"bearer\" needs token", p.Name)
		}
	default:
		return fmt.Errorf("profile %q: unknown auth %q", p.Name, p.Auth)
	}
	switch p.Confirm {
	case "", ConfirmNever, ConfirmWrites, ConfirmAll:
	default:
		return fmt.Errorf("profile %q: confirm must be never, writes or all", p.Name)
	}
	if _, err := p.TimeoutDuration(); err != nil {
		return err
	}
	return nil
}

// TimeoutDuration parses Timeout; zero means no timeout.
func (p *Profile) TimeoutDuration() (time.Duration, error) {
	if p.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil {
		return 0, fmt.Errorf("profile %q: bad timeout: %w", p.Name, err)
	}
	return d, nil
}

// ResolvedToken expands env: in Token.
func (p *Profile) ResolvedToken() (string, error) { return ExpandValue(p.Token) }

// ResolvedHeaders expands env: in header values.
func (p *Profile) ResolvedHeaders() (map[string]string, error) {
	out := make(map[string]string, len(p.Headers))
	for k, v := range p.Headers {
		ev, err := ExpandValue(v)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		out[k] = ev
	}
	return out, nil
}

// ResolvedEnv expands env: in stdio environment values.
func (p *Profile) ResolvedEnv() (map[string]string, error) {
	out := make(map[string]string, len(p.Env))
	for k, v := range p.Env {
		ev, err := ExpandValue(v)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = ev
	}
	return out, nil
}

// ExpandValue resolves "env:VAR" references.
func ExpandValue(v string) (string, error) {
	if name, ok := strings.CutPrefix(v, "env:"); ok {
		val, set := os.LookupEnv(name)
		if !set {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return val, nil
	}
	return v, nil
}

// ConfirmMode returns the effective confirmation policy.
func (p *Profile) ConfirmMode() string {
	if p.Confirm != "" {
		return p.Confirm
	}
	if p.EnvBadge == "prod" {
		return ConfirmWrites
	}
	return ""
}

// NeedsConfirm decides whether calling a tool needs confirmation.
// destructive and readOnly are the tool's annotations.
func (p *Profile) NeedsConfirm(readOnly, destructive bool) bool {
	switch p.ConfirmMode() {
	case ConfirmNever:
		return false
	case ConfirmAll:
		return true
	case ConfirmWrites:
		return !readOnly
	default:
		return destructive
	}
}

// Target describes where the profile connects, for display.
func (p *Profile) Target() string {
	if p.Transport == "stdio" {
		return strings.Join(p.Command, " ")
	}
	return p.URL
}

// WriteFileAtomic writes via a temp file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
