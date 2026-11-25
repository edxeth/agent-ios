// Package tools holds the WebKit Inspector Protocol helpers that the registry
// commands, the CDP endpoint (internal/cdp) and internal/devtools share:
// script evaluation, screenshots, DOM lookup and cookies.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// ScreenshotOptions selects what CaptureScreenshot captures. The zero value
// captures the visible viewport.
type ScreenshotOptions struct {
	// FullPage captures the whole scrollable document.
	FullPage bool
	// Selector captures the first element that matches this CSS selector.
	Selector string
}

// CaptureScreenshot captures the viewport (default), the full document, or one
// element as a base64-encoded PNG data URL.
func CaptureScreenshot(ctx context.Context, client webkit.Caller, opts ScreenshotOptions) (string, error) {
	switch {
	case opts.FullPage && opts.Selector != "":
		return "", errors.New("choose either a full-page or an element screenshot, not both")
	case opts.Selector != "":
		doc, err := GetDocument(ctx, client, 1)
		if err != nil {
			return "", fmt.Errorf("getting document for selector %q: %w", opts.Selector, err)
		}
		nodeID, err := QuerySelector(ctx, client, doc.NodeID, opts.Selector)
		if err != nil {
			return "", err
		}
		return SnapshotNode(ctx, client, nodeID)
	case opts.FullPage:
		return TakeScreenshot(ctx, client)
	default:
		return viewportScreenshot(ctx, client)
	}
}

// viewportScreenshot captures the visible viewport with Page.snapshotRect in
// Viewport coordinates.
func viewportScreenshot(ctx context.Context, client webkit.Caller) (string, error) {
	dims, err := evaluateDimensions(ctx, client, `JSON.stringify({width: window.innerWidth, height: window.innerHeight})`)
	if err != nil {
		return "", fmt.Errorf("getting viewport size: %w", err)
	}
	return snapshotRect(ctx, client, dims, "Viewport")
}

type pageDimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// evaluateDimensions runs an expression that returns JSON.stringify({width, height}).
func evaluateDimensions(ctx context.Context, client webkit.Caller, expression string) (pageDimensions, error) {
	var dims pageDimensions
	dimResult, err := client.Send(ctx, "Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
	})
	if err != nil {
		return dims, err
	}

	var evalResp struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(dimResult, &evalResp); err != nil {
		return dims, fmt.Errorf("decoding dimension result: %w", err)
	}

	dimStr := string(evalResp.Result.Value)
	// Value comes back as a JSON string (since we used JSON.stringify), so unquote first.
	var unquoted string
	if err := json.Unmarshal(evalResp.Result.Value, &unquoted); err == nil {
		dimStr = unquoted
	}
	if err := json.Unmarshal([]byte(dimStr), &dims); err != nil {
		return dims, fmt.Errorf("parsing dimensions: %w", err)
	}
	if dims.Width <= 0 || dims.Height <= 0 {
		return dims, fmt.Errorf("invalid dimensions: %dx%d", dims.Width, dims.Height)
	}
	return dims, nil
}

// snapshotRect captures a rectangle starting at 0,0 in the given WebKit
// coordinate system ("Viewport" or "Page").
func snapshotRect(ctx context.Context, client webkit.Caller, dims pageDimensions, coordinateSystem string) (string, error) {
	result, err := client.Send(ctx, "Page.snapshotRect", map[string]interface{}{
		"x":                0,
		"y":                0,
		"width":            dims.Width,
		"height":           dims.Height,
		"coordinateSystem": coordinateSystem,
	})
	if err != nil {
		return "", err
	}

	var snap struct {
		DataURL string `json:"dataURL"`
	}
	if err := json.Unmarshal(result, &snap); err != nil {
		return "", fmt.Errorf("decoding screenshot: %w", err)
	}
	return snap.DataURL, nil
}

// TakeScreenshot captures the full scrollable document as a base64-encoded PNG.
func TakeScreenshot(ctx context.Context, client webkit.Caller) (string, error) {
	// Page.snapshotRect requires explicit width/height.
	dims, err := evaluateDimensions(ctx, client, `JSON.stringify({
			width: Math.max(
				document.body.scrollWidth, document.documentElement.scrollWidth,
				document.body.offsetWidth, document.documentElement.offsetWidth,
				document.body.clientWidth, document.documentElement.clientWidth
			),
			height: Math.max(
				document.body.scrollHeight, document.documentElement.scrollHeight,
				document.body.offsetHeight, document.documentElement.offsetHeight,
				document.body.clientHeight, document.documentElement.clientHeight
			)
		})`)
	if err != nil {
		return "", fmt.Errorf("getting page dimensions: %w", err)
	}
	return snapshotRect(ctx, client, dims, "Page")
}

