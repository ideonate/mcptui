package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/schemaform"
)

// pickerState is the profile chooser shown by a bare `mcptui` or ctrl+p.
type pickerState struct {
	list *listPane
	form *schemaform.Form // non-nil while creating or editing
	// editing is the profile being edited ("" for a new one).
	editing string
	err     string
}

const profileFormSchema = `{
  "type": "object",
  "required": ["name", "target"],
  "properties": {
    "name":      {"type": "string", "title": "Name", "description": "letters, digits, - _ ."},
    "target":    {"type": "string", "title": "URL or command", "description": "https://host/mcp, or a stdio command such as: npx -y @modelcontextprotocol/server-everything"},
    "env_badge": {"type": "string", "title": "Env badge", "enum": ["dev", "staging", "prod"], "description": "header colour; prod also confirms tools that aren't read-only"},
    "auth":      {"type": "string", "title": "Auth", "enum": ["oauth", "bearer", "none"], "description": "HTTP only; oauth (default) is only used if the server returns 401"},
    "token":     {"type": "string", "title": "Token", "description": "for bearer auth: the token, or env:VAR"},
    "scope":     {"type": "string", "title": "Scope", "description": "optional OAuth scope"},
    "client_id": {"type": "string", "title": "Client ID", "description": "only if the server has no dynamic client registration"},
    "default":   {"type": "boolean", "title": "Default", "description": "open this profile when mcptui starts"}
  }
}`

var profileNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (a *App) config() *config.Config {
	if a.opts.Config == nil {
		cfg, err := config.Load("")
		if err != nil {
			cfg = &config.Config{Profiles: map[string]*config.Profile{}}
			a.setFlash("config: %v", err)
		}
		a.opts.Config = cfg
	}
	return a.opts.Config
}

// openPicker shows the profile chooser. With no profiles, it goes straight
// to the new-profile form.
func (a *App) openPicker() tea.Cmd {
	a.picker = &pickerState{list: newListPane(a.th, "no saved profiles")}
	a.rebuildPickerList("")
	if len(a.config().Profiles) == 0 && a.profile == nil {
		return a.openProfileForm("", nil)
	}
	return nil
}

func (a *App) rebuildPickerList(selectName string) {
	cfg := a.config()
	var items []listItem
	if a.profile != nil && a.profile.Temporary {
		items = append(items, listItem{
			key:    "temp:",
			title:  a.profile.Name,
			desc:   a.profile.Target(),
			marker: a.th.warnText.Render("unsaved · s to save"),
			group:  "current connection",
		})
	}
	for _, name := range cfg.ProfileNames() {
		p, _ := cfg.Resolve(name)
		marker := a.th.faintText.Render(truncate(p.Target(), 60))
		if b := a.th.envBadge(p.EnvBadge); b != "" {
			marker = b + " " + marker
		}
		if name == cfg.DefaultProfile {
			marker = a.th.accentText.Render("★") + " " + marker
		}
		if a.profile != nil && !a.profile.Temporary && a.profile.Name == name {
			marker = a.th.okText.Render("● ") + marker
		}
		items = append(items, listItem{key: "profile:" + name, title: name, desc: p.Target(), marker: marker, group: "profiles"})
	}
	items = append(items, listItem{key: "new:", title: a.th.accentText.Render("+ New profile…"), group: " "})
	l := a.picker.list
	l.setItems(items)
	switch {
	case selectName != "":
		l.selectKey("profile:" + selectName)
	case a.profile != nil && !a.profile.Temporary:
		l.selectKey("profile:" + a.profile.Name)
	case cfg.DefaultProfile != "":
		l.selectKey("profile:" + cfg.DefaultProfile)
	}
}

