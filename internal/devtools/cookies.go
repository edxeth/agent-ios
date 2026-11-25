package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Cookie import and clearing for "cookies set --curl <file>" and "cookies
// clear" (design 4.2; agent-browser commands.md "Cookie import"). Error
// messages name cookies by name or position and never contain a value.

// CookieImport is a parsed cookie file.
type CookieImport struct {
	Format  string // "json", "curl" or "header"
	Cookies []webkit.Cookie
}

// ParseCookieImport reads the cookies of a file in one of three formats,
// detected from its content:
//   - JSON: an array of cookie objects, or an object whose "cookies" member
//     is one (a state file). Fields: name, value, domain, path, expires
//     (Unix seconds; absent, 0 or negative: a session cookie), httpOnly,
//     secure, sameSite (Lax, Strict or None, any case), session.
//   - cURL: a curl command as DevTools "Copy as cURL" writes it (POSIX
//     shell quoting, including $'...'); the cookies come from -H/--header
//     "Cookie: ..." and -b/--cookie "name=value; ...".
//   - header: one line "name=value; name2=value2", with or without a
//     "Cookie:" prefix.
//
// A cookie without a domain gets domainFlag (--domain), else the host of
// the cURL URL, else pageHost. Path defaults to "/", SameSite to
// tools.DefaultSameSite, and cookies without an expiry are session cookies.
func ParseCookieImport(data []byte, domainFlag, pageHost string) (CookieImport, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return CookieImport{}, errors.New("the cookie file is empty")
	}
	if !utf8.ValidString(text) {
		return CookieImport{}, errors.New("the cookie file is not UTF-8 text")
	}
	var out CookieImport
	var err error
	switch {
	case text[0] == '[' || text[0] == '{':
		out.Format = "json"
		out.Cookies, err = parseJSONCookies(text, firstNonEmpty(domainFlag, pageHost))
	case text == "curl" || strings.HasPrefix(text, "curl ") || strings.HasPrefix(text, "curl\t") || strings.HasPrefix(text, "curl\\\n"):
		out.Format = "curl"
		out.Cookies, err = parseCurlCookies(text, domainFlag, pageHost)
	default:
		out.Format = "header"
		if strings.ContainsAny(text, "\r\n") {
			return CookieImport{}, errors.New("a Cookie header file holds one line (name=value; ...); this file has several")
		}
		if len(text) >= len("cookie:") && strings.EqualFold(text[:len("cookie:")], "cookie:") {
			text = text[len("cookie:"):]
		}
		out.Cookies, err = parseCookieHeader(text, firstNonEmpty(domainFlag, pageHost))
	}
	if err != nil {
		return CookieImport{}, err
	}
	if len(out.Cookies) == 0 {
		return CookieImport{}, fmt.Errorf("the %s cookie file holds no cookies", out.Format)
	}
	return out, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// importedCookie is a cookie of the importer's defaults.
func importedCookie(name, value, domain string) webkit.Cookie {
	return webkit.Cookie{Name: name, Value: value, Domain: domain, Path: "/", Session: true, SameSite: tools.DefaultSameSite}
}

// parseCookieHeader parses "name=value; name2=value2" (RFC 6265 section
// 4.2.1). Values keep any "=" after the first one.
func parseCookieHeader(s, domain string) ([]webkit.Cookie, error) {
	var out []webkit.Cookie
	n := 0
	for _, part := range strings.Split(s, ";") {
		part = strings.Trim(part, " \t")
		if part == "" {
			continue
		}
		n++
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		switch {
		case !ok:
			return nil, fmt.Errorf("pair %d of the Cookie header has no '=': want name=value", n)
		case name == "":
			return nil, fmt.Errorf("pair %d of the Cookie header has no name", n)
		case domain == "":
			return nil, fmt.Errorf("cookie %q has no domain: pass --domain", name)
		}
		out = append(out, importedCookie(name, strings.TrimSpace(value), domain))
	}
	return out, nil
}

type jsonCookie struct {
	Name     string   `json:"name"`
	Value    string   `json:"value"`
	Domain   string   `json:"domain"`
	Path     string   `json:"path"`
	Expires  *float64 `json:"expires"`
	HTTPOnly bool     `json:"httpOnly"`
	Secure   bool     `json:"secure"`
	SameSite string   `json:"sameSite"`
	Session  bool     `json:"session"`
}

