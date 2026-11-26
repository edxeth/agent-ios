package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of tab_find.go and of --only-tab (core.Manager.OnlyTab) over fake
// pages.

// addAt lists a page at url whose document answers the comparison of
// tab find as a browser does: true when the target's href is its own.
// reads counts those comparisons. With fail, the page refuses the read.
func addAt(f *tabFake, title, url string, reads *atomic.Int32, fail bool) {
	s := f.add(title, url)
	s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		lit, ok := strings.CutPrefix(p.Expression, "location.href === new URL(")
		if !ok {
			return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
		}
		reads.Add(1)
		if fail {
			return nil, &fakewebkit.Error{Code: -32000, Message: "the page " + privateMarker + " refused"}
		}
		var target string
		_ = json.Unmarshal([]byte(strings.TrimSuffix(lit, ").href")), &target)
		return map[string]any{"result": map[string]any{"type": "boolean", "value": href(target) == href(url)}}, nil
	})
}

// browserHrefs holds the URLs of these tests that a browser writes in
// another form, each with the href that node 26 printed for it
// (new URL(raw).href). The keys are the targets as the page receives them
// (tab find sends the target as the caller wrote it). Every other URL of
// these tests is its own href.
var browserHrefs = map[string]string{
	"https://app.test:443/a/b?x=1#k":      "https://app.test/a/b?x=1#k",
	"https://app.test:0443/a/b?x=1#k":     "https://app.test/a/b?x=1#k",
	"HTTPS://APP.TEST/a/b?x=1#k":          "https://app.test/a/b?x=1#k",
	"https://app.test/a/./c/../b?x=1#k":   "https://app.test/a/b?x=1#k",
	"https://app.test/a/%2e%2e/a/b?x=1#k": "https://app.test/a/b?x=1#k",
	"https://app.test/a/%2E/b?x=1#k":      "https://app.test/a/b?x=1#k",
	"https://app.test/c/.%2E/a/b?x=1#k":   "https://app.test/a/b?x=1#k",
	"https://BÜCHER.example/a/b?x=1#k":    "https://xn--bcher-kva.example/a/b?x=1#k",
	"https://app.test?x=1#k":              "https://app.test/?x=1#k",
	`https://app.test/a\b?x=1`:            "https://app.test/a/b?x=1",
}

// href is a URL as the parser of a browser writes it (browserHrefs).
func href(raw string) string {
	if h, ok := browserHrefs[raw]; ok {
		return h
	}
	return raw
}

func leasedTitles(m *core.Manager, session string) []string {
	var titles []string
	for _, p := range sessionPages(m, session) {
		titles = append(titles, p.Title)
	}
	return titles
}

// The listing shows no query and no fragment, so only the read inside the
// page tells the target from another state of the same path. tab find
// prints the id of the one tab that shows the target, reads no tab of
// another path, and leaves no lease on a tab that does not show it.
func TestTabFindPrintsTheTabThatShowsTheWholeURL(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	target := "https://app.test/reset?sig=NEW#k=1"
	addAt(f, "Old state", "https://app.test/reset?sig=OLD#k=1", &reads, false)
	addAt(f, "Target", target, &reads, false)
	addAt(f, "Other path", "https://app.test/account?sig=NEW#k=1", &reads, false)
	res, err := runIn(t, m, "a", "tab", "find", target)
	if err != nil || res.Text != "t2" {
		t.Fatalf("tab find: %q, %v; want t2", res.Text, err)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("tab find read %d tabs; want the 2 tabs of the target's path", got)
	}
	if got := leasedTitles(m, "a"); len(got) != 1 || got[0] != "Target" {
		t.Errorf("after tab find session a holds %v; want only the target", got)
	}
	// It selects nothing: the session still has no current page.
	if _, ok := m.CurrentPage("a"); ok {
		t.Error("tab find selected a tab for session a")
	}
}

func TestTabFindReportsNoTabAndSeveralTabs(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "One", "https://app.test/a?x=1", &reads, false)
	addAt(f, "Two", "https://app.test/a?x=1", &reads, false)
	_, err := runIn(t, m, "a", "tab", "find", "https://app.test/a?x=2")
	if err == nil || err.Error() != "no tab shows the target" || ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("another query: %v (exit %d); want no tab, exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	if got := leasedTitles(m, "a"); len(got) != 0 {
		t.Errorf("after a tab find without a match session a holds %v", got)
	}
	// Several tabs are not "no tab": exit 1, and no lease stays on either.
	_, err = runIn(t, m, "a", "tab", "find", "https://app.test/a?x=1")
	if err == nil || err.Error() != "2 tabs show the target: t1, t2" || ExitCodeOf(err) != ExitError {
		t.Fatalf("two tabs at the target: %v (exit %d)", err, ExitCodeOf(err))
	}
	if got := leasedTitles(m, "a"); len(got) != 0 {
		t.Errorf("after a tab find with two matches session a holds %v", got)
	}
	_, err = runIn(t, m, "a", "tab", "find", "https://elsewhere.test/a?x=1")
	if ExitCodeOf(err) != ExitNoMatch || reads.Load() != 4 {
		t.Fatalf("another host: %v after %d reads; want no tab (exit %d) and no further read", err, reads.Load(), ExitNoMatch)
	}
}