// openProfileForm starts creating (name == "") or editing a profile.
// prefill seeds a new profile (e.g. from a temporary connection).
func (a *App) openProfileForm(name string, prefill *config.Profile) tea.Cmd {
	f, err := schemaform.Parse(json.RawMessage(profileFormSchema))
	if err != nil {
		a.setFlash("%v", err)
		return nil
	}
	f.UseTitleLabels()
	f.SetWidth(max(min(a.width-4, 100), 20))
	src := prefill
	if name != "" {
		src = a.config().Profiles[name]
	}
	if src != nil {
		vals := map[string]any{"target": targetString(src)}
		if name != "" {
			vals["name"] = name
			vals["default"] = a.config().DefaultProfile == name
		} else if !src.Temporary {
			vals["name"] = src.Name
		}
		for k, v := range map[string]string{"env_badge": src.EnvBadge, "auth": src.Auth, "token": src.Token, "scope": src.Scope, "client_id": src.ClientID} {
			if v != "" {
				vals[k] = v
			}
		}
		b, _ := json.Marshal(vals)
		_ = f.SetValues(b)
	}
	a.picker.form = f
	a.picker.editing = name
	a.picker.err = ""
	return f.Focus()
}

func targetString(p *config.Profile) string {
	if p.Transport == "stdio" || (p.URL == "" && len(p.Command) > 0) {
		q := make([]string, len(p.Command))
		for i, arg := range p.Command {
			if arg == "" || strings.ContainsAny(arg, " \t'\"\\") {
				arg = "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
			}
			q[i] = arg
		}
		return strings.Join(q, " ")
	}
	return p.URL
}

