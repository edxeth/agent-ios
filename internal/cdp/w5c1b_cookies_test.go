package cdp

import (
	"encoding/json"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
	"github.com/nnemirovsky/agent-ios/internal/wip"
)

// jarRig is the rig with a page "Jar" at https://app.example/ whose cookie
// jar has WebKit's semantics (devtoolstest.Page: setCookie replaces the
// cookie of the same name, domain and path; deleteCookie(name, url)
// deletes every cookie of that name that the jar sends to url).
func jarRig(t *testing.T) (*rig, *devtoolstest.Page) {
	t.Helper()
	r := newRig(t)
	jar := devtoolstest.New(t, "https://app.example")
	// The jar's own evaluate serves only W4's storage function; the
	// attach's liveness probe needs an answer.
	jar.Server.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}}, nil
	})
	r.fake["Jar"] = jar.Server
	r.listed["Jar"] = r.devA.AddPage("Jar", "https://app.example/", jar.Server.URL)
	return r, jar
}

func (r *rig) jarPage(t *testing.T, session string) (*cdptest.Client, string) {
	t.Helper()
	c := dial(t, r.issue(t, session, "Jar", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	return c, page
}

// jarCookies lists the jar as "name=value domain path", sorted.
func jarCookies(jar *devtoolstest.Page) []string {
	jar.Mu.Lock()
	defer jar.Mu.Unlock()
	var out []string
	for _, c := range jar.Cookies {
		out = append(out, c.Name+"="+c.Value+" "+c.Domain+" "+c.Path)
	}
	sort.Strings(out)
	return out
}

func setJar(jar *devtoolstest.Page, cs ...webkit.Cookie) {
	jar.Mu.Lock()
	defer jar.Mu.Unlock()
	jar.Cookies = append([]webkit.Cookie(nil), cs...)
}

func cdpFail(t *testing.T, c *cdptest.Client, session, method string, params any) *cdptest.Error {
	t.Helper()
	_, err := c.Call(testCtx(t), session, method, params)
	ce, ok := err.(*cdptest.Error)
	if !ok {
		t.Fatalf("%s: got %v, want a CDP error", method, err)
	}
	return ce
}

func sessionCookie(name, value, domain, path string) webkit.Cookie {
	return webkit.Cookie{Name: name, Value: value, Domain: domain, Path: path, Session: true, SameSite: "Lax"}
}

// A cookie set through Network.setCookie with a url is a host-only cookie
// of the url's host with path "/" (Chromium MakeCookieFromProtocolValues),
// verified by its read-back, and Network.getCookies{urls} returns it as a
// CDP Network.Cookie: expires -1 and session for a session cookie, size =
// name and value bytes, Chrome's defaults for the fields WebKit lacks.
func TestCDPSetAndGetCookie(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", map[string]any{
		"name": "sid", "value": "v1", "url": "https://app.example/app/index.html", "httpOnly": true, "sameSite": "Strict",
	}))
	if !res.Success {
		t.Fatal("Network.setCookie: success false")
	}
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"sid=v1 app.example /"}) {
		t.Fatalf("jar %v, want a host-only sid on path /", got)
	}
	got := decode[struct{ Cookies []map[string]any }](t, call(t, c, page, "Network.getCookies", map[string]any{"urls": []string{"https://app.example/x"}}))
	want := map[string]any{
		"name": "sid", "value": "v1", "domain": "app.example", "path": "/", "expires": -1.0, "size": 5.0,
		"httpOnly": true, "secure": true, "session": true, "sameSite": "Strict", "priority": "Medium",
		"sourceScheme": "Unset", "sourcePort": -1.0,
	}
	if len(got.Cookies) != 1 || !reflect.DeepEqual(got.Cookies[0], want) {
		t.Fatalf("Network.getCookies = %v, want [%v]", got.Cookies, want)
	}
	// A URL the cookie is not sent to: http (secure cookie), another host.
	for _, u := range []string{"http://app.example/", "https://other.example/", "https://sub.app.example/"} {
		got := decode[struct{ Cookies []any }](t, call(t, c, page, "Network.getCookies", map[string]any{"urls": []string{u}}))
		if len(got.Cookies) != 0 {
			t.Errorf("Network.getCookies{%s} = %v, want none", u, got.Cookies)
		}
	}
	// A persistent domain cookie: expires in seconds, domain with its dot.
	exp := float64(time.Now().Add(time.Hour).Unix())
	call(t, c, page, "Network.setCookies", map[string]any{"cookies": []any{
		map[string]any{"name": "d", "value": "2", "domain": ".app.example", "path": "/p", "expires": exp},
	}})
	all := decode[struct{ Cookies []map[string]any }](t, call(t, c, "", "Storage.getCookies", nil))
	if len(all.Cookies) != 2 || all.Cookies[1]["domain"] != ".app.example" || all.Cookies[1]["expires"] != exp || all.Cookies[1]["session"] != false {
		t.Fatalf("Storage.getCookies = %v, want sid and the domain cookie d expiring at %v", all.Cookies, exp)
	}
	sub := decode[struct{ Cookies []map[string]any }](t, call(t, c, page, "Network.getCookies", map[string]any{"urls": []string{"https://sub.app.example/p/q"}}))
	if len(sub.Cookies) != 1 || sub.Cookies[0]["name"] != "d" {
		t.Fatalf("Network.getCookies{sub.app.example/p/q} = %v, want the domain cookie d only", sub.Cookies)
	}
}

