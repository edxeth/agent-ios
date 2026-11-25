//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// w5c1bCookie is the part of a CDP Network.Cookie that the tests check.
type w5c1bCookie struct {
	Name, Value, Domain, Path string
	Expires                   float64
	Session                   bool
}

// w5c1bCookies returns the cookies of a getCookies reply.
func w5c1bCookies(t *testing.T, raw json.RawMessage) []w5c1bCookie {
	t.Helper()
	var r struct{ Cookies []w5c1bCookie }
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("getCookies reply %s: %v", raw, err)
	}
	return r.Cookies
}

// namedCookies returns "value domain path" of the cookies named name,
// sorted.
func namedCookies(cs []w5c1bCookie, name string) []string {
	var out []string
	for _, c := range cs {
		if c.Name == name {
			out = append(out, c.Value+" "+c.Domain+" "+c.Path)
		}
	}
	sort.Strings(out)
	return out
}

// docCookies returns the name=value pairs of document.cookie whose name is
// name, sorted.
func (p *cdpPage) docCookies(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, kv := range strings.Split(p.evalString(t, "document.cookie"), ";") {
		if kv = strings.TrimSpace(kv); strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

// tracingComplete waits for the page's Tracing.tracingComplete after seq
// and returns it.
func (p *cdpPage) tracingComplete(t *testing.T, seq int) cdptest.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e, err := p.c.WaitEvent(ctx, seq, "Tracing.tracingComplete", func(e cdptest.Event) bool { return e.SessionID == p.page })
	if err != nil {
		t.Fatalf("Tracing.tracingComplete: %v", err)
	}
	return e
}

// traceEventNames returns the names of the trace events.
func traceEventNames(t *testing.T, events []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, raw := range events {
		var e struct{ Name string }
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("trace event %s: %v", raw, err)
		}
		out = append(out, e.Name)
	}
	return out
}

