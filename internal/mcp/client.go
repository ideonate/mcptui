package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Direction of a traffic record.
type Direction string

const (
	Outbound Direction = "→"
	Inbound  Direction = "←"
)

// Traffic is one raw JSON-RPC message seen on the wire.
type Traffic struct {
	Time      time.Time
	Direction Direction
	Raw       json.RawMessage
}

// ClientOptions configures a Client.
type ClientOptions struct {
	ClientInfo Implementation
	Log        Logf
	// OnNotification receives every server notification except progress for
	// requests that registered a progress handler.
	OnNotification func(method string, params json.RawMessage)
	// OnTraffic observes every raw message.
	OnTraffic func(Traffic)
	// OnServerRequest observes requests the server sends to the client
	// (sampling, elicitation, roots, ...) together with the reply sent.
	OnServerRequest func(method string, params, reply json.RawMessage)
}

// Exchange is a completed (or failed) request with its raw messages.
type Exchange struct {
	ID       int64
	Method   string
	Params   json.RawMessage
	Request  json.RawMessage
	Response json.RawMessage
	Result   json.RawMessage
	Err      error
	Started  time.Time
	Duration time.Duration
}

// Decode unmarshals the result into v, returning the exchange error first.
func (e *Exchange) Decode(v any) error {
	if e.Err != nil {
		return e.Err
	}
	if err := json.Unmarshal(e.Result, v); err != nil {
		return fmt.Errorf("decode %s result: %w", e.Method, err)
	}
	return nil
}

// RequestOptions tweak a single request.
type RequestOptions struct {
	// OnProgress, when set, adds a progress token and receives progress
	// notifications for this request.
	OnProgress func(ProgressParams)
}

type inbound struct {
	raw json.RawMessage
	msg rpcMessage
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Client is an MCP client over a Transport.
type Client struct {
	t    Transport
	opts ClientOptions

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan inbound
	progress map[string]func(ProgressParams)
	init     *InitializeResult
	initGen  int
	closed   bool
	readDone chan struct{}

	initMu sync.Mutex
}

// NewClient wraps a transport. Call Connect next.
func NewClient(t Transport, opts ClientOptions) *Client {
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = Implementation{Name: "mcptui", Version: "dev"}
	}
	return &Client{
		t:        t,
		opts:     opts,
		pending:  map[string]chan inbound{},
		progress: map[string]func(ProgressParams){},
		readDone: make(chan struct{}),
	}
}

// Transport returns the underlying transport.
func (c *Client) Transport() Transport { return c.t }

// Done is closed when the transport's message stream ends.
func (c *Client) Done() <-chan struct{} { return c.readDone }

// InitializeResult returns the last successful initialize result.
func (c *Client) InitializeResult() *InitializeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init
}

// Connect starts the transport and performs the initialize handshake.
func (c *Client) Connect(ctx context.Context) (*Exchange, error) {
	if err := c.t.Start(ctx); err != nil {
		return nil, err
	}
	go c.readLoop()
	ex := c.initialize(ctx, -1)
	return ex, ex.Err
}

func (c *Client) initialize(ctx context.Context, seenGen int) *Exchange {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	c.mu.Lock()
	gen := c.initGen
	c.mu.Unlock()
	if seenGen >= 0 && gen != seenGen {
		// Someone else re-initialized while we waited.
		return &Exchange{Method: "initialize"}
	}
	if seenGen >= 0 {
		c.opts.Log.emit("client", "info", "re-initializing expired session")
		if r, ok := c.t.(SessionResetter); ok {
			r.ResetSession()
		}
	}
	params := map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      c.opts.ClientInfo,
	}
	ex := c.send(ctx, "initialize", params, nil, false)
	if ex.Err != nil {
		return ex
	}
	var res InitializeResult
	if err := json.Unmarshal(ex.Result, &res); err != nil {
		ex.Err = fmt.Errorf("decode initialize result: %w", err)
		return ex
	}
	var capsOnly struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(ex.Result, &capsOnly)
	res.RawCapabilities = capsOnly.Capabilities
	if s, ok := c.t.(ProtocolVersionSetter); ok {
		s.SetProtocolVersion(res.ProtocolVersion)
	}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		ex.Err = fmt.Errorf("send initialized: %w", err)
		return ex
	}
	c.mu.Lock()
	c.init = &res
	c.initGen++
	c.mu.Unlock()
	c.opts.Log.emit("client", "info", "initialized: %s %s (protocol %s)", res.ServerInfo.Name, res.ServerInfo.Version, res.ProtocolVersion)
	if l, ok := c.t.(SessionStarter); ok {
		l.StartListening()
	}
	return ex
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.observe(Outbound, raw)
	return c.t.Send(ctx, raw)
}

// Request sends a request and waits for its response. It never returns nil.
func (c *Client) Request(ctx context.Context, method string, params any, opts *RequestOptions) *Exchange {
	return c.send(ctx, method, params, opts, true)
}

