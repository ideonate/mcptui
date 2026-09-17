package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/copycmd"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/schemaform"
	"github.com/ideonate/mcptui/internal/uritemplate"
)

// scrollView is a minimal line scroller.
type scrollView struct {
	lines  []string
	offset int
}

func (s *scrollView) set(content string) {
	s.lines = strings.Split(content, "\n")
	s.clamp(0)
}

func (s *scrollView) clamp(h int) {
	maxOff := max(len(s.lines)-h, 0)
	s.offset = min(max(s.offset, 0), maxOff)
}

func (s *scrollView) scroll(d, h int) {
	s.offset += d
	s.clamp(h)
}

func (s *scrollView) bottom(h int) { s.offset = max(len(s.lines)-h, 0) }

func (s *scrollView) atBottom(h int) bool { return s.offset >= len(s.lines)-h }

func (s *scrollView) view(w, h int) string {
	s.clamp(h)
	end := min(s.offset+h, len(s.lines))
	return fitLines(strings.Join(s.lines[s.offset:end], "\n"), w, h)
}

// resultState is the result area of a tab.
type resultState struct {
	title    string
	subject  string // item key the result belongs to
	kind     string // tool | prompt | resource | schema | doc
	run      int
	running  bool
	started  time.Time
	cancel   context.CancelFunc
	progress *mcp.ProgressParams

	ex      *mcp.Exchange
	tool    *mcp.CallToolResult
	toolDef *mcp.Tool
	prompt  *mcp.GetPromptResult
	read    *mcp.ReadResourceResult
	args    json.RawMessage
	static  string // pre-rendered content for schema/doc views
	// custom renders non-call entries (connection, notifications) by mode.
	custom func(width int, mode viewMode) string

	mode  viewMode
	sv    scrollView
	dirty bool
	width int
}

func (rs *resultState) render(a *App, width int) {
	if !rs.dirty && rs.width == width {
		return
	}
	rs.width = width
	rs.dirty = false
	w := max(width-1, 10)
	var content string
	switch {
	case rs.custom != nil:
		content = rs.custom(w, rs.mode)
	case rs.static != "":
		content = rs.static
	case rs.mode == modeRaw:
		content = a.rd.renderRaw(rs.ex, w)
	case rs.ex != nil && errors.Is(rs.ex.Err, context.Canceled):
		content = a.th.warnText.Render("⊘ cancelled; notifications/cancelled was sent to the server")
	case rs.ex != nil && errors.Is(rs.ex.Err, context.DeadlineExceeded):
		content = a.th.badText.Render("✗ timed out (profile timeout); notifications/cancelled was sent")
	case rs.ex != nil && rs.ex.Err != nil:
		content = a.th.badText.Render(wrap("✗ "+rs.ex.Err.Error(), w))
		if errors.Is(rs.ex.Err, auth.ErrLoginRequired) {
			content += "\n" + a.th.dimText.Render("press L to log in")
		}
	case rs.tool != nil:
		content = a.rd.renderToolResult(rs.tool, rs.toolDef, rs.mode, w)
	case rs.prompt != nil:
		content = a.rd.renderPromptResult(rs.prompt, rs.mode, w)
	case rs.read != nil:
		content = a.rd.renderReadResult(rs.read, rs.mode, w)
	case rs.ex != nil && len(rs.ex.Result) > 0:
		content = a.rd.jsonBlock(rs.ex.Result, w)
	}
	rs.sv.set(content)
}

// ---------------------------------------------------------------- lists

func (a *App) rebuildToolList() {
	var items []listItem
	for _, t := range a.tools {
		items = append(items, listItem{key: "tool:" + t.Name, title: t.Name, desc: t.Title + " " + t.Description, marker: a.toolMarkers(&t, true)})
	}
	a.lists[tabTools].setItems(items)
}

func (a *App) toolMarkers(t *mcp.Tool, compact bool) string {
	th := a.th
	an := t.Annotations
	var out []string
	if an.IsDestructive() {
		if compact {
			out = append(out, th.badText.Render("⚠"))
		} else {
			out = append(out, th.badText.Render("⚠ destructive"))
		}
	}
	if compact {
		if an.IsReadOnly() {
			out = append(out, th.faintText.Render("ro"))
		}
		return strings.Join(out, " ")
	}
	if an.IsReadOnly() {
		out = append(out, th.pill("read-only", th.ok))
	}
	if an != nil && an.IdempotentHint != nil && *an.IdempotentHint {
		out = append(out, th.pill("idempotent", th.info))
	}
	if an != nil && an.OpenWorldHint != nil && *an.OpenWorldHint {
		out = append(out, th.pill("open-world", th.warn))
	}
	return strings.Join(out, " ")
}

func (a *App) rebuildPromptList() {
	var items []listItem
	for _, p := range a.prompts {
		items = append(items, listItem{key: "prompt:" + p.Name, title: p.Name, desc: p.Title + " " + p.Description})
	}
	a.lists[tabPrompts].setItems(items)
}

func (a *App) rebuildResourceList() {
	var items []listItem
	for _, r := range a.resources {
		title := firstNonEmpty(r.Name, r.URI)
		items = append(items, listItem{key: "res:" + r.URI, title: title, desc: r.URI + " " + r.Title + " " + r.Description, group: "resources"})
	}
	for _, t := range a.templates {
		items = append(items, listItem{key: "tmpl:" + t.URITemplate, title: firstNonEmpty(t.Name, t.URITemplate), desc: t.URITemplate + " " + t.Description, group: "templates"})
	}
	a.lists[tabResources].setItems(items)
}

// ---------------------------------------------------------------- selection

func (a *App) selectedKey() string {
	if it := a.lists[a.tab].selected(); it != nil {
		return it.key
	}
	return ""
}

