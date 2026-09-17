package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ideonate/mcptui/internal/history"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/uritemplate"
)

// The Chat tab is a transcript of every request in the session, with a
// message box for firing off new ones: `add {"a":1}`, `greet name=Ada`,
// `read test://readme`. Picking something that needs arguments opens its
// form on the right; the right pane otherwise shows the selected entry in
// full.

// chatItem is one request in the transcript.
type chatItem struct {
	run    int    // startCall run id (0 for recorded list/initialize requests)
	key    string // item key: tool:x, prompt:x, res:uri, tmpl:t, ping:
	kind   string // tool | prompt | resource | template | list | init | ping
	name   string
	args   json.RawMessage
	rs     *resultState
	entry  *history.Entry
	origin tabID

	// Non-call entries: init (connection), notify (server notification),
	// server-request, event (connect/disconnect/login).
	at      time.Time
	title   string   // event headline
	lines   []string // event detail
	payload json.RawMessage
	reply   json.RawMessage
}

// hidden reports whether the item is background noise (list requests).
func (it *chatItem) hidden() bool { return it.kind == "list" }

// isCall reports whether the item is a request the user made.
func (it *chatItem) isCall() bool {
	switch it.kind {
	case "tool", "prompt", "resource", "template", "ping":
		return true
	}
	return false
}

func (it *chatItem) time() time.Time {
	if it.entry != nil {
		return it.entry.Time
	}
	return it.at
}

type suggestion struct {
	icon, label, desc string
	insert            string // replaces the whole input
	category          bool   // a menu entry that lists items rather than a call
}

type chatState struct {
	items   []*chatItem
	sel     int // index into visible()
	follow  bool
	offset  int
	showAll bool

	input     textinput.Model
	sugg      []suggestion
	suggIdx   int    // -1: nothing highlighted
	suggOff2  int    // first visible suggestion row
	suggTitle string // header shown above a category list
	suggOff   bool   // hidden with esc until the input changes
	lastInput string

	compose string // key whose form is open on the right

	cmdHist []string
	histIdx int

	lineItem []int // transcript line → visible item index (-1 none)
}

func newChatState() *chatState {
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "tool, prompt or resource name…"
	return &chatState{follow: true, input: in, suggIdx: -1, lastInput: "\x00"}
}

func (c *chatState) visible() []*chatItem {
	if c.showAll {
		return c.items
	}
	out := make([]*chatItem, 0, len(c.items))
	for _, it := range c.items {
		if !it.hidden() {
			out = append(out, it)
		}
	}
	return out
}

func (c *chatState) selected() *chatItem {
	v := c.visible()
	if c.sel < 0 || c.sel >= len(v) {
		return nil
	}
	return v[c.sel]
}

func kindIcon(kind string) string {
	switch kind {
	case "tool":
		return "⚒"
	case "prompt":
		return "✎"
	case "resource", "res":
		return "▤"
	case "template", "tmpl":
		return "⧉"
	case "ping":
		return "↔"
	case "init":
		return "●"
	case "notify":
		return "◂"
	case "server-request":
		return "⇠"
	}
	return "·"
}

// ---------------------------------------------------------------- items

// addChatItem appends an item and follows it if the view is following.
func (a *App) addChatItem(it *chatItem) {
	c := a.chat
	if it.at.IsZero() {
		it.at = time.Now()
	}
	cur := c.selected()
	c.items = append(c.items, it)
	if it.hidden() && !c.showAll {
		return
	}
	// Following: new calls and events take the selection, but a notification
	// doesn't pull it away from a call you're looking at.
	if c.follow && (cur == nil || (it.kind != "notify" && (it.isCall() || !cur.isCall()))) {
		c.sel = len(c.visible()) - 1
		a.syncChatResult()
	} else if cur != nil {
		for i, v := range c.visible() {
			if v == cur {
				c.sel = i
			}
		}
	}
}

// addEvent records something that happened locally (connection, login).
func (a *App) addEvent(icon, title, detail string) {
	it := &chatItem{kind: "event", name: title, title: icon + " " + title, origin: a.tab}
	if detail != "" {
		it.lines = strings.Split(detail, "\n")
	}
	it.rs = &resultState{title: title, kind: "event", dirty: true}
	it.rs.custom = func(w int, mode viewMode) string {
		return a.th.bold.Render(title) + "\n\n" + wrap(detail, w)
	}
	a.addChatItem(it)
}

// addNotification records a server notification.
func (a *App) addNotification(method string, params json.RawMessage) {
	it := &chatItem{kind: "notify", name: method, payload: params, origin: a.tab}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	it.rs = &resultState{title: method, kind: "notify", dirty: true}
	it.rs.custom = func(w int, mode viewMode) string {
		if mode == modeRaw {
			return a.rd.section("← notification", w) + "\n" + a.rd.jsonBlock(raw, w)
		}
		var parts []string
		if method == "notifications/message" {
			var p mcp.LogMessageParams
			_ = json.Unmarshal(params, &p)
			var text string
			if json.Unmarshal(p.Data, &text) != nil {
				text = a.rd.jsonBlock(p.Data, w)
			}
			parts = append(parts, a.th.bold.Render("log "+p.Level)+a.th.dimText.Render(" "+p.Logger), "", wrap(text, w))
		}
		switch method {
		case "notifications/tools/list_changed", "notifications/prompts/list_changed", "notifications/resources/list_changed":
			parts = append(parts, wrap("The server says this list changed, so mcptui fetched it again.", w))
		}
		if len(params) > 0 && string(params) != "{}" && string(params) != "null" {
			parts = append(parts, a.rd.section("params", w), a.rd.jsonBlock(params, w))
		} else {
			parts = append(parts, a.th.dimText.Render("(no params)"))
		}
		return strings.Join(parts, "\n")
	}
	a.addChatItem(it)
}

