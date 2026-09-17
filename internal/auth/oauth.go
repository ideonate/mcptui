package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// OAuthError is an error response from an OAuth endpoint.
type OAuthError struct {
	Endpoint    string
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	msg := e.Code
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.Status)
	}
	if e.Description != "" {
		msg += ": " + e.Description
	}
	return fmt.Sprintf("%s: %s", e.Endpoint, msg)
}

// ErrClientRejected is wrapped by errors indicating the authorization server
// rejected our client (invalid_client, unregistered redirect URI). Suggest
// `mcptui auth reset-client`.
var ErrClientRejected = errors.New("client rejected by authorization server")

func isInvalidGrant(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_grant"
}

// clientRejectedHint wraps err with ErrClientRejected if it looks like the
// server no longer accepts our client registration.
func (m *Manager) clientRejectedHint(err error) error {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return err
	}
	desc := strings.ToLower(oe.Description)
	if oe.Code == "invalid_client" || oe.Code == "unauthorized_client" ||
		strings.Contains(desc, "redirect") || strings.Contains(desc, "client") && strings.Contains(desc, "unknown") {
		return fmt.Errorf("%w (%w); run `mcptui auth reset-client %s` to register again", err, ErrClientRejected, m.opts.Profile.Name)
	}
	return err
}

func randomString(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkcePair returns a verifier and its S256 challenge.
func pkcePair() (verifier, challenge string) {
	verifier = randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	TokenType    string  `json:"token_type"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    flexInt `json:"expires_in"`
	Scope        string  `json:"scope"`
}

// flexInt accepts numbers or numeric strings.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

// clientAuth describes how to authenticate the client at the token endpoint.
type clientAuth struct {
	ClientID     string
	ClientSecret string
	Method       string // none, client_secret_post, client_secret_basic
}

func (ca clientAuth) apply(form url.Values, req *http.Request) {
	if ca.ClientSecret != "" && ca.Method == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(ca.ClientID), url.QueryEscape(ca.ClientSecret))
		return
	}
	form.Set("client_id", ca.ClientID)
	if ca.ClientSecret != "" {
		form.Set("client_secret", ca.ClientSecret)
	}
}

func (m *Manager) postForm(ctx context.Context, endpoint string, form url.Values, ca clientAuth) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	ca.apply(form, req)
	body := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

func parseOAuthError(endpoint string, status int, body []byte) error {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	oe := &OAuthError{Endpoint: endpoint, Status: status, Code: e.Error, Description: e.ErrorDescription}
	if oe.Code == "" && oe.Description == "" {
		s := strings.TrimSpace(string(body))
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		oe.Description = s
	}
	return oe
}

func (m *Manager) tokenRequest(ctx context.Context, endpoint string, form url.Values, ca clientAuth) (*tokenResponse, error) {
	b, status, err := m.postForm(ctx, endpoint, form, ca)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	if status != http.StatusOK {
		return nil, parseOAuthError("token endpoint", status, b)
	}
	var tr tokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		// Some servers answer form-encoded.
		if v, perr := url.ParseQuery(string(b)); perr == nil && v.Get("access_token") != "" {
			tr.AccessToken = v.Get("access_token")
			tr.RefreshToken = v.Get("refresh_token")
			tr.Scope = v.Get("scope")
			tr.TokenType = v.Get("token_type")
			if n, err := strconv.ParseInt(v.Get("expires_in"), 10, 64); err == nil {
				tr.ExpiresIn = flexInt(n)
			}
		} else {
			return nil, fmt.Errorf("token endpoint: invalid response: %w", err)
		}
	}
	if tr.AccessToken == "" {
		if e := parseOAuthError("token endpoint", status, b); e.(*OAuthError).Code != "" {
			return nil, e
		}
		return nil, errors.New("token endpoint: response has no access_token")
	}
	return &tr, nil
}

// applyTokens copies a token response into a credential.
func (m *Manager) applyTokens(c *Credential, tr *tokenResponse) {
	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.Scope != "" {
		c.Scope = tr.Scope
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = m.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		c.ExpiresAt = time.Time{}
	}
}

// register performs RFC 7591 dynamic client registration.
func (m *Manager) register(ctx context.Context, d *discovery, redirectURIs []string) (*clientRegistration, error) {
	body := map[string]any{
		"client_name":                clientName(),
		"redirect_uris":              redirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if s := m.opts.Profile.Scope; s != "" {
		body["scope"] = s
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ASM.RegistrationEndpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("client registration: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		err := parseOAuthError("registration endpoint", resp.StatusCode, b)
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w (registration is rate limited; try again later)", err)
		}
		return nil, err
	}
	var rr struct {
		ClientID                string   `json:"client_id"`
		ClientSecret            string   `json:"client_secret"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(b, &rr); err != nil || rr.ClientID == "" {
		return nil, fmt.Errorf("registration endpoint: invalid response (%s)", strings.TrimSpace(string(b)))
	}
	reg := &clientRegistration{
		Issuer:          d.issuerKey(),
		ClientID:        rr.ClientID,
		ClientSecret:    rr.ClientSecret,
		RedirectURIs:    redirectURIs,
		TokenAuthMethod: rr.TokenEndpointAuthMethod,
		RegisteredAt:    m.now(),
	}
	if len(rr.RedirectURIs) > 0 {
		reg.RedirectURIs = rr.RedirectURIs
	}
	if reg.TokenAuthMethod == "" {
		reg.TokenAuthMethod = "none"
		if reg.ClientSecret != "" {
			reg.TokenAuthMethod = "client_secret_post"
		}
	}
	m.logf("info", "registered client %s at %s", reg.ClientID, d.ASM.RegistrationEndpoint)
	return reg, nil
}

func clientName() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if name == "" {
		name = "user"
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("mcptui (%s@%s)", name, host)
}

// revoke calls the RFC 7009 revocation endpoint.
func (m *Manager) revoke(ctx context.Context, endpoint, token, hint string, ca clientAuth) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}}
	b, status, err := m.postForm(ctx, endpoint, form, ca)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return parseOAuthError("revocation endpoint", status, b)
	}
	return nil
}
