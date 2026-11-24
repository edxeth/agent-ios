package sim

import (
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// atom is a piece of a generated path segment, query or fragment: text as
// the caller writes it, and form as a browser writes it (WHATWG URL),
// taken from the URL standard's percent-encode sets, not from browserForm.
// escape is true when text is an escape that the caller wrote ("%41"),
// which the browser keeps as written, in its hex case (final review F3:
// new URL("http://h/%2f").href keeps "%2f"); browserEscapes is true when
// the browser escapes text, so that a spelling may write it either as text
// or as the browser's escape, in upper-case hex.
type atom struct {
	text, form             string
	escape, browserEscapes bool
}

func lit(s string) atom    { return atom{text: s, form: s} }
func enc(s, f string) atom { return atom{text: s, form: f, browserEscapes: true} }
func esc(s string) atom    { return atom{text: s, form: s, escape: true} }

var (
	pathAtoms = []atom{
		lit("a"), lit("b"), lit("x.y"), lit("~"), lit("'"), lit("!$&()*+,;=:@"),
		enc(" ", "%20"), enc("\"", "%22"), enc("<", "%3C"), enc(">", "%3E"), enc("`", "%60"), enc("{", "%7B"), enc("}", "%7D"),
		enc("€", "%E2%82%AC"), enc("é", "%C3%A9"), enc("日本", "%E6%97%A5%E6%9C%AC"),
		esc("%41"), esc("%20"), esc("%2f"), esc("%2F"), esc("%e2%82%ac"), esc("%E2%82%AC"), esc("%7e"),
	}
	queryAtoms = []atom{
		lit("x"), lit("="), lit("&"), lit("1"), lit("+"), lit("?"), lit("/"),
		enc(" ", "%20"), enc("\"", "%22"), enc("<", "%3C"), enc("€", "%E2%82%AC"), enc("ü", "%C3%BC"),
		esc("%20"), esc("%2b"), esc("%2B"), esc("%41"), esc("%3d"), esc("%3c"),
	}
	fragmentAtoms = []atom{
		lit("top"), lit("k"), lit("'"), lit("?"), lit("/"),
		enc("`", "%60"), enc(" ", "%20"), enc(">", "%3E"), enc("é", "%C3%A9"),
		esc("%60"), esc("%41"), esc("%c3%a9"), esc("%C3%A9"), esc("%ab"), esc("%AB"),
	}
)

// urlParts are the parts of a generated URL; the oracle (key) compares
// them, not text.
type urlParts struct {
	https    bool
	host     host
	port     string   // "" or a port that is not the scheme's default
	segments [][]atom // path segments, each of one atom or more
	query    []atom   // nil: no "?"
	fragment []atom   // nil: no "#"
}

// host is a host name in lower case, a plain IPv4 address, or an IPv6
// address.
type host struct {
	name string
	ipv4 *[4]byte
	ipv6 *[8]uint16
}

func (h host) key() string {
	switch {
	case h.ipv4 != nil:
		return fmt.Sprintf("v4%v", *h.ipv4)
	case h.ipv6 != nil:
		return fmt.Sprintf("v6%v", *h.ipv6)
	}
	return h.name
}

func formOf(as []atom) string {
	var b strings.Builder
	for _, a := range as {
		b.WriteString(a.form)
	}
	return b.String()
}

// key is the URL as a browser writes it, from the parts alone: what sim up
// compares.
func (p urlParts) key() string {
	path := make([]string, len(p.segments))
	for i, s := range p.segments {
		path[i] = formOf(s)
	}
	q, f := "-", "-"
	if p.query != nil {
		q = "?" + formOf(p.query)
	}
	if p.fragment != nil {
		f = "#" + formOf(p.fragment)
	}
	return fmt.Sprintf("%t|%s|%s|/%s|%s|%s", p.https, p.host.key(), p.port, strings.Join(path, "/"), q, f)
}

// spell writes p in one of the forms that a browser reads as p: the
// scheme, a host name and IPv6 hex digits in any case, an IPv6 address
// with or without "::" and leading zeros, the default port written or not
// (with leading zeros), "." and "x/.." segments, also escaped, before a
// segment, an empty path as "" or "/", and a character that a browser
// escapes as itself or as the browser's escape of it, in upper-case hex
// (except a space at the very end, which a browser strips). An escape of
// the URL is written as it is: its hex case is part of the URL.
func (p urlParts) spell(r *rand.Rand) string {
	var b strings.Builder
	scheme, def := "http", "80"
	if p.https {
		scheme, def = "https", "443"
	}
	b.WriteString(randomCase(r, scheme) + "://" + p.host.spell(r))
	switch {
	case p.port != "":
		b.WriteString(":" + strings.Repeat("0", r.Intn(2)) + p.port)
	case r.Intn(3) == 0:
		b.WriteString(":" + strings.Repeat("0", r.Intn(2)) + def)
	}
	for _, s := range p.segments {
		if r.Intn(4) == 0 {
			b.WriteString(pick(r, "/.", "/zz/..", "/%2e", "/zz/.%2E", "/zz/%2e%2e"))
		}
		b.WriteString("/" + spellAtoms(r, s))
	}
	if len(p.segments) == 0 && r.Intn(2) == 0 {
		b.WriteString("/")
	}
	if p.query != nil {
		b.WriteString("?" + spellAtoms(r, p.query))
	}
	if p.fragment != nil {
		b.WriteString("#" + spellAtoms(r, p.fragment))
	}
	// A browser strips a space at the end of the text; only a space atom,
	// which it escapes, can end it.
	if s, ok := strings.CutSuffix(b.String(), " "); ok {
		return s + "%20"
	}
	return b.String()
}

func spellAtoms(r *rand.Rand, as []atom) string {
	var b strings.Builder
	for _, a := range as {
		switch {
		case a.escape:
			b.WriteString(a.text)
		case a.browserEscapes && r.Intn(2) == 0:
			b.WriteString(a.form)
		default:
			b.WriteString(a.text)
		}
	}
	return b.String()
}

func (h host) spell(r *rand.Rand) string {
	switch {
	case h.ipv4 != nil:
		return fmt.Sprintf("%d.%d.%d.%d", h.ipv4[0], h.ipv4[1], h.ipv4[2], h.ipv4[3])
	case h.ipv6 != nil:
		groups := make([]string, 8)
		for i, g := range h.ipv6 {
			s := strconv.FormatUint(uint64(g), 16)
			if n := r.Intn(5 - len(s)); n > 0 {
				s = strings.Repeat("0", n) + s
			}
			groups[i] = randomCase(r, s)
		}
		// "::" may stand for any run of zero groups.
		var runs [][2]int
		for i := range 8 {
			for j := i; j < 8 && h.ipv6[j] == 0; j++ {
				runs = append(runs, [2]int{i, j + 1})
			}
		}
		if len(runs) > 0 && r.Intn(3) != 0 {
			run := runs[r.Intn(len(runs))]
			return "[" + strings.Join(groups[:run[0]], ":") + "::" + strings.Join(groups[run[1]:], ":") + "]"
		}
		return "[" + strings.Join(groups, ":") + "]"
	}
	return randomCase(r, h.name)
}

func randomCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i := range b {
		if r.Intn(2) == 0 {
			b[i] = strings.ToUpper(string(b[i]))[0]
		}
	}
	return string(b)
}