func parseJSONCookies(text, domain string) ([]webkit.Cookie, error) {
	var list []jsonCookie
	var err error
	if text[0] == '{' {
		var obj struct {
			Cookies []jsonCookie `json:"cookies"`
		}
		err = json.Unmarshal([]byte(text), &obj)
		list = obj.Cookies
	} else {
		err = json.Unmarshal([]byte(text), &list)
	}
	if err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("the cookie file is not valid JSON (error at byte %d)", se.Offset)
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return nil, fmt.Errorf("the cookie file is not a JSON list of cookies: field %q is not a %s", te.Field, te.Type)
		}
		return nil, fmt.Errorf("the cookie file is not a JSON list of cookies: %w", err)
	}
	out := make([]webkit.Cookie, 0, len(list))
	for i, j := range list {
		if j.Name == "" {
			return nil, fmt.Errorf("cookie %d of the JSON file has no name", i+1)
		}
		c := importedCookie(j.Name, j.Value, firstNonEmpty(j.Domain, domain))
		if c.Domain == "" {
			return nil, fmt.Errorf("cookie %q has no domain: pass --domain", j.Name)
		}
		if j.Path != "" {
			c.Path = j.Path
		}
		c.HTTPOnly, c.Secure = j.HTTPOnly, j.Secure
		if c.SameSite, err = tools.NormalizeSameSite(j.SameSite); err != nil {
			return nil, fmt.Errorf("cookie %q: %w", j.Name, err)
		}
		if j.Expires != nil && *j.Expires > 0 && !j.Session {
			c.Session, c.Expires = false, math.Round(*j.Expires*1000)
		}
		out = append(out, c)
	}
	return out, nil
}

// curlValueOptions are curl options whose next word is a value that is
// neither a URL nor a cookie, so it is skipped.
var curlValueOptions = map[string]bool{
	"-X": true, "--request": true, "-d": true, "--data": true, "--data-raw": true, "--data-binary": true,
	"--data-urlencode": true, "-A": true, "--user-agent": true, "-e": true, "--referer": true,
	"-u": true, "--user": true, "-o": true, "--output": true, "-F": true, "--form": true,
}

