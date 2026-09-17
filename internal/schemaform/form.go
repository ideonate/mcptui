package schemaform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	styleLabel    = lipgloss.NewStyle().Bold(true)
	styleRequired = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleMarker   = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleValue    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
)

const (
	boolUnset = iota
	boolTrue
	boolFalse
)

// FieldError is a validation error attached to a field ("" for form-level).
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

type fieldState struct {
	Field

	input     textinput.Model
	area      textarea.Model
	multiline bool // strings and JSON use area when true

	boolState int
	enumIdx   int
	items     []textinput.Model
	item      int

	dirty bool
	err   string

	inW, areaW, itemW int
}

// Form is an editable set of fields.
type Form struct {
	schema   json.RawMessage
	fields   []*fieldState
	rootMode bool // single KindJSON field holding the whole arguments object
	strings  bool // built by FromStrings

	extras   map[string]json.RawMessage
	formErrs []string

	focus   int
	focused bool
	width   int

	titleLabels bool // label fields by Title instead of Name

	// lineField maps each line of the last View to its field index (-1 for
	// form-level lines), for mouse hit-testing and scrolling.
	lineField []int
}

// FieldAtLine returns the field shown on line n of the last View, or -1.
func (f *Form) FieldAtLine(n int) int {
	if n < 0 || n >= len(f.lineField) {
		return -1
	}
	return f.lineField[n]
}

// FieldLine returns the first line of field i in the last View, or -1.
func (f *Form) FieldLine(i int) int {
	for n, fi := range f.lineField {
		if fi == i {
			return n
		}
	}
	return -1
}

// ClickField focuses field i as a mouse click would: a click on an already
// focused boolean or enum toggles/cycles it.
func (f *Form) ClickField(i int) tea.Cmd {
	if i < 0 || i >= len(f.fields) {
		return nil
	}
	if f.focused && f.focus == i {
		switch f.fields[i].Kind {
		case KindBoolean:
			return f.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		case KindEnum:
			return f.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		}
		return nil
	}
	cmd := f.Focus()
	return tea.Batch(cmd, f.FocusField(i))
}

// AtLastField / AtFirstField report the focus position, so a host can move
// focus out of the form with tab / shift+tab.
func (f *Form) AtLastField() bool  { return len(f.fields) == 0 || f.focus >= len(f.fields)-1 }
func (f *Form) AtFirstField() bool { return f.focus <= 0 }

// UseTitleLabels labels fields with their schema title (falling back to the
// name). Tool forms keep the default, the argument name, since that is what
// gets sent.
func (f *Form) UseTitleLabels() { f.titleLabels = true }

func (f *Form) label(fs *fieldState) string {
	if f.titleLabels && fs.Title != "" {
		return fs.Title
	}
	return fs.Name
}

// StringField describes a plain string argument.
type StringField struct {
	Name, Title, Description string
	Required                 bool
}

// Parse builds a form from an object JSON Schema.
func Parse(schema json.RawMessage) (*Form, error) {
	f := &Form{schema: append(json.RawMessage(nil), schema...), width: 80}
	fields, ok, err := parseFields(schema)
	if err != nil {
		return nil, err
	}
	if !ok {
		f.rootMode = true
		fields = []Field{{
			Name:        RootFieldName,
			Kind:        KindJSON,
			Required:    true,
			Description: "Arguments as a JSON object",
			Schema:      f.schema,
		}}
	}
	for _, fd := range fields {
		f.fields = append(f.fields, newFieldState(fd))
	}
	f.SetWidth(f.width)
	return f, nil
}

// FromStrings builds a form of string fields.
func FromStrings(fields []StringField) *Form {
	f := &Form{width: 80, strings: true}
	for _, sf := range fields {
		f.fields = append(f.fields, newFieldState(Field{
			Name:        sf.Name,
			Title:       sf.Title,
			Description: sf.Description,
			Required:    sf.Required,
			Kind:        KindString,
		}))
	}
	f.SetWidth(f.width)
	return f
}