func pick(r *rand.Rand, xs ...string) string { return xs[r.Intn(len(xs))] }

func randomAtoms(r *rand.Rand, pool []atom, min int) []atom {
	as := []atom{}
	for range min + r.Intn(3) {
		as = append(as, pool[r.Intn(len(pool))])
	}
	return as
}

func randomHost(r *rand.Rand) host {
	switch r.Intn(3) {
	case 0:
		var a [4]byte
		for i := range a {
			a[i] = []byte{0, 1, 2, 10, 127, 255}[r.Intn(6)]
		}
		return host{ipv4: &a}
	case 1:
		var a [8]uint16
		for i := range a {
			// Mostly zeros and small values, so that runs of zero groups
			// and equal addresses come up.
			a[i] = []uint16{0, 0, 0, 1, 2, 0xdb8, 0xffff, 0xabcd}[r.Intn(8)]
		}
		if a[0]|a[1]|a[2]|a[3]|a[4] == 0 && a[5] == 0xffff {
			a[4] = 1 // an IPv4-mapped address has no certain browser form
		}
		return host{ipv6: &a}
	}
	return host{name: pick(r, "localhost", "app.test", "a-b.example", "x1.y2.test")}
}

func randomParts(r *rand.Rand) urlParts {
	p := urlParts{https: r.Intn(2) == 0, host: randomHost(r)}
	if r.Intn(2) == 0 {
		p.port = pick(r, "8791", "18080", "8443", "1")
	}
	for range r.Intn(3) {
		p.segments = append(p.segments, randomAtoms(r, pathAtoms, 1))
	}
	if r.Intn(2) == 0 {
		p.query = randomAtoms(r, queryAtoms, 0)
	}
	if r.Intn(3) == 0 {
		p.fragment = randomAtoms(r, fragmentAtoms, 0)
	}
	return p
}