func (a *App) findTool(name string) *mcp.Tool {
	for i := range a.tools {
		if a.tools[i].Name == name {
			return &a.tools[i]
		}
	}
	return nil
}

func (a *App) findPrompt(name string) *mcp.Prompt {
	for i := range a.prompts {
		if a.prompts[i].Name == name {
			return &a.prompts[i]
		}
	}
	return nil
}

func (a *App) findResource(uri string) *mcp.Resource {
	for i := range a.resources {
		if a.resources[i].URI == uri {
			return &a.resources[i]
		}
	}
	return nil
}

func (a *App) findTemplate(t string) *mcp.ResourceTemplate {
	for i := range a.templates {
		if a.templates[i].URITemplate == t {
			return &a.templates[i]
		}
	}
	return nil
}

// currentKey is the item the form and actions apply to: the list selection,
// or on the Chat tab the item being composed.
func (a *App) currentKey() string {
	switch a.tab {
	case tabChat:
		return a.chat.compose
	case tabLog:
		return ""
	}
	return a.selectedKey()
}

// currentForm returns (creating if needed) the form for the current item.
func (a *App) currentForm() *schemaform.Form {
	return a.formFor(a.currentKey())
}

func (a *App) formFor(key string) *schemaform.Form {
	if key == "" {
		return nil
	}
	if f, ok := a.forms[key]; ok {
		return f
	}
	kind, name, _ := strings.Cut(key, ":")
	var f *schemaform.Form
	switch kind {
	case "tool":
		t := a.findTool(name)
		if t == nil {
			return nil
		}
		var err error
		f, err = schemaform.Parse(t.InputSchema)
		if err != nil {
			a.logf("client", "warn", "tool %s: bad inputSchema: %v", name, err)
			f, _ = schemaform.Parse(json.RawMessage(`{"anyOf":[{"type":"object"}]}`))
		}
	case "prompt":
		p := a.findPrompt(name)
		if p == nil {
			return nil
		}
		var fields []schemaform.StringField
		for _, arg := range p.Arguments {
			fields = append(fields, schemaform.StringField{Name: arg.Name, Title: arg.Title, Description: arg.Description, Required: arg.Required})
		}
		f = schemaform.FromStrings(fields)
	case "tmpl":
		var fields []schemaform.StringField
		for _, v := range uritemplate.Variables(name) {
			fields = append(fields, schemaform.StringField{Name: v, Required: true})
		}
		f = schemaform.FromStrings(fields)
	case "res":
		f = schemaform.FromStrings(nil)
	default:
		return nil
	}
	f.SetWidth(max(a.layout().detailW-2, 10))
	a.forms[key] = f
	return f
}

// ---------------------------------------------------------------- keys

func (a *App) listKey(k tea.KeyPressMsg) tea.Cmd {
	l := a.lists[a.tab]
	switch k.String() {
	case "up", "k":
		if l.cursor == 0 {
			return a.focusTabBar()
		}
		l.move(-1)
	case "down", "j":
		l.move(1)
	case "pgup":
		l.move(-10)
	case "pgdown":
		l.move(10)
	case "home", "g":
		l.cursor = 0
	case "end", "G":
		l.move(len(l.visible))
	case "/":
		return l.startFilter()
	case "esc":
		if l.filter.Value() != "" {
			l.filter.SetValue("")
			l.applyFilter()
			return nil
		}
		return a.focusTabBar()
	case "shift+tab":
		return a.focusTabBar()
	case "enter", "right", "l", "tab":
		if l.selected() == nil {
			return nil
		}
		if k.String() == "enter" {
			if f := a.currentForm(); f != nil && len(f.Fields()) == 0 {
				return a.execute()
			}
		}
		return a.focusFormArea()
	case "e":
		if l.selected() != nil {
			return a.openJSONEditor()
		}
	case "E":
		if l.selected() != nil {
			return a.openExternalEditor()
		}
	case "S":
		return a.toggleSubscribe()
	case "d":
		if key := a.selectedKey(); key != "" {
			a.overlay = newDocOverlay(a, key)
		}
	case "x":
		if a.connErr != nil && isClientRejected(a.connErr) {
			return a.resetClient()
		}
	case "ctrl+down":
		if a.results[a.tab] != nil {
			a.focus = focusResult
		}
	}
	return nil
}

func (a *App) focusFormArea() tea.Cmd {
	f := a.currentForm()
	if f == nil {
		return nil
	}
	a.focus = focusForm
	return f.Focus()
}

// formShortcut handles ctrl-combos while the form has focus.
func (a *App) formShortcut(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "enter":
		if f := a.currentForm(); f != nil && f.InputActive() && isMultiline(f) {
			return nil, false
		}
		return a.execute(), true
	case "ctrl+s":
		return a.execute(), true
	case "tab":
		// Leave the form after its last field, like focus cycling in Textual.
		if f := a.currentForm(); f == nil || f.AtLastField() {
			a.blurForm()
			if a.tab == tabChat {
				return a.focusComposer(), true
			}
			if a.results[a.tab] != nil {
				a.focus = focusResult
			} else {
				a.focus = focusTabs
			}
			return nil, true
		}
		return nil, false
	case "shift+tab":
		if f := a.currentForm(); f == nil || f.AtFirstField() {
			a.blurForm()
			if a.tab == tabChat {
				return a.focusComposer(), true
			}
			a.focus = focusList
			return nil, true
		}
		return nil, false
	case "esc":
		if a.tab == tabChat {
			a.closeCompose()
			return a.focusComposer(), true
		}
		a.blurForm()
		a.focus = focusList
		return nil, true
	case "ctrl+e":
		return a.openJSONEditor(), true
	case "ctrl+o":
		return a.openExternalEditor(), true
	case "ctrl+down":
		if a.results[a.tab] != nil {
			a.blurForm()
			a.focus = focusResult
		}
		return nil, true
	case "ctrl+@", "ctrl+space":
		return a.complete(), true
	}
	return nil, false
}

