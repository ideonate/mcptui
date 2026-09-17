// Package tui is the interactive Bubble Tea interface.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/history"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/paths"
	"github.com/ideonate/mcptui/internal/schemaform"
	"github.com/ideonate/mcptui/internal/session"
)

// Options configure the TUI.
type Options struct {
	Profile         *config.Profile
	Config          *config.Config
	NoBrowser       bool
	CallbackHost    string
	NoHistory       bool
	CredentialsPath string
	HistoryPath     string // "" = default when persisting
	NoMouse         bool   // leave the mouse to the terminal (native selection)
	Dark            *bool  // nil = detect
}

type tabID int

const (
	tabTools tabID = iota
	tabPrompts
	tabResources
	tabChat
	tabLog
	numTabs
)

var tabNames = [numTabs]string{"Tools", "Prompts", "Resources", "Chat", "Log"}

type focusArea int

const (
	focusList     focusArea = iota
	focusTabs               // the tab bar: ←/→ switch tabs
	focusComposer           // the Chat tab's message box
	focusForm
	focusResult
)

type connState int

const (
	stateConnecting connState = iota
	stateConnected
	stateError
	stateDisconnected
)

// bridge delivers messages from goroutines to the program (or a queue in
// tests).
type bridge struct {
	mu    sync.Mutex
	prog  *tea.Program
	queue []tea.Msg
}

func (b *bridge) send(msg tea.Msg) {
	b.mu.Lock()
	p := b.prog
	if p == nil {
		b.queue = append(b.queue, msg)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	go p.Send(msg)
}

func (b *bridge) drain() []tea.Msg {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queue
	b.queue = nil
	return q
}

// Messages.
type (
	connectedMsg struct {
		sess *session.Session
		ex   *mcp.Exchange
		err  error
		gen  int
	}
	disconnectedMsg struct {
		err error
		gen int
	}
	listLoadedMsg struct {
		kind  string // tools | prompts | resources | templates
		items any
		exs   []*mcp.Exchange
		err   error
		gen   int
	}
	logMsg          struct{ ev logEntry }
	notificationMsg struct {
		method string
		params json.RawMessage
		gen    int
	}
	progressMsg struct {
		tab tabID
		run int
		p   mcp.ProgressParams
	}
	callDoneMsg struct {
		tab     tabID
		run     int
		ex      *mcp.Exchange
		tool    *mcp.CallToolResult
		prompt  *mcp.GetPromptResult
		read    *mcp.ReadResourceResult
		entry   history.Entry
		subject string
	}
	loginPromptMsg   struct{ p auth.LoginPrompt }
	loginFinishedMsg struct {
		scope string
		err   error
	}
	authActionDoneMsg struct {
		action string
		err    error
	}
	tickMsg       time.Time
	credentialMsg struct{ cred *auth.Credential }
	editorDoneMsg struct {
		path string
		err  error
	}
	flashMsg         struct{ text string }
	serverRequestMsg struct {
		method        string
		params, reply json.RawMessage
		gen           int
	}
)

type logEntry struct {
	time   time.Time
	source string
	level  string
	text   string
}

// App is the root model.
type App struct {
	opts Options
	th   *theme
	rd   *renderer
	br   *bridge

	width, height int
	tab           tabID
	focus         focusArea

	profile  *config.Profile
	sess     *session.Session
	state    connState
	connErr  error
	gen      int // connection generation; stale async results are ignored
	opCancel context.CancelFunc
	cred     *auth.Credential

	tools          []mcp.Tool
	prompts        []mcp.Prompt
	resources      []mcp.Resource
	templates      []mcp.ResourceTemplate
	listErr        map[string]error
	loading        map[string]bool
	subscribed     map[string]bool
	lastCompletion *completionMsg
	picker         *pickerState
	chat           *chatState
	runs           map[int]*resultState // in-flight and finished calls by run id

	zones     []zone
	buttons   []buttonSpec
	lastClick clickRecord
	detailX   int // screen column where the detail pane starts
	resultY   int // screen row where the result area starts

	lists [numTabs]*listPane

	// Detail state for tools/prompts/resources.
	forms    map[string]*schemaform.Form // by item key
	lastArgs map[string]json.RawMessage
	results  [numTabs]*resultState

	hist        *history.Log
	logs        []logEntry
	logVP       scrollView
	logFollow   bool
	showTraffic bool

	overlay overlay
	flash   string
	flashAt time.Time
	quitArm time.Time

	spin spinner.Model
	prog progress.Model
}

// New creates the model (without starting a program).
func New(opts Options) *App {
	dark := true
	if opts.Dark != nil {
		dark = *opts.Dark
	}
	th := newTheme(dark)
	a := &App{
		opts:       opts,
		th:         th,
		rd:         newRenderer(th),
		br:         &bridge{},
		profile:    opts.Profile,
		forms:      map[string]*schemaform.Form{},
		lastArgs:   map[string]json.RawMessage{},
		listErr:    map[string]error{},
		loading:    map[string]bool{},
		subscribed: map[string]bool{},
		logFollow:  true,
		spin:       spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		prog:       progress.New(progress.WithDefaultBlend(), progress.WithWidth(30)),
		width:      100,
		height:     30,
	}
	a.lists[tabTools] = newListPane(th, "no tools")
	a.lists[tabPrompts] = newListPane(th, "no prompts")
	a.lists[tabResources] = newListPane(th, "no resources")
	a.lists[tabChat] = newListPane(th, "")
	a.chat = newChatState()
	a.runs = map[int]*resultState{}
	histPath := ""
	if !opts.NoHistory && (opts.Config == nil || opts.Config.PersistHistory()) {
		histPath = opts.HistoryPath
		if histPath == "" {
			histPath = paths.HistoryFile()
		}
	}
	a.hist = history.New(histPath)
	return a
}

// Run starts the TUI.
func Run(opts Options) error {
	if opts.Dark == nil {
		d := lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
		opts.Dark = &d
	}
	a := New(opts)
	p := tea.NewProgram(a)
	a.br.mu.Lock()
	a.br.prog = p
	queued := a.br.queue
	a.br.queue = nil
	a.br.mu.Unlock()
	for _, m := range queued {
		go p.Send(m)
	}
	_, err := p.Run()
	if a.opCancel != nil {
		a.opCancel()
	}
	if a.sess != nil {
		a.sess.Close()
	}
	return err
}

func (a *App) Init() tea.Cmd {
	if a.profile == nil {
		return tea.Batch(a.openPicker(), a.tick(), a.spin.Tick)
	}
	return tea.Batch(a.connect(), a.tick(), a.spin.Tick)
}

func (a *App) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) logf(source, level, format string, args ...any) {
	a.addLog(logEntry{time: time.Now(), source: source, level: level, text: fmt.Sprintf(format, args...)})
}