// A tab that cannot be read is not a tab that shows another page: the
// error is the read's, with its exit code, never "no tab shows the
// target".
func TestTabFindDoesNotTakeAFailedReadForAMismatch(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	target := "https://app.test/a?x=1"
	addAt(f, "Refuses", target, &reads, true)
	_, err := runIn(t, m, "a", "tab", "find", target)
	if err == nil || !strings.Contains(err.Error(), "reading tab t1") || strings.Contains(err.Error(), "no tab shows") || ExitCodeOf(err) == ExitNoMatch {
		t.Fatalf("a refused read: %v (exit %d); want the error of the read, never exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	// The read took a lease; the failed command leaves none (review of
	// 2026-11-13, F11).
	if got := leasedTitles(m, "a"); len(got) != 0 {
		t.Errorf("after a failed read session a holds %v", got)
	}
	// A page that another session holds: exit 4, as for any page command.
	m2, f2 := newTabManager(t)
	addAt(f2, "Held", target, &reads, false)
	if _, err := runIn(t, m2, "b", "tab", "t1"); err != nil {
		t.Fatal(err)
	}
	_, err = runIn(t, m2, "a", "tab", "find", target)
	if !errors.Is(err, core.ErrLeased) || ExitCodeOf(err) != ExitBusy {
		t.Fatalf("a tab that session b holds: %v (exit %d); want the lease error, exit 4", err, ExitCodeOf(err))
	}
}

// A command that fails keeps no lease that it took, also not that of a tab
// that it found before the failure: here the first tab shows the target
// and the second cannot be read (review of 2026-11-13, F18: one place ends
// the leases for every outcome).
func TestTabFindThatFailsKeepsNoLeaseItTook(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	target := "https://app.test/a?x=1"
	addAt(f, "Target", target, &reads, false)
	addAt(f, "Refuses", target, &reads, true)
	_, err := runIn(t, m, "a", "tab", "find", target)
	if err == nil || !strings.Contains(err.Error(), "reading tab t2") || ExitCodeOf(err) == ExitNoMatch {
		t.Fatalf("a match, then a refused read: %v (exit %d); want the error of the read", err, ExitCodeOf(err))
	}
	if got := leasedTitles(m, "a"); len(got) != 0 {
		t.Errorf("after the failed command session a holds %v; want no lease", got)
	}
}

// A lease that the session held before tab find stays, whatever the
// command finds.
func TestTabFindKeepsTheLeasesThatTheSessionHeld(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "Mine", "https://app.test/a?x=1", &reads, false)
	if _, err := runIn(t, m, "a", "tab", "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "a", "tab", "find", "https://app.test/a?x=2"); ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("tab find: %v (exit %d), want exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	if got := leasedTitles(m, "a"); len(got) != 1 || got[0] != "Mine" {
		t.Errorf("after tab find session a holds %v; want the page it held before", got)
	}
}

// A target file can hold a form of the URL that a browser writes in
// another way. The tab that shows it is found (review of 2026-11-13, F10
// and F31). A host with a trailing dot is another host for a browser
// (node: new URL("https://app.test./").href keeps the dot), so that form
// finds no tab.
func TestTabFindTakesTheFormsOfOneURL(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "Target", "https://app.test/a/b?x=1#k", &reads, false)
	addAt(f, "Books", "https://xn--bcher-kva.example/a/b?x=1#k", &reads, false)
	addAt(f, "Root", "https://app.test/?x=1#k", &reads, false)
	// A browser keeps "|" in a path, and net/url writes it as "%7C": the
	// page must get the target as the caller wrote it.
	addAt(f, "Pipe", "https://app.test/p|q?x=1#k", &reads, false)
	for target, want := range map[string]string{
		"https://app.test:443/a/b?x=1#k":      "t1",
		"https://app.test:0443/a/b?x=1#k":     "t1",
		"HTTPS://APP.TEST/a/b?x=1#k":          "t1",
		"https://app.test/a/./c/../b?x=1#k":   "t1",
		"https://app.test/a/%2e%2e/a/b?x=1#k": "t1",
		"https://app.test/a/%2E/b?x=1#k":      "t1",
		"https://app.test/c/.%2E/a/b?x=1#k":   "t1",
		"https://BÜCHER.example/a/b?x=1#k":    "t2",
		"https://app.test?x=1#k":              "t3",
		"https://app.test/p|q?x=1#k":          "t4",
	} {
		if res, err := runIn(t, m, "a", "tab", "find", target); err != nil || res.Text != want {
			t.Errorf("tab find %s: %q, %v; want %s", target, res.Text, err, want)
		}
	}
	for _, target := range []string{"https://app.test:8443/a/b?x=1#k", "http://app.test/a/b?x=1#k", "https://app.test/a/b/?x=1#k", "https://app.test./a/b?x=1#k"} {
		if _, err := runIn(t, m, "a", "tab", "find", target); ExitCodeOf(err) != ExitNoMatch {
			t.Errorf("tab find %s: %v (exit %d); want no tab", target, err, ExitCodeOf(err))
		}
	}
}