// isMultiline reports whether enter should insert a newline.
func isMultiline(f *schemaform.Form) bool { return f.FocusedMultiline() }

func (a *App) blurForm() {
	if f := a.currentForm(); f != nil {
		f.Blur()
	}
}

func (a *App) formKey(k tea.KeyPressMsg) tea.Cmd {
	if cmd, ok := a.formShortcut(k); ok {
		return cmd
	}
	switch k.String() {
	case "e":
		return a.openJSONEditor()
	case "E":
		return a.openExternalEditor()
	case "S":
		return a.toggleSubscribe()
	case "d":
		a.overlay = newDocOverlay(a, a.currentKey())
		return nil
	}
	if f := a.currentForm(); f != nil {
		return f.Update(k)
	}
	return nil
}

func (a *App) resultKey(k tea.KeyPressMsg) tea.Cmd {
	rs := a.results[a.tab]
	if rs == nil {
		a.focus = focusForm
		return nil
	}
	h := a.resultHeight()
	switch k.String() {
	case "esc", "ctrl+up", "enter", "shift+tab":
		return a.focusFormArea()
	case "tab":
		a.focus = focusTabs
		return nil
	case "up", "k":
		rs.sv.scroll(-1, h)
	case "down", "j":
		rs.sv.scroll(1, h)
	case "pgup", "b":
		rs.sv.scroll(-h, h)
	case "pgdown", "space", "f":
		rs.sv.scroll(h, h)
	case "g", "home":
		rs.sv.offset = 0
	case "G", "end":
		rs.sv.bottom(h)
	case "p":
		rs.mode, rs.dirty = modePretty, true
	case "m":
		rs.mode, rs.dirty = modeMarkdown, true
	case "r":
		rs.mode, rs.dirty = modeRaw, true
	case "y":
		return a.copyResult(rs)
	case "s":
		a.overlay = newSaveOverlay(a, rs)
		return a.overlay.init()
	case "c":
		return a.copyCommand(rs.subject, rs.args, false, rs.ex)
	case "C":
		return a.copyCommand(rs.subject, rs.args, true, rs.ex)
	case "e":
		return a.openJSONEditor()
	case "E":
		return a.openExternalEditor()
	}
	return nil
}

// ---------------------------------------------------------------- execute

func (a *App) execute() tea.Cmd {
	key := a.currentKey()
	if key == "" {
		return nil
	}
	f := a.formFor(key)
	if f == nil {
		return nil
	}
	if errs := f.Validate(); len(errs) > 0 {
		a.setFlash("fix %d validation error(s): %v", len(errs), errs[0])
		return nil
	}
	var args json.RawMessage
	switch {
	case strings.HasPrefix(key, "tool:"):
		v, err := f.Values()
		if err != nil {
			a.setFlash("%v", err)
			return nil
		}
		args = v
	case strings.HasPrefix(key, "prompt:"), strings.HasPrefix(key, "tmpl:"):
		args, _ = json.Marshal(f.StringValues())
	}
	cmd := a.invoke(a.tab, key, args)
	if a.tab == tabChat && cmd != nil {
		a.closeCompose()
	}
	return cmd
}

// invoke runs the request for key with args, recording it in the chat. It
// asks for confirmation first where the profile's safety rules require it.
func (a *App) invoke(tab tabID, key string, args json.RawMessage) tea.Cmd {
	if a.sess == nil || a.state != stateConnected {
		a.setFlash("not connected (R to reconnect)")
		return nil
	}
	if tab != tabChat {
		if rs := a.results[tab]; rs != nil && rs.running {
			a.setFlash("a request is already running (ctrl+c to cancel)")
			return nil
		}
	}
	if tab == tabChat {
		a.chat.follow = true
	}
	kind, name, _ := strings.Cut(key, ":")
	if len(args) > 0 {
		a.lastArgs[key] = args
	}
	strArgs := func() map[string]string {
		vals := map[string]string{}
		var raw map[string]any
		_ = json.Unmarshal(args, &raw)
		for k, v := range raw {
			if s, ok := v.(string); ok {
				vals[k] = s
			} else {
				b, _ := json.Marshal(v)
				vals[k] = string(b)
			}
		}
		return vals
	}
	switch kind {
	case "tool":
		t := a.findTool(name)
		if t == nil {
			a.setFlash("no tool named %q", name)
			return nil
		}
		run := func() tea.Cmd { return a.callTool(tab, t, args) }
		if a.profile.NeedsConfirm(t.Annotations.IsReadOnly(), t.Annotations.IsDestructive()) {
			why := "Call " + t.Name + "?"
			if t.Annotations.IsDestructive() {
				why = "⚠ " + t.Name + " is marked destructive. Call it?"
			} else if a.profile.EnvBadge == "prod" {
				why = "This is a prod profile and " + t.Name + " is not read-only. Call it?"
			}
			shown := string(args)
			if shown == "" {
				shown = "{}"
			}
			a.overlay = newConfirmOverlay(a, why+"\n\n"+shown, run)
			return func() tea.Msg { return nil }
		}
		return run()
	case "prompt":
		vals := strArgs()
		return a.startCall(tab, key, "prompt", name, args, func(ctx context.Context, done *callDoneMsg) {
			done.prompt, done.ex = a.sess.Client.GetPrompt(ctx, name, vals)
		})
	case "res":
		return a.startCall(tab, key, "resource", name, nil, func(ctx context.Context, done *callDoneMsg) {
			done.read, done.ex = a.sess.Client.ReadResource(ctx, name)
		})
	case "tmpl":
		uri := uritemplate.Expand(name, strArgs())
		return a.startCall(tab, key, "template", uri, args, func(ctx context.Context, done *callDoneMsg) {
			done.read, done.ex = a.sess.Client.ReadResource(ctx, uri)
		})
	case "ping":
		return a.startCall(tab, key, "ping", "ping", nil, func(ctx context.Context, done *callDoneMsg) {
			done.ex = a.sess.Client.Ping(ctx)
		})
	}
	return nil
}

