package schemaform

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// jsonEqual compares two JSON documents semantically.
func jsonEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func mustParse(t *testing.T, schema string) *Form {
	t.Helper()
	f, err := Parse(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParseKinds(t *testing.T) {
	f := mustParse(t, `{
		"type": "object",
		"$defs": {"Color": {"type": "string", "enum": ["red", "green"]}},
		"properties": {
			"s": {"type": "string", "description": "a string"},
			"i": {"type": "integer"},
			"n": {"type": "number"},
			"b": {"type": "boolean"},
			"e": {"enum": ["a", "b", 3]},
			"arr": {"type": "array", "items": {"type": "integer"}},
			"opt": {"anyOf": [{"type": "string"}, {"type": "null"}]},
			"opt2": {"type": ["integer", "null"]},
			"ref": {"$ref": "#/$defs/Color"},
			"obj": {"type": "object", "properties": {"x": {"type": "integer"}}, "required": ["x"]},
			"union": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
			"objs": {"type": "array", "items": {"type": "object"}}
		},
		"required": ["s", "e"]
	}`)
	want := []struct {
		name     string
		kind     Kind
		item     Kind
		required bool
		nullable bool
	}{
		{"s", KindString, 0, true, false},
		{"i", KindInteger, 0, false, false},
		{"n", KindNumber, 0, false, false},
		{"b", KindBoolean, 0, false, false},
		{"e", KindEnum, 0, true, false},
		{"arr", KindScalarArray, KindInteger, false, false},
		{"opt", KindString, 0, false, true},
		{"opt2", KindInteger, 0, false, true},
		{"ref", KindEnum, 0, false, false},
		{"obj", KindJSON, 0, false, false},
		{"union", KindJSON, 0, false, false},
		{"objs", KindJSON, 0, false, false},
	}
	fields := f.Fields()
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		fd := fields[i]
		if fd.Name != w.name || fd.Kind != w.kind || fd.Required != w.required || fd.Nullable != w.nullable {
			t.Errorf("field %d = %+v, want %+v", i, fd, w)
		}
		if w.kind == KindScalarArray && fd.ItemKind != w.item {
			t.Errorf("field %s item kind = %v, want %v", fd.Name, fd.ItemKind, w.item)
		}
	}
	if fields[0].Description != "a string" {
		t.Errorf("description not parsed: %q", fields[0].Description)
	}
}

func TestParseEmptyAndRootFallback(t *testing.T) {
	for _, s := range []string{``, `null`, `{}`, `{"type":"object"}`, `{"type":"object","properties":{}}`} {
		f := mustParse(t, s)
		if len(f.Fields()) != 0 {
			t.Errorf("%q: expected no fields, got %d", s, len(f.Fields()))
		}
		v, err := f.Values()
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, v, `{}`)
	}
	f := mustParse(t, `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object"}]}`)
	fs := f.Fields()
	if len(fs) != 1 || fs[0].Name != RootFieldName || fs[0].Kind != KindJSON {
		t.Fatalf("expected root JSON field, got %+v", fs)
	}
	if err := f.SetValues(json.RawMessage(`{"a":"x","z":[1]}`)); err != nil {
		t.Fatal(err)
	}
	v, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, v, `{"a":"x","z":[1]}`)
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		in     string // SetValues input ("" = none)
		want   string
	}{
		{
			name:   "unset optional omitted",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"boolean"},"d":{"type":"array","items":{"type":"string"}},"e":{"enum":["x","y"]},"f":{"type":"object"}}}`,
			want:   `{}`,
		},
		{
			name:   "defaults omitted when optional and pristine",
			schema: `{"type":"object","properties":{"a":{"type":"string","default":"hi"},"n":{"type":"number","default":1.5},"b":{"type":"boolean","default":true}}}`,
			want:   `{}`,
		},
		{
			name:   "defaults sent when required",
			schema: `{"type":"object","properties":{"a":{"type":"string","default":"hi"},"n":{"type":"integer","default":7},"e":{"enum":["x","y"],"default":"y"},"l":{"type":"array","items":{"type":"number"},"default":[1,2.5]}},"required":["a","n","e","l"]}`,
			want:   `{"a":"hi","n":7,"e":"y","l":[1,2.5]}`,
		},
		{
			name:   "all scalar types",
			schema: `{"type":"object","properties":{"s":{"type":"string"},"i":{"type":"integer"},"n":{"type":"number"},"b":{"type":"boolean"},"e":{"enum":["a",2,true]}}}`,
			in:     `{"s":"line1\nline2","i":42,"n":-0.25,"b":false,"e":2}`,
			want:   `{"s":"line1\nline2","i":42,"n":-0.25,"b":false,"e":2}`,
		},
		{
			name:   "nullable",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"type":"integer"},{"type":"null"}]},"y":{"type":["string","null"]}}}`,
			in:     `{"x":5,"y":null}`,
			want:   `{"x":5,"y":null}`,
		},
		{
			name:   "arrays",
			schema: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}},"nums":{"type":"array","items":{"type":"integer"}},"flags":{"type":"array","items":{"type":"boolean"}}}}`,
			in:     `{"tags":["a","b c"],"nums":[1,2,3],"flags":[true,false]}`,
			want:   `{"tags":["a","b c"],"nums":[1,2,3],"flags":[true,false]}`,
		},
		{
			name:   "nested object as JSON",
			schema: `{"type":"object","properties":{"cfg":{"type":"object","properties":{"depth":{"type":"integer"}}}}}`,
			in:     `{"cfg":{"depth":3,"deep":{"k":[1,{"z":null}]}}}`,
			want:   `{"cfg":{"depth":3,"deep":{"k":[1,{"z":null}]}}}`,
		},
		{
			name:   "unknown keys and mismatched types preserved",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"n":{"type":"integer"},"e":{"enum":["x"]}}}`,
			in:     `{"a":"v","n":"not a number","e":"other","extra":{"k":1}}`,
			want:   `{"a":"v","n":"not a number","e":"other","extra":{"k":1}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, tc.schema)
			if tc.in != "" {
				if err := f.SetValues(json.RawMessage(tc.in)); err != nil {
					t.Fatalf("SetValues: %v", err)
				}
			}
			got, err := f.Values()
			if err != nil {
				t.Fatalf("Values: %v", err)
			}
			jsonEqual(t, got, tc.want)
			// A second SetValues with the produced output is stable.
			if err := f.SetValues(got); err != nil {
				t.Fatal(err)
			}
			again, err := f.Values()
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, again, tc.want)
		})
	}
}

func TestSetValuesResetsAbsentFields(t *testing.T) {
	f := mustParse(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string","default":"d"}}}`)
	_ = f.SetValues(json.RawMessage(`{"a":"1","b":"2","x":1}`))
	_ = f.SetValues(json.RawMessage(`{"a":"1"}`))
	got, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, `{"a":"1"}`)
}

