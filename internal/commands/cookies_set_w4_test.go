package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/devtoolstest"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design 4.2 row "cookies set": the registry command sets one cookie
// (name, value, domain, path, expires, secure, http-only, same-site, as
// the W0 tool set_cookie did) and imports a file (curl). Help works and an
// unknown flag exits 2 before any connection, as for the other W4
// commands.
func TestCookiesSetHelpAndFlags(t *testing.T) {
	c, n := Lookup([]string{"cookies", "set"})
	if c == nil || n != 2 {
		t.Fatal("cookies set is not registered")
	}
	if len(c.Args) != 2 || c.Args[0].Name != "name" || c.Args[1].Name != "value" {
		t.Errorf("cookies set arguments %+v, want name and value", c.Args)
	}
	flags := map[string]FlagType{}
	for _, f := range c.Flags {
		flags[f.Name] = f.Type
	}
	for _, f := range []string{"curl", "domain", "path", "expires", "same-site"} {
		if _, ok := flags[f]; !ok {
			t.Errorf("cookies set lacks the flag --%s: %v", f, flags)
		}
	}
	for _, f := range []string{"secure", "http-only"} {
		if typ, ok := flags[f]; !ok || typ != FlagBool {
			t.Errorf("cookies set --%s: present %v, type %v; want a boolean flag", f, ok, typ)
		}
	}
	var out, errb bytes.Buffer
	called := false
	newEnv := func(context.Context, *Invocation) (Env, error) { called = true; return nil, fmt.Errorf("no env") }
	if code := RunCLIStarted(context.Background(), []string{"cookies", "set", "--help"}, &out, &errb, nil, newEnv); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-ios cookies set") || called {
		t.Errorf("cookies set --help: exit %d, called %v, out %q", code, called, out.String())
	}
	errb.Reset()
	if code := RunCLIStarted(context.Background(), []string{"cookies", "set", "--no-such-flag"}, &out, &errb, nil, newEnv); code != ExitUsage || called || !strings.Contains(errb.String(), "unknown flag --no-such-flag") {
		t.Errorf("cookies set --no-such-flag: exit %d, called %v, stderr %q; want exit 2 before any connection", code, called, errb.String())
	}
}

// setCookie runs "cookies set" with argv on page p; dir resolves relative
// paths. It returns the result, its data as JSON and the error.
func setCookie(t *testing.T, p *devtoolstest.Page, dir string, argv ...string) (Result, string, error) {
	t.Helper()
	res, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}, dir: dir}, append([]string{"cookies", "set"}, argv...)...)
	data, merr := json.Marshal(res.Data)
	if merr != nil {
		t.Fatalf("cookies set %v: result data: %v", argv, merr)
	}
	return res, string(data), err
}

func storedCookie(p *devtoolstest.Page, name string) (webkit.Cookie, bool) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for _, c := range p.Cookies {
		if c.Name == name {
			return c, true
		}
	}
	return webkit.Cookie{}, false
}

// The cookie flags keep the meaning of the W0 tool set_cookie: http-only
// and same-site, expires in Unix seconds with 0 for a session cookie. The
// result data keeps the W0 output {ok, cookie}, with the value redacted.
func TestCookiesSetKeepsTheW0Meaning(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	dir := t.TempDir()

	res, data, err := setCookie(t, p, dir, "sid", "topsecret7", "--domain", "app.test", "--path", "/",
		"--expires", "0", "--secure", "--http-only", "--same-site", "strict")
	if err != nil {
		t.Fatalf("cookies set: %v", err)
	}
	got, ok := storedCookie(p, "sid")
	want := webkit.Cookie{Name: "sid", Value: "topsecret7", Domain: "app.test", Path: "/", Secure: true, HTTPOnly: true, Session: true, SameSite: "Strict"}
	if !ok || got != want {
		t.Fatalf("stored %+v, want %+v (expires 0 is a session cookie)", got, want)
	}
	if !strings.Contains(data, `"ok":true`) || !strings.Contains(data, `"name":"sid"`) || strings.Contains(data+res.Text, "topsecret7") {
		t.Fatalf("data %s, text %q; want {ok, cookie} without the value", data, res.Text)
	}

	if _, _, err = setCookie(t, p, dir, "persist", "v", "--domain", "app.test", "--expires", "1893456000"); err != nil {
		t.Fatalf("cookies set with expires: %v", err)
	}
	if got, _ = storedCookie(p, "persist"); got.Session || got.Expires != 1893456000*1000 {
		t.Fatalf("stored %+v, want expires 1893456000 s in milliseconds", got)
	}
}

