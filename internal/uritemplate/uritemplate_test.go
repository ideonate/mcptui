package uritemplate

import (
	"reflect"
	"testing"
)

func TestExpandRFCExamples(t *testing.T) {
	vars := map[string]string{
		"var":   "value",
		"hello": "Hello World!",
		"path":  "/foo/bar",
		"x":     "1024",
		"y":     "768",
		"empty": "",
	}
	cases := map[string]string{
		// Level 1
		"{var}":   "value",
		"{hello}": "Hello%20World%21",
		// Level 2
		"{+var}":           "value",
		"{+hello}":         "Hello%20World!",
		"{+path}/here":     "/foo/bar/here",
		"here?ref={+path}": "here?ref=/foo/bar",
		"X{#var}":          "X#value",
		"X{#hello}":        "X#Hello%20World!",
		// Level 3
		"map?{x,y}":      "map?1024,768",
		"{x,hello,y}":    "1024,Hello%20World%21,768",
		"{+x,hello,y}":   "1024,Hello%20World!,768",
		"{+path,x}/here": "/foo/bar,1024/here",
		"{#x,hello,y}":   "#1024,Hello%20World!,768",
		"{#path,x}/here": "#/foo/bar,1024/here",
		"X{.var}":        "X.value",
		"X{.x,y}":        "X.1024.768",
		"{/var}":         "/value",
		"{/var,x}/here":  "/value/1024/here",
		"{;x,y}":         ";x=1024;y=768",
		"{;x,y,empty}":   ";x=1024;y=768;empty",
		"{?x,y}":         "?x=1024&y=768",
		"{?x,y,empty}":   "?x=1024&y=768&empty=",
		"?fixed=yes{&x}": "?fixed=yes&x=1024",
		"{&x,y,empty}":   "&x=1024&y=768&empty=",
		// Extras
		"{var:3}":           "val",
		"{?x,undef}":        "?x=1024",
		"{undef}":           "",
		"file:///{+path}":   "file:////foo/bar",
		"unterminated {var": "unterminated {var",
		"{+var}%20{+hello}": "value%20Hello%20World!",
	}
	for tmpl, want := range cases {
		if got := Expand(tmpl, vars); got != want {
			t.Errorf("Expand(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

func TestVariables(t *testing.T) {
	got := Variables("repo://{owner}/{repo}/issues{?state,owner,labels*}{#frag:3}")
	want := []string{"owner", "repo", "state", "labels", "frag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Variables = %v, want %v", got, want)
	}
	if v := Variables("plain://uri"); len(v) != 0 {
		t.Fatalf("expected no variables, got %v", v)
	}
}
