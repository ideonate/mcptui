package tui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/schemaform"
)

type viewMode int

const (
	modePretty viewMode = iota
	modeMarkdown
	modeRaw
)

func (v viewMode) String() string {
	switch v {
	case modeMarkdown:
		return "markdown"
	case modeRaw:
		return "raw"
	}
	return "pretty"
}

// renderer caches glamour renderers per width.
type renderer struct {
	th *theme
	mu sync.Mutex
	md map[int]*glamour.TermRenderer
}

func newRenderer(th *theme) *renderer { return &renderer{th: th, md: map[int]*glamour.TermRenderer{}} }

// markdown renders md at width, falling back to wrapped text.
func (r *renderer) markdown(md string, width int) string {
	if width < 20 {
		width = 20
	}
	r.mu.Lock()
	tr := r.md[width]
	if tr == nil {
		style := "dark"
		if !r.th.dark {
			style = "light"
		}
		var err error
		tr, err = glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
		if err == nil {
			r.md[width] = tr
		}
	}
	r.mu.Unlock()
	if tr == nil {
		return wrap(md, width)
	}
	out, err := tr.Render(md)
	if err != nil {
		return wrap(md, width)
	}
	return strings.Trim(out, "\n")
}

func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, " ,-/")
}

// prettyJSON indents raw JSON; ok is false if it isn't JSON.
func prettyJSON(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[' && raw[0] != '"') {
		// Scalars are valid JSON too, but showing them highlighted adds nothing.
		if !json.Valid(raw) || len(raw) == 0 {
			return "", false
		}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "", false
	}
	return buf.String(), true
}

// colorJSON highlights indented JSON.
func (r *renderer) colorJSON(s string) string {
	th := r.th
	var out strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			tok := s[i : j+1]
			k := j + 1
			for k < len(s) && (s[k] == ' ') {
				k++
			}
			if k < len(s) && s[k] == ':' {
				out.WriteString(th.jsonKey.Render(tok))
			} else {
				out.WriteString(th.jsonString.Render(tok))
			}
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(s) && strings.IndexByte("-+.eE0123456789", s[j]) >= 0 {
				j++
			}
			out.WriteString(th.jsonNumber.Render(s[i:j]))
			i = j
		case strings.HasPrefix(s[i:], "true"), strings.HasPrefix(s[i:], "null"):
			out.WriteString(th.jsonBool.Render(s[i : i+4]))
			i += 4
		case strings.HasPrefix(s[i:], "false"):
			out.WriteString(th.jsonBool.Render(s[i : i+5]))
			i += 5
		case strings.IndexByte("{}[],:", c) >= 0:
			out.WriteString(th.jsonPunct.Render(string(c)))
			i++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			out.WriteString(s[i : i+size])
			i += size
		}
	}
	return out.String()
}

// jsonBlock renders JSON pretty and coloured, or the text if not JSON.
func (r *renderer) jsonBlock(raw []byte, width int) string {
	if p, ok := prettyJSON(raw); ok {
		return wrap(r.colorJSON(p), width)
	}
	return wrap(string(raw), width)
}

// textBlock renders a text content block by mode.
func (r *renderer) textBlock(text, mime string, mode viewMode, width int) string {
	if mode == modeMarkdown {
		return r.markdown(text, width)
	}
	if p, ok := prettyJSON([]byte(text)); ok && (strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "[")) {
		return wrap(r.colorJSON(p), width)
	}
	if strings.Contains(mime, "markdown") {
		return r.markdown(text, width)
	}
	return wrap(text, width)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func base64Len(s string) int {
	return base64.StdEncoding.DecodedLen(len(strings.TrimRight(s, "=")))
}

// sectionTitle renders a dim rule with a title.
func (r *renderer) section(title string, width int) string {
	line := "── " + title + " "
	if w := width - lipgloss.Width(line); w > 0 {
		line += strings.Repeat("─", w)
	}
	return r.th.faintText.Render(line)
}

// renderContents renders a list of content blocks.
func (r *renderer) renderContents(blocks []mcp.Content, mode viewMode, width int) string {
	var parts []string
	for i, c := range blocks {
		if len(blocks) > 1 {
			parts = append(parts, r.section(fmt.Sprintf("content[%d] %s", i, c.Type), width))
		}
		parts = append(parts, r.renderContent(c, mode, width))
	}
	return strings.Join(parts, "\n")
}