// A listed URL is never excluded for a target that a browser writes as
// that URL, whatever the target's spelling: a default port, also with
// leading zeros or empty, upper case, "." and ".." also percent-encoded, a
// host that needs IDNA, an empty path, and the spellings that only a
// browser reads (WHATWG URL): a backslash for any slash of an http(s)
// URL, also before the host, more or fewer slashes before the host, a tab
// or a line break anywhere, and a C0 control or a space at either end.
// node 26 writes each of these forms as the listed URL; a host with a
// trailing dot, which is another host for a browser, is not excluded
// either (browserHost). Another host, port or path in the safe form is
// excluded; a host that needs IDNA is not in the safe form and excludes
// nothing (tab_find.go).
func TestDocumentKeyProperty(t *testing.T) {
	label := func(n uint8) string { return string(rune('a'+n%26)) + "x" }
	// idnHosts are host names that need IDNA, each with the host that node
	// 26 prints for it (new URL("https://" + name + "/").host).
	idnHosts := [][2]string{
		{"bücher.example", "xn--bcher-kva.example"},
		{"münchen.test", "xn--mnchen-3ya.test"},
		{"äpfel.test", "xn--pfel-koa.test"},
	}
	dots := []string{".", "%2e", "%2E"} // the spellings of one dot of a dot segment
	property := func(h, p1, p2, other, d1, d2, zeros, idn, pos, ws, c0 uint8, tls, useIDN bool) bool {
		scheme, port := "http", "80"
		if tls {
			scheme, port = "https", "443"
		}
		host, listedHost := label(h)+".test", label(h)+".test"
		if useIDN {
			pair := idnHosts[int(idn)%len(idnHosts)]
			host, listedHost = pair[0], pair[1]
		}
		path := "/" + label(p1) + "/" + label(p2)
		listed := scheme + "://" + listedHost + path // as a browser writes it
		dot, dotdot := dots[d1%3], dots[d1%3]+dots[d2%3]
		check := func(form string, want bool) bool {
			if sameDocument(listed, form) != want {
				t.Logf("sameDocument(%q, %q) = %v", listed, form, !want)
				return false
			}
			return true
		}
		plain := scheme + "://" + host + path + "?x=1"
		runes := []rune(plain)
		at := int(pos) % (len(runes) + 1) // a break between two characters
		edge := string(rune(c0 % 0x21))   // a C0 control or a space
		for _, form := range []string{
			scheme + "://" + host + path,
			scheme + "://" + host + ":" + strings.Repeat("0", int(zeros%4)) + port + path,
			scheme + "://" + host + ":" + path,
			strings.ToUpper(scheme) + "://" + strings.ToUpper(host) + path,
			scheme + "://" + host + "." + path,
			scheme + "://" + host + "/" + label(p1) + "/" + dot + "/q/" + dotdot + "/" + label(p2),
			scheme + "://" + host + "/q/" + dotdot + path + "?x=1",
			strings.ReplaceAll(plain, "/", "\\"),
			scheme + "://" + host + "\\" + label(p1) + "/" + label(p2),
			scheme + ":/\\/" + host + path,
			scheme + ":" + strings.Repeat("/", int(zeros%4)) + host + path,
			string(runes[:at]) + string("\t\n\r"[ws%3]) + string(runes[at:]),
			edge + plain,
			plain + edge,
		} {
			if !check(form, true) {
				return false
			}
		}
		excluded := []string{
			scheme + "://" + host + ":" + strings.Repeat("0", int(zeros%4)) + "8443" + path,
			scheme + "://" + host + path + "/more",
			// A double-encoded dot is no dot segment.
			scheme + "://" + host + "/q/%252e%252e" + path,
			// A backslash in the query changes neither host nor path.
			scheme + "://" + host + path + "/more?q=\\",
		}
		if !useIDN {
			// Another letter: the sum stays below 26 steps from h's letter.
			excluded = append(excluded, scheme+"://"+label(uint8((int(h)%26+1+int(other)%25)%26))+".test"+path)
		}
		for _, form := range excluded {
			if !check(form, false) {
				return false
			}
		}
		listed = scheme + "://" + listedHost + "/"
		return check(scheme+"://"+host, true) && check(scheme+"://"+host+"?x=1", true)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
}

// documentOf is an href without its query and fragment: what a listing
// shows of it.
func documentOf(href string) string {
	href, _, _ = strings.Cut(href, "#")
	href, _, _ = strings.Cut(href, "?")
	return href
}

// The table of the filter. href is what node 26 printed for the target
// (node -e 'console.log(new URL(process.argv[1]).href)' "$target"); ""
// when node rejected it (no URL for a browser, so no tab shows it, and
// the read reports the error). A row that the filter excludes must have
// an href whose document is not the listed URL: node gives another
// document. A row whose href shows the listed URL must not be excluded.
// Some listed URLs are not a browser's href (a malformed escape, an empty
// URL): they exclude nothing (review of 2026-11-13, F31 and F36).
func TestDocumentKeyBrowserForms(t *testing.T) {
	for _, tc := range []struct {
		listed, target, href string
		exclude              bool
	}{
		{"https://app.test/b", "https://app.test/a/%2e%2e/b?x=1", "https://app.test/b?x=1", false},
		{"https://app.test/b", "https://app.test/a/%2E%2e/b", "https://app.test/b", false},
		{"https://app.test/b", "https://app.test/a/.%2E/b", "https://app.test/b", false},
		{"https://app.test/a/b", "https://app.test/a/%2e/b", "https://app.test/a/b", false},
		{"https://app.test/", "https://app.test/a/%2e%2e", "https://app.test/", false},
		{"https://app.test/a/", "https://app.test/a/%2e", "https://app.test/a/", false},
		{"https://app.test/b", "https://app.test:0443/b?x=1", "https://app.test/b?x=1", false},
		{"http://app.test/x", "http://app.test:00080/x", "http://app.test/x", false},
		{"https://app.test:0/x", "https://APP.test:00/x", "https://app.test:0/x", false},
		{"https://app.test/", "https://app.test?x=1", "https://app.test/?x=1", false},
		{"https://app.test/a/b", "https://app.test:/a/b", "https://app.test/a/b", false},
		{"https://xn--bcher-kva.example/b", "https://bücher.example/b?x=1", "https://xn--bcher-kva.example/b?x=1", false},
		{"https://xn--bcher-kva.example/", "https://BÜCHER.EXAMPLE/", "https://xn--bcher-kva.example/", false},
		{"https://xn--mnchen-3ya.test/stra%C3%9Fe", "https://münchen.test/straße", "https://xn--mnchen-3ya.test/stra%C3%9Fe", false},
		{"https://xn--a.test/b", "https://xn--A.test/b", "https://xn--a.test/b", false},
		{"https://app.test/%C3%A4", "https://app.test/ä", "https://app.test/%C3%A4", false},
		{"https://app.test/a%20b", "https://app.test/a b", "https://app.test/a%20b", false},
		{"https://app.test/a%5Eb", "https://app.test/a^b", "https://app.test/a%5Eb", false},
		{"https://app.test/a|b", "https://app.test/a|b", "https://app.test/a|b", false},
		{"https://127.0.0.1/", "https://0x7f.1/", "https://127.0.0.1/", false},
		{"https://1.2.0.3/", "https://1.2.3/", "https://1.2.0.3/", false},
		{"https://[::ffff:7f00:1]/", "https://[::FFFF:127.0.0.1]/", "https://[::ffff:7f00:1]/", false},
		{"https://u:p@app.test/a/b", "https://u:p@app.test/a/b", "https://u:p@app.test/a/b", false},
		{"https://a_b.test/", "https://a_b.test/", "https://a_b.test/", false},
		{"foo://h/b", "foo://h/a/%2e%2e/b", "foo://h/b", false},
		// The spellings that only a browser reads (F36).
		{"https://app.test/a/b", "https://app.test/a\\b?x=1", "https://app.test/a/b?x=1", false},
		{"https://app.test/a/b", "https:\\\\app.test\\a\\b", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https://app.test\\a/b", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https:///app.test/a/b", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https:app.test/a/b", "https://app.test/a/b", false},
		{"https://evil.test/@app.test/a/b", "https://evil.test\\@app.test/a/b", "https://evil.test/@app.test/a/b", false},
		{"https://app.test/a/b", " https://app.test/a/b", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https://app.test/a/b ", "https://app.test/a/b", false},
		{"https://app.test/a/b", "\x01https://app.test/a/b\x1f", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https://app.test/a/\tb", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https://app.test/a/b\n", "https://app.test/a/b", false},
		{"https://app.test/a/b", "https://ap\rp.test/a/b", "https://app.test/a/b", false},
		{"https://app.test/a/b", "ht\ntps://app.test/a/b", "https://app.test/a/b", false},
		// A listed URL that is not in the safe form excludes nothing, also
		// for a target that is another document.
		{"https://xn--bcher-kva.example/b", "https://bucher.example/b", "https://bucher.example/b", false},
		{"https://app.test/%zz", "https://app.test/b", "https://app.test/b", false},
		{"", "https://app.test/b", "https://app.test/b", false},
		{"https://app.test/b", "https://app.test:65536/b", "", false},
		// Another document in a part that both have in the safe form.
		{"https://app.test/b", "https://app.test:8443/b", "https://app.test:8443/b", true},
		{"https://app.test/b", "https://app.test:08443/b", "https://app.test:8443/b", true},
		{"https://app.test/b", "http://app.test/b", "http://app.test/b", true},
		{"https://app.test/b", "https://other.test/b", "https://other.test/b", true},
		{"https://app.test/b", "https://app.test/b/", "https://app.test/b/", true},
		{"https://app.test/b", "https://app.test/a/%252e%252e/b", "https://app.test/a/%252e%252e/b", true},
		{"https://app.test/x", "https://app.test:0/x", "https://app.test:0/x", true},
		{"https://127.0.0.1/b", "https://127.0.0.2/b", "https://127.0.0.2/b", true},
		{"https://app.test/a/b", "https://APP.TEST./a/c", "https://app.test./a/c", true},
		{"https://app.test/a/b", "https://bücher.example/a/c", "https://xn--bcher-kva.example/a/c", true},
		{"https://app.test/a/b", "https://evil.test\\@app.test/a/b", "https://evil.test/@app.test/a/b", true},
		{"https://app.test/a/b", "https://other.test/a/b?q=\\", "https://other.test/a/b?q=\\", true},
		{"about:blank", "https://app.test/", "https://app.test/", true},
		{"file:///b", "https://app.test/b", "https://app.test/b", true},
	} {
		if tc.exclude && (tc.href == "" || documentOf(tc.href) == tc.listed) {
			t.Fatalf("table row %q, %q: node does not give another document (%q); the row cannot expect an exclusion", tc.listed, tc.target, tc.href)
		}
		if got := !sameDocument(tc.listed, tc.target); got != tc.exclude {
			t.Errorf("sameDocument(%q, %q) excludes: %v; want %v (node: %q)", tc.listed, tc.target, got, tc.exclude, tc.href)
		}
	}
}

// simulatorAndPhone is an iwdp listing of the SIMULATOR pseudo-device,
// which needs no reservation, and of a phone, which does: each lists the
// pages with its device id.
type simulatorAndPhone struct{ *tabFake }

func (f simulatorAndPhone) Kind() transport.Kind { return transport.KindIWDP }

func (f simulatorAndPhone) Devices(context.Context) ([]transport.Device, error) {
	return []transport.Device{
		{ID: "SIMULATOR", Label: "Simulator", Transport: transport.KindIWDP},
		{ID: "phone", Label: "Phone", Transport: transport.KindIWDP},
	}, nil
}

func (f simulatorAndPhone) Pages(ctx context.Context, device string) ([]transport.Page, error) {
	all, err := f.tabFake.Pages(ctx, device)
	var mine []transport.Page
	for _, p := range all {
		if p.DeviceID == device {
			mine = append(mine, p)
		}
	}
	return mine, err
}

// A page that the listing hides (a page of a physical device without a
// reservation of the caller's owner) can show the target: the command
// proves neither that a listed match is the only one nor that no tab
// shows the target. It fails with the reservation exit code, names no
// page and reads no tab (review of 2026-11-13, F30).
func TestTabFindRefusesAListingThatHidesPages(t *testing.T) {
	for _, visible := range []string{"https://app.test/a?x=1", "https://app.test/a?x=2"} {
		f := &tabFake{t: t}
		m := core.NewManager(core.ManagerOptions{
			Transport: func(context.Context, string, string) (transport.Transport, error) { return simulatorAndPhone{f}, nil },
			Session:   webkit.SessionOptions{TargetWait: time.Second},
		})
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = m.Close(ctx)
		})
		var reads atomic.Int32
		addAt(f, "Visible", visible, &reads, false)
		addAt(f, "Hidden "+privateMarker, "https://app.test/a?x=1", &reads, false)
		f.mu.Lock()
		f.pages[0].DeviceID, f.pages[1].DeviceID = "SIMULATOR", "phone"
		f.mu.Unlock()
		res, err := runIn(t, m, "a", "tab", "find", "https://app.test/a?x=1")
		if err == nil || ExitCodeOf(err) != ExitReservation || strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), "app.test") {
			t.Fatalf("visible tab at %s and a hidden page at the target: %q, %v (exit %d); want exit %d without a page", visible, res.Text, err, ExitCodeOf(err), ExitReservation)
		}
		if reads.Load() != 0 || len(leasedTitles(m, "a")) != 0 {
			t.Errorf("visible tab at %s: %d reads, leases %v; want none", visible, reads.Load(), leasedTitles(m, "a"))
		}
	}
}