// Network.deleteCookies deletes exactly the named identity, and keeps the
// namesakes that WebKit's Page.deleteCookie(name, url) would also delete
// (the cookie of path "/" for a url of path "/sub", and the domain cookie
// of the host).
func TestCDPDeleteCookiesExactIdentity(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	all := []webkit.Cookie{
		sessionCookie("k", "root", "app.example", "/"),
		sessionCookie("k", "sub", "app.example", "/sub"),
		sessionCookie("k", "dom", ".app.example", "/"),
		sessionCookie("other", "o", "app.example", "/sub"),
	}
	for _, tc := range []struct {
		params map[string]any
		want   []string
	}{
		{map[string]any{"name": "k", "domain": "app.example", "path": "/sub"}, []string{"k=dom .app.example /", "k=root app.example /", "other=o app.example /sub"}},
		{map[string]any{"name": "k", "url": "https://app.example/sub", "path": "/"}, []string{"k=dom .app.example /", "k=sub app.example /sub", "other=o app.example /sub"}},
		{map[string]any{"name": "k", "domain": ".app.example"}, []string{"k=root app.example /", "k=sub app.example /sub", "other=o app.example /sub"}},
		{map[string]any{"name": "k", "url": "https://app.example/"}, []string{"k=dom .app.example /", "other=o app.example /sub"}},
		// Puppeteer's second call names the page's partition: WebKit has no
		// partitioned cookie, so nothing matches.
		{map[string]any{"name": "k", "domain": "app.example", "partitionKey": map[string]any{"topLevelSite": "https://app.example", "hasCrossSiteAncestor": false}}, nil},
	} {
		setJar(jar, all...)
		call(t, c, page, "Network.deleteCookies", tc.params)
		want := tc.want
		if want == nil {
			want = []string{"k=dom .app.example /", "k=root app.example /", "k=sub app.example /sub", "other=o app.example /sub"}
		}
		if got := jarCookies(jar); !reflect.DeepEqual(got, want) {
			t.Errorf("deleteCookies %v: jar %v, want %v", tc.params, got, want)
		}
	}
	if e := cdpFail(t, c, page, "Network.deleteCookies", map[string]any{"name": "k"}); e.Code != -32602 || e.Message != "At least one of the url and domain needs to be specified" {
		t.Errorf("deleteCookies without url and domain: %+v", e)
	}
	if e := cdpFail(t, c, page, "Network.deleteCookies", map[string]any{"name": "k", "url": "about:blank"}); e.Message != "An http or https url URL must be specified" {
		t.Errorf("deleteCookies with about:blank: %+v", e)
	}
}