func (r *renderer) renderContent(c mcp.Content, mode viewMode, width int) string {
	th := r.th
	switch c.Type {
	case "text":
		return r.textBlock(c.Text, "", mode, width)
	case "image", "audio":
		return th.infoText.Render(fmt.Sprintf("◆ %s %s, %s", c.Type, c.MimeType, humanBytes(base64Len(c.Data)))) +
			th.dimText.Render("  (s to save)")
	case "resource_link":
		var b strings.Builder
		b.WriteString(th.infoText.Render("↗ " + c.URI))
		if c.Name != "" || c.Title != "" {
			b.WriteString("  " + th.bold.Render(firstNonEmpty(c.Title, c.Name)))
		}
		if c.MimeType != "" {
			b.WriteString(th.dimText.Render("  " + c.MimeType))
		}
		if c.Description != "" {
			b.WriteString("\n" + wrap(c.Description, width))
		}
		return b.String()
	case "resource":
		if c.Resource == nil {
			break
		}
		return r.renderResourceContents(*c.Resource, mode, width)
	}
	b, _ := json.Marshal(c)
	return r.jsonBlock(b, width)
}

func (r *renderer) renderResourceContents(rc mcp.ResourceContents, mode viewMode, width int) string {
	th := r.th
	head := th.infoText.Render("▤ "+rc.URI) + th.dimText.Render("  "+rc.MimeType)
	switch {
	case rc.Text != nil:
		body := *rc.Text
		m := mode
		if mode == modePretty && strings.Contains(rc.MimeType, "markdown") {
			m = modeMarkdown
		}
		if strings.Contains(rc.MimeType, "json") {
			return head + "\n" + r.jsonBlock([]byte(body), width)
		}
		if m == modeMarkdown {
			return head + "\n" + r.markdown(body, width)
		}
		return head + "\n" + wrap(body, width)
	case rc.Blob != nil:
		return head + "\n" + th.infoText.Render(fmt.Sprintf("binary, %s", humanBytes(base64Len(*rc.Blob)))) + th.dimText.Render("  (s to save)")
	}
	return head
}

// renderToolResult renders a tools/call result.
func (r *renderer) renderToolResult(res *mcp.CallToolResult, tool *mcp.Tool, mode viewMode, width int) string {
	th := r.th
	var parts []string
	if res.IsError {
		parts = append(parts, th.badText.Bold(true).Render("✗ tool returned isError"))
	}
	if len(res.Content) > 0 {
		parts = append(parts, r.renderContents(res.Content, mode, width))
	} else if len(res.StructuredContent) == 0 {
		parts = append(parts, th.dimText.Render("(no content)"))
	}
	if len(res.StructuredContent) > 0 {
		title := "structuredContent"
		if tool != nil && len(tool.OutputSchema) > 0 {
			if err := schemaform.ValidateValue(tool.OutputSchema, res.StructuredContent); err != nil {
				title += " " + th.badText.Render("✗ does not match outputSchema")
				parts = append(parts, r.section(title, width), r.jsonBlock(res.StructuredContent, width), th.badText.Render(wrap(err.Error(), width)))
				return strings.Join(parts, "\n")
			}
			title += " " + th.okText.Render("✓ matches outputSchema")
		}
		parts = append(parts, r.section(title, width), r.jsonBlock(res.StructuredContent, width))
	}
	return strings.Join(parts, "\n")
}

// renderPromptResult renders prompts/get as a transcript.
func (r *renderer) renderPromptResult(res *mcp.GetPromptResult, mode viewMode, width int) string {
	th := r.th
	var parts []string
	if res.Description != "" {
		parts = append(parts, th.dimText.Render(wrap(res.Description, width)))
	}
	for _, m := range res.Messages {
		role := th.bold.Foreground(th.accent).Render(strings.ToUpper(m.Role))
		if m.Role == "assistant" {
			role = th.bold.Foreground(th.ok).Render(strings.ToUpper(m.Role))
		}
		parts = append(parts, "", role, r.renderContent(m.Content, mode, width))
	}
	return strings.Join(parts, "\n")
}

// renderReadResult renders resources/read.
func (r *renderer) renderReadResult(res *mcp.ReadResourceResult, mode viewMode, width int) string {
	var parts []string
	for _, rc := range res.Contents {
		parts = append(parts, r.renderResourceContents(rc, mode, width))
	}
	if len(parts) == 0 {
		return r.th.dimText.Render("(no contents)")
	}
	return strings.Join(parts, "\n\n")
}

// renderRaw shows the exchange's request and response.
func (r *renderer) renderRaw(ex *mcp.Exchange, width int) string {
	if ex == nil {
		return ""
	}
	parts := []string{r.section("request →", width), r.jsonBlock(ex.Request, width)}
	if len(ex.Response) > 0 {
		parts = append(parts, r.section("← response", width), r.jsonBlock(ex.Response, width))
	} else if ex.Err != nil {
		parts = append(parts, r.section("← error", width), r.th.badText.Render(wrap(ex.Err.Error(), width)))
	}
	return strings.Join(parts, "\n")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// truncate cuts a single-line string to width cells.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

// oneLine collapses whitespace.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// padRight pads a styled string to width cells.
func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return truncate(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}

// fitLines pads/truncates a block to exactly h lines of width w.
func fitLines(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = padRight(l, w)
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}