// A tab that the listing does not exclude and that cannot be read (not
// debuggable) can show the target: the command ends with an error that is
// neither exit 7 nor a match, before it reads a tab. A tab that is not
// debuggable and shows another document changes nothing (review of
// 2026-11-13, F30).
func TestTabFindStopsAtATabThatCannotBeDebugged(t *testing.T) {
	target := "https://app.test/a?x=1"
	for name, urls := range map[string][]string{
		"alone":         {target},
		"after a match": {target, target},
	} {
		m, f := newTabManager(t)
		var reads atomic.Int32
		for i, u := range urls {
			addAt(f, "Tab", u, &reads, false)
			f.mu.Lock()
			f.pages[i].Debuggable = i != len(urls)-1
			f.mu.Unlock()
		}
		res, err := runIn(t, m, "a", "tab", "find", target)
		id := "t" + strconv.Itoa(len(urls))
		if err == nil || ExitCodeOf(err) != ExitError || !strings.Contains(err.Error(), "tab "+id+" ") {
			t.Fatalf("%s: %q, %v (exit %d); want exit %d that names %s", name, res.Text, err, ExitCodeOf(err), ExitError, id)
		}
		if reads.Load() != 0 || len(leasedTitles(m, "a")) != 0 {
			t.Errorf("%s: %d reads, leases %v; want none", name, reads.Load(), leasedTitles(m, "a"))
		}
	}
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "Target", target, &reads, false)
	addAt(f, "Elsewhere", "https://elsewhere.test/a", &reads, false)
	f.mu.Lock()
	f.pages[1].Debuggable = false
	f.mu.Unlock()
	if res, err := runIn(t, m, "a", "tab", "find", target); err != nil || res.Text != "t1" {
		t.Fatalf("a tab of another site that is not debuggable: %q, %v; want t1", res.Text, err)
	}
}

