package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/testserver"
)

func TestMain(m *testing.M) {
	switch os.Getenv("MCPTUI_TEST_STDIO") {
	case "serve":
		_ = testserver.New(testserver.Options{PageSize: 2}).ServeStdio(os.Stdin, os.Stdout)
		os.Exit(0)
	case "crash":
		fmt.Fprintln(os.Stderr, "boom: missing config")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func httpClient(t *testing.T, opts testserver.Options) (*mcp.Client, *testserver.Server) {
	t.Helper()
	srv := testserver.New(opts)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	tr := mcp.NewHTTPTransport(mcp.HTTPOptions{URL: hs.URL + "/mcp", Headers: map[string]string{"X-Test": "1"}})
	c := mcp.NewClient(tr, mcp.ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv
}

func TestHTTPJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			c, srv := httpClient(t, testserver.Options{SSE: sse, Sessions: true, PageSize: 2})
			ctx := context.Background()
			init := c.InitializeResult()
			if init.ServerInfo.Name != "testserver" || init.ProtocolVersion != "2025-06-18" {
				t.Fatalf("init = %+v", init)
			}
			tools, exs, err := c.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 5 || len(exs) != 3 {
				t.Fatalf("got %d tools over %d pages", len(tools), len(exs))
			}
			res, ex := c.CallTool(ctx, "add", json.RawMessage(`{"a":2,"b":3}`), nil)
			if ex.Err != nil {
				t.Fatal(ex.Err)
			}
			if string(res.StructuredContent) != `{"sum":5}` {
				t.Fatalf("structured = %s", res.StructuredContent)
			}
			if !strings.Contains(string(ex.Request), `"method":"tools/call"`) || !strings.Contains(string(ex.Response), `"result"`) {
				t.Fatalf("raw exchange missing: %s / %s", ex.Request, ex.Response)
			}
			if h := srv.LastHeaders; h.Get("MCP-Protocol-Version") != "2025-06-18" || h.Get("Mcp-Session-Id") == "" || h.Get("X-Test") != "1" {
				t.Fatalf("headers = %v", h)
			}
			if !strings.Contains(srv.LastHeaders.Get("Accept"), "text/event-stream") {
				t.Fatalf("accept = %q", srv.LastHeaders.Get("Accept"))
			}
			fail, ex := c.CallTool(ctx, "fail", nil, nil)
			if ex.Err != nil || !fail.IsError {
				t.Fatalf("fail: %v %+v", ex.Err, fail)
			}
			if ex := c.Request(ctx, "nope", nil, nil); ex.Err == nil {
				t.Fatal("expected rpc error")
			} else if re, ok := ex.Err.(*mcp.RPCError); !ok || re.Code != -32601 {
				t.Fatalf("err = %v", ex.Err)
			}
		})
	}
}

func TestSessionExpiryReinitializes(t *testing.T) {
	c, srv := httpClient(t, testserver.Options{Sessions: true})
	srv.ExpireSessions()
	if ex := c.Ping(context.Background()); ex.Err != nil {
		t.Fatalf("ping after expiry: %v", ex.Err)
	}
	if inits, _, _ := srv.Stats(); inits != 2 {
		t.Fatalf("inits = %d", inits)
	}
	c.Close()
	if _, deletes, _ := srv.Stats(); deletes != 1 {
		t.Fatalf("deletes = %d", deletes)
	}
}

func TestProgressAndCancel(t *testing.T) {
	c, srv := httpClient(t, testserver.Options{SSE: true})
	var mu sync.Mutex
	var got []mcp.ProgressParams
	_, ex := c.CallTool(context.Background(), "slow", json.RawMessage(`{"steps":3,"interval":10}`), &mcp.RequestOptions{
		OnProgress: func(p mcp.ProgressParams) { mu.Lock(); got = append(got, p); mu.Unlock() },
	})
	if ex.Err != nil {
		t.Fatal(ex.Err)
	}
	mu.Lock()
	if len(got) != 3 || got[2].Message != "step 3" {
		t.Fatalf("progress = %+v", got)
	}
	mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, ex = c.CallTool(ctx, "slow", json.RawMessage(`{"steps":100,"interval":20}`), nil)
	if ex.Err != context.Canceled {
		t.Fatalf("err = %v", ex.Err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, cancelled := srv.Stats(); len(cancelled) == 1 && cancelled[0] == fmt.Sprint(ex.ID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, cancelled := srv.Stats()
	t.Fatalf("server saw cancellations %v, want [%d]", cancelled, ex.ID)
}

func TestNotifications(t *testing.T) {
	srv := testserver.New(testserver.Options{SSE: true})
	hs := httptest.NewServer(srv)
	defer hs.Close()
	got := make(chan string, 4)
	c := mcp.NewClient(mcp.NewHTTPTransport(mcp.HTTPOptions{URL: hs.URL}), mcp.ClientOptions{
		OnNotification: func(method string, params json.RawMessage) { got <- method },
	})
	defer c.Close()
	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ex := c.CallTool(context.Background(), "echo", json.RawMessage(`{"message":"hi"}`), nil); ex.Err != nil {
		t.Fatal(ex.Err)
	}
	select {
	case m := <-got:
		if m != "notifications/message" {
			t.Fatalf("got %s", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
}

func TestStdio(t *testing.T) {
	exe, _ := os.Executable()
	tr := mcp.NewStdioTransport(mcp.StdioOptions{Command: []string{exe}, Env: map[string]string{"MCPTUI_TEST_STDIO": "serve"}})
	c := mcp.NewClient(tr, mcp.ClientOptions{})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	res, ex := c.CallTool(ctx, "echo", json.RawMessage(`{"message":"over stdio"}`), nil)
	if ex.Err != nil || res.Content[0].Text != "over stdio" {
		t.Fatalf("echo: %v %+v", ex.Err, res)
	}
	tmpls, _, err := c.ListResourceTemplates(ctx)
	if err != nil || len(tmpls) != 1 {
		t.Fatalf("templates: %v %v", err, tmpls)
	}
	rr, ex := c.ReadResource(ctx, "test://items/7")
	if ex.Err != nil || *rr.Contents[0].Text != `{"id":"7"}` {
		t.Fatalf("read: %v", ex.Err)
	}
}

func TestStdioCrashShowsStderr(t *testing.T) {
	exe, _ := os.Executable()
	var mu sync.Mutex
	var logs []mcp.Event
	tr := mcp.NewStdioTransport(mcp.StdioOptions{
		Command: []string{exe}, Env: map[string]string{"MCPTUI_TEST_STDIO": "crash"},
		Log: func(e mcp.Event) { mu.Lock(); logs = append(logs, e); mu.Unlock() },
	})
	c := mcp.NewClient(tr, mcp.ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Connect(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	<-c.Done()
	if !strings.Contains(tr.Err().Error(), "boom: missing config") {
		t.Fatalf("transport err = %v", tr.Err())
	}
}
