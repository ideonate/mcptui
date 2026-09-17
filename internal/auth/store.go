package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/paths"
)

// Credential is the stored OAuth state for one protected resource.
type Credential struct {
	Resource            string    `json:"resource"`
	MCPURL              string    `json:"mcp_url,omitempty"`
	AuthorizationServer string    `json:"authorization_server,omitempty"`
	ClientID            string    `json:"client_id,omitempty"`
	ClientSecret        string    `json:"client_secret,omitempty"`
	AccessToken         string    `json:"access_token,omitempty"`
	RefreshToken        string    `json:"refresh_token,omitempty"`
	Scope               string    `json:"scope,omitempty"`
	ExpiresAt           time.Time `json:"expires_at,omitzero"`
	TokenEndpoint       string    `json:"token_endpoint,omitempty"`
	RevocationEndpoint  string    `json:"revocation_endpoint,omitempty"`
	// TokenAuthMethod is how the client authenticates at the token endpoint
	// ("none", "client_secret_post" or "client_secret_basic").
	TokenAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
}

// clientRegistration is a cached dynamic client registration.
type clientRegistration struct {
	Issuer          string    `json:"issuer"`
	ClientID        string    `json:"client_id"`
	ClientSecret    string    `json:"client_secret,omitempty"`
	RedirectURIs    []string  `json:"redirect_uris,omitempty"`
	TokenAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	RegisteredAt    time.Time `json:"registered_at"`
}

// storeData is the on-disk format of credentials.json.
type storeData struct {
	Credentials map[string]*Credential `json:"credentials,omitempty"`
	// Aliases maps an MCP URL to the resource key it was stored under.
	Aliases map[string]string `json:"aliases,omitempty"`
	// Clients caches dynamic registrations keyed by normalised issuer.
	Clients map[string]*clientRegistration `json:"clients,omitempty"`
}

func (d *storeData) init() {
	if d.Credentials == nil {
		d.Credentials = map[string]*Credential{}
	}
	if d.Aliases == nil {
		d.Aliases = map[string]string{}
	}
	if d.Clients == nil {
		d.Clients = map[string]*clientRegistration{}
	}
}

// lookup finds the credential for an MCP URL.
func (d *storeData) lookup(mcpURL string) (string, *Credential) {
	key := normalizeURL(mcpURL)
	if res, ok := d.Aliases[key]; ok {
		if c := d.Credentials[res]; c != nil {
			return res, c
		}
	}
	if c := d.Credentials[key]; c != nil {
		return key, c
	}
	return "", nil
}

func (d *storeData) put(c *Credential) {
	key := normalizeURL(c.Resource)
	if key == "" {
		key = normalizeURL(c.MCPURL)
	}
	d.Credentials[key] = c
	if c.MCPURL != "" {
		d.Aliases[normalizeURL(c.MCPURL)] = key
	}
}

func (d *storeData) remove(mcpURL string) {
	key, _ := d.lookup(mcpURL)
	if key != "" {
		delete(d.Credentials, key)
	}
	for alias, res := range d.Aliases {
		if res == key || alias == normalizeURL(mcpURL) {
			delete(d.Aliases, alias)
		}
	}
}

// Store persists credentials in a JSON file (mode 0600, dir 0700), guarded by
// an in-process mutex and a cross-process lock file.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a store at path ("" = default credentials file).
func NewStore(path string) *Store {
	if path == "" {
		path = paths.CredentialsFile()
	}
	return &Store{path: path}
}

// Path returns the credentials file path.
func (s *Store) Path() string { return s.path }

func (s *Store) read() (*storeData, error) {
	d := &storeData{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		d.init()
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, d); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	d.init()
	return d, nil
}

func (s *Store) write(d *storeData) error {
	if err := paths.EnsureDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.path, b, 0o600)
}

// load reads a snapshot without taking the file lock.
func (s *Store) load() (*storeData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// update runs fn with exclusive access (process mutex + file lock), writing
// the data back if fn returns changed=true.
func (s *Store) update(ctx context.Context, fn func(d *storeData) (changed bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := paths.EnsureDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	lk := flock.New(s.path+".lock", flock.SetPermissions(0o600))
	defer lk.Close()
	if ctx == nil {
		ctx = context.Background()
	}
	ok, err := lk.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	if !ok {
		return errors.New("lock credentials: not acquired")
	}
	defer lk.Unlock()
	d, err := s.read()
	if err != nil {
		return err
	}
	changed, err := fn(d)
	if changed {
		if werr := s.write(d); werr != nil {
			return werr
		}
	}
	return err
}

// normalizeURL trims trailing slashes, for comparisons and map keys.
func normalizeURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}
