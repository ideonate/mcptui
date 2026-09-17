package tui

import (
	"encoding/json"
	"errors"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/testserver"
)

var update = flag.Bool("update", false, "update golden files")

// harness drives an App without a real terminal.
type harness struct {
	t    *testing.T
	a    *App
	msgs chan tea.Msg
	srv  *testserver.Server
}

func newHarness(t *testing.T, prof func(p *config.Profile), sopts testserver.Options) *harness {
	t.Helper()
	srv := testserver.New(sopts)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	p := config.TemporaryURL(hs.URL + "/mcp")
	p.Name = "test"
	p.Auth = config.AuthNone
	if prof != nil {
		prof(p)
	}
	dark := true
	dir := t.TempDir()
	a := New(Options{Profile: p, NoHistory: true, CredentialsPath: filepath.Join(dir, "creds.json"), Dark: &dark})
	h := &harness{t: t, a: a, msgs: make(chan tea.Msg, 256), srv: srv}
	t.Cleanup(func() {
		if a.sess != nil {
			a.sess.Close()
		}
	})
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.run(a.Init())
	h.waitFor("connected and lists loaded", func() bool {
		return a.state == stateConnected && len(a.tools) == 5 && len(a.prompts) == 1 && len(a.resources) == 3 && len(a.templates) == 1
	})
	return h
}

// run executes a command asynchronously, feeding results back.
func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if msg != nil {
			h.msgs <- msg
		}
	}()
}

func (h *harness) send(msg tea.Msg) {
	switch m := msg.(type) {
	case tea.BatchMsg:
		for _, c := range m {
			h.run(c)
		}
		return
	case tickMsg, spinner.TickMsg, progress.FrameMsg:
		return // don't keep animation loops alive in tests
	}
	if reflect.TypeOf(msg).String() == "tea.setClipboardMsg" {
		return
	}
	_, cmd := h.a.Update(msg)
	h.run(cmd)
}

func (h *harness) pump() {
	for _, m := range h.a.br.drain() {
		h.send(m)
	}
	for {
		select {
		case m := <-h.msgs:
			h.send(m)
		default:
			return
		}
	}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.pump()
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\n%s", what, h.screen())
}

func (h *harness) key(keys ...string) {
	for _, k := range keys {
		h.send(keyMsg(k))
		h.pump()
	}
}

func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	h.pump()
}

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	if len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: k}
	}
	panic("unknown key " + k)
}