func (a *App) addLog(e logEntry) {
	const maxLogs = 5000
	a.logs = append(a.logs, e)
	if len(a.logs) > maxLogs {
		a.logs = a.logs[len(a.logs)-maxLogs:]
	}
}

func (a *App) setFlash(format string, args ...any) {
	a.flash = fmt.Sprintf(format, args...)
	a.flashAt = time.Now()
}

// connect builds a session and connects it asynchronously.
func (a *App) connect() tea.Cmd {
	if a.opCancel != nil {
		a.opCancel()
	}
	if a.sess != nil {
		old := a.sess
		go old.Close()
		a.sess = nil
	}
	a.gen++
	gen := a.gen
	a.state = stateConnecting
	a.connErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	a.opCancel = cancel
	br := a.br
	opts := a.sessionOptions(gen)
	return func() tea.Msg {
		s, err := session.New(opts)
		if err != nil {
			return connectedMsg{err: err, gen: gen}
		}
		ex, err := s.Connect(ctx)
		if err != nil {
			s.Close()
			return connectedMsg{err: err, ex: ex, gen: gen}
		}
		go func() {
			<-s.Client.Done()
			br.send(disconnectedMsg{err: s.Client.Transport().Err(), gen: gen})
		}()
		return connectedMsg{sess: s, ex: ex, gen: gen}
	}
}

func (a *App) sessionOptions(gen int) session.Options {
	br := a.br
	return session.Options{
		Profile:         a.profile,
		CredentialsPath: a.opts.CredentialsPath,
		Interactive:     true,
		NoBrowser:       a.opts.NoBrowser,
		CallbackHost:    a.opts.CallbackHost,
		LoginUI:         &tuiLoginUI{br: br},
		Log: func(e mcp.Event) {
			br.send(logMsg{ev: logEntry{time: time.Now(), source: e.Source, level: e.Level, text: e.Text}})
		},
		OnServerRequest: func(method string, params, reply json.RawMessage) {
			br.send(serverRequestMsg{method: method, params: params, reply: reply, gen: gen})
		},
		OnNotification: func(method string, params json.RawMessage) {
			br.send(notificationMsg{method: method, params: params, gen: gen})
		},
		OnTraffic: func(t mcp.Traffic) {
			raw := string(t.Raw)
			if len(raw) > maxTrafficLog {
				raw = raw[:maxTrafficLog] + fmt.Sprintf("… (%d bytes)", len(t.Raw))
			}
			br.send(logMsg{ev: logEntry{time: t.Time, source: "traffic", level: "debug", text: string(t.Direction) + " " + raw}})
		},
	}
}

// maxTrafficLog caps raw messages shown in the Log tab; History keeps them whole.
const maxTrafficLog = 4000

type tuiLoginUI struct{ br *bridge }

func (u *tuiLoginUI) ShowLogin(p auth.LoginPrompt) { u.br.send(loginPromptMsg{p: p}) }
func (u *tuiLoginUI) LoginFinished(scope string, err error) {
	u.br.send(loginFinishedMsg{scope: scope, err: err})
}

// loadLists fetches every list the server advertises.
func (a *App) loadLists() tea.Cmd {
	if a.sess == nil {
		return nil
	}
	init := a.sess.Client.InitializeResult()
	var cmds []tea.Cmd
	if init == nil || init.Capabilities.Tools != nil {
		cmds = append(cmds, a.loadList("tools"))
	}
	if init == nil || init.Capabilities.Prompts != nil {
		cmds = append(cmds, a.loadList("prompts"))
	}
	if init == nil || init.Capabilities.Resources != nil {
		cmds = append(cmds, a.loadList("resources"), a.loadList("templates"))
	}
	return tea.Batch(cmds...)
}