// recased is p with the escapes of the URL in a random hex case, each
// still written as it is: a URL that differs from p at most in the hex case
// of its escapes.
func (p urlParts) recased(r *rand.Rand) urlParts {
	re := func(as []atom) []atom {
		if as == nil {
			return nil
		}
		out := make([]atom, len(as))
		for i, a := range as {
			if a.escape {
				a = esc(randomCase(r, strings.ToLower(a.text)))
			}
			out[i] = a
		}
		return out
	}
	q := p
	q.segments = make([][]atom, len(p.segments))
	for i, s := range p.segments {
		q.segments[i] = re(s)
	}
	q.query, q.fragment = re(p.query), re(p.fragment)
	return q
}

// Generate makes pairs of URLs that share their parts, differ in one
// part, differ at most in the hex case of their escapes, or are drawn
// apart.
func (urlPair) Generate(r *rand.Rand, _ int) reflect.Value {
	a := randomParts(r)
	b, other := a, randomParts(r)
	switch r.Intn(4) {
	case 0:
		switch r.Intn(6) {
		case 0:
			b.https = !a.https
		case 1:
			b.host = other.host
		case 2:
			b.port = other.port
		case 3:
			b.segments = other.segments
		case 4:
			b.query = other.query
		default:
			b.fragment = other.fragment
		}
	case 1:
		b = other
	case 2:
		b = a.recased(r)
	}
	return reflect.ValueOf(urlPair{a, b, r.Int63()})
}

type urlPair struct {
	a, b urlParts
	seed int64
}

// Review F13 and F7: sim up takes a listed URL for its --url exactly when
// a browser writes both alike: generated pairs of URLs (host names, plain
// IPv4 and IPv6 addresses; paths, queries and fragments with characters
// outside ASCII, characters that a browser escapes, and escapes), spelled
// in forms that a browser reads alike (case, IPv6 notation, default
// ports, dot segments, an empty path, a character or the browser's escape
// of it), match exactly when their browser forms, built from the parts,
// are equal; the query and the fragment, also an empty one, count, and so
// does the hex case of an escape that the URL has (final review F3).
func TestSameURLProperty(t *testing.T) {
	prop := func(pair urlPair) bool {
		r := rand.New(rand.NewSource(pair.seed))
		listed, target := pair.a.spell(r), pair.b.spell(r)
		want := pair.a.key() == pair.b.key()
		if got := sameURL(listed, target); got != want {
			t.Errorf("sameURL(%q, %q) = %t; the parts say %t", listed, target, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261124))}); err != nil {
		t.Fatal(err)
	}
}