// An expired cookie (Puppeteer's BrowserContext.deleteCookie sends
// expires 1 through Storage.setCookies) deletes the cookie of that
// identity and nothing else.
func TestCDPExpiredCookieDeletes(t *testing.T) {
	r, jar := jarRig(t)
	c, _ := r.jarPage(t, "s1")
	setJar(jar, sessionCookie("k", "root", "app.example", "/"), sessionCookie("k", "sub", "app.example", "/sub"))
	call(t, c, "", "Storage.setCookies", map[string]any{"cookies": []any{
		map[string]any{"name": "k", "value": "", "domain": "app.example", "path": "/sub", "expires": 1},
	}})
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"k=root app.example /"}) {
		t.Fatalf("jar %v, want only k on /", got)
	}
}

func TestCDPCookieErrors(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	for _, tc := range []struct {
		session, method string
		params          map[string]any
		code            int
		msg             string
	}{
		{page, "Network.setCookie", map[string]any{"name": "a", "value": "b"}, -32602, "At least one of the url or domain needs to be specified"},
		{page, "Network.setCookie", map[string]any{"name": "a", "value": "b", "url": "ftp://app.example/"}, -32602, "URL must have scheme http or https"},
		{page, "Network.setCookie", map[string]any{"name": "a", "value": "b", "url": "https://app.example/", "partitionKey": map[string]any{"topLevelSite": "https://app.example"}}, -32000, "partitioned cookies"},
		{page, "Network.setCookies", map[string]any{"cookies": []any{map[string]any{"name": "ok", "value": "1", "url": "https://app.example/"}, map[string]any{"name": "a", "value": "b"}}}, -32602, "Invalid cookie fields"},
		{page, "Storage.getCookies", map[string]any{"browserContextId": "default"}, -32602, "browserContextId is only allowed for Browser target"},
		{"", "Storage.clearCookies", map[string]any{"browserContextId": "x"}, -32602, "Failed to find browser context for id x"},
	} {
		e := cdpFail(t, c, tc.session, tc.method, tc.params)
		if e.Code != tc.code || !strings.Contains(e.Message, tc.msg) {
			t.Errorf("%s %v: %+v, want %d %q", tc.method, tc.params, e, tc.code, tc.msg)
		}
	}
	if got := jarCookies(jar); len(got) != 0 {
		t.Fatalf("a refused request set cookies: %v", got)
	}
	// A cookie that WebKit does not store as requested: success false.
	jar.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", map[string]any{"name": "a", "value": "b", "url": "https://app.example/"}))
	if res.Success {
		t.Fatal("Network.setCookie of a cookie that was not stored: success true")
	}
	if e := cdpFail(t, c, page, "Network.setCookies", map[string]any{"cookies": []any{map[string]any{"name": "a", "value": "b", "url": "https://app.example/"}}}); !strings.Contains(e.Message, "1 of 1 cookies were not set") {
		t.Fatalf("Network.setCookies of a cookie that was not stored: %+v", e)
	}
}

// Storage.clearCookies (browser session) and Network.clearBrowserCookies
// delete the page's cookies.
func TestCDPClearCookies(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	for _, m := range [][2]string{{"", "Storage.clearCookies"}, {page, "Network.clearBrowserCookies"}} {
		setJar(jar, sessionCookie("a", "1", "app.example", "/"), sessionCookie("b", "2", ".app.example", "/x"))
		call(t, c, m[0], m[1], nil)
		if got := jarCookies(jar); len(got) != 0 {
			t.Fatalf("%s: jar %v, want empty", m[1], got)
		}
	}
}