// Design 4.2 row "cookies set": the import part is agent-browser
// "cookies set --curl <file>".
func TestCookiesSetImportsAFile(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("sid=topsecret8; theme=dark"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, data, err := setCookie(t, p, dir, "--curl", "c.txt")
	if err != nil {
		t.Fatalf("cookies set --curl: %v", err)
	}
	for _, name := range []string{"sid", "theme"} {
		if c, ok := storedCookie(p, name); !ok || c.Domain != "app.test" {
			t.Fatalf("cookie %s: %+v, %v; want it on the page's host app.test", name, c, ok)
		}
	}
	if !strings.Contains(data, `"ok":true`) || !strings.Contains(data, `"imported":2`) || strings.Contains(data+res.Text, "topsecret8") {
		t.Fatalf("data %s, text %q", data, res.Text)
	}
}

// --expires keeps the W0 meaning: Unix seconds, and 0 is a session cookie
// (the W0 tool set_cookie: "0 or omit for session cookie"). An RFC 3339
// time is the CLI form.
func TestCookieExpiresProperty(t *testing.T) {
	session, ms, err := cookieExpiry("0")
	if err != nil || !session || ms != 0 {
		t.Fatalf(`cookieExpiry("0") = %v, %v, %v; want a session cookie`, session, ms, err)
	}
	f := func(s int32) bool {
		if s == 0 {
			return true
		}
		session, ms, err := cookieExpiry(strconv.FormatInt(int64(s), 10))
		return err == nil && !session && ms == float64(int64(s)*1000)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
	if session, ms, err := cookieExpiry("2030-01-02T03:04:05Z"); err != nil || session || ms != 1893553445000 {
		t.Fatalf("RFC 3339: %v, %v, %v", session, ms, err)
	}
}

// Device check 3, P3-F: "cookies set" with an expiry in the past is the
// exact delete that a browser makes of it (RFC 6265 section 5.3; the CDP
// endpoint's Network.setCookie does the same, internal/cdp
// cookies_domain.go setCookie). It deletes only the cookie of that name,
// domain and path, reports the deletion and succeeds; the read-back that
// the cookie is gone stays.
func TestSetCookieWithAPastExpiryDeletesThatCookie(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.Mu.Lock()
	p.Cookies = []webkit.Cookie{
		{Name: "gone", Value: "v-path", Domain: "app.test", Path: "/anything", Session: true, SameSite: "Lax"},
		{Name: "gone", Value: "v-root", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"},
	}
	p.Mu.Unlock()
	dir := t.TempDir()
	res, data, err := setCookie(t, p, dir, "gone", "x", "--domain", "app.test", "--path", "/anything", "--expires", "1")
	if err != nil {
		t.Fatalf("cookies set with an expiry in the past failed: %v", err)
	}
	for _, want := range []string{"gone", "/anything", "deleted"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("cookies set with an expiry in the past = %q, want it to report the deletion (%q)", res.Text, want)
		}
	}
	if !strings.Contains(data, `"ok":true`) || !strings.Contains(data, `"deleted":true`) {
		t.Errorf("data %s, want ok and deleted", data)
	}
	p.Mu.Lock()
	left := append([]webkit.Cookie(nil), p.Cookies...)
	p.Mu.Unlock()
	if len(left) != 1 || left[0].Path != "/" || left[0].Value != "v-root" {
		t.Fatalf("the jar holds %+v; want only the namesake of path /", left)
	}

	// A WebKit that keeps the cookie fails the read-back, as before.
	p.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) { return nil, nil })
	if _, _, err = setCookie(t, p, dir, "gone", "x", "--domain", "app.test", "--path", "/", "--expires", "2001-01-01T00:00:00Z"); err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("cookies set with a past expiry that WebKit ignores: %v; want the read-back failure", err)
	}
}

