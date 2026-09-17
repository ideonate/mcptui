package tui

import (
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Mouse support: every render registers zones (screen rectangles with click
// and wheel handlers) and buttons (labels found in the rendered text). The
// latest registration wins, so overlays and buttons take precedence over the
// areas beneath them.

type zone struct {
	x0, y0, x1, y1 int // [x0,x1) × [y0,y1)
	// click receives coordinates relative to the zone origin.
	click func(x, y int, double bool) tea.Cmd
	wheel func(delta int) tea.Cmd
}

type buttonSpec struct {
	x0, y0, x1, y1 int
	label          string
	action         func() tea.Cmd
}

type clickRecord struct {
	x, y int
	at   time.Time
}

const doubleClickWindow = 400 * time.Millisecond

func (a *App) resetZones() {
	a.zones = a.zones[:0]
	a.buttons = a.buttons[:0]
}

func (a *App) addZone(z zone) { a.zones = append(a.zones, z) }

// addButton registers a clickable label to be located in [x0,x1)×[y0,y1).
func (a *App) addButton(x0, y0, x1, y1 int, label string, action func() tea.Cmd) {
	a.buttons = append(a.buttons, buttonSpec{x0, y0, x1, y1, label, action})
}

// keyButton is a button that behaves like pressing key.
func (a *App) keyButton(x0, y0, x1, y1 int, label, key string) {
	a.addButton(x0, y0, x1, y1, label, func() tea.Cmd { return a.onKey(keyFromString(key)) })
}

// resolveButtons finds button labels in the final screen and turns them into
// zones.
func (a *App) resolveButtons(screen string) {
	if len(a.buttons) == 0 {
		return
	}
	lines := strings.Split(screen, "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansiStrip(l)
	}
	for _, b := range a.buttons {
		width := lipgloss.Width(b.label)
		for y := max(b.y0, 0); y < b.y1 && y < len(plain); y++ {
			line := plain[y]
			from := 0
			for {
				idx := strings.Index(line[from:], b.label)
				if idx < 0 {
					break
				}
				col := lipgloss.Width(line[:from+idx])
				if col >= b.x0 && col+width <= b.x1 {
					action := b.action
					a.addZone(zone{x0: col, y0: y, x1: col + width, y1: y + 1, click: func(int, int, bool) tea.Cmd { return action() }})
				}
				_, size := utf8.DecodeRuneInString(line[from+idx:])
				from += idx + size
			}
		}
	}
}

func (a *App) onMouse(msg tea.MouseMsg) tea.Cmd {
	m := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		if m.Button != tea.MouseLeft {
			return nil
		}
		now := time.Now()
		double := a.lastClick.x == m.X && a.lastClick.y == m.Y && now.Sub(a.lastClick.at) < doubleClickWindow
		a.lastClick = clickRecord{m.X, m.Y, now}
		if double {
			a.lastClick.at = time.Time{} // a third click starts over
		}
		for i := len(a.zones) - 1; i >= 0; i-- {
			z := a.zones[i]
			if z.click != nil && m.X >= z.x0 && m.X < z.x1 && m.Y >= z.y0 && m.Y < z.y1 {
				return z.click(m.X-z.x0, m.Y-z.y0, double)
			}
		}
	case tea.MouseWheelMsg:
		delta := 0
		switch m.Button {
		case tea.MouseWheelUp:
			delta = -3
		case tea.MouseWheelDown:
			delta = 3
		default:
			return nil
		}
		for i := len(a.zones) - 1; i >= 0; i-- {
			z := a.zones[i]
			if z.wheel != nil && m.X >= z.x0 && m.X < z.x1 && m.Y >= z.y0 && m.Y < z.y1 {
				return z.wheel(delta)
			}
		}
	}
	return nil
}

// keyFromString builds a key press from its String() form.
func keyFromString(s string) tea.KeyPressMsg {
	var mod tea.KeyMod
	for {
		switch {
		case strings.HasPrefix(s, "ctrl+") && len(s) > 5:
			mod |= tea.ModCtrl
			s = s[5:]
			continue
		case strings.HasPrefix(s, "shift+") && len(s) > 6:
			mod |= tea.ModShift
			s = s[6:]
			continue
		}
		break
	}
	named := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "space": tea.KeySpace,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
	}
	if code, ok := named[s]; ok {
		return tea.KeyPressMsg{Code: code, Mod: mod}
	}
	r, _ := utf8.DecodeRuneInString(s)
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	return tea.KeyPressMsg{Code: r, Text: s}
}

// hintKey maps a footer hint's key label to a key, or "" if it isn't a
// single action.
func hintKey(label string) string {
	first := label
	if i := strings.Index(label, "/"); i > 0 && label != "/" {
		first = label[:i]
	}
	switch first {
	case "⏎":
		return "enter"
	case "ctrl+↓":
		return "ctrl+down"
	case "↓":
		return "down"
	case "↑":
		return "up"
	case "[ ]":
		return "]"
	case "j", "p", "J", "←→", "↑↓", "1-5", "space":
		return ""
	}
	return first
}

// listZones makes a list pane clickable: click selects, double-click
// activates, the wheel moves the selection.
func (a *App) listZones(l *listPane, x0, y0, w, h int, activate func() tea.Cmd) {
	a.addZone(zone{x0: x0, y0: y0, x1: x0 + w, y1: y0 + h,
		click: func(x, y int, double bool) tea.Cmd {
			if a.picker == nil {
				a.blurForm()
				a.focus = focusList
			}
			switch r := l.rowAt(y); {
			case r == rowFilter:
				return l.startFilter()
			case r >= 0:
				l.cursor = r
				l.touched = true
				if double {
					return activate()
				}
			}
			return nil
		},
		wheel: func(d int) tea.Cmd {
			if d < 0 {
				l.move(-1)
			} else {
				l.move(1)
			}
			return nil
		},
	})
}

// activateSelection does what enter does on the focused list.
func (a *App) activateSelection() tea.Cmd {
	return a.listKey(keyFromString("enter"))
}
