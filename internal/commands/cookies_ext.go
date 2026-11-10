package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// cookies clear and cookies set (design docs/design/agentic-parity.md,
// section 4.2; agent-browser "cookies clear", "cookies set <name> <value>"
// and "cookies set --curl <file>"). Cookie values never appear in the
// output or in errors (W0, design R17). The listing "cookies" is
// cookies_list.go.
func init() {
	Register(&Command{
		Name:    "cookies clear",
		Summary: "Delete the page's cookies (those Page.getCookies lists: the cookies of the page's frames and resources), read back.",
		Flags: []Flag{
			{Name: "domain", Type: FlagString, Help: "delete only the cookies of this domain and its subdomains"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runCookiesClear,
	})
	Register(&Command{
		Name:    "cookies set",
		Summary: "Set one cookie (cookies set <name> <value> [flags]) or import cookies from a file (--curl: a JSON list, a cURL command or a Cookie header), each read back. Values are redacted in the result.",
		Args: []Arg{
			{Name: "name", Help: "the cookie name (not with --curl)"},
			{Name: "value", Help: "the cookie value (not with --curl)"},
		},
		Flags: []Flag{
			{Name: "curl", Type: FlagPath, Help: "import the cookies of this file: a JSON list of {name, value, ...}, a cURL command (DevTools Copy as cURL) or a Cookie header"},
			{Name: "domain", Type: FlagString, Help: "the domain of cookies that have none (default: the cURL URL's host, else the page's host)"},
			{Name: "path", Type: FlagString, Help: "the cookie path (default /)"},
			{Name: "expires", Type: FlagString, Help: "expiry as Unix seconds (decimals allowed) or RFC 3339 time; 0 or none: a session cookie; a time in the past deletes the cookie of this name, domain and path"},
			{Name: "http-only", Type: FlagBool, Help: "set HttpOnly"},
			{Name: "secure", Type: FlagBool, Help: "set Secure"},
			{Name: "same-site", Type: FlagString, Help: "Lax, Strict or None (default Lax)"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Check:   checkCookieValues,
		Run:     runCookiesSet,
	})
}

// cookieLabel names a cookie without its value.
func cookieLabel(c webkit.Cookie) string {
	return fmt.Sprintf("%s (domain %s, path %s)", c.Name, c.Domain, c.Path)
}

func runCookiesClear(ctx context.Context, env Env, in Input) (Result, error) {
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	deleted, err := devtools.ClearCookies(ctx, c, flagString(in, "domain"))
	if err != nil {
		return Result{}, err
	}
	labels := make([]string, len(deleted))
	list := make([]map[string]string, len(deleted))
	for i, ck := range deleted {
		labels[i] = cookieLabel(ck)
		list[i] = map[string]string{"name": ck.Name, "domain": ck.Domain, "path": ck.Path}
	}
	data := map[string]any{"deleted": len(deleted), "cookies": list}
	if len(deleted) == 0 {
		return Result{Text: "No cookies to delete.", Data: data}, nil
	}
	return Result{Text: fmt.Sprintf("Deleted %s: %s", nounCount(len(deleted), "cookie"), strings.Join(labels, ", ")), Data: data}, nil
}

// maxCookieFile bounds a cookie import file.
const maxCookieFile = 4 << 20

// singleCookieFlags are the flags that describe one cookie.
var singleCookieFlags = []string{"path", "expires", "http-only", "secure", "same-site"}

// unixSeconds matches --expires in Unix seconds: an integer or a decimal
// fraction.
var unixSeconds = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// maxExpiryMs bounds --expires in milliseconds from the Unix epoch: the
// range of a JavaScript Date (ECMA-262, "Time Values and Time Range":
// 8.64e15 ms, 100 000 000 days, on each side of the epoch). Beyond it
// seconds*1000 can also overflow to an infinity that JSON cannot encode.
const maxExpiryMs = 8.64e15

// cookieExpiry reads --expires as the W0 tool set_cookie read its
// expires: Unix seconds, where 0 is a session cookie, returned as
// seconds*1000 milliseconds (WebKit's Page.Cookie.expires); the CLI also
// takes an RFC 3339 time. A value whose milliseconds are outside
// ±maxExpiryMs is a usage error.
func cookieExpiry(v string) (session bool, ms float64, err error) {
	if unixSeconds.MatchString(v) {
		s, err := strconv.ParseFloat(v, 64)
		if err != nil { // only a value out of the float64 range
			return false, 0, usagef("--expires %q: %v", v, err)
		}
		// Only a zero written as zero is a session cookie: a tiny
		// nonzero value that rounds to 0 is a time in the past.
		if strings.Trim(v, "-0.") == "" {
			return true, 0, nil
		}
		ms := s * 1000
		if math.Abs(ms) > maxExpiryMs { // also an infinity
			return false, 0, usagef("--expires %q: out of range; want Unix seconds within ±%.0f", v, maxExpiryMs/1000)
		}
		return false, ms, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false, 0, usagef("--expires %q: want Unix seconds or an RFC 3339 time such as 2030-01-02T03:04:05Z", v)
	}
	return false, float64(t.UnixMilli()), nil
}

// checkCookieValues is the value check of "cookies set" (Command.Check):
// a known --same-site and a valid --expires. runCookiesSet reads them
// with the same functions.
func checkCookieValues(in Input) error {
	if _, err := tools.NormalizeSameSite(flagString(in, "same-site")); err != nil {
		return usagef("--same-site: %v", err)
	}
	if v := flagString(in, "expires"); v != "" {
		if _, _, err := cookieExpiry(v); err != nil {
			return err
		}
	}
	return nil
}

func runCookiesSet(ctx context.Context, env Env, in Input) (Result, error) {
	file := flagString(in, "curl")
	if file != "" {
		if len(in.Args) > 0 {
			return Result{}, usagef("cookies set --curl <file> takes no name or value")
		}
		for _, f := range singleCookieFlags {
			if _, ok := in.Flags[f]; ok {
				return Result{}, usagef("--%s describes one cookie; it does not apply to --curl", f)
			}
		}
		return importCookies(ctx, env, env.ResolvePath(file), flagString(in, "domain"))
	}
	if len(in.Args) != 2 {
		return Result{}, usagef("cookies set needs <name> <value>, or --curl <file>")
	}
	ck := webkit.Cookie{
		Name: in.Args[0], Value: in.Args[1], Domain: flagString(in, "domain"), Path: flagString(in, "path"),
		HTTPOnly: flagBool(in, "http-only"), Secure: flagBool(in, "secure"), Session: true,
	}
	if ck.Path == "" {
		ck.Path = "/"
	}
	var err error
	if ck.SameSite, err = tools.NormalizeSameSite(flagString(in, "same-site")); err != nil {
		return Result{}, usagef("--same-site: %v", err)
	}
	if v := flagString(in, "expires"); v != "" {
		if ck.Session, ck.Expires, err = cookieExpiry(v); err != nil {
			return Result{}, err
		}
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if ck.Domain == "" {
		if ck.Domain, err = pageHostname(ctx, c); err != nil {
			return Result{}, err
		}
		if ck.Domain == "" {
			return Result{}, fmt.Errorf("the page has no host (about:blank?); pass --domain")
		}
	}
	if !ck.Session && ck.Expires <= float64(time.Now().UnixMilli()) {
		// A cookie that expired when it is set deletes the cookie of its
		// name, domain and path (RFC 6265 section 5.3), as the CDP
		// endpoint's Network.setCookie does; WebKit stores nothing, so
		// SetCookieVerified would report a missing cookie.
		return deleteByPastExpiry(ctx, c, ck)
	}
	stored, err := tools.SetCookieVerified(ctx, c, ck)
	if err != nil {
		return Result{}, err
	}
	// Data is the W0 set_cookie output: ok and the cookie as read back,
	// value redacted.
	redacted := *stored
	redacted.Value = tools.RedactCookieValue(stored.Value)
	return Result{
		Text: fmt.Sprintf("Cookie %s set and verified (domain %s, path %s).", stored.Name, stored.Domain, stored.Path),
		Data: map[string]any{"ok": true, "cookie": redacted},
	}, nil
}

// deleteByPastExpiry makes the exact delete of ck's name, domain and path
// (devtools.DeleteListedCookie, which reads the jar back) and reports only
// what the read-back can show. Page.getCookies lists only the cookies of
// the page's URLs (device check 4, P3-F): "deleted" needs a cookie that was
// listed before the delete; "the page had none" needs a page whose URL
// would list such a cookie (cookieHiddenReason); otherwise the result is
// not verified. Data: deleted (a listed cookie is gone) and verified (the
// read-back could see a cookie of that identity).
func deleteByPastExpiry(ctx context.Context, c webkit.Caller, ck webkit.Cookie) (Result, error) {
	listed, err := devtools.DeleteListedCookie(ctx, c, ck)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"ok": true, "deleted": listed, "verified": listed, "cookie": map[string]string{"name": ck.Name, "domain": ck.Domain, "path": ck.Path}}
	head := fmt.Sprintf("Cookie %s has an expiry in the past", ck.Name)
	if listed {
		return Result{
			Text: fmt.Sprintf("%s, so it was deleted (domain %s, path %s); the read-back shows no cookie of that name, domain and path.", head, ck.Domain, ck.Path),
			Data: data,
		}, nil
	}
	href, err := pageHref(ctx, c)
	if err != nil {
		return Result{}, fmt.Errorf("the delete was sent; %w", err)
	}
	hidden, err := cookieHiddenReason(href, ck)
	if err != nil {
		return Result{}, fmt.Errorf("the delete was sent; %w", err)
	}
	if hidden == "" {
		data["verified"] = true
		return Result{
			Text: fmt.Sprintf("%s, so it deletes the cookie of its name, domain and path (domain %s, path %s), but the page had no cookie of that name, domain and path, so nothing was deleted; the read-back shows none.", head, ck.Domain, ck.Path),
			Data: data,
		}, nil
	}
	return Result{
		Text: fmt.Sprintf("%s, so the delete was sent for the cookie of its name, domain and path (domain %s, path %s), but the read-back cannot verify the result: %s, so Page.getCookies does not list such a cookie. To check, list the cookies on a page of that domain and path.", head, ck.Domain, ck.Path, hidden),
		Data: data,
	}, nil
}

// pageHref reads location.href.
func pageHref(ctx context.Context, c webkit.Caller) (string, error) {
	res, err := tools.EvaluateScript(ctx, c, "location.href", true)
	if err != nil {
		return "", fmt.Errorf("reading the page's URL: %w", err)
	}
	var href string
	if err := json.Unmarshal(res.Result.Value, &href); err != nil {
		return "", fmt.Errorf("reading the page's URL: the page returned %s", res.Result.Type)
	}
	return href, nil
}

// cookieHiddenReason says why Page.getCookies on the page at pageURL can
// leave out a cookie of ck's name, domain and path, or returns "" when the
// page's URL gets every such cookie: it domain-matches and path-matches
// the cookie (RFC 6265 sections 5.1.3 and 5.1.4), and it is https, so a
// Secure cookie of that identity is listed too. The path match compares
// the URI path as the page's URL has it (percent-encoded), as a browser
// does: a page at /dc%34 does not get a cookie of path /dc4.
func cookieHiddenReason(pageURL string, ck webkit.Cookie) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("reading the page's URL: %w", err)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	switch {
	case !devtools.DomainMatch(u.Hostname(), ck.Domain):
		return fmt.Sprintf("the page's host %s is outside the cookie domain %s", u.Hostname(), ck.Domain), nil
	case !devtools.PathMatch(path, ck.Path):
		return fmt.Sprintf("the page's path %s is outside the cookie path %s", path, ck.Path), nil
	case u.Scheme != "https":
		return "the page is not https, and a Secure cookie is not listed for such a page", nil
	}
	return "", nil
}

func importCookies(ctx context.Context, env Env, path, domain string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("reading the cookie file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCookieFile+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return Result{}, fmt.Errorf("reading the cookie file %s: %w", path, err)
	}
	if len(data) > maxCookieFile {
		return Result{}, fmt.Errorf("the cookie file %s is larger than %d bytes", path, maxCookieFile)
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	host, err := pageHostname(ctx, c)
	if err != nil {
		return Result{}, err
	}
	imp, err := devtools.ParseCookieImport(data, domain, host)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", path, err)
	}
	n, err := devtools.ImportCookies(ctx, c, imp.Cookies)
	if err != nil {
		return Result{}, err
	}
	labels := make([]string, len(imp.Cookies))
	for i, ck := range imp.Cookies {
		labels[i] = cookieLabel(ck)
	}
	return Result{
		Text: fmt.Sprintf("Imported %s from %s (%s): %s", nounCount(n, "cookie"), path, imp.Format, strings.Join(labels, ", ")),
		Data: map[string]any{"ok": true, "imported": n, "format": imp.Format, "file": path},
	}, nil
}
