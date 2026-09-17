package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/mcp"
)

// mockServer is an in-process MCP resource server plus OAuth authorization
// server with configurable discovery layouts.
type mockServer struct {
	t   *testing.T
	srv *httptest.Server

	// Layout.
	prmMode        string // "header", "path", "root", "none"
	asMode         string // "8414path", "8414root", "oidcpath", "oidcappend", "oidcroot", "none"
	asPath         string // issuer path, e.g. "/tenant"
	issuerSlash    bool   // metadata issuer has a trailing slash; PRM doesn't
	noRegistration bool
	rotate         bool
	expiresIn      int
	scopes         []string
	resource       string // PRM resource; default <srv>/mcp

	mu            sync.Mutex
	clients       map[string]mockClient
	codes         map[string]mockCode
	access        map[string]bool
	refresh       map[string]string // refresh token -> client id
	invalidGrant  bool
	registerCount int
	refreshCount  int
	codeCount     int
	pkceVerified  int
	revoked       []string
	tokenForms    []url.Values
	authorizeReqs []url.Values
	seq           int
	// onAuthorizedMCP runs for each authorised /mcp request.
	onAuthorizedMCP func(token string)
}

type mockClient struct {
	secret    string
	redirects []string
}

type mockCode struct {
	client, redirect, challenge, resource string
}

func newMock(t *testing.T) *mockServer {
	m := &mockServer{
		t:         t,
		prmMode:   "path",
		asMode:    "8414root",
		rotate:    true,
		expiresIn: 3600,
		scopes:    []string{"read", "write"},
		clients:   map[string]mockClient{},
		codes:     map[string]mockCode{},
		access:    map[string]bool{},
		refresh:   map[string]string{},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockServer) mcpURL() string { return m.srv.URL + "/mcp" }

func (m *mockServer) issuer() string {
	is := m.srv.URL + m.asPath
	if m.issuerSlash {
		is += "/"
	}
	return is
}

func (m *mockServer) asMetadata() map[string]any {
	md := map[string]any{
		"issuer":                           m.issuer(),
		"authorization_endpoint":           m.srv.URL + "/authorize",
		"token_endpoint":                   m.srv.URL + "/token",
		"revocation_endpoint":              m.srv.URL + "/revoke",
		"code_challenge_methods_supported": []string{"S256"},
	}
	if !m.noRegistration {
		md["registration_endpoint"] = m.srv.URL + "/register"
	}
	return md
}

func (m *mockServer) next(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s-%d", prefix, m.seq)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func (m *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/mcp":
		m.handleMCP(w, r)
		return
	case p == "/authorize":
		m.handleAuthorize(w, r)
		return
	case p == "/token":
		m.handleToken(w, r)
		return
	case p == "/register" && !m.noRegistration:
		m.handleRegister(w, r)
		return
	case p == "/revoke":
		_ = r.ParseForm()
		m.mu.Lock()
		m.revoked = append(m.revoked, r.Form.Get("token_type_hint")+":"+r.Form.Get("token"))
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}

	// Protected resource metadata.
	prmPath := map[string]string{
		"header": "/custom/prm",
		"path":   "/.well-known/oauth-protected-resource/mcp",
		"root":   "/.well-known/oauth-protected-resource",
	}[m.prmMode]
	if prmPath != "" && p == prmPath {
		res := m.resource
		if res == "" {
			res = m.mcpURL()
		}
		writeJSON(w, 200, map[string]any{
			"resource":              res,
			"authorization_servers": []string{m.srv.URL + m.asPath},
			"scopes_supported":      m.scopes,
		})
		return
	}

	// Authorization server metadata.
	asPath := map[string]string{
		"8414path":   "/.well-known/oauth-authorization-server" + m.asPath,
		"8414root":   "/.well-known/oauth-authorization-server",
		"oidcpath":   "/.well-known/openid-configuration" + m.asPath,
		"oidcappend": m.asPath + "/.well-known/openid-configuration",
		"oidcroot":   "/.well-known/openid-configuration",
	}[m.asMode]
	if asPath != "" && p == asPath {
		writeJSON(w, 200, m.asMetadata())
		return
	}
	http.NotFound(w, r)
}

func (m *mockServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	m.mu.Lock()
	ok := tok != "" && m.access[tok]
	hook := m.onAuthorizedMCP
	m.mu.Unlock()
	if !ok {
		ch := `Bearer realm="mock"`
		if m.prmMode == "header" {
			ch += fmt.Sprintf(`, resource_metadata="%s/custom/prm"`, m.srv.URL)
		}
		w.Header().Set("WWW-Authenticate", ch)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if hook != nil {
		hook(tok)
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
}

func (m *mockServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		oauthErr(w, "invalid_client_metadata", err.Error())
		return
	}
	if !strings.HasPrefix(body.ClientName, "mcptui (") || body.AuthMethod != "none" {
		oauthErr(w, "invalid_client_metadata", "unexpected client metadata")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerCount++
	id := m.next("client")
	m.clients[id] = mockClient{redirects: body.RedirectURIs}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"redirect_uris":              body.RedirectURIs,
		"token_endpoint_auth_method": "none",
	})
}

func (m *mockServer) addClient(id, secret string, redirects []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[id] = mockClient{secret: secret, redirects: redirects}
}

func (m *mockServer) expectedResource() string {
	if m.resource != "" {
		return m.resource
	}
	return m.mcpURL()
}

