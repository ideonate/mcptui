// Package schemaform builds editable forms from JSON Schemas (tool input
// schemas, prompt arguments, URI template variables) and validates values.
package schemaform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Kind is the editing widget a field uses.
type Kind int

const (
	KindString Kind = iota
	KindInteger
	KindNumber
	KindBoolean
	KindEnum
	KindScalarArray
	// KindJSON is the fallback for anything the form can't edit directly:
	// nested objects, real unions, arrays of objects, unresolvable $refs.
	KindJSON
)

// KindStringArray is an alias of KindScalarArray.
const KindStringArray = KindScalarArray

func (k Kind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindInteger:
		return "integer"
	case KindNumber:
		return "number"
	case KindBoolean:
		return "boolean"
	case KindEnum:
		return "enum"
	case KindScalarArray:
		return "array"
	case KindJSON:
		return "json"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Field describes one top-level argument.
type Field struct {
	Name, Title, Description string
	Kind                     Kind
	Required                 bool
	Nullable                 bool // anyOf [X, null] or type [X, "null"]
	Enum                     []any
	Default                  json.RawMessage
	Schema                   json.RawMessage // the property's own schema
	ItemKind                 Kind            // for KindScalarArray
}

// RootFieldName names the single field used when the root schema isn't a
// plain object with properties.
const RootFieldName = "(arguments)"

type node = map[string]any

func decodeAny(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func isEmptySchema(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("{}")) || bytes.Equal(t, []byte("true"))
}

// lookupPointer follows a local JSON pointer ("#/a/b") within root.
func lookupPointer(root any, ref string) (node, bool) {
	frag, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil, false
	}
	cur := root
	if frag != "" {
		for _, tok := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
			m, ok := cur.(node)
			if !ok {
				return nil, false
			}
			if cur, ok = m[tok]; !ok {
				return nil, false
			}
		}
	}
	m, ok := cur.(node)
	return m, ok
}

// resolve follows local $refs, carrying over annotation keywords from the
// referring schema. ok is false for non-local or broken refs.
func resolve(root any, s node) (node, bool) {
	for depth := 0; depth < 16; depth++ {
		ref, has := s["$ref"].(string)
		if !has {
			return s, true
		}
		target, ok := lookupPointer(root, ref)
		if !ok {
			return s, false
		}
		merged := node{}
		for k, v := range target {
			merged[k] = v
		}
		for _, k := range []string{"title", "description", "default"} {
			if v, ok := s[k]; ok {
				merged[k] = v
			}
		}
		s = merged
	}
	return s, false
}

func isNullSchema(root any, v any) bool {
	m, ok := v.(node)
	if !ok {
		return false
	}
	m, _ = resolve(root, m)
	if t, ok := m["type"].(string); ok && t == "null" && len(m) <= 3 {
		return true
	}
	if c, ok := m["const"]; ok && c == nil {
		return true
	}
	return false
}

// unwrapNullable turns anyOf/oneOf [X, null] and type [X, "null"] into X.
func unwrapNullable(root any, s node) (node, bool) {
	for _, key := range []string{"anyOf", "oneOf"} {
		opts, ok := s[key].([]any)
		if !ok || len(opts) != 2 {
			continue
		}
		var other any
		nulls := 0
		for _, o := range opts {
			if isNullSchema(root, o) {
				nulls++
			} else {
				other = o
			}
		}
		om, ok := other.(node)
		if nulls != 1 || !ok {
			continue
		}
		om, ok = resolve(root, om)
		if !ok {
			return s, false
		}
		merged := node{}
		for k, v := range om {
			merged[k] = v
		}
		for k, v := range s {
			if k == key {
				continue
			}
			merged[k] = v
		}
		return merged, true
	}
	if types, ok := s["type"].([]any); ok {
		var rest []any
		nullable := false
		for _, t := range types {
			if t == "null" {
				nullable = true
			} else {
				rest = append(rest, t)
			}
		}
		if nullable && len(rest) == 1 {
			merged := node{}
			for k, v := range s {
				merged[k] = v
			}
			merged["type"] = rest[0]
			return merged, true
		}
	}
	return s, false
}

var scalarTypes = map[string]Kind{
	"string":  KindString,
	"integer": KindInteger,
	"number":  KindNumber,
	"boolean": KindBoolean,
}

// hasComposition reports keywords that make a schema more than one type.
func hasComposition(s node) bool {
	for _, k := range []string{"anyOf", "oneOf", "allOf", "not", "if", "$ref", "$dynamicRef"} {
		if _, ok := s[k]; ok {
			return true
		}
	}
	return false
}

