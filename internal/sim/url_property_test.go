package sim

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// urlParts are the parts of a generated URL; the oracle compares them,
// not text.
type urlParts struct {
	https    bool
	host     string   // lower case
	port     string   // "" or a port that is not the scheme's default
	segments []string // path segments, without dot segments
	query    *string  // nil: no "?"
	fragment *string  // nil: no "#"
}

// key is the URL as a browser writes it, from the parts alone: what sim up
// and tab find compare.
func (p urlParts) key() string {
	q, f := "-", "-"
	if p.query != nil {
		q = "?" + *p.query
	}
	if p.fragment != nil {
		f = "#" + *p.fragment
	}
	return fmt.Sprintf("%t|%s|%s|/%s|%s|%s", p.https, p.host, p.port, strings.Join(p.segments, "/"), q, f)
}

// spell writes p in one of the forms that a browser reads as p: the
// scheme and host in any case, the default port written or not (with
// leading zeros), "." and "x/.." segments before a segment, and an empty
// path as "" or "/".
func (p urlParts) spell(r *rand.Rand) string {
	var b strings.Builder
	scheme, def := "http", "80"
	if p.https {
		scheme, def = "https", "443"
	}
	b.WriteString(randomCase(r, scheme) + "://" + randomCase(r, p.host))
	switch {
	case p.port != "":
		b.WriteString(":" + p.port)
	case r.Intn(3) == 0:
		b.WriteString(":" + strings.Repeat("0", r.Intn(2)) + def)
	}
	for _, s := range p.segments {
		if r.Intn(4) == 0 {
			b.WriteString([]string{"/.", "/zz/.."}[r.Intn(2)])
		}
		b.WriteString("/" + s)
	}
	if len(p.segments) == 0 && r.Intn(2) == 0 {
		b.WriteString("/")
	}
	if p.query != nil {
		b.WriteString("?" + *p.query)
	}
	if p.fragment != nil {
		b.WriteString("#" + *p.fragment)
	}
	return b.String()
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

func randomParts(r *rand.Rand) urlParts {
	p := urlParts{https: r.Intn(2) == 0, host: pick(r, "10.0.2.2", "localhost", "app.test", "a-b.example")}
	if r.Intn(2) == 0 {
		p.port = pick(r, "8791", "18080", "8443")
	}
	for range r.Intn(3) {
		p.segments = append(p.segments, pick(r, "a", "b", "index.html", "x~y", "%41"))
	}
	if r.Intn(2) == 0 {
		q := pick(r, "", "x=1", "x=2", "a=b&c")
		p.query = &q
	}
	if r.Intn(3) == 0 {
		f := pick(r, "", "top", "k")
		p.fragment = &f
	}
	return p
}

// Generate makes pairs of URLs that share their parts, differ in one
// part, or are drawn apart.
func (urlPair) Generate(r *rand.Rand, _ int) reflect.Value {
	a := randomParts(r)
	b, other := a, randomParts(r)
	switch r.Intn(3) {
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
	}
	return reflect.ValueOf(urlPair{a, b, r.Int63()})
}

type urlPair struct {
	a, b urlParts
	seed int64
}

// Review F13 and F7: sim up takes a listed URL for its --url exactly when
// a browser writes both alike: generated pairs of URLs, spelled in forms
// that a browser reads alike (case, default ports, dot segments, an empty
// path), match exactly when their parts are equal; the query and the
// fragment, also an empty one, count.
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
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261124))}); err != nil {
		t.Fatal(err)
	}
}