func (h *harness) screen() string {
	s := ansiStrip(h.a.render())
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

func (h *harness) selectItem(tab tabID, key string) {
	h.t.Helper()
	h.a.switchTab(tab)
	if !h.a.lists[tab].selectKey(key) {
		h.t.Fatalf("no item %s", key)
	}
}

var volatile = []*regexp.Regexp{
	regexp.MustCompile(`\d+(\.\d+)?(ms|µs|s)\b`),
	regexp.MustCompile(`\d{4}-\d\d-\d\d`),
	regexp.MustCompile(`\d\d:\d\d:\d\d(\.\d+)?`),
	regexp.MustCompile(`[^ │]* *…`), // truncation point shifts with duration digits
	regexp.MustCompile(`127\.0\.0\.1:\d+`),
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	// Result headers end in buttons that get cut off at different points
	// depending on how long the duration is; compare only up to the modes.
	got = regexp.MustCompile(`(?m)(\(r\)aw).*$`).ReplaceAllString(got, "$1")
	for _, re := range volatile {
		got = re.ReplaceAllString(got, "#")
	}
	path := filepath.Join("testdata", name+".golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func assertFits(t *testing.T, screen string, w, h int) {
	t.Helper()
	lines := strings.Split(screen, "\n")
	if len(lines) != h {
		t.Errorf("screen has %d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("line %d is %d wide (> %d): %q", i, lw, w, l)
		}
	}
}

func TestToolsSnapshot(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{PageSize: 2})
	s := h.screen()
	assertFits(t, s, 100, 30)
	golden(t, "tools", s)
}

func TestCallToolWithForm(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{SSE: true, Sessions: true})
	h.selectItem(tabTools, "tool:add")
	h.key("enter") // focus form
	h.typeText("2")
	h.key("tab")
	h.typeText("3")
	h.key("enter")
	h.waitFor("result", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	s := h.screen()
	if !strings.Contains(s, "✓ matches outputSchema") || !strings.Contains(s, `"sum": 5`) {
		t.Fatalf("unexpected result screen:\n%s", s)
	}
	golden(t, "tool_result", s)

	// Raw view shows the JSON-RPC request.
	h.key("r")
	if s := h.screen(); !strings.Contains(s, `"method": "tools/call"`) {
		t.Fatalf("raw view missing request:\n%s", s)
	}

	// The call also landed in the Chat tab; Edit there restores its arguments.
	h.key("4")
	it := h.a.chat.selected()
	if it == nil || it.name != "add" || string(it.args) != `{"a":2,"b":3}` || it.entry == nil {
		t.Fatalf("chat item = %+v", it)
	}
	if !strings.Contains(h.screen(), "⚒ add") {
		t.Fatalf("chat should show the call:\n%s", h.screen())
	}
}

func TestChatComposer(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{SSE: true})
	h.key("4")
	if h.a.tab != tabChat || h.a.focus != focusComposer {
		t.Fatalf("Chat should open with the message box focused (focus %d)", h.a.focus)
	}
	golden(t, "chat_empty", h.screen())

	// Suggestions, completed with tab.
	h.typeText("ad")
	if len(h.a.chat.sugg) == 0 || h.a.chat.sugg[0].label != "add" {
		t.Fatalf("suggestions = %+v", h.a.chat.sugg)
	}
	h.key("tab")
	if v := h.a.chat.input.Value(); v != "add " {
		t.Fatalf("after tab: %q", v)
	}
	// key=value arguments are typed by schema: numbers stay numbers.
	h.typeText("a=2 b=40")
	h.key("enter")
	h.waitFor("add result", func() bool {
		it := h.a.chat.selected()
		return it != nil && it.rs != nil && !it.rs.running && it.entry != nil
	})
	it := h.a.chat.selected()
	if string(it.args) != `{"a":2,"b":40}` || !strings.Contains(h.screen(), "42") {
		t.Fatalf("args %s\n%s", it.args, h.screen())
	}

	// JSON arguments and a prompt.
	h.typeText(`greet {"name":"Ada"}`)
	h.key("enter")
	h.waitFor("prompt", func() bool { it := h.a.chat.selected(); return it != nil && it.name == "greet" && it.entry != nil })
	if s := h.screen(); !strings.Contains(s, "assistant: Hello, Ada!") {
		t.Fatalf("prompt preview missing:\n%s", s)
	}

	// A tool with required arguments and none given opens its form.
	h.typeText("echo")
	h.key("enter")
	if h.a.chat.compose != "tool:echo" || h.a.focus != focusForm {
		t.Fatalf("compose %q focus %d", h.a.chat.compose, h.a.focus)
	}
	h.typeText("from the form")
	h.key("enter")
	if h.a.chat.compose != "" || h.a.focus != focusComposer {
		t.Fatalf("sending should close the form (compose %q focus %d)", h.a.chat.compose, h.a.focus)
	}
	h.waitFor("echo", func() bool { it := h.a.chat.selected(); return it != nil && it.name == "echo" && it.entry != nil })

	// Resources, and a template with its variable given inline.
	h.typeText("read test://readme")
	h.key("enter")
	h.waitFor("read", func() bool {
		it := h.a.chat.selected()
		return it != nil && it.name == "test://readme" && it.entry != nil
	})
	h.typeText("read test://items/{id} id=7")
	h.key("enter")
	h.waitFor("template", func() bool {
		it := h.a.chat.selected()
		return it != nil && it.name == "test://items/7" && it.entry != nil
	})

	// ↑ ↑ (past the browse bar) recalls the previous command.
	h.key("up", "up")
	if v := h.a.chat.input.Value(); v != "read test://items/{id} id=7" {
		t.Fatalf("history recall = %q", v)
	}
	h.key("esc") // clear

	// Unknown names are reported, not sent.
	n := len(h.a.chat.items)
	h.typeText("nope")
	h.key("enter")
	if len(h.a.chat.items) != n || !strings.Contains(h.a.flash, "nope") {
		t.Fatalf("unknown command: items %d→%d flash %q", n, len(h.a.chat.items), h.a.flash)
	}
	h.key("esc")

	s := h.screen()
	assertFits(t, s, 100, 30)
	if strings.Contains(s, "tools/list") {
		t.Fatalf("list requests should be hidden by default:\n%s", s)
	}
}

func TestChatTranscriptActions(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("4")
	h.typeText(`add {"a":1,"b":1}`)
	h.key("enter")
	h.waitFor("first", func() bool { it := h.a.chat.selected(); return it != nil && it.entry != nil })
	h.typeText("ping")
	h.key("enter")
	h.waitFor("ping", func() bool { it := h.a.chat.selected(); return it != nil && it.name == "ping" && it.entry != nil })

	// Into the transcript, select the add call, run it again.
	h.send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	h.pump()
	if h.a.focus != focusList {
		t.Fatalf("shift+tab should focus the transcript, focus %d", h.a.focus)
	}
	h.key("up")
	if it := h.a.chat.selected(); it.name != "add" {
		t.Fatalf("selected %q", it.name)
	}
	golden(t, "chat_transcript", h.screen())
	h.key("r")
	h.waitFor("re-run", func() bool {
		v := h.a.chat.visible()
		last := v[len(v)-1]
		return len(v) == 4 && last.name == "add" && last.entry != nil
	})
	// Edit opens the form with the same arguments.
	h.key("esc")
	h.send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	h.pump()
	h.key("e")
	if h.a.chat.compose != "tool:add" {
		t.Fatalf("compose = %q", h.a.chat.compose)
	}
	if v, _ := h.a.forms["tool:add"].Values(); string(v) != `{"a":1,"b":1}` {
		t.Fatalf("form values = %s", v)
	}
	h.key("esc")
	if h.a.chat.compose != "" || h.a.focus != focusComposer {
		t.Fatal("esc should close the form")
	}
	// Show all reveals the list/initialize requests.
	h.send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	h.pump()
	h.key("a")
	if !strings.Contains(h.screen(), "tools/list") {
		t.Fatalf("show all:\n%s", h.screen())
	}
}