// SnapshotNode captures a specific DOM node as a base64-encoded PNG.
func SnapshotNode(ctx context.Context, client webkit.Caller, nodeID int) (string, error) {
	result, err := client.Send(ctx, "Page.snapshotNode", map[string]interface{}{
		"nodeId": nodeID,
	})
	if err != nil {
		return "", err
	}

	var snap struct {
		DataURL string `json:"dataURL"`
	}
	if err := json.Unmarshal(result, &snap); err != nil {
		return "", fmt.Errorf("decoding node snapshot: %w", err)
	}
	return snap.DataURL, nil
}

// GetCookies returns all cookies for the current page, including httpOnly and secure.
func GetCookies(ctx context.Context, client webkit.Caller) ([]webkit.Cookie, error) {
	result, err := client.Send(ctx, "Page.getCookies", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Cookies []webkit.Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decoding cookies: %w", err)
	}
	return resp.Cookies, nil
}

// sameSitePolicies maps lower-case input to WebKit's CookieSameSitePolicy
// values, which are case-sensitive on the wire.
var sameSitePolicies = map[string]string{"lax": "Lax", "strict": "Strict", "none": "None"}

// DefaultSameSite is used when a cookie has no SameSite value; browsers apply
// the same default.
const DefaultSameSite = "Lax"

// NormalizeSameSite returns the WebKit spelling of a SameSite value (Lax,
// Strict or None, any case). An empty value becomes DefaultSameSite.
func NormalizeSameSite(v string) (string, error) {
	if v == "" {
		return DefaultSameSite, nil
	}
	if p, ok := sameSitePolicies[strings.ToLower(v)]; ok {
		return p, nil
	}
	return "", fmt.Errorf("invalid sameSite %q: use Lax, Strict or None", v)
}

// normalizeCookie returns the cookie as SetCookie sends it: SameSite in
// WebKit's spelling (empty becomes DefaultSameSite), and a cookie with
// Session set or Expires 0 marked as a session cookie with Expires 0.
func normalizeCookie(cookie webkit.Cookie) (webkit.Cookie, error) {
	sameSite, err := NormalizeSameSite(cookie.SameSite)
	if err != nil {
		return cookie, err
	}
	cookie.SameSite = sameSite
	if cookie.Session || cookie.Expires == 0 {
		cookie.Session = true
		cookie.Expires = 0
	}
	return cookie, nil
}

// setCookieParam is the Page.setCookie cookie object. It differs from
// webkit.Cookie in one key: a session cookie has no expires. iOS 16.7 WebKit
// keeps any supplied expires, and Cocoa turns 0 into an expiry at the Unix
// epoch (InspectorPageAgent.cpp#L684-L691 and CookieCocoa.mm#L158-L160 at
// WebKit-7615.3.12.11.5). session is always sent: that version dereferences
// it without a check.
type setCookieParam struct {
	Name     string   `json:"name"`
	Value    string   `json:"value"`
	Domain   string   `json:"domain"`
	Path     string   `json:"path"`
	Expires  *float64 `json:"expires,omitempty"`
	HTTPOnly bool     `json:"httpOnly"`
	Secure   bool     `json:"secure"`
	Session  bool     `json:"session"`
	SameSite string   `json:"sameSite"`
}

// SetCookie sets a cookie with Page.setCookie. It never sends an empty
// sameSite (WebKit rejects it): an empty value becomes DefaultSameSite. A
// cookie with Session set or Expires 0 is sent as a session cookie, without
// an expiry. Expires is in milliseconds since the Unix epoch, as in WebKit's
// Page.Cookie.
func SetCookie(ctx context.Context, client webkit.Caller, cookie webkit.Cookie) error {
	cookie, err := normalizeCookie(cookie)
	if err != nil {
		return err
	}
	param := setCookieParam{
		Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path,
		HTTPOnly: cookie.HTTPOnly, Secure: cookie.Secure, Session: cookie.Session, SameSite: cookie.SameSite,
	}
	if !cookie.Session {
		param.Expires = &cookie.Expires
	}
	_, err = client.Send(ctx, "Page.setCookie", map[string]interface{}{
		"cookie": param,
	})
	return err
}

