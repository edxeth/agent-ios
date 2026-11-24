package sim

import (
	"context"
	"testing"
)

// Review F7: sim up takes a listed URL for its --url only when both are
// the same URL as a browser writes it (WHATWG URL serialization): a
// different IPv6 address, a path of other characters, or an escape of a
// letter is another URL, while a space and its escape in a query are the
// same one. Final review F3: the browser keeps the hex case of an escape
// that the URL has, so "%2f" and "%2F" are different URLs. The expected
// values come from the reviews' cases and from WHATWG URL (new
// URL(text).href, Node 24).
func TestSameURLIsBrowserEquality(t *testing.T) {
	for _, c := range []struct {
		page, target string
		want         bool
	}{
		{"http://[::2]:8791/a", "http://[::1]:8791/a", false},
		{"http://[::1]:8791/a", "http://[0:0:0:0:0:0:0:1]:8791/a", true},
		{"http://[2001:db8::1]/", "http://[2001:DB8:0::0:1]:80/", true},
		{"http://site.test/unrelated", "http://site.test/€", false},
		{"http://site.test/%E2%82%AC", "http://site.test/€", true},
		{"http://site.test/%e2%82%ac", "http://site.test/€", false},
		{"http://site.test/%2F", "http://site.test/%2f", false},
		{"http://site.test/?q=%2B", "http://site.test/?q=%2b", false},
		{"http://site.test/#%AB", "http://site.test/#%ab", false},
		{"http://site.test/%3c", "http://site.test/<", false},
		{"http://site.test/%2f", "http://site.test/%2f", true},
		{"http://site.test/%41", "http://site.test/A", false},
		{"http://site.test/?q=hello%20world", "http://site.test/?q=hello world", true},
		{"http://site.test/?q=a+b", "http://site.test/?q=a b", false},
		{"http://site.test/#a%20b", "http://site.test/#a b", true},
		{"http://site.test/#%60", "http://site.test/#`", true},
		{"http://site.test/a%20b", "http://site.test/a b", true},
		{"http://site.test/a/", "http://site.test/a/b/..", true},
		{"http://site.test/a/", "http://site.test/a/b/%2E%2e", true},
		{"http://site.test/", "http://site.test", true},
		{"http://site.test/", "HTTP://SITE.test:080/", true},
		{"http://site.test/", "http://site.test/?", false},
		{"http://site.test/", "http://site.test/#", false},
	} {
		if got := sameURL(c.page, c.target); got != c.want {
			t.Errorf("sameURL(%q, %q) = %t, want %t", c.page, c.target, got, c.want)
		}
	}
}

// Review F7: a URL whose browser form is not certain from its text (user
// info, an IDNA host, an IPv4 address in another form than dotted
// decimal, an IPv4-mapped or scoped IPv6 address, a malformed escape, a
// character whose escaping browsers differ on, another scheme) never
// matches, not even itself.
func TestSameURLRefusesUncertainURLs(t *testing.T) {
	for _, u := range []string{
		"http://user@site.test/",
		"http://user:pw@site.test/",
		"http://xn--nxasmq6b.test/",
		"http://ünï.test/",
		"http://127.1/",
		"http://0x7f.0.0.1/",
		"http://2130706433/",
		"http://127.000.0.1/",
		"http://1.2.3.4.5/",
		"http://site.123/",
		"http://[::ffff:1.2.3.4]/",
		"http://[::ffff:102:304]/",
		"http://[::1.2.3.4]/",
		"http://[fe80::1%25eth0]/",
		"http://site.test./",
		"http://site..test/",
		"http://si_te.test/",
		"http://site.test:99999/",
		"http://site.test:8x/",
		"http://site.test/%4",
		"http://site.test/%zz",
		"http://site.test/a^b",
		"http://site.test/a|b",
		"http://site.test/a\\b",
		"http://site.test/?a'b",
		"http://site.test/?a`b",
		"http://site.test/#a#b",
		"http://site.test/a\x01b",
		"http:/site.test/",
		"http:///site.test/",
		"ftp://site.test/",
		"http://site.test/\xff",
	} {
		if sameURL(u, u) {
			t.Errorf("sameURL(%q, itself) = true, want false: its browser form is not certain", u)
		}
	}
}

// Review F18: Up names the new tab of its --url (NewPage, the page id that
// iwdp lists), also beside an earlier tab of the same URL, so that the
// command can print its tab id; with two new tabs of the URL it names
// none, since either can be the one that it opened.
func TestUpNamesTheNewTabOfTheURL(t *testing.T) {
	const u = "http://10.0.2.2:8791/"
	fm := readyMac()
	iw := &fakeIWDP{urls: []string{u}}
	fm.opened = func() { iw.urls = append(iw.urls, "http://other.test/", u) }
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if r, err := s.Up(context.Background(), "", u, false); err != nil || r.NewPage != "p2" {
		t.Fatalf("Up beside an earlier tab of the URL: NewPage %q, %v; want the new tab p2", r.NewPage, err)
	}
	iw.urls = []string{"http://other.test/"}
	fm.opened = func() { iw.urls = append(iw.urls, u, u) }
	if r, err := s.Up(context.Background(), "", u, false); err != nil || !r.TabOpened || r.NewPage != "" {
		t.Fatalf("Up with two new tabs of the URL: %+v, %v; want TabOpened and no NewPage", r, err)
	}
}

// Final review F6: Up says when the new tab of its --url is not the only
// tab of that URL (an earlier tab, or a second new one), where tab find
// would find several; a URL that only the new tab shows is not shared.
func TestUpSaysWhenAnotherTabSharesTheURL(t *testing.T) {
	const u = "http://10.0.2.2:8791/"
	for _, c := range []struct {
		name          string
		before, added []string
		want          bool
	}{
		{"earlier tab of the URL", []string{u}, []string{u}, true},
		{"two new tabs of the URL", []string{"http://other.test/"}, []string{u, u}, true},
		{"only the new tab", []string{"http://other.test/"}, []string{"http://third.test/", u}, false},
	} {
		fm := readyMac()
		iw := &fakeIWDP{urls: c.before}
		fm.opened = func() { iw.urls = append(iw.urls, c.added...) }
		s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
		r, err := s.Up(context.Background(), "", u, false)
		if err != nil || !r.TabOpened || r.SharedURL != c.want {
			t.Errorf("%s: %+v, %v; want TabOpened and SharedURL %t", c.name, r, err, c.want)
		}
	}
}

// Final review F3: Up does not take a new tab whose URL differs from its
// --url only in the hex case of an escape: the browser lists another URL.
func TestUpRefusesANewTabOfAnotherEscapeCase(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{urls: []string{"http://old.test/"}}
	fm.opened = func() { iw.urls = append(iw.urls, "http://site.test/?q=%2F") }
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if r, err := s.Up(context.Background(), "", "http://site.test/?q=%2f", false); err == nil {
		t.Fatalf("Up took a tab of http://site.test/?q=%%2F for http://site.test/?q=%%2f: %+v", r)
	}
}

// Review F7: Up does not take a new tab of another IPv6 host for the tab
// of its --url.
func TestUpRefusesANewTabOfAnotherIPv6Host(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{urls: []string{"http://old.test/"}}
	fm.opened = func() { iw.urls = append(iw.urls, "http://[::2]:8791/a") }
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if r, err := s.Up(context.Background(), "", "http://[::1]:8791/a", false); err == nil {
		t.Fatalf("Up took a tab of http://[::2]:8791/a for http://[::1]:8791/a: %+v", r)
	}
}
