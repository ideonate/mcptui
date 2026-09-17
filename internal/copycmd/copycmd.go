// Package copycmd renders requests as shell commands (curl or mcptui).
package copycmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ideonate/mcptui/internal/config"
)

// ShellQuote single-quotes s for POSIX shells.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@=,+%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// profileArg is how to refer to the profile on the command line.
func profileArg(p *config.Profile) string {
	if p.Temporary && p.Transport == "http" {
		return "--profile-url " + ShellQuote(p.URL)
	}
	return ShellQuote(p.Name)
}

// Curl renders a JSON-RPC request body as a curl command. Streamable HTTP
// servers that use sessions will additionally need initialize first, which
// is noted in a comment.
func Curl(p *config.Profile, request json.RawMessage, sessionID string) (string, error) {
	if p.Transport != "http" {
		return "", fmt.Errorf("curl is only available for HTTP profiles")
	}
	var compact strings.Builder
	var v any
	if err := json.Unmarshal(request, &v); err != nil {
		return "", err
	}
	b, _ := json.Marshal(v)
	compact.Write(b)

	parts := []string{"curl -sS -X POST " + ShellQuote(p.URL)}
	parts = append(parts, "-H 'Content-Type: application/json'", "-H 'Accept: application/json, text/event-stream'")
	switch p.Auth {
	case config.AuthOAuth:
		parts = append(parts, fmt.Sprintf(`-H "Authorization: Bearer $(mcptui auth token %s)"`, profileArg(p)))
	case config.AuthBearer:
		if name, ok := strings.CutPrefix(p.Token, "env:"); ok {
			parts = append(parts, fmt.Sprintf(`-H "Authorization: Bearer $%s"`, name))
		} else {
			parts = append(parts, `-H "Authorization: Bearer $MCP_TOKEN"`)
		}
	}
	keys := make([]string, 0, len(p.Headers))
	for k := range p.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := p.Headers[k]
		if name, ok := strings.CutPrefix(v, "env:"); ok {
			parts = append(parts, fmt.Sprintf(`-H "%s: $%s"`, k, name))
		} else {
			parts = append(parts, "-H "+ShellQuote(k+": "+v))
		}
	}
	if sessionID != "" {
		parts = append(parts, "-H "+ShellQuote("Mcp-Session-Id: "+sessionID))
	}
	parts = append(parts, "--data "+ShellQuote(compact.String()))
	return strings.Join(parts, " \\\n  "), nil
}

// Mcptui renders the equivalent non-interactive mcptui invocation.
// kind is tool | prompt | resource | list.
func Mcptui(p *config.Profile, kind, name string, args json.RawMessage) string {
	prof := profileArg(p)
	if p.Temporary && p.Transport == "stdio" {
		prof = "--stdio " + ShellQuote(joinQuoted(p.Command))
	}
	switch kind {
	case "tool":
		cmd := fmt.Sprintf("mcptui call %s %s", prof, ShellQuote(name))
		if len(args) > 0 && string(args) != "{}" && string(args) != "null" {
			cmd += " " + ShellQuote(compactJSON(args))
		}
		return cmd
	case "prompt":
		cmd := fmt.Sprintf("mcptui prompt %s %s", prof, ShellQuote(name))
		var m map[string]any
		_ = json.Unmarshal(args, &m)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cmd += " " + ShellQuote(fmt.Sprintf("%s=%v", k, m[k]))
		}
		return cmd
	case "resource":
		return fmt.Sprintf("mcptui read %s %s", prof, ShellQuote(name))
	default:
		return fmt.Sprintf("mcptui ls %s %s", prof, ShellQuote(name))
	}
}

func joinQuoted(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = ShellQuote(a)
	}
	return strings.Join(q, " ")
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