// addServerRequest records a request the server sent us, and our reply.
func (a *App) addServerRequest(msg serverRequestMsg) {
	it := &chatItem{kind: "server-request", name: msg.method, payload: msg.params, reply: msg.reply, origin: a.tab}
	it.rs = &resultState{title: msg.method, kind: "server-request", dirty: true}
	it.rs.custom = func(w int, mode viewMode) string {
		parts := []string{
			wrap("The server asked mcptui to handle "+msg.method+". mcptui doesn't answer server requests yet, so it replied with an error.", w),
			a.rd.section("← params", w), a.rd.jsonBlock(msg.params, w),
			a.rd.section("→ reply", w), a.rd.jsonBlock(msg.reply, w),
		}
		return strings.Join(parts, "\n")
	}
	a.addChatItem(it)
}

// initCustom renders the connection entry: server info and instructions.
func (a *App) initCustom(ex *mcp.Exchange) func(int, viewMode) string {
	return func(w int, mode viewMode) string {
		th := a.th
		if mode == modeRaw || ex.Err != nil {
			return a.rd.renderRaw(ex, w)
		}
		var r mcp.InitializeResult
		_ = json.Unmarshal(ex.Result, &r)
		var caps struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		_ = json.Unmarshal(ex.Result, &caps)
		lines := []string{
			th.bold.Render(firstNonEmpty(r.ServerInfo.Title, r.ServerInfo.Name)) + " " + r.ServerInfo.Version,
			th.dimText.Render("protocol " + r.ProtocolVersion + " · " + a.profile.Target()),
			"",
		}
		if r.Instructions != "" {
			lines = append(lines, a.rd.section("instructions", w), a.rd.markdown(r.Instructions, w), "")
		} else {
			lines = append(lines, th.dimText.Render("(the server sent no instructions)"), "")
		}
		lines = append(lines, a.rd.section("capabilities", w), a.rd.jsonBlock(caps.Capabilities, w))
		return strings.Join(lines, "\n")
	}
}

// headline is the first transcript line of an item.
func (a *App) headline(it *chatItem) string {
	th := a.th
	switch it.kind {
	case "init":
		ex := it.rs.ex
		if ex != nil && ex.Err != nil {
			return th.badText.Render("✗ Could not connect")
		}
		var r mcp.InitializeResult
		if ex != nil {
			_ = json.Unmarshal(ex.Result, &r)
		}
		return th.okText.Render("●") + " Connected to " + th.bold.Render(firstNonEmpty(r.ServerInfo.Title, r.ServerInfo.Name)) + " " + th.dimText.Render(r.ServerInfo.Version)
	case "event":
		return th.bold.Render(it.title)
	case "notify":
		return th.infoText.Render("◂") + " " + th.dimText.Render(it.name)
	case "server-request":
		return th.warnText.Render("⇠") + " server asked: " + th.bold.Render(it.name)
	case "list":
		return "· " + th.dimText.Render(it.name)
	}
	head := kindIcon(it.kind) + " " + th.bold.Render(it.name)
	if args := commandArgs(it.args); args != "" {
		head += " " + th.dimText.Render(args)
	}
	return head
}

func (a *App) chatItemByRun(run int) *chatItem {
	for i := len(a.chat.items) - 1; i >= 0; i-- {
		if a.chat.items[i].run == run {
			return a.chat.items[i]
		}
	}
	return nil
}

// syncChatResult points the chat tab's result pane at the selected item.
func (a *App) syncChatResult() {
	if it := a.chat.selected(); it != nil {
		a.results[tabChat] = it.rs
	} else {
		a.results[tabChat] = nil
	}
}

func (a *App) selectChat(i int) {
	v := a.chat.visible()
	if len(v) == 0 {
		return
	}
	a.chat.sel = min(max(i, 0), len(v)-1)
	// Keep following if nothing but notifications come after the selection.
	a.chat.follow = true
	for _, it := range v[a.chat.sel+1:] {
		if it.kind != "notify" {
			a.chat.follow = false
		}
	}
	a.syncChatResult()
}

// ---------------------------------------------------------------- commands

type chatTarget struct {
	key       string
	args      json.RawMessage
	needsForm bool
}

// parseCommand turns a message-box line into a request.
func (a *App) parseCommand(line string) (*chatTarget, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("type a tool, prompt or resource name")
	}
	word, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(word) {
	case "ping":
		return &chatTarget{key: "ping:"}, nil
	case "call", "tool":
		name, args, _ := strings.Cut(rest, " ")
		if a.findTool(name) == nil {
			return nil, fmt.Errorf("no tool named %q", name)
		}
		return a.toolTarget(name, args)
	case "get", "prompt":
		name, args, _ := strings.Cut(rest, " ")
		if a.findPrompt(name) == nil {
			return nil, fmt.Errorf("no prompt named %q", name)
		}
		return a.promptTarget(name, args)
	case "read", "resource":
		uri, args, _ := strings.Cut(rest, " ")
		return a.readTarget(uri, args)
	}
	switch {
	case a.findTool(word) != nil:
		return a.toolTarget(word, rest)
	case a.findPrompt(word) != nil:
		return a.promptTarget(word, rest)
	case strings.Contains(word, "://"), a.findTemplate(word) != nil:
		return a.readTarget(word, rest)
	}
	for _, r := range a.resources {
		if r.Name == line || r.Title == line {
			return &chatTarget{key: "res:" + r.URI}, nil
		}
	}
	return nil, fmt.Errorf("no tool, prompt or resource named %q", word)
}

func (a *App) toolTarget(name, rest string) (*chatTarget, error) {
	t := a.findTool(name)
	target := &chatTarget{key: "tool:" + name}
	args, err := parseArgs(strings.TrimSpace(rest), func(k string) string { return schemaPropType(t.InputSchema, k) })
	if err != nil {
		return nil, err
	}
	target.args = args
	if args == nil {
		var s struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(t.InputSchema, &s)
		target.needsForm = len(s.Required) > 0
	}
	return target, nil
}