// blockedListing is a tabFake whose first listing waits until proceed is
// closed, after it closed entered.
type blockedListing struct {
	*tabFake
	first   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
}

func (f *blockedListing) Pages(ctx context.Context, device string) ([]transport.Page, error) {
	if f.first.CompareAndSwap(false, true) {
		close(f.entered)
		select {
		case <-f.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.tabFake.Pages(ctx, device)
}

// tab find ends only the leases that its own reads were granted. Another
// command of the same session that takes the lease of a tab after tab
// find listed the tabs and before tab find reads that tab keeps it, with
// its epoch (review of 2026-11-13, F32).
func TestTabFindReleasesOnlyTheLeasesThatItsReadsWereGranted(t *testing.T) {
	f := &blockedListing{tabFake: &tabFake{t: t}, entered: make(chan struct{}), proceed: make(chan struct{})}
	m := core.NewManager(core.ManagerOptions{
		Transport: func(context.Context, string, string) (transport.Transport, error) { return f, nil },
		Session:   webkit.SessionOptions{TargetWait: time.Second},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	var reads atomic.Int32
	addAt(f.tabFake, "Other state", "https://app.test/a?x=1", &reads, false)
	done := make(chan error, 1)
	go func() {
		_, err := runInCtx(t, context.Background(), m, "a", "tab", "find", "https://app.test/a?x=2")
		done <- err
	}()
	<-f.entered
	if _, err := runIn(t, m, "a", "tab", "t1"); err != nil {
		t.Fatalf("tab t1 while tab find lists: %v", err)
	}
	before := sessionPages(m, "a")
	if len(before) != 1 {
		t.Fatalf("after tab t1 session a holds %v; want t1", before)
	}
	close(f.proceed)
	if err := <-done; ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("tab find: %v (exit %d); want exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	if reads.Load() != 1 {
		t.Errorf("tab find read %d tabs; want 1", reads.Load())
	}
	after := sessionPages(m, "a")
	if len(after) != 1 || after[0].Key != before[0].Key || after[0].Epoch != before[0].Epoch {
		t.Errorf("after tab find session a holds %+v; want the lease of tab t1 with epoch %d", after, before[0].Epoch)
	}
}

// A browser reads a backslash in the path of an http(s) URL as a slash
// (node 26: new URL("https://app.test/a\\b?x=1").href is
// "https://app.test/a/b?x=1"): the tab at that path is read, and the page
// decides (review of 2026-11-13, F36).
func TestTabFindReadsATabThatABackslashPathCanShow(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "Target", "https://app.test/a/b?x=1", &reads, false)
	res, err := runIn(t, m, "a", "tab", "find", `https://app.test/a\b?x=1`)
	if err != nil || res.Text != "t1" {
		t.Fatalf("tab find with a backslash in the path: %q, %v (exit %d) after %d reads; want t1", res.Text, err, ExitCodeOf(err), reads.Load())
	}
}

// blockingPage lists a page at url whose answer to the comparison of tab
// find is false. The first other Runtime.evaluate that it receives after
// that comparison (the page-side release of the lease that the read took)
// closes released and waits until proceed is closed; with readBlocks, the
// comparison itself closes entered and waits until proceed is closed.
func blockingPage(f *tabFake, title, url string, readBlocks bool, entered, proceed chan struct{}) {
	s := f.add(title, url)
	var read atomic.Bool
	var once sync.Once
	s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if strings.HasPrefix(p.Expression, "location.href === new URL(") {
			read.Store(true)
			if readBlocks {
				close(entered)
				<-proceed
			}
			return map[string]any{"result": map[string]any{"type": "boolean", "value": false}}, nil
		}
		if read.Load() && !readBlocks {
			once.Do(func() { close(entered); <-proceed })
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
}

// otherRead is a read of another command of session on the page of tab
// id, as any page command of that session runs it.
func otherRead(t *testing.T, m *core.Manager, session, id string) error {
	t.Helper()
	key, ok := m.TabKey(id)
	if !ok {
		t.Fatalf("no tab %s", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.Do(ctx, core.PageRequest{Page: key}, core.AcquireOptions{Session: session}, "other read", core.ClassRead, false, func(context.Context, *core.ManagedOp) error { return nil })
}

// leaseOn returns the epoch of session's lease on the page titled title,
// or 0.
func leaseOn(m *core.Manager, session, title string) uint64 {
	for _, p := range sessionPages(m, session) {
		if p.Title == title {
			return p.Epoch
		}
	}
	return 0
}

// tab find never ends a lease that another command of the same session
// used after the find's read was granted it: here another read uses the
// lease of tab t1 while tab find reads tab t2 (review of 2026-11-13, F37).
func TestTabFindKeepsALeaseThatAnotherCommandReused(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "first", "https://app.test/a?x=1", &reads, false)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(proceed) }) }
	defer unblock()
	blockingPage(f, "second", "https://app.test/a?x=3", true, entered, proceed)
	done := make(chan error, 1)
	go func() {
		_, err := runInCtx(t, context.Background(), m, "a", "tab", "find", "https://app.test/a?x=2")
		done <- err
	}()
	<-entered
	granted := leaseOn(m, "a", "first")
	if granted == 0 {
		t.Fatal("tab find read tab t1 without a lease")
	}
	if err := otherRead(t, m, "a", "t1"); err != nil {
		t.Fatalf("another read of tab t1: %v", err)
	}
	unblock()
	if err := <-done; ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("tab find: %v (exit %d); want exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	if got := leasedTitles(m, "a"); len(got) != 1 || got[0] != "first" || leaseOn(m, "a", "first") != granted {
		t.Fatalf("after tab find session a holds %v (epoch %d); want the lease of tab t1 that the other read used (epoch %d)", got, leaseOn(m, "a", "first"), granted)
	}
}

// Another command of the same session that starts on a tab while tab
// find ends the lease that its read took there waits for that release and
// runs under a new lease, which stays (review of 2026-11-13, F37).
func TestTabFindKeepsALeaseThatAnotherCommandTakesDuringTheRelease(t *testing.T) {
	m, f := newTabManager(t)
	released, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(proceed) }) }
	defer unblock()
	blockingPage(f, "first", "https://app.test/a?x=1", false, released, proceed)
	done := make(chan error, 1)
	go func() {
		_, err := runInCtx(t, context.Background(), m, "a", "tab", "find", "https://app.test/a?x=2")
		done <- err
	}()
	<-released
	granted := leaseOn(m, "a", "first")
	other := make(chan error, 1)
	go func() { other <- otherRead(t, m, "a", "t1") }()
	unblock()
	if err := <-done; ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("tab find: %v (exit %d); want exit %d", err, ExitCodeOf(err), ExitNoMatch)
	}
	if err := <-other; err != nil {
		t.Fatalf("the read that started during the release: %v", err)
	}
	if now := leaseOn(m, "a", "first"); now == 0 || now == granted {
		t.Fatalf("after tab find session a holds %v, epoch %d on tab t1; want the new lease of the other read (the find's was %d)", leasedTitles(m, "a"), now, granted)
	}
}

