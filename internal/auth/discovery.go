package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// protectedResourceMetadata is RFC 9728 metadata.
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authServerMetadata is RFC 8414 / OIDC discovery metadata.
type authServerMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	RevocationEndpoint            string   `json:"revocation_endpoint"`
	ScopesSupported               []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// discovery is the result of resolving where and how to authorize.
type discovery struct {
	MCPURL   string
	Resource string
	PRM      *protectedResourceMetadata // nil if none found
	// AuthServer is the authorization server URL we chose (issuer identifier).
	AuthServer string
	ASM        *authServerMetadata
	// WWWScope is the scope from the WWW-Authenticate challenge, if any.
	WWWScope string
}

// issuerKey identifies the authorization server for the client cache.
func (d *discovery) issuerKey() string {
	if d.ASM != nil && d.ASM.Issuer != "" {
		return normalizeURL(d.ASM.Issuer)
	}
	return normalizeURL(d.AuthServer)
}

// bearerChallenge holds the parameters of a Bearer WWW-Authenticate challenge.
type bearerChallenge struct {
	ResourceMetadata string
	Scope            string
	Error            string
}

// parseWWWAuthenticate extracts the Bearer challenge parameters.
func parseWWWAuthenticate(h string) bearerChallenge {
	var out bearerChallenge
	params := parseChallenges(h)["bearer"]
	out.ResourceMetadata = params["resource_metadata"]
	out.Scope = params["scope"]
	out.Error = params["error"]
	return out
}

// parseChallenges parses a WWW-Authenticate header into scheme -> params.
// Scheme names are lower-cased.
func parseChallenges(h string) map[string]map[string]string {
	res := map[string]map[string]string{}
	var cur map[string]string
	i := 0
	n := len(h)
	skipSpace := func() {
		for i < n && (h[i] == ' ' || h[i] == '\t' || h[i] == ',') {
			i++
		}
	}
	readToken := func() string {
		start := i
		for i < n && h[i] != ' ' && h[i] != '\t' && h[i] != ',' && h[i] != '=' {
			i++
		}
		return h[start:i]
	}
	for {
		skipSpace()
		if i >= n {
			break
		}
		tok := readToken()
		// Look ahead: "tok=" is a parameter, otherwise a new scheme.
		j := i
		for j < n && (h[j] == ' ' || h[j] == '\t') {
			j++
		}
		if j < n && h[j] == '=' && cur != nil {
			i = j + 1
			for i < n && (h[i] == ' ' || h[i] == '\t') {
				i++
			}
			var val string
			if i < n && h[i] == '"' {
				i++
				var sb strings.Builder
				for i < n && h[i] != '"' {
					if h[i] == '\\' && i+1 < n {
						i++
					}
					sb.WriteByte(h[i])
					i++
				}
				i++ // closing quote
				val = sb.String()
			} else {
				start := i
				for i < n && h[i] != ',' && h[i] != ' ' && h[i] != '\t' {
					i++
				}
				val = h[start:i]
			}
			cur[strings.ToLower(tok)] = val
			continue
		}
		if tok == "" {
			i++
			continue
		}
		cur = map[string]string{}
		res[strings.ToLower(tok)] = cur
	}
	return res
}

// wellKnownURLs returns path-inserted then root well-known URLs for u.
func wellKnownURLs(u, suffix string, pathInsertOnly bool) []string {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" {
		return nil
	}
	origin := pu.Scheme + "://" + pu.Host
	p := strings.TrimRight(pu.Path, "/")
	var out []string
	if p != "" {
		out = append(out, origin+"/.well-known/"+suffix+p)
	}
	if !pathInsertOnly {
		out = append(out, origin+"/.well-known/"+suffix)
	}
	return out
}

func originOf(u string) string {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" {
		return ""
	}
	return pu.Scheme + "://" + pu.Host
}

