package cdp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// The cookie methods (design 5.3): Network getCookies, setCookie,
// deleteCookies and clearBrowserCookies (tier 1) and Storage getCookies,
// setCookies and clearCookies (tier 2), plus Network.setCookies, which
// Puppeteer's page.setCookie sends (puppeteer-core 25.16.0
// src/cdp/Page.ts). Puppeteer's BrowserContext and Playwright send the
// Storage methods on the browser session.
//
// WIP commands: Page.getCookies and Page.setCookie, through W4's helpers
// (internal/tools, internal/devtools). They are in the command list of
// every iOS version (internal/wip; 16.0 to 27.3, see
// TestCookieCommandsInEveryIOSList). iOS 27.3 adds Storage.getCookies,
// setCookie and deleteCookies, but keeps the Page commands; the bridge
// uses the Page commands on every version, because W4's verified cookie
// identity is built on them, and the command lists do not give the shape
// of the Storage commands' filter and partition objects.
//
// Page.getCookies lists the cookies of the page's frames and resources,
// not the whole jar. So Storage.getCookies (all cookies in Chrome) gives
// the selected page's cookies, a cookie of another site can be set but
// not verified, and only the page's cookies can be deleted.
//
// Rules taken from Chromium (network_handler.cc and storage_handler.cc at
// cc6b8e46; /home/devkit/.pi/artifacts/worker/w5c1b/chrome-research.md):
//   - setCookie: the domain is url's host when absent. A leading dot makes
//     a domain cookie, no dot a host-only cookie, exactly as Page.setCookie
//     stores the domain it gets. The path is "/" when absent. secure is
//     also set by an https url. expires is in seconds; absent or negative
//     is a session cookie; 0 or a past time deletes the cookie of that
//     identity (Puppeteer's BrowserContext.deleteCookie sends expires 1).
//   - deleteCookies: the name and the domain (url's host when the domain
//     is absent) are compared exactly, the leading dot included; the path
//     only when given. Without a partitionKey only unpartitioned cookies
//     match; with one, nothing does (WebKit has no partitioned cookies
//     through Page.getCookies).
//
// Every delete is exact: Page.setCookie of the cookie with an expiry in
// the past deletes only that identity (devtools.DeleteCookies). The bridge
// never sends Page.deleteCookie(name, url), which also deletes namesakes,
// and never sets a cookie again from an earlier read of the jar, which
// would undo the page's own cookie changes.

func init() {
	register(
		Method{Name: "Network.getCookies", Levels: OnPage, Handle: networkGetCookies},
		Method{Name: "Network.setCookie", Levels: OnPage, Handle: networkSetCookie},
		Method{Name: "Network.setCookies", Levels: OnPage, Handle: setCookiesHandler},
		Method{Name: "Network.deleteCookies", Levels: OnPage, Handle: networkDeleteCookies},
		Method{Name: "Network.clearBrowserCookies", Levels: OnPage, Handle: clearCookiesHandler},
		Method{Name: "Storage.getCookies", Levels: OnBrowser | OnPage, Handle: storageGetCookies},
		Method{Name: "Storage.setCookies", Levels: OnBrowser | OnPage, Handle: setCookiesHandler},
		Method{Name: "Storage.clearCookies", Levels: OnBrowser | OnPage, Handle: clearCookiesHandler},
	)
}

// Values of the CDP cookie that WebKit's Page.Cookie does not have
// (docs/notes/w5.md "W5c-1b"): Chrome's default priority, an unknown
// source scheme, and Chrome's "unspecified" source port.
const (
	cookiePriority     = "Medium"
	cookieSourceScheme = "Unset"
	cookieSourcePort   = -1
)

// cdpCookie is a CDP Network.Cookie (devtools-protocol 0.0.1704951, the
// version of the pinned clients). WebKit has no partitioned cookies, so
// partitionKey and partitionKeyOpaque are never set.
type cdpCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	Size         int     `json:"size"`
	HTTPOnly     bool    `json:"httpOnly"`
	Secure       bool    `json:"secure"`
	Session      bool    `json:"session"`
	SameSite     string  `json:"sameSite,omitempty"`
	Priority     string  `json:"priority"`
	SourceScheme string  `json:"sourceScheme"`
	SourcePort   int     `json:"sourcePort"`
}