func (a *App) loadList(kind string) tea.Cmd {
	s := a.sess
	if s == nil {
		return nil
	}
	gen := a.gen
	a.loading[kind] = true
	return func() tea.Msg {
		ctx := context.Background()
		msg := listLoadedMsg{kind: kind, gen: gen}
		switch kind {
		case "tools":
			msg.items, msg.exs, msg.err = mustItems(s.Client.ListTools(ctx))
		case "prompts":
			msg.items, msg.exs, msg.err = mustItems(s.Client.ListPrompts(ctx))
		case "resources":
			msg.items, msg.exs, msg.err = mustItems(s.Client.ListResources(ctx))
		case "templates":
			msg.items, msg.exs, msg.err = mustItems(s.Client.ListResourceTemplates(ctx))
		}
		return msg
	}
}

func mustItems[T any](items []T, exs []*mcp.Exchange, err error) (any, []*mcp.Exchange, error) {
	return items, exs, err
}

func (a *App) refreshCredential() tea.Cmd {
	if a.sess == nil || a.sess.Auth == nil {
		return nil
	}
	m := a.sess.Auth
	return func() tea.Msg {
		c, _ := m.Status()
		return credentialMsg{cred: c}
	}
}

// ---------------------------------------------------------------- Update

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.relayout()
		if a.picker != nil && a.picker.form != nil {
			a.picker.form.SetWidth(max(min(a.width-4, 100), 20))
		}
		return a, nil

	case tickMsg:
		cmds := []tea.Cmd{a.tick()}
		if time.Time(msg).Second()%5 == 0 {
			cmds = append(cmds, a.refreshCredential())
		}
		return a, tea.Batch(cmds...)

	case spinner.TickMsg:
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(msg)
		return a, cmd

	case progress.FrameMsg:
		var cmd tea.Cmd
		a.prog, cmd = a.prog.Update(msg)
		return a, cmd

	case credentialMsg:
		a.cred = msg.cred
		return a, nil

	case flashMsg:
		a.setFlash("%s", msg.text)
		return a, nil

	case logMsg:
		a.addLog(msg.ev)
		if a.tab == tabLog {
			a.refreshLogView()
		}
		return a, nil

	case connectedMsg:
		if msg.gen != a.gen {
			if msg.sess != nil {
				msg.sess.Close()
			}
			return a, nil
		}
		if msg.ex != nil {
			a.recordExchange(msg.ex, "init", "initialize", nil)
		} else if msg.err != nil {
			a.addEvent("✗", "Could not connect to "+a.profile.Target(), msg.err.Error())
		}
		if msg.err != nil {
			a.state = stateError
			a.connErr = msg.err
			a.logf("client", "error", "%v", msg.err)
			if ov, ok := a.overlay.(*loginOverlay); ok && !ov.done {
				a.overlay = nil
			}
			return a, nil
		}
		a.sess = msg.sess
		a.state = stateConnected
		a.connErr = nil
		if ov, ok := a.overlay.(*loginOverlay); ok {
			_ = ov
			a.overlay = nil
		}
		return a, tea.Batch(a.loadLists(), a.refreshCredential())

	case disconnectedMsg:
		if msg.gen != a.gen || a.state != stateConnected {
			return a, nil
		}
		a.state = stateDisconnected
		a.connErr = msg.err
		detail := "press R to reconnect"
		if msg.err != nil {
			detail = msg.err.Error()
		}
		a.addEvent("○", "Disconnected", detail)
		if msg.err != nil {
			a.logf("client", "error", "disconnected: %v", msg.err)
			a.setFlash("disconnected: %s (R to reconnect, 5 for log)", oneLine(msg.err.Error()))
		}
		return a, nil

	case listLoadedMsg:
		if msg.gen != a.gen {
			return a, nil
		}
		return a, a.onListLoaded(msg)

	case notificationMsg:
		if msg.gen != a.gen {
			return a, nil
		}
		return a, a.onNotification(msg)

	case progressMsg:
		rs := a.runs[msg.run]
		if rs == nil || !rs.running {
			return a, nil
		}
		rs.progress = &msg.p
		if msg.p.Total != nil && *msg.p.Total > 0 {
			return a, a.prog.SetPercent(msg.p.Progress / *msg.p.Total)
		}
		return a, nil

	case callDoneMsg:
		return a, a.onCallDone(msg)

	case serverRequestMsg:
		if msg.gen == a.gen {
			a.addServerRequest(msg)
		}
		return a, nil

	case loginPromptMsg:
		a.overlay = newLoginOverlay(a, msg.p)
		return a, a.overlay.init()

	case loginFinishedMsg:
		if ov, ok := a.overlay.(*loginOverlay); ok {
			ov.done = true
			a.overlay = nil
		}
		if msg.err != nil {
			a.logf("auth", "error", "login failed: %v", msg.err)
			a.setFlash("login failed: %v", msg.err)
			a.addEvent("✗", "Login failed", msg.err.Error())
		} else {
			a.logf("auth", "info", "logged in (scope: %s)", msg.scope)
			scope := "scope: " + msg.scope
			if msg.scope == "" {
				scope = "no scope reported"
			}
			a.addEvent("🔑", "Logged in", scope)
			a.setFlash("logged in")
		}
		return a, a.refreshCredential()

	case authActionDoneMsg:
		if msg.err != nil {
			a.setFlash("%s failed: %v", msg.action, msg.err)
			a.logf("auth", "error", "%s failed: %v", msg.action, msg.err)
			return a, nil
		}
		switch msg.action {
		case "logout":
			a.cred = nil
			a.setFlash("logged out")
			a.addEvent("🔑", "Logged out", "tokens revoked and deleted")
			if a.sess != nil {
				a.sess.Close()
				a.sess = nil
			}
			a.gen++
			a.state = stateDisconnected
			a.connErr = fmt.Errorf("logged out; press L to log in or R to reconnect")
			return a, nil
		case "login", "reset-client":
			a.setFlash("%s done; reconnecting", msg.action)
			return a, a.connect()
		}
		return a, nil

	case editorDoneMsg:
		return a, a.onEditorDone(msg)

	case completionMsg:
		a.applyCompletion(msg)
		return a, nil

	case tea.PasteMsg:
		if a.picker != nil && a.overlay == nil {
			return a, a.pickerUpdate(msg)
		}
		if a.overlay != nil {
			return a, a.overlay.update(msg)
		}
		return a, a.routeInput(msg)

	case tea.KeyPressMsg:
		return a, a.onKey(msg)

	case tea.MouseClickMsg:
		return a, a.onMouse(msg)
	case tea.MouseWheelMsg:
		return a, a.onMouse(msg)
	}

	if a.overlay != nil {
		return a, a.overlay.update(msg)
	}
	return a, a.routeInput(msg)
}

