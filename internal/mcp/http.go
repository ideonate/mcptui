package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Authorizer supplies bearer tokens and reacts to 401 responses.
type Authorizer interface {
	// Token returns the access token to send, or "" for none. It may refresh
	// proactively.
	Token(ctx context.Context) (string, error)
	// HandleUnauthorized is called on a 401. It should refresh or log in, and
	// return nil if the request should be retried.
	HandleUnauthorized(ctx context.Context, wwwAuthenticate string) error
}

// HTTPOptions configures an HTTPTransport.
type HTTPOptions struct {
	URL        string
	Headers    map[string]string
	Client     *http.Client
	Authorizer Authorizer
	Log        Logf
}

// HTTPTransport implements the streamable HTTP transport.
type HTTPTransport struct {
	opts HTTPOptions

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	closed          bool
	err             error

	msgs     chan []byte
	msgsMu   sync.RWMutex
	msgsDone bool
	done     chan struct{}
	closeOne sync.Once

	listenCancel context.CancelFunc
}

// NewHTTPTransport creates a streamable HTTP transport.
func NewHTTPTransport(opts HTTPOptions) *HTTPTransport {
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	return &HTTPTransport{
		opts: opts,
		msgs: make(chan []byte, 64),
		done: make(chan struct{}),
	}
}

func (t *HTTPTransport) Start(ctx context.Context) error { return nil }

func (t *HTTPTransport) Messages() <-chan []byte { return t.msgs }

func (t *HTTPTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// SessionID returns the current Mcp-Session-Id, if any.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// URL returns the endpoint URL.
func (t *HTTPTransport) URL() string { return t.opts.URL }

func (t *HTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

func (t *HTTPTransport) ResetSession() {
	t.mu.Lock()
	t.sessionID = ""
	t.protocolVersion = ""
	cancel := t.listenCancel
	t.listenCancel = nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *HTTPTransport) deliver(msg []byte) {
	t.msgsMu.RLock()
	defer t.msgsMu.RUnlock()
	if t.msgsDone {
		return
	}
	select {
	case t.msgs <- msg:
	case <-t.done:
	}
}

func (t *HTTPTransport) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.opts.URL, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	for k, v := range t.opts.Headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	if t.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocolVersion)
	}
	t.mu.Unlock()
	if t.opts.Authorizer != nil {
		tok, err := t.opts.Authorizer.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return req, nil
}

// do performs a request, handling a single 401 retry via the Authorizer.
func (t *HTTPTransport) do(ctx context.Context, method string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := t.newRequest(ctx, method, body)
		if err != nil {
			return nil, err
		}
		resp, err := t.opts.Client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && t.opts.Authorizer != nil && attempt == 0 {
			wa := resp.Header.Get("WWW-Authenticate")
			drain(resp)
			t.opts.Log.emit("auth", "info", "%s %s → 401 (WWW-Authenticate: %s)", method, t.opts.URL, wa)
			if err := t.opts.Authorizer.HandleUnauthorized(ctx, wa); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

func (t *HTTPTransport) Send(ctx context.Context, msg []byte) error {
	t.mu.Lock()
	closed := t.closed
	hadSession := t.sessionID != ""
	t.mu.Unlock()
	if closed {
		return errors.New("transport closed")
	}

	var head struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(msg, &head)

	resp, err := t.do(ctx, http.MethodPost, msg)
	if err != nil {
		return err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		if t.sessionID != sid {
			t.opts.Log.emit("transport", "debug", "session id %s", sid)
		}
		t.sessionID = sid
		t.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusNotFound && hadSession && head.Method != "initialize":
		drain(resp)
		t.opts.Log.emit("transport", "warn", "HTTP 404 with session id: session expired")
		return ErrSessionExpired
	case resp.StatusCode == http.StatusAccepted:
		drain(resp)
		return nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		return &HTTPError{
			StatusCode:      resp.StatusCode,
			Status:          resp.Status,
			Body:            strings.TrimSpace(string(b)),
			WWWAuthenticate: resp.Header.Get("WWW-Authenticate"),
		}
	}

	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "text/event-stream":
		go func() {
			defer resp.Body.Close()
			t.readStream(resp.Body)
		}()
		return nil
	default:
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			return nil
		}
		if ct != "application/json" && ct != "" {
			t.opts.Log.emit("transport", "warn", "unexpected content-type %q", ct)
		}
		// A JSON body may be a single message or a batch.
		if b[0] == '[' {
			var batch []json.RawMessage
			if err := json.Unmarshal(b, &batch); err != nil {
				return fmt.Errorf("invalid JSON batch: %w", err)
			}
			for _, m := range batch {
				t.deliver(m)
			}
			return nil
		}
		t.deliver(b)
		return nil
	}
}

func (t *HTTPTransport) readStream(r io.Reader) {
	err := readSSE(r, func(ev sseEvent) bool {
		if ev.Event != "" && ev.Event != "message" {
			return true
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			return true
		}
		t.deliver([]byte(data))
		select {
		case <-t.done:
			return false
		default:
			return true
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		select {
		case <-t.done:
		default:
			t.opts.Log.emit("transport", "debug", "event stream ended: %v", err)
		}
	}
}

// StartListening opens the optional GET stream for server-initiated
// messages. Servers that don't support it answer 405, which is fine.
func (t *HTTPTransport) StartListening() {
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	if t.closed || t.listenCancel != nil {
		t.mu.Unlock()
		cancel()
		return
	}
	t.listenCancel = cancel
	t.mu.Unlock()

	go func() {
		backoff := time.Second
		for attempt := 0; attempt < 5; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-t.done:
				return
			default:
			}
			resp, err := t.do(ctx, http.MethodGet, nil)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				t.opts.Log.emit("transport", "debug", "GET stream: %v", err)
			} else {
				ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
				if resp.StatusCode == http.StatusOK && ct == "text/event-stream" {
					t.opts.Log.emit("transport", "debug", "GET stream opened")
					attempt = 0
					backoff = time.Second
					t.readStream(resp.Body)
					_ = resp.Body.Close()
				} else {
					drain(resp)
					t.opts.Log.emit("transport", "debug", "server has no GET stream (HTTP %d)", resp.StatusCode)
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.done:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}()
}

// Close ends the session with DELETE (if we have one) and stops streams.
func (t *HTTPTransport) Close() error {
	t.closeOne.Do(func() {
		t.mu.Lock()
		t.closed = true
		sid := t.sessionID
		cancel := t.listenCancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if sid != "" {
			ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
			if req, err := t.newRequest(ctx, http.MethodDelete, nil); err == nil {
				if resp, err := t.opts.Client.Do(req); err == nil {
					drain(resp)
				}
			}
			c()
		}
		close(t.done)
		t.mu.Lock()
		if t.err == nil {
			t.err = errors.New("transport closed")
		}
		t.mu.Unlock()
		// deliver() unblocks on done, so taking the write lock cannot deadlock.
		t.msgsMu.Lock()
		t.msgsDone = true
		close(t.msgs)
		t.msgsMu.Unlock()
	})
	return nil
}