func (a *App) callTool(tab tabID, t *mcp.Tool, args json.RawMessage) tea.Cmd {
	br := a.br
	cmd := a.startCall(tab, "tool:"+t.Name, "tool", t.Name, args, func(ctx context.Context, done *callDoneMsg) {
		opts := &mcp.RequestOptions{OnProgress: func(p mcp.ProgressParams) {
			br.send(progressMsg{tab: tab, run: done.run, p: p})
		}}
		done.tool, done.ex = a.sess.Client.CallTool(ctx, t.Name, args, opts)
	})
	if rs := a.runs[runCounter]; rs != nil {
		rs.toolDef = t
	}
	return cmd
}

var runCounter int

func (a *App) startCall(tab tabID, key, kind, name string, args json.RawMessage, do func(ctx context.Context, done *callDoneMsg)) tea.Cmd {
	runCounter++
	run := runCounter
	timeout, _ := a.profile.TimeoutDuration()
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	title := name
	rs := &resultState{title: title, subject: key, kind: kind, run: run, running: true, started: time.Now(), cancel: cancel, args: args, dirty: true}
	if prev := a.results[tab]; prev != nil && prev.static == "" {
		rs.mode = prev.mode
	}
	a.runs[run] = rs
	if tab != tabChat {
		a.results[tab] = rs
	}
	a.addChatItem(&chatItem{run: run, key: key, kind: kind, name: name, args: args, rs: rs, origin: tab})
	a.prog.SetPercent(0)
	sess := a.sess
	return tea.Batch(a.spin.Tick, func() tea.Msg {
		defer cancel()
		done := callDoneMsg{tab: tab, run: run, subject: key}
		if sess == nil {
			return done
		}
		do(ctx, &done)
		return done
	})
}

func (a *App) onCallDone(msg callDoneMsg) tea.Cmd {
	rs := a.runs[msg.run]
	item := a.chatItemByRun(msg.run)
	if msg.ex != nil && item != nil {
		e := a.recordExchange(msg.ex, item.kind, item.name, item.args)
		item.entry = &e
	}
	if rs == nil {
		return nil
	}
	rs.running = false
	rs.cancel = nil
	rs.ex = msg.ex
	rs.tool, rs.prompt, rs.read = msg.tool, msg.prompt, msg.read
	rs.dirty = true
	rs.sv.offset = 0
	if msg.ex != nil && msg.ex.Err != nil {
		if errors.Is(msg.ex.Err, context.Canceled) {
			a.setFlash("cancelled")
		}
		if isClientRejected(msg.ex.Err) {
			a.setFlash("the server rejected our client registration; press x in the list to reset it")
		}
	}
	if msg.tab != tabChat && a.tab == msg.tab && a.results[msg.tab] == rs && a.focus != focusList {
		a.blurForm()
		a.focus = focusResult
	}
	return nil
}

func isClientRejected(err error) bool {
	return errors.Is(err, auth.ErrClientRejected)
}

// ---------------------------------------------------------------- completion

func (a *App) complete() tea.Cmd {
	init := a.sess
	if init == nil || a.sess.Client.InitializeResult() == nil || a.sess.Client.InitializeResult().Capabilities.Completions == nil {
		a.setFlash("server does not support completions")
		return nil
	}
	key := a.currentKey()
	f := a.formFor(key)
	if f == nil {
		return nil
	}
	kind, name, _ := strings.Cut(key, ":")
	var ref mcp.CompleteRef
	switch kind {
	case "prompt":
		ref = mcp.CompleteRef{Type: "ref/prompt", Name: name}
	case "tmpl":
		ref = mcp.CompleteRef{Type: "ref/resource", URI: name}
	default:
		return nil
	}
	fields := f.Fields()
	i := f.FocusedField()
	if i < 0 || i >= len(fields) {
		return nil
	}
	vals := f.StringValues()
	field := fields[i].Name
	current := vals[field]
	// Repeated ctrl+space cycles through the previous suggestions.
	if lc := a.lastCompletion; lc != nil && lc.key == key && lc.field == field && len(lc.values) > 1 {
		for j, v := range lc.values {
			if v == current {
				vals[field] = lc.values[(j+1)%len(lc.values)]
				b, _ := json.Marshal(vals)
				_ = f.SetValues(b)
				a.setFlash("completion %d/%d", (j+1)%len(lc.values)+1, len(lc.values))
				return nil
			}
		}
	}
	c := a.sess.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, ex := c.Complete(ctx, ref, field, current, vals)
		if ex.Err != nil {
			return flashMsg{text: "completion failed: " + ex.Err.Error()}
		}
		return completionMsg{key: key, field: field, prefix: current, values: res.Completion.Values}
	}
}

type completionMsg struct {
	key, field, prefix string
	values             []string
}

func (a *App) applyCompletion(msg completionMsg) {
	a.lastCompletion = &msg
	f := a.forms[msg.key]
	if f == nil || len(msg.values) == 0 {
		a.setFlash("no completions")
		return
	}
	vals := f.StringValues()
	vals[msg.field] = msg.values[0]
	b, _ := json.Marshal(vals)
	_ = f.SetValues(b)
	if len(msg.values) > 1 {
		a.setFlash("completions (ctrl+space for next): %s", strings.Join(msg.values, ", "))
	}
}

