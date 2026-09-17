// Package testserver is a small in-process MCP server used as a test fixture.
// It supports streamable HTTP (JSON or SSE responses, sessions, GET stream)
// and stdio.
package testserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options tune fixture behaviour.
type Options struct {
	// SSE makes POST responses to requests use text/event-stream.
	SSE bool
	// Sessions issues Mcp-Session-Id and enforces it.
	Sessions bool
	// PageSize paginates list results (0 = no pagination).
	PageSize int
	// RequireToken, when set, demands "Authorization: Bearer <token>".
	RequireToken string
}

// Server is the fixture.
type Server struct {
	opts Options

	mu        sync.Mutex
	sessions  map[string]bool
	cancelled map[string]chan struct{}
	gotCancel []string
	deletes   int
	inits     int
	nextSess  atomic.Int64

	// Headers seen on the last POST, for assertions.
	LastHeaders http.Header
}

// New creates a fixture server.
func New(opts Options) *Server {
	return &Server{opts: opts, sessions: map[string]bool{}, cancelled: map[string]chan struct{}{}}
}

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

// ExpireSessions forgets all sessions so the next request gets 404.
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	s.sessions = map[string]bool{}
	s.mu.Unlock()
}

// Stats returns counters for assertions.
func (s *Server) Stats() (inits, deletes int, cancelled []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inits, s.deletes, append([]string(nil), s.gotCancel...)
}

var tools = []map[string]any{
	{
		"name": "echo", "description": "Echo back the **message**.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"message": map[string]any{"type": "string", "description": "Text to echo"},
		}, "required": []string{"message"}},
		"annotations": map[string]any{"readOnlyHint": true},
	},
	{
		"name": "add", "description": "Add two numbers.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"},
		}, "required": []string{"a", "b"}},
		"outputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"sum": map[string]any{"type": "number"},
		}, "required": []string{"sum"}},
		"annotations": map[string]any{"readOnlyHint": true, "idempotentHint": true},
	},
	{
		"name": "slow", "description": "Reports progress for `steps` steps.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"steps":    map[string]any{"type": "integer", "default": 5},
			"interval": map[string]any{"type": "integer", "description": "ms per step", "default": 50},
		}},
	},
	{
		"name": "fail", "description": "Always returns isError.",
		"inputSchema": map[string]any{"type": "object"},
	},
	{
		"name": "delete_everything", "description": "Pretends to delete things.",
		"inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"destructiveHint": true, "readOnlyHint": false},
	},
}

var prompts = []map[string]any{
	{"name": "greet", "description": "Greets someone", "arguments": []map[string]any{
		{"name": "name", "description": "Who to greet", "required": true},
	}},
}

var resources = []map[string]any{
	{"uri": "test://readme", "name": "readme", "mimeType": "text/markdown"},
	{"uri": "test://data.json", "name": "data", "mimeType": "application/json"},
	{"uri": "test://blob", "name": "blob", "mimeType": "application/octet-stream"},
}

var templates = []map[string]any{
	{"uriTemplate": "test://items/{id}", "name": "item", "mimeType": "application/json"},
}

func (s *Server) page(items []map[string]any, key string, params json.RawMessage) map[string]any {
	if s.opts.PageSize <= 0 {
		return map[string]any{key: items}
	}
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	start, _ := strconv.Atoi(p.Cursor)
	end := min(start+s.opts.PageSize, len(items))
	res := map[string]any{key: items[start:end]}
	if end < len(items) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	return res
}

func text(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }

