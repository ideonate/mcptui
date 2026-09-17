package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type theme struct {
	dark bool

	accent, dim, faint, text, ok, warn, bad, info, sel, border color.Color

	title, dimText, faintText, bold, okText, warnText, badText, infoText, accentText lipgloss.Style
	selected, tabActive, tabInactive, sepStyle, keyHint, badge                       lipgloss.Style
	jsonKey, jsonString, jsonNumber, jsonBool, jsonPunct                             lipgloss.Style
}

func newTheme(dark bool) *theme {
	ld := lipgloss.LightDark(dark)
	t := &theme{
		dark:   dark,
		accent: ld(lipgloss.Color("#5A4FCF"), lipgloss.Color("#9D8CFF")),
		dim:    ld(lipgloss.Color("#666666"), lipgloss.Color("#9A9A9A")),
		faint:  ld(lipgloss.Color("#999999"), lipgloss.Color("#5F5F5F")),
		text:   ld(lipgloss.Color("#1A1A1A"), lipgloss.Color("#E6E6E6")),
		ok:     ld(lipgloss.Color("#1E8E3E"), lipgloss.Color("#5FD787")),
		warn:   ld(lipgloss.Color("#B26A00"), lipgloss.Color("#FFB454")),
		bad:    ld(lipgloss.Color("#C62828"), lipgloss.Color("#FF6B6B")),
		info:   ld(lipgloss.Color("#0B6BCB"), lipgloss.Color("#6CB6FF")),
		sel:    ld(lipgloss.Color("#E4E0FF"), lipgloss.Color("#3A3470")),
		border: ld(lipgloss.Color("#CCCCCC"), lipgloss.Color("#444444")),
	}
	s := lipgloss.NewStyle
	t.title = s().Bold(true).Foreground(t.accent)
	t.dimText = s().Foreground(t.dim)
	t.faintText = s().Foreground(t.faint)
	t.bold = s().Bold(true)
	t.okText = s().Foreground(t.ok)
	t.warnText = s().Foreground(t.warn)
	t.badText = s().Foreground(t.bad)
	t.infoText = s().Foreground(t.info)
	t.accentText = s().Foreground(t.accent)
	t.selected = s().Background(t.sel).Bold(true)
	t.tabActive = s().Bold(true).Foreground(t.accent).Underline(true)
	t.tabInactive = s().Foreground(t.dim)
	t.sepStyle = s().Foreground(t.border)
	t.keyHint = s().Foreground(t.accent).Bold(true)
	t.badge = s().Padding(0, 1)
	t.jsonKey = s().Foreground(t.info)
	t.jsonString = s().Foreground(t.ok)
	t.jsonNumber = s().Foreground(t.warn)
	t.jsonBool = s().Foreground(t.accent)
	t.jsonPunct = s().Foreground(t.dim)
	return t
}

// envBadge renders the environment badge.
func (t *theme) envBadge(env string) string {
	if env == "" {
		return ""
	}
	var bg color.Color
	switch env {
	case "prod", "production":
		bg = t.bad
	case "staging", "stage":
		bg = t.warn
	default:
		bg = t.ok
	}
	return lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color("#000000")).Bold(true).Padding(0, 1).Render(env)
}

// pill renders a small coloured label.
func (t *theme) pill(text string, c color.Color) string {
	return lipgloss.NewStyle().Foreground(c).Render("[" + text + "]")
}
