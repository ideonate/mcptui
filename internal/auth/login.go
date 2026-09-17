package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ideonate/mcptui/internal/config"
)

// callbackResult is what arrives from the loopback redirect or a paste.
type callbackResult struct {
	Code        string
	State       string
	Error       string
	Description string
	// CheckState is false for a pasted bare code.
	CheckState bool
	Source     string
}

// parsePasted extracts code/state/error from a pasted redirect URL, query
// string, or bare code.
func parsePasted(s string) (callbackResult, error) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'<>`)
	if s == "" {
		return callbackResult{}, errors.New("nothing pasted")
	}
	looksLikeQuery := strings.Contains(s, "code=") || strings.Contains(s, "error=")
	if !looksLikeQuery {
		if strings.ContainsAny(s, " \t/?&") {
			return callbackResult{}, errors.New("could not find code= in the pasted text")
		}
		return callbackResult{Code: s, Source: "paste"}, nil
	}
	query := s
	if i := strings.Index(s, "?"); i >= 0 {
		query = s[i+1:]
	}
	if i := strings.Index(query, "#"); i >= 0 {
		// Some servers use the fragment; merge it.
		query = query[:i] + "&" + query[i+1:]
	}
	v, err := url.ParseQuery(query)
	if err != nil {
		return callbackResult{}, fmt.Errorf("could not parse pasted URL: %w", err)
	}
	r := callbackResult{
		Code:        v.Get("code"),
		State:       v.Get("state"),
		Error:       v.Get("error"),
		Description: v.Get("error_description"),
		CheckState:  true,
		Source:      "paste",
	}
	if r.Code == "" && r.Error == "" {
		return callbackResult{}, errors.New("pasted URL has no code or error parameter")
	}
	return r, nil
}

// callbackPorts returns the three ports to register and try.
func (m *Manager) callbackPorts() []int {
	p := m.opts.Profile.CallbackPort
	if p == 0 {
		p = DefaultCallbackPort
	}
	return []int{p, p + 1, p + 2}
}

func redirectURIsFor(ports []int) []string {
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("http://127.0.0.1:%d/callback", p))
	}
	for _, p := range ports {
		out = append(out, fmt.Sprintf("http://localhost:%d/callback", p))
	}
	return out
}

// clientIdentity resolves the client to use, registering if necessary.
func (m *Manager) clientIdentity(ctx context.Context, d *discovery) (clientAuth, []string, error) {
	p := m.opts.Profile
	wantURIs := redirectURIsFor(m.callbackPorts())
	if p.ClientID != "" {
		id, err := config.ExpandValue(p.ClientID)
		if err != nil {
			return clientAuth{}, nil, fmt.Errorf("client_id: %w", err)
		}
		secret, err := config.ExpandValue(p.ClientSecret)
		if err != nil {
			return clientAuth{}, nil, fmt.Errorf("client_secret: %w", err)
		}
		ca := clientAuth{ClientID: id, ClientSecret: secret, Method: "none"}
		if secret != "" {
			ca.Method = "client_secret_post"
		}
		m.logf("info", "using pre-registered client_id %s from profile", id)
		return ca, wantURIs, nil
	}

	key := d.issuerKey()
	var reg *clientRegistration
	if sd, err := m.opts.Store.load(); err == nil {
		if r := sd.Clients[key]; r != nil {
			reg = r
		}
	}
	if reg != nil {
		covered := false
		for _, u := range reg.RedirectURIs {
			if slices.Contains(wantURIs, u) {
				covered = true
				break
			}
		}
		if covered || len(reg.RedirectURIs) == 0 {
			m.logf("info", "using cached client registration %s for %s", reg.ClientID, key)
			return clientAuth{ClientID: reg.ClientID, ClientSecret: reg.ClientSecret, Method: reg.TokenAuthMethod}, uriList(reg.RedirectURIs, wantURIs), nil
		}
		m.logf("info", "cached client registration does not cover callback ports %v; registering again", m.callbackPorts())
	}

	if d.ASM.RegistrationEndpoint == "" {
		return clientAuth{}, nil, fmt.Errorf("this server needs a pre-registered client_id; set `client_id` in the profile (authorization server %s has no registration_endpoint)", d.AuthServer)
	}
	var regErr error
	err := m.opts.Store.update(ctx, func(sd *storeData) (bool, error) {
		// Another process may have registered meanwhile.
		if r := sd.Clients[key]; r != nil && r != reg && (reg == nil || r.ClientID != reg.ClientID) {
			reg = r
			return false, nil
		}
		r, err := m.register(ctx, d, wantURIs)
		if err != nil {
			regErr = err
			return false, nil
		}
		sd.Clients[key] = r
		reg = r
		return true, nil
	})
	if err != nil {
		return clientAuth{}, nil, err
	}
	if regErr != nil {
		return clientAuth{}, nil, regErr
	}
	return clientAuth{ClientID: reg.ClientID, ClientSecret: reg.ClientSecret, Method: reg.TokenAuthMethod}, uriList(reg.RedirectURIs, wantURIs), nil
}

