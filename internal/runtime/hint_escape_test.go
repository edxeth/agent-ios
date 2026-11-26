package runtime

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
)

// parity-045 review F6: a hint is printed with Rust's {:?} for str
// (agent-browser render_tree), so debugQuote escapes exactly the
// characters that Rust's char::escape_debug escapes in a str: grapheme
// extenders and the non-printable characters, also Unicode spaces such as
// NBSP that Go's unicode.IsGraphic accepts. The expected values come from
// rustc: these cases from a Rust program that printed format!("{:?}", s),
// the table from testdata/rust_debug_escapes.rs.

// Each want is the output of rustc 1.97 (Unicode 17) for the input.
func TestDebugQuoteRustCases(t *testing.T) {
	for in, want := range map[string]string{
		"a\u00a0b":   `"a\u{a0}b"`,
		"\u3000":     `"\u{3000}"`,
		"\u1680":     `"\u{1680}"`,
		"\u202f":     `"\u{202f}"`,
		"\u2028":     `"\u{2028}"`,
		"\u200b":     `"\u{200b}"`,
		"\u200c":     `"\u{200c}"`,
		"\u00ad":     `"\u{ad}"`,
		"\u007f":     `"\u{7f}"`,
		"e\u0301":    `"e\u{301}"`,
		"\u09be":     `"\u{9be}"`,
		"\ue000":     `"\u{e000}"`,
		"\u0378":     `"\u{378}"`,
		"\U0001F3FB": "\"\U0001F3FB\"",
		"😀":          `"😀"`,
	} {
		if got := debugQuote(in); got != want {
			t.Errorf("debugQuote(%+q) = %s, want %s", in, got, want)
		}
	}
}

// rustEscapes reads testdata/rust_debug_escapes.txt: the code points that
// Rust's Debug for str escapes, and the Unicode version of that rustc.
func rustEscapes(t *testing.T) ([]bool, string) {
	t.Helper()
	data, err := os.ReadFile("testdata/rust_debug_escapes.txt")
	if err != nil {
		t.Fatal(err)
	}
	esc := make([]bool, unicode.MaxRune+1)
	var version string
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		var a, b, x, y, z int
		if n, _ := fmt.Sscanf(s.Text(), "# unicode (%d, %d, %d)", &x, &y, &z); n == 3 {
			version = fmt.Sprintf("%d.%d.%d", x, y, z)
			continue
		}
		if n, err := fmt.Sscanf(s.Text(), "%x-%x", &a, &b); n != 2 || a > b || b > unicode.MaxRune {
			t.Fatalf("rust_debug_escapes.txt line %q: %v", s.Text(), err)
		}
		for i := a; i <= b; i++ {
			esc[i] = true
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return esc, version
}

// rustQuote is the reference encoder: Rust's escape_debug for a str whose
// escaped characters are the oracle's table.
func rustQuote(esc []bool, s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case !esc[r]:
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == 0:
			b.WriteString(`\0`)
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "\\u{%x}", r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Every code point a Go string can hold is escaped as Rust escapes it.
func TestDebugQuoteMatchesRustTable(t *testing.T) {
	esc, version := rustEscapes(t)
	if version != unicode.Version {
		t.Fatalf("rust_debug_escapes.txt is from Unicode %s, Go has %s: regenerate it (testdata/rust_debug_escapes.rs) with a matching rustc", version, unicode.Version)
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		s := string(r)
		if got, want := debugQuote(s), rustQuote(esc, s); got != want {
			t.Errorf("debugQuote(%+q) = %s, want %s", s, got, want)
		}
	}
}

// Property: a string of any of those characters, mixed (escaped ones next
// to printable ones and combining marks at any position), is the
// concatenation of their Rust escapes.
func TestDebugQuoteProperty(t *testing.T) {
	esc, _ := rustEscapes(t)
	// The alphabet: both sides of every boundary of the table, and ASCII.
	var alphabet []rune
	for r := rune(1); r <= unicode.MaxRune; r++ {
		if esc[r] != esc[r-1] {
			for _, c := range []rune{r - 1, r} {
				if c < 0xd800 || c > 0xdfff {
					alphabet = append(alphabet, c)
				}
			}
		}
	}
	for r := rune(0); r < 0x80; r++ {
		alphabet = append(alphabet, r)
	}
	cfg := &quick.Config{MaxCount: 3000, Values: func(v []reflect.Value, r *rand.Rand) {
		rs := make([]rune, r.Intn(10))
		for i := range rs {
			rs[i] = alphabet[r.Intn(len(alphabet))]
		}
		v[0] = reflect.ValueOf(string(rs))
	}}
	f := func(s string) bool {
		got, want := debugQuote(s), rustQuote(esc, s)
		if got != want {
			t.Logf("debugQuote(%+q) = %s, want %s", s, got, want)
		}
		return got == want
	}
	if err := quick.Check(f, cfg); err != nil {
		t.Fatal(err)
	}
}