func TestNumberParseError(t *testing.T) {
	f := mustParse(t, `{"type":"object","properties":{"i":{"type":"integer"}}}`)
	f.fields[0].input.SetValue("1.5")
	f.fields[0].dirty = true
	if _, err := f.Values(); err == nil {
		t.Fatal("expected error for non-integer")
	}
	if !strings.Contains(f.View(), "not an integer") {
		t.Fatalf("error not shown inline:\n%s", f.View())
	}
	f.fields[0].input.SetValue("2.0")
	got, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, `{"i":2}`)
}

func TestValidate(t *testing.T) {
	f := mustParse(t, `{
		"type":"object",
		"properties":{
			"name":{"type":"string","minLength":3},
			"count":{"type":"integer","maximum":10},
			"cfg":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}
		},
		"required":["name","count"],
		"additionalProperties": false
	}`)
	_ = f.SetValues(json.RawMessage(`{"name":"ab","cfg":{"x":"no"},"bogus":1}`))
	errs := f.Validate()
	if len(errs) == 0 {
		t.Fatal("expected validation errors")
	}
	byField := map[string]string{}
	for _, e := range errs {
		var fe *FieldError
		if !errors.As(e, &fe) {
			t.Fatalf("unexpected error type %T", e)
		}
		byField[fe.Field] += fe.Message + ";"
	}
	if !strings.Contains(byField["count"], "required") {
		t.Errorf("count should be required, got %v", byField)
	}
	if byField["name"] == "" {
		t.Errorf("name minLength error missing: %v", byField)
	}
	if !strings.Contains(byField["cfg"], "/x") {
		t.Errorf("cfg nested error should mention /x: %v", byField)
	}
	if !strings.Contains(byField[""], "bogus") {
		t.Errorf("additionalProperties error should be form-level: %v", byField)
	}
	view := f.View()
	if !strings.Contains(view, "✗") {
		t.Errorf("inline errors not rendered:\n%s", view)
	}

	_ = f.SetValues(json.RawMessage(`{"name":"abc","count":3}`))
	if errs := f.Validate(); len(errs) != 0 {
		t.Fatalf("expected valid, got %v", errs)
	}
}

