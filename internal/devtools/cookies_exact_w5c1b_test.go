package devtools

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

func exactJar(p *devtoolstest.Page) []string {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var out []string
	for _, c := range p.Cookies {
		out = append(out, c.Name+"="+c.Value+" "+c.Domain+" "+c.Path)
	}
	sort.Strings(out)
	return out
}

func exactCookie(name, value, domain, path string) webkit.Cookie {
	return webkit.Cookie{Name: name, Value: value, Domain: domain, Path: path, Session: true, SameSite: "Lax"}
}

// DeleteCookies deletes each cookie by its identity (name, domain with its
// leading dot, path) and nothing else (W5c-1b review F1, the W4 finding).
// The fake's Page.deleteCookie is iOS's (NetworkStorageSessionCocoa: every
// cookie of the name that the url gets), so Page.deleteCookie(name, url)
// would also delete the parent domain's namesake, the namesake of a shorter
// path and the domain cookie of the host. DeleteCookies sends no
// Page.deleteCookie: one Page.setCookie of each cookie with an expiry in
// the past (RFC 6265 section 5.3), then one read-back.
func TestDeleteCookiesDeletesExactlyTheIdentities(t *testing.T) {
	p := devtoolstest.New(t, "https://sub.app.test")
	secure := exactCookie("k", "s", "sub.app.test", "/x")
	secure.Secure, secure.HTTPOnly, secure.SameSite = true, true, "None"
	p.Cookies = []webkit.Cookie{
		exactCookie("k", "target", "sub.app.test", "/"),
		exactCookie("k", "parent", ".app.test", "/"),
		exactCookie("k", "dom", ".sub.app.test", "/"),
		secure,
		exactCookie("k", "deep", "sub.app.test", "/x/y"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := DeleteCookies(ctx, p.Conn, []webkit.Cookie{p.Cookies[0], secure}); err != nil {
		t.Fatal(err)
	}
	if got, want := exactJar(p), []string{"k=deep sub.app.test /x/y", "k=dom .sub.app.test /", "k=parent .app.test /"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("jar %q, want %q", got, want)
	}
	if n := len(p.Server.ReceivedMethod("Page.deleteCookie")); n != 0 {
		t.Fatalf("Page.deleteCookie sent %d times", n)
	}
	if n := len(p.Server.ReceivedMethod("Page.setCookie")); n != 2 {
		t.Fatalf("Page.setCookie sent %d times, want 2", n)
	}
}

// DeleteCookies fails and names the cookie, never its value, when the
// cookie is still there after its expired Page.setCookie, when WebKit
// refuses the Page.setCookie, and when the read-back fails.
func TestDeleteCookiesFailures(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	target := exactCookie("k", "secretvalue", "app.test", "/sub")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reset := func() {
		p.Mu.Lock()
		defer p.Mu.Unlock()
		p.Cookies = []webkit.Cookie{target, exactCookie("k", "root", "app.test", "/")}
	}

	reset()
	p.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate read failure"}
	})
	err := DeleteCookies(ctx, p.Conn, []webkit.Cookie{target})
	if err == nil || !strings.Contains(err.Error(), "reading the cookies back") || !strings.Contains(err.Error(), "deliberate read failure") {
		t.Errorf("read-back failure: %v", err)
	}
	p.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		p.Mu.Lock()
		defer p.Mu.Unlock()
		return map[string]any{"cookies": slices.Clone(p.Cookies)}, nil
	})

	reset()
	p.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	err = DeleteCookies(ctx, p.Conn, []webkit.Cookie{target})
	if err == nil || !strings.Contains(err.Error(), "k (domain app.test, path /sub)") || strings.Contains(err.Error(), "secretvalue") {
		t.Errorf("a cookie that WebKit keeps: %v, want an error naming it without its value", err)
	}
	if got := exactJar(p); len(got) != 2 {
		t.Errorf("jar after the failed delete %q", got)
	}

	p.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate set refusal"}
	})
	err = DeleteCookies(ctx, p.Conn, []webkit.Cookie{target})
	if err == nil || !strings.Contains(err.Error(), "k (domain app.test, path /sub)") || !strings.Contains(err.Error(), "deliberate set refusal") || strings.Contains(err.Error(), "secretvalue") {
		t.Errorf("a refused Page.setCookie: %v", err)
	}
	if n := len(p.Server.ReceivedMethod("Page.deleteCookie")); n != 0 {
		t.Fatalf("Page.deleteCookie sent %d times", n)
	}
}
