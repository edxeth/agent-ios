// Package devtoolstest holds a fake page for the tests of the DevTools
// cookie, storage and state code (internal/devtools) and of the commands
// that use it (internal/commands). It does not import internal/devtools,
// so that package's own tests can use it.
package devtoolstest

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Page is a fake page at Origin: a cookie jar behind Page.getCookies,
// setCookie and deleteCookie, and localStorage and sessionStorage behind
// the storage function of internal/devtools (Runtime.evaluate of
// "(function iwdpStorage(a) {...})(args)"); "location.hostname" evaluates
// to the origin's host and "location.href" to the origin and URLPath. The
// jar follows WebKit: setCookie replaces the cookie of the same name,
// domain and path, and a cookie that expired
// (not a session cookie, expires not after jarNow) only removes that cookie
// (RFC 6265 section 5.3 steps 11 and 12 and its eviction of expired
// cookies; libsoup soup_cookie_jar_add_cookie_full, seen on WebKitGTK);
// deleteCookie(name, url) removes the cookies of that name that the jar
// would send to url (RFC 6265 domain and path match; a secure cookie only
// to https), as iOS does (NetworkStorageSessionCocoa deleteCookie).
//
// Lock Mu to read or change the fields while the page is in use.
type Page struct {
	Mu     sync.Mutex
	Origin string
	// URLPath is the path of the page's URL, which location.href ends
	// with ("/" when it is empty). When it is set, Page.getCookies lists
	// only the cookies that the jar would send to Origin+URLPath, as
	// WebKit lists the cookies of the page's document and resource URLs
	// (InspectorPageAgent::getCookies); when it is empty, Page.getCookies
	// lists the whole jar.
	URLPath        string
	Cookies        []webkit.Cookie
	LocalStorage   map[string]string
	SessionStorage map[string]string
	Evals          int // storage function calls
	// RestoreError makes a setItem of the storage function's apply op
	// throw, as the page script does at the storage quota: the apply
	// undoes its own changes and its reply has this error. With
	// RestoreErrorKey set, only a setItem of that key throws.
	RestoreError    string
	RestoreErrorKey string
	// Writes logs every change of the storage that the storage function
	// made, as "local set k", "session remove k" (the storage events that
	// other documents of the origin get).
	Writes []string
	// Reload, when set, runs before each apply op; it can change Origin or
	// drop the staged parts (Stage = nil), as a navigation does.
	Reload func(p *Page, apply int)
	// Stage is the apply op's staged parts (the page property
	// __iwdpStateStage), nil when the page has none.
	Stage *Stage
	// QuotaBytes, when not 0, is the quota of each storage area in
	// WebKit's count (WebKitBytes): a setItem of the apply op, also one of
	// its undo, that would make the area larger throws QuotaExceededError,
	// as StorageMap::setItem does (Source/WebCore/storage/StorageMap.cpp:
	// 91-118, WebKit commit f20844f96faf). Peak holds the largest size of
	// each area ("local", "session") after a write of an apply op; set it
	// to nil to measure a new load.
	QuotaBytes int
	Peak       map[string]int
	applies    int
	// BeforeCookieWrite, when set, runs with Mu held before a
	// Page.setCookie or Page.deleteCookie changes the jar: a page script or
	// a server response that changes cookies while a command runs. It may
	// change Cookies and BeforeCookieWrite.
	BeforeCookieWrite func(p *Page)

	Server *fakewebkit.Server
	Conn   *webkit.Session // a session to the page
}

// Stage is the staged parts of a state load in the page: the key parts
// of the next op, and the value parts per "<area>:<key>".
type Stage struct {
	Token string
	Key   string
	Parts map[string]string
}

