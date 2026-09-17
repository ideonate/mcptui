// Package auth implements MCP authorization (OAuth 2.1 with PKCE, RFC 9728
// resource metadata, RFC 8414 discovery, RFC 7591 registration) for mcptui.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/mcp"
)

// ErrLoginRequired means there is no usable token and a browser login is
// needed. Errors wrap it with the command to run.
var ErrLoginRequired = errors.New("login required")

// DefaultCallbackPort is the first loopback port tried for the redirect.
const DefaultCallbackPort = 33418

// DefaultLoginTimeout bounds how long a login waits for the redirect.
const DefaultLoginTimeout = 10 * time.Minute

// refreshMargin is how early tokens are refreshed proactively.
const refreshMargin = 5 * time.Minute

// Options configures a Manager.
type Options struct {
	Profile      *config.Profile
	Store        *Store
	HTTPClient   *http.Client
	CallbackHost string // bind address, default 127.0.0.1
	NoBrowser    bool
	Interactive  bool // false: never start a browser login
	UI           LoginUI
	Log          mcp.Logf
	Now          func() time.Time
	// LoginTimeout overrides DefaultLoginTimeout.
	LoginTimeout time.Duration
}

// LoginPrompt is shown to the user while a login is waiting.
type LoginPrompt struct {
	AuthURL     string
	RedirectURI string
	// Submit accepts a pasted redirect URL (or bare code). Safe from any
	// goroutine.
	Submit func(pasted string)
}

// LoginUI displays login progress.
type LoginUI interface {
	ShowLogin(p LoginPrompt)
	LoginFinished(scope string, err error)
}

// Manager handles tokens for one profile. It implements mcp.Authorizer.
type Manager struct {
	opts Options

	mu       sync.Mutex
	disc     *discovery
	lastWWW  string
	lastSent string

	loginMu sync.Mutex
}

var _ mcp.Authorizer = (*Manager)(nil)