func (a *App) promptTarget(name, rest string) (*chatTarget, error) {
	p := a.findPrompt(name)
	args, err := parseArgs(strings.TrimSpace(rest), func(string) string { return "string" })
	if err != nil {
		return nil, err
	}
	target := &chatTarget{key: "prompt:" + name, args: args}
	var have map[string]any
	_ = json.Unmarshal(args, &have)
	for _, arg := range p.Arguments {
		if _, ok := have[arg.Name]; arg.Required && !ok {
			target.needsForm = true
		}
	}
	return target, nil
}

func (a *App) readTarget(uri, rest string) (*chatTarget, error) {
	if uri == "" {
		return nil, fmt.Errorf("read what? e.g. read test://readme")
	}
	if a.findTemplate(uri) != nil || strings.Contains(uri, "{") {
		args, err := parseArgs(strings.TrimSpace(rest), func(string) string { return "string" })
		if err != nil {
			return nil, err
		}
		target := &chatTarget{key: "tmpl:" + uri, args: args}
		var have map[string]any
		_ = json.Unmarshal(args, &have)
		for _, v := range uritemplate.Variables(uri) {
			if _, ok := have[v]; !ok {
				target.needsForm = true
			}
		}
		return target, nil
	}
	for _, r := range a.resources {
		if r.Name == uri {
			uri = r.URI
		}
	}
	return &chatTarget{key: "res:" + uri}, nil
}

// parseArgs accepts a JSON object or key=value pairs. typeOf gives the JSON
// Schema type for a key so values like 3 or true are sent as numbers or
// booleans only where the schema expects them.
func parseArgs(rest string, typeOf func(string) string) (json.RawMessage, error) {
	if rest == "" {
		return nil, nil
	}
	if strings.HasPrefix(rest, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(rest), &obj); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object or key=value pairs: %v", err)
		}
		return json.RawMessage(rest), nil
	}
	parts, err := splitArgs(rest)
	if err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for _, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", p)
		}
		switch typeOf(k) {
		case "string", "":
			b, _ := json.Marshal(v)
			obj[k] = b
		default:
			if json.Valid([]byte(v)) {
				obj[k] = json.RawMessage(v)
			} else {
				b, _ := json.Marshal(v)
				obj[k] = b
			}
		}
	}
	b, _ := json.Marshal(obj)
	return b, nil
}

