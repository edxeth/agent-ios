package commands

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// Locations as chrome-devtools-mcp's set_logpoints takes them
// (<url>:<line>[:<column>], 1-based); a port is not a line.
func TestParseLocation(t *testing.T) {
	for _, c := range []struct {
		in        string
		url       string
		line, col int
	}{
		{"app.js:42", "app.js", 42, 0},
		{"src/cart.ts:12:5", "src/cart.ts", 12, 5},
		{"http://h:8080/a.js:3", "http://h:8080/a.js", 3, 0},
		{"http://h:8080/a.js:3:7", "http://h:8080/a.js", 3, 7},
		{"http://h:8080:12", "http://h:8080", 12, 0},
		{"webpack:///src/a.ts:1:1", "webpack:///src/a.ts", 1, 1},
	} {
		u, line, col, err := ParseLocation(c.in)
		if err != nil || u != c.url || line != c.line || col != c.col {
			t.Errorf("ParseLocation(%q) = %q %d %d %v; want %q %d %d", c.in, u, line, col, err, c.url, c.line, c.col)
		}
	}
	for _, bad := range []string{"app.js", "app.js:0", "app.js:x", ":3", "app.js:-1", ""} {
		if _, _, _, err := ParseLocation(bad); err == nil {
			t.Errorf("ParseLocation(%q) did not fail", bad)
		}
	}
}

// lpLoc is a generated location: a path or URL that does not end in
// ":<digits>", a line and an optional column.
type lpLoc struct {
	url       string
	line, col int
}

func (lpLoc) Generate(r *rand.Rand, _ int) reflect.Value {
	parts := []string{"http://h", "https://h:8443", "webpack://", "", "src", "a.b", "x-y", "9a"}
	var b strings.Builder
	for n := r.Intn(4) + 1; n > 0; n-- {
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		b.WriteString(parts[r.Intn(len(parts))])
	}
	b.WriteString("/f.js")
	col := 0
	if r.Intn(2) == 0 {
		col = r.Intn(500) + 1
	}
	return reflect.ValueOf(lpLoc{url: b.String(), line: r.Intn(10000) + 1, col: col})
}

// Property: formatting a location and parsing it gives it back.
func TestParseLocationRoundTripProperty(t *testing.T) {
	f := func(l lpLoc) bool {
		s := fmt.Sprintf("%s:%d", l.url, l.line)
		if l.col > 0 {
			s += fmt.Sprintf(":%d", l.col)
		}
		u, line, col, err := ParseLocation(s)
		return err == nil && u == l.url && line == l.line && col == l.col
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