func (a *App) onListLoaded(msg listLoadedMsg) tea.Cmd {
	a.loading[msg.kind] = false
	for _, ex := range msg.exs {
		a.recordExchange(ex, "list", msg.kind, nil)
	}
	if msg.err != nil {
		a.listErr[msg.kind] = msg.err
		a.logf("client", "warn", "%s: %v", msg.kind, msg.err)
	} else {
		delete(a.listErr, msg.kind)
	}
	switch msg.kind {
	case "tools":
		a.tools, _ = msg.items.([]mcp.Tool)
		a.rebuildToolList()
	case "prompts":
		a.prompts, _ = msg.items.([]mcp.Prompt)
		a.rebuildPromptList()
	case "resources":
		a.resources, _ = msg.items.([]mcp.Resource)
		a.rebuildResourceList()
	case "templates":
		a.templates, _ = msg.items.([]mcp.ResourceTemplate)
		a.rebuildResourceList()
	}
	return nil
}

func (a *App) onNotification(msg notificationMsg) tea.Cmd {
	a.addNotification(msg.method, msg.params)
	switch msg.method {
	case "notifications/message":
		var p mcp.LogMessageParams
		_ = json.Unmarshal(msg.params, &p)
		text := string(p.Data)
		var s string
		if json.Unmarshal(p.Data, &s) == nil {
			text = s
		}
		if p.Logger != "" {
			text = p.Logger + ": " + text
		}
		a.logf("server", p.Level, "%s", text)
	case "notifications/tools/list_changed":
		a.logf("server", "info", "tools list changed; refreshing")
		return a.loadList("tools")
	case "notifications/prompts/list_changed":
		a.logf("server", "info", "prompts list changed; refreshing")
		return a.loadList("prompts")
	case "notifications/resources/list_changed":
		a.logf("server", "info", "resources list changed; refreshing")
		return tea.Batch(a.loadList("resources"), a.loadList("templates"))
	case "notifications/resources/updated":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(msg.params, &p)
		a.logf("server", "info", "resource updated: %s", p.URI)
		a.setFlash("resource updated: %s", p.URI)
	case "notifications/progress":
		a.logf("server", "debug", "progress %s", msg.params)
	default:
		a.logf("server", "info", "%s %s", msg.method, msg.params)
	}
	return nil
}