func (c *Client) send(ctx context.Context, method string, params any, opts *RequestOptions, allowReinit bool) *Exchange {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ex := &Exchange{ID: id, Method: method, Started: time.Now()}
	finish := func(err error) *Exchange {
		ex.Err = err
		ex.Duration = time.Since(ex.Started)
		return ex
	}

	// Build params as an object so we can inject _meta.progressToken.
	var p map[string]any
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return finish(err)
		}
		if !bytes.Equal(b, []byte("null")) {
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			if err := d.Decode(&p); err != nil {
				return finish(fmt.Errorf("params must be a JSON object: %w", err))
			}
		}
	}
	if opts != nil && opts.OnProgress != nil {
		if p == nil {
			p = map[string]any{}
		}
		meta, _ := p["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["progressToken"] = key
		p["_meta"] = meta
		c.mu.Lock()
		c.progress[key] = opts.OnProgress
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			delete(c.progress, key)
			c.mu.Unlock()
		}()
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if p != nil {
		msg["params"] = p
		ex.Params, _ = json.Marshal(p)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return finish(err)
	}
	ex.Request = raw

	ch := make(chan inbound, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return finish(c.transportErr())
	}
	c.pending[key] = ch
	gen := c.initGen
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	c.observe(Outbound, raw)
	err = c.t.Send(ctx, raw)
	if errors.Is(err, ErrSessionExpired) && allowReinit {
		if iex := c.initialize(ctx, gen); iex.Err != nil {
			return finish(fmt.Errorf("session expired and re-initialize failed: %w", iex.Err))
		}
		c.observe(Outbound, raw)
		err = c.t.Send(ctx, raw)
	}
	if err != nil {
		if ctx.Err() != nil {
			// The request may already be running on the server.
			c.sendCancel(id, ctx.Err())
			return finish(ctx.Err())
		}
		return finish(err)
	}

	select {
	case in, ok := <-ch:
		if !ok {
			return finish(c.transportErr())
		}
		ex.Response = in.raw
		if in.msg.Error != nil {
			return finish(in.msg.Error)
		}
		ex.Result = in.msg.Result
		return finish(nil)
	case <-ctx.Done():
		c.sendCancel(id, ctx.Err())
		return finish(ctx.Err())
	}
}

func (c *Client) sendCancel(id int64, cause error) {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reason := "cancelled by user"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "timeout"
	}
	if err := c.Notify(cctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": reason}); err != nil {
		c.opts.Log.emit("client", "warn", "send cancellation: %v", err)
	}
}

func (c *Client) transportErr() error {
	if err := c.t.Err(); err != nil {
		return err
	}
	return errors.New("connection closed")
}

func (c *Client) observe(d Direction, raw []byte) {
	if c.opts.OnTraffic != nil {
		c.opts.OnTraffic(Traffic{Time: time.Now(), Direction: d, Raw: append(json.RawMessage(nil), raw...)})
	}
}

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closed = true
		for k, ch := range c.pending {
			close(ch)
			delete(c.pending, k)
		}
		c.mu.Unlock()
		close(c.readDone)
	}()
	for raw := range c.t.Messages() {
		c.observe(Inbound, raw)
		var msg rpcMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.opts.Log.emit("transport", "warn", "invalid JSON-RPC message: %v", err)
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case hasID && msg.Method != "":
			c.handleServerRequest(msg)
		case hasID:
			key := idKey(msg.ID)
			c.mu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch == nil {
				c.opts.Log.emit("client", "debug", "response for unknown request id %s", key)
				continue
			}
			ch <- inbound{raw: raw, msg: msg}
		case msg.Method != "":
			c.handleNotification(msg)
		}
	}
}

func idKey(id json.RawMessage) string {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	if d.Decode(&n) == nil {
		return n.String()
	}
	return string(id)
}

func (c *Client) handleNotification(msg rpcMessage) {
	if msg.Method == "notifications/progress" {
		var pp ProgressParams
		if json.Unmarshal(msg.Params, &pp) == nil {
			key := fmt.Sprint(pp.ProgressToken)
			if f, ok := pp.ProgressToken.(float64); ok {
				key = strconv.FormatFloat(f, 'f', -1, 64)
			}
			c.mu.Lock()
			fn := c.progress[key]
			c.mu.Unlock()
			if fn != nil {
				fn(pp)
				return
			}
		}
	}
	if c.opts.OnNotification != nil {
		c.opts.OnNotification(msg.Method, msg.Params)
	}
}

func (c *Client) handleServerRequest(msg rpcMessage) {
	resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if msg.Method == "ping" {
		resp["result"] = map[string]any{}
	} else {
		c.opts.Log.emit("server", "warn", "server request %s is not supported; replying with an error", msg.Method)
		resp["error"] = RPCError{Code: -32601, Message: "mcptui does not support " + msg.Method}
	}
	raw, _ := json.Marshal(resp)
	c.observe(Outbound, raw)
	if c.opts.OnServerRequest != nil && msg.Method != "ping" {
		c.opts.OnServerRequest(msg.Method, msg.Params, raw)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.t.Send(ctx, raw); err != nil {
			c.opts.Log.emit("client", "warn", "reply to %s: %v", msg.Method, err)
		}
	}()
}

// Close shuts down the transport.
func (c *Client) Close() error { return c.t.Close() }