func TestChatMouse(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.click("4 Chat")
	h.typeText("sl")
	h.click("slow")
	if h.a.chat.compose != "tool:slow" || h.a.focus != focusForm || len(h.a.chat.items) == 0 {
		t.Fatalf("clicking a suggestion should open its form: compose %q focus %d", h.a.chat.compose, h.a.focus)
	}
	h.click("[ Call ⏎ ]")
	h.waitFor("slow", func() bool { it := h.a.chat.selected(); return it != nil && it.entry != nil })
	h.click("[ Edit e ]")
	if h.a.chat.compose != "tool:slow" {
		t.Fatalf("Edit button: compose %q", h.a.chat.compose)
	}
}

func TestDestructiveToolConfirms(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.selectItem(tabTools, "tool:delete_everything")
	h.key("enter")
	if _, ok := h.a.overlay.(*confirmOverlay); !ok {
		t.Fatalf("expected confirmation, got %T\n%s", h.a.overlay, h.screen())
	}
	if !strings.Contains(h.screen(), "marked destructive") {
		t.Fatalf("confirm text missing:\n%s", h.screen())
	}
	h.key("n")
	if h.a.overlay != nil || h.a.results[tabTools] != nil {
		t.Fatal("declining should not call")
	}
	h.key("enter", "y")
	h.waitFor("result", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	if !strings.Contains(h.screen(), "deleted nothing") {
		t.Fatalf("screen:\n%s", h.screen())
	}
}

func TestProdProfileConfirmsWrites(t *testing.T) {
	h := newHarness(t, func(p *config.Profile) { p.EnvBadge = "prod" }, testserver.Options{})
	// Read-only tool: no confirmation.
	h.selectItem(tabTools, "tool:echo")
	h.key("enter")
	h.typeText("hi")
	h.key("enter")
	if h.a.overlay != nil {
		t.Fatal("read-only tool should not need confirmation")
	}
	h.waitFor("echo", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	h.key("esc", "esc")
	// Not read-only: confirm.
	h.selectItem(tabTools, "tool:fail")
	h.key("enter")
	if _, ok := h.a.overlay.(*confirmOverlay); !ok {
		t.Fatalf("expected confirmation on prod profile\n%s", h.screen())
	}
	if !strings.Contains(h.screen(), "prod") {
		t.Fatalf("header should show prod badge:\n%s", h.screen())
	}
}

func TestProgressAndCancel(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{SSE: true})
	h.selectItem(tabTools, "tool:slow")
	h.key("enter")
	h.key("enter")
	h.waitFor("progress", func() bool {
		rs := h.a.results[tabTools]
		return rs != nil && rs.progress != nil
	})
	if s := h.screen(); !strings.Contains(s, "running") || !strings.Contains(s, "step") {
		t.Fatalf("progress not shown:\n%s", s)
	}
	h.key("ctrl+c")
	h.waitFor("cancelled", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	if s := h.screen(); !strings.Contains(s, "cancelled") {
		t.Fatalf("screen:\n%s", s)
	}
	h.waitFor("server saw cancellation", func() bool { _, _, c := h.srv.Stats(); return len(c) == 1 })
}

func TestPromptTranscript(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.selectItem(tabPrompts, "prompt:greet")
	h.key("enter")
	h.key("enter") // required argument missing
	if h.a.results[tabPrompts] != nil {
		t.Fatal("should not get prompt with a missing required argument")
	}
	h.typeText("Ada")
	h.key("enter")
	h.waitFor("prompt", func() bool { rs := h.a.results[tabPrompts]; return rs != nil && !rs.running })
	s := h.screen()
	if !strings.Contains(s, "USER") || !strings.Contains(s, "Hello, Ada!") {
		t.Fatalf("transcript:\n%s", s)
	}
}

func TestResourceTemplateRead(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.selectItem(tabResources, "tmpl:test://items/{id}")
	h.key("enter")
	h.typeText("42")
	h.key("enter")
	h.waitFor("read", func() bool { rs := h.a.results[tabResources]; return rs != nil && !rs.running })
	if s := h.screen(); !strings.Contains(s, `"id": "42"`) || !strings.Contains(s, "test://items/42") {
		t.Fatalf("screen:\n%s", s)
	}
	// Markdown resource read straight from the list.
	h.key("esc", "esc")
	h.selectItem(tabResources, "res:test://readme")
	h.key("enter")
	h.waitFor("readme", func() bool {
		rs := h.a.results[tabResources]
		return rs != nil && !rs.running && rs.subject == "res:test://readme"
	})
	if s := h.screen(); !strings.Contains(s, "Readme") {
		t.Fatalf("screen:\n%s", s)
	}
}

func TestNarrowLayoutStacks(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.send(tea.WindowSizeMsg{Width: 60, Height: 20})
	s := h.screen()
	assertFits(t, s, 60, 20)
	if strings.Contains(s, "│") {
		t.Fatalf("narrow layout should not split panes:\n%s", s)
	}
	h.key("enter")
	s = h.screen()
	assertFits(t, s, 60, 20)
	if !strings.Contains(s, "message") {
		t.Fatalf("detail pane should show the form:\n%s", s)
	}
}

func TestHelpAndInstructions(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("?")
	if !strings.Contains(h.screen(), "Keys") {
		t.Fatalf("help:\n%s", h.screen())
	}
	h.key("esc", "i")
	s := h.screen()
	if !strings.Contains(s, "testserver") || !strings.Contains(s, "echo") {
		t.Fatalf("instructions:\n%s", s)
	}
	assertFits(t, s, 100, 30)
}

func TestSessionExpiryIsTransparent(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{Sessions: true})
	h.srv.ExpireSessions()
	h.selectItem(tabTools, "tool:echo")
	h.key("enter")
	h.typeText("again")
	h.key("enter")
	h.waitFor("result", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	if s := h.screen(); !strings.Contains(s, "again") || strings.Contains(s, "error") {
		t.Fatalf("screen:\n%s", s)
	}
}

func TestLoginOverlayKeys(t *testing.T) {
	t.Setenv("MCPTUI_STATE_DIR", t.TempDir())
	h := newHarness(t, nil, testserver.Options{})
	var submitted string
	url := "https://auth.example.com/authorize?client_id=abc&redirect_uri=http%3A%2F%2F127.0.0.1%3A33418%2Fcallback&" + strings.Repeat("x", 150)
	h.send(loginPromptMsg{p: authPrompt(url, func(s string) { submitted = s })})
	ov, ok := h.a.overlay.(*loginOverlay)
	if !ok {
		t.Fatalf("overlay = %T", h.a.overlay)
	}
	s := h.screen()
	if !strings.Contains(s, "c copy") || !strings.Contains(s, "Unbroken copy: cat ") {
		t.Fatalf("login overlay:\n%s", s)
	}
	if b, _ := os.ReadFile(ov.urlFile); strings.TrimSpace(string(b)) != url {
		t.Fatalf("url file = %q", b)
	}
	h.key("c")
	if ov.status == "" || h.a.overlay == nil {
		t.Fatal("c should copy and keep the overlay open")
	}
	// Bracketed paste focuses the field; enter submits.
	h.send(tea.PasteMsg{Content: "http://127.0.0.1:33418/callback?code=1&state=2"})
	h.key("enter")
	if submitted != "http://127.0.0.1:33418/callback?code=1&state=2" {
		t.Fatalf("submitted %q", submitted)
	}
	// p focuses the field, esc leaves it, esc again cancels.
	h.key("p")
	if !ov.in.Focused() {
		t.Fatal("p should focus the paste field")
	}
	h.key("esc")
	if h.a.overlay == nil || ov.in.Focused() {
		t.Fatal("first esc should only leave the paste field")
	}
	h.key("esc")
	if h.a.overlay != nil {
		t.Fatal("second esc should cancel the login")
	}
}

func authPrompt(url string, submit func(string)) auth.LoginPrompt {
	return auth.LoginPrompt{AuthURL: url, RedirectURI: "http://127.0.0.1:33418/callback", Submit: submit}
}

// newPickerHarness starts the TUI with no profile, as a bare `mcptui` does.
func newPickerHarness(t *testing.T, cfgText string) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if cfgText != "" {
		os.WriteFile(cfgPath, []byte(cfgText), 0o600)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	dark := true
	a := New(Options{Config: cfg, NoHistory: true, CredentialsPath: filepath.Join(dir, "creds.json"), Dark: &dark})
	h := &harness{t: t, a: a, msgs: make(chan tea.Msg, 256)}
	t.Cleanup(func() {
		if a.sess != nil {
			a.sess.Close()
		}
	})
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.run(a.Init())
	h.pump()
	return h, cfgPath
}

func TestPickerCreatesFirstProfile(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	hs := httptest.NewServer(srv)
	defer hs.Close()
	h, cfgPath := newPickerHarness(t, "")
	if h.a.picker == nil || h.a.picker.form == nil {
		t.Fatalf("no profiles should open the new-profile form\n%s", h.screen())
	}
	assertFits(t, h.screen(), 100, 30)
	h.typeText("local")
	h.key("tab")
	h.a.Update(tea.PasteMsg{Content: hs.URL + "/mcp"})
	h.key("tab") // env badge
	h.key("tab") // auth
	h.send(tea.KeyPressMsg{Code: tea.KeyRight})
	h.send(tea.KeyPressMsg{Code: tea.KeyRight})
	h.send(tea.KeyPressMsg{Code: tea.KeyRight}) // oauth -> bearer -> none
	h.key("enter")
	if h.a.picker != nil {
		t.Fatalf("save should close the picker: %s\n%s", h.a.picker.err, h.screen())
	}
	h.waitFor("connected", func() bool { return h.a.state == stateConnected && len(h.a.tools) == 5 })
	cfg, _ := config.Load(cfgPath)
	p := cfg.Profiles["local"]
	if p == nil || p.URL != hs.URL+"/mcp" || cfg.DefaultProfile != "local" {
		t.Fatalf("saved config = %+v default %q", p, cfg.DefaultProfile)
	}
	if p.Auth != config.AuthNone {
		t.Fatalf("auth = %q", p.Auth)
	}
}

func TestPickerSelectsAndSwitches(t *testing.T) {
	a := httptest.NewServer(testserver.New(testserver.Options{}))
	defer a.Close()
	h, cfgPath := newPickerHarness(t, `default_profile = "two"
[profiles.one]
url = "`+a.URL+`/one"
auth = "none"
[profiles.two]
url = "`+a.URL+`/two"
auth = "none"
env_badge = "staging"
[profiles.cmd]
command = ["python", "-m", "my server"]
`)
	s := h.screen()
	if !strings.Contains(s, "Choose a profile") || !strings.Contains(s, "▸ two") || !strings.Contains(s, "staging") {
		t.Fatalf("picker:\n%s", s)
	}
	// The config path varies by OS (and is truncated when long), so mask the whole line.
	s = regexp.MustCompile(`(Profiles +)\S.*`).ReplaceAllString(strings.ReplaceAll(s, a.URL, "http://SERVER"), "${1}CONFIG")
	golden(t, "picker", s)
	h.key("enter")
	h.waitFor("connected to two", func() bool { return h.a.state == stateConnected && h.a.profile.Name == "two" })

	// ctrl+p, make "one" the default, edit "cmd", then switch to "one".
	h.send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if h.a.picker == nil {
		t.Fatal("ctrl+p should open the picker")
	}
	h.a.picker.list.selectKey("profile:cmd")
	h.key("e")
	if v := h.a.picker.form.StringValues()["target"]; v != "python -m 'my server'" {
		t.Fatalf("edit target = %q", v)
	}
	h.key("esc")
	h.a.picker.list.selectKey("profile:one")
	h.key("*")
	h.key("enter")
	h.waitFor("connected to one", func() bool { return h.a.state == stateConnected && h.a.profile.Name == "one" && len(h.a.tools) == 5 })
	cfg, _ := config.Load(cfgPath)
	if cfg.DefaultProfile != "one" {
		t.Fatalf("default = %q", cfg.DefaultProfile)
	}

	// Delete asks first and refuses the connected profile.
	h.send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	h.key("d")
	if h.a.overlay != nil {
		t.Fatal("deleting the connected profile should be refused")
	}
	h.a.picker.list.selectKey("profile:cmd")
	h.key("d", "y")
	cfg, _ = config.Load(cfgPath)
	if _, ok := cfg.Profiles["cmd"]; ok {
		t.Fatal("cmd should be deleted")
	}
	h.key("esc")
	if h.a.picker != nil {
		t.Fatal("esc should return to the connection")
	}
}

func TestPickerSavesTemporaryConnection(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	dir := t.TempDir()
	cfg, _ := config.Load(filepath.Join(dir, "config.toml"))
	h.a.opts.Config = cfg
	h.a.profile.Temporary = true
	h.send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if !strings.Contains(h.screen(), "unsaved") {
		t.Fatalf("picker should list the temporary connection:\n%s", h.screen())
	}
	h.key("s")
	if h.a.picker.form == nil || !strings.HasSuffix(h.a.picker.form.StringValues()["target"], "/mcp") {
		t.Fatal("s should prefill the form from the connection")
	}
	h.typeText("saved")
	h.key("enter")
	h.waitFor("reconnected", func() bool {
		return h.a.state == stateConnected && h.a.profile.Name == "saved" && !h.a.profile.Temporary
	})
	again, _ := config.Load(cfg.Path())
	if again.Profiles["saved"] == nil {
		t.Fatal("profile not written")
	}
}

func TestProfileFormUsesTitles(t *testing.T) {
	h, _ := newPickerHarness(t, "")
	s := h.screen()
	for _, want := range []string{"Name *", "URL or command *", "Env badge", "Client ID"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing label %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "client_id") || strings.Contains(s, "env_badge") {
		t.Fatalf("raw field names shown:\n%s", s)
	}
}

// find locates text on the rendered screen (cell coordinates).
func (h *harness) find(text string) (int, int) {
	h.t.Helper()
	for y, line := range strings.Split(h.screen(), "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return lipgloss.Width(line[:i]), y
		}
	}
	h.t.Fatalf("%q not on screen:\n%s", text, h.screen())
	return 0, 0
}

func (h *harness) click(text string) {
	h.t.Helper()
	x, y := h.find(text)
	h.send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.pump()
}

func (h *harness) doubleClick(text string) {
	h.t.Helper()
	x, y := h.find(text)
	h.send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.pump()
}

func TestMouseToolFlow(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.click("2 Prompts")
	if h.a.tab != tabPrompts {
		t.Fatalf("tab = %d", h.a.tab)
	}
	h.click("1 Tools")
	h.click("  add ")
	if h.a.selectedKey() != "tool:add" || h.a.focus != focusList {
		t.Fatalf("selection %q focus %d", h.a.selectedKey(), h.a.focus)
	}
	// Clicking the "b" field focuses it.
	h.click("b *")
	if h.a.focus != focusForm || h.a.forms["tool:add"].FocusedField() != 1 {
		t.Fatalf("focus %d field %d", h.a.focus, h.a.forms["tool:add"].FocusedField())
	}
	h.typeText("4")
	h.click("a *")
	h.typeText("1")
	h.click("[ Call ⏎ ]")
	h.waitFor("result", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	if !strings.Contains(h.screen(), `"sum": 5`) {
		t.Fatalf("screen:\n%s", h.screen())
	}
	h.click("(r)aw")
	if h.a.results[tabTools].mode != modeRaw || h.a.focus != focusResult {
		t.Fatal("clicking (r)aw should switch to raw view")
	}
	// Double-clicking a list row opens its form.
	h.doubleClick("  echo ")
	if h.a.selectedKey() != "tool:echo" || h.a.focus != focusForm {
		t.Fatalf("double-click: selection %q focus %d", h.a.selectedKey(), h.a.focus)
	}
}

func TestMouseDialogsAndFooter(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.selectItem(tabTools, "tool:delete_everything")
	h.click("[ Call ⏎ ]")
	if _, ok := h.a.overlay.(*confirmOverlay); !ok {
		t.Fatalf("overlay = %T", h.a.overlay)
	}
	h.click("[ No n ]")
	if h.a.overlay != nil || h.a.results[tabTools] != nil {
		t.Fatal("No should dismiss without calling")
	}
	h.a.flash = "" // the "cancelled" message temporarily replaces the key hints
	h.click("? help")
	if _, ok := h.a.overlay.(*pagerOverlay); !ok {
		t.Fatalf("footer ? should open help, got %T", h.a.overlay)
	}
	h.screen()                                                    // render, as the program does after every update
	h.send(tea.MouseClickMsg{X: 0, Y: 29, Button: tea.MouseLeft}) // outside the box
	if h.a.overlay != nil {
		t.Fatal("clicking outside should close help")
	}
}

func TestKeyboardTabCyclesFocus(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.selectItem(tabTools, "tool:add")
	h.key("tab")
	if h.a.focus != focusForm || h.a.forms["tool:add"].FocusedField() != 0 {
		t.Fatal("tab from list should focus the first field")
	}
	h.key("tab")
	if h.a.focus != focusForm || h.a.forms["tool:add"].FocusedField() != 1 {
		t.Fatal("tab should move to the next field")
	}
	h.key("tab")
	if h.a.focus != focusTabs {
		t.Fatalf("tab after the last field (no result) should reach the tab bar, focus %d", h.a.focus)
	}
	h.key("tab")
	if h.a.focus != focusList {
		t.Fatalf("tab from the tab bar should enter the list, focus %d", h.a.focus)
	}
}

func TestTabBarArrows(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("esc")
	if h.a.focus != focusTabs {
		t.Fatalf("esc from the list should focus the tab bar, focus %d", h.a.focus)
	}
	h.send(tea.KeyPressMsg{Code: tea.KeyRight})
	h.send(tea.KeyPressMsg{Code: tea.KeyRight})
	if h.a.tab != tabResources || h.a.focus != focusTabs {
		t.Fatalf("right arrows: tab %d focus %d", h.a.tab, h.a.focus)
	}
	h.send(tea.KeyPressMsg{Code: tea.KeyLeft})
	h.send(tea.KeyPressMsg{Code: tea.KeyLeft})
	h.send(tea.KeyPressMsg{Code: tea.KeyLeft})
	if h.a.tab != tabLog {
		t.Fatalf("left should wrap to Log, tab %d", h.a.tab)
	}
	h.key("down")
	if h.a.focus != focusList {
		t.Fatal("down should leave the tab bar")
	}
	h.key("esc")
	if h.a.focus != focusTabs {
		t.Fatal("esc on the Log tab should return to the tab bar")
	}
	h.send(tea.KeyPressMsg{Code: tea.KeyRight}) // wraps to Tools
	h.key("enter")
	// From the list, ↑ on the first row goes back up too.
	h.key("up")
	if h.a.tab != tabTools || h.a.focus != focusTabs {
		t.Fatalf("up on first row: tab %d focus %d", h.a.tab, h.a.focus)
	}
	// Result → form → list → tabs via esc.
	h.key("enter")
	h.selectItem(tabTools, "tool:echo")
	h.key("enter")
	h.typeText("x")
	h.key("enter")
	h.waitFor("result", func() bool { rs := h.a.results[tabTools]; return rs != nil && !rs.running })
	for _, want := range []focusArea{focusForm, focusList, focusTabs} {
		h.key("esc")
		if h.a.focus != want {
			t.Fatalf("esc chain: focus %d, want %d", h.a.focus, want)
		}
	}
}

func TestPickerMouse(t *testing.T) {
	h, _ := newPickerHarness(t, "[profiles.one]\nurl = \"http://127.0.0.1:1/mcp\"\n")
	h.doubleClick("+ New profile")
	if h.a.picker.form == nil {
		t.Fatalf("double-click on + New profile should open the form\n%s", h.screen())
	}
	h.click("[ Cancel esc ]")
	if h.a.picker == nil || h.a.picker.form != nil {
		t.Fatal("Cancel should close the form but stay in the picker")
	}
}

func TestChatFormFitsPane(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("4")
	h.typeText("echo")
	h.key("enter")
	s := h.screen()
	assertFits(t, s, 100, 30)
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "message *") && strings.HasSuffix(strings.TrimRight(l, " "), "…") {
			t.Fatalf("form line clipped: %q", l)
		}
	}
}