func parseCurlCookies(text, domainFlag, pageHost string) ([]webkit.Cookie, error) {
	words, err := shellWords(text)
	if err != nil {
		return nil, fmt.Errorf("reading the cURL command: %w", err)
	}
	var target string
	var jars []string
	for i := 1; i < len(words); i++ {
		w := words[i]
		next := func() (string, error) {
			if i+1 >= len(words) {
				return "", fmt.Errorf("the cURL option %s has no value", w)
			}
			i++
			return words[i], nil
		}
		switch {
		case w == "-H" || w == "--header":
			v, err := next()
			if err != nil {
				return nil, err
			}
			if name, value, ok := strings.Cut(v, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "cookie") {
				jars = append(jars, value)
			}
		case w == "-b" || w == "--cookie":
			v, err := next()
			if err != nil {
				return nil, err
			}
			if !strings.Contains(v, "=") {
				return nil, fmt.Errorf("the cURL command reads cookies from the cookie file %q; export them as a Cookie header instead", v)
			}
			jars = append(jars, v)
		case w == "--url":
			if target, err = next(); err != nil {
				return nil, err
			}
		case curlValueOptions[w]:
			if _, err := next(); err != nil {
				return nil, err
			}
		case target == "" && (strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://")):
			target = w
		}
	}
	if len(jars) == 0 {
		return nil, errors.New("the cURL command has no Cookie header (-H 'Cookie: ...') and no -b name=value")
	}
	domain := domainFlag
	if domain == "" && target != "" {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("the cURL URL does not parse: %w", err)
		}
		domain = u.Hostname()
	}
	domain = firstNonEmpty(domain, pageHost)
	var out []webkit.Cookie
	for _, j := range jars {
		cs, err := parseCookieHeader(j, domain)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}

// shellWords splits a POSIX shell command line into words: blanks
// separate words; a backslash escapes the next character (and joins lines
// before a newline); '...' is literal; "..." keeps a backslash only before
// characters other than $, `, ", \ and newline; $'...' takes the ANSI-C
// escapes of bash. Expansions ($x, globs) are not performed.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\\':
			if i+1 < len(r) && r[i+1] == '\n' {
				i++
				continue
			}
			if i+1 < len(r) {
				i++
				cur.WriteRune(r[i])
			}
			inWord = true
		case c == '\'':
			end := indexRune(r, i+1, '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(string(r[i+1 : end]))
			i, inWord = end, true
		case c == '$' && i+1 < len(r) && r[i+1] == '\'':
			n, err := ansiC(r, i+2, &cur)
			if err != nil {
				return nil, err
			}
			i, inWord = n, true
		case c == '"':
			i++
			for ; i < len(r) && r[i] != '"'; i++ {
				if r[i] == '\\' && i+1 < len(r) && strings.ContainsRune("$`\"\\\n", r[i+1]) {
					i++
					if r[i] == '\n' {
						continue
					}
				}
				cur.WriteRune(r[i])
			}
			if i >= len(r) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func indexRune(r []rune, from int, c rune) int {
	for i := from; i < len(r); i++ {
		if r[i] == c {
			return i
		}
	}
	return -1
}

// ansiCEscapes are the single-character escapes of bash's $'...'.
var ansiCEscapes = map[rune]rune{
	'n': '\n', 'r': '\r', 't': '\t', 'a': '\a', 'b': '\b', 'e': 0x1b, 'E': 0x1b,
	'f': '\f', 'v': '\v', '\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// ansiC reads a $'...' word from r[i:] (after the opening quote) into b and
// returns the index of the closing quote.
func ansiC(r []rune, i int, b *strings.Builder) (int, error) {
	for ; i < len(r); i++ {
		c := r[i]
		if c == '\'' {
			return i, nil
		}
		if c != '\\' || i+1 >= len(r) {
			b.WriteRune(c)
			continue
		}
		i++
		if e, ok := ansiCEscapes[r[i]]; ok {
			b.WriteRune(e)
			continue
		}
		digits := map[rune]int{'x': 2, 'u': 4, 'U': 8}[r[i]]
		if digits == 0 {
			b.WriteRune('\\')
			b.WriteRune(r[i])
			continue
		}
		j := i + 1
		for j < len(r) && j < i+1+digits && strings.ContainsRune("0123456789abcdefABCDEF", r[j]) {
			j++
		}
		v, err := strconv.ParseUint(string(r[i+1:j]), 16, 32)
		if err != nil {
			return 0, fmt.Errorf("bad \\%c escape in a $'...' word", r[i])
		}
		if r[i] == 'x' {
			b.WriteByte(byte(v))
		} else {
			b.WriteRune(rune(v))
		}
		i = j - 1
	}
	return 0, errors.New("unterminated $'...' quote")
}

// DomainMatch reports whether host domain-matches a cookie domain (RFC
// 6265 section 5.1.3; the leading dot of a domain cookie is ignored as in
// section 5.2.3): the same name, or a subdomain when host is not an IP
// address. Case is ignored.
func DomainMatch(host, domain string) bool {
	h, d := strings.ToLower(host), strings.ToLower(strings.TrimPrefix(domain, "."))
	if d == "" {
		return false
	}
	return h == d || (strings.HasSuffix(h, "."+d) && net.ParseIP(h) == nil)
}

// PathMatch reports whether a request path path-matches a cookie path
// (RFC 6265 section 5.1.4): the same path, or a path below the cookie
// path. An empty request path is "/".
func PathMatch(reqPath, cookiePath string) bool {
	if reqPath == "" {
		reqPath = "/"
	}
	return reqPath == cookiePath || (strings.HasPrefix(reqPath, cookiePath) && (strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'))
}

// cookieKey identifies a cookie in a jar: name, domain and path.
type cookieKey struct{ name, domain, path string }

func keyOf(c webkit.Cookie) cookieKey {
	return cookieKey{c.Name, strings.ToLower(c.Domain), c.Path}
}

// expiredCookieMs is the expiry that DeleteCookies sets: one second after
// the Unix epoch, in milliseconds. Any past time deletes; 0 would make a
// session cookie (tools.SetCookie).
const expiredCookieMs = 1000

// DeleteCookies deletes exactly the cookies, each by its identity (name,
// domain with its leading dot, path), and reads the jar back. It fails,
// naming the cookies without their values, when any of them is still
// there.
//
// Each cookie gets one Page.setCookie of itself with an expiry in the
// past: a new cookie replaces the cookie of its name, domain and path, and
// an expired one is then evicted (RFC 6265 section 5.3), so no other
// cookie changes. WebKitGTK does so (libsoup soup_cookie_jar_add_cookie_full
// deletes the cookie of that domain, name and path; e2e/webkitgtk
// TestCDPCookiesStorageTracingProfilerOnWebKitGTK). iOS passes the cookie to
// NSHTTPCookieStorage setCookie (WebKit NetworkStorageSessionCocoa), which
// replaces the cookie of the same name, domain and path; that the expired
// cookie is not kept there is closed source and a device check
// (docs/notes/w5.md "W5c-1b"). Page.deleteCookie(name, url) is not used:
// on iOS it deletes every cookie of the name that the url gets (also a
// parent domain's or a shorter path's namesake), on WebKitGTK the first of
// them in libsoup's order, which can be a namesake.
//
// The expired cookie has sameSite Lax: sameSite is not part of the
// identity, and libsoup refuses a SameSite=None cookie without Secure
// before it looks for the cookie to replace.
func DeleteCookies(ctx context.Context, c webkit.Caller, cookies []webkit.Cookie) error {
	for _, ck := range cookies {
		expired := ck
		expired.Session, expired.Expires, expired.SameSite = false, expiredCookieMs, tools.DefaultSameSite
		if err := tools.SetCookie(ctx, c, expired); err != nil {
			return fmt.Errorf("deleting the cookie %s with Page.setCookie of an expired cookie: %w", cookieLabel(ck), err)
		}
	}
	return CheckCookiesGone(ctx, c, cookies)
}

// DeleteListedCookie deletes the cookie of ck's name, domain and path as
// DeleteCookies does, and reports whether Page.getCookies listed such a
// cookie before the delete. Page.getCookies lists only the cookies of the
// page's frame and resource URLs, so "not listed" does not prove that the
// jar had no such cookie.
func DeleteListedCookie(ctx context.Context, c webkit.Caller, ck webkit.Cookie) (listed bool, err error) {
	before, err := tools.GetCookies(ctx, c)
	if err != nil {
		return false, fmt.Errorf("reading the cookies before the delete: %w", err)
	}
	for _, b := range before {
		if keyOf(b) == keyOf(ck) {
			listed = true
		}
	}
	return listed, DeleteCookies(ctx, c, []webkit.Cookie{ck})
}

// CheckCookiesGone reads the jar and fails, naming the cookies without
// their values, when a cookie of the identity of one of cookies is there.
func CheckCookiesGone(ctx context.Context, c webkit.Caller, cookies []webkit.Cookie) error {
	after, err := tools.GetCookies(ctx, c)
	if err != nil {
		return fmt.Errorf("reading the cookies back: %w", err)
	}
	gone := map[cookieKey]bool{}
	for _, ck := range cookies {
		gone[keyOf(ck)] = true
	}
	var left []string
	for _, ck := range after {
		if gone[keyOf(ck)] {
			left = append(left, cookieLabel(ck))
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("WebKit kept %d cookie(s) after Page.setCookie with an expiry in the past: %s; the page may have set them again, or this WebKit does not delete a cookie that way: list the cookies and run the command again", len(left), strings.Join(left, ", "))
	}
	return nil
}

// cookieLabel names a cookie by its identity, never its value.
func cookieLabel(c webkit.Cookie) string {
	return fmt.Sprintf("%s (domain %s, path %s)", c.Name, c.Domain, c.Path)
}

// ClearCookies deletes the page's cookies (Page.getCookies: the cookies of
// the page's frames and resources, MAP 1.8). With domain, only cookies of
// that domain and its subdomains go. It returns the deleted cookies with
// their values removed.
func ClearCookies(ctx context.Context, c webkit.Caller, domain string) ([]webkit.Cookie, error) {
	all, err := tools.GetCookies(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("Page.getCookies: %w", err)
	}
	var targets []webkit.Cookie
	for _, ck := range all {
		if domain == "" || DomainMatch(strings.TrimPrefix(ck.Domain, "."), domain) {
			targets = append(targets, ck)
		}
	}
	if err := DeleteCookies(ctx, c, targets); err != nil {
		return nil, err
	}
	for i := range targets {
		targets[i].Value = ""
	}
	return targets, nil
}

// ImportCookies sets each cookie with Page.setCookie and its read-back
// (tools.SetCookieVerified). It sets every cookie it can and returns an
// error that names those that failed.
func ImportCookies(ctx context.Context, c webkit.Caller, cookies []webkit.Cookie) (int, error) {
	var failed []string
	set := 0
	for _, ck := range cookies {
		if _, err := tools.SetCookieVerified(ctx, c, ck); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		set++
	}
	if len(failed) > 0 {
		return set, fmt.Errorf("%d of %d cookies were not set: %s", len(failed), len(cookies), strings.Join(failed, "; "))
	}
	return set, nil
}