// ---------------------------------------------------------------- subscribe

func (a *App) toggleSubscribe() tea.Cmd {
	key := a.selectedKey()
	uri, ok := strings.CutPrefix(key, "res:")
	if !ok || a.sess == nil {
		return nil
	}
	init := a.sess.Client.InitializeResult()
	if init == nil || init.Capabilities.Resources == nil || !init.Capabilities.Resources.Subscribe {
		a.setFlash("server does not support resource subscriptions")
		return nil
	}
	c := a.sess.Client
	sub := !a.subscribed[uri]
	a.subscribed[uri] = sub
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var ex *mcp.Exchange
		if sub {
			ex = c.Subscribe(ctx, uri)
		} else {
			ex = c.Unsubscribe(ctx, uri)
		}
		if ex.Err != nil {
			return flashMsg{text: fmt.Sprintf("%s failed: %v", ex.Method, ex.Err)}
		}
		if sub {
			return flashMsg{text: "subscribed to " + uri}
		}
		return flashMsg{text: "unsubscribed from " + uri}
	}
}

// ---------------------------------------------------------------- copy/save

func (a *App) copyResult(rs *resultState) tea.Cmd {
	var text string
	switch {
	case rs.custom != nil:
		text = stripANSI(rs.custom(200, rs.mode))
	case rs.mode == modeRaw && rs.ex != nil:
		text = string(rs.ex.Response)
	case rs.static != "":
		text = stripANSI(rs.static)
	case rs.tool != nil && len(rs.tool.Content) == 1 && rs.tool.Content[0].Type == "text":
		text = rs.tool.Content[0].Text
	case rs.read != nil && len(rs.read.Contents) == 1 && rs.read.Contents[0].Text != nil:
		text = *rs.read.Contents[0].Text
	case rs.ex != nil && rs.ex.Result != nil:
		p, _ := prettyJSON(rs.ex.Result)
		text = p
	}
	if text == "" {
		a.setFlash("nothing to copy")
		return nil
	}
	a.setFlash("copied %d bytes to clipboard (OSC 52)", len(text))
	return tea.SetClipboard(text)
}

func (a *App) copyCommand(subject string, args json.RawMessage, curl bool, ex *mcp.Exchange) tea.Cmd {
	kind, name, _ := strings.Cut(subject, ":")
	var text string
	if curl {
		if ex == nil || len(ex.Request) == 0 {
			a.setFlash("run the request first to copy it as curl")
			return nil
		}
		sid := ""
		if a.sess != nil {
			sid = a.sess.SessionID()
		}
		s, err := copycmd.Curl(a.profile, ex.Request, sid)
		if err != nil {
			a.setFlash("%v", err)
			return nil
		}
		text = s
	} else {
		switch kind {
		case "tool":
			text = copycmd.Mcptui(a.profile, "tool", name, args)
		case "prompt":
			text = copycmd.Mcptui(a.profile, "prompt", name, args)
		case "res":
			text = copycmd.Mcptui(a.profile, "resource", name, nil)
		case "tmpl":
			var vals map[string]string
			_ = json.Unmarshal(args, &vals)
			text = copycmd.Mcptui(a.profile, "resource", uritemplate.Expand(name, vals), nil)
		default:
			return nil
		}
	}
	a.setFlash("copied: %s", oneLine(text))
	return tea.SetClipboard(text)
}

// saveResult writes the result to path. Binary blocks are decoded.
func saveResult(rs *resultState, path string) (int, error) {
	var data []byte
	switch {
	case rs.tool != nil:
		for _, c := range rs.tool.Content {
			if c.Data != "" {
				b, err := base64.StdEncoding.DecodeString(c.Data)
				if err != nil {
					return 0, err
				}
				data = b
				break
			}
			if c.Resource != nil && c.Resource.Blob != nil {
				b, err := base64.StdEncoding.DecodeString(*c.Resource.Blob)
				if err != nil {
					return 0, err
				}
				data = b
				break
			}
		}
		if data == nil && len(rs.tool.Content) == 1 && rs.tool.Content[0].Type == "text" {
			data = []byte(rs.tool.Content[0].Text)
		}
	case rs.read != nil && len(rs.read.Contents) > 0:
		c := rs.read.Contents[0]
		if c.Blob != nil {
			b, err := base64.StdEncoding.DecodeString(*c.Blob)
			if err != nil {
				return 0, err
			}
			data = b
		} else if c.Text != nil {
			data = []byte(*c.Text)
		}
	}
	if data == nil && rs.ex != nil {
		if rs.mode == modeRaw {
			data = rs.ex.Response
		} else {
			p, _ := prettyJSON(rs.ex.Result)
			data = []byte(p)
		}
	}
	if data == nil {
		return 0, fmt.Errorf("nothing to save")
	}
	return len(data), os.WriteFile(path, data, 0o644)
}

// ---------------------------------------------------------------- editors

func (a *App) currentArgsJSON() json.RawMessage {
	f := a.currentForm()
	if f == nil {
		return json.RawMessage("{}")
	}
	if strings.HasPrefix(a.currentKey(), "tool:") {
		v, err := f.Values()
		if err == nil && len(v) > 0 {
			return v
		}
		return json.RawMessage("{}")
	}
	b, _ := json.Marshal(f.StringValues())
	return b
}

func (a *App) openJSONEditor() tea.Cmd {
	if a.currentForm() == nil {
		return nil
	}
	if strings.HasPrefix(a.currentKey(), "res:") {
		return nil
	}
	a.blurForm()
	a.overlay = newJSONOverlay(a, a.currentKey(), a.currentArgsJSON())
	return a.overlay.init()
}