// recordExchange adds an exchange to history.
func (a *App) recordExchange(ex *mcp.Exchange, kind, name string, args json.RawMessage) history.Entry {
	e := history.Entry{
		Time:       ex.Started,
		Profile:    a.profile.Name,
		Kind:       kind,
		Name:       name,
		Method:     ex.Method,
		Arguments:  args,
		Status:     history.StatusOK,
		DurationMS: ex.Duration.Milliseconds(),
		Request:    ex.Request,
		Response:   ex.Response,
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if ex.Err != nil {
		e.Status = history.StatusError
		if ex.Err == context.Canceled {
			e.Status = history.StatusCancelled
		}
		e.Error = ex.Err.Error()
	}
	if kind == "tool" && ex.Err == nil {
		var r struct {
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(ex.Result, &r)
		if r.IsError {
			e.Status = history.StatusToolError
		}
	}
	a.hist.Add(e)
	if kind == "list" || kind == "init" {
		rs := &resultState{title: name, kind: kind, ex: ex, dirty: true}
		if kind == "init" {
			rs.title = "initialize"
			rs.custom = a.initCustom(ex)
		}
		a.addChatItem(&chatItem{kind: kind, name: ex.Method, rs: rs, entry: &e, origin: a.tab})
	}
	return e
}

// ---------------------------------------------------------------- Keys

// typing reports whether keystrokes should go to a text widget.
func (a *App) typing() bool {
	if a.tab < tabChat && a.lists[a.tab].filtering {
		return true
	}
	if a.tab == tabChat && a.focus == focusComposer {
		return true
	}
	if a.focus == focusForm {
		if f := a.currentForm(); f != nil && f.InputActive() {
			return true
		}
	}
	return false
}

func (a *App) onKey(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if a.overlay != nil {
		if key == "ctrl+c" && a.overlay.cancelOnCtrlC() {
			a.overlay.close()
			a.overlay = nil
			return nil
		}
		return a.overlay.update(k)
	}

	if a.picker != nil {
		if key == "ctrl+c" {
			return tea.Quit
		}
		return a.pickerUpdate(k)
	}

	// Global keys that work even while typing.
	switch key {
	case "ctrl+p":
		return a.openPicker()
	case "ctrl+c":
		if a.tab == tabChat && a.cancelChat() {
			a.setFlash("cancelling…")
			return nil
		}
		if rs := a.results[a.tab]; rs != nil && rs.running && rs.cancel != nil {
			rs.cancel()
			a.setFlash("cancelling…")
			return nil
		}
		if a.state == stateConnecting && a.opCancel != nil {
			a.opCancel()
			a.state = stateError
			a.connErr = fmt.Errorf("connection cancelled; press R to retry")
			return nil
		}
		if time.Since(a.quitArm) < 2*time.Second || !a.typing() {
			return tea.Quit
		}
		a.quitArm = time.Now()
		a.setFlash("press ctrl+c again to quit")
		return nil
	}

	if a.typing() {
		return a.routeInput(k)
	}

	switch key {
	case "q":
		if a.focus == focusList || a.focus == focusTabs || a.tab >= tabChat {
			return tea.Quit
		}
	case "?":
		a.overlay = newHelpOverlay(a)
		return nil
	case "i":
		if a.focus != focusForm {
			a.overlay = newInstructionsOverlay(a)
			return nil
		}
	case "1", "2", "3", "4", "5":
		a.switchTab(tabID(key[0] - '1'))
		return nil
	case "]", "ctrl+right", "alt+right":
		a.switchTab((a.tab + 1) % numTabs)
		return nil
	case "[", "ctrl+left", "alt+left":
		a.switchTab((a.tab + numTabs - 1) % numTabs)
		return nil
	case "R":
		return a.refresh()
	case "L":
		return a.login()
	case "O":
		return a.logout()
	case "P":
		return a.ping()
	}

	if a.focus == focusTabs {
		return a.tabBarKey(k)
	}
	switch a.tab {
	case tabChat:
		if a.focus == focusForm {
			return a.formKey(k)
		}
		return a.chatKey(k)
	case tabLog:
		return a.logKey(k)
	}
	switch a.focus {
	case focusList:
		return a.listKey(k)
	case focusForm:
		return a.formKey(k)
	case focusResult:
		return a.resultKey(k)
	}
	return nil
}

// routeInput forwards non-shortcut input to the focused widget.
func (a *App) routeInput(msg tea.Msg) tea.Cmd {
	if a.tab == tabChat && a.focus == focusComposer {
		return a.composerKey(msg)
	}
	if a.tab < tabChat && a.lists[a.tab].filtering {
		cmd, _ := a.lists[a.tab].updateFilter(msg)
		return cmd
	}
	if a.focus == focusForm {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			if cmd, handled := a.formShortcut(k); handled {
				return cmd
			}
		}
		if f := a.currentForm(); f != nil {
			return f.Update(msg)
		}
	}
	return nil
}

// tabBarKey handles keys while the tab bar has focus.
func (a *App) tabBarKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "left", "h", "shift+tab":
		a.switchTab((a.tab + numTabs - 1) % numTabs)
	case "right", "l":
		a.switchTab((a.tab + 1) % numTabs)
	case "down", "j", "enter", "tab", "space":
		if a.tab == tabChat {
			return a.focusComposer()
		}
		a.focus = focusList
	}
	return nil
}

// focusTabBar moves focus up to the tab bar.
func (a *App) focusTabBar() tea.Cmd {
	a.blurForm()
	a.focus = focusTabs
	return nil
}

func (a *App) switchTab(t tabID) {
	if a.tab == t {
		return
	}
	a.tab = t
	a.chat.input.Blur()
	if a.focus != focusTabs {
		a.focus = focusList
		if t == tabChat {
			a.focusComposer()
		}
	}
	for _, f := range a.forms {
		f.Blur()
	}
	switch t {
	case tabLog:
		a.refreshLogView()
	}
}

func (a *App) refresh() tea.Cmd {
	if a.state != stateConnected {
		a.setFlash("reconnecting…")
		return a.connect()
	}
	a.setFlash("refreshing lists")
	return a.loadLists()
}