// classify returns the widget kind for a (resolved, unwrapped) schema.
func classify(root any, s node) (Kind, Kind, []any) {
	if hasComposition(s) {
		return KindJSON, 0, nil
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		for _, e := range enum {
			switch e.(type) {
			case string, json.Number, bool:
			case nil:
			default:
				return KindJSON, 0, nil
			}
		}
		return KindEnum, 0, enum
	}
	t, _ := s["type"].(string)
	if k, ok := scalarTypes[t]; ok {
		return k, 0, nil
	}
	if t == "array" {
		items, ok := s["items"].(node)
		if !ok {
			return KindJSON, 0, nil
		}
		items, ok = resolve(root, items)
		if !ok || hasComposition(items) {
			return KindJSON, 0, nil
		}
		if _, ok := items["enum"]; ok {
			return KindScalarArray, KindString, nil
		}
		it, _ := items["type"].(string)
		if k, ok := scalarTypes[it]; ok {
			return KindScalarArray, k, nil
		}
	}
	return KindJSON, 0, nil
}

func stringsOf(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseFields extracts fields from a root object schema. ok is false when
// the root isn't a plain object with properties (callers fall back to a
// single JSON field). A schema with no properties yields zero fields.
func parseFields(raw json.RawMessage) (fields []Field, ok bool, err error) {
	if isEmptySchema(raw) {
		return nil, true, nil
	}
	rootAny, err := decodeAny(raw)
	if err != nil {
		return nil, false, fmt.Errorf("invalid schema JSON: %w", err)
	}
	root, isObj := rootAny.(node)
	if !isObj {
		return nil, false, nil
	}
	s, rok := resolve(rootAny, root)
	if !rok {
		return nil, false, nil
	}
	if t, has := s["type"]; has && t != "object" {
		return nil, false, nil
	}
	props, _ := s["properties"].(node)
	if len(props) == 0 {
		for _, k := range []string{"anyOf", "oneOf", "allOf", "patternProperties"} {
			if _, has := s[k]; has {
				return nil, false, nil
			}
		}
		if ap, has := s["additionalProperties"]; has && ap != false {
			return nil, false, nil
		}
		return nil, true, nil
	}
	required := map[string]bool{}
	for _, r := range stringsOf(s["required"]) {
		required[r] = true
	}
	names := orderedKeys(raw, props)
	for _, name := range names {
		pm, _ := props[name].(node)
		if pm == nil {
			pm = node{}
		}
		f := Field{Name: name, Required: required[name]}
		f.Schema, _ = json.Marshal(pm)
		res, rok := resolve(rootAny, pm)
		nullable := false
		if rok {
			res, nullable = unwrapNullable(rootAny, res)
		}
		f.Nullable = nullable
		f.Title, _ = res["title"].(string)
		f.Description, _ = res["description"].(string)
		if d, has := res["default"]; has {
			f.Default, _ = json.Marshal(d)
		}
		if !rok {
			f.Kind = KindJSON
		} else {
			f.Kind, f.ItemKind, f.Enum = classify(rootAny, res)
		}
		if f.Kind == KindJSON && len(pm) > 0 {
			// Make the property schema self-contained enough for Skeleton by
			// embedding the root's definitions.
			f.Schema = embedDefs(root, pm)
		}
		fields = append(fields, f)
	}
	return fields, true, nil
}

func embedDefs(root, prop node) json.RawMessage {
	cp := node{}
	for k, v := range prop {
		cp[k] = v
	}
	for _, k := range []string{"$defs", "definitions"} {
		if d, ok := root[k]; ok {
			if _, exists := cp[k]; !exists {
				cp[k] = d
			}
		}
	}
	b, _ := json.Marshal(cp)
	return b
}

// orderedKeys returns property names in their order in the source JSON,
// since Go maps lose it.
func orderedKeys(raw json.RawMessage, props node) []string {
	order := propertyOrder(raw)
	seen := map[string]bool{}
	var out []string
	for _, k := range order {
		if _, ok := props[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	var rest []string
	for k := range props {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// propertyOrder finds the key order of the root "properties" object.
func propertyOrder(raw json.RawMessage) []string {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	for d.More() {
		kt, err := d.Token()
		if err != nil {
			return nil
		}
		key, _ := kt.(string)
		if key != "properties" {
			var skip json.RawMessage
			if d.Decode(&skip) != nil {
				return nil
			}
			continue
		}
		if t, err := d.Token(); err != nil || t != json.Delim('{') {
			return nil
		}
		var out []string
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return out
			}
			if k, ok := kt.(string); ok {
				out = append(out, k)
			}
			var skip json.RawMessage
			if d.Decode(&skip) != nil {
				return out
			}
		}
		return out
	}
	return nil
}

// Skeleton builds an example value for a schema: defaults, consts, the first
// enum value, zero values for scalars, [] for arrays, and objects with their
// required properties (or all properties when none are required).
func Skeleton(schema json.RawMessage) any {
	if isEmptySchema(schema) {
		return node{}
	}
	root, err := decodeAny(schema)
	if err != nil {
		return node{}
	}
	return skeleton(root, root, 0)
}

func skeleton(root, v any, depth int) any {
	s, ok := v.(node)
	if !ok || depth > 8 {
		return nil
	}
	s, _ = resolve(root, s)
	if d, ok := s["default"]; ok {
		return d
	}
	if c, ok := s["const"]; ok {
		return c
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	if ex, ok := s["examples"].([]any); ok && len(ex) > 0 {
		return ex[0]
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if opts, ok := s[k].([]any); ok {
			for _, o := range opts {
				if !isNullSchema(root, o) {
					return skeleton(root, o, depth+1)
				}
			}
		}
	}
	if all, ok := s["allOf"].([]any); ok {
		merged := node{}
		for _, part := range all {
			if m, ok := skeleton(root, part, depth+1).(node); ok {
				for k, val := range m {
					merged[k] = val
				}
			}
		}
		if props, ok := s["properties"].(node); ok {
			for k, val := range skeletonObject(root, s, props, depth) {
				merged[k] = val
			}
		}
		return merged
	}
	t := s["type"]
	if types, ok := t.([]any); ok {
		t = nil
		for _, tt := range types {
			if tt != "null" {
				t = tt
				break
			}
		}
	}
	switch t {
	case "string":
		return ""
	case "integer", "number":
		return json.Number("0")
	case "boolean":
		return false
	case "array":
		return []any{}
	case "null":
		return nil
	case "object":
		props, _ := s["properties"].(node)
		return skeletonObject(root, s, props, depth)
	}
	if props, ok := s["properties"].(node); ok {
		return skeletonObject(root, s, props, depth)
	}
	return nil
}

func skeletonObject(root any, s, props node, depth int) node {
	out := node{}
	req := stringsOf(s["required"])
	if len(req) == 0 {
		for k := range props {
			req = append(req, k)
		}
	}
	for _, k := range req {
		out[k] = skeleton(root, props[k], depth+1)
	}
	return out
}

// SchemaError is one validation failure.
type SchemaError struct {
	// Path is a JSON pointer into the instance ("" for the root).
	Path    string
	Message string
	// Missing lists property names for "required" failures.
	Missing []string
}

// ValidationError is returned by ValidateValue when the value doesn't match.
type ValidationError struct {
	Errors []SchemaError
}

func (e *ValidationError) Error() string {
	lines := make([]string, 0, len(e.Errors))
	for _, se := range e.Errors {
		p := se.Path
		if p == "" {
			p = "/"
		}
		lines = append(lines, fmt.Sprintf("at %s: %s", p, se.Message))
	}
	return strings.Join(lines, "; ")
}

// SchemaCompileError means the schema itself couldn't be compiled; callers
// typically skip validation rather than block the user.
type SchemaCompileError struct{ Err error }

func (e *SchemaCompileError) Error() string { return "invalid schema: " + e.Err.Error() }
func (e *SchemaCompileError) Unwrap() error { return e.Err }

var (
	compiled sync.Map // string(schema) -> *jsonschema.Schema
	printer  = message.NewPrinter(language.English)
)

func compile(schema json.RawMessage) (*jsonschema.Schema, error) {
	key := string(schema)
	if v, ok := compiled.Load(key); ok {
		return v.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, &SchemaCompileError{err}
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	const loc = "urn:mcptui:schema"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, &SchemaCompileError{err}
	}
	sch, err := c.Compile(loc)
	if err != nil {
		return nil, &SchemaCompileError{err}
	}
	compiled.Store(key, sch)
	return sch, nil
}

// noLoader refuses to fetch remote or file schemas.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("refusing to load external schema %s", url)
}

// ValidateValue checks value against schema. It returns nil when valid (or
// when the schema is empty), *ValidationError when invalid, and
// *SchemaCompileError when the schema is broken.
func ValidateValue(schema json.RawMessage, value json.RawMessage) error {
	if isEmptySchema(schema) {
		return nil
	}
	sch, err := compile(schema)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("invalid JSON value: %w", err)
	}
	err = sch.Validate(inst)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err
	}
	out := &ValidationError{}
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		se := SchemaError{Path: pointer(e.InstanceLocation), Message: e.ErrorKind.LocalizedString(printer)}
		if r, ok := e.ErrorKind.(*kind.Required); ok {
			se.Missing = append([]string(nil), r.Missing...)
		}
		k := se.Path + "\x00" + se.Message
		if !seen[k] {
			seen[k] = true
			out.Errors = append(out.Errors, se)
		}
	}
	walk(ve)
	if len(out.Errors) == 0 {
		out.Errors = []SchemaError{{Message: ve.Error()}}
	}
	return out
}

func pointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"))
	}
	return b.String()
}