func TestValidateBrokenSchemaIsSkipped(t *testing.T) {
	f := mustParse(t, `{"type":"object","properties":{"a":{"type":"string","pattern":"(["}}}`)
	_ = f.SetValues(json.RawMessage(`{"a":"x"}`))
	if errs := f.Validate(); len(errs) != 0 {
		t.Fatalf("broken schema should not block: %v", errs)
	}
	var ce *SchemaCompileError
	if err := ValidateValue(json.RawMessage(`{"type":"string","pattern":"(["}`), json.RawMessage(`"x"`)); !errors.As(err, &ce) {
		t.Fatalf("expected SchemaCompileError, got %v", err)
	}
}

func TestValidateValue(t *testing.T) {
	schema := json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`)
	if err := ValidateValue(schema, json.RawMessage(`{"id":1}`)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	err := ValidateValue(schema, json.RawMessage(`{"id":"x"}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Errors[0].Path != "/id" {
		t.Fatalf("expected error at /id, got %#v", err)
	}
	err = ValidateValue(schema, json.RawMessage(`{}`))
	if !errors.As(err, &ve) || len(ve.Errors[0].Missing) != 1 || ve.Errors[0].Missing[0] != "id" {
		t.Fatalf("expected missing id, got %#v", err)
	}
	if err := ValidateValue(nil, json.RawMessage(`42`)); err != nil {
		t.Fatalf("empty schema should accept anything: %v", err)
	}
	// External refs are never fetched.
	if err := ValidateValue(json.RawMessage(`{"$ref":"https://example.com/s.json"}`), json.RawMessage(`1`)); err == nil {
		t.Fatal("expected error for external ref")
	}
}

func TestSkeleton(t *testing.T) {
	schema := `{
		"type":"object",
		"$defs":{"Pt":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}},
		"properties":{
			"name":{"type":"string"},
			"mode":{"enum":["fast","slow"]},
			"limit":{"type":"integer","default":10},
			"on":{"type":"boolean"},
			"tags":{"type":"array","items":{"type":"string"}},
			"pt":{"$ref":"#/$defs/Pt"},
			"maybe":{"anyOf":[{"type":"null"},{"type":"string","const":"k"}]},
			"opt":{"type":"string"}
		},
		"required":["name","mode","limit","on","tags","pt","maybe"]
	}`
	got, _ := json.Marshal(Skeleton(json.RawMessage(schema)))
	jsonEqual(t, got, `{"name":"","mode":"fast","limit":10,"on":false,"tags":[],"pt":{"x":0,"y":0},"maybe":"k"}`)

	// No required list: include all properties.
	got, _ = json.Marshal(Skeleton(json.RawMessage(`{"properties":{"a":{"type":"integer"},"b":{"type":["string","null"]}}}`)))
	jsonEqual(t, got, `{"a":0,"b":""}`)

	// KindJSON field pre-fills with the skeleton of its property, resolving root defs.
	f := mustParse(t, schema)
	for _, fs := range f.fields {
		if fs.Name == "pt" && fs.Kind == KindEnum {
			t.Fatal("pt misclassified")
		}
	}
	f2 := mustParse(t, `{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}},"properties":{"p":{"type":"array","items":{"$ref":"#/$defs/P"}},"o":{"$ref":"#/$defs/P"}}}`)
	if f2.fields[1].Kind != KindJSON || !strings.Contains(f2.fields[1].area.Value(), `"q"`) {
		t.Fatalf("expected object skeleton in JSON field, got kind %v text %q", f2.fields[1].Kind, f2.fields[1].area.Value())
	}
	// Pristine optional skeleton isn't sent.
	v, _ := f2.Values()
	jsonEqual(t, v, `{}`)
}

func TestFromStrings(t *testing.T) {
	f := FromStrings([]StringField{{Name: "owner", Required: true}, {Name: "repo"}, {Name: "q"}})
	if err := f.SetValues(json.RawMessage(`{"repo":"mcptui"}`)); err != nil {
		t.Fatal(err)
	}
	sv := f.StringValues()
	if !reflect.DeepEqual(sv, map[string]string{"owner": "", "repo": "mcptui"}) {
		t.Fatalf("StringValues = %v", sv)
	}
	errs := f.Validate()
	if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "owner") {
		t.Fatalf("expected owner required, got %v", errs)
	}
}

// --- key-driven editing ---