func (m *mockServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.authorizeReqs = append(m.authorizeReqs, q)
	c, ok := m.clients[q.Get("client_id")]
	if !ok {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	if !slices.Contains(c.redirects, q.Get("redirect_uri")) {
		http.Error(w, "redirect_uri not registered", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("response_type") != "code" {
		http.Error(w, "PKCE required", http.StatusBadRequest)
		return
	}
	if m.prmMode != "none" && q.Get("resource") != m.expectedResource() {
		http.Error(w, "bad resource "+q.Get("resource"), http.StatusBadRequest)
		return
	}
	code := m.next("code")
	m.codes[code] = mockCode{client: q.Get("client_id"), redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), resource: q.Get("resource")}
	loc, _ := url.Parse(q.Get("redirect_uri"))
	lq := loc.Query()
	lq.Set("code", code)
	lq.Set("state", q.Get("state"))
	loc.RawQuery = lq.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

func (m *mockServer) issueTokens(w http.ResponseWriter, client string) {
	at := m.next("at")
	rt := m.next("rt")
	m.access[at] = true
	m.refresh[rt] = client
	writeJSON(w, 200, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"refresh_token": rt,
		"expires_in":    m.expiresIn,
		"scope":         strings.Join(m.scopes, " "),
	})
}

func (m *mockServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenForms = append(m.tokenForms, f)
	client, ok := m.clients[f.Get("client_id")]
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if client.secret != "" && f.Get("client_secret") != client.secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if m.prmMode != "none" && f.Get("resource") != m.expectedResource() {
		oauthErr(w, "invalid_target", "bad resource")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		c, ok := m.codes[f.Get("code")]
		if !ok || c.client != f.Get("client_id") || c.redirect != f.Get("redirect_uri") {
			oauthErr(w, "invalid_grant", "bad code")
			return
		}
		delete(m.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			oauthErr(w, "invalid_grant", "PKCE verification failed")
			return
		}
		m.pkceVerified++
		m.codeCount++
		m.issueTokens(w, f.Get("client_id"))
	case "refresh_token":
		m.refreshCount++
		rt := f.Get("refresh_token")
		owner, ok := m.refresh[rt]
		if m.invalidGrant || !ok || owner != f.Get("client_id") {
			oauthErr(w, "invalid_grant", "refresh token invalid")
			return
		}
		if m.rotate {
			delete(m.refresh, rt)
			m.issueTokens(w, owner)
			return
		}
		at := m.next("at")
		m.access[at] = true
		writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": m.expiresIn})
	default:
		oauthErr(w, "unsupported_grant_type", f.Get("grant_type"))
	}
}

func (m *mockServer) invalidateAccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.access = map[string]bool{}
}

func (m *mockServer) counts() (register, refresh, codes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registerCount, m.refreshCount, m.codeCount
}

// fakeUI drives the browser step.
type fakeUI struct {
	t *testing.T
	// mode: "callback" (follow redirect to loopback), "paste" (submit URL),
	// "code" (submit bare code), "none" (do nothing).
	mode        string
	tamperState bool

	mu       sync.Mutex
	prompts  []LoginPrompt
	finished []error
}

func (u *fakeUI) ShowLogin(p LoginPrompt) {
	u.mu.Lock()
	u.prompts = append(u.prompts, p)
	u.mu.Unlock()
	if u.mode == "none" {
		return
	}
	go func() {
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := noFollow.Get(p.AuthURL)
		if err != nil {
			u.t.Errorf("authorize: %v", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			u.t.Errorf("authorize: HTTP %d: %s", resp.StatusCode, body)
			return
		}
		loc := resp.Header.Get("Location")
		if u.tamperState {
			lu, _ := url.Parse(loc)
			q := lu.Query()
			q.Set("state", "tampered")
			lu.RawQuery = q.Encode()
			loc = lu.String()
		}
		switch u.mode {
		case "callback":
			r2, err := http.Get(loc)
			if err != nil {
				u.t.Errorf("callback: %v", err)
				return
			}
			io.Copy(io.Discard, r2.Body)
			r2.Body.Close()
		case "paste":
			p.Submit("  " + loc + "\n")
		case "code":
			lu, _ := url.Parse(loc)
			p.Submit(lu.Query().Get("code"))
		}
	}()
}

func (u *fakeUI) LoginFinished(scope string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.finished = append(u.finished, err)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type testEnv struct {
	mock      *mockServer
	storePath string
	profile   *config.Profile
	ui        *fakeUI
}

func newEnv(t *testing.T, mock *mockServer) *testEnv {
	return &testEnv{
		mock:      mock,
		storePath: filepath.Join(t.TempDir(), "cfg", "credentials.json"),
		profile:   &config.Profile{Name: "test", URL: mock.mcpURL(), CallbackPort: freePort(t)},
		ui:        &fakeUI{t: t, mode: "callback"},
	}
}

func (e *testEnv) manager(t *testing.T, mut ...func(*Options)) *Manager {
	opts := Options{
		Profile:      e.profile,
		Store:        NewStore(e.storePath),
		NoBrowser:    true,
		Interactive:  true,
		UI:           e.ui,
		LoginTimeout: 10 * time.Second,
		Log: func(ev mcp.Event) {
			t.Logf("[%s] %s", ev.Level, ev.Text)
		},
	}
	for _, f := range mut {
		f(&opts)
	}
	return NewManager(opts)
}

func listenLocal(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}