func (a *App) ping() tea.Cmd {
	if a.sess == nil {
		a.setFlash("not connected")
		return nil
	}
	c := a.sess.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ex := c.Ping(ctx)
		if ex.Err != nil {
			return flashMsg{text: "ping failed: " + ex.Err.Error()}
		}
		return flashMsg{text: "pong in " + ex.Duration.Round(time.Millisecond/10).String()}
	}
}

func (a *App) authManager() *auth.Manager {
	if a.sess != nil && a.sess.Auth != nil {
		return a.sess.Auth
	}
	if a.profile.Transport == "http" && a.profile.Auth == config.AuthOAuth {
		return session.NewAuthManager(a.profile, a.sessionOptions(a.gen))
	}
	return nil
}

func (a *App) login() tea.Cmd {
	m := a.authManager()
	if m == nil {
		a.setFlash("profile %q does not use OAuth", a.profile.Name)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	if a.opCancel != nil {
		a.opCancel()
	}
	a.opCancel = cancel
	return func() tea.Msg {
		return authActionDoneMsg{action: "login", err: m.Login(ctx)}
	}
}

func (a *App) logout() tea.Cmd {
	m := a.authManager()
	if m == nil {
		a.setFlash("profile %q does not use OAuth", a.profile.Name)
		return nil
	}
	a.overlay = newConfirmOverlay(a, "Log out of "+a.profile.Name+"? Tokens will be revoked and deleted.", func() tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return authActionDoneMsg{action: "logout", err: m.Logout(ctx)}
		}
	})
	return nil
}

func (a *App) resetClient() tea.Cmd {
	m := a.authManager()
	if m == nil {
		return nil
	}
	return func() tea.Msg {
		return authActionDoneMsg{action: "reset-client", err: m.ResetClient()}
	}
}

// ---------------------------------------------------------------- Layout

type layout struct {
	bodyH          int
	listW, detailW int
	stacked        bool
}

func (a *App) layout() layout {
	l := layout{bodyH: max(a.height-3, 3)}
	if a.width < 80 {
		l.stacked = true
		l.listW, l.detailW = a.width, a.width
		return l
	}
	l.listW = min(max(a.width*30/100, 24), 44)
	l.detailW = a.width - l.listW - 1
	return l
}

func (a *App) relayout() {
	l := a.layout()
	for _, f := range a.forms {
		f.SetWidth(max(l.detailW-2, 10))
	}
	for _, rs := range a.results {
		if rs != nil {
			rs.dirty = true
		}
	}
	a.prog.SetWidth(min(40, max(l.detailW-20, 10)))
	a.refreshLogView()
}

// ---------------------------------------------------------------- View

func (a *App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	if !a.opts.NoMouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	v.WindowTitle = "mcptui"
	if a.profile != nil {
		v.WindowTitle += " – " + a.profile.Name
	}
	return v
}

func (a *App) render() string {
	if a.width <= 0 || a.height <= 0 {
		return ""
	}
	a.resetZones()
	header := a.renderHeader()
	tabs := a.renderTabs()
	footer := a.renderFooter()
	l := a.layout()

	var body string
	if a.overlay != nil && a.overlay.fullscreen() {
		body = fitLines(a.overlay.view(a.width, l.bodyH), a.width, l.bodyH)
		a.registerOverlayButtons(0, 2, a.width, 2+l.bodyH)
	} else {
		zmark, bmark := len(a.zones), len(a.buttons)
		if a.picker != nil {
			body = a.renderPicker(l.bodyH)
		} else {
			body = a.renderBody(l)
		}
		if a.overlay != nil {
			// The overlay hides the body, so its zones don't apply.
			a.zones, a.buttons = a.zones[:zmark], a.buttons[:bmark]
			body = a.placeOverlay(body, l)
		}
	}
	screen := strings.Join([]string{header, tabs, body, footer}, "\n")
	a.resolveButtons(screen)
	return screen
}

func (a *App) renderHeader() string {
	th := a.th
	sep := th.faintText.Render(" ─ ")
	parts := []string{th.title.Render("mcptui")}
	if a.profile == nil {
		return truncate(strings.Join(parts, sep)+sep+th.dimText.Render("profiles"), a.width)
	}
	prof := th.bold.Render(a.profile.Name)
	if b := th.envBadge(a.profile.EnvBadge); b != "" {
		prof += " " + b
	}
	parts = append(parts, prof)
	if a.sess != nil {
		if init := a.sess.Client.InitializeResult(); init != nil {
			parts = append(parts, fmt.Sprintf("%s %s %s", init.ServerInfo.Name, init.ServerInfo.Version, th.dimText.Render("("+init.ProtocolVersion+")")))
		}
	}
	if a.cred != nil {
		if a.cred.Scope != "" {
			parts = append(parts, th.dimText.Render("scope: ")+a.cred.Scope)
		}
		if !a.cred.ExpiresAt.IsZero() {
			left := time.Until(a.cred.ExpiresAt)
			txt := "token " + shortDuration(left)
			switch {
			case left <= 0:
				parts = append(parts, th.badText.Render("token expired"))
			case left < 5*time.Minute:
				parts = append(parts, th.warnText.Render(txt))
			default:
				parts = append(parts, th.dimText.Render(txt))
			}
		}
	}
	var st string
	switch a.state {
	case stateConnecting:
		st = th.warnText.Render(a.spin.View() + " connecting")
	case stateConnected:
		st = th.okText.Render("● connected")
	case stateError:
		st = th.badText.Render("● error")
	case stateDisconnected:
		st = th.badText.Render("○ disconnected")
	}
	left := strings.Join(parts, sep)
	gap := a.width - lipgloss.Width(left) - lipgloss.Width(st) - 1
	if gap < 1 {
		left = truncate(left, max(a.width-lipgloss.Width(st)-2, 0))
		gap = max(a.width-lipgloss.Width(left)-lipgloss.Width(st)-1, 1)
	}
	return truncate(left+strings.Repeat(" ", gap)+st, a.width)
}