func typeText(f *Form, s string) {
	for _, r := range s {
		f.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func press(f *Form, code rune, mod tea.KeyMod) {
	f.Update(tea.KeyPressMsg{Code: code, Mod: mod})
}

func TestKeyDrivenEditing(t *testing.T) {
	f := mustParse(t, `{
		"type":"object",
		"properties":{
			"name":{"type":"string"},
			"count":{"type":"integer"},
			"verbose":{"type":"boolean"},
			"level":{"enum":["low","mid","high"]},
			"tags":{"type":"array","items":{"type":"string"}},
			"body":{"type":"string"}
		},
		"required":["name"]
	}`)
	f.SetWidth(60)
	f.Focus()
	if !f.InputActive() {
		t.Fatal("string field should make input active")
	}

	typeText(f, "widget")
	press(f, tea.KeyTab, 0)
	typeText(f, "12")
	press(f, tea.KeyBackspace, 0) // "1"
	typeText(f, "5")              // "15"
	press(f, tea.KeyDown, 0)      // -> verbose
	if f.FocusedField() != 2 || f.InputActive() {
		t.Fatalf("expected boolean field focused without input, got %d", f.FocusedField())
	}
	f.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}) // unset -> true
	press(f, tea.KeyTab, 0)
	press(f, tea.KeyRight, 0) // low
	press(f, tea.KeyRight, 0) // mid
	press(f, tea.KeyTab, 0)   // tags
	typeText(f, "a")          // auto-adds first item
	press(f, 'n', tea.ModCtrl)
	typeText(f, "b")
	press(f, 'n', tea.ModCtrl)
	typeText(f, "zz")
	press(f, 'd', tea.ModCtrl) // remove "zz"
	press(f, tea.KeyTab, 0)    // body
	typeText(f, "one")
	press(f, 't', tea.ModCtrl) // multi-line
	press(f, tea.KeyEnter, 0)
	typeText(f, "two")

	got, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, `{"name":"widget","count":15,"verbose":true,"level":"mid","tags":["a","b"],"body":"one\ntwo"}`)

	// Navigate back up through the multi-line body and the array.
	press(f, tea.KeyUp, 0) // cursor to line 1 of body
	press(f, tea.KeyUp, 0) // leave body -> tags (last item)
	if f.FocusedField() != 4 {
		t.Fatalf("expected tags focused, got %d", f.FocusedField())
	}
	press(f, tea.KeyUp, 0) // item b -> item a
	press(f, tea.KeyUp, 0) // leave tags -> level
	if f.FocusedField() != 3 {
		t.Fatalf("expected level focused, got %d", f.FocusedField())
	}
	press(f, tea.KeyTab, 0|tea.ModShift)
	if f.FocusedField() != 2 {
		t.Fatalf("shift+tab should go back, got %d", f.FocusedField())
	}

	view := f.View()
	for _, want := range []string{"name", "*", "[x] true", "mid", "ctrl+n add"} {
		if !strings.Contains(view, want) && !(want == "ctrl+n add" && f.FocusedField() != 4) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	f.Blur()
	if f.Focused() || f.InputActive() {
		t.Fatal("blur should clear focus")
	}
}

func TestPasteIntoArrayAddsItem(t *testing.T) {
	f := mustParse(t, `{"type":"object","properties":{"ids":{"type":"array","items":{"type":"integer"}}}}`)
	f.Focus()
	f.Update(tea.PasteMsg{Content: "7"})
	got, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, `{"ids":[7]}`)
}

func TestTitleLabels(t *testing.T) {
	f, err := Parse(json.RawMessage(`{"type":"object","properties":{"client_id":{"type":"string","title":"Client ID"},"plain":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	f.SetWidth(80)
	if v := f.View(); !strings.Contains(v, "client_id") || strings.Contains(v, "Client ID") {
		t.Fatalf("default labels should be names:\n%s", v)
	}
	f.UseTitleLabels()
	if v := f.View(); !strings.Contains(v, "Client ID") || !strings.Contains(v, "plain") {
		t.Fatalf("title labels:\n%s", v)
	}
}

func TestFieldLinesAndClick(t *testing.T) {
	f, err := Parse(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","description":"first"},"b":{"type":"boolean"},"c":{"type":"string","enum":["x","y"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	f.SetWidth(80)
	f.Focus()
	lines := strings.Split(f.View(), "\n")
	if f.FieldAtLine(0) != 0 || f.FieldAtLine(1) != 0 || f.FieldLine(1) != 2 || f.FieldAtLine(len(lines)-1) != 2 {
		t.Fatalf("line map %v for\n%s", f.lineField, strings.Join(lines, "\n"))
	}
	f.ClickField(1)
	if f.FocusedField() != 1 {
		t.Fatalf("focus = %d", f.FocusedField())
	}
	f.ClickField(1) // toggle
	if v, _ := f.Values(); string(v) != `{"b":true}` {
		t.Fatalf("values = %s", v)
	}
	if !f.AtLastField() == (f.FocusedField() == 2) {
		t.Fatal("AtLastField wrong")
	}
}