func TestTabFindRefusesAURLThatIsNotAbsoluteWithoutRepeatingIt(t *testing.T) {
	m, _ := newTabManager(t)
	var ue *UsageError
	_, err := runIn(t, m, "a", "tab", "find", "/"+privateMarker)
	if !errors.As(err, &ue) || strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("a relative URL: %v; want a usage error without the URL", err)
	}
}

// --only-tab names the one tab of the listing, for a caller that may act
// only when no other page can be meant.
func TestOnlyTabNeedsExactlyOneListedTab(t *testing.T) {
	m, f := newTabManager(t)
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		_, err := m.OnlyTab(ctx, core.PageRequest{}, core.AcquireOptions{Session: "a"})
		if err == nil || !errors.Is(err, core.ErrNoPage) || ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), privateMarker) {
			t.Fatalf("OnlyTab: %v; want exit 5 with %q and no title", err, want)
		}
	}
	check("--only-tab needs a listing of exactly one tab and no hidden page; it shows 0 tab(s) and 0 hidden page(s)")
	f.add("One "+privateMarker, "https://one.test/"+privateMarker)
	key, err := m.OnlyTab(ctx, core.PageRequest{}, core.AcquireOptions{Session: "a"})
	if got, ok := m.TabKey("t1"); err != nil || !ok || key != got {
		t.Fatalf("OnlyTab with one tab: %q, %v; want the key of t1 (%q)", key, err, got)
	}
	f.add("Two "+privateMarker, "https://two.test/")
	check("it shows 2 tab(s) and 0 hidden page(s)")
}