func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (a *App) renderTabs() string {
	th := a.th
	if a.picker != nil {
		return truncate(" "+th.tabActive.Render("Profiles")+th.dimText.Render("   "+a.config().Path()), a.width)
	}
	var parts []string
	for i := tabID(0); i < numTabs; i++ {
		name := tabNames[i]
		switch i {
		case tabTools:
			name += countSuffix(len(a.tools), a.loading["tools"])
		case tabPrompts:
			name += countSuffix(len(a.prompts), a.loading["prompts"])
		case tabResources:
			name += countSuffix(len(a.resources)+len(a.templates), a.loading["resources"] || a.loading["templates"])
		case tabChat:
			name += countSuffix(len(a.chat.visible()), false)
		}
		label := fmt.Sprintf("%d %s", i+1, name)
		x := 1 + lipgloss.Width(strings.Join(append(append([]string{}, stripAll(parts)...), ""), "   "))
		if len(parts) == 0 {
			x = 1
		}
		tab := i
		a.addZone(zone{x0: x, y0: 1, x1: x + lipgloss.Width(label), y1: 2, click: func(int, int, bool) tea.Cmd {
			a.switchTab(tab)
			return nil
		}})
		if i == a.tab && a.focus == focusTabs {
			parts = append(parts, th.selected.Render(label))
		} else if i == a.tab {
			parts = append(parts, th.tabActive.Render(label))
		} else {
			parts = append(parts, th.tabInactive.Render(label))
		}
	}
	return truncate(" "+strings.Join(parts, "   "), a.width)
}

func countSuffix(n int, loading bool) string {
	if loading && n == 0 {
		return " …"
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d)", n)
}

func (a *App) renderFooter() string {
	th := a.th
	if a.flash != "" && time.Since(a.flashAt) < 4*time.Second {
		return truncate(" "+th.infoText.Render(oneLine(a.flash)), a.width)
	}
	var hints [][2]string
	switch {
	case a.overlay != nil:
		hints = a.overlay.hints()
	case a.picker != nil:
		hints = a.pickerHints()
	case a.tab < tabChat && a.lists[a.tab].filtering:
		hints = [][2]string{{"enter", "done"}, {"esc", "clear"}}
	case a.focus == focusTabs:
		hints = [][2]string{{"←→", "switch tab"}, {"↓/⏎", "open"}, {"1-5", "jump to tab"}, {"?", "help"}, {"q", "quit"}}
	case a.tab == tabLog:
		hints = [][2]string{{"esc", "tabs"}, {"j/k", "scroll"}, {"G", "follow"}, {"t", "traffic"}, {"c", "clear"}, {"[ ]", "tabs"}, {"q", "quit"}}
	case a.tab == tabChat && a.focus == focusComposer:
		hints = [][2]string{{"⏎", "send"}, {"tab", "complete"}, {"↑", "previous"}, {"shift+tab", "transcript"}, {"esc", "tabs"}}
	case a.tab == tabChat && a.focus == focusList:
		hints = [][2]string{{"↑↓", "select"}, {"⏎", "details"}, {"e", "edit"}, {"r", "rerun"}, {"a", "show all"}, {"esc", "message box"}}
	case a.tab == tabChat && a.focus == focusResult:
		hints = [][2]string{{"p/m/r", "view"}, {"ctrl+r", "rerun"}, {"e", "edit"}, {"y", "copy"}, {"s", "save"}, {"esc", "message box"}}
	case a.focus == focusList:
		hints = [][2]string{{"?", "help"}, {"⏎", "open"}, {"tab", "focus"}, {"esc", "tabs"}, {"/", "filter"}, {"e", "json"}, {"R", "refresh"}, {"L", "login"}, {"i", "info"}, {"ctrl+p", "profiles"}, {"q", "quit"}}
	case a.focus == focusForm:
		hints = [][2]string{{"⏎/ctrl+s", "call"}, {"tab", "next"}, {"shift+tab", "previous"}, {"ctrl+e", "json"}, {"ctrl+o", "$EDITOR"}, {"esc", "back"}}
	case a.focus == focusResult:
		hints = [][2]string{{"p/m/r", "view"}, {"y", "copy"}, {"s", "save"}, {"c/C", "copy cmd"}, {"e", "json"}, {"tab", "list"}, {"esc", "form"}}
	}
	if rs := a.results[a.tab]; rs != nil && rs.running {
		hints = append([][2]string{{"ctrl+c", "cancel"}}, hints...)
	}
	var parts []string
	for _, h := range hints {
		parts = append(parts, th.keyHint.Render(h[0])+" "+th.dimText.Render(h[1]))
		if k := hintKey(h[0]); k != "" {
			a.keyButton(0, a.height-1, a.width, a.height, h[0]+" "+h[1], k)
		}
	}
	return truncate(" "+strings.Join(parts, "  "), a.width)
}