// toCDPCookie converts a WebKit cookie. expires is in seconds since the
// epoch, -1 for a session cookie (WebKit: milliseconds, 0); size is the
// bytes of name and value, as Chrome counts it.
func toCDPCookie(c webkit.Cookie) cdpCookie {
	out := cdpCookie{
		Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: -1,
		Size: len(c.Name) + len(c.Value), HTTPOnly: c.HTTPOnly, Secure: c.Secure, Session: c.Session,
		SameSite: c.SameSite, Priority: cookiePriority, SourceScheme: cookieSourceScheme, SourcePort: cookieSourcePort,
	}
	if !c.Session {
		out.Expires = c.Expires / 1000
	}
	return out
}

func toCDPCookies(cs []webkit.Cookie) []cdpCookie {
	out := make([]cdpCookie, 0, len(cs))
	for _, c := range cs {
		out = append(out, toCDPCookie(c))
	}
	return out
}

// cookieParam is a CDP Network.CookieParam. priority, sourceScheme and
// sourcePort have no WebKit field and are ignored.
type cookieParam struct {
	Name         string        `json:"name"`
	Value        string        `json:"value"`
	URL          string        `json:"url"`
	Domain       string        `json:"domain"`
	Path         string        `json:"path"`
	Secure       bool          `json:"secure"`
	HTTPOnly     bool          `json:"httpOnly"`
	SameSite     string        `json:"sameSite"`
	Expires      *float64      `json:"expires"`
	PartitionKey *partitionKey `json:"partitionKey"`
}

// partitionKey is a CDP Network.CookiePartitionKey; an empty topLevelSite
// means no partition (Chromium).
type partitionKey struct {
	TopLevelSite string `json:"topLevelSite"`
}

func (k *partitionKey) partitioned() bool { return k != nil && k.TopLevelSite != "" }

// errNoPartitions refuses a partitioned cookie.
var errNoPartitions = serverError("partitioned cookies (partitionKey) are not supported on iOS WebKit")

// fromCookieParam converts a CDP cookie into the WebKit cookie that
// Page.setCookie stores with the same identity. expired reports a cookie
// whose expiry is not after now: Chrome deletes the cookie of that
// identity, and the caller does the same.
func fromCookieParam(p cookieParam, now time.Time) (c webkit.Cookie, expired bool, err error) {
	if p.PartitionKey.partitioned() {
		return c, false, errNoPartitions
	}
	if p.URL == "" && p.Domain == "" {
		return c, false, paramError("At least one of the url or domain needs to be specified")
	}
	domain, secure := p.Domain, p.Secure
	if p.URL != "" {
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return c, false, paramError("URL must have scheme http or https")
		}
		secure = secure || u.Scheme == "https"
		if domain == "" {
			domain = u.Hostname()
		}
	}
	path := p.Path
	if path == "" {
		path = "/"
	}
	sameSite, err := tools.NormalizeSameSite(p.SameSite)
	if err != nil {
		return c, false, paramError(err.Error())
	}
	c = webkit.Cookie{Name: p.Name, Value: p.Value, Domain: domain, Path: path, HTTPOnly: p.HTTPOnly, Secure: secure, SameSite: sameSite, Session: true}
	if p.Expires != nil && *p.Expires >= 0 {
		ms := math.Round(*p.Expires * 1000)
		c.Session, c.Expires = false, ms
		expired = ms <= float64(now.UnixMilli())
	}
	return c, expired, nil
}

// epochExpiryMs is the expiry, in milliseconds since the Unix epoch, that
// setCookie sends for an expired cookie whose expiry is the epoch itself:
// the earliest one that tools.SetCookie sends as an expiry.
const epochExpiryMs = 1

// setCookie sets c and verifies it. An expired c (Chrome: the store
// deletes the cookie of that identity) goes to WebKit as converted, in one
// Page.setCookie, which deletes exactly that identity (RFC 6265 section
// 5.3; devtools.DeleteCookies), and the read-back fails when the identity
// is still there. tools.SetCookie sends Expires 0 as a session cookie (W4's
// convention for a cookie without an expiry), which would store c, so an
// expiry at the Unix epoch (CDP expires 0, or below 0.5 ms) goes as
// epochExpiryMs, which is as much in the past.
func setCookie(ctx context.Context, wc webkit.Caller, c webkit.Cookie, expired bool) error {
	if !expired {
		_, err := tools.SetCookieVerified(ctx, wc, c)
		return err
	}
	c.Expires = max(c.Expires, epochExpiryMs)
	if err := tools.SetCookie(ctx, wc, c); err != nil {
		return fmt.Errorf("Page.setCookie of the expired cookie %s (domain %s, path %s): %w", c.Name, c.Domain, c.Path, err)
	}
	return devtools.CheckCookiesGone(ctx, wc, []webkit.Cookie{c})
}