// schemaPropType returns the (first non-null) type of a property.
func schemaPropType(schema json.RawMessage, prop string) string {
	var s struct {
		Properties map[string]struct {
			Type  any `json:"type"`
			AnyOf []struct {
				Type any `json:"type"`
			} `json:"anyOf"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	p, ok := s.Properties[prop]
	if !ok {
		return ""
	}
	pick := func(t any) string {
		switch t := t.(type) {
		case string:
			return t
		case []any:
			for _, x := range t {
				if xs, _ := x.(string); xs != "null" {
					return xs
				}
			}
		}
		return ""
	}
	if t := pick(p.Type); t != "" {
		return t
	}
	for _, alt := range p.AnyOf {
		if t := pick(alt.Type); t != "" && t != "null" {
			return t
		}
	}
	return "any"
}

// commandLine renders an item as the text you could type to repeat it.
func commandLine(key string, args json.RawMessage) string {
	kind, name, _ := strings.Cut(key, ":")
	compact := ""
	if len(args) > 0 && string(args) != "{}" && string(args) != "null" {
		var v any
		if json.Unmarshal(args, &v) == nil {
			b, _ := json.Marshal(v)
			compact = " " + string(b)
		}
	}
	switch kind {
	case "prompt", "tool":
		return name + compact
	case "res":
		return "read " + name
	case "tmpl":
		return "read " + name + compact
	case "ping":
		return "ping"
	}
	return name + compact
}

// sendCommand runs what's in the message box.
func (a *App) sendCommand() tea.Cmd {
	c := a.chat
	line := strings.TrimSpace(c.input.Value())
	if line == "" {
		return nil
	}
	target, err := a.parseCommand(line)
	if err != nil {
		a.setFlash("%v", err)
		return nil
	}
	c.cmdHist = append(c.cmdHist, line)
	c.histIdx = len(c.cmdHist)
	c.input.SetValue("")
	a.updateSuggestions()
	if target.needsForm {
		return a.openCompose(target.key, target.args)
	}
	return a.invoke(tabChat, target.key, target.args)
}

// openCompose shows the form for key on the right, prefilled with args.
func (a *App) openCompose(key string, args json.RawMessage) tea.Cmd {
	if key == "ping:" {
		return a.invoke(tabChat, key, nil)
	}
	f := a.formFor(key)
	if f == nil {
		_, name, _ := strings.Cut(key, ":")
		a.setFlash("%s is no longer listed by the server", name)
		return nil
	}
	if len(args) > 0 {
		_ = f.SetValues(args)
	}
	a.chat.compose = key
	a.focus = focusForm
	return f.Focus()
}

func (a *App) closeCompose() {
	a.blurForm()
	a.chat.compose = ""
	a.focusComposer()
}

func (a *App) focusComposer() tea.Cmd {
	a.focus = focusComposer
	a.updateSuggestions()
	return a.chat.input.Focus()
}

// menuMode reports whether the message box is empty and showing the
// Tools / Prompts / Resources bar.
func (c *chatState) menuMode() bool {
	return len(c.sugg) > 0 && c.sugg[0].category && strings.TrimSpace(c.input.Value()) == ""
}

// runAgain repeats the selected item exactly.
func (a *App) runAgain() tea.Cmd {
	it := a.chat.selected()
	if it == nil || !it.isCall() {
		return nil
	}
	a.chat.follow = true
	return a.invoke(tabChat, it.key, it.args)
}

// editItem opens the selected item's form with its arguments.
func (a *App) editItem() tea.Cmd {
	it := a.chat.selected()
	if it == nil || !it.isCall() {
		return nil
	}
	return a.openCompose(it.key, it.args)
}

// ---------------------------------------------------------------- suggestions

func (a *App) updateSuggestions() {
	c := a.chat
	val := c.input.Value()
	if val != c.lastInput {
		c.suggOff = false
		c.lastInput = val
		c.suggIdx = 0
		c.suggOff2 = 0
		if strings.TrimSpace(val) == "" {
			c.suggIdx = -1 // the menu waits for ↓ so ↑ can still recall commands
		}
	}
	c.sugg = nil
	c.suggTitle = ""
	if c.suggOff {
		return
	}
	if strings.TrimSpace(val) == "" {
		c.sugg = a.categoryMenu()
		c.suggTitle = "Browse"
		if c.suggIdx >= len(c.sugg) {
			c.suggIdx = -1
		}
		return
	}
	word, rest, hasRest := strings.Cut(val, " ")
	verb := ""
	switch strings.ToLower(word) {
	case "read", "resource", "call", "tool", "get", "prompt":
		if hasRest {
			verb = strings.ToLower(word)
			word, _, hasRest = strings.Cut(rest, " ")
		}
	}
	if hasRest {
		return // already typing arguments
	}
	switch verb {
	case "call", "tool":
		c.suggTitle = fmt.Sprintf("%s Tools (%d)", kindIcon("tool"), len(a.tools))
	case "get", "prompt":
		c.suggTitle = fmt.Sprintf("%s Prompts (%d)", kindIcon("prompt"), len(a.prompts))
	case "read", "resource":
		c.suggTitle = fmt.Sprintf("%s Resources & templates (%d)", kindIcon("resource"), len(a.resources)+len(a.templates))
	}
	if c.suggTitle != "" && word != "" {
		c.suggTitle += " matching " + strconv.Quote(word)
	}
	q := strings.ToLower(word)
	var prefix, contains []suggestion
	add := func(s suggestion, name string) {
		n := strings.ToLower(name)
		switch {
		case strings.HasPrefix(n, q):
			prefix = append(prefix, s)
		case strings.Contains(n, q):
			contains = append(contains, s)
		}
	}
	if verb == "" || verb == "call" || verb == "tool" {
		for _, t := range a.tools {
			insert := t.Name + " "
			if verb != "" {
				insert = "call " + insert
			}
			add(suggestion{icon: kindIcon("tool"), label: t.Name, desc: t.Description, insert: insert}, t.Name)
		}
	}
	if verb == "" || verb == "get" || verb == "prompt" {
		for _, p := range a.prompts {
			insert := p.Name + " "
			if verb != "" {
				insert = "get " + insert
			}
			add(suggestion{icon: kindIcon("prompt"), label: p.Name, desc: p.Description, insert: insert}, p.Name)
		}
	}
	if verb == "" || verb == "read" || verb == "resource" {
		for _, r := range a.resources {
			s := suggestion{icon: kindIcon("resource"), label: firstNonEmpty(r.Name, r.URI), desc: r.URI, insert: "read " + r.URI}
			add(s, r.Name+" "+r.URI)
		}
		for _, t := range a.templates {
			s := suggestion{icon: kindIcon("template"), label: firstNonEmpty(t.Name, t.URITemplate), desc: t.URITemplate, insert: "read " + t.URITemplate + " "}
			add(s, t.Name+" "+t.URITemplate)
		}
	}
	if q != "" {
		sort.SliceStable(prefix, func(i, j int) bool { return len(prefix[i].label) < len(prefix[j].label) })
	}
	c.sugg = append(prefix, contains...)
	if len(c.sugg) == 1 && strings.TrimSpace(c.sugg[0].insert) == strings.TrimSpace(val) {
		c.sugg = nil // exact match; nothing to offer
	}
	if len(c.sugg) == 0 && verb != "" {
		c.sugg = []suggestion{{icon: " ", label: "(no matches)", insert: val}}
		c.suggIdx = -1
	}
	if c.suggIdx >= len(c.sugg) {
		c.suggIdx = 0
	}
}

// categoryMenu is what the empty message box offers.
func (a *App) categoryMenu() []suggestion {
	var m []suggestion
	if len(a.tools) > 0 {
		m = append(m, suggestion{icon: kindIcon("tool"), label: "Tools", desc: fmt.Sprintf("%d · call a tool", len(a.tools)), insert: "call ", category: true})
	}
	if len(a.prompts) > 0 {
		m = append(m, suggestion{icon: kindIcon("prompt"), label: "Prompts", desc: fmt.Sprintf("%d · get a prompt", len(a.prompts)), insert: "get ", category: true})
	}
	if n := len(a.resources) + len(a.templates); n > 0 {
		m = append(m, suggestion{icon: kindIcon("resource"), label: "Resources & templates", desc: fmt.Sprintf("%d · read a resource", n), insert: "read ", category: true})
	}
	m = append(m, suggestion{icon: kindIcon("ping"), label: "Ping", desc: "check the server responds", insert: "ping"})
	return m
}

func (a *App) acceptSuggestion(i int) {
	c := a.chat
	if i < 0 || i >= len(c.sugg) {
		return
	}
	sg := c.sugg[i]
	c.input.SetValue(sg.insert)
	c.input.CursorEnd()
	if sg.category {
		// Show everything in the category; typing narrows it.
		a.updateSuggestions()
		return
	}
	c.lastInput = c.input.Value()
	c.suggOff = true
	c.sugg = nil
}

// ---------------------------------------------------------------- keys

// isBareVerb reports whether the input is only a category verb like "call ".
func isBareVerb(v string) bool {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "call", "tool", "get", "prompt", "read", "resource":
		return strings.HasSuffix(v, " ")
	}
	return false
}

// composerKey handles input while the message box has focus.
func (a *App) composerKey(msg tea.Msg) tea.Cmd {
	c := a.chat
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "enter":
			if len(c.sugg) > 0 && c.suggIdx >= 0 {
				wasCategory := c.sugg[c.suggIdx].category
				a.acceptSuggestion(c.suggIdx)
				if wasCategory {
					return nil
				}
			}
			if isBareVerb(c.input.Value()) {
				return nil
			}
			return a.sendCommand()
		case "tab":
			if len(c.sugg) > 0 && c.sugg[0].label != "(no matches)" {
				a.acceptSuggestion(max(c.suggIdx, 0))
				return nil
			}
			if c.compose != "" {
				return a.openCompose(c.compose, nil)
			}
			if a.results[tabChat] != nil {
				c.input.Blur()
				a.focus = focusResult
				return nil
			}
			c.input.Blur()
			a.focus = focusTabs
			return nil
		case "shift+tab":
			c.input.Blur()
			a.focus = focusList
			return nil
		case "left", "right":
			if c.menuMode() && c.suggIdx >= 0 {
				d := 1
				if k.String() == "left" {
					d = len(c.sugg) - 1
				}
				c.suggIdx = (c.suggIdx + d) % len(c.sugg)
				return nil
			}
		case "up", "ctrl+p":
			if c.menuMode() && c.suggIdx >= 0 {
				c.suggIdx = -1
				return nil
			}
			if len(c.sugg) > 0 && c.suggIdx >= 0 {
				c.suggIdx--
				if c.suggIdx < 0 && strings.TrimSpace(c.input.Value()) != "" {
					c.suggIdx = len(c.sugg) - 1
				}
				return nil
			}
			if len(c.sugg) > 0 && strings.TrimSpace(c.input.Value()) != "" {
				c.suggIdx = len(c.sugg) - 1
				return nil
			}
			if c.histIdx > 0 {
				c.histIdx--
				c.input.SetValue(c.cmdHist[c.histIdx])
				c.input.CursorEnd()
				c.lastInput, c.suggOff, c.sugg = c.input.Value(), true, nil
			}
			return nil
		case "down", "ctrl+n":
			if c.menuMode() {
				if c.suggIdx < 0 {
					c.suggIdx = 0
				}
				return nil
			}
			if len(c.sugg) > 0 {
				c.suggIdx = (c.suggIdx + 1) % len(c.sugg)
				return nil
			}
			if c.histIdx < len(c.cmdHist) {
				c.histIdx++
				v := ""
				if c.histIdx < len(c.cmdHist) {
					v = c.cmdHist[c.histIdx]
				}
				c.input.SetValue(v)
				c.input.CursorEnd()
				c.lastInput, c.suggOff, c.sugg = v, true, nil
			}
			return nil
		case "esc":
			switch {
			case c.menuMode() && c.suggIdx >= 0:
				c.suggIdx = -1
			case isBareVerb(c.input.Value()):
				c.input.SetValue("") // back to the menu
				a.updateSuggestions()
			case len(c.sugg) > 0 && c.input.Value() != "":
				c.suggOff = true
				c.sugg = nil
			case c.input.Value() != "":
				c.input.SetValue("")
				a.updateSuggestions()
			default:
				c.input.Blur()
				a.focus = focusTabs
			}
			return nil
		case "pgup":
			if len(c.sugg) > 0 {
				c.suggIdx = max(c.suggIdx-8, 0)
				return nil
			}
			a.selectChat(c.sel - 1)
			return nil
		case "pgdown":
			if len(c.sugg) > 0 {
				c.suggIdx = min(c.suggIdx+8, len(c.sugg)-1)
				return nil
			}
			a.selectChat(c.sel + 1)
			return nil
		}
	}
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(msg)
	a.updateSuggestions()
	return cmd
}

// chatKey handles keys on the Chat tab outside the message box and form.
func (a *App) chatKey(k tea.KeyPressMsg) tea.Cmd {
	c := a.chat
	switch a.focus {
	case focusList:
		switch k.String() {
		case "up", "k":
			if c.sel == 0 {
				return a.focusTabBar()
			}
			a.selectChat(c.sel - 1)
		case "down", "j":
			if c.sel >= len(c.visible())-1 {
				return a.focusComposer()
			}
			a.selectChat(c.sel + 1)
		case "home", "g":
			a.selectChat(0)
		case "end", "G":
			a.selectChat(len(c.visible()) - 1)
		case "enter":
			if a.results[tabChat] != nil {
				a.focus = focusResult
			}
		case "e":
			return a.editItem()
		case "r", "ctrl+r":
			return a.runAgain()
		case "a":
			return a.toggleShowAll()
		case "esc", "tab":
			return a.focusComposer()
		case "shift+tab":
			return a.focusTabBar()
		}
		return nil
	case focusResult:
		switch k.String() {
		case "esc", "shift+tab":
			return a.focusComposer()
		case "tab":
			a.focus = focusTabs
			return nil
		case "ctrl+r":
			return a.runAgain()
		case "e":
			return a.editItem()
		case "enter":
			return nil
		}
		if a.results[tabChat] == nil {
			return nil
		}
		return a.resultKey(k)
	}
	return a.focusComposer()
}

func (a *App) toggleShowAll() tea.Cmd {
	c := a.chat
	cur := c.selected()
	c.showAll = !c.showAll
	c.sel = len(c.visible()) - 1
	for i, it := range c.visible() {
		if it == cur {
			c.sel = i
		}
	}
	a.syncChatResult()
	if c.showAll {
		a.setFlash("showing list and initialize requests too")
	} else {
		a.setFlash("hiding list and initialize requests")
	}
	return nil
}

// cancelChat cancels the selected item if running, else the newest running
// one. It reports whether anything was cancelled.
func (a *App) cancelChat() bool {
	if it := a.chat.selected(); it != nil && it.rs != nil && it.rs.running && it.rs.cancel != nil {
		it.rs.cancel()
		return true
	}
	for i := len(a.chat.items) - 1; i >= 0; i-- {
		if rs := a.chat.items[i].rs; rs != nil && rs.running && rs.cancel != nil {
			rs.cancel()
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- previews

// previewLines summarizes an item's response for the transcript.
func (a *App) previewLines(it *chatItem, width int) []string {
	th := a.th
	rs := it.rs
	clip := func(s string) string { return truncate(oneLine(s), max(width, 4)) }
	if rs != nil && rs.running {
		line := a.spin.View() + " running " + time.Since(rs.started).Round(100*time.Millisecond).String()
		if p := rs.progress; p != nil {
			if p.Total != nil && *p.Total > 0 {
				line += fmt.Sprintf(" · %d%%", int(100*p.Progress / *p.Total))
			}
			if p.Message != "" {
				line += " · " + p.Message
			}
		}
		return []string{th.warnText.Render(clip(line))}
	}
	switch it.kind {
	case "event":
		var out []string
		for _, l := range it.lines {
			if len(out) < 3 && strings.TrimSpace(l) != "" {
				out = append(out, th.dimText.Render(clip(l)))
			}
		}
		return out
	case "notify":
		return []string{clip(notificationSummary(it.name, it.payload))}
	case "server-request":
		return []string{th.dimText.Render(clip("declined: mcptui doesn't answer server requests yet"))}
	case "init":
		if rs.ex != nil && rs.ex.Err == nil {
			var r mcp.InitializeResult
			_ = json.Unmarshal(rs.ex.Result, &r)
			var out []string
			for _, l := range strings.Split(r.Instructions, "\n") {
				l = strings.NewReplacer("**", "", "`", "", "__", "").Replace(strings.TrimSpace(strings.TrimLeft(l, "#>*- ")))
				if l != "" && len(out) < 2 {
					out = append(out, th.dimText.Render(clip(l)))
				}
			}
			if len(out) == 0 {
				out = append(out, th.dimText.Render(clip("protocol "+r.ProtocolVersion)))
			}
			return out
		}
	}
	var ex *mcp.Exchange
	if rs != nil {
		ex = rs.ex
	}
	if ex == nil {
		return nil
	}
	if ex.Err != nil {
		if errors.Is(ex.Err, context.Canceled) {
			return []string{th.warnText.Render("⊘ cancelled")}
		}
		return []string{th.badText.Render(clip("✗ " + ex.Err.Error()))}
	}
	var lines []string
	textLines := func(s string, n int) {
		s = strings.TrimSpace(s)
		if p, ok := prettyJSON([]byte(s)); ok && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
			var v any
			_ = json.Unmarshal([]byte(p), &v)
			b, _ := json.Marshal(v)
			lines = append(lines, clip(string(b)))
			return
		}
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if len(lines) >= n {
				break
			}
			lines = append(lines, clip(l))
		}
	}
	switch {
	case rs.tool != nil:
		for _, cb := range rs.tool.Content {
			switch cb.Type {
			case "text":
				textLines(cb.Text, 3)
			case "image", "audio":
				lines = append(lines, clip(fmt.Sprintf("[%s %s %s]", cb.Type, cb.MimeType, humanBytes(base64Len(cb.Data)))))
			case "resource_link":
				lines = append(lines, clip("↗ "+cb.URI))
			case "resource":
				if cb.Resource != nil {
					lines = append(lines, clip("▤ "+cb.Resource.URI))
				}
			}
		}
		if len(lines) == 0 && len(rs.tool.StructuredContent) > 0 {
			textLines(string(rs.tool.StructuredContent), 1)
		}
		if rs.tool.IsError {
			for i := range lines {
				lines[i] = th.badText.Render(lines[i])
			}
			lines = append([]string{th.badText.Render("✗ isError")}, lines...)
		}
	case rs.prompt != nil:
		for _, m := range rs.prompt.Messages {
			if len(lines) >= 3 {
				break
			}
			lines = append(lines, clip(m.Role+": "+contentPreview(m.Content)))
		}
	case rs.read != nil:
		for _, rc := range rs.read.Contents {
			if rc.Text != nil {
				textLines(*rc.Text, 3)
			} else if rc.Blob != nil {
				lines = append(lines, clip(fmt.Sprintf("[binary %s %s]", rc.MimeType, humanBytes(base64Len(*rc.Blob)))))
			}
		}
	default:
		lines = append(lines, clip(summarizeResult(it.kind, ex.Result)))
	}
	if len(lines) > 4 {
		lines = lines[:4]
	}
	if len(lines) == 0 {
		lines = []string{th.dimText.Render("(empty)")}
	}
	return lines
}