func TestChatBrowseMenu(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("4")
	s := h.screen()
	if !strings.Contains(s, "⚒ Tools") || !strings.Contains(s, "✎ Prompts") || !strings.Contains(s, "▤ Resources & templates") {
		t.Fatalf("empty message box should offer the browse bar:\n%s", s)
	}
	// ↑ highlights the bar, → moves, ⏎ opens the category.
	h.key("up")
	h.send(tea.KeyPressMsg{Code: tea.KeyRight})
	h.send(tea.KeyPressMsg{Code: tea.KeyLeft})
	h.key("enter")
	if v := h.a.chat.input.Value(); v != "call " {
		t.Fatalf("input after picking Tools = %q", v)
	}
	if len(h.a.chat.sugg) != 5 || !strings.Contains(h.screen(), "Tools (5)") {
		t.Fatalf("all tools should be listed: %d\n%s", len(h.a.chat.sugg), h.screen())
	}
	golden(t, "chat_browse_tools", h.screen())
	// Typing narrows; picking fills the command.
	h.typeText("sl")
	if len(h.a.chat.sugg) != 1 || h.a.chat.sugg[0].label != "slow" {
		t.Fatalf("filtered = %+v", h.a.chat.sugg)
	}
	// Picking opens the form, even with no required arguments; nothing is
	// sent until it's confirmed.
	n := len(h.a.chat.items)
	h.key("enter")
	if h.a.chat.compose != "tool:slow" || h.a.focus != focusForm || len(h.a.chat.items) != n {
		t.Fatalf("picking should open the form without sending: compose %q items %d→%d", h.a.chat.compose, n, len(h.a.chat.items))
	}
	if !strings.Contains(h.screen(), "Cancel esc") {
		t.Fatalf("form should offer Cancel:\n%s", h.screen())
	}
	h.key("enter")
	h.waitFor("slow", func() bool { it := h.a.chat.selected(); return it != nil && it.name == "slow" && it.entry != nil })

	// esc on a bare category goes back to the bar.
	h.key("up", "right", "right", "enter")
	if v := h.a.chat.input.Value(); v != "read " {
		t.Fatalf("third category = %q", v)
	}
	if !strings.Contains(h.screen(), "test://items/{id}") {
		t.Fatalf("templates should be listed:\n%s", h.screen())
	}
	h.key("esc")
	if h.a.chat.input.Value() != "" || !h.a.chat.menuMode() {
		t.Fatal("esc should return to the browse bar")
	}
	// ↑ highlights the bar, ↓ leaves it, and ↑ ↑ recalls commands.
	h.key("up")
	if h.a.chat.suggIdx != 0 {
		t.Fatal("↑ should highlight the bar")
	}
	h.key("down")
	if h.a.chat.suggIdx != -1 {
		t.Fatal("↓ should leave the bar")
	}
	h.key("up", "up")
	if v := h.a.chat.input.Value(); v != "slow" {
		t.Fatalf("recall = %q", v)
	}
	// Clicking the bar works too.
	h.key("esc")
	h.click("✎ Prompts")
	if v := h.a.chat.input.Value(); v != "get " {
		t.Fatalf("click Prompts = %q", v)
	}
}