func (a *App) renderBody(l layout) string {
	switch a.tab {
	case tabChat:
		return a.renderChat(l)
	case tabLog:
		a.addZone(zone{x0: 0, y0: 2, x1: a.width, y1: 2 + l.bodyH, wheel: func(d int) tea.Cmd {
			a.logVP.scroll(d, l.bodyH)
			a.logFollow = a.logVP.atBottom(l.bodyH)
			return nil
		}})
		return a.renderLog(l)
	}
	if a.state != stateConnected && a.tab != tabChat && a.sess == nil {
		return fitLines(a.renderConnState(l), a.width, l.bodyH)
	}
	list := a.lists[a.tab]
	if l.stacked {
		if a.focus == focusList {
			out := list.view(a.width, l.bodyH, true)
			a.listZones(list, 0, 2, a.width, l.bodyH, a.activateSelection)
			return out
		}
		a.detailX = 0
		return a.renderDetail(a.width, l.bodyH)
	}
	left := list.view(l.listW, l.bodyH, a.focus == focusList)
	a.listZones(list, 0, 2, l.listW, l.bodyH, a.activateSelection)
	a.detailX = l.listW + 1
	right := a.renderDetail(l.detailW, l.bodyH)
	sepLines := make([]string, l.bodyH)
	for i := range sepLines {
		sepLines[i] = a.th.sepStyle.Render("│")
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Join(sepLines, "\n"), right)
}

func (a *App) renderConnState(l layout) string {
	th := a.th
	var b strings.Builder
	b.WriteString("\n")
	switch a.state {
	case stateConnecting:
		fmt.Fprintf(&b, "  %s Connecting to %s…\n", a.spin.View(), th.bold.Render(a.profile.Target()))
		b.WriteString(th.dimText.Render("\n  ctrl+c cancel · 5 log"))
	default:
		fmt.Fprintf(&b, "  %s\n\n", th.badText.Bold(true).Render("Could not connect to "+a.profile.Target()))
		if a.connErr != nil {
			b.WriteString(indent(wrap(a.connErr.Error(), a.width-4), "  "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		hints := []string{th.keyHint.Render("R") + " retry", th.keyHint.Render("5") + " log"}
		if a.profile.Transport == "http" && a.profile.Auth == config.AuthOAuth {
			hints = append(hints, th.keyHint.Render("L")+" log in")
			if a.connErr != nil && isClientRejected(a.connErr) {
				hints = append(hints, th.keyHint.Render("x")+" reset client registration")
			}
		}
		hints = append(hints, th.keyHint.Render("q")+" quit")
		b.WriteString("  " + strings.Join(hints, "  ·  "))
	}
	return b.String()
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (a *App) placeOverlay(body string, l layout) string {
	w := min(a.width-4, 100)
	content := a.overlay.view(w-4, l.bodyH-4)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(a.th.accent).Padding(0, 1).Width(w).Render(content)
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	ox, oy := max((a.width-bw)/2, 0), 2+max((l.bodyH-bh)/2, 0)
	ov := a.overlay
	a.addZone(zone{x0: 0, y0: 0, x1: a.width, y1: a.height,
		click: func(int, int, bool) tea.Cmd {
			if o, ok := ov.(interface{ clickOutside() tea.Cmd }); ok {
				return o.clickOutside()
			}
			return nil
		},
		wheel: func(d int) tea.Cmd {
			if o, ok := ov.(interface{ wheel(int) }); ok {
				o.wheel(d)
			}
			return nil
		},
	})
	a.addZone(zone{x0: ox, y0: oy, x1: ox + bw, y1: oy + bh, click: func(int, int, bool) tea.Cmd { return nil }})
	a.registerOverlayButtons(ox, oy, ox+bw, oy+bh)
	return lipgloss.Place(a.width, l.bodyH, lipgloss.Center, lipgloss.Center, box)
}

// registerOverlayButtons makes the overlay's [ Label key ] buttons clickable.
func (a *App) registerOverlayButtons(x0, y0, x1, y1 int) {
	o, ok := a.overlay.(interface{ buttons() [][2]string })
	if !ok {
		return
	}
	for _, b := range o.buttons() {
		a.keyButton(x0, y0, x1, y1, buttonLabel(b[0], b[1]), b[1])
	}
}

// buttonLabel is the plain text of a rendered button.
func buttonLabel(label, key string) string {
	return "[ " + label + " " + keyGlyph(key) + " ]"
}

func keyGlyph(key string) string {
	if key == "enter" {
		return "⏎"
	}
	return key
}

// renderButton renders a clickable [ Label key ] button.
func (a *App) renderButton(label, key string) string {
	th := a.th
	return th.accentText.Render("[ "+label+" ") + th.keyHint.Render(keyGlyph(key)) + th.accentText.Render(" ]")
}

// stripAll removes styling from each string.
func stripAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = ansiStrip(s)
	}
	return out
}