func (a *App) openExternalEditor() tea.Cmd {
	if a.currentForm() == nil {
		return nil
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	fh, err := os.CreateTemp("", "mcptui-args-*.json")
	if err != nil {
		a.setFlash("%v", err)
		return nil
	}
	p, _ := prettyJSON(a.currentArgsJSON())
	fh.WriteString(p + "\n")
	fh.Close()
	path := fh.Name()
	argv := strings.Fields(editor)
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	key := a.currentKey()
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorDoneMsg{path: path + "\x00" + key, err: err}
	})
}

func (a *App) onEditorDone(msg editorDoneMsg) tea.Cmd {
	path, key, _ := strings.Cut(msg.path, "\x00")
	defer os.Remove(path)
	if msg.err != nil {
		a.setFlash("editor: %v", msg.err)
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		a.setFlash("%v", err)
		return nil
	}
	return a.applyArgsJSON(key, b)
}

func (a *App) applyArgsJSON(key string, b []byte) tea.Cmd {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		a.setFlash("invalid JSON object: %v", err)
		return nil
	}
	f := a.formFor(key)
	if f == nil {
		return nil
	}
	if !strings.HasPrefix(key, "tool:") {
		// String forms: stringify values.
		strs := map[string]string{}
		for k, v := range obj {
			if s, ok := v.(string); ok {
				strs[k] = s
			} else {
				vb, _ := json.Marshal(v)
				strs[k] = string(vb)
			}
		}
		b, _ = json.Marshal(strs)
	}
	if err := f.SetValues(b); err != nil {
		a.setFlash("%v", err)
		return nil
	}
	a.setFlash("arguments updated")
	a.focus = focusForm
	return f.Focus()
}

// ---------------------------------------------------------------- detail view

func (a *App) resultHeight() int {
	l := a.layout()
	return max(l.bodyH*55/100-1, 3)
}

func (a *App) renderDetail(width, height int) string {
	th := a.th
	key := a.selectedKey()
	if key == "" {
		msg := "nothing selected"
		kind := map[tabID]string{tabTools: "tools", tabPrompts: "prompts", tabResources: "resources"}[a.tab]
		if err := a.listErr[kind]; err != nil {
			msg = "failed to list " + kind + ": " + err.Error()
		} else if a.loading[kind] {
			msg = "loading…"
		}
		return fitLines("\n "+th.dimText.Render(wrap(msg, width-2)), width, height)
	}
	f := a.formFor(key)
	rs := a.results[a.tab]
	if rs != nil && rs.subject != key && !rs.running {
		rs = nil // result belongs to another item
	}

	topH := height
	resH := 0
	if rs != nil {
		resH = a.resultHeight() + 1
		topH = height - resH
	}
	top := a.renderDoc(key, f, width, topH)
	a.resultY = 2 + topH
	if rs == nil {
		return fitLines(top, width, height)
	}
	return fitLines(top, width, topH) + "\n" + a.renderResult(rs, width, resH)
}

// renderDoc renders title, description, form and action hints.
func (a *App) renderDoc(key string, f *schemaform.Form, width, height int) string {
	th := a.th
	kind, name, _ := strings.Cut(key, ":")
	var head []string
	var desc string
	switch kind {
	case "tool":
		t := a.findTool(name)
		if t == nil {
			return ""
		}
		title := th.title.Render(t.Name)
		if dt := t.DisplayTitle(); dt != t.Name {
			title += " " + th.dimText.Render(dt)
		}
		if m := a.toolMarkers(t, false); m != "" {
			title += "  " + m
		}
		head = append(head, title)
		desc = t.Description
	case "prompt":
		p := a.findPrompt(name)
		if p == nil {
			return ""
		}
		head = append(head, th.title.Render(p.Name)+" "+th.dimText.Render(p.Title))
		desc = p.Description
	case "res":
		r := a.findResource(name)
		if r == nil {
			return ""
		}
		title := th.title.Render(firstNonEmpty(r.Title, r.Name))
		if a.subscribed[r.URI] {
			title += " " + th.pill("subscribed", th.info)
		}
		head = append(head, title, th.infoText.Render(r.URI))
		meta := []string{}
		if r.MimeType != "" {
			meta = append(meta, r.MimeType)
		}
		if r.Size != nil {
			meta = append(meta, humanBytes(int(*r.Size)))
		}
		if len(meta) > 0 {
			head = append(head, th.dimText.Render(strings.Join(meta, " · ")))
		}
		desc = r.Description
	case "tmpl":
		t := a.findTemplate(name)
		if t == nil {
			return ""
		}
		head = append(head, th.title.Render(firstNonEmpty(t.Title, t.Name))+" "+th.dimText.Render("template"), th.infoText.Render(t.URITemplate))
		if t.MimeType != "" {
			head = append(head, th.dimText.Render(t.MimeType))
		}
		desc = t.Description
	}
	head = append(head, th.sepStyle.Render(strings.Repeat("─", max(width-1, 1))))

	var formView string
	if f != nil && len(f.Fields()) > 0 {
		formView = f.View()
	}
	actions := a.actionHints(key, kind, f, height)

	// Budget: head + description (truncated) + form (scrolled) + actions.
	budget := height - len(head) - 1
	descLines := []string{}
	if strings.TrimSpace(desc) != "" {
		descLines = strings.Split(a.rd.markdown(desc, max(width-2, 20)), "\n")
	}
	formLines := []string{}
	if formView != "" {
		formLines = strings.Split(formView, "\n")
	}
	maxDesc := len(descLines)
	if len(formLines) > 0 {
		maxDesc = min(maxDesc, max(budget/3, 2))
	}
	if maxDesc > budget {
		maxDesc = max(budget, 0)
	}
	if len(descLines) > maxDesc {
		descLines = append(descLines[:max(maxDesc-1, 0)], th.faintText.Render("  … (d for full description)"))
	}
	remain := max(budget-len(descLines), 0)
	formStart := 0
	if len(formLines) > remain && remain > 0 {
		// Keep the focused field visible.
		focusLine := max(f.FieldLine(max(f.FocusedField(), 0)), 0)
		formStart = min(max(focusLine-remain/3, 0), len(formLines)-remain)
		formLines = formLines[formStart : formStart+remain]
	}
	lines := append(head, descLines...)
	if len(descLines) > 0 && len(formLines) > 0 {
		lines = append(lines, "")
	}
	formTop := len(lines)
	lines = append(lines, formLines...)

	// Clicking a form line focuses (or toggles) that field; anywhere else in
	// the doc area focuses the form.
	visible := len(formLines)
	a.addZone(zone{x0: a.detailX, y0: 2, x1: a.detailX + width, y1: 2 + height,
		click: func(x, y int, double bool) tea.Cmd {
			if f == nil || len(f.Fields()) == 0 {
				return nil
			}
			if y >= formTop && y < formTop+visible {
				if fi := f.FieldAtLine(formStart + y - formTop); fi >= 0 {
					a.focus = focusForm
					return f.ClickField(fi)
				}
			}
			return a.focusFormArea()
		},
	})
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:min(len(lines), height-1)], actions)
	for i, l := range lines {
		lines[i] = " " + l
	}
	return strings.Join(lines, "\n")
}