// Device check 4, P3-F residue: Page.getCookies lists only the cookies of
// the page's URLs, so the read-back of a delete by a past expiry cannot
// see a cookie whose path, domain or Secure flag hides it from the
// current page. The report says "deleted" only when a cookie of that
// name, domain and path was listed before the delete, says so when the
// page had none, and says when the read-back cannot verify the result.
// The delete itself is sent in every case.
func TestSetCookieWithAPastExpiryReportsWhatTheReadBackCanSee(t *testing.T) {
	jar := []webkit.Cookie{
		{Name: "dc4n", Value: "v-path", Domain: "example.com", Path: "/dc4", Session: true, SameSite: "Lax"},
		{Name: "dc4n", Value: "v-root", Domain: "example.com", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "dc4o", Value: "v-other", Domain: "example.com", Path: "/dc4", Session: true, SameSite: "Lax"},
	}
	for _, c := range []struct {
		name, origin, cookie, domain, path string
		wantText                           []string
		wantVerified                       bool
		wantLeft                           []string // name and path of the cookies left in the jar
	}{
		{
			"a hidden cookie that exists", "https://example.com", "dc4n", "", "/dc4",
			[]string{"cannot verify", "the page's path / is outside the cookie path /dc4", "the delete was sent"},
			false,
			[]string{"dc4n /", "dc4o /dc4"},
		},
		{
			"a hidden cookie that never existed", "https://example.com", "dc4zz", "", "/dc4",
			[]string{"cannot verify", "the page's path / is outside the cookie path /dc4"},
			false,
			[]string{"dc4n /dc4", "dc4n /", "dc4o /dc4"},
		},
		{
			"a visible cookie that never existed", "https://example.com", "dc4zz", "", "/",
			[]string{"the page had no cookie of that name, domain and path, so nothing was deleted", "the read-back shows none"},
			true,
			[]string{"dc4n /dc4", "dc4n /", "dc4o /dc4"},
		},
		{
			"a page that is not https", "http://example.com", "dc4zz", "", "/",
			[]string{"cannot verify", "the page is not https"},
			false,
			[]string{"dc4n /dc4", "dc4n /", "dc4o /dc4"},
		},
		{
			"a cookie of another domain", "https://example.com", "dc4zz", "other.test", "/",
			[]string{"cannot verify", "the page's host example.com is outside the cookie domain other.test"},
			false,
			[]string{"dc4n /dc4", "dc4n /", "dc4o /dc4"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := devtoolstest.New(t, c.origin)
			p.Mu.Lock()
			p.URLPath = "/"
			p.Cookies = append([]webkit.Cookie(nil), jar...)
			p.Mu.Unlock()
			argv := []string{"cookies", "set", c.cookie, "x", "--path", c.path, "--expires", "1"}
			if c.domain != "" {
				argv = append(argv, "--domain", c.domain)
			}
			res, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, argv...)
			if err != nil {
				t.Fatalf("cookies set with an expiry in the past failed: %v", err)
			}
			for _, want := range c.wantText {
				if !strings.Contains(res.Text, want) {
					t.Errorf("cookies set = %q, want %q in it", res.Text, want)
				}
			}
			for _, claim := range []string{"so it was deleted", "the read-back shows no cookie"} {
				if strings.Contains(res.Text, claim) {
					t.Errorf("cookies set = %q; it claims %q, which the read-back did not show", res.Text, claim)
				}
			}
			data, _ := json.Marshal(res.Data)
			want := fmt.Sprintf(`"deleted":false,"ok":true,"verified":%v`, c.wantVerified)
			if !strings.Contains(string(data), want) {
				t.Errorf("data %s, want %s", data, want)
			}
			p.Mu.Lock()
			var left []string
			for _, ck := range p.Cookies {
				left = append(left, ck.Name+" "+ck.Path)
			}
			p.Mu.Unlock()
			if strings.Join(left, ", ") != strings.Join(c.wantLeft, ", ") {
				t.Fatalf("the jar holds %v, want %v", left, c.wantLeft)
			}
		})
	}
}

