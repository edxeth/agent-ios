package core

import (
	"math/rand"
	"net/url"
	"reflect"
	"testing"
	"testing/quick"
)

// pageURLCase is a generated pair of page URLs from a few hosts (loopback
// names and others), ports and paths.
type pageURLCase struct{ A, B string }

// Generate draws both URLs from the same small sets, so equal and
// near-equal pairs are common.
func (pageURLCase) Generate(r *rand.Rand, _ int) reflect.Value {
	hosts := []string{"localhost", "127.0.0.1", "example.test", "10.0.0.2"}
	ports := []string{"9224", "9225", "37071"}
	paths := []string{"/devtools/page/1", "/devtools/page/2", "/x?y=1"}
	one := func() string {
		return "ws://" + hosts[r.Intn(len(hosts))] + ":" + ports[r.Intn(len(ports))] + paths[r.Intn(len(paths))]
	}
	return reflect.ValueOf(pageURLCase{one(), one()})
}

// Property: two page URLs name the same page exactly when they are equal,
// or when both hosts are loopback names (localhost, 127.0.0.1) and port
// and path are equal (iwdp names one page with the Host of each listing
// request); the relation is symmetric.
func TestSamePageURLProperty(t *testing.T) {
	prop := func(c pageURLCase) bool {
		ua, _ := url.Parse(c.A)
		ub, _ := url.Parse(c.B)
		loop := func(u *url.URL) bool { return u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" }
		want := c.A == c.B || (loop(ua) && loop(ub) && ua.Port() == ub.Port() && ua.RequestURI() == ub.RequestURI())
		got, back := samePageURL(c.A, c.B), samePageURL(c.B, c.A)
		if got != want || back != got {
			t.Logf("samePageURL(%s, %s) = %v (reverse %v), want %v", c.A, c.B, got, back, want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
