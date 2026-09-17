package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/paths"
)

type overlay interface {
	init() tea.Cmd
	update(msg tea.Msg) tea.Cmd
	view(width, height int) string
	hints() [][2]string
	fullscreen() bool
	cancelOnCtrlC() bool
	close()
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

func ansiStrip(s string) string { return ansiRE.ReplaceAllString(s, "") }

// ---------------------------------------------------------------- text pager

// pagerOverlay shows scrollable read-only content (help, instructions, docs).
type pagerOverlay struct {
	a       *App
	title   string
	content func(width int) string
	sv      scrollView
	h       int
}

func (o *pagerOverlay) clickOutside() tea.Cmd { o.a.overlay = nil; return nil }
func (o *pagerOverlay) wheel(d int)           { o.sv.scroll(d, o.h) }
func (o *pagerOverlay) init() tea.Cmd         { return nil }
func (o *pagerOverlay) fullscreen() bool      { return false }
func (o *pagerOverlay) cancelOnCtrlC() bool   { return true }
func (o *pagerOverlay) close()                {}
func (o *pagerOverlay) hints() [][2]string {
	return [][2]string{{"j/k", "scroll"}, {"esc", "close"}}
}

func (o *pagerOverlay) update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "esc", "q", "?", "i", "enter", "d":
		o.a.overlay = nil
	case "up", "k":
		o.sv.scroll(-1, o.h)
	case "down", "j":
		o.sv.scroll(1, o.h)
	case "pgup", "b":
		o.sv.scroll(-o.h, o.h)
	case "pgdown", "space", "f":
		o.sv.scroll(o.h, o.h)
	case "y":
		o.a.setFlash("copied")
		return tea.SetClipboard(ansiStrip(o.content(200)))
	}
	return nil
}

func (o *pagerOverlay) view(width, height int) string {
	o.h = max(height-2, 1)
	off := o.sv.offset
	o.sv.set(o.content(width))
	o.sv.offset = off
	return o.a.th.title.Render(o.title) + "\n\n" + strings.TrimRight(o.sv.view(width, o.h), " \n")
}

func newHelpOverlay(a *App) overlay {
	return &pagerOverlay{a: a, title: "Keys", content: func(w int) string {
		th := a.th
		sec := func(title string, rows [][2]string) string {
			var b strings.Builder
			b.WriteString(th.bold.Render(title) + "\n")
			for _, r := range rows {
				b.WriteString("  " + padRight(th.keyHint.Render(r[0]), 16) + r[1] + "\n")
			}
			return b.String()
		}
		return strings.Join([]string{
			sec("Mouse", [][2]string{
				{"click", "tabs, list rows, form fields, [ buttons ], footer keys"}, {"double-click", "open a list item / connect to a profile"},
				{"wheel", "move through lists, scroll results, log and popups"}, {"shift+drag", "select text (option+drag in some terminals), or run with --no-mouse"},
			}),
			sec("Anywhere", [][2]string{
				{"esc / ↑", "up to the tab bar, then ← → to switch tabs"}, {"1-5", "jump to a tab"}, {"R", "refresh lists (or reconnect)"}, {"L / O", "log in / log out (OAuth)"}, {"P", "ping"}, {"ctrl+p", "profiles: switch, create, edit"},
				{"i", "server instructions & info"}, {"?", "this help"}, {"ctrl+c", "cancel running request · quit"}, {"q", "quit (from lists)"},
			}),
			sec("Lists", [][2]string{
				{"↑↓ j k", "move"}, {"/", "filter"}, {"⏎", "open form, or run it if it has no arguments"}, {"tab shift+tab", "cycle focus: tabs → list → fields → result"}, {"e / E", "edit arguments as JSON / in $EDITOR"},
				{"d", "full description and schemas"}, {"S", "subscribe / unsubscribe resource"}, {"x", "reset client registration (after rejection)"},
			}),
			sec("Form", [][2]string{
				{"tab shift+tab", "next / previous field (leaves the form at the ends)"}, {"⏎ ctrl+s", "call / get / read"}, {"space ← →", "toggle bool, cycle enum"},
				{"ctrl+n ctrl+d", "add / remove array item"}, {"ctrl+t", "multi-line string"}, {"ctrl+e ctrl+o", "JSON editor / $EDITOR"},
				{"ctrl+space", "complete argument (if supported)"}, {"ctrl+↓", "focus result"}, {"esc", "back to list"},
			}),
			sec("Result", [][2]string{
				{"p m r", "pretty / markdown / raw JSON-RPC"}, {"j k pgup pgdn", "scroll"}, {"y", "copy"}, {"s", "save to file"},
				{"c / C", "copy as mcptui command / curl"}, {"esc", "back to form"},
			}),
			sec("Chat", [][2]string{
				{"name args", `send: add(1, 2) · add {"a":1} · add a=1 · greet name=Ada · read <uri> · ping`}, {"tab / ⏎", "fill in a suggested name / open its form"}, {"↑ (empty box)", "browse Tools / Prompts / Resources, ←→ ⏎ to pick a category, ↑ again for earlier commands"},
				{"↑ ↓", "earlier commands (or pick a suggestion)"}, {"shift+tab", "into the transcript: ↑↓ select, r rerun, e edit, a show all"},
				{"ctrl+r", "rerun the selected request"}, {"e", "edit its arguments in a form"},
			}),
			sec("Log", [][2]string{{"t", "show raw JSON-RPC traffic"}, {"G", "follow"}, {"c", "clear"}, {"y", "copy"}}),
		}, "\n")
	}}
}

