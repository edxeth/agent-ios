package commands

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

// unicodeText is a generated option text: valid Unicode code points of
// every UTF-8 length (surrogate pairs in UTF-16), of lengths around the
// maxOptionText cut.
type unicodeText string

func (unicodeText) Generate(r *rand.Rand, _ int) reflect.Value {
	var n int
	switch r.Intn(3) {
	case 0:
		n = r.Intn(10)
	case 1:
		n = maxOptionText - 5 + r.Intn(11)
	default:
		n = maxOptionText + 6 + r.Intn(200)
	}
	var b strings.Builder
	for range n {
		var c rune
		switch r.Intn(4) {
		case 0:
			c = rune(0x20 + r.Intn(0x5f)) // one byte
		case 1:
			c = rune(0x80 + r.Intn(0x780)) // two bytes
		case 2:
			c = rune(0x800 + r.Intn(0xd800-0x800)) // three bytes, below the surrogates
		default:
			c = rune(0x10000 + r.Intn(0x10ffff-0x10000+1)) // four bytes: a UTF-16 surrogate pair
		}
		b.WriteRune(c)
	}
	return reflect.ValueOf(unicodeText(b.String()))
}

// codePoints returns the first n code points of s, counted by decoding
// UTF-8 (the page's cut: the first OLEN + 1 code points, mod-forms cut).
func codePoints(s string, n int) (string, bool) {
	for i := 0; i < len(s); n-- {
		if n == 0 {
			return s[:i], true
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s, false
}

// Review2 F16: an option text is cut on code points: a text of at most
// maxOptionText code points is kept whole, a longer one is its first
// maxOptionText code points and "…", valid UTF-8 without U+FFFD; and
// cutting the page's cut (the first maxOptionText + 1 code points) gives
// the same text as cutting the whole, so the page and Go agree.
func TestCutOptionOnCodePoints(t *testing.T) {
	prop := func(u unicodeText) bool {
		s := string(u)
		got := cutOption(s)
		want := s
		if head, cut := codePoints(s, maxOptionText); cut {
			want = head + "\u2026"
		}
		page, _ := codePoints(s, maxOptionText+1)
		if got != want || !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) || cutOption(page) != got {
			t.Logf("cutOption(%q) = %q, of the page's cut %q; want %q", s, got, cutOption(page), want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