// twoDevices is an iwdp listing of the SIMULATOR pseudo-device, which
// needs no reservation, and of a phone, which does: each lists the pages
// with its device id.
type twoDevices struct{ *tabFake }

func (f twoDevices) Kind() transport.Kind { return transport.KindIWDP }

func (f twoDevices) Devices(context.Context) ([]transport.Device, error) {
	return []transport.Device{
		{ID: "SIMULATOR", Label: "Simulator", Transport: transport.KindIWDP},
		{ID: "phone", Label: "Phone", Transport: transport.KindIWDP},
	}, nil
}

func (f twoDevices) Pages(ctx context.Context, device string) ([]transport.Page, error) {
	all, err := f.tabFake.Pages(ctx, device)
	var mine []transport.Page
	for _, p := range all {
		if p.DeviceID == device {
			mine = append(mine, p)
		}
	}
	return mine, err
}

// One listed tab is not the only page when the listing hides another: a
// page of a physical device without the caller's reservation.
func TestOnlyTabCountsHiddenPages(t *testing.T) {
	f := &tabFake{t: t}
	m := core.NewManager(core.ManagerOptions{
		Transport: func(context.Context, string, string) (transport.Transport, error) { return twoDevices{f}, nil },
		Session:   webkit.SessionOptions{TargetWait: time.Second},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	f.add("Simulator page", "https://sim.test/")
	f.add("Bank "+privateMarker, "https://bank.test/account")
	f.mu.Lock()
	f.pages[0].DeviceID, f.pages[1].DeviceID = "SIMULATOR", "phone"
	f.mu.Unlock()
	_, err := m.OnlyTab(context.Background(), core.PageRequest{}, core.AcquireOptions{Session: "a"})
	if err == nil || ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), "it shows 1 tab(s) and 1 hidden page(s)") || strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("OnlyTab with one listed tab and a page of an unreserved device: %v", err)
	}
}