func newInstructionsOverlay(a *App) overlay {
	return &pagerOverlay{a: a, title: "Server", content: func(w int) string {
		th := a.th
		if a.sess == nil || a.sess.Client.InitializeResult() == nil {
			return th.dimText.Render("not connected")
		}
		init := a.sess.Client.InitializeResult()
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s\n", th.bold.Render(firstNonEmpty(init.ServerInfo.Title, init.ServerInfo.Name)), init.ServerInfo.Version)
		fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("protocol"), init.ProtocolVersion)
		fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("target  "), a.profile.Target())
		if sid := a.sess.SessionID(); sid != "" {
			fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("session "), sid)
		}
		if a.cred != nil {
			fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("auth    "), a.cred.AuthorizationServer)
			fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("client  "), a.cred.ClientID)
			if a.cred.Scope != "" {
				fmt.Fprintf(&b, "%s %s\n", th.dimText.Render("scope   "), a.cred.Scope)
			}
		}
		b.WriteString("\n" + a.rd.section("instructions", w) + "\n")
		if init.Instructions == "" {
			b.WriteString(th.dimText.Render("(the server sent no instructions)") + "\n")
		} else {
			b.WriteString(a.rd.markdown(init.Instructions, w) + "\n")
		}
		b.WriteString("\n" + a.rd.section("capabilities", w) + "\n")
		b.WriteString(a.rd.jsonBlock(init.RawCapabilities, w))
		return b.String()
	}}
}

// newDocOverlay shows the full description and schemas for the selection.
func newDocOverlay(a *App, key string) overlay {
	kind, name, _ := strings.Cut(key, ":")
	return &pagerOverlay{a: a, title: name, content: func(w int) string {
		var parts []string
		switch kind {
		case "tool":
			t := a.findTool(name)
			if t == nil {
				return ""
			}
			if t.Description != "" {
				parts = append(parts, a.rd.markdown(t.Description, w))
			}
			if len(t.InputSchema) > 0 {
				parts = append(parts, a.rd.section("input schema", w), a.rd.jsonBlock(t.InputSchema, w))
			}
			if len(t.OutputSchema) > 0 {
				parts = append(parts, a.rd.section("output schema", w), a.rd.jsonBlock(t.OutputSchema, w))
			}
			if t.Annotations != nil {
				b, _ := json.Marshal(t.Annotations)
				parts = append(parts, a.rd.section("annotations", w), a.rd.jsonBlock(b, w))
			}
		case "prompt":
			p := a.findPrompt(name)
			if p == nil {
				return ""
			}
			parts = append(parts, a.rd.markdown(p.Description, w))
			b, _ := json.Marshal(p.Arguments)
			parts = append(parts, a.rd.section("arguments", w), a.rd.jsonBlock(b, w))
		case "res":
			r := a.findResource(name)
			if r == nil {
				return ""
			}
			b, _ := json.Marshal(r)
			parts = append(parts, a.rd.markdown(r.Description, w), a.rd.jsonBlock(b, w))
		case "tmpl":
			t := a.findTemplate(name)
			if t == nil {
				return ""
			}
			b, _ := json.Marshal(t)
			parts = append(parts, a.rd.markdown(t.Description, w), a.rd.jsonBlock(b, w))
		}
		return strings.Join(parts, "\n")
	}}
}