func uriList(registered, fallback []string) []string {
	if len(registered) == 0 {
		return fallback
	}
	return registered
}

// scopeFor picks the scope: profile, PRM scopes_supported, WWW-Authenticate.
func (m *Manager) scopeFor(d *discovery) string {
	if s := m.opts.Profile.Scope; s != "" {
		return s
	}
	if d.PRM != nil && len(d.PRM.ScopesSupported) > 0 {
		return strings.Join(d.PRM.ScopesSupported, " ")
	}
	return d.WWWScope
}

// listenCallback binds the first free port. It returns the listener and the
// redirect URI to use (chosen from the registered ones).
func (m *Manager) listenCallback(registered []string) (net.Listener, string) {
	for _, port := range m.callbackPorts() {
		ln, err := net.Listen("tcp", net.JoinHostPort(m.opts.CallbackHost, strconv.Itoa(port)))
		if err != nil {
			m.logf("info", "callback port %d unavailable: %v", port, err)
			continue
		}
		return ln, pickRedirect(registered, port)
	}
	port := m.callbackPorts()[0]
	m.logf("warn", "no callback port free; only the paste fallback will work")
	return nil, pickRedirect(registered, port)
}

func pickRedirect(registered []string, port int) string {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		u := fmt.Sprintf("http://%s:%d/callback", host, port)
		if len(registered) == 0 || slices.Contains(registered, u) {
			return u
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", port)
}

const callbackPage = `<!doctype html><html><head><meta charset="utf-8"><title>mcptui</title>
<style>body{font-family:system-ui,sans-serif;margin:4rem auto;max-width:36rem;padding:0 1rem}</style></head>
<body><h1>%s</h1><p>%s</p></body></html>`

// login performs the authorization code flow. failed is the token that was
// rejected ("\x00force" to always log in).
func (m *Manager) login(ctx context.Context, wwwAuth, failed string) (retErr error) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	if failed != "\x00force" {
		if c, _ := m.Status(); failed != "" && c != nil && c.AccessToken != "" && c.AccessToken != failed {
			return nil // a concurrent login already succeeded
		}
	}
	ui := m.opts.UI
	if ui == nil {
		ui = stderrUI{}
	}
	var grantedScope string
	defer func() { ui.LoginFinished(grantedScope, retErr) }()

	m.mu.Lock()
	if wwwAuth != "" {
		m.lastWWW = wwwAuth
	}
	m.mu.Unlock()
	d, err := m.discovery(ctx, true)
	if err != nil {
		return err
	}
	if len(d.ASM.CodeChallengeMethodsSupported) > 0 && !slices.Contains(d.ASM.CodeChallengeMethodsSupported, "S256") {
		m.logf("warn", "authorization server does not list S256 in code_challenge_methods_supported; continuing with S256")
	}
	ca, registered, err := m.clientIdentity(ctx, d)
	if err != nil {
		return err
	}

	verifier, challenge := pkcePair()
	state := randomString(16)
	ln, redirectURI := m.listenCallback(registered)

	results := make(chan callbackResult, 4)
	send := func(r callbackResult) {
		select {
		case results <- r:
		default:
		}
	}
	if ln != nil {
		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			res := callbackResult{
				Code: q.Get("code"), State: q.Get("state"), Error: q.Get("error"),
				Description: q.Get("error_description"), CheckState: true, Source: "callback",
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch {
			case res.Error != "":
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, callbackPage, "Login failed", html.EscapeString(res.Error+": "+res.Description))
			case res.Code == "":
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, callbackPage, "Login failed", "The redirect had no authorization code.")
				return
			case res.State != state:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, callbackPage, "Login failed", "State mismatch. Start the login again from mcptui.")
			default:
				fmt.Fprintf(w, callbackPage, "Logged in", "You can close this tab and return to mcptui.")
			}
			send(res)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = srv.Shutdown(sctx)
			cancel()
		}()
		m.logf("info", "listening for callback on %s", ln.Addr())
	}

	authURL, err := url.Parse(d.ASM.AuthorizationEndpoint)
	if err != nil {
		return fmt.Errorf("bad authorization_endpoint: %w", err)
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", ca.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if d.Resource != "" {
		q.Set("resource", d.Resource)
	}
	scope := m.scopeFor(d)
	if scope != "" {
		q.Set("scope", scope)
	}
	authURL.RawQuery = q.Encode()

	if !m.opts.NoBrowser {
		if err := openBrowser(authURL.String()); err != nil {
			m.logf("info", "could not open a browser (%v); open the URL manually", err)
		}
	}
	var pasteMu sync.Mutex
	ui.ShowLogin(LoginPrompt{
		AuthURL:     authURL.String(),
		RedirectURI: redirectURI,
		Submit: func(pasted string) {
			pasteMu.Lock()
			defer pasteMu.Unlock()
			r, err := parsePasted(pasted)
			if err != nil {
				m.logf("warn", "paste: %v", err)
				send(callbackResult{Error: "invalid_paste", Description: err.Error(), Source: "paste"})
				return
			}
			send(r)
		},
	})

	timer := time.NewTimer(m.opts.LoginTimeout)
	defer timer.Stop()
	var res callbackResult
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("login cancelled: %w", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("login timed out after %s waiting for the redirect to %s. Common causes: the redirect URI is not registered with the authorization server (try `mcptui auth reset-client %s`), or the callback port is not forwarded to this machine (paste the URL from the browser's address bar instead)", m.opts.LoginTimeout, redirectURI, m.opts.Profile.Name)
		case res = <-results:
		}
		if res.Error == "invalid_paste" {
			continue // let the user paste again
		}
		break
	}
	if res.Error != "" {
		err := &OAuthError{Endpoint: "authorization endpoint", Code: res.Error, Description: res.Description}
		return m.clientRejectedHint(err)
	}
	if res.CheckState && res.State != state {
		return errors.New("state mismatch in authorization response; possible CSRF or a stale browser tab — start the login again")
	}
	m.logf("info", "received authorization code via %s", res.Source)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.Code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	if d.Resource != "" {
		form.Set("resource", d.Resource)
	}
	tr, err := m.tokenRequest(ctx, d.ASM.TokenEndpoint, form, ca)
	if err != nil {
		return m.clientRejectedHint(err)
	}
	cred := &Credential{
		Resource:            d.Resource,
		MCPURL:              m.opts.Profile.URL,
		AuthorizationServer: d.issuerKey(),
		ClientID:            ca.ClientID,
		ClientSecret:        ca.ClientSecret,
		TokenEndpoint:       d.ASM.TokenEndpoint,
		RevocationEndpoint:  d.ASM.RevocationEndpoint,
		TokenAuthMethod:     ca.Method,
	}
	m.applyTokens(cred, tr)
	if cred.Scope == "" {
		cred.Scope = scope
	}
	if err := m.opts.Store.update(ctx, func(sd *storeData) (bool, error) {
		sd.remove(m.opts.Profile.URL)
		sd.put(cred)
		return true, nil
	}); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	grantedScope = cred.Scope
	m.logf("info", "logged in; granted scope %q, expires %s", cred.Scope, expiryText(cred.ExpiresAt))
	return nil
}

// stderrUI is used when no LoginUI is supplied.
type stderrUI struct{}

func (stderrUI) ShowLogin(p LoginPrompt) {
	fmt.Fprintf(os.Stderr, "Open this URL to log in:\n\n  %s\n\n", p.AuthURL)
}

func (stderrUI) LoginFinished(scope string, err error) {}