// WebKitBytes is the size that WebKit counts for s against the storage
// quota: String::sizeInBytes (Source/WTF/wtf/text/WTFString.h:127) is the
// length in UTF-16 code units, times 1 for an 8-bit string and 2 for a
// 16-bit one. A string of Latin-1 characters (up to U+00FF) is 8-bit.
func WebKitBytes(s string) int {
	units, wide := 0, false
	for _, r := range s {
		units++
		if r > 0xFFFF {
			units++ // a surrogate pair
		}
		if r > 0xFF {
			wide = true
		}
	}
	if wide {
		return 2 * units
	}
	return units
}

// AreaBytes is the size of a storage area in WebKit's count: the bytes
// of every key and value.
func AreaBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += WebKitBytes(k) + WebKitBytes(v)
	}
	return n
}

// storageCallPrefix starts the expression of a storage function call,
// "(function iwdpStorage(a) {...})(<JSON argument>)".
const storageCallPrefix = "(function iwdpStorage(a) {"

// storageCallArgs returns the JSON argument of a storage function call. The
// argument is the text after the first "})(" that is followed by valid JSON
// up to the closing parenthesis: a value in the argument can hold "})("
// too, and the function's source has none that JSON follows.
func storageCallArgs(expr string) (string, bool) {
	if !strings.HasPrefix(expr, storageCallPrefix) || !strings.HasSuffix(expr, ")") {
		return "", false
	}
	for i := len(storageCallPrefix); ; {
		j := strings.Index(expr[i:], "})(")
		if j < 0 {
			return "", false
		}
		i += j + len("})(")
		if args := expr[i : len(expr)-1]; json.Valid([]byte(args)) {
			return args, true
		}
	}
}

// New starts a fake page and connects a session to it. Both close at test
// cleanup. The session has WebKitGTK's message limit (the smallest of the
// transports), so a message above it fails as on that transport.
func New(t testing.TB, origin string) *Page {
	t.Helper()
	p := &Page{Origin: origin, LocalStorage: map[string]string{}, SessionStorage: map[string]string{}}
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	p.Server = f
	f.Handle("Page.getCookies", p.getCookies)
	f.Handle("Page.setCookie", p.setCookie)
	f.Handle("Page.deleteCookie", p.deleteCookie)
	f.Handle("Runtime.evaluate", p.evaluate)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true, MaxOutgoing: transport.WebKitGTKMaxOutgoing})
	}, webkit.SessionOptions{CallTimeout: untilDeadline(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p.Conn = s
	return p
}

// untilDeadline is the call budget of the page's session: the time left
// until the test's own deadline (go test -timeout). The fake answers every
// call, so the budget only turns a hang into a failure, and a fixed budget
// does not fit: a call that carries megabytes of storage took more than
// webkit.DefaultCallTimeout under -race and a host load of 70 (devfix R3).
// Without a deadline (go test -timeout 0, or a benchmark) it is 0, which
// the session takes as webkit.DefaultCallTimeout.
func untilDeadline(t testing.TB) time.Duration {
	if dt, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if d, ok := dt.Deadline(); ok {
			return time.Until(d)
		}
	}
	return 0
}

// jarNow is the time of the jar's clock, which decides whether a cookie
// that Page.setCookie gets has expired. It is fixed, so that the expiries
// that the tests set (2023 and later) stay in the future on any day the
// tests run; a delete by expiry uses an earlier time (1970 in
// devtools.DeleteCookies, Puppeteer's expires 1).
var jarNow = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

func key(c webkit.Cookie) [3]string { return [3]string{c.Name, strings.ToLower(c.Domain), c.Path} }

func (p *Page) getCookies(fakewebkit.Call) (any, *fakewebkit.Error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.URLPath == "" {
		return map[string]any{"cookies": append([]webkit.Cookie{}, p.Cookies...)}, nil
	}
	u, err := url.Parse(p.Origin)
	if err != nil {
		return nil, &fakewebkit.Error{Code: -32000, Message: err.Error()}
	}
	listed := []webkit.Cookie{}
	for _, ck := range p.Cookies {
		if sentTo(u.Scheme, u.Hostname(), p.URLPath, ck) {
			listed = append(listed, ck)
		}
	}
	return map[string]any{"cookies": listed}, nil
}