// ---------------------------------------------------------------- confirm

type confirmOverlay struct {
	a     *App
	text  string
	onYes func() tea.Cmd
}

func newConfirmOverlay(a *App, text string, onYes func() tea.Cmd) overlay {
	return &confirmOverlay{a: a, text: text, onYes: onYes}
}

func (o *confirmOverlay) buttons() [][2]string { return [][2]string{{"Yes", "y"}, {"No", "n"}} }
func (o *confirmOverlay) init() tea.Cmd        { return nil }
func (o *confirmOverlay) fullscreen() bool     { return false }
func (o *confirmOverlay) cancelOnCtrlC() bool  { return true }
func (o *confirmOverlay) close()               {}
func (o *confirmOverlay) hints() [][2]string   { return [][2]string{{"y", "yes"}, {"n/esc", "no"}} }

func (o *confirmOverlay) update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "y", "Y":
		o.a.overlay = nil
		return o.onYes()
	case "n", "N", "esc", "q":
		o.a.overlay = nil
		o.a.setFlash("cancelled")
	}
	return nil
}

func (o *confirmOverlay) view(width, height int) string {
	th := o.a.th
	lines := strings.Split(wrap(o.text, width), "\n")
	if len(lines) > height-2 {
		lines = append(lines[:max(height-3, 1)], "…")
	}
	return th.warnText.Bold(true).Render("Confirm") + "\n\n" + strings.Join(lines, "\n") + "\n\n" +
		o.a.renderButton("Yes", "y") + "  " + o.a.renderButton("No", "n")
}

// ---------------------------------------------------------------- save

type saveOverlay struct {
	a   *App
	rs  *resultState
	in  textinput.Model
	err string
}