// sentTo reports whether the browser sends c to u (RFC 6265 sections
// 5.1.3, 5.1.4 and 5.4): a host-only cookie to its host only, a domain
// cookie (leading dot) to the domain and its subdomains, the path as a
// prefix that ends at a "/", and a secure cookie over https only.
func sentTo(c webkit.Cookie, u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	domain := strings.ToLower(c.Domain)
	if d, ok := strings.CutPrefix(domain, "."); ok {
		if host != d && (!strings.HasSuffix(host, domain) || net.ParseIP(host) != nil) {
			return false
		}
	} else if host != domain {
		return false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	// below: c.Path is a shorter prefix of path that ends at a "/" of it.
	below := len(path) > len(c.Path) && strings.HasPrefix(path, c.Path) && (strings.HasSuffix(c.Path, "/") || path[len(c.Path)] == '/')
	if path != c.Path && !below {
		return false
	}
	return !c.Secure || u.Scheme == "https"
}

// cookiePage runs fn with a caller of the call's page: the attachment's
// page on a page session (under its lease, session.do), the record's
// selected page on the browser session (record.inPage).
func cookiePage(ctx context.Context, call *Call, class core.Class, fn func(ctx context.Context, c webkit.Caller) error) error {
	if s := call.session; s != nil {
		return s.do(ctx, call.Method, class, func(ctx context.Context, mo *core.ManagedOp) error {
			return fn(ctx, mo.Op.Caller())
		})
	}
	rec := call.conn.rec
	t, err := rec.selectedTarget(ctx)
	if err != nil {
		return err
	}
	return rec.inPage(ctx, t, call.Method, class, fn)
}

// selectedTarget is the record's selected page, the page through which a
// browser-session call reaches the device (design 5.2: the record's
// selected page, under the logical session's lease).
func (rec *record) selectedTarget(ctx context.Context) (target, error) {
	ts, err := rec.targets(ctx)
	if err != nil {
		return target{}, err
	}
	if len(ts) == 0 || ts[0].key != rec.selected.Key {
		return target{}, fmt.Errorf("the selected page %q is gone; issue a new URL with 'iwdp-cli get cdp-url'", rec.selected.Title)
	}
	return ts[0], nil
}

// inPage runs fn in one operation on the page of t for the record's
// logical session, as record.evalIn does: it takes or renews the session's
// lease (Touch), so a page that another session leases fails with the
// lease message and keeps that lease.
func (rec *record) inPage(ctx context.Context, t target, name string, class core.Class, fn func(ctx context.Context, c webkit.Caller) error) error {
	m := rec.s.o.Manager
	pg, epoch, err := m.Touch(ctx, t.req, rec.ao())
	if err != nil {
		return err
	}
	mo, err := m.BeginLeased(ctx, pg, rec.session, epoch, "cdp "+name, class)
	if err != nil {
		return err
	}
	defer m.End(mo.ID)
	return fn(mo.Op.Context(), mo.Op.Caller())
}

// checkContext refuses a browserContextId as Chrome does: only on the
// browser session, and only the default context exists (design 4.3).
func checkContext(call *Call, id string) error {
	switch {
	case id == "":
		return nil
	case call.session != nil:
		return paramError("browserContextId is only allowed for Browser target")
	case id != defaultContext:
		return paramError("Failed to find browser context for id " + id)
	}
	return nil
}

func networkGetCookies(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		URLs []string `json:"urls"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	var urls []*url.URL
	for _, raw := range p.URLs {
		if u, err := url.Parse(raw); err == nil {
			urls = append(urls, u)
		}
	}
	var out []webkit.Cookie
	err := cookiePage(ctx, call, core.ClassRead, func(ctx context.Context, c webkit.Caller) error {
		jar, err := tools.GetCookies(ctx, c)
		if err != nil {
			return fmt.Errorf("reading the cookies (Page.getCookies): %w", err)
		}
		for _, ck := range jar {
			match := p.URLs == nil
			for _, u := range urls {
				match = match || sentTo(ck, u)
			}
			if match {
				out = append(out, ck)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"cookies": toCDPCookies(out)}, nil, nil
}

func storageGetCookies(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if err := checkContext(call, p.BrowserContextID); err != nil {
		return nil, nil, err
	}
	var jar []webkit.Cookie
	err := cookiePage(ctx, call, core.ClassRead, func(ctx context.Context, c webkit.Caller) error {
		var err error
		if jar, err = tools.GetCookies(ctx, c); err != nil {
			return fmt.Errorf("reading the cookies (Page.getCookies): %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"cookies": toCDPCookies(jar)}, nil, nil
}

// networkSetCookie answers {success} as Chrome does: false when the
// cookie was not stored as requested (its read-back failed), with no
// protocol error. A conversion error is a protocol error.
func networkSetCookie(ctx context.Context, call *Call) (any, Later, error) {
	var p cookieParam
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	ck, expired, err := fromCookieParam(p, time.Now())
	if err != nil {
		return nil, nil, err
	}
	var stored error
	err = cookiePage(ctx, call, core.ClassMutate, func(ctx context.Context, c webkit.Caller) error {
		stored = setCookie(ctx, c, ck, expired)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"success": stored == nil}, nil, nil
}

// setCookiesHandler serves Network.setCookies and Storage.setCookies. As in
// Chrome, every cookie is converted first, and a conversion error sets
// none ("Invalid cookie fields"). Unlike Chrome, which ignores the store's
// result here, a cookie that is not stored as requested fails the call and
// is named.
func setCookiesHandler(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Cookies          []cookieParam `json:"cookies"`
		BrowserContextID string        `json:"browserContextId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if err := checkContext(call, p.BrowserContextID); err != nil {
		return nil, nil, err
	}
	type item struct {
		c       webkit.Cookie
		expired bool
	}
	now := time.Now()
	items := make([]item, 0, len(p.Cookies))
	for _, cp := range p.Cookies {
		ck, expired, err := fromCookieParam(cp, now)
		if errors.Is(err, errNoPartitions) {
			return nil, nil, err
		}
		if err != nil {
			return nil, nil, paramError("Invalid cookie fields")
		}
		items = append(items, item{ck, expired})
	}
	if len(items) == 0 {
		return map[string]any{}, nil, nil
	}
	err := cookiePage(ctx, call, core.ClassMutate, func(ctx context.Context, c webkit.Caller) error {
		var failed []string
		for _, it := range items {
			if err := setCookie(ctx, c, it.c, it.expired); err != nil {
				failed = append(failed, err.Error())
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d cookies were not set as requested: %s", len(failed), len(items), strings.Join(failed, "; "))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

func networkDeleteCookies(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Name         string        `json:"name"`
		URL          string        `json:"url"`
		Domain       string        `json:"domain"`
		Path         string        `json:"path"`
		PartitionKey *partitionKey `json:"partitionKey"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.URL == "" && p.Domain == "" {
		return nil, nil, paramError("At least one of the url and domain needs to be specified")
	}
	domain := p.Domain
	if domain == "" {
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, nil, paramError("An http or https url URL must be specified")
		}
		domain = u.Hostname()
	}
	if p.PartitionKey.partitioned() {
		return map[string]any{}, nil, nil // no partitioned cookie can match
	}
	err := cookiePage(ctx, call, core.ClassMutate, func(ctx context.Context, c webkit.Caller) error {
		jar, err := tools.GetCookies(ctx, c)
		if err != nil {
			return fmt.Errorf("reading the cookies (Page.getCookies): %w", err)
		}
		var targets []webkit.Cookie
		for _, ck := range jar {
			if ck.Name == p.Name && strings.EqualFold(ck.Domain, domain) && (p.Path == "" || ck.Path == p.Path) {
				targets = append(targets, ck)
			}
		}
		return devtools.DeleteCookies(ctx, c, targets)
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// clearCookiesHandler serves Network.clearBrowserCookies and
// Storage.clearCookies: it deletes every cookie that Page.getCookies lists,
// each by its exact identity, and reads the jar back (devtools.ClearCookies,
// W4's "cookies clear").
func clearCookiesHandler(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if err := checkContext(call, p.BrowserContextID); err != nil {
		return nil, nil, err
	}
	err := cookiePage(ctx, call, core.ClassMutate, func(ctx context.Context, c webkit.Caller) error {
		_, err := devtools.ClearCookies(ctx, c, "")
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// paramError is a CDP invalid-params error with Chrome's exact text.
func paramError(msg string) *Error { return &Error{Code: codeInvalidParams, Message: msg} }