func (a *App) actionHints(key, kind string, f *schemaform.Form, height int) string {
	var parts []string
	btn := func(label, keyName string, action func() tea.Cmd) {
		parts = append(parts, a.renderButton(label, keyName))
		a.addButton(a.detailX, 2, a.width, 2+height, buttonLabel(label, keyName), action)
	}
	caps := func() *mcp.InitializeResult {
		if a.sess == nil {
			return nil
		}
		return a.sess.Client.InitializeResult()
	}()
	switch kind {
	case "tool":
		btn("Call", "enter", a.execute)
		btn("Edit JSON", "e", a.openJSONEditor)
		btn("Copy cmd", "c", func() tea.Cmd { return a.copyCommand(key, a.currentArgsJSON(), false, nil) })
		btn("Docs", "d", func() tea.Cmd { a.overlay = newDocOverlay(a, key); return nil })
	case "prompt":
		btn("Get", "enter", a.execute)
		btn("Edit JSON", "e", a.openJSONEditor)
		if caps != nil && caps.Capabilities.Completions != nil {
			btn("Complete", "ctrl+space", a.complete)
		}
	case "res":
		btn("Read", "enter", a.execute)
		if caps != nil && caps.Capabilities.Resources != nil && caps.Capabilities.Resources.Subscribe {
			label := "Subscribe"
			if a.subscribed[strings.TrimPrefix(key, "res:")] {
				label = "Unsubscribe"
			}
			btn(label, "S", a.toggleSubscribe)
		}
	case "tmpl":
		btn("Read", "enter", a.execute)
	}
	return strings.Join(parts, " ")
}

func (a *App) renderResult(rs *resultState, width, height int) string {
	th := a.th
	var status string
	switch {
	case rs.running:
		el := time.Since(rs.started).Round(100 * time.Millisecond)
		status = th.warnText.Render(a.spin.View()+" running "+el.String()) + th.dimText.Render("  ctrl+c cancel")
	case rs.ex != nil && rs.ex.Err != nil:
		if errors.Is(rs.ex.Err, context.Canceled) {
			status = th.warnText.Render("⊘ cancelled")
		} else {
			status = th.badText.Render("✗ error")
		}
		status += " " + th.dimText.Render(rs.ex.Duration.Round(time.Millisecond).String())
	case rs.tool != nil && rs.tool.IsError:
		status = th.badText.Render("✗ isError") + " " + th.dimText.Render(rs.ex.Duration.Round(time.Millisecond).String())
	case rs.ex != nil:
		status = th.okText.Render("✓") + " " + th.dimText.Render(rs.ex.Duration.Round(time.Millisecond).String())
	}
	resY := a.resultY
	modes := []string{}
	for _, m := range []viewMode{modePretty, modeMarkdown, modeRaw} {
		label := "(" + m.String()[:1] + ")" + m.String()[1:]
		if m == rs.mode {
			modes = append(modes, th.bold.Render(label))
		} else {
			modes = append(modes, th.dimText.Render(label))
		}
		mode := m
		a.addButton(a.detailX, resY+1, a.detailX+width, resY+2, label, func() tea.Cmd {
			rs.mode, rs.dirty = mode, true
			a.blurForm()
			a.focus = focusResult
			return nil
		})
	}
	if !rs.running {
		modes = append(modes, " "+a.renderButton("Copy", "y"), a.renderButton("Save", "s"))
		for _, b := range [][2]string{{"Copy", "y"}, {"Save", "s"}} {
			k := b[1]
			a.addButton(a.detailX, resY+1, a.detailX+width, resY+2, buttonLabel(b[0], b[1]), func() tea.Cmd {
				a.blurForm()
				a.focus = focusResult
				return a.resultKey(keyFromString(k))
			})
		}
	} else {
		modes = append(modes, " "+a.renderButton("Cancel", "ctrl+c"))
		a.keyButton(a.detailX, resY+1, a.detailX+width, resY+2, buttonLabel("Cancel", "ctrl+c"), "ctrl+c")
	}
	bodyRows := max(height-2, 1)
	a.addZone(zone{x0: a.detailX, y0: resY, x1: a.detailX + width, y1: resY + height,
		click: func(int, int, bool) tea.Cmd {
			a.blurForm()
			a.focus = focusResult
			return nil
		},
		wheel: func(d int) tea.Cmd {
			rs.sv.scroll(d, bodyRows)
			return nil
		},
	})
	title := th.bold.Render("Result") + " " + th.dimText.Render(rs.title) + "  " + status + "   " + strings.Join(modes, " ")
	focusMark := th.sepStyle.Render("─")
	if a.focus == focusResult {
		focusMark = th.accentText.Render("━")
	}
	rule := strings.Repeat(focusMark, max(width-1, 1))
	header := " " + truncate(title, width-1)

	bodyH := max(height-2, 1)
	var body string
	if rs.running {
		var lines []string
		if rs.progress != nil {
			p := rs.progress
			line := a.prog.View()
			if p.Total == nil {
				line = th.infoText.Render(fmt.Sprintf("progress %g", p.Progress))
			}
			lines = append(lines, line)
			if p.Message != "" {
				lines = append(lines, th.dimText.Render(p.Message))
			}
		} else {
			lines = append(lines, th.dimText.Render("waiting for response…"))
		}
		body = fitLines(indent(strings.Join(lines, "\n"), " "), width, bodyH)
	} else {
		rs.render(a, width-1)
		body = indent(rs.sv.view(width-1, bodyH), " ")
		if len(rs.sv.lines) > bodyH {
			pct := 100 * (rs.sv.offset + bodyH) / len(rs.sv.lines)
			header = " " + truncate(title+th.faintText.Render(fmt.Sprintf("  %d%%", min(pct, 100))), width-1)
		}
	}
	return rule + "\n" + fitLines(header, width, 1) + "\n" + body
}