func (m *Manager) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// probe sends an unauthenticated request to the MCP URL to obtain a
// WWW-Authenticate challenge.
func (m *Manager) probe(ctx context.Context) string {
	body := []byte(`{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.opts.Profile.URL, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		m.logf("info", "probe %s: %v", m.opts.Profile.URL, err)
		return ""
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	wa := resp.Header.Get("WWW-Authenticate")
	m.logf("info", "probe %s → HTTP %d (WWW-Authenticate: %q)", m.opts.Profile.URL, resp.StatusCode, wa)
	return wa
}

// discover resolves resource and authorization server metadata.
// wwwAuth is the most recent challenge ("" to probe).
func (m *Manager) discover(ctx context.Context, wwwAuth string) (*discovery, error) {
	p := m.opts.Profile
	d := &discovery{MCPURL: p.URL, Resource: p.URL}
	if wwwAuth == "" {
		wwwAuth = m.probe(ctx)
	}
	ch := parseWWWAuthenticate(wwwAuth)
	d.WWWScope = ch.Scope

	// 1. Protected resource metadata (RFC 9728).
	var candidates []string
	if ch.ResourceMetadata != "" {
		candidates = append(candidates, ch.ResourceMetadata)
	}
	candidates = append(candidates, wellKnownURLs(p.URL, "oauth-protected-resource", false)...)
	for _, u := range dedupe(candidates) {
		var prm protectedResourceMetadata
		if err := m.getJSON(ctx, u, &prm); err != nil {
			m.logf("info", "protected resource metadata %s: %v", u, err)
			continue
		}
		m.logf("info", "protected resource metadata found at %s (resource %s)", u, prm.Resource)
		d.PRM = &prm
		break
	}

	if d.PRM != nil {
		if d.PRM.Resource != "" {
			d.Resource = d.PRM.Resource
			if normalizeURL(d.Resource) != normalizeURL(p.URL) {
				m.logf("info", "resource %s differs from MCP URL %s (allowed)", d.Resource, p.URL)
			}
		}
		switch len(d.PRM.AuthorizationServers) {
		case 0:
		case 1:
			d.AuthServer = d.PRM.AuthorizationServers[0]
		default:
			d.AuthServer = d.PRM.AuthorizationServers[0]
			if p.AuthServer != "" {
				for _, as := range d.PRM.AuthorizationServers {
					if normalizeURL(as) == normalizeURL(p.AuthServer) {
						d.AuthServer = as
					}
				}
			}
			m.logf("info", "several authorization servers advertised %v; using %s (set auth_server in the profile to choose)", d.PRM.AuthorizationServers, d.AuthServer)
		}
	}
	if d.AuthServer == "" {
		if p.AuthServer != "" {
			d.AuthServer = p.AuthServer
			m.logf("info", "no authorization server advertised; using profile auth_server %s", d.AuthServer)
		} else {
			d.AuthServer = originOf(p.URL)
			m.logf("info", "no authorization server advertised; falling back to MCP origin %s", d.AuthServer)
		}
	}

	// 2. Authorization server metadata (RFC 8414, then OIDC).
	asURLs := wellKnownURLs(d.AuthServer, "oauth-authorization-server", false)
	asURLs = append(asURLs, wellKnownURLs(d.AuthServer, "openid-configuration", true)...)
	if pu, err := url.Parse(d.AuthServer); err == nil && strings.Trim(pu.Path, "/") != "" {
		asURLs = append(asURLs, normalizeURL(d.AuthServer)+"/.well-known/openid-configuration")
	}
	if o := originOf(d.AuthServer); o != "" {
		asURLs = append(asURLs, o+"/.well-known/openid-configuration")
	}
	for _, u := range dedupe(asURLs) {
		var asm authServerMetadata
		if err := m.getJSON(ctx, u, &asm); err != nil {
			m.logf("info", "authorization server metadata %s: %v", u, err)
			continue
		}
		if asm.AuthorizationEndpoint == "" || asm.TokenEndpoint == "" {
			m.logf("info", "authorization server metadata %s lacks endpoints; skipping", u)
			continue
		}
		if asm.Issuer != "" && normalizeURL(asm.Issuer) != normalizeURL(d.AuthServer) {
			m.logf("warn", "issuer %s does not match authorization server %s; continuing", asm.Issuer, d.AuthServer)
		}
		m.logf("info", "authorization server metadata found at %s", u)
		d.ASM = &asm
		break
	}
	if d.ASM == nil {
		o := originOf(d.AuthServer)
		if o == "" {
			return nil, fmt.Errorf("cannot determine authorization server for %s", p.URL)
		}
		m.logf("warn", "no authorization server metadata found; assuming default endpoints %s/authorize and %s/token", o, o)
		d.ASM = &authServerMetadata{
			Issuer:                d.AuthServer,
			AuthorizationEndpoint: o + "/authorize",
			TokenEndpoint:         o + "/token",
		}
	}
	if d.ASM.Issuer == "" {
		d.ASM.Issuer = d.AuthServer
	}
	return d, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