func newFieldState(fd Field) *fieldState {
	fs := &fieldState{Field: fd}
	fs.reset()
	return fs
}

func newInput(placeholder string) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = placeholder
	return in
}

func newArea() textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.DynamicHeight = true
	ta.MinHeight = 3
	ta.MaxHeight = 12
	ta.MaxContentHeight = 100000
	return ta
}

// reset returns the field to its initial state (defaults pre-filled, pristine).
func (fs *fieldState) reset() {
	fs.dirty = false
	fs.err = ""
	fs.boolState = boolUnset
	fs.enumIdx = -1
	fs.items = nil
	fs.item = 0
	wasFocused := fs.input.Focused() || fs.area.Focused()
	fs.input = newInput(fs.Kind.String())
	fs.area = newArea()
	fs.applyWidths()
	fs.multiline = fs.Kind == KindJSON

	var def any
	if len(fs.Default) > 0 {
		def, _ = decodeAny(fs.Default)
	}
	switch fs.Kind {
	case KindString:
		if s, ok := def.(string); ok {
			fs.setText(s)
		}
	case KindInteger, KindNumber:
		if n, ok := def.(json.Number); ok {
			fs.input.SetValue(n.String())
		}
	case KindBoolean:
		if b, ok := def.(bool); ok {
			fs.boolState = boolFalse
			if b {
				fs.boolState = boolTrue
			}
		}
	case KindEnum:
		if len(fs.Default) > 0 {
			fs.enumIdx = fs.enumIndexOf(fs.Default)
		}
	case KindScalarArray:
		if arr, ok := def.([]any); ok {
			for _, v := range arr {
				in := newInput(fs.ItemKind.String())
				in.SetValue(scalarText(v))
				if fs.itemW > 0 {
					in.SetWidth(fs.itemW)
				}
				fs.items = append(fs.items, in)
			}
		}
	case KindJSON:
		if len(fs.Default) > 0 {
			fs.area.SetValue(indentJSON(fs.Default))
		} else {
			fs.area.SetValue(marshalIndent(Skeleton(fs.Schema)))
		}
	}
	if wasFocused {
		fs.focusWidget(false)
	}
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func marshalIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func indentJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func canonical(raw []byte) string {
	v, err := decodeAny(raw)
	if err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (fs *fieldState) enumIndexOf(raw []byte) int {
	want := canonical(raw)
	for i, e := range fs.Enum {
		b, _ := json.Marshal(e)
		if string(b) == want {
			return i
		}
	}
	return -1
}

func (fs *fieldState) setText(s string) {
	if strings.Contains(s, "\n") {
		fs.multiline = true
	}
	if fs.multiline {
		fs.area.SetValue(s)
	} else {
		fs.input.SetValue(s)
	}
}

func (fs *fieldState) text() string {
	if fs.multiline {
		return fs.area.Value()
	}
	return fs.input.Value()
}

func (fs *fieldState) isText() bool {
	switch fs.Kind {
	case KindString, KindInteger, KindNumber, KindJSON, KindScalarArray:
		return true
	}
	return false
}

// focusWidget focuses the field's text widget; fromEnd selects the last
// array item.
func (fs *fieldState) focusWidget(fromEnd bool) tea.Cmd {
	switch fs.Kind {
	case KindString, KindInteger, KindNumber, KindJSON:
		if fs.multiline {
			return fs.area.Focus()
		}
		return fs.input.Focus()
	case KindScalarArray:
		if len(fs.items) == 0 {
			return nil
		}
		if fromEnd {
			fs.item = len(fs.items) - 1
		} else if fs.item >= len(fs.items) {
			fs.item = 0
		}
		return fs.items[fs.item].Focus()
	}
	return nil
}

func (fs *fieldState) blurWidget() {
	fs.input.Blur()
	fs.area.Blur()
	for i := range fs.items {
		fs.items[i].Blur()
	}
}

// Fields returns the field descriptions.
func (f *Form) Fields() []Field {
	out := make([]Field, len(f.fields))
	for i, fs := range f.fields {
		out[i] = fs.Field
	}
	return out
}

// SetWidth sets the render width.
func (f *Form) SetWidth(w int) {
	if w < 20 {
		w = 20
	}
	f.width = w
	lw := f.labelWidth()
	inW := w - 2 - lw - 1
	if inW < 10 {
		inW = 10
	}
	for _, fs := range f.fields {
		fs.inW, fs.areaW, fs.itemW = inW, w-4, w-8
		fs.applyWidths()
	}
}

func (fs *fieldState) applyWidths() {
	if fs.inW <= 0 {
		return
	}
	fs.input.SetWidth(fs.inW)
	fs.area.SetWidth(fs.areaW)
	for i := range fs.items {
		fs.items[i].SetWidth(fs.itemW)
	}
}

func (f *Form) labelWidth() int {
	lw := 0
	for _, fs := range f.fields {
		if n := lipgloss.Width(f.label(fs)) + 2; n > lw {
			lw = n
		}
	}
	if max := f.width / 3; lw > max {
		lw = max
	}
	if lw > 24 {
		lw = 24
	}
	return lw
}

// Focus gives the form keyboard focus.
func (f *Form) Focus() tea.Cmd {
	f.focused = true
	if len(f.fields) == 0 {
		return nil
	}
	if f.focus >= len(f.fields) {
		f.focus = 0
	}
	return f.fields[f.focus].focusWidget(false)
}

// Blur removes keyboard focus.
func (f *Form) Blur() {
	f.focused = false
	for _, fs := range f.fields {
		fs.blurWidget()
	}
}

// Focused reports whether the form has focus.
func (f *Form) Focused() bool { return f.focused }

// FocusedField returns the index of the focused field.
func (f *Form) FocusedField() int { return f.focus }

// FocusField moves focus to field i.
func (f *Form) FocusField(i int) tea.Cmd {
	if i < 0 || i >= len(f.fields) {
		return nil
	}
	return f.moveTo(i, false)
}

// InputActive reports whether a text widget has focus, so letters are input
// rather than shortcuts.
func (f *Form) InputActive() bool {
	if !f.focused || len(f.fields) == 0 {
		return false
	}
	return f.fields[f.focus].isText()
}

// FocusedMultiline reports whether the focused field is a multi-line
// editor, where enter inserts a newline.
func (f *Form) FocusedMultiline() bool {
	if !f.focused || len(f.fields) == 0 {
		return false
	}
	return f.fields[f.focus].multiline
}

func (f *Form) moveTo(i int, fromEnd bool) tea.Cmd {
	if len(f.fields) == 0 {
		return nil
	}
	if i < 0 {
		i = 0
	}
	if i >= len(f.fields) {
		i = len(f.fields) - 1
	}
	f.fields[f.focus].blurWidget()
	f.focus = i
	if !f.focused {
		return nil
	}
	return f.fields[i].focusWidget(fromEnd)
}

// Update handles a message while the form is focused.
func (f *Form) Update(msg tea.Msg) tea.Cmd {
	if !f.focused || len(f.fields) == 0 {
		return nil
	}
	fs := f.fields[f.focus]
	key, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		if _, ok := msg.(tea.PasteMsg); ok && fs.Kind == KindScalarArray && len(fs.items) == 0 {
			f.addItem(fs)
		}
		return f.forward(fs, msg)
	}

	switch key.String() {
	case "tab":
		if f.focus < len(f.fields)-1 {
			return f.moveTo(f.focus+1, false)
		}
		return nil
	case "shift+tab":
		if f.focus > 0 {
			return f.moveTo(f.focus-1, false)
		}
		return nil
	case "up":
		switch {
		case fs.multiline && fs.area.Line() > 0:
			return f.forward(fs, msg)
		case fs.Kind == KindScalarArray && fs.item > 0 && len(fs.items) > 0:
			fs.items[fs.item].Blur()
			fs.item--
			return fs.items[fs.item].Focus()
		case f.focus > 0:
			return f.moveTo(f.focus-1, true)
		}
		return nil
	case "down":
		switch {
		case fs.multiline && fs.area.Line() < fs.area.LineCount()-1:
			return f.forward(fs, msg)
		case fs.Kind == KindScalarArray && fs.item < len(fs.items)-1:
			fs.items[fs.item].Blur()
			fs.item++
			return fs.items[fs.item].Focus()
		case f.focus < len(f.fields)-1:
			return f.moveTo(f.focus+1, false)
		}
		return nil
	case "ctrl+t":
		if fs.Kind == KindString {
			v := fs.text()
			fs.blurWidget()
			fs.multiline = !fs.multiline
			if fs.multiline {
				fs.area.SetValue(v)
			} else {
				fs.input.SetValue(strings.ReplaceAll(v, "\n", " "))
			}
			return fs.focusWidget(false)
		}
	case "ctrl+n":
		if fs.Kind == KindScalarArray {
			return f.addItem(fs)
		}
	case "ctrl+d":
		if fs.Kind == KindScalarArray {
			if len(fs.items) > 0 {
				fs.items = append(fs.items[:fs.item], fs.items[fs.item+1:]...)
				fs.dirty = true
				if fs.item >= len(fs.items) && fs.item > 0 {
					fs.item--
				}
				return fs.focusWidget(false)
			}
			return nil
		}
	}

	switch fs.Kind {
	case KindBoolean:
		switch key.String() {
		case "space", " ", "enter", "left", "right", "t", "f", "y", "n":
			switch key.String() {
			case "t", "y":
				fs.boolState = boolTrue
			case "f", "n":
				fs.boolState = boolFalse
			default:
				fs.boolState = nextBool(fs.boolState, fs.Required)
			}
			fs.dirty = true
		case "backspace", "delete":
			if !fs.Required {
				fs.boolState = boolUnset
				fs.dirty = true
			}
		}
		return nil
	case KindEnum:
		n := len(fs.Enum)
		switch key.String() {
		case "right", "space", " ":
			fs.enumIdx++
			if fs.enumIdx >= n {
				fs.enumIdx = -1
				if fs.Required {
					fs.enumIdx = 0
				}
			}
			fs.dirty = true
		case "left":
			fs.enumIdx--
			if fs.enumIdx < -1 || (fs.enumIdx == -1 && fs.Required) {
				fs.enumIdx = n - 1
			}
			fs.dirty = true
		case "backspace", "delete":
			if !fs.Required {
				fs.enumIdx = -1
				fs.dirty = true
			}
		}
		return nil
	case KindScalarArray:
		if len(fs.items) == 0 {
			if key.Text == "" {
				return nil
			}
			cmd := f.addItem(fs)
			return tea.Batch(cmd, f.forward(fs, msg))
		}
	}
	return f.forward(fs, msg)
}

func nextBool(s int, required bool) int {
	switch s {
	case boolUnset:
		return boolTrue
	case boolTrue:
		return boolFalse
	default:
		if required {
			return boolTrue
		}
		return boolUnset
	}
}

func (f *Form) addItem(fs *fieldState) tea.Cmd {
	in := newInput(fs.ItemKind.String())
	in.SetWidth(fs.itemW)
	pos := 0
	if len(fs.items) > 0 {
		fs.items[fs.item].Blur()
		pos = fs.item + 1
	}
	fs.items = append(fs.items, textinput.Model{})
	copy(fs.items[pos+1:], fs.items[pos:])
	fs.items[pos] = in
	fs.item = pos
	fs.dirty = true
	return fs.items[pos].Focus()
}

// forward routes msg to the focused text widget, tracking edits.
func (f *Form) forward(fs *fieldState, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch fs.Kind {
	case KindString, KindInteger, KindNumber, KindJSON:
		before := fs.text()
		if fs.multiline {
			fs.area, cmd = fs.area.Update(msg)
		} else {
			fs.input, cmd = fs.input.Update(msg)
		}
		if fs.text() != before {
			fs.dirty = true
			fs.err = ""
		}
	case KindScalarArray:
		if len(fs.items) == 0 {
			return nil
		}
		before := fs.items[fs.item].Value()
		fs.items[fs.item], cmd = fs.items[fs.item].Update(msg)
		if fs.items[fs.item].Value() != before {
			fs.dirty = true
			fs.err = ""
		}
	}
	return cmd
}

// value returns the field's JSON value and whether it should be sent.
func (fs *fieldState) value() (json.RawMessage, bool, error) {
	send := func(has bool) bool { return has && (fs.Required || fs.dirty) }
	switch fs.Kind {
	case KindString:
		t := fs.text()
		has := t != "" || (fs.dirty && fs.Required)
		if !send(has) {
			return nil, false, nil
		}
		b, _ := json.Marshal(t)
		return b, true, nil
	case KindInteger, KindNumber:
		t := strings.TrimSpace(fs.text())
		if !send(t != "") {
			return nil, false, nil
		}
		b, err := parseNumber(t, fs.Kind)
		return b, err == nil, err
	case KindBoolean:
		if !send(fs.boolState != boolUnset) {
			return nil, false, nil
		}
		return json.RawMessage(strconv.FormatBool(fs.boolState == boolTrue)), true, nil
	case KindEnum:
		if !send(fs.enumIdx >= 0 && fs.enumIdx < len(fs.Enum)) {
			return nil, false, nil
		}
		b, _ := json.Marshal(fs.Enum[fs.enumIdx])
		return b, true, nil
	case KindScalarArray:
		var parts []json.RawMessage
		for i, it := range fs.items {
			t := it.Value()
			if fs.ItemKind != KindString {
				t = strings.TrimSpace(t)
			}
			if t == "" {
				continue
			}
			b, err := parseScalar(t, fs.ItemKind)
			if err != nil {
				return nil, false, fmt.Errorf("item %d: %w", i+1, err)
			}
			parts = append(parts, b)
		}
		if !send(len(parts) > 0 || fs.Required) {
			return nil, false, nil
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, p := range parts {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(p)
		}
		buf.WriteByte(']')
		return buf.Bytes(), true, nil
	case KindJSON:
		t := strings.TrimSpace(fs.text())
		if !send(t != "") {
			return nil, false, nil
		}
		v, err := decodeAny([]byte(t))
		if err != nil {
			return nil, false, fmt.Errorf("invalid JSON: %v", err)
		}
		b, _ := json.Marshal(v)
		return b, true, nil
	}
	return nil, false, nil
}

func parseNumber(t string, k Kind) (json.RawMessage, error) {
	if k == KindInteger {
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(t, 64)
			if ferr != nil || f != float64(int64(f)) {
				return nil, fmt.Errorf("%q is not an integer", t)
			}
			n = int64(f)
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), nil
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", t)
	}
	if json.Valid([]byte(t)) {
		return json.RawMessage(t), nil
	}
	return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64)), nil
}

