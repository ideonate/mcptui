package copycmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ideonate/mcptui/internal/config"
)

func TestCurl(t *testing.T) {
	p := &config.Profile{Name: "prod", Transport: "http", URL: "https://mcp.example.com/mcp", Auth: config.AuthOAuth,
		Headers: map[string]string{"X-Key": "env:KEY"}}
	got, err := Curl(p, json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"it's"}}`), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"curl -sS -X POST https://mcp.example.com/mcp",
		"Accept: application/json, text/event-stream",
		`-H "Authorization: Bearer $(mcptui auth token prod)"`,
		`-H "X-Key: $KEY"`,
		"Mcp-Session-Id: sess-1",
		`"name":"it'"'"'s"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if _, err := Curl(&config.Profile{Transport: "stdio"}, nil, ""); err == nil {
		t.Fatal("expected error for stdio")
	}
}

func TestMcptui(t *testing.T) {
	p := &config.Profile{Name: "local", Transport: "http"}
	if got := Mcptui(p, "tool", "add", json.RawMessage(`{"a": 1}`)); got != `mcptui call local add '{"a":1}'` {
		t.Errorf("tool: %s", got)
	}
	if got := Mcptui(p, "prompt", "greet", json.RawMessage(`{"name":"Ada Lovelace"}`)); got != `mcptui prompt local greet 'name=Ada Lovelace'` {
		t.Errorf("prompt: %s", got)
	}
	tmp := config.TemporaryCommand([]string{"npx", "-y", "server"})
	if got := Mcptui(tmp, "resource", "test://x", nil); got != `mcptui read --stdio 'npx -y server' test://x` {
		t.Errorf("stdio: %s", got)
	}
	u := config.TemporaryURL("https://h/mcp")
	if got := Mcptui(u, "tool", "ping", nil); got != `mcptui call --profile-url https://h/mcp ping` {
		t.Errorf("url: %s", got)
	}
}