// NewManager creates a Manager.
func NewManager(opts Options) *Manager {
	if opts.Store == nil {
		opts.Store = NewStore("")
	}
	if opts.CallbackHost == "" {
		opts.CallbackHost = "127.0.0.1"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LoginTimeout == 0 {
		opts.LoginTimeout = DefaultLoginTimeout
	}
	if opts.Profile == nil {
		opts.Profile = &config.Profile{}
	}
	return &Manager{opts: opts}
}

func (m *Manager) now() time.Time { return m.opts.Now() }

func (m *Manager) httpClient() *http.Client {
	if m.opts.HTTPClient != nil {
		return m.opts.HTTPClient
	}
	return http.DefaultClient
}

func (m *Manager) logf(level, format string, args ...any) {
	if m.opts.Log != nil {
		m.opts.Log(mcp.Event{Source: "auth", Level: level, Text: fmt.Sprintf(format, args...)})
	}
}

func (m *Manager) loginRequired() error {
	name := m.opts.Profile.Name
	if name == "" || m.opts.Profile.Temporary {
		name = "--profile-url " + m.opts.Profile.URL
	}
	return fmt.Errorf("%w: run `mcptui auth login %s`", ErrLoginRequired, name)
}

// Status returns the stored credential, or nil if none.
func (m *Manager) Status() (*Credential, error) {
	d, err := m.opts.Store.load()
	if err != nil {
		return nil, err
	}
	_, c := d.lookup(m.opts.Profile.URL)
	if c == nil {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (m *Manager) stale(c *Credential) bool {
	return !c.ExpiresAt.IsZero() && m.now().Add(refreshMargin).After(c.ExpiresAt)
}

func (m *Manager) expired(c *Credential) bool {
	return !c.ExpiresAt.IsZero() && !m.now().Before(c.ExpiresAt)
}

// Token returns the stored access token (refreshing proactively when close
// to expiry), or "" if there is none. It never starts a login.
func (m *Manager) Token(ctx context.Context) (string, error) {
	c, err := m.Status()
	if err != nil {
		m.logf("warn", "read credentials: %v", err)
		return "", nil
	}
	if c == nil || c.AccessToken == "" {
		return "", nil
	}
	if m.stale(c) && c.RefreshToken != "" {
		m.logf("info", "access token expires %s; refreshing", c.ExpiresAt.Format(time.RFC3339))
		if nc, err := m.refresh(ctx, "", false); err != nil {
			m.logf("warn", "proactive refresh failed: %v", err)
		} else if nc != nil {
			c = nc
		}
	}
	m.mu.Lock()
	m.lastSent = c.AccessToken
	m.mu.Unlock()
	return c.AccessToken, nil
}

// HandleUnauthorized reacts to a 401: refresh once, else log in (when
// interactive) or return ErrLoginRequired.
func (m *Manager) HandleUnauthorized(ctx context.Context, wwwAuth string) error {
	m.mu.Lock()
	m.lastWWW = wwwAuth
	failed := m.lastSent
	m.mu.Unlock()

	c, err := m.Status()
	if err != nil {
		return err
	}
	if failed != "" && c != nil && c.AccessToken != "" && c.AccessToken != failed {
		// Someone (another request or process) already obtained a new token.
		return nil
	}
	ch := parseWWWAuthenticate(wwwAuth)
	if c != nil && c.RefreshToken != "" && ch.Error != "insufficient_scope" {
		if _, err := m.refresh(ctx, failed, true); err == nil {
			return nil
		} else {
			m.logf("warn", "refresh after 401 failed: %v", err)
		}
	}
	if !m.opts.Interactive {
		return m.loginRequired()
	}
	return m.login(ctx, wwwAuth, failed)
}

// ValidToken returns a usable access token, refreshing if needed.
func (m *Manager) ValidToken(ctx context.Context) (string, error) {
	c, err := m.Status()
	if err != nil {
		return "", err
	}
	if c == nil || c.AccessToken == "" {
		return "", m.loginRequired()
	}
	if m.stale(c) {
		if c.RefreshToken == "" {
			if m.expired(c) {
				return "", m.loginRequired()
			}
			return c.AccessToken, nil
		}
		nc, err := m.refresh(ctx, "", false)
		if err != nil {
			if !m.expired(c) && !isInvalidGrant(err) {
				m.logf("warn", "refresh failed, using current token: %v", err)
				return c.AccessToken, nil
			}
			return "", fmt.Errorf("%w (refresh failed: %v)", m.loginRequired(), err)
		}
		if nc != nil {
			c = nc
		}
	}
	return c.AccessToken, nil
}

// discovery returns cached discovery, running it if needed.
func (m *Manager) discovery(ctx context.Context, fresh bool) (*discovery, error) {
	m.mu.Lock()
	d := m.disc
	www := m.lastWWW
	m.mu.Unlock()
	if d != nil && !fresh {
		return d, nil
	}
	d, err := m.discover(ctx, www)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.disc = d
	m.mu.Unlock()
	return d, nil
}

// refresh exchanges the refresh token. With force, it refreshes unless the
// stored access token already differs from failedToken; otherwise only if
// the token is still stale after re-reading the store under lock.
// Returns the updated credential (or the one another process stored).
func (m *Manager) refresh(ctx context.Context, failedToken string, force bool) (*Credential, error) {
	c, err := m.Status()
	if err != nil {
		return nil, err
	}
	if c == nil || c.RefreshToken == "" {
		return nil, errors.New("no refresh token")
	}
	tokenEndpoint := c.TokenEndpoint
	if tokenEndpoint == "" {
		d, err := m.discovery(ctx, false)
		if err != nil {
			return nil, err
		}
		tokenEndpoint = d.ASM.TokenEndpoint
	}

	var result *Credential
	var refreshErr error
	err = m.opts.Store.update(ctx, func(sd *storeData) (bool, error) {
		_, cur := sd.lookup(m.opts.Profile.URL)
		if cur == nil || cur.RefreshToken == "" {
			return false, errors.New("credentials were removed")
		}
		if force {
			if failedToken != "" && cur.AccessToken != failedToken && cur.AccessToken != "" {
				m.logf("info", "token already refreshed elsewhere")
				cp := *cur
				result = &cp
				return false, nil
			}
		} else if !m.stale(cur) {
			m.logf("info", "token already refreshed elsewhere")
			cp := *cur
			result = &cp
			return false, nil
		}
		form := map[string][]string{
			"grant_type":    {"refresh_token"},
			"refresh_token": {cur.RefreshToken},
		}
		if cur.Resource != "" {
			form["resource"] = []string{cur.Resource}
		}
		ep := cur.TokenEndpoint
		if ep == "" {
			ep = tokenEndpoint
		}
		ca := clientAuth{ClientID: cur.ClientID, ClientSecret: cur.ClientSecret, Method: cur.TokenAuthMethod}
		tr, err := m.tokenRequest(ctx, ep, form, ca)
		if err != nil {
			refreshErr = err
			if isInvalidGrant(err) {
				// The refresh token is dead; drop it so we don't retry it.
				cur.RefreshToken = ""
				return true, nil
			}
			return false, nil
		}
		m.applyTokens(cur, tr)
		if cur.TokenEndpoint == "" {
			cur.TokenEndpoint = ep
		}
		cp := *cur
		result = &cp
		m.logf("info", "refreshed access token (expires %s)", expiryText(cur.ExpiresAt))
		return true, nil // persisted before we return and the request is retried
	})
	if err != nil {
		return nil, err
	}
	if refreshErr != nil {
		return nil, m.clientRejectedHint(refreshErr)
	}
	return result, nil
}

func expiryText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// Login forces a full login with fresh discovery.
func (m *Manager) Login(ctx context.Context) error {
	m.mu.Lock()
	www := m.lastWWW
	m.mu.Unlock()
	return m.login(ctx, www, "\x00force")
}

// Logout revokes tokens (if the server supports revocation) and deletes them.
func (m *Manager) Logout(ctx context.Context) error {
	c, err := m.Status()
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	endpoint := c.RevocationEndpoint
	if endpoint == "" {
		if d, err := m.discovery(ctx, false); err == nil {
			endpoint = d.ASM.RevocationEndpoint
		} else {
			m.logf("warn", "discovery for revocation failed: %v", err)
		}
	}
	var revokeErr error
	if endpoint != "" {
		ca := clientAuth{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Method: c.TokenAuthMethod}
		if c.RefreshToken != "" {
			if err := m.revoke(ctx, endpoint, c.RefreshToken, "refresh_token", ca); err != nil {
				m.logf("warn", "revoke refresh token: %v", err)
				revokeErr = err
			} else {
				m.logf("info", "revoked refresh token")
			}
		}
		if c.AccessToken != "" {
			if err := m.revoke(ctx, endpoint, c.AccessToken, "access_token", ca); err != nil {
				m.logf("warn", "revoke access token: %v", err)
				revokeErr = err
			} else {
				m.logf("info", "revoked access token")
			}
		}
	} else {
		m.logf("info", "no revocation endpoint; deleting tokens locally")
	}
	err = m.opts.Store.update(ctx, func(sd *storeData) (bool, error) {
		sd.remove(m.opts.Profile.URL)
		return true, nil
	})
	if err != nil {
		return err
	}
	if revokeErr != nil {
		return fmt.Errorf("tokens deleted locally, but revocation failed: %w", revokeErr)
	}
	return nil
}

// ResetClient drops the cached dynamic registration for this profile's
// authorization server.
func (m *Manager) ResetClient() error {
	var issuers []string
	if c, err := m.Status(); err == nil && c != nil && c.AuthorizationServer != "" {
		issuers = append(issuers, normalizeURL(c.AuthorizationServer))
	}
	if len(issuers) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d, err := m.discovery(ctx, false)
		if err != nil {
			return err
		}
		issuers = append(issuers, d.issuerKey(), normalizeURL(d.AuthServer))
	}
	return m.opts.Store.update(context.Background(), func(sd *storeData) (bool, error) {
		changed := false
		for _, is := range issuers {
			if _, ok := sd.Clients[is]; ok {
				delete(sd.Clients, is)
				m.logf("info", "dropped cached client registration for %s", is)
				changed = true
			}
		}
		return changed, nil
	})
}

// Redact shortens a secret for display.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return "…"
	}
	return s[:6] + "…"
}