func parseScalar(t string, k Kind) (json.RawMessage, error) {
	switch k {
	case KindInteger, KindNumber:
		return parseNumber(t, k)
	case KindBoolean:
		switch strings.ToLower(t) {
		case "true":
			return json.RawMessage("true"), nil
		case "false":
			return json.RawMessage("false"), nil
		}
		return nil, fmt.Errorf("%q is not true or false", t)
	}
	b, _ := json.Marshal(t)
	return b, nil
}

// Values returns the arguments object built from the form. Unset optional
// fields are omitted; keys from SetValues that no field owns are kept.
// Field parse errors are shown inline and returned joined.
func (f *Form) Values() (json.RawMessage, error) {
	f.formErrs = nil
	var errs []error
	if f.rootMode {
		fs := f.fields[0]
		fs.err = ""
		t := strings.TrimSpace(fs.text())
		if t == "" {
			return json.RawMessage("{}"), nil
		}
		v, err := decodeAny([]byte(t))
		if err != nil {
			fs.err = "invalid JSON: " + err.Error()
			return nil, &FieldError{Field: fs.Name, Message: fs.err}
		}
		b, _ := json.Marshal(v)
		return b, nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	write := func(k string, v json.RawMessage) {
		if n > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
		n++
	}
	owned := map[string]bool{}
	for _, fs := range f.fields {
		fs.err = ""
		v, ok, err := fs.value()
		if err != nil {
			fs.err = err.Error()
			errs = append(errs, &FieldError{Field: fs.Name, Message: fs.err})
			owned[fs.Name] = true
			continue
		}
		if ok {
			owned[fs.Name] = true
			write(fs.Name, v)
		}
	}
	keys := make([]string, 0, len(f.extras))
	for k := range f.extras {
		if !owned[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, f.extras[k])
	}
	buf.WriteByte('}')
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return buf.Bytes(), nil
}

// StringValues returns non-empty string values (and required ones even when
// empty), for FromStrings forms.
func (f *Form) StringValues() map[string]string {
	out := map[string]string{}
	for _, fs := range f.fields {
		t := fs.text()
		if fs.Kind != KindString {
			if v, ok, err := fs.value(); err == nil && ok {
				t = string(v)
			} else {
				continue
			}
		}
		if t != "" || fs.Required {
			out[fs.Name] = t
		}
	}
	return out
}

// SetValues populates the form from a JSON object. Fields absent from obj
// return to their initial state; keys no field can represent are kept and
// merged back by Values.
func (f *Form) SetValues(obj json.RawMessage) error {
	f.formErrs = nil
	if f.rootMode {
		fs := f.fields[0]
		t := bytes.TrimSpace(obj)
		if len(t) == 0 {
			fs.area.SetValue("")
		} else {
			if !json.Valid(t) {
				return errors.New("invalid JSON")
			}
			fs.area.SetValue(indentJSON(t))
		}
		fs.dirty = true
		fs.err = ""
		return nil
	}
	var m map[string]json.RawMessage
	if t := bytes.TrimSpace(obj); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		if err := json.Unmarshal(t, &m); err != nil {
			return fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}
	extras := map[string]json.RawMessage{}
	byName := map[string]*fieldState{}
	for _, fs := range f.fields {
		byName[fs.Name] = fs
		fs.reset()
	}
	for k, raw := range m {
		fs := byName[k]
		if fs == nil || !fs.apply(raw) {
			extras[k] = append(json.RawMessage(nil), raw...)
		}
	}
	f.extras = extras
	if f.focused && len(f.fields) > 0 {
		f.fields[f.focus].blurWidget()
		f.fields[f.focus].focusWidget(false)
	}
	return nil
}

// apply sets the widget from a JSON value; false if it can't be represented.
func (fs *fieldState) apply(raw json.RawMessage) bool {
	v, err := decodeAny(raw)
	if err != nil || v == nil {
		return false
	}
	switch fs.Kind {
	case KindString:
		s, ok := v.(string)
		if !ok {
			return false
		}
		fs.setText(s)
	case KindInteger, KindNumber:
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		if fs.Kind == KindInteger {
			if _, err := n.Int64(); err != nil {
				return false
			}
		}
		fs.input.SetValue(n.String())
	case KindBoolean:
		b, ok := v.(bool)
		if !ok {
			return false
		}
		fs.boolState = boolFalse
		if b {
			fs.boolState = boolTrue
		}
	case KindEnum:
		i := fs.enumIndexOf(raw)
		if i < 0 {
			return false
		}
		fs.enumIdx = i
	case KindScalarArray:
		arr, ok := v.([]any)
		if !ok {
			return false
		}
		var items []textinput.Model
		for _, e := range arr {
			switch e.(type) {
			case string:
				if fs.ItemKind != KindString {
					return false
				}
			case json.Number:
				if fs.ItemKind != KindInteger && fs.ItemKind != KindNumber {
					return false
				}
			case bool:
				if fs.ItemKind != KindBoolean {
					return false
				}
			default:
				return false
			}
			in := newInput(fs.ItemKind.String())
			in.SetValue(scalarText(e))
			if fs.itemW > 0 {
				in.SetWidth(fs.itemW)
			}
			items = append(items, in)
		}
		fs.items = items
		fs.item = 0
	case KindJSON:
		fs.area.SetValue(indentJSON(raw))
	default:
		return false
	}
	fs.dirty = true
	return true
}

// Validate builds the values and checks them against the schema, attaching
// errors to fields. Broken schemas are not treated as errors.
func (f *Form) Validate() []error {
	vals, err := f.Values()
	if err != nil {
		var out []error
		for _, fs := range f.fields {
			if fs.err != "" {
				out = append(out, &FieldError{Field: fs.Name, Message: fs.err})
			}
		}
		return out
	}
	if f.strings || isEmptySchema(f.schema) {
		var out []error
		for _, fs := range f.fields {
			if fs.Required && strings.TrimSpace(fs.text()) == "" {
				fs.err = "required"
				out = append(out, &FieldError{Field: fs.Name, Message: fs.err})
			}
		}
		return out
	}
	verr := ValidateValue(f.schema, vals)
	var ve *ValidationError
	if !errors.As(verr, &ve) {
		return nil
	}
	byName := map[string]*fieldState{}
	for _, fs := range f.fields {
		byName[fs.Name] = fs
	}
	var out []error
	addField := func(fs *fieldState, msg string) {
		if fs.err == "" {
			fs.err = msg
		} else if !strings.Contains(fs.err, msg) {
			fs.err += "; " + msg
		}
		out = append(out, &FieldError{Field: fs.Name, Message: msg})
	}
	for _, se := range ve.Errors {
		if f.rootMode {
			msg := se.Message
			if se.Path != "" {
				msg = se.Path + ": " + msg
			}
			addField(f.fields[0], msg)
			continue
		}
		if se.Path == "" && len(se.Missing) > 0 {
			for _, name := range se.Missing {
				if fs := byName[name]; fs != nil {
					addField(fs, "required")
				} else {
					f.formErrs = append(f.formErrs, "missing property "+name)
					out = append(out, &FieldError{Message: "missing property " + name})
				}
			}
			continue
		}
		name, rest := splitPointer(se.Path)
		if fs := byName[name]; fs != nil && name != "" {
			msg := se.Message
			if rest != "" {
				msg = rest + ": " + msg
			}
			addField(fs, msg)
			continue
		}
		msg := se.Message
		if se.Path != "" {
			msg = se.Path + ": " + msg
		}
		f.formErrs = append(f.formErrs, msg)
		out = append(out, &FieldError{Message: msg})
	}
	return out
}

func splitPointer(p string) (first, rest string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	first, rest, _ = strings.Cut(p, "/")
	first = strings.ReplaceAll(strings.ReplaceAll(first, "~1", "/"), "~0", "~")
	if rest != "" {
		rest = "/" + rest
	}
	return first, rest
}

// View renders the form.
func (f *Form) View() string {
	if len(f.fields) == 0 {
		return styleDim.Render("(no arguments)")
	}
	lw := f.labelWidth()
	var b strings.Builder
	f.lineField = f.lineField[:0]
	mark := func(i int) {
		for n := strings.Count(b.String(), "\n"); len(f.lineField) < n; {
			f.lineField = append(f.lineField, i)
		}
	}
	for i, fs := range f.fields {
		mark(i - 1)
		focused := f.focused && i == f.focus
		marker := "  "
		if focused {
			marker = styleMarker.Render("▸ ")
		}
		label := f.label(fs)
		if max := lw - 2; lipgloss.Width(label) > max && max > 1 {
			label = string([]rune(label)[:max-1]) + "…"
		}
		labelR := styleLabel.Render(label)
		if fs.Required {
			labelR += styleRequired.Render(" *")
		}
		pad := lw - lipgloss.Width(label)
		if fs.Required {
			pad -= 2
		}
		if pad < 1 {
			pad = 1
		}
		head := marker + labelR + strings.Repeat(" ", pad)

		switch {
		case fs.Kind == KindScalarArray:
			hint := ""
			if focused {
				hint = styleDim.Render("ctrl+n add · ctrl+d remove")
			}
			b.WriteString(head + hint + "\n")
			if len(fs.items) == 0 {
				b.WriteString("    " + styleDim.Render("(no items)") + "\n")
			}
			for j := range fs.items {
				bullet := "  - "
				if focused && j == fs.item {
					bullet = "  " + styleMarker.Render("› ")
				}
				b.WriteString("  " + bullet + fs.items[j].View() + "\n")
			}
		case fs.multiline:
			hint := ""
			if focused && fs.Kind == KindString {
				hint = styleDim.Render("ctrl+t single-line")
			} else if focused && fs.Kind == KindJSON {
				hint = styleDim.Render("JSON")
			}
			b.WriteString(head + hint + "\n")
			for _, line := range strings.Split(fs.area.View(), "\n") {
				b.WriteString("  " + line + "\n")
			}
		default:
			b.WriteString(head + f.inlineWidget(fs, focused) + "\n")
		}
		d := firstLine(fs.Description)
		if !fs.dirty && !fs.Required && len(fs.Default) > 0 && (fs.Kind == KindString || fs.Kind == KindInteger || fs.Kind == KindNumber || fs.Kind == KindScalarArray || fs.Kind == KindJSON) {
			if d != "" {
				d += " "
			}
			d += "(default, not sent unless edited)"
		}
		if d != "" {
			max := f.width - 4
			if lipgloss.Width(d) > max && max > 1 {
				d = string([]rune(d)[:max-1]) + "…"
			}
			b.WriteString("    " + styleDim.Render(d) + "\n")
		}
		if fs.err != "" {
			b.WriteString("    " + styleError.Render("✗ "+fs.err) + "\n")
		}
	}
	mark(len(f.fields) - 1)
	for _, e := range f.formErrs {
		b.WriteString(styleError.Render("✗ "+e) + "\n")
	}
	mark(-1)
	return strings.TrimRight(b.String(), "\n")
}

func (f *Form) inlineWidget(fs *fieldState, focused bool) string {
	pristineDefault := !fs.dirty && !fs.Required && len(fs.Default) > 0
	suffix := ""
	if pristineDefault {
		suffix = styleDim.Render(" (default)")
	}
	switch fs.Kind {
	case KindBoolean:
		var s string
		switch fs.boolState {
		case boolTrue:
			s = "[x] true"
		case boolFalse:
			s = "[ ] false"
		default:
			s = styleDim.Render("[-] unset")
		}
		if focused {
			suffix += styleDim.Render("  space toggle")
		}
		return styleValue.Render(s) + suffix
	case KindEnum:
		s := styleDim.Render("(unset)")
		if fs.enumIdx >= 0 && fs.enumIdx < len(fs.Enum) {
			s = styleValue.Render(scalarText(fs.Enum[fs.enumIdx]))
		}
		if focused {
			pos := fmt.Sprintf("  %d/%d ←/→", fs.enumIdx+1, len(fs.Enum))
			return "‹ " + s + " ›" + suffix + styleDim.Render(pos)
		}
		return "‹ " + s + " ›" + suffix
	default:
		// The input pads to its width, so the default marker goes on the
		// description line instead.
		return fs.input.View()
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
