package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ideonate/mcptui/internal/mcp"
)

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestParseWWWAuthenticate(t *testing.T) {
	h := `Basic realm="x", Bearer realm="mcp", error="invalid_token", resource_metadata="https://h/.well-known/oauth-protected-resource/mcp", scope="a b"`
	c := parseWWWAuthenticate(h)
	if c.ResourceMetadata != "https://h/.well-known/oauth-protected-resource/mcp" || c.Scope != "a b" || c.Error != "invalid_token" {
		t.Fatalf("got %+v", c)
	}
	c = parseWWWAuthenticate(`Bearer resource_metadata=https://h/prm, scope="x \"y\""`)
	if c.ResourceMetadata != "https://h/prm" || c.Scope != `x "y"` {
		t.Fatalf("got %+v", c)
	}
	if c := parseWWWAuthenticate(""); c != (bearerChallenge{}) {
		t.Fatalf("got %+v", c)
	}
}

func TestDiscoveryFallbacks(t *testing.T) {
	cases := []struct {
		name        string
		prm, as     string
		asPath      string
		profileAS   bool
		issuerSlash bool
		wantPRM     bool
	}{
		{name: "www-authenticate resource_metadata + 8414 path-insert", prm: "header", as: "8414path", asPath: "/tenant", wantPRM: true},
		{name: "prm path-insert + 8414 root", prm: "path", as: "8414root", wantPRM: true},
		{name: "prm root + oidc path-insert", prm: "root", as: "oidcpath", asPath: "/tenant", wantPRM: true},
		{name: "prm root + oidc appended", prm: "root", as: "oidcappend", asPath: "/tenant", wantPRM: true},
		{name: "prm path + oidc root", prm: "path", as: "oidcroot", wantPRM: true},
		{name: "no prm, profile auth_server", prm: "none", as: "8414path", asPath: "/tenant", profileAS: true},
		{name: "no prm, MCP origin", prm: "none", as: "8414root"},
		{name: "trailing-slash issuer mismatch", prm: "path", as: "8414root", issuerSlash: true, wantPRM: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMock(t)
			mock.prmMode, mock.asMode, mock.asPath, mock.issuerSlash = tc.prm, tc.as, tc.asPath, tc.issuerSlash
			env := newEnv(t, mock)
			if tc.profileAS {
				env.profile.AuthServer = mock.srv.URL + tc.asPath
			}
			m := env.manager(t)
			d, err := m.discover(ctxT(t), "")
			if err != nil {
				t.Fatal(err)
			}
			if (d.PRM != nil) != tc.wantPRM {
				t.Fatalf("PRM found = %v, want %v", d.PRM != nil, tc.wantPRM)
			}
			if d.ASM.TokenEndpoint != mock.srv.URL+"/token" || d.ASM.RegistrationEndpoint != mock.srv.URL+"/register" {
				t.Fatalf("wrong ASM: %+v", d.ASM)
			}
			if d.Resource != mock.mcpURL() {
				t.Fatalf("resource = %s", d.Resource)
			}
			if got, want := d.issuerKey(), normalizeURL(mock.srv.URL+tc.asPath); got != want {
				t.Fatalf("issuer key %s, want %s", got, want)
			}
			// A full login works for every layout.
			if err := m.Login(ctxT(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscoveryNoMetadataUsesDefaults(t *testing.T) {
	mock := newMock(t)
	mock.prmMode, mock.asMode = "none", "none"
	env := newEnv(t, mock)
	d, err := env.manager(t).discover(ctxT(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.ASM.AuthorizationEndpoint != mock.srv.URL+"/authorize" || d.ASM.TokenEndpoint != mock.srv.URL+"/token" {
		t.Fatalf("got %+v", d.ASM)
	}
}

func TestResourceMayDifferFromURL(t *testing.T) {
	mock := newMock(t)
	mock.resource = "https://public.example.com/mcp"
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	c, _ := m.Status()
	if c == nil || c.Resource != "https://public.example.com/mcp" {
		t.Fatalf("credential %+v", c)
	}
	// Token() finds it via the MCP URL alias.
	if tok, _ := m.Token(ctxT(t)); tok == "" {
		t.Fatal("no token via alias")
	}
}

func TestRegisterOnceThenCache(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	for i := 0; i < 2; i++ {
		if err := m.Login(ctxT(t)); err != nil {
			t.Fatal(err)
		}
	}
	// A new manager (new process) also reuses the cached registration.
	if err := env.manager(t).Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	reg, _, codes := mock.counts()
	if reg != 1 || codes != 3 {
		t.Fatalf("register=%d codes=%d", reg, codes)
	}
	// Registered with all three ports on both loopback hosts.
	sd, _ := NewStore(env.storePath).load()
	for _, c := range sd.Clients {
		if len(c.RedirectURIs) != 6 {
			t.Fatalf("redirects %v", c.RedirectURIs)
		}
	}
	// Scope defaults to PRM scopes_supported and resource is sent.
	q := mock.authorizeReqs[0]
	if q.Get("scope") != "read write" || q.Get("resource") != mock.mcpURL() {
		t.Fatalf("authorize query %v", q)
	}
	tok, err := m.Token(ctxT(t))
	if err != nil || !strings.HasPrefix(tok, "at-") {
		t.Fatalf("token %q %v", tok, err)
	}
}

func TestResetClientRegistersAgain(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.ResetClient(); err != nil {
		t.Fatal(err)
	}
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if reg, _, _ := mock.counts(); reg != 2 {
		t.Fatalf("register=%d", reg)
	}
}

func TestPreRegisteredClient(t *testing.T) {
	mock := newMock(t)
	mock.noRegistration = true
	env := newEnv(t, mock)
	mock.addClient("pre-id", "s3cret", redirectURIsFor([]int{env.profile.CallbackPort, env.profile.CallbackPort + 1, env.profile.CallbackPort + 2}))

	// Without client_id: clear error.
	err := env.manager(t).Login(ctxT(t))
	if err == nil || !strings.Contains(err.Error(), "pre-registered client_id; set `client_id` in the profile") {
		t.Fatalf("err = %v", err)
	}

	os.Setenv("MCPTUI_TEST_SECRET", "s3cret")
	defer os.Unsetenv("MCPTUI_TEST_SECRET")
	env.profile.ClientID = "pre-id"
	env.profile.ClientSecret = "env:MCPTUI_TEST_SECRET"
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if reg, _, _ := mock.counts(); reg != 0 {
		t.Fatalf("registered %d times", reg)
	}
	f := mock.tokenForms[len(mock.tokenForms)-1]
	if f.Get("client_id") != "pre-id" || f.Get("client_secret") != "s3cret" {
		t.Fatalf("token form %v", f)
	}
	// Refresh also authenticates with client_secret_post.
	mock.invalidateAccess()
	if err := m.HandleUnauthorized(ctxT(t), ""); err != nil {
		t.Fatal(err)
	}
	if _, refreshes, _ := mock.counts(); refreshes != 1 {
		t.Fatalf("refreshes %d", refreshes)
	}
}

func TestPKCEVerified(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if mock.pkceVerified != 1 {
		t.Fatalf("pkce verified %d", mock.pkceVerified)
	}
	c, _ := m.Status()

	// A code redeemed with the wrong verifier is rejected.
	_, challenge := pkcePair()
	redirect := redirectURIsFor([]int{env.profile.CallbackPort})[0]
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"s"}, "resource": {mock.mcpURL()}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(mock.srv.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	wrongVerifier, _ := pkcePair()
	_, err = m.tokenRequest(ctxT(t), mock.srv.URL+"/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "redirect_uri": {redirect},
		"code_verifier": {wrongVerifier}, "resource": {mock.mcpURL()},
	}, clientAuth{ClientID: c.ClientID})
	if !isInvalidGrant(err) {
		t.Fatalf("expected invalid_grant, got %v", err)
	}
}

func TestStateMismatchRejected(t *testing.T) {
	for _, mode := range []string{"callback", "paste"} {
		t.Run(mode, func(t *testing.T) {
			mock := newMock(t)
			env := newEnv(t, mock)
			env.ui.mode = mode
			env.ui.tamperState = true
			err := env.manager(t).Login(ctxT(t))
			if err == nil || !strings.Contains(err.Error(), "state mismatch") {
				t.Fatalf("err = %v", err)
			}
			if _, _, codes := mock.counts(); codes != 0 {
				t.Fatal("code was exchanged despite state mismatch")
			}
			if len(env.ui.finished) != 1 || env.ui.finished[0] == nil {
				t.Fatalf("LoginFinished not reported: %v", env.ui.finished)
			}
		})
	}
}

func TestPasteFallback(t *testing.T) {
	for _, mode := range []string{"paste", "code"} {
		t.Run(mode, func(t *testing.T) {
			mock := newMock(t)
			env := newEnv(t, mock)
			env.ui.mode = mode
			// Occupy nothing, but make sure the callback isn't what's used:
			// the fake UI never touches the loopback listener in these modes.
			m := env.manager(t)
			if err := m.Login(ctxT(t)); err != nil {
				t.Fatal(err)
			}
			if tok, _ := m.Token(ctxT(t)); tok == "" {
				t.Fatal("no token after paste login")
			}
			if p := env.ui.prompts[0]; !strings.HasPrefix(p.RedirectURI, "http://127.0.0.1:") || !strings.Contains(p.AuthURL, "code_challenge=") {
				t.Fatalf("prompt %+v", p)
			}
		})
	}
}

func TestPasteFallbackWhenPortsBusy(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	env.ui.mode = "paste"
	var lns []interface{ Close() error }
	for i := 0; i < 3; i++ {
		ln, err := listenLocal(env.profile.CallbackPort + i)
		if err != nil {
			t.Skipf("port busy: %v", err)
		}
		lns = append(lns, ln)
	}
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	if err := env.manager(t).Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
}

func TestParsePasted(t *testing.T) {
	r, err := parsePasted("http://127.0.0.1:33418/callback?code=abc&state=xyz")
	if err != nil || r.Code != "abc" || r.State != "xyz" || !r.CheckState {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = parsePasted("?error=access_denied&error_description=nope&state=s")
	if err != nil || r.Error != "access_denied" || r.Description != "nope" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = parsePasted("  rawcode123 ")
	if err != nil || r.Code != "rawcode123" || r.CheckState {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := parsePasted("http://example.com/no/params"); err == nil {
		t.Fatal("expected error")
	}
}

func TestAuthorizationErrorReturned(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	env.ui.mode = "none"
	m := env.manager(t, func(o *Options) {
		o.UI = submitUI{text: "http://127.0.0.1/callback?error=access_denied&error_description=user+said+no"}
	})
	err := m.Login(ctxT(t))
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

type submitUI struct{ text string }

func (s submitUI) ShowLogin(p LoginPrompt)   { go p.Submit(s.text) }
func (submitUI) LoginFinished(string, error) {}

func TestLoginTimeoutMessage(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	env.ui.mode = "none"
	err := env.manager(t, func(o *Options) { o.LoginTimeout = 100 * time.Millisecond }).Login(ctxT(t))
	if err == nil || !strings.Contains(err.Error(), "not registered") || !strings.Contains(err.Error(), "forwarded") {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationPersistedBeforeRetry(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Status()
	mock.invalidateAccess()

	var mu sync.Mutex
	var checked, persisted bool
	mock.mu.Lock()
	mock.onAuthorizedMCP = func(tok string) {
		sd, err := NewStore(env.storePath).load()
		mu.Lock()
		defer mu.Unlock()
		checked = true
		if err == nil {
			if _, c := sd.lookup(mock.mcpURL()); c != nil && c.AccessToken == tok && c.RefreshToken != before.RefreshToken {
				persisted = true
			}
		}
	}
	mock.mu.Unlock()

	tr := mcp.NewHTTPTransport(mcp.HTTPOptions{URL: mock.mcpURL(), Authorizer: m})
	defer tr.Close()
	if err := tr.Send(ctxT(t), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.Messages():
	case <-time.After(5 * time.Second):
		t.Fatal("no response")
	}
	mu.Lock()
	defer mu.Unlock()
	if !checked || !persisted {
		t.Fatalf("checked=%v persisted=%v", checked, persisted)
	}
	after, _ := m.Status()
	if after.RefreshToken == before.RefreshToken || after.AccessToken == before.AccessToken {
		t.Fatal("tokens not rotated")
	}
	info, err := os.Stat(env.storePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode %v %v", info.Mode(), err)
	}
	if dinfo, _ := os.Stat(filepath.Dir(env.storePath)); dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dinfo.Mode())
	}
}

func TestRefreshKeepsOldRefreshTokenWithoutRotation(t *testing.T) {
	mock := newMock(t)
	mock.rotate = false
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Status()
	mock.invalidateAccess()
	if err := m.HandleUnauthorized(ctxT(t), ""); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Status()
	if after.RefreshToken != before.RefreshToken || after.AccessToken == before.AccessToken {
		t.Fatalf("before %+v after %+v", before, after)
	}
	f := mock.tokenForms[len(mock.tokenForms)-1]
	if f.Get("resource") != mock.mcpURL() {
		t.Fatal("refresh did not send resource")
	}
}

func TestConcurrentRefreshAcrossManagers(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	if err := env.manager(t).Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	// Both "processes" think the token expires within the refresh margin.
	future := func() time.Time { return time.Now().Add(time.Duration(mock.expiresIn)*time.Second - time.Minute) }
	a := env.manager(t, func(o *Options) { o.Now = future })
	b := env.manager(t, func(o *Options) { o.Now = future })

	var wg sync.WaitGroup
	tokens := make([]string, 8)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := a
			if i%2 == 1 {
				m = b
			}
			tok, err := m.ValidToken(ctxT(t))
			if err != nil {
				t.Errorf("ValidToken: %v", err)
			}
			tokens[i] = tok
		}(i)
	}
	wg.Wait()
	if _, refreshes, _ := mock.counts(); refreshes != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes)
	}
	for _, tok := range tokens[1:] {
		if tok != tokens[0] {
			t.Fatalf("tokens differ: %v", tokens)
		}
	}
}

func TestInvalidGrantStartsNewLogin(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	mock.invalidateAccess()
	mock.mu.Lock()
	mock.invalidGrant = true
	mock.mu.Unlock()

	// Non-interactive: ErrLoginRequired.
	ni := env.manager(t, func(o *Options) { o.Interactive = false; o.UI = nil })
	if _, err := ni.Token(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	err := ni.HandleUnauthorized(ctxT(t), `Bearer realm="mock"`)
	if !errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "mcptui auth login test") {
		t.Fatalf("err = %v", err)
	}

	// Interactive: a new browser login.
	mock.mu.Lock()
	mock.invalidGrant = false
	mock.mu.Unlock()
	if _, err := m.Token(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.HandleUnauthorized(ctxT(t), `Bearer realm="mock"`); err != nil {
		t.Fatal(err)
	}
	if _, _, codes := mock.counts(); codes != 2 {
		t.Fatalf("codes = %d, want 2 logins", codes)
	}
	tok, _ := m.Token(ctxT(t))
	mock.mu.Lock()
	valid := mock.access[tok]
	mock.mu.Unlock()
	if !valid {
		t.Fatal("new token not valid")
	}
}

func TestValidTokenWithoutCredentials(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if _, err := m.ValidToken(ctxT(t)); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if tok, err := m.Token(ctxT(t)); tok != "" || err != nil {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	if c, err := m.Status(); c != nil || err != nil {
		t.Fatalf("Status = %v, %v", c, err)
	}
}

func TestLogoutRevokes(t *testing.T) {
	mock := newMock(t)
	env := newEnv(t, mock)
	m := env.manager(t)
	if err := m.Login(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	c, _ := m.Status()
	// A fresh manager (no cached discovery) uses the stored revocation endpoint.
	if err := env.manager(t).Logout(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	want := []string{"refresh_token:" + c.RefreshToken, "access_token:" + c.AccessToken}
	if len(mock.revoked) != 2 || mock.revoked[0] != want[0] || mock.revoked[1] != want[1] {
		t.Fatalf("revoked %v, want %v", mock.revoked, want)
	}
	if c, _ := m.Status(); c != nil {
		t.Fatal("credential not deleted")
	}
}

func TestLoginViaTransport401(t *testing.T) {
	// End to end: no token → 401 → interactive login → retried request succeeds.
	mock := newMock(t)
	mock.prmMode = "header"
	env := newEnv(t, mock)
	m := env.manager(t)
	tr := mcp.NewHTTPTransport(mcp.HTTPOptions{URL: mock.mcpURL(), Authorizer: m})
	defer tr.Close()
	if err := tr.Send(ctxT(t), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.Messages():
	case <-time.After(5 * time.Second):
		t.Fatal("no response")
	}
}

func TestRedact(t *testing.T) {
	if Redact("abcdefghij") != "abcdef…" || Redact("abc") != "…" || Redact("") != "" {
		t.Fatal("redact")
	}
}