// notificationSummary is a one-line description of a notification.
func notificationSummary(method string, params json.RawMessage) string {
	switch method {
	case "notifications/message":
		var p mcp.LogMessageParams
		_ = json.Unmarshal(params, &p)
		text := string(p.Data)
		var s string
		if json.Unmarshal(p.Data, &s) == nil {
			text = s
		}
		if p.Logger != "" {
			text = p.Logger + ": " + text
		}
		return "[" + p.Level + "] " + text
	case "notifications/tools/list_changed", "notifications/prompts/list_changed", "notifications/resources/list_changed":
		return "list changed · refreshed"
	case "notifications/resources/updated":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(params, &p)
		return "updated " + p.URI
	}
	if len(params) == 0 {
		return ""
	}
	var v any
	_ = json.Unmarshal(params, &v)
	b, _ := json.Marshal(v)
	return string(b)
}

func contentPreview(c mcp.Content) string {
	switch c.Type {
	case "text":
		return c.Text
	case "resource":
		if c.Resource != nil {
			return "▤ " + c.Resource.URI
		}
	}
	return "[" + c.Type + "]"
}

func summarizeResult(kind string, result json.RawMessage) string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(result, &m)
	for _, k := range []string{"tools", "prompts", "resources", "resourceTemplates"} {
		if raw, ok := m[k]; ok {
			var items []any
			_ = json.Unmarshal(raw, &items)
			return fmt.Sprintf("%d %s", len(items), k)
		}
	}
	if kind == "init" {
		var r mcp.InitializeResult
		_ = json.Unmarshal(result, &r)
		return fmt.Sprintf("%s %s (protocol %s)", r.ServerInfo.Name, r.ServerInfo.Version, r.ProtocolVersion)
	}
	if kind == "ping" {
		return "pong"
	}
	b, _ := json.Marshal(json.RawMessage(result))
	return string(b)
}