func (p *Page) setCookie(c fakewebkit.Call) (any, *fakewebkit.Error) {
	var v struct{ Cookie webkit.Cookie }
	if err := json.Unmarshal(c.Params, &v); err != nil {
		return nil, &fakewebkit.Error{Code: -32602, Message: err.Error()}
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.BeforeCookieWrite != nil {
		p.BeforeCookieWrite(p)
	}
	for i, old := range p.Cookies {
		if key(old) == key(v.Cookie) {
			p.Cookies = append(p.Cookies[:i], p.Cookies[i+1:]...)
			break
		}
	}
	if v.Cookie.Session || v.Cookie.Expires > float64(jarNow) {
		p.Cookies = append(p.Cookies, v.Cookie)
	}
	return nil, nil
}

func (p *Page) deleteCookie(c fakewebkit.Call) (any, *fakewebkit.Error) {
	var v struct{ CookieName, URL string }
	_ = json.Unmarshal(c.Params, &v)
	u, err := url.Parse(v.URL)
	if err != nil {
		return nil, &fakewebkit.Error{Code: -32602, Message: err.Error()}
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.BeforeCookieWrite != nil {
		p.BeforeCookieWrite(p)
	}
	kept := p.Cookies[:0]
	for _, ck := range p.Cookies {
		if !sentTo(u.Scheme, u.Hostname(), u.Path, ck) || ck.Name != v.CookieName {
			kept = append(kept, ck)
		}
	}
	p.Cookies = kept
	return nil, nil
}

// sentTo reports whether the jar sends ck to a URL of scheme, host and
// path: RFC 6265 domain and path match, and a secure cookie only to https.
func sentTo(scheme, host, path string, ck webkit.Cookie) bool {
	return domainMatch(host, ck.Domain) && pathMatch(path, ck.Path) && (!ck.Secure || scheme == "https")
}

// domainMatch is RFC 6265 section 5.1.3 (a leading dot of the cookie
// domain ignored).
func domainMatch(host, domain string) bool {
	h, d := strings.ToLower(host), strings.ToLower(strings.TrimPrefix(domain, "."))
	return d != "" && (h == d || (strings.HasSuffix(h, "."+d) && net.ParseIP(h) == nil))
}

// pathMatch is RFC 6265 section 5.1.4.
func pathMatch(reqPath, cookiePath string) bool {
	if reqPath == "" {
		reqPath = "/"
	}
	return reqPath == cookiePath || (strings.HasPrefix(reqPath, cookiePath) && (strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'))
}

func pairs(m map[string]string) [][2]string {
	out := [][2]string{}
	for k, v := range m {
		out = append(out, [2]string{k, v})
	}
	return out
}

func stringResult(s string) map[string]any {
	return map[string]any{"result": map[string]any{"type": "string", "value": s}}
}

func (p *Page) evaluate(c fakewebkit.Call) (any, *fakewebkit.Error) {
	var v struct{ Expression string }
	_ = json.Unmarshal(c.Params, &v)
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if v.Expression == "location.hostname" {
		u, _ := url.Parse(p.Origin)
		return stringResult(u.Hostname()), nil
	}
	if v.Expression == "location.href" {
		path := p.URLPath
		if path == "" {
			path = "/"
		}
		return stringResult(strings.TrimSuffix(p.Origin, "/") + path), nil
	}
	args, ok := storageCallArgs(v.Expression)
	if !ok {
		return nil, &fakewebkit.Error{Code: -32000, Message: "the fake page evaluates only the storage function, location.hostname and location.href"}
	}
	var a struct {
		Op, Area, Key, Value, Origin, Token string
		First, Last                         bool
		Ops                                 []struct {
			A, K  string
			V     *string
			P, KP bool
		}
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, &fakewebkit.Error{Code: -32000, Message: err.Error()}
	}
	p.Evals++
	r := map[string]any{"origin": p.Origin}
	area := map[string]map[string]string{"local": p.LocalStorage, "session": p.SessionStorage}[a.Area]
	switch a.Op {
	case "list":
		r["items"] = pairs(area)
	case "get":
		val, ok := area[a.Key]
		r["found"], r["value"] = ok, val
	case "set":
		area[a.Key] = a.Value
	case "clear":
		r["cleared"] = len(area)
		clear(area)
	case "snapshot":
		r["local"], r["session"] = pairs(p.LocalStorage), pairs(p.SessionStorage)
	case "apply":
		// As the page script: origin check, the staged parts with the
		// load's token, writes only of changed keys, an undo of the
		// apply's own writes when a setItem throws.
		p.applies++
		if p.Reload != nil {
			p.Reload(p, p.applies)
			r["origin"] = p.Origin
		}
		if a.Origin != p.Origin {
			r["wrongOrigin"] = true
			break
		}
		if a.First {
			p.Stage = &Stage{Token: a.Token, Parts: map[string]string{}}
		} else if p.Stage == nil || p.Stage.Token != a.Token {
			r["error"] = "the page was reloaded while the state loaded"
			break
		}
		areas := map[string]map[string]string{"local": p.LocalStorage, "session": p.SessionStorage}
		type undoItem struct {
			area, key string
			old       *string
		}
		var undo []undoItem
		// write changes the storage as removeItem or setItem; a setItem
		// above QuotaBytes changes nothing and returns the error.
		write := func(area, key string, v *string) string {
			m := areas[area]
			if v == nil {
				delete(m, key)
				p.Writes = append(p.Writes, area+" remove "+key)
			} else {
				old, had := m[key]
				size := AreaBytes(m) - WebKitBytes(old) + WebKitBytes(*v)
				if !had {
					size = AreaBytes(m) + WebKitBytes(key) + WebKitBytes(*v)
				}
				if p.QuotaBytes > 0 && size > p.QuotaBytes {
					return "QuotaExceededError: The quota has been exceeded."
				}
				m[key] = *v
				p.Writes = append(p.Writes, area+" set "+key)
			}
			if p.Peak == nil {
				p.Peak = map[string]int{}
			}
			p.Peak[area] = max(p.Peak[area], AreaBytes(m))
			return ""
		}
		for _, op := range a.Ops {
			if op.KP {
				p.Stage.Key += op.K
				continue
			}
			k := p.Stage.Key + op.K
			id := op.A + ":" + k
			if op.P {
				p.Stage.Parts[id] += *op.V
				continue
			}
			p.Stage.Key = ""
			old, had := areas[op.A][k]
			var oldp *string
			if had {
				oldp = &old
			}
			if op.V == nil {
				if had {
					undo = append(undo, undoItem{op.A, k, oldp})
					write(op.A, k, nil)
				}
				continue
			}
			v := p.Stage.Parts[id] + *op.V
			delete(p.Stage.Parts, id)
			if had && old == v {
				continue
			}
			if p.RestoreError != "" && (p.RestoreErrorKey == "" || p.RestoreErrorKey == k) {
				r["error"] = p.RestoreError
				break
			}
			if e := write(op.A, k, &v); e != "" {
				r["error"] = e
				break
			}
			undo = append(undo, undoItem{op.A, k, oldp})
		}
		if r["error"] != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				if e := write(undo[i].area, undo[i].key, undo[i].old); e != "" {
					r["undoError"] = e
				}
			}
		}
		r["changed"] = len(undo)
		if a.Last || r["error"] != nil {
			p.Stage = nil
		}
	default:
		return nil, &fakewebkit.Error{Code: -32000, Message: "unknown storage op " + a.Op}
	}
	data, _ := json.Marshal(r)
	return stringResult(string(data)), nil
}