func newSaveOverlay(a *App, rs *resultState) overlay {
	in := textinput.New()
	in.Prompt = "path: "
	name := rs.title
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>| `, r) {
			return '_'
		}
		return r
	}, name)
	ext := ".json"
	switch {
	case rs.tool != nil:
		for _, c := range rs.tool.Content {
			if c.MimeType != "" {
				ext = extFor(c.MimeType)
				break
			}
		}
		if len(rs.tool.Content) == 1 && rs.tool.Content[0].Type == "text" {
			ext = ".txt"
		}
	case rs.read != nil && len(rs.read.Contents) > 0:
		ext = extFor(rs.read.Contents[0].MimeType)
	}
	if rs.mode == modeRaw {
		ext = ".json"
	}
	in.SetValue(name + ext)
	in.CursorEnd()
	return &saveOverlay{a: a, rs: rs, in: in}
}

func extFor(mime string) string {
	switch {
	case strings.Contains(mime, "json"):
		return ".json"
	case strings.Contains(mime, "markdown"):
		return ".md"
	case strings.HasPrefix(mime, "text/"):
		return ".txt"
	case mime == "image/png":
		return ".png"
	case mime == "image/jpeg":
		return ".jpg"
	case mime == "image/gif":
		return ".gif"
	case mime == "image/webp":
		return ".webp"
	case mime == "audio/wav" || mime == "audio/x-wav":
		return ".wav"
	case mime == "audio/mpeg":
		return ".mp3"
	case mime == "application/pdf":
		return ".pdf"
	}
	return ".bin"
}

func (o *saveOverlay) buttons() [][2]string {
	return [][2]string{{"Save", "enter"}, {"Cancel", "esc"}}
}
func (o *saveOverlay) clickOutside() tea.Cmd { o.a.overlay = nil; return nil }
func (o *saveOverlay) init() tea.Cmd         { return o.in.Focus() }
func (o *saveOverlay) fullscreen() bool      { return false }
func (o *saveOverlay) cancelOnCtrlC() bool   { return true }
func (o *saveOverlay) close()                {}
func (o *saveOverlay) hints() [][2]string    { return [][2]string{{"⏎", "save"}, {"esc", "cancel"}} }

func (o *saveOverlay) update(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			o.a.overlay = nil
			return nil
		case "enter":
			path := strings.TrimSpace(o.in.Value())
			if strings.HasPrefix(path, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					path = filepath.Join(home, path[2:])
				}
			}
			n, err := saveResult(o.rs, path)
			if err != nil {
				o.err = err.Error()
				return nil
			}
			o.a.overlay = nil
			o.a.setFlash("saved %s to %s", humanBytes(n), path)
			return nil
		}
	}
	var cmd tea.Cmd
	o.in, cmd = o.in.Update(msg)
	return cmd
}

func (o *saveOverlay) view(width, height int) string {
	th := o.a.th
	o.in.SetWidth(max(width-8, 10))
	s := th.bold.Render("Save result") + "\n\n" + o.in.View() + "\n\n" + o.a.renderButton("Save", "enter") + "  " + o.a.renderButton("Cancel", "esc")
	if o.err != "" {
		s += "\n" + th.badText.Render(wrap(o.err, width))
	}
	return s
}

// ---------------------------------------------------------------- JSON editor

type jsonOverlay struct {
	a   *App
	key string
	ta  textarea.Model
	err string
}

func newJSONOverlay(a *App, key string, current json.RawMessage) overlay {
	ta := textarea.New()
	ta.ShowLineNumbers = true
	ta.CharLimit = 0
	ta.MaxHeight = 0
	p, ok := prettyJSON(current)
	if !ok {
		p = string(current)
	}
	ta.SetValue(p)
	return &jsonOverlay{a: a, key: key, ta: ta}
}

func (o *jsonOverlay) buttons() [][2]string {
	return [][2]string{{"Apply", "ctrl+s"}, {"Apply & run", "ctrl+g"}, {"Format", "ctrl+f"}, {"Cancel", "esc"}}
}
func (o *jsonOverlay) init() tea.Cmd       { return o.ta.Focus() }
func (o *jsonOverlay) fullscreen() bool    { return true }
func (o *jsonOverlay) cancelOnCtrlC() bool { return true }
func (o *jsonOverlay) close()              {}
func (o *jsonOverlay) hints() [][2]string {
	return [][2]string{{"ctrl+s", "apply"}, {"ctrl+g", "apply & run"}, {"ctrl+f", "format"}, {"esc", "cancel"}}
}

func (o *jsonOverlay) update(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			o.a.overlay = nil
			return o.a.focusFormArea()
		case "ctrl+f":
			if p, ok := prettyJSON([]byte(o.ta.Value())); ok {
				o.ta.SetValue(p)
				o.err = ""
			} else {
				o.err = "not valid JSON"
			}
			return nil
		case "ctrl+s", "ctrl+g":
			var obj map[string]any
			if err := json.Unmarshal([]byte(o.ta.Value()), &obj); err != nil {
				o.err = "arguments must be a JSON object: " + err.Error()
				return nil
			}
			o.a.overlay = nil
			cmd := o.a.applyArgsJSON(o.key, []byte(o.ta.Value()))
			if k.String() == "ctrl+g" {
				return tea.Batch(cmd, o.a.execute())
			}
			return cmd
		}
	}
	var cmd tea.Cmd
	o.ta, cmd = o.ta.Update(msg)
	return cmd
}

func (o *jsonOverlay) view(width, height int) string {
	th := o.a.th
	_, name, _ := strings.Cut(o.key, ":")
	head := " " + th.title.Render("Arguments JSON") + " " + th.dimText.Render(name) + "   " +
		o.a.renderButton("Apply", "ctrl+s") + " " + o.a.renderButton("Apply & run", "ctrl+g") + " " +
		o.a.renderButton("Format", "ctrl+f") + " " + o.a.renderButton("Cancel", "esc")
	errLine := ""
	if o.err != "" {
		errLine = " " + th.badText.Render(truncate(o.err, width-2))
	} else if json.Valid([]byte(o.ta.Value())) {
		errLine = " " + th.okText.Render("✓ valid JSON")
	} else {
		errLine = " " + th.warnText.Render("… not valid JSON yet")
	}
	o.ta.SetWidth(max(width-2, 10))
	o.ta.SetHeight(max(height-3, 3))
	return head + "\n" + indent(o.ta.View(), " ") + "\n" + errLine
}

// ---------------------------------------------------------------- login

type loginOverlay struct {
	a       *App
	p       auth.LoginPrompt
	in      textinput.Model
	done    bool
	status  string
	urlFile string
}

func newLoginOverlay(a *App, p auth.LoginPrompt) *loginOverlay {
	in := textinput.New()
	in.Prompt = "paste: "
	in.Placeholder = "press p (or just paste) the redirect URL from the browser"
	o := &loginOverlay{a: a, p: p, in: in}
	// A file copy of the URL is an unbroken line for `cat` in another shell.
	if err := paths.EnsureDir(paths.StateDir()); err == nil {
		f := filepath.Join(paths.StateDir(), "login-url.txt")
		if os.WriteFile(f, []byte(p.AuthURL+"\n"), 0o600) == nil {
			o.urlFile = f
		}
	}
	return o
}

func (o *loginOverlay) buttons() [][2]string {
	return [][2]string{{"Copy URL", "c"}, {"Open browser", "o"}, {"Paste redirect URL", "p"}}
}
func (o *loginOverlay) init() tea.Cmd       { return nil }
func (o *loginOverlay) fullscreen() bool    { return false }
func (o *loginOverlay) cancelOnCtrlC() bool { return true }

func (o *loginOverlay) close() {
	if !o.done && o.a.opCancel != nil {
		o.a.opCancel()
	}
}

func (o *loginOverlay) hints() [][2]string {
	if o.in.Focused() {
		return [][2]string{{"⏎", "submit pasted URL"}, {"esc", "stop pasting"}}
	}
	return [][2]string{{"c", "copy URL"}, {"o", "open browser"}, {"p", "paste redirect URL"}, {"esc", "cancel login"}}
}

func (o *loginOverlay) update(msg tea.Msg) tea.Cmd {
	if pm, ok := msg.(tea.PasteMsg); ok && !o.in.Focused() {
		cmd := o.in.Focus()
		var c2 tea.Cmd
		o.in, c2 = o.in.Update(pm)
		return tea.Batch(cmd, c2)
	}
	k, isKey := msg.(tea.KeyPressMsg)
	if isKey && o.in.Focused() {
		switch k.String() {
		case "esc":
			o.in.Blur()
			return nil
		case "enter":
			v := strings.TrimSpace(o.in.Value())
			if v == "" {
				return nil
			}
			o.p.Submit(v)
			o.status = "submitted; exchanging code…"
			o.in.SetValue("")
			o.in.Blur()
			return nil
		}
		var cmd tea.Cmd
		o.in, cmd = o.in.Update(msg)
		return cmd
	}
	if !isKey {
		return nil
	}
	switch k.String() {
	case "esc", "q":
		o.close()
		o.a.overlay = nil
		o.a.setFlash("login cancelled")
		return nil
	case "c", "y", "ctrl+y":
		o.status = copyStatus(o.p.AuthURL)
		return tea.SetClipboard(o.p.AuthURL)
	case "o", "ctrl+o":
		if err := auth.OpenBrowser(o.p.AuthURL); err != nil {
			o.status = "could not open a browser: " + err.Error()
		} else {
			o.status = "opened browser"
		}
		return nil
	case "p", "tab", "enter":
		return o.in.Focus()
	}
	// Typing without bracketed paste: start the paste field with this key.
	if k.Text != "" {
		cmd := o.in.Focus()
		var c2 tea.Cmd
		o.in, c2 = o.in.Update(msg)
		return tea.Batch(cmd, c2)
	}
	return nil
}

func (o *loginOverlay) view(width, height int) string {
	th := o.a.th
	var b strings.Builder
	b.WriteString(th.title.Render("Log in to "+o.a.profile.Name) + "\n\n")
	b.WriteString("Open this URL in a browser to authorize mcptui:\n")
	b.WriteString(o.a.renderButton("Copy URL", "c") + "  " + o.a.renderButton("Open browser", "o") + "  " + o.a.renderButton("Paste redirect URL", "p") + "\n\n")
	// The URL is wrapped to fit; every piece is an OSC 8 hyperlink to the
	// whole URL, so ctrl/cmd+click works in terminals that support it.
	link := th.infoText.Hyperlink(o.p.AuthURL)
	url := o.p.AuthURL
	for len(url) > 0 {
		n := min(width, len(url))
		b.WriteString(link.Render(url[:n]) + "\n")
		url = url[n:]
	}
	if o.urlFile != "" {
		b.WriteString(th.dimText.Render("Unbroken copy: cat "+o.urlFile) + "\n")
	}
	b.WriteString("\n" + th.dimText.Render("Waiting for the redirect to "+o.p.RedirectURI) + "\n\n")
	b.WriteString("Browser can't reach the callback? Paste the URL from the browser's address bar here.\n")
	o.in.SetWidth(max(width-8, 10))
	b.WriteString(o.in.View() + "\n")
	if o.status != "" {
		b.WriteString("\n" + th.infoText.Render(o.status))
	}
	return b.String()
}