// handle processes one request. notify sends a notification tied to the
// request (progress, logging). It returns result or rpc error.
func (s *Server) handle(ctx context.Context, m msg, notify func(method string, params any)) (any, any) {
	switch m.Method {
	case "initialize":
		s.mu.Lock()
		s.inits++
		s.mu.Unlock()
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": true}, "prompts": map[string]any{},
				"resources": map[string]any{"subscribe": true}, "logging": map[string]any{},
				"completions": map[string]any{},
			},
			"serverInfo":   map[string]any{"name": "testserver", "version": "1.2.3"},
			"instructions": "Use **echo** to test.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.page(tools, "tools", m.Params), nil
	case "prompts/list":
		return s.page(prompts, "prompts", m.Params), nil
	case "resources/list":
		return s.page(resources, "resources", m.Params), nil
	case "resources/templates/list":
		return s.page(templates, "resourceTemplates", m.Params), nil
	case "resources/subscribe", "resources/unsubscribe", "logging/setLevel":
		return map[string]any{}, nil
	case "resources/read":
		var p struct{ URI string }
		_ = json.Unmarshal(m.Params, &p)
		switch {
		case p.URI == "test://readme":
			return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "text/markdown", "text": "# Readme\n\nHello *world*."}}}, nil
		case p.URI == "test://data.json":
			return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "application/json", "text": `{"a":1,"b":[1,2]}`}}}, nil
		case p.URI == "test://blob":
			return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "application/octet-stream", "blob": "AAECAw=="}}}, nil
		case strings.HasPrefix(p.URI, "test://items/"):
			id := strings.TrimPrefix(p.URI, "test://items/")
			return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "application/json", "text": fmt.Sprintf(`{"id":%q}`, id)}}}, nil
		}
		return nil, map[string]any{"code": -32002, "message": "resource not found"}
	case "prompts/get":
		var p struct {
			Name      string
			Arguments map[string]string
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Name != "greet" {
			return nil, map[string]any{"code": -32602, "message": "unknown prompt"}
		}
		return map[string]any{"description": "greeting", "messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": "Please greet " + p.Arguments["name"]}},
			{"role": "assistant", "content": map[string]any{"type": "text", "text": "Hello, " + p.Arguments["name"] + "!"}},
		}}, nil
	case "completion/complete":
		var p struct {
			Argument struct{ Name, Value string }
		}
		_ = json.Unmarshal(m.Params, &p)
		var vals []string
		for _, v := range []string{"alice", "bob", "carol"} {
			if strings.HasPrefix(v, p.Argument.Value) {
				vals = append(vals, v)
			}
		}
		return map[string]any{"completion": map[string]any{"values": vals}}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch p.Name {
		case "echo":
			var a struct{ Message string }
			_ = json.Unmarshal(p.Arguments, &a)
			notify("notifications/message", map[string]any{"level": "info", "data": "echoing"})
			return map[string]any{"content": text(a.Message)}, nil
		case "add":
			var a struct{ A, B float64 }
			_ = json.Unmarshal(p.Arguments, &a)
			sum := a.A + a.B
			return map[string]any{
				"content":           text(strconv.FormatFloat(sum, 'f', -1, 64)),
				"structuredContent": map[string]any{"sum": sum},
			}, nil
		case "fail":
			return map[string]any{"content": text("it failed"), "isError": true}, nil
		case "delete_everything":
			return map[string]any{"content": text("deleted nothing")}, nil
		case "slow":
			a := struct {
				Steps    int `json:"steps"`
				Interval int `json:"interval"`
			}{5, 50}
			_ = json.Unmarshal(p.Arguments, &a)
			for i := 1; i <= a.Steps; i++ {
				select {
				case <-ctx.Done():
					return nil, nil
				case <-time.After(time.Duration(a.Interval) * time.Millisecond):
				}
				if p.Meta.ProgressToken != nil {
					notify("notifications/progress", map[string]any{
						"progressToken": p.Meta.ProgressToken, "progress": i, "total": a.Steps,
						"message": fmt.Sprintf("step %d", i),
					})
				}
			}
			return map[string]any{"content": text("done")}, nil
		}
		return nil, map[string]any{"code": -32602, "message": "unknown tool " + p.Name}
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + m.Method}
}

func (s *Server) onNotification(m msg) {
	if m.Method == "notifications/cancelled" {
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		key := string(p.RequestID)
		s.mu.Lock()
		s.gotCancel = append(s.gotCancel, key)
		if ch, ok := s.cancelled[key]; ok {
			close(ch)
			delete(s.cancelled, key)
		}
		s.mu.Unlock()
	}
}

// requestContext returns a context cancelled by notifications/cancelled.
func (s *Server) requestContext(parent context.Context, id json.RawMessage) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan struct{})
	key := string(id)
	s.mu.Lock()
	s.cancelled[key] = ch
	s.mu.Unlock()
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.cancelled, key)
		s.mu.Unlock()
	}
}

// ServeHTTP implements streamable HTTP.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.opts.RequireToken != "" && r.Header.Get("Authorization") != "Bearer "+s.opts.RequireToken {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodDelete:
		s.mu.Lock()
		s.deletes++
		delete(s.sessions, sid)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		http.Error(w, "no GET stream", http.StatusMethodNotAllowed)
		return
	case http.MethodPost:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m msg
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.LastHeaders = r.Header.Clone()
	s.mu.Unlock()
	if s.opts.Sessions && m.Method != "initialize" {
		s.mu.Lock()
		ok := s.sessions[sid]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	}
	isRequest := len(m.ID) > 0
	if !isRequest {
		if m.Method != "" {
			s.onNotification(m)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if s.opts.Sessions && m.Method == "initialize" {
		id := fmt.Sprintf("sess-%d", s.nextSess.Add(1))
		s.mu.Lock()
		s.sessions[id] = true
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
	}

	ctx, done := s.requestContext(r.Context(), m.ID)
	defer done()

	if !s.opts.SSE {
		res, rpcErr := s.handle(ctx, m, func(string, any) {})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply(m.ID, res, rpcErr))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	res, rpcErr := s.handle(ctx, m, func(method string, params any) {
		write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	})
	if ctx.Err() != nil {
		return
	}
	write(reply(m.ID, res, rpcErr))
}

func reply(id json.RawMessage, res, rpcErr any) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = res
	}
	return out
}

// ServeStdio speaks newline-delimited JSON-RPC until in closes.
func (s *Server) ServeStdio(in io.Reader, out io.Writer) error {
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		out.Write(append(b, '\n'))
	}
	rd := bufio.NewScanner(in)
	rd.Buffer(make([]byte, 0, 1<<16), 1<<24)
	var wg sync.WaitGroup
	for rd.Scan() {
		var m msg
		if err := json.Unmarshal(rd.Bytes(), &m); err != nil {
			continue
		}
		if len(m.ID) == 0 {
			if m.Method != "" {
				s.onNotification(m)
			}
			continue
		}
		if m.Method == "" {
			continue // response to something we never sent
		}
		wg.Add(1)
		go func(m msg) {
			defer wg.Done()
			ctx, done := s.requestContext(context.Background(), m.ID)
			defer done()
			res, rpcErr := s.handle(ctx, m, func(method string, params any) {
				write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
			})
			if ctx.Err() != nil {
				return
			}
			write(reply(m.ID, res, rpcErr))
		}(m)
	}
	wg.Wait()
	return rd.Err()
}