// ---------------------------------------------------------------- render

func (a *App) chatLayout() (leftW, rightW int) {
	if a.width < 80 {
		return a.width, a.width
	}
	leftW = min(max(a.width*40/100, 32), 90)
	return leftW, a.width - leftW - 1
}

func (a *App) renderChat(l layout) string {
	th := a.th
	c := a.chat
	leftW, rightW := a.chatLayout()
	stacked := a.width < 80
	w := a.width

	// The message box (and its browse bar) spans the full width below both
	// panes.
	composerRows := 2
	menu := a.focus == focusComposer && c.menuMode()
	if menu {
		composerRows = 3
	}
	topH := max(l.bodyH-composerRows, 1)

	var top string
	switch {
	case stacked && a.focus != focusResult && a.focus != focusForm:
		top = a.renderTranscript(0, w, topH)
	case stacked:
		a.detailX = 0
		top = a.renderChatDetail(w, topH)
	default:
		left := a.renderTranscript(0, leftW, topH)
		a.detailX = leftW + 1
		right := a.renderChatDetail(rightW, topH)
		sep := make([]string, topH)
		for i := range sep {
			sep[i] = th.sepStyle.Render("│")
		}
		top = lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Join(sep, "\n"), right)
	}
	topLines := strings.Split(fitLines(top, w, topH), "\n")

	var popupZones []zone
	var bottom []string
	if menu {
		x := 1
		var parts []string
		for i, sg := range c.sugg {
			label := " " + sg.icon + " " + sg.label + " "
			if i == c.suggIdx {
				parts = append(parts, th.selected.Render(label))
			} else {
				parts = append(parts, th.accentText.Render(label))
			}
			lw := lipgloss.Width(label)
			idx := i
			popupZones = append(popupZones, zone{x0: x, y0: 2 + topH, x1: x + lw, y1: 3 + topH, click: func(int, int, bool) tea.Cmd {
				a.acceptSuggestion(idx)
				return a.focusComposer()
			}})
			x += lw + 1
		}
		hint := "↓ browse"
		if c.suggIdx >= 0 {
			hint = "←→ ⏎ pick · ↑ back"
		}
		bottom = append(bottom, fitLines(" "+strings.Join(parts, " ")+"  "+th.dimText.Render(hint), w, 1))
	} else if a.focus == focusComposer && len(c.sugg) > 0 {
		popupZones = a.overlaySuggestions(topLines, w, topH)
	}

	rule := th.sepStyle.Render(strings.Repeat("─", w))
	if a.focus == focusComposer {
		rule = th.accentText.Render(strings.Repeat("━", w))
	}
	c.input.SetWidth(max(w-4, 10))
	bottom = append(bottom, rule, fitLines(" "+c.input.View(), w, 1))

	a.addZone(zone{x0: 0, y0: 2 + topH, x1: w, y1: 2 + l.bodyH, click: func(int, int, bool) tea.Cmd {
		a.blurForm()
		return a.focusComposer()
	}})
	for _, z := range popupZones { // above everything else in the tab
		a.addZone(z)
	}
	return strings.Join(append(topLines, bottom...), "\n")
}