func TestChatShowsConnectionAndServerMessages(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{SSE: true})
	h.key("4")
	s := h.screen()
	if !strings.Contains(s, "● Connected to testserver 1.2.3") || !strings.Contains(s, "Use echo to test.") {
		t.Fatalf("connection intro missing:\n%s", s)
	}
	if it := h.a.chat.selected(); it == nil || it.kind != "init" {
		t.Fatalf("the intro should be selected at first: %+v", it)
	}
	// A notification arriving now doesn't take the selection from the intro.
	h.a.addNotification("notifications/tools/list_changed", nil)
	if it := h.a.chat.selected(); it == nil || it.kind != "init" {
		t.Fatalf("a notification stole the selection: %+v", it)
	}
	s = h.screen()
	if !strings.Contains(s, "instructions") || !strings.Contains(s, "capabilities") {
		t.Fatalf("intro detail should show instructions and capabilities:\n%s", s)
	}
	golden(t, "chat_intro", s)

	// The intro isn't a call: double-click, e and r do nothing (and don't panic).
	h.doubleClick("▌ ● Connected to testserver")
	h.key("e", "r")
	if h.a.chat.compose != "" || h.a.focus != focusList {
		t.Fatalf("editing the intro: compose %q focus %d", h.a.chat.compose, h.a.focus)
	}
	h.key("esc")

	// A call that makes the server log: the notification appears but the
	// call stays selected.
	h.typeText(`echo message=hi`)
	h.key("enter")
	h.waitFor("echo + log", func() bool {
		it := h.a.chat.selected()
		if it == nil || it.name != "echo" || it.entry == nil {
			return false
		}
		for _, x := range h.a.chat.items {
			if x.kind == "notify" {
				return true
			}
		}
		return false
	})
	if s := h.screen(); !strings.Contains(s, "◂ notifications/message") || !strings.Contains(s, "[info] echoing") {
		t.Fatalf("notification missing:\n%s", s)
	}

	// Server-to-client requests and disconnects are recorded too.
	h.send(serverRequestMsg{method: "sampling/createMessage", params: json.RawMessage(`{"messages":[]}`), reply: json.RawMessage(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`), gen: h.a.gen})
	h.send(disconnectedMsg{err: errors.New("server went away"), gen: h.a.gen})
	s = h.screen()
	if !strings.Contains(s, "⇠ server asked: sampling") || !strings.Contains(s, "○ Disconnected") || !strings.Contains(s, "server went away") {
		t.Fatalf("server request / disconnect missing:\n%s", s)
	}
	// Selecting the notification shows its params.
	for i, it := range h.a.chat.visible() {
		if it.kind == "notify" {
			h.a.selectChat(i)
		}
	}
	if s := h.screen(); !strings.Contains(s, "log info") || !strings.Contains(s, `"level": "info"`) {
		t.Fatalf("notification detail:\n%s", s)
	}
}

func TestChatCallSyntax(t *testing.T) {
	h := newHarness(t, nil, testserver.Options{})
	h.key("4")
	send := func(line, name, wantArgs string) {
		t.Helper()
		h.typeText(line)
		h.key("enter")
		h.waitFor(line, func() bool {
			it := h.a.chat.selected()
			return it != nil && it.name == name && it.entry != nil
		})
		if got := commandArgs(h.a.chat.selected().args); got != wantArgs {
			t.Fatalf("%s: args %s, want %s", line, got, wantArgs)
		}
	}
	send("add(2, 3)", "add", `{"a":2,"b":3}`)
	send(`call echo("hi, (there)")`, "echo", `{"message":"hi, (there)"}`)
	send("slow(steps=1, interval=1)", "slow", `{"interval":1,"steps":1}`)
	send("greet(Ada)", "greet", `{"name":"Ada"}`)

	n := len(h.a.chat.items)
	h.typeText("add(1, 2, 3)")
	h.key("enter")
	if len(h.a.chat.items) != n || !strings.Contains(h.a.flash, "too many arguments") {
		t.Fatalf("extra argument: items %d→%d flash %q", n, len(h.a.chat.items), h.a.flash)
	}
}

func TestParseCallArgs(t *testing.T) {
	typeOf := func(k string) string {
		return map[string]string{"id": "integer", "name": "string", "tags": "array", "on": "boolean"}[k]
	}
	params := []string{"id", "name", "tags", "on"}
	for in, want := range map[string]string{
		"()":                      `{}`,
		"(7025)":                  `{"id":7025}`,
		"(7, 42)":                 `{"id":7,"name":"42"}`,
		`(7, "a, b", ["x", "y"])`: `{"id":7,"name":"a, b","tags":["x","y"]}`,
		"(7, 'it''s')":            `{"id":7,"name":"it''s"}`,
		"(on=true, id=1)":         `{"id":1,"on":true}`,
		`(1, name="x=y")`:         `{"id":1,"name":"x=y"}`,
		`(1, "a\"b")`:             `{"id":1,"name":"a\"b"}`,
	} {
		got, err := parseArgs(in, params, typeOf)
		if err != nil || string(got) != want {
			t.Errorf("parseArgs(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"(1", "(1, 2, 3, 4, 5)", `(1, "x)`, "(1, [1)", "(id=)"} {
		if _, err := parseArgs(in, params, typeOf); err == nil {
			t.Errorf("parseArgs(%s) should fail", in)
		}
	}
}