// cookieExpiryPrecisionMs is how far a read-back expiry may differ from the
// requested one and still match. Cookie dates have second precision (RFC 6265
// section 5.1.1), and the --expires input is in seconds.
const cookieExpiryPrecisionMs = 1000

// SetCookieVerified sets a cookie and reads it back with Page.getCookies. It
// returns the stored cookie, or an error when the cookie is missing or stored
// with other attributes than requested, including the session status and,
// for a persistent cookie, the expiry to the second. Errors never contain
// cookie values.
func SetCookieVerified(ctx context.Context, client webkit.Caller, cookie webkit.Cookie) (*webkit.Cookie, error) {
	cookie, err := normalizeCookie(cookie)
	if err != nil {
		return nil, err
	}
	if err := SetCookie(ctx, client, cookie); err != nil {
		return nil, err
	}
	cookies, err := GetCookies(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("reading cookie %s back: %w", cookie.Name, err)
	}
	if got := storedCookie(cookies, cookie); got != nil {
		var diffs []string
		if got.Value != cookie.Value {
			diffs = append(diffs, "value differs")
		}
		if got.SameSite != cookie.SameSite {
			diffs = append(diffs, fmt.Sprintf("sameSite=%s (requested %s)", got.SameSite, cookie.SameSite))
		}
		if got.Secure != cookie.Secure {
			diffs = append(diffs, fmt.Sprintf("secure=%t (requested %t)", got.Secure, cookie.Secure))
		}
		if got.HTTPOnly != cookie.HTTPOnly {
			diffs = append(diffs, fmt.Sprintf("httpOnly=%t (requested %t)", got.HTTPOnly, cookie.HTTPOnly))
		}
		if got.Session != cookie.Session {
			diffs = append(diffs, fmt.Sprintf("session=%t (requested %t)", got.Session, cookie.Session))
		} else if !cookie.Session && math.Abs(got.Expires-cookie.Expires) >= cookieExpiryPrecisionMs {
			diffs = append(diffs, fmt.Sprintf("expires=%s (requested %s)", formatCookieExpiry(got.Expires), formatCookieExpiry(cookie.Expires)))
		}
		if len(diffs) > 0 {
			return got, fmt.Errorf("cookie %s was stored with other attributes: %s", cookie.Name, strings.Join(diffs, ", "))
		}
		return got, nil
	}
	return nil, fmt.Errorf("cookie %s (domain %q, path %q) was not found after Page.setCookie: WebKit rejected it, or Page.getCookies does not list it because the domain does not match a frame or resource of the current page (SameSite=None also needs secure)", cookie.Name, cookie.Domain, cookie.Path)
}

// formatCookieExpiry renders an expiry in milliseconds since the Unix epoch
// as an RFC 3339 UTC time.
func formatCookieExpiry(ms float64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
}

// storedCookie finds the cookie that Page.setCookie stored for want among
// cookies: the same name, the same path and the same domain, case aside
// (RFC 6265 section 5.1.2 compares host names without case). Page.setCookie
// stores the domain and the path as given, and the domain's leading dot is
// the cookie's kind: "a.test" is a host-only cookie, ".a.test" a domain
// cookie, and both can exist with the same name and path
// (TestDevtoolsCookieIdentityIsStoredAsSet on WebKitGTK; WebKit passes the
// domain unchanged to NSHTTPCookieDomain and soup_cookie_new). Only a
// Set-Cookie Domain attribute gets the dot added, and Page.getCookies then
// reports it with the dot. So a cookie of the other kind, of another path
// or of another domain is never the one that was set.
func storedCookie(cookies []webkit.Cookie, want webkit.Cookie) *webkit.Cookie {
	for i := range cookies {
		got := &cookies[i]
		if got.Name == want.Name && got.Path == want.Path && strings.EqualFold(got.Domain, want.Domain) {
			return got
		}
	}
	return nil
}

// maxRedactedPrefix is the most characters RedactCookieValue reveals.
const maxRedactedPrefix = 4

// RedactCookieValue hides a cookie value. It keeps a prefix of at most
// maxRedactedPrefix characters and never more than a quarter of the value,
// so short values reveal nothing, and it reports the length in characters.
// An empty value stays empty.
func RedactCookieValue(v string) string {
	n := utf8.RuneCountInString(v)
	if n == 0 {
		return ""
	}
	keep := min(maxRedactedPrefix, n/4)
	suffix := fmt.Sprintf("(redacted, %d chars)", n)
	if keep == 0 {
		return suffix
	}
	return string([]rune(v)[:keep]) + "..." + suffix
}
