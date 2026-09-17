package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type listItem struct {
	key    string // stable identity
	title  string
	desc   string // filter text and optional dim suffix
	marker string // styled prefix (badges)
	group  string // section header when it changes
}

// listPane is a filterable, scrollable list.
type listPane struct {
	th        *theme
	items     []listItem
	visible   []int // indexes into items
	cursor    int   // index into visible
	offset    int
	filter    textinput.Model
	filtering bool
	empty     string
	touched   bool // the user moved the cursor; keep the selection on reload
	// rows maps each line of the last view to a visible index, or rowFilter /
	// rowOther.
	rows []int
}

const (
	rowFilter = -2
	rowOther  = -1
)

func newListPane(th *theme, empty string) *listPane {
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "filter"
	return &listPane{th: th, filter: ti, empty: empty}
}

func (l *listPane) setItems(items []listItem) {
	var key string
	if it := l.selected(); it != nil && l.touched {
		key = it.key
	}
	l.items = items
	l.applyFilter()
	if !l.selectKey(key) && !l.touched {
		l.cursor, l.offset = 0, 0
	}
}

func (l *listPane) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(l.filter.Value()))
	l.visible = l.visible[:0]
	for i, it := range l.items {
		if q == "" || strings.Contains(strings.ToLower(it.title), q) || strings.Contains(strings.ToLower(it.desc), q) {
			l.visible = append(l.visible, i)
		}
	}
	if l.cursor >= len(l.visible) {
		l.cursor = max(0, len(l.visible)-1)
	}
}

func (l *listPane) selectKey(key string) bool {
	for vi, i := range l.visible {
		if l.items[i].key == key {
			l.cursor = vi
			return true
		}
	}
	return false
}

func (l *listPane) selected() *listItem {
	if l.cursor < 0 || l.cursor >= len(l.visible) {
		return nil
	}
	return &l.items[l.visible[l.cursor]]
}

func (l *listPane) move(d int) {
	if len(l.visible) == 0 {
		return
	}
	l.touched = true
	l.cursor = min(max(l.cursor+d, 0), len(l.visible)-1)
}

func (l *listPane) startFilter() tea.Cmd {
	l.filtering = true
	return l.filter.Focus()
}

// updateFilter handles keys while filtering. done reports the filter closed.
func (l *listPane) updateFilter(msg tea.Msg) (tea.Cmd, bool) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			l.filter.SetValue("")
			l.filter.Blur()
			l.filtering = false
			l.applyFilter()
			return nil, true
		case "enter", "down", "up", "tab":
			l.filter.Blur()
			l.filtering = false
			return nil, true
		}
	}
	var cmd tea.Cmd
	l.filter, cmd = l.filter.Update(msg)
	l.applyFilter()
	return cmd, false
}

func (l *listPane) view(width, height int, focused bool) string {
	th := l.th
	var lines []string
	if l.filtering || l.filter.Value() != "" {
		l.filter.SetWidth(max(width-3, 1))
		lines = append(lines, truncate(l.filter.View(), width))
	} else {
		lines = append(lines, th.faintText.Render(truncate("/ filter", width)))
	}
	rows := height - 1
	l.rows = append(l.rows[:0], rowFilter)
	if len(l.visible) == 0 {
		msg := l.empty
		if len(l.items) > 0 {
			msg = "no matches"
		}
		lines = append(lines, th.dimText.Render(truncate("  "+msg, width)))
		return fitLines(strings.Join(lines, "\n"), width, height)
	}

	// Build display rows including group headers.
	type row struct {
		text string
		vi   int // -1 for headers
	}
	var all []row
	lastGroup := ""
	cursorRow := 0
	for vi, i := range l.visible {
		it := l.items[i]
		if it.group != "" && it.group != lastGroup {
			all = append(all, row{text: th.faintText.Render(truncate(it.group, width)), vi: -1})
			lastGroup = it.group
		}
		if vi == l.cursor {
			cursorRow = len(all)
		}
		all = append(all, row{vi: vi})
	}
	if cursorRow < l.offset {
		l.offset = cursorRow
		if l.offset > 0 && all[l.offset-1].vi == -1 {
			l.offset--
		}
	}
	if cursorRow >= l.offset+rows {
		l.offset = cursorRow - rows + 1
	}
	if l.offset > max(0, len(all)-rows) {
		l.offset = max(0, len(all)-rows)
	}
	for r := l.offset; r < len(all) && r < l.offset+rows; r++ {
		rw := all[r]
		l.rows = append(l.rows, rw.vi)
		if rw.vi < 0 {
			lines = append(lines, rw.text)
			continue
		}
		it := l.items[l.visible[rw.vi]]
		prefix := "  "
		if rw.vi == l.cursor {
			prefix = "▸ "
		}
		text := prefix + it.title
		if it.marker != "" {
			text += " " + it.marker
		}
		text = padRight(text, width)
		if rw.vi == l.cursor {
			if focused {
				text = th.selected.Render(text)
			} else {
				text = th.bold.Render(text)
			}
		}
		lines = append(lines, text)
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

// rowAt returns what line y of the last view shows.
func (l *listPane) rowAt(y int) int {
	if y < 0 || y >= len(l.rows) {
		return rowOther
	}
	return l.rows[y]
}
