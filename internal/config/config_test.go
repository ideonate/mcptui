package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolveDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`
default_profile = "local"

[profiles.local]
url = "http://127.0.0.1:8001/mcp"
env_badge = "dev"

[profiles.remote]
url = "https://mcp.example.com/mcp"
client_id = "abc123"
scope = "read write"
callback_port = 33418
env_badge = "prod"

[profiles.stdio]
command = ["python", "-m", "my_server"]
cwd = "/tmp"
env = { LOG_LEVEL = "debug" }

[profiles.bearer]
url = "https://x/mcp"
token = "env:MCPTUI_TEST_TOKEN"
headers = { "X-Api-Key" = "env:MCPTUI_TEST_KEY" }
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Resolve("")
	if err != nil || p.Name != "local" || p.Transport != "http" || p.Auth != AuthOAuth {
		t.Fatalf("default = %+v, %v", p, err)
	}
	r, _ := cfg.Resolve("remote")
	if r.ClientID != "abc123" || r.CallbackPort != 33418 || r.ConfirmMode() != ConfirmWrites {
		t.Fatalf("remote = %+v", r)
	}
	s, _ := cfg.Resolve("stdio")
	if s.Transport != "stdio" || s.Auth != AuthNone || s.Validate() != nil {
		t.Fatalf("stdio = %+v", s)
	}
	b, _ := cfg.Resolve("bearer")
	if b.Auth != AuthBearer {
		t.Fatalf("bearer auth = %q", b.Auth)
	}
	if _, err := b.ResolvedToken(); err == nil {
		t.Fatal("expected unset env error")
	}
	t.Setenv("MCPTUI_TEST_TOKEN", "tok")
	t.Setenv("MCPTUI_TEST_KEY", "key")
	if tok, _ := b.ResolvedToken(); tok != "tok" {
		t.Fatalf("token = %q", tok)
	}
	if h, _ := b.ResolvedHeaders(); h["X-Api-Key"] != "key" {
		t.Fatalf("headers = %v", h)
	}
	// Same URL reuses the saved profile.
	if u, _ := cfg.Resolve("https://mcp.example.com/mcp"); u.Name != "remote" || u.Temporary {
		t.Fatalf("url resolve = %+v", u)
	}
	if u, _ := cfg.Resolve("https://other/mcp"); !u.Temporary || u.Name != "other" {
		t.Fatalf("temp = %+v", u)
	}
	if _, err := cfg.Resolve("nope"); err == nil {
		t.Fatal("expected unknown profile error")
	}
}

func TestNeedsConfirm(t *testing.T) {
	cases := []struct {
		badge, confirm     string
		readOnly, destruct bool
		want               bool
	}{
		{"", "", false, false, false},
		{"", "", false, true, true},
		{"prod", "", true, false, false},
		{"prod", "", false, false, true},
		{"prod", "never", false, true, false},
		{"dev", "all", true, false, true},
		{"dev", "writes", false, false, true},
	}
	for _, c := range cases {
		p := &Profile{EnvBadge: c.badge, Confirm: c.confirm}
		if got := p.NeedsConfirm(c.readOnly, c.destruct); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	cfg, _ := Load(path)
	cfg.DefaultProfile = "x"
	cfg.Profiles["x"] = &Profile{URL: "https://x/mcp", Scope: "a b"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil || again.Profiles["x"].Scope != "a b" || again.Profiles["x"].Name != "x" {
		t.Fatalf("%+v %v", again, err)
	}
}