// A browser-session Storage call reaches the record's selected page under
// the logical session's lease (design 5.2). A page that another session
// leases fails with the lease message; that lease stays, and the page gets
// no cookie command.
func TestCDPBrowserStorageOnALeasedPage(t *testing.T) {
	r, jar := jarRig(t)
	setJar(jar, sessionCookie("a", "1", "app.example", "/"))
	c1, _ := r.jarPage(t, "s1")
	call(t, c1, "", "Storage.getCookies", nil)
	_, epoch := r.leaseOf(t, "Jar")
	mark := len(jar.Server.Received())
	c2 := dial(t, r.issue(t, "s2", "Jar", ScopePage).WS)
	for _, m := range []string{"Storage.getCookies", "Storage.setCookies", "Storage.clearCookies"} {
		params := map[string]any{"cookies": []any{map[string]any{"name": "x", "value": "y", "url": "https://app.example/"}}}
		e := cdpFail(t, c2, "", m, params)
		if !strings.Contains(e.Message, "s1") {
			t.Errorf("%s through s2's endpoint: %+v, want the lease message naming s1", m, e)
		}
	}
	for _, rec := range jar.Server.Received()[mark:] {
		if strings.HasPrefix(rec.Method, "Page.") {
			t.Errorf("the leased page got %s", rec.Method)
		}
	}
	if s, e := r.leaseOf(t, "Jar"); s != "s1" || e != epoch {
		t.Fatalf("lease %s/%d, want s1/%d", s, e, epoch)
	}
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"a=1 app.example /"}) {
		t.Fatalf("jar %v", got)
	}
}

// The bridge uses Page.getCookies and Page.setCookie on every iOS
// version: every command list has them (internal/wip, iOS 16.0 to 27.3).
func TestCookieCommandsInEveryIOSList(t *testing.T) {
	for _, v := range []string{"16.0", "16.4", "16.7.10", "17.0", "18.0", "26.0", "26.2", "26.4", "27.0", "27.3"} {
		for _, m := range []string{"Page.getCookies", "Page.setCookie"} {
			if known, ok := wip.Supported(v, m); !known || !ok {
				t.Errorf("iOS %s: %s known %v supported %v", v, m, known, ok)
			}
		}
	}
}

// genCookie is a WebKit cookie as Page.getCookies reports one.
type genCookie struct{ C webkit.Cookie }

func (genCookie) Generate(r *rand.Rand, _ int) reflect.Value {
	word := func() string {
		const letters = "abcXYZ019-_."
		b := make([]byte, 1+r.Intn(8))
		for i := range b {
			b[i] = letters[r.Intn(len(letters))]
		}
		return string(b)
	}
	c := webkit.Cookie{
		Name: word(), Value: word(), Domain: []string{"", "."}[r.Intn(2)] + word() + ".test", Path: "/" + []string{"", word()}[r.Intn(2)],
		HTTPOnly: r.Intn(2) == 0, Secure: r.Intn(2) == 0, SameSite: []string{"Lax", "Strict", "None"}[r.Intn(3)], Session: r.Intn(2) == 0,
	}
	if !c.Session {
		// Milliseconds since the epoch, from 1970 to about 2096, past and
		// future alike.
		c.Expires = float64(r.Int63n(4e12))
	}
	return reflect.ValueOf(genCookie{c})
}

// The cookie conversion keeps the identity and the attributes: a WebKit
// cookie converted to a CDP cookie and back as the CookieParam a client
// sends (Playwright's addCookies of a cookie it read) is the cookie
// Page.setCookie stores, and it is "expired" exactly when its expiry is
// not after now.
func TestCookieConversionProperty(t *testing.T) {
	now := time.Now()
	prop := func(g genCookie) bool {
		in := g.C
		cc := toCDPCookie(in)
		raw, err := json.Marshal(cc)
		if err != nil {
			return false
		}
		var p cookieParam
		if json.Unmarshal(raw, &p) != nil {
			return false
		}
		if cc.Session {
			p.Expires = nil // a client sends no expiry for a session cookie
		}
		out, expired, err := fromCookieParam(p, now)
		if err != nil {
			t.Logf("fromCookieParam(%+v): %v", p, err)
			return false
		}
		wantExpired := !in.Session && in.Expires <= float64(now.UnixMilli())
		if out != in || expired != wantExpired || cc.Size != len(in.Name)+len(in.Value) {
			t.Logf("%+v -> %+v -> %+v (expired %v)", in, cc, out, expired)
			return false
		}
		return cc.Session == (cc.Expires == -1) && (cc.Session || math.Abs(cc.Expires*1000-in.Expires) < 1)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
