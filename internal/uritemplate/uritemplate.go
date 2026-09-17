// Package uritemplate implements RFC 6570 URI template expansion for string
// variables (levels 1–3, plus the level-4 prefix modifier).
package uritemplate

import (
	"strconv"
	"strings"
)

type operator struct {
	first         string
	sep           string
	named         bool
	ifEmpty       string
	allowReserved bool
}

var operators = map[byte]operator{
	'+': {first: "", sep: ",", allowReserved: true},
	'#': {first: "#", sep: ",", allowReserved: true},
	'.': {first: ".", sep: "."},
	'/': {first: "/", sep: "/"},
	';': {first: ";", sep: ";", named: true},
	'?': {first: "?", sep: "&", named: true, ifEmpty: "="},
	'&': {first: "&", sep: "&", named: true, ifEmpty: "="},
}

var simple = operator{first: "", sep: ","}

type varSpec struct {
	name   string
	prefix int // 0 = none
}

// parseExpr splits "{op var,var:3,var*}" contents into operator and specs.
func parseExpr(expr string) (operator, []varSpec) {
	op := simple
	if expr != "" {
		if o, ok := operators[expr[0]]; ok {
			op = o
			expr = expr[1:]
		}
	}
	var specs []varSpec
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimSuffix(part, "*")
		if part == "" {
			continue
		}
		spec := varSpec{name: part}
		if name, n, ok := strings.Cut(part, ":"); ok {
			spec.name = name
			if p, err := strconv.Atoi(n); err == nil && p > 0 {
				spec.prefix = p
			}
		}
		specs = append(specs, spec)
	}
	return op, specs
}

// walk calls lit for literal text and expr for each {expression}.
func walk(tmpl string, lit func(string), expr func(string)) {
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			lit(tmpl)
			return
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			lit(tmpl)
			return
		}
		lit(tmpl[:i])
		expr(tmpl[i+1 : i+j])
		tmpl = tmpl[i+j+1:]
	}
}

// Variables returns the variable names in order of first appearance.
func Variables(tmpl string) []string {
	var out []string
	seen := map[string]bool{}
	walk(tmpl, func(string) {}, func(e string) {
		_, specs := parseExpr(e)
		for _, s := range specs {
			if !seen[s.name] {
				seen[s.name] = true
				out = append(out, s.name)
			}
		}
	})
	return out
}

// Expand substitutes vars into tmpl. Variables absent from vars are
// undefined and skipped; an empty string is defined.
func Expand(tmpl string, vars map[string]string) string {
	var b strings.Builder
	walk(tmpl, func(s string) { b.WriteString(s) }, func(e string) {
		op, specs := parseExpr(e)
		first := true
		for _, s := range specs {
			val, ok := vars[s.name]
			if !ok {
				continue
			}
			if first {
				b.WriteString(op.first)
				first = false
			} else {
				b.WriteString(op.sep)
			}
			if s.prefix > 0 {
				if r := []rune(val); len(r) > s.prefix {
					val = string(r[:s.prefix])
				}
			}
			if op.named {
				b.WriteString(encode(s.name, true))
				if val == "" {
					b.WriteString(op.ifEmpty)
					continue
				}
				b.WriteByte('=')
			}
			b.WriteString(encode(val, op.allowReserved))
		}
	})
	return b.String()
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

func isReserved(c byte) bool {
	return strings.IndexByte(":/?#[]@!$&'()*+,;=", c) >= 0
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func encode(s string, allowReserved bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isUnreserved(c):
			b.WriteByte(c)
		case allowReserved && isReserved(c):
			b.WriteByte(c)
		case allowReserved && c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteString(s[i : i+3])
			i += 2
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
