// Package session builds a connected MCP client from a profile.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/paths"
)

// Version is the mcptui version, set by main.
var Version = "dev"

// Options configure Open.
type Options struct {
	Profile         *config.Profile
	CredentialsPath string // "" = default
	Interactive     bool
	NoBrowser       bool
	CallbackHost    string
	LoginUI         auth.LoginUI
	Log             mcp.Logf
	OnNotification  func(method string, params json.RawMessage)
	OnTraffic       func(mcp.Traffic)
	OnServerRequest func(method string, params, reply json.RawMessage)
	HTTPClient      *http.Client
}

// Session is a profile plus its client.
type Session struct {
	Profile *config.Profile
	Client  *mcp.Client
	Auth    *auth.Manager      // nil unless auth = oauth over HTTP
	HTTP    *mcp.HTTPTransport // nil for stdio
	Stdio   *mcp.StdioTransport
}

// NewAuthManager builds the OAuth manager for a profile.
func NewAuthManager(p *config.Profile, o Options) *auth.Manager {
	path := o.CredentialsPath
	if path == "" {
		path = paths.CredentialsFile()
	}
	return auth.NewManager(auth.Options{
		Profile:      p,
		Store:        auth.NewStore(path),
		HTTPClient:   o.HTTPClient,
		CallbackHost: o.CallbackHost,
		NoBrowser:    o.NoBrowser,
		Interactive:  o.Interactive,
		UI:           o.LoginUI,
		Log:          o.Log,
	})
}

// New builds (but does not connect) a session.
func New(o Options) (*Session, error) {
	p := o.Profile
	if err := p.Validate(); err != nil {
		return nil, err
	}
	s := &Session{Profile: p}
	var tr mcp.Transport
	switch p.Transport {
	case "stdio":
		env, err := p.ResolvedEnv()
		if err != nil {
			return nil, err
		}
		s.Stdio = mcp.NewStdioTransport(mcp.StdioOptions{Command: p.Command, Dir: p.Cwd, Env: env, Log: o.Log})
		tr = s.Stdio
	default:
		headers, err := p.ResolvedHeaders()
		if err != nil {
			return nil, err
		}
		hopts := mcp.HTTPOptions{URL: p.URL, Headers: headers, Client: o.HTTPClient, Log: o.Log}
		switch p.Auth {
		case config.AuthBearer:
			tok, err := p.ResolvedToken()
			if err != nil {
				return nil, err
			}
			if headers == nil {
				headers = map[string]string{}
			}
			headers["Authorization"] = "Bearer " + tok
			hopts.Headers = headers
		case config.AuthOAuth:
			s.Auth = NewAuthManager(p, o)
			hopts.Authorizer = s.Auth
		}
		s.HTTP = mcp.NewHTTPTransport(hopts)
		tr = s.HTTP
	}
	s.Client = mcp.NewClient(tr, mcp.ClientOptions{
		ClientInfo:      mcp.Implementation{Name: "mcptui", Version: Version},
		Log:             o.Log,
		OnNotification:  o.OnNotification,
		OnTraffic:       o.OnTraffic,
		OnServerRequest: o.OnServerRequest,
	})
	return s, nil
}

// Connect initializes the session.
func (s *Session) Connect(ctx context.Context) (*mcp.Exchange, error) {
	ex, err := s.Client.Connect(ctx)
	if err != nil {
		return ex, fmt.Errorf("connect to %s: %w", s.Profile.Target(), err)
	}
	return ex, nil
}

// Open builds and connects.
func Open(ctx context.Context, o Options) (*Session, error) {
	s, err := New(o)
	if err != nil {
		return nil, err
	}
	if _, err := s.Connect(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// SessionID returns the HTTP session id, if any.
func (s *Session) SessionID() string {
	if s.HTTP != nil {
		return s.HTTP.SessionID()
	}
	return ""
}

// Close ends the session.
func (s *Session) Close() error {
	if s.Client != nil {
		return s.Client.Close()
	}
	return nil
}