// W5c-1b on WebKitGTK (design 5.3: Network and Storage cookies,
// Storage.clearDataForOrigin, Tracing with IO, Profiler): a CDP client of
// session s1 attaches through "get cdp-url" as Puppeteer does, and in the
// real browser
//   - a cookie set with Network.setCookie reads back through
//     Network.getCookies, Storage.getCookies on the browser session and
//     document.cookie;
//   - Network.deleteCookies, an expired Network.setCookie and "cookies
//     clear --domain" delete exactly their identities and keep the cookies
//     of the same name on another path, of the host's domain and of the
//     parent domain (Page.setCookie with an expiry in the past;
//     Page.deleteCookie(name, url) would delete namesakes);
//   - Storage.clearDataForOrigin clears localStorage and refuses a request
//     of storage types that iOS cannot clear;
//   - a trace holds the converted Timeline event of a script that the test
//     runs, through dataCollected and as a stream through IO.read and
//     IO.close;
//   - a profile holds the samples of a function that the test runs.
//
// The page runs on sub.app.localhost under /sub/: a domain cookie needs a
// host name, a parent domain cookie needs a parent domain, and
// Page.getCookies lists only the cookies that the page's URLs get, so a
// cookie of path /sub needs a page below it. Expected values
// come from the CDP definitions and RFC 6265, not from a browser run.
func TestCDPCookiesStorageTracingProfilerOnWebKitGTK(t *testing.T) {
	s := newSite(t, "sub.app.localhost")
	s.html("/sub/page.html", "<!doctype html><title>W5c-1b</title><p>w5c</p>")
	devtoolsBrowser(t, s.url+"/sub/page.html")
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	if r := cli(t, "cdp", "start", "--port", port); r.code != 0 {
		t.Fatalf("cdp start --port %s: exit %d, stderr %q", port, r.code, r.stderr)
	}
	mustCLI(t, "--session", "s1", "snapshot")
	ws := strings.TrimSpace(mustCLI(t, "--session", "s1", "get", "cdp-url"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer c.Close()
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	p := &cdpPage{c: c, tab: tab}
	p.attachPage(t)

	t.Run("cookie set and read back", func(t *testing.T) {
		var set struct{ Success bool }
		_ = json.Unmarshal(p.call(t, "Network.setCookie", map[string]any{"name": "w5c", "value": "one", "url": s.url + "/"}), &set)
		if !set.Success {
			t.Fatal("Network.setCookie: success false")
		}
		// A url sets a host-only session cookie of its host with path /.
		want := w5c1bCookie{Name: "w5c", Value: "one", Domain: "sub.app.localhost", Path: "/", Expires: -1, Session: true}
		got := w5c1bCookies(t, p.call(t, "Network.getCookies", map[string]any{"urls": []string{s.url + "/sub/page.html"}}))
		if i := slices.IndexFunc(got, func(c w5c1bCookie) bool { return c.Name == "w5c" }); i < 0 || got[i] != want {
			t.Fatalf("Network.getCookies: %+v, want %+v", got, want)
		}
		browser := w5c1bCookies(t, gtkCall(t, c, "", "Storage.getCookies", map[string]any{}))
		if i := slices.IndexFunc(browser, func(c w5c1bCookie) bool { return c.Name == "w5c" }); i < 0 || browser[i] != want {
			t.Fatalf("Storage.getCookies on the browser session: %+v, want %+v", browser, want)
		}
		if got := p.docCookies(t, "w5c"); !reflect.DeepEqual(got, []string{"w5c=one"}) {
			t.Fatalf("document.cookie w5c: %q, want w5c=one", got)
		}
	})

	t.Run("deleteCookies of one identity", func(t *testing.T) {
		for _, ck := range []map[string]any{
			{"name": "dup", "value": "target", "domain": "sub.app.localhost", "path": "/sub"},
			{"name": "dup", "value": "root", "domain": "sub.app.localhost", "path": "/"},
			{"name": "dup", "value": "domain", "domain": ".sub.app.localhost", "path": "/sub"},
			{"name": "dup", "value": "parent", "domain": ".app.localhost", "path": "/sub"},
			{"name": "dup", "value": "parentroot", "domain": ".app.localhost", "path": "/", "httpOnly": true},
		} {
			var set struct{ Success bool }
			_ = json.Unmarshal(p.call(t, "Network.setCookie", ck), &set)
			if !set.Success {
				t.Fatalf("Network.setCookie %v: success false", ck)
			}
		}
		// document.cookie omits the HttpOnly cookie.
		if got, want := p.docCookies(t, "dup"), []string{"dup=domain", "dup=parent", "dup=root", "dup=target"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("setup: document.cookie dup %q, want %q", got, want)
		}
		step := func(what string, want []string) {
			t.Helper()
			if got := namedCookies(w5c1bCookies(t, p.call(t, "Network.getCookies", map[string]any{})), "dup"); !reflect.DeepEqual(got, want) {
				t.Fatalf("dup cookies after %s: %q, want %q", what, got, want)
			}
		}
		p.call(t, "Network.deleteCookies", map[string]any{"name": "dup", "domain": "sub.app.localhost", "path": "/sub"})
		step("deleting the host-only one of /sub", []string{"domain .sub.app.localhost /sub", "parent .app.localhost /sub", "parentroot .app.localhost /", "root sub.app.localhost /"})
		if got, want := p.docCookies(t, "dup"), []string{"dup=domain", "dup=parent", "dup=root"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("document.cookie dup after the delete: %q, want %q", got, want)
		}
		p.call(t, "Network.deleteCookies", map[string]any{"name": "dup", "domain": ".sub.app.localhost"})
		step("deleting the host's domain cookie", []string{"parent .app.localhost /sub", "parentroot .app.localhost /", "root sub.app.localhost /"})
		var set struct{ Success bool }
		_ = json.Unmarshal(p.call(t, "Network.setCookie", map[string]any{"name": "dup", "value": "", "domain": ".app.localhost", "path": "/", "expires": 1}), &set)
		if !set.Success {
			t.Fatal("Network.setCookie of an expired cookie: success false")
		}
		step("an expired cookie of the parent domain's path /", []string{"parent .app.localhost /sub", "root sub.app.localhost /"})
		mustCLI(t, "--session", "s1", "cookies", "clear", "--domain", "sub.app.localhost")
		step("cookies clear --domain sub.app.localhost", []string{"parent .app.localhost /sub"})
	})

	// Reviewer repro of W5c-1b review R1 F4, kept: expires 0 (the Unix
	// epoch, a past date as in Chrome), and 0.0004 s, which rounds to 0 ms,
	// delete the cookie of that identity through each setter; neither
	// stores a session cookie.
	t.Run("reviewer_epoch_expiry", func(t *testing.T) {
		for _, method := range []string{"Network.setCookie", "Network.setCookies", "Storage.setCookies"} {
			for _, expiry := range []float64{0, 0.0004} {
				p.call(t, "Network.setCookie", map[string]any{"name": "epoch", "value": "old", "url": s.url + "/"})
				if got := namedCookies(w5c1bCookies(t, p.call(t, "Network.getCookies", map[string]any{})), "epoch"); !reflect.DeepEqual(got, []string{"old sub.app.localhost /"}) {
					t.Fatalf("setup before %s with expires %v: epoch cookies %q", method, expiry, got)
				}
				cp := map[string]any{"name": "epoch", "value": "replacement", "url": s.url + "/", "expires": expiry}
				var raw json.RawMessage
				switch method {
				case "Network.setCookie":
					raw = p.call(t, method, cp)
					var set struct{ Success bool }
					if err := json.Unmarshal(raw, &set); err != nil || !set.Success {
						t.Errorf("Network.setCookie with expires %v: %s, want success true", expiry, raw)
					}
				case "Network.setCookies":
					raw = p.call(t, method, map[string]any{"cookies": []any{cp}})
				default:
					raw = gtkCall(t, c, "", method, map[string]any{"cookies": []any{cp}})
				}
				if got := namedCookies(w5c1bCookies(t, p.call(t, "Network.getCookies", map[string]any{})), "epoch"); len(got) != 0 {
					t.Fatalf("%s with expires %v must delete epoch; response=%s; cookies=%v", method, expiry, raw, got)
				}
			}
		}
	})

	t.Run("clearDataForOrigin", func(t *testing.T) {
		if v := p.evalString(t, `localStorage.setItem("w5c", "local"); sessionStorage.setItem("w5c", "session"); String(localStorage.length)`); v == "0" {
			t.Fatalf("setup: localStorage.length %s", v)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := c.Call(ctx, "", "Storage.clearDataForOrigin", map[string]any{"origin": s.url, "storageTypes": "indexeddb,cache_storage"})
		if ce, ok := err.(*cdptest.Error); !ok || !strings.Contains(ce.Message, "cannot clear") {
			t.Fatalf("clearDataForOrigin of types iOS cannot clear: %v, want a refusal", err)
		}
		if v := p.evalString(t, `localStorage.getItem("w5c")`); v != "local" {
			t.Fatalf("the refused request cleared localStorage: %q", v)
		}
		gtkCall(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": s.url, "storageTypes": "local_storage"})
		if v := p.evalString(t, `String(localStorage.length)`); v != "0" {
			t.Fatalf("localStorage.length after clearDataForOrigin: %s, want 0", v)
		}
		if v := p.evalString(t, `sessionStorage.getItem("w5c")`); v != "session" {
			t.Fatalf("sessionStorage after clearDataForOrigin: %q, want it kept", v)
		}
	})

	// mark runs a script that the trace records: console.timeStamp makes a
	// Timeline TimeStamp record with its message.
	mark := func(t *testing.T, label string) string {
		t.Helper()
		m := fmt.Sprintf("w5c-%s-%d", label, time.Now().UnixNano())
		p.evalString(t, fmt.Sprintf(`(function w5cTraced() { console.timeStamp(%q); let x = 0; for (let i = 0; i < 1e5; i++) x += i; return String(x); })()`, m))
		return m
	}

	t.Run("trace through dataCollected", func(t *testing.T) {
		p.call(t, "Tracing.start", map[string]any{"categories": "devtools.timeline,v8.execute"})
		m := mark(t, "events")
		seq := c.Seq()
		p.call(t, "Tracing.end", nil)
		done := p.tracingComplete(t, seq)
		var events []json.RawMessage
		for _, e := range c.Events() {
			if e.Method == "Tracing.dataCollected" && e.SessionID == p.page && e.Seq > seq && e.Seq < done.Seq {
				var v struct{ Value []json.RawMessage }
				if err := json.Unmarshal(e.Params, &v); err != nil {
					t.Fatal(err)
				}
				events = append(events, v.Value...)
			}
		}
		if names := traceEventNames(t, events); !slices.Contains(names, "TimeStamp "+m) {
			t.Fatalf("dataCollected events %v, want TimeStamp %s", names, m)
		}
	})

	t.Run("trace as a stream", func(t *testing.T) {
		p.call(t, "Tracing.start", map[string]any{
			"transferMode": "ReturnAsStream",
			"traceConfig":  map[string]any{"includedCategories": []string{"devtools.timeline", "v8.execute"}, "excludedCategories": []string{"*"}},
		})
		m := mark(t, "stream")
		seq := c.Seq()
		p.call(t, "Tracing.end", nil)
		var tc struct{ Stream string }
		if err := json.Unmarshal(p.tracingComplete(t, seq).Params, &tc); err != nil || tc.Stream == "" {
			t.Fatalf("tracingComplete without a stream: %v", err)
		}
		var data strings.Builder
		for eof := false; !eof; {
			var r struct {
				Data string
				EOF  bool
			}
			if err := json.Unmarshal(p.call(t, "IO.read", map[string]any{"handle": tc.Stream, "size": 4096}), &r); err != nil {
				t.Fatal(err)
			}
			data.WriteString(r.Data)
			eof = r.EOF
		}
		var file struct{ TraceEvents []json.RawMessage }
		if err := json.Unmarshal([]byte(data.String()), &file); err != nil {
			t.Fatalf("the stream is no trace file: %v", err)
		}
		if names := traceEventNames(t, file.TraceEvents); !slices.Contains(names, "TimeStamp "+m) {
			t.Fatalf("stream trace events %v, want TimeStamp %s", names, m)
		}
		p.call(t, "IO.close", map[string]any{"handle": tc.Stream})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := c.Call(ctx, p.page, "IO.read", map[string]any{"handle": tc.Stream}); err == nil {
			t.Fatal("IO.read after IO.close succeeded")
		}
	})

	t.Run("profile", func(t *testing.T) {
		p.call(t, "Profiler.enable", nil)
		p.call(t, "Profiler.start", nil)
		// A named function that runs for 300 ms, so that the sampling
		// profiler takes samples with its frame.
		p.evalString(t, `(function w5cProfiledHotLoop() { const end = Date.now() + 300; let x = 0; while (Date.now() < end) x += Math.sqrt(x + 1); return String(x); })()`)
		var r struct {
			Profile struct {
				Nodes []struct {
					CallFrame struct{ FunctionName string }
					HitCount  int
				}
				Samples []int
			}
		}
		if err := json.Unmarshal(p.call(t, "Profiler.stop", nil), &r); err != nil {
			t.Fatal(err)
		}
		hits := 0
		for _, n := range r.Profile.Nodes {
			if n.CallFrame.FunctionName == "w5cProfiledHotLoop" {
				hits += n.HitCount
			}
		}
		if hits == 0 {
			t.Fatalf("the profile has no sample of w5cProfiledHotLoop (%d samples, %d nodes)", len(r.Profile.Samples), len(r.Profile.Nodes))
		}
		p.call(t, "Profiler.disable", nil)
	})
}