// The failures of the delete by a past expiry name what failed and, once
// the delete was sent, that it was: the jar read before the delete
// (devtools.DeleteListedCookie), and the page's URL that judges the
// read-back (pageHref, cookieHiddenReason).
func TestSetCookieWithAPastExpiryReportsItsFailures(t *testing.T) {
	for _, c := range []struct {
		name, origin string
		setup        func(p *devtoolstest.Page)
		want         []string
	}{
		{"the jar cannot be read", "https://example.com", func(p *devtoolstest.Page) {
			p.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "getCookies refused"}
			})
		}, []string{"reading the cookies before the delete", "getCookies refused"}},
		{"the page does not answer location.href", "https://example.com", func(p *devtoolstest.Page) {
			p.Server.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "evaluate refused"}
			})
		}, []string{"the delete was sent; reading the page's URL", "evaluate refused"}},
		{"location.href is not a string", "https://example.com", func(p *devtoolstest.Page) {
			p.Server.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
			})
		}, []string{"the delete was sent; reading the page's URL: the page returned number"}},
		{"location.href is not a URL", "https://exa mple.com", func(*devtoolstest.Page) {}, []string{"the delete was sent; reading the page's URL", "invalid character"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := devtoolstest.New(t, c.origin)
			c.setup(p)
			_, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, "cookies", "set", "dc4zz", "x", "--domain", "example.com", "--expires", "1")
			if err == nil {
				t.Fatal("cookies set succeeded; want the failure")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("cookies set: %v; want %q in it", err, want)
				}
			}
		})
	}
	// A URL without a path has the path "/" (RFC 6265 section 5.1.4).
	if got, err := cookieHiddenReason("https://example.com", webkit.Cookie{Name: "n", Domain: "example.com", Path: "/dc4"}); err != nil || got != "the page's path / is outside the cookie path /dc4" {
		t.Fatalf("cookieHiddenReason of a URL without a path = %q, %v", got, err)
	}
}

// Review F1: RFC 6265 section 5.1.4 matches the URI path as the request
// has it, percent-encoded. A page at /dc%34 does not get a cookie of path
// /dc4, so the read-back cannot see it: the report must not say that the
// page had none. The delete still removes it.
func TestSetCookieWithAPastExpiryComparesTheEscapedPath(t *testing.T) {
	p := devtoolstest.New(t, "https://example.com")
	p.Mu.Lock()
	p.URLPath = "/dc%34"
	p.Cookies = []webkit.Cookie{{Name: "hidden", Value: "v", Domain: "example.com", Path: "/dc4", Session: true, SameSite: "Lax"}}
	p.Mu.Unlock()
	res, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, "cookies", "set", "hidden", "x", "--path", "/dc4", "--expires", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "cannot verify") || !strings.Contains(res.Text, "the page's path /dc%34 is outside the cookie path /dc4") {
		t.Fatalf("cookies set from /dc%%34 = %q; want the unverified report with the encoded path", res.Text)
	}
	p.Mu.Lock()
	left := len(p.Cookies)
	p.Mu.Unlock()
	if left != 0 {
		t.Fatalf("the jar keeps %d cookie(s); want the delete to remove the cookie", left)
	}
}