// saveProfileForm validates the form and writes config.toml. It returns the
// saved profile name.
func (a *App) saveProfileForm() (string, error) {
	pk := a.picker
	if errs := pk.form.Validate(); len(errs) > 0 {
		return "", errs[0]
	}
	raw, err := pk.form.Values()
	if err != nil {
		return "", err
	}
	var v struct {
		Name     string `json:"name"`
		Target   string `json:"target"`
		EnvBadge string `json:"env_badge"`
		Auth     string `json:"auth"`
		Token    string `json:"token"`
		Scope    string `json:"scope"`
		ClientID string `json:"client_id"`
		Default  bool   `json:"default"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	v.Name = strings.TrimSpace(v.Name)
	v.Target = strings.TrimSpace(v.Target)
	if !profileNameRE.MatchString(v.Name) {
		return "", fmt.Errorf("name may only contain letters, digits, - _ and .")
	}
	cfg := a.config()
	if _, exists := cfg.Profiles[v.Name]; exists && v.Name != pk.editing {
		return "", fmt.Errorf("a profile named %q already exists", v.Name)
	}
	var p *config.Profile
	if pk.editing != "" {
		cp := *cfg.Profiles[pk.editing]
		p = &cp
	} else {
		p = &config.Profile{}
	}
	p.Name = v.Name
	if strings.HasPrefix(v.Target, "http://") || strings.HasPrefix(v.Target, "https://") {
		p.Transport, p.URL, p.Command = "", v.Target, nil
	} else {
		argv, err := splitArgs(v.Target)
		if err != nil {
			return "", err
		}
		p.Transport, p.URL, p.Command = "stdio", "", argv
	}
	p.EnvBadge, p.Auth, p.Token, p.Scope, p.ClientID = v.EnvBadge, v.Auth, v.Token, v.Scope, v.ClientID

	// Validate with defaults applied, as they will be when connecting.
	probe := &config.Config{Profiles: map[string]*config.Profile{p.Name: p}}
	resolved, _ := probe.Resolve(p.Name)
	if err := resolved.Validate(); err != nil {
		return "", err
	}

	if pk.editing != "" && pk.editing != v.Name {
		delete(cfg.Profiles, pk.editing)
		if cfg.DefaultProfile == pk.editing {
			cfg.DefaultProfile = v.Name
		}
	}
	cfg.Profiles[v.Name] = p
	switch {
	case v.Default || len(cfg.Profiles) == 1:
		cfg.DefaultProfile = v.Name
	case cfg.DefaultProfile == v.Name:
		cfg.DefaultProfile = ""
	}
	if err := cfg.Save(); err != nil {
		return "", err
	}
	// Keep the running connection's profile in step with a rename/edit.
	if a.profile != nil && !a.profile.Temporary && a.profile.Name == pk.editing {
		a.profile.Name = v.Name
		a.profile.EnvBadge = p.EnvBadge
	}
	return v.Name, nil
}

// splitArgs splits a command line with simple shell quoting.
func splitArgs(s string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		inArg bool
		esc   bool
	)
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, inArg = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	if inArg {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enter a URL or a command")
	}
	return out, nil
}

// switchProfile drops the current connection and connects to p.
func (a *App) switchProfile(p *config.Profile) tea.Cmd {
	a.picker = nil
	if a.profile != nil && p.Name == a.profile.Name && p.Temporary == a.profile.Temporary && a.sess != nil {
		return nil
	}
	if a.opCancel != nil {
		a.opCancel()
		a.opCancel = nil
	}
	if a.sess != nil {
		old := a.sess
		go old.Close()
		a.sess = nil
	}
	a.profile = p
	a.cred = nil
	a.tools, a.prompts, a.resources, a.templates = nil, nil, nil, nil
	a.listErr = map[string]error{}
	a.loading = map[string]bool{}
	a.subscribed = map[string]bool{}
	a.forms = map[string]*schemaform.Form{}
	a.lastArgs = map[string]json.RawMessage{}
	a.lastCompletion = nil
	for i := range a.results {
		a.results[i] = nil
	}
	a.chat = newChatState()
	a.runs = map[int]*resultState{}
	for _, t := range []tabID{tabTools, tabPrompts, tabResources} {
		a.lists[t].setItems(nil)
		a.lists[t].touched = false
	}
	a.tab, a.focus = tabTools, focusList
	a.logf("client", "info", "switching to profile %s (%s)", p.Name, p.Target())
	return a.connect()
}

func (a *App) closePicker() {
	if a.profile == nil {
		return // nothing to go back to
	}
	a.picker = nil
}

func (a *App) pickerUpdate(msg tea.Msg) tea.Cmd {
	pk := a.picker
	if pk.form != nil {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			switch k.String() {
			case "esc":
				pk.form = nil
				pk.err = ""
				if len(a.config().Profiles) == 0 && a.profile == nil {
					return tea.Quit
				}
				return nil
			case "enter", "ctrl+s":
				if k.String() == "enter" && pk.form.FocusedMultiline() {
					break
				}
				name, err := a.saveProfileForm()
				if err != nil {
					pk.err = err.Error()
					return nil
				}
				wasNew := pk.editing == ""
				pk.form = nil
				pk.err = ""
				if wasNew {
					p, _ := a.config().Resolve(name)
					a.setFlash("saved profile %s to %s", name, a.config().Path())
					return a.switchProfile(p)
				}
				a.rebuildPickerList(name)
				a.setFlash("saved profile %s", name)
				return nil
			}
		}
		return pk.form.Update(msg)
	}

	if pk.list.filtering {
		cmd, _ := pk.list.updateFilter(msg)
		return cmd
	}
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	sel := pk.list.selected()
	selName := ""
	if sel != nil {
		selName = strings.TrimPrefix(sel.key, "profile:")
	}
	switch k.String() {
	case "up", "k":
		pk.list.move(-1)
	case "down", "j":
		pk.list.move(1)
	case "/":
		return pk.list.startFilter()
	case "q":
		if a.profile == nil {
			return tea.Quit
		}
		a.closePicker()
	case "esc", "ctrl+p":
		if a.profile == nil {
			return tea.Quit
		}
		a.closePicker()
	case "enter":
		if sel == nil {
			return nil
		}
		if sel.key == "temp:" {
			a.closePicker()
			return nil
		}
		if sel.key == "new:" {
			return a.openProfileForm("", nil)
		}
		p, err := a.config().Resolve(selName)
		if err != nil {
			a.setFlash("%v", err)
			return nil
		}
		return a.switchProfile(p)
	case "n", "a":
		return a.openProfileForm("", nil)
	case "s":
		if sel != nil && sel.key == "temp:" {
			return a.openProfileForm("", a.profile)
		}
	case "e":
		if sel != nil && strings.HasPrefix(sel.key, "profile:") {
			return a.openProfileForm(selName, nil)
		}
	case "*":
		if sel != nil && strings.HasPrefix(sel.key, "profile:") {
			cfg := a.config()
			cfg.DefaultProfile = selName
			if err := cfg.Save(); err != nil {
				a.setFlash("%v", err)
				return nil
			}
			a.rebuildPickerList(selName)
			a.setFlash("%s is now the default profile", selName)
		}
	case "d", "x":
		if sel == nil || !strings.HasPrefix(sel.key, "profile:") {
			return nil
		}
		if a.profile != nil && !a.profile.Temporary && a.profile.Name == selName {
			a.setFlash("can't delete the profile you're connected to")
			return nil
		}
		a.overlay = newConfirmOverlay(a, fmt.Sprintf("Delete profile %q from %s?\n\nStored OAuth tokens are kept; use `mcptui auth logout` first to revoke them.", selName, a.config().Path()), func() tea.Cmd {
			cfg := a.config()
			delete(cfg.Profiles, selName)
			if cfg.DefaultProfile == selName {
				cfg.DefaultProfile = ""
			}
			if err := cfg.Save(); err != nil {
				a.setFlash("%v", err)
				return nil
			}
			a.rebuildPickerList("")
			a.setFlash("deleted profile %s", selName)
			return nil
		})
	}
	return nil
}

func (a *App) renderPicker(height int) string {
	th := a.th
	pk := a.picker
	w := a.width
	if pk.form != nil {
		title := "New profile"
		if pk.editing != "" {
			title = "Edit profile " + pk.editing
		}
		lines := []string{" " + th.title.Render(title), ""}
		formLines := strings.Split(pk.form.View(), "\n")
		for _, l := range formLines {
			lines = append(lines, " "+l)
		}
		if pk.err != "" {
			lines = append(lines, "", " "+th.badText.Render(wrap(pk.err, w-2)))
		}
		lines = append(lines, "", " "+a.renderButton("Save", "enter")+"  "+a.renderButton("Cancel", "esc"),
			"", " "+th.dimText.Render("Saved to "+a.config().Path()))
		form := pk.form
		a.addZone(zone{x0: 0, y0: 4, x1: w, y1: 4 + len(formLines), click: func(x, y int, double bool) tea.Cmd {
			if fi := form.FieldAtLine(y); fi >= 0 {
				return form.ClickField(fi)
			}
			return nil
		}})
		a.keyButton(0, 2, w, 2+height, buttonLabel("Save", "enter"), "ctrl+s")
		a.keyButton(0, 2, w, 2+height, buttonLabel("Cancel", "esc"), "esc")
		return fitLines(strings.Join(lines, "\n"), w, height)
	}
	head := " " + th.title.Render("Choose a profile") + th.dimText.Render("   double-click or ⏎ to connect")
	a.listZones(pk.list, 0, 3, w, height-1, func() tea.Cmd { return a.pickerUpdate(keyFromString("enter")) })
	return head + "\n" + pk.list.view(w, height-1, true)
}

func (a *App) pickerHints() [][2]string {
	pk := a.picker
	switch {
	case pk.form != nil:
		return [][2]string{{"⏎/ctrl+s", "save"}, {"tab", "next field"}, {"←→", "choose"}, {"esc", "cancel"}}
	case pk.list.filtering:
		return [][2]string{{"enter", "done"}, {"esc", "clear"}}
	}
	back := "quit"
	if a.profile != nil {
		back = "back"
	}
	hints := [][2]string{{"⏎", "connect"}, {"n", "new"}, {"e", "edit"}, {"d", "delete"}, {"*", "default"}, {"/", "filter"}, {"esc", back}}
	if sel := pk.list.selected(); sel != nil && sel.key == "temp:" {
		hints = append([][2]string{{"s", "save as profile"}}, hints...)
	}
	return hints
}