// ---------------------------------------------------------------- log

func (a *App) logLines() []string {
	th := a.th
	var lines []string
	for _, e := range a.logs {
		if e.source == "traffic" && !a.showTraffic {
			continue
		}
		src := fmt.Sprintf("%-9s", e.source)
		var st lipgloss.Style
		switch e.level {
		case "error", "critical", "alert", "emergency":
			st = th.badText
		case "warn", "warning":
			st = th.warnText
		case "debug":
			st = th.faintText
		default:
			st = lipgloss.NewStyle()
		}
		srcStyle := th.infoText
		switch e.source {
		case "auth":
			srcStyle = th.accentText
		case "stderr":
			srcStyle = th.warnText
		case "server":
			srcStyle = th.okText
		case "traffic":
			srcStyle = th.faintText
		}
		text := e.text
		if e.source == "traffic" {
			text = redactTraffic(text)
		}
		lines = append(lines, th.faintText.Render(e.time.Format("15:04:05.000"))+" "+srcStyle.Render(src)+" "+st.Render(text))
	}
	return lines
}

// redactTraffic hides anything that looks like a bearer token.
func redactTraffic(s string) string {
	idx := strings.Index(strings.ToLower(s), "bearer ")
	for idx >= 0 {
		start := idx + len("bearer ")
		end := start
		for end < len(s) && !strings.ContainsRune(" \"',}", rune(s[end])) {
			end++
		}
		s = s[:start] + auth.Redact(s[start:end]) + s[end:]
		next := strings.Index(strings.ToLower(s[start+1:]), "bearer ")
		if next < 0 {
			break
		}
		idx = start + 1 + next
	}
	return s
}

func (a *App) refreshLogView() {
	l := a.layout()
	h := l.bodyH
	var wrapped []string
	for _, line := range a.logLines() {
		wrapped = append(wrapped, strings.Split(wrap(line, max(a.width-1, 10)), "\n")...)
	}
	a.logVP.lines = wrapped
	if a.logFollow {
		a.logVP.bottom(h)
	} else {
		a.logVP.clamp(h)
	}
}

func (a *App) renderLog(l layout) string {
	if len(a.logVP.lines) == 0 || (len(a.logVP.lines) == 1 && a.logVP.lines[0] == "") {
		return fitLines("\n "+a.th.dimText.Render("no log entries yet"), a.width, l.bodyH)
	}
	return a.logVP.view(a.width, l.bodyH)
}

func (a *App) logKey(k tea.KeyPressMsg) tea.Cmd {
	h := a.layout().bodyH
	switch k.String() {
	case "esc", "shift+tab":
		return a.focusTabBar()
	case "up", "k":
		if a.logVP.offset == 0 {
			return a.focusTabBar()
		}
		a.logFollow = false
		a.logVP.scroll(-1, h)
	case "down", "j":
		a.logVP.scroll(1, h)
		a.logFollow = a.logVP.atBottom(h)
	case "pgup", "b":
		a.logFollow = false
		a.logVP.scroll(-h, h)
	case "pgdown", "space", "f":
		a.logVP.scroll(h, h)
		a.logFollow = a.logVP.atBottom(h)
	case "g", "home":
		a.logFollow = false
		a.logVP.offset = 0
	case "G", "end":
		a.logFollow = true
		a.refreshLogView()
	case "t":
		a.showTraffic = !a.showTraffic
		a.refreshLogView()
		if a.showTraffic {
			a.setFlash("showing raw JSON-RPC traffic")
		} else {
			a.setFlash("hiding raw traffic")
		}
	case "c":
		a.logs = nil
		a.refreshLogView()
	case "y":
		var b strings.Builder
		for _, line := range a.logLines() {
			b.WriteString(stripANSI(line) + "\n")
		}
		a.setFlash("copied log")
		return tea.SetClipboard(b.String())
	}
	return nil
}

func stripANSI(s string) string {
	return ansiStrip(s)
}
