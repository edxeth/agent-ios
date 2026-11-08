//go:build webkitgtk

package webkitgtk

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// cookieHeader is the sorted name=value pairs of a request's Cookie header.
func cookieHeader(r seen) []string {
	var out []string
	for _, p := range strings.Split(r.header.Get("Cookie"), ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// What WebKit stores for a cookie, which tools.SetCookieVerified relies on
// to confirm the cookie it set (flakes review F1). Set-Cookie without a
// Domain attribute makes a host-only cookie whose domain is the host
// ("app.localhost"); with one, a domain cookie whose domain has a leading
// dot (".app.localhost"), whether the attribute had the dot or not.
// Page.setCookie stores the domain as given: "app.localhost" is a
// host-only cookie and ".app.localhost" a domain cookie, and both live in
// the jar with the same name and path. The host gets both, its subdomain
// only the domain cookies. "cookies set" verifies each, and "state save"
// and "state load" keep them apart. The page runs on app.localhost in a
// browser of its own: a domain cookie needs a host name, and the suite's
// shared page must not change site.
func TestDevtoolsCookieIdentityIsStoredAsSet(t *testing.T) {
	s := newSite(t, "app.localhost")
	s.html("/page.html", "<!doctype html><title>cookies</title>")
	u, err := url.Parse(s.url)
	if err != nil {
		t.Fatal(err)
	}
	sub := "http://sub.app.localhost:" + u.Port()
	devtoolsBrowser(t, s.url+"/page.html")

	js(t, `document.cookie = "plain=1"; document.cookie = "attr=2; domain=app.localhost"; document.cookie = "attrdot=3; domain=.app.localhost"; "ok"`)
	for _, c := range [][2]string{{"host", "app.localhost"}, {"dom", ".app.localhost"}} {
		out := mustCLI(t, "cookies", "set", "pc", c[0], "--domain", c[1])
		if want := "Cookie pc set and verified (domain " + c[1] + ", path /)."; strings.TrimSpace(out) != want {
			t.Fatalf("cookies set pc %s --domain %s: %q, want %q", c[0], c[1], out, want)
		}
	}
	// state save lists the cookies that the browser sends to the page's
	// host, as Page.getCookies reports them.
	dir := t.TempDir()
	saved, again := filepath.Join(dir, "saved.json"), filepath.Join(dir, "again.json")
	read := func(path string) []devtools.StateCookie {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := devtools.DecodeState(data)
		if err != nil {
			t.Fatal(err)
		}
		return st.Cookies
	}
	mustCLI(t, "state", "save", saved)
	var stored []string
	for _, c := range read(saved) {
		stored = append(stored, c.Name+"="+c.Value+" "+c.Domain)
	}
	sort.Strings(stored)
	if want := []string{"attr=2 .app.localhost", "attrdot=3 .app.localhost", "pc=dom .app.localhost", "pc=host app.localhost", "plain=1 app.localhost"}; !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored cookies %q, want %q", stored, want)
	}

	js(t, fmt.Sprintf(`fetch(%q, {credentials: "include"}); fetch(%q, {credentials: "include", mode: "no-cors"}); "ok"`, s.url+"/host", sub+"/sub"))
	if got, want := cookieHeader(s.waitGot(t, "/host")), []string{"attr=2", "attrdot=3", "pc=dom", "pc=host", "plain=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cookies sent to app.localhost %q, want %q", got, want)
	}
	if got, want := cookieHeader(s.waitGot(t, "/sub")), []string{"attr=2", "attrdot=3", "pc=dom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cookies sent to sub.app.localhost %q, want %q (the domain cookies only)", got, want)
	}

	mustCLI(t, "cookies", "clear")
	mustCLI(t, "state", "load", saved)
	mustCLI(t, "state", "save", again)
	if before, after := read(saved), read(again); len(before) != 5 || !reflect.DeepEqual(after, before) {
		t.Fatalf("cookies after state load %+v, want %+v", after, before)
	}
}