// overlaySuggestions draws the suggestion list over the bottom rows of the
// panes and returns its click zones.
func (a *App) overlaySuggestions(lines []string, width, height int) []zone {
	th := a.th
	c := a.chat
	rows := min(len(c.sugg), max(height-1, 1), 14)
	if c.suggIdx >= 0 {
		if c.suggIdx < c.suggOff2 {
			c.suggOff2 = c.suggIdx
		}
		if c.suggIdx >= c.suggOff2+rows {
			c.suggOff2 = c.suggIdx - rows + 1
		}
	}
	c.suggOff2 = min(max(c.suggOff2, 0), len(c.sugg)-rows)
	base := lipgloss.NewStyle().Background(th.sel)
	title := c.suggTitle
	if len(c.sugg) > rows {
		title += fmt.Sprintf("  %d–%d of %d", c.suggOff2+1, c.suggOff2+rows, len(c.sugg))
	}
	hint := "↓ choose · tab/⏎ pick"
	if c.suggIdx >= 0 {
		hint = "↑↓ move · tab/⏎ pick · esc back"
	}
	if top := height - rows - 1; top >= 0 {
		head := padRight(" "+strings.TrimSpace(title+"  "+hint), width)
		lines[top] = base.Bold(true).Render(truncate(head, width))
	}
	labelW := 0
	for _, sg := range c.sugg[c.suggOff2 : c.suggOff2+rows] {
		labelW = max(labelW, lipgloss.Width(sg.label))
	}
	labelW = min(labelW+2, width/2)
	var zones []zone
	for r := 0; r < rows; r++ {
		i := c.suggOff2 + r
		sg := c.sugg[i]
		text := " " + sg.icon + " " + padRight(truncate(sg.label, labelW), labelW) + oneLine(sg.desc)
		text = padRight(truncate(text, width), width)
		if i == c.suggIdx {
			text = th.selected.Render(text)
		} else {
			text = base.Render(text)
		}
		row := height - rows + r
		if row < 0 {
			continue
		}
		lines[row] = text
		idx := i
		zones = append(zones, zone{x0: 0, y0: 2 + row, x1: width, y1: 3 + row,
			click: func(int, int, bool) tea.Cmd {
				a.acceptSuggestion(idx)
				return a.focusComposer()
			},
			wheel: func(d int) tea.Cmd {
				c.suggOff2 = min(max(c.suggOff2+d, 0), max(len(c.sugg)-rows, 0))
				return nil
			}})
	}
	return zones
}

