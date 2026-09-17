package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/testserver"
)

func TestMain(m *testing.M) {
	if os.Getenv("MCPTUI_TEST_STDIO") == "serve" {
		_ = testserver.New(testserver.Options{}).ServeStdio(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, cfgPath string, args ...string) result {
	t.Helper()
	t.Setenv("MCPTUI_CONFIG_DIR", filepath.Dir(cfgPath))
	t.Setenv("MCPTUI_STATE_DIR", t.TempDir())
	var out, errb bytes.Buffer
	g := &globals{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, runTUI: func(*globals, *config.Profile, *config.Config) error {
		return errors.New("tui not available in tests")
	}}
	root := newRoot(g)
	root.SetArgs(append([]string{"--config", cfgPath}, args...))
	err := root.Execute()
	code := 0
	if err != nil {
		code = 1
		var ee *ExitError
		if errors.As(err, &ee) {
			code = ee.Code
			if ee.Err != nil {
				errb.WriteString(ee.Err.Error())
			}
		} else {
			errb.WriteString(err.Error())
		}
	}
	return result{code, out.String(), errb.String()}
}

func setup(t *testing.T) (cfgPath, url string) {
	t.Helper()
	hs := httptest.NewServer(testserver.New(testserver.Options{SSE: true, Sessions: true, PageSize: 2}))
	t.Cleanup(hs.Close)
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.toml")
	cfg := "default_profile = \"local\"\n[profiles.local]\nurl = \"" + hs.URL + "/mcp\"\nauth = \"none\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, hs.URL + "/mcp"
}

func TestCallPrintsJSONAndExitCodes(t *testing.T) {
	cfg, _ := setup(t)
	r := run(t, cfg, "call", "local", "add", `{"a":1,"b":2}`)
	if r.code != 0 {
		t.Fatalf("code %d: %s", r.code, r.stderr)
	}
	var res struct {
		StructuredContent map[string]float64 `json:"structuredContent"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res.StructuredContent["sum"] != 3 {
		t.Fatalf("stdout = %s (%v)", r.stdout, err)
	}

	r = run(t, cfg, "call", "local", "fail")
	if r.code != 1 || !strings.Contains(r.stdout, `"isError": true`) {
		t.Fatalf("fail: code %d stdout %s", r.code, r.stdout)
	}

	r = run(t, cfg, "call", "local", "echo", `{"message":"raw"}`, "--raw")
	if r.code != 0 || !strings.Contains(r.stdout, `"jsonrpc": "2.0"`) {
		t.Fatalf("raw: %+v", r)
	}

	argsFile := filepath.Join(t.TempDir(), "args.json")
	os.WriteFile(argsFile, []byte(`{"message":"from file"}`), 0o600)
	r = run(t, cfg, "call", "local", "echo", "@"+argsFile)
	if r.code != 0 || !strings.Contains(r.stdout, "from file") {
		t.Fatalf("@file: %+v", r)
	}

	r = run(t, cfg, "call", "local", "echo", `not json`)
	if r.code == 0 || !strings.Contains(r.stderr, "JSON object") {
		t.Fatalf("bad json: %+v", r)
	}
}

func TestTemporaryProfileURL(t *testing.T) {
	cfg, url := setup(t)
	r := run(t, cfg, "--profile-url", url, "call", "echo", `{"message":"temp"}`)
	if r.code != 0 || !strings.Contains(r.stdout, "temp") {
		t.Fatalf("%+v", r)
	}
}

func TestStdioProfile(t *testing.T) {
	cfg, _ := setup(t)
	exe, _ := os.Executable()
	t.Setenv("MCPTUI_TEST_STDIO", "serve")
	r := run(t, cfg, "--stdio", exe, "ls", "tools")
	if r.code != 0 || !strings.Contains(r.stdout, "delete_everything") {
		t.Fatalf("%+v", r)
	}
}

func TestLsPromptReadInfo(t *testing.T) {
	cfg, _ := setup(t)
	r := run(t, cfg, "ls", "local", "tools")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 5 || !strings.Contains(r.stdout, "Echo back") {
		t.Fatalf("ls tools (pagination): %+v", r)
	}
	r = run(t, cfg, "ls", "local", "templates", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"uriTemplate": "test://items/{id}"`) {
		t.Fatalf("ls templates: %+v", r)
	}
	r = run(t, cfg, "prompt", "local", "greet", "name=Ada")
	if r.code != 0 || !strings.Contains(r.stdout, "[assistant]\nHello, Ada!") {
		t.Fatalf("prompt: %+v", r)
	}
	r = run(t, cfg, "read", "local", "test://readme")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "# Readme") {
		t.Fatalf("read: %+v", r)
	}
	r = run(t, cfg, "read", "local", "test://blob")
	if r.code == 0 || !strings.Contains(r.stderr, "binary") {
		t.Fatalf("read blob without -o: %+v", r)
	}
	out := filepath.Join(t.TempDir(), "blob.bin")
	r = run(t, cfg, "read", "local", "test://blob", "-o", out)
	if b, _ := os.ReadFile(out); r.code != 0 || !bytes.Equal(b, []byte{0, 1, 2, 3}) {
		t.Fatalf("read blob: %+v %v", r, b)
	}
	r = run(t, cfg, "info", "local")
	if r.code != 0 || !strings.Contains(r.stdout, "testserver 1.2.3") || !strings.Contains(r.stdout, "Use **echo**") {
		t.Fatalf("info: %+v", r)
	}
	// Default profile.
	r = run(t, cfg, "info", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"protocolVersion"`) {
		t.Fatalf("info default: %+v", r)
	}
}

func TestProfilesAddAndList(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	r := run(t, cfg, "profiles", "add", "remote", "https://mcp.example.com/mcp", "--env", "prod", "--scope", "read write")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	r = run(t, cfg, "profiles", "add", "local", "--", "python", "-m", "server")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	r = run(t, cfg, "profiles")
	if !strings.Contains(r.stdout, "* remote [prod]  https://mcp.example.com/mcp  (http, auth oauth)") ||
		!strings.Contains(r.stdout, "local  python -m server  (stdio, auth none)") {
		t.Fatalf("profiles:\n%s", r.stdout)
	}
	r = run(t, cfg, "profiles", "add", "remote", "https://x")
	if r.code == 0 {
		t.Fatal("duplicate profile should fail")
	}
	st, _ := os.Stat(cfg)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", st.Mode())
	}
}

func TestNonInteractiveLoginRequired(t *testing.T) {
	srv := testserver.New(testserver.Options{RequireToken: "secret"})
	hs := httptest.NewServer(srv)
	defer hs.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte("[profiles.p]\nurl = \""+hs.URL+"/mcp\"\n"), 0o600)
	r := run(t, cfg, "call", "p", "echo", `{"message":"x"}`)
	if r.code == 0 || !strings.Contains(r.stderr, "mcptui auth login p") {
		t.Fatalf("expected login hint, got %+v", r)
	}

	// Bearer auth from the environment works.
	t.Setenv("MY_TOKEN", "secret")
	os.WriteFile(cfg, []byte("[profiles.p]\nurl = \""+hs.URL+"/mcp\"\nauth = \"bearer\"\ntoken = \"env:MY_TOKEN\"\n"), 0o600)
	r = run(t, cfg, "call", "p", "echo", `{"message":"bearer ok"}`)
	if r.code != 0 || !strings.Contains(r.stdout, "bearer ok") {
		t.Fatalf("bearer: %+v", r)
	}
}

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`npx -y "@scope/pkg name" 'a b' c\ d`)
	want := []string{"npx", "-y", "@scope/pkg name", "a b", "c d"}
	if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := splitCommand(`"unterminated`); err == nil {
		t.Fatal("expected error")
	}
}