// renderTranscript draws the chat items (the left pane).
func (a *App) renderTranscript(x0, width, height int) string {
	th := a.th
	c := a.chat
	items := c.visible()
	transH := max(height, 1)

	type line struct {
		text string
		item int
	}
	var all []line
	selStart, selEnd := 0, 0
	for i, it := range items {
		if i > 0 {
			all = append(all, line{"", -1})
		}
		if i == c.sel {
			selStart = len(all)
		}
		all = append(all, line{truncate(a.headline(it), width-3), i})
		for _, p := range a.previewLines(it, width-4) {
			all = append(all, line{"  " + p, i})
		}
		if i == c.sel {
			selEnd = len(all)
		}
	}
	if len(items) == 0 {
		all = append(all, line{th.dimText.Render(truncate("No requests yet. Browse or type a name below.", width-3)), -1})
	}

	// Anchor short transcripts to the message box, like a chat.
	if pad := transH - len(all); pad > 0 {
		blank := make([]line, pad)
		for i := range blank {
			blank[i] = line{"", -1}
		}
		all = append(blank, all...)
		selStart += pad
		selEnd += pad
	}

	// Scroll: follow the bottom, else keep the selection visible.
	maxOff := max(len(all)-transH, 0)
	if c.follow {
		c.offset = maxOff
	} else {
		if selStart < c.offset {
			c.offset = selStart
		}
		if selEnd > c.offset+transH {
			c.offset = selEnd - transH
		}
		c.offset = min(max(c.offset, 0), maxOff)
	}

	c.lineItem = c.lineItem[:0]
	var out []string
	for r := c.offset; r < len(all) && r < c.offset+transH; r++ {
		ln := all[r]
		c.lineItem = append(c.lineItem, ln.item)
		bar := " "
		if ln.item >= 0 && ln.item == c.sel && len(items) > 0 {
			if a.focus == focusList {
				bar = th.accentText.Render("▌")
			} else {
				bar = th.faintText.Render("▌")
			}
		}
		out = append(out, bar+" "+ln.text)
	}
	for len(out) < transH {
		c.lineItem = append(c.lineItem, -1)
		out = append(out, "")
	}

	a.addZone(zone{x0: x0, y0: 2, x1: x0 + width, y1: 2 + transH,
		click: func(x, y int, double bool) tea.Cmd {
			c.input.Blur()
			a.blurForm()
			if y < len(c.lineItem) && c.lineItem[y] >= 0 {
				a.selectChat(c.lineItem[y])
				a.focus = focusList
				if double {
					return a.editItem()
				}
				return nil
			}
			a.focus = focusList
			return nil
		},
		wheel: func(d int) tea.Cmd {
			c.follow = false
			c.offset = min(max(c.offset+d, 0), maxOff)
			return nil
		},
	})
	return fitLines(strings.Join(out, "\n"), width, transH)
}

func commandArgs(args json.RawMessage) string {
	if len(args) == 0 || string(args) == "{}" || string(args) == "null" {
		return ""
	}
	var v any
	if json.Unmarshal(args, &v) != nil {
		return string(args)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// renderChatDetail draws the compose form or the selected item in full.
func (a *App) renderChatDetail(width, height int) string {
	th := a.th
	c := a.chat
	if c.compose != "" {
		f := a.formFor(c.compose)
		if f == nil {
			c.compose = ""
		} else {
			f.SetWidth(max(width-2, 10)) // the Chat pane is narrower than on the other tabs
			defer f.SetWidth(max(a.layout().detailW-2, 10))
			return fitLines(a.renderDoc(c.compose, f, width, height), width, height)
		}
	}
	it := c.selected()
	if it == nil {
		msg := []string{"", " " + th.title.Render("Chat"), "",
			" Every request in this session appears here, including ones",
			" made from the Tools, Prompts and Resources tabs."}
		return fitLines(strings.Join(msg, "\n"), width, height)
	}

	var head []string
	title := " " + truncate(a.headline(it), width-2)
	if it.isCall() {
		title = " " + kindIcon(it.kind) + " " + th.title.Render(it.name)
		if it.entry != nil {
			title += " " + th.dimText.Render(it.entry.Method)
		} else if it.rs != nil && it.rs.running {
			title += " " + th.warnText.Render("running")
		}
	}
	head = append(head, title)
	meta := " " + it.time().Format("2006-01-02 15:04:05")
	if it.entry != nil {
		meta += fmt.Sprintf(" · %s · %s", shortMS(it.entry.DurationMS), it.entry.Status)
	}
	if it.isCall() && it.origin < tabChat {
		meta += " · from " + tabNames[it.origin]
	}
	head = append(head, th.dimText.Render(meta))
	if it.isCall() {
		var btns []string
		for _, b := range [][3]string{{"Rerun", "ctrl+r"}, {"Edit", "e"}, {"Copy cmd", "c"}, {"curl", "C"}} {
			btns = append(btns, a.renderButton(b[0], b[1]))
		}
		head = append(head, " "+strings.Join(btns, " "))
		a.addButton(a.detailX, 2, a.width, 2+height, buttonLabel("Rerun", "ctrl+r"), a.runAgain)
		a.addButton(a.detailX, 2, a.width, 2+height, buttonLabel("Edit", "e"), a.editItem)
		key := it.key
		args := it.args
		ex := func() *mcp.Exchange {
			if it.rs != nil {
				return it.rs.ex
			}
			return nil
		}
		a.addButton(a.detailX, 2, a.width, 2+height, buttonLabel("Copy cmd", "c"), func() tea.Cmd { return a.copyCommand(key, args, false, ex()) })
		a.addButton(a.detailX, 2, a.width, 2+height, buttonLabel("curl", "C"), func() tea.Cmd { return a.copyCommand(key, args, true, ex()) })
	}
	if args := commandArgs(it.args); args != "" {
		block := strings.Split(a.rd.jsonBlock(it.args, width-2), "\n")
		limit := max(height/4, 3)
		if len(block) > limit {
			block = append(block[:limit-1], th.faintText.Render("… (r for the raw request)"))
		}
		head = append(head, " "+a.rd.section("arguments", width-2))
		for _, l := range block {
			head = append(head, " "+l)
		}
	}
	topH := min(len(head), height-3)
	top := fitLines(strings.Join(head, "\n"), width, topH)
	if it.rs == nil {
		return fitLines(top, width, height)
	}
	a.resultY = 2 + topH
	return top + "\n" + a.renderResult(it.rs, width, height-topH)
}

func shortMS(ms int64) string {
	if ms >= 10000 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	return fmt.Sprintf("%dms", ms)
}
