//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/tools"
)

// W4-2b commands on WebKitGTK (design docs/design/agentic-parity.md,
// section 4.2 rows storage, cookies, state, vitals, diff screenshot,
// trace and profiler). The first tests use the suite's shared page
// connection; trace and profiler keep DevTools state and run through a
// daemon of their own.

// storageCleanup leaves the shared page without the cookies and storage
// these tests made.
func storageCleanup(t *testing.T) {
	t.Cleanup(func() {
		ps := pageSession("d1", "w4b", 1)
		run(t, ps, "cookies", "clear")
		run(t, ps, "storage", "local", "clear")
		run(t, ps, "storage", "session", "clear")
	})
}

// storage local|session runs the page's real localStorage and
// sessionStorage: the values set by the command are what page script
// reads, and clear empties them.
func TestDevtoolsStorageCommands(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	mustRun(t, ps, "storage", "local", "clear")
	mustRun(t, ps, "storage", "local", "set", "theme", "dark é")
	mustRun(t, ps, "storage", "session", "set", "authToken", "tok-1")
	if got := string(eval(t, "localStorage.getItem('theme') + '|' + sessionStorage.getItem('authToken')")); got != `"dark é|tok-1"` {
		t.Fatalf("page storage %s; want dark é|tok-1", got)
	}
	if out := mustRun(t, ps, "storage", "local", "theme"); out != "dark é" {
		t.Fatalf("storage local theme = %q", out)
	}
	if out := mustRun(t, ps, "storage", "session"); !strings.HasPrefix(out, "authToken=[masked]") {
		t.Fatalf("storage session = %q; want the token masked", out)
	}
	if out := mustRun(t, ps, "storage", "session", "--show-values"); out != "authToken=tok-1" {
		t.Fatalf("storage session --show-values = %q", out)
	}
	if out := mustRun(t, ps, "storage", "local", "clear"); !strings.HasPrefix(out, "Cleared 1 localStorage item on "+origin) {
		t.Fatalf("clear: %q", out)
	}
	if got := string(eval(t, "localStorage.length")); got != "0" {
		t.Fatalf("localStorage.length after clear = %s", got)
	}
}

// cookies set (one cookie and an import), cookies clear and state save and
// load against the real WebKitGTK cookie jar and storage: state load
// restores exactly what state save wrote.
func TestDevtoolsCookiesAndState(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	dir := t.TempDir()
	mustRun(t, ps, "cookies", "clear")
	mustRun(t, ps, "cookies", "set", "one", "v1", "--path", "/")
	curl := filepath.Join(dir, "c.curl")
	if err := os.WriteFile(curl, []byte("curl '"+origin+"/forms.html' -H 'Cookie: two=v2; three=v3'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, ps, "cookies", "set", "--curl", curl); !strings.HasPrefix(out, "Imported 2 cookies") {
		t.Fatalf("import: %q", out)
	}
	if got := string(eval(t, "document.cookie.split('; ').sort().join(';')")); got != `"one=v1;three=v3;two=v2"` {
		t.Fatalf("document.cookie = %s; want the three cookies", got)
	}
	mustRun(t, ps, "storage", "local", "set", "k", "v")
	mustRun(t, ps, "storage", "session", "set", "s", "w")
	stateFile := filepath.Join(dir, "state.json")
	mustRun(t, ps, "state", "save", stateFile)
	fi, err := os.Stat(stateFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v; want mode 0600", fi, err)
	}
	saved, _ := os.ReadFile(stateFile)
	want, err := devtools.DecodeState(saved)
	if err != nil || len(want.Cookies) != 3 || len(want.Origins[0].LocalStorage) != 1 || want.Origins[0].Origin != origin {
		t.Fatalf("state file %s: %v", saved, err)
	}

	// Change everything, then load.
	if out := mustRun(t, ps, "cookies", "clear", "--domain", "127.0.0.1"); !strings.HasPrefix(out, "Deleted 3 cookies") {
		t.Fatalf("cookies clear: %q", out)
	}
	if got := string(eval(t, "document.cookie")); got != `""` {
		t.Fatalf("document.cookie after clear = %s", got)
	}
	mustRun(t, ps, "cookies", "set", "extra", "x")
	eval(t, "localStorage.clear(), localStorage.setItem('junk', '1'), sessionStorage.clear()")
	mustRun(t, ps, "state", "load", stateFile)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := devtools.SaveState(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	got.SavedAt = want.SavedAt
	gd, _ := devtools.EncodeState(got)
	wd, _ := devtools.EncodeState(want)
	if string(gd) != string(wd) {
		t.Fatalf("state after load\n%s\nwant\n%s", gd, wd)
	}
}

// vitals on WebKitGTK: navigation and paint timing have values; LCP has a
// value exactly when the engine lists largest-contentful-paint (MAP 1.12).
func TestDevtoolsVitals(t *testing.T) {
	navigate(t, "forms.html", "")
	ps := pageSession("d1", "w4b", 1)
	code, out, errOut := run(t, ps, "--json", "vitals")
	if code != 0 {
		t.Fatalf("vitals: exit %d, %s", code, errOut)
	}
	var v devtools.Vitals
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("vitals JSON %q: %v", out, err)
	}
	for name, m := range map[string]devtools.VitalsMetric{"ttfb": v.TTFB, "domContentLoaded": v.DOMContentLoaded, "load": v.Load} {
		if m.Value == nil || *m.Value < 0 {
			t.Errorf("%s = %+v; want a value", name, m)
		}
	}
	hasLCP := string(eval(t, "PerformanceObserver.supportedEntryTypes.includes('largest-contentful-paint')")) == "true"
	if (v.LCP.Value != nil || v.LCP.Unavailable == "no largest-contentful-paint entry yet") != hasLCP {
		t.Errorf("LCP %+v, but the engine lists largest-contentful-paint: %v", v.LCP, hasLCP)
	}
	t.Logf("WebKitGTK vitals: %s", out)
}

// diff screenshot: a baseline taken from the page matches, and a changed
// page differs.
func TestDevtoolsDiffScreenshot(t *testing.T) {
	navigate(t, "forms.html", "")
	ps := pageSession("d1", "w4b", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url, err := tools.CaptureScreenshot(ctx, sess, tools.ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "base.png")
	if err := os.WriteFile(base, png, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, ps, "diff", "screenshot", "--baseline", base); !strings.HasPrefix(out, "No pixel differs") {
		t.Fatalf("unchanged page: %q", out)
	}
	eval(t, "document.body.style.background = 'rgb(255, 0, 0)', 1")
	t.Cleanup(func() { eval(t, "document.body.style.background = '', 1") })
	time.Sleep(200 * time.Millisecond)
	out := mustRun(t, ps, "diff", "screenshot", "--baseline", base)
	if !regexp.MustCompile(`^[1-9][0-9]* of [0-9]+ pixels differ`).MatchString(out) {
		t.Fatalf("changed page: %q; want differing pixels", out)
	}
}

// trace and profiler through the daemon on WebKitGTK: the trace file loads
// as Chrome trace JSON with Timeline events, the profile is a CDP profile
// rooted at (root), and both files have mode 0600. The trace records the
// Timeline only: the profiler and a trace with script tracking cannot
// share WebKit's one ScriptProfiler run (devtools.scriptProfilerFree;
// before, the profiler silently joined the trace's run, which has no
// samples).
func TestDevtoolsTraceAndProfilerThroughTheDaemon(t *testing.T) {
	addr := runtimeBrowser(t, "forms.html")
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() { cli(t, "daemon", "stop") })
	dir := t.TempDir()
	for _, args := range [][]string{{"trace", "start", "--categories", "timeline"}, {"profiler", "start"}} {
		if r := cli(t, append([]string{"--session", "w4b"}, args...)...); r.code != 0 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	// Page activity while recording: storage writes and the vitals
	// function run script in the page.
	for _, args := range [][]string{{"storage", "local", "set", "busy", "1"}, {"vitals"}, {"storage", "local", "clear"}} {
		if r := cli(t, append([]string{"--session", "w4b"}, args...)...); r.code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	trace := filepath.Join(dir, "t.json")
	if r := cli(t, "--session", "w4b", "trace", "stop", trace); r.code != 0 || !strings.Contains(r.stdout, "Trace written to "+trace) {
		t.Fatalf("trace stop: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	prof := filepath.Join(dir, "p.cpuprofile")
	if r := cli(t, "--session", "w4b", "profiler", "stop", prof); r.code != 0 || !strings.Contains(r.stdout, "CPU profile written to "+prof) {
		t.Fatalf("profiler stop: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for _, f := range []string{trace, prof} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v; want mode 0600", f, fi, err)
		}
	}
	data, _ := os.ReadFile(trace)
	var tr devtools.Trace
	if err := json.Unmarshal(data, &tr); err != nil || tr.DisplayTimeUnit != "ms" {
		t.Fatalf("trace JSON: %v", err)
	}
	events := 0
	for _, e := range tr.TraceEvents {
		if e.Ph == "X" || e.Ph == "I" {
			events++
			if e.Ts < 0 {
				t.Fatalf("event %+v before the recording start", e)
			}
		}
	}
	if events == 0 {
		t.Fatalf("the trace of real page activity has no events: %s", data)
	}
	var p devtools.CPUProfile
	data, _ = os.ReadFile(prof)
	if err := json.Unmarshal(data, &p); err != nil || len(p.Nodes) == 0 || p.Nodes[0].CallFrame.FunctionName != "(root)" || len(p.Samples) != len(p.TimeDeltas) {
		t.Fatalf("profile: %v %s", err, data)
	}
	t.Logf("trace: %d events; profile: %d samples, %d nodes", events, len(p.Samples), len(p.Nodes))
}

// liveState is the page's storage and the cookies that page script sees,
// as one JSON text.
func liveState(t *testing.T) string {
	t.Helper()
	return string(eval(t, "JSON.stringify({local: Object.assign({}, localStorage), session: Object.assign({}, sessionStorage), cookie: document.cookie.split('; ').sort().join(';')})"))
}

// state load of a file with an empty storage area on the real page (W4
// storage review, finding storage.go:55): the page script used to get no
// list for an empty area and threw after it had cleared both areas. Each
// case loads exactly the saved storage and cookies.
func TestDevtoolsStateLoadWithAnEmptyStorageArea(t *testing.T) {
	for _, area := range []string{"both empty", "local empty", "session empty"} {
		t.Run(area, func(t *testing.T) {
			navigate(t, "forms.html", "")
			storageCleanup(t)
			ps := pageSession("d1", "w4b", 1)
			mustRun(t, ps, "cookies", "clear")
			mustRun(t, ps, "storage", "local", "clear")
			mustRun(t, ps, "storage", "session", "clear")
			mustRun(t, ps, "cookies", "set", "kept", "c1")
			switch area {
			case "local empty":
				mustRun(t, ps, "storage", "session", "set", "saved", "v")
			case "session empty":
				mustRun(t, ps, "storage", "local", "set", "saved", "v")
			}
			path := filepath.Join(t.TempDir(), "state.json")
			mustRun(t, ps, "state", "save", path)
			want := liveState(t)
			mustRun(t, ps, "storage", "local", "set", "junk", "1")
			mustRun(t, ps, "storage", "session", "set", "junk", "2")
			mustRun(t, ps, "cookies", "set", "junk", "3")
			if code, out, errOut := run(t, ps, "state", "load", path); code != 0 {
				t.Fatalf("state load: exit %d, %q, %q; storage now %s", code, out, errOut, liveState(t))
			}
			if got := liveState(t); got != want {
				t.Fatalf("after state load: %s, want %s", got, want)
			}
		})
	}
}

// A state load that the page cannot complete leaves the earlier values
// (W4 storage review: never clear live storage before the restore is
// known to succeed). A page-side setItem that throws for one key of the
// file stands in for an unexpected storage failure (QuotaExceededError
// after the size check passed): the page puts the earlier items back, and
// the cookies are not touched.
func TestDevtoolsStateLoadThatFailsChangesNothing(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	mustRun(t, ps, "cookies", "clear")
	mustRun(t, ps, "cookies", "set", "live", "c1")
	mustRun(t, ps, "storage", "local", "set", "live", "1")
	mustRun(t, ps, "storage", "session", "set", "live", "2")
	before := liveState(t)
	st := devtools.State{
		Version: devtools.StateVersion,
		Cookies: []devtools.StateCookie{{Name: "fromfile", Value: "f", Domain: "127.0.0.1", Path: "/", Expires: -1, SameSite: "Lax"}},
		Origins: []devtools.StateOrigin{{
			Origin:         origin,
			LocalStorage:   []devtools.StorageEntry{{Name: "a", Value: "1"}, {Name: "poison", Value: "x"}},
			SessionStorage: []devtools.StorageEntry{{Name: "s", Value: "w"}},
		}},
	}
	data, err := devtools.EncodeState(st)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	eval(t, `(function () {
  var set = Storage.prototype.setItem;
  window.__iwdpSetItem = set;
  Storage.prototype.setItem = function (k, v) {
    if (k === "poison") { throw new DOMException("the quota has been exceeded", "QuotaExceededError"); }
    return set.call(this, k, v);
  };
})()`)
	t.Cleanup(func() { eval(t, "Storage.prototype.setItem = window.__iwdpSetItem") })
	code, out, errOut := run(t, ps, "state", "load", path)
	if code == 0 || !strings.Contains(errOut, "QuotaExceededError") || !strings.Contains(errOut, "not changed") {
		t.Fatalf("state load: exit %d, %q, %q; want the page's QuotaExceededError and nothing changed", code, out, errOut)
	}
	if after := liveState(t); after != before {
		t.Fatalf("a failed state load changed the page: %s, want %s", after, before)
	}
}

// storageEventRecorder adds a same-origin iframe to the page that records
// the storage events it gets (W4 repair round 3, brief item 5): other
// documents of the origin see every write of a state load. It returns a
// function that returns the events recorded since the last call, as
// "<area> <key> <oldValue> -> <newValue>" (a value as JSON, null for none).
func storageEventRecorder(t *testing.T) func() []string {
	t.Helper()
	eval(t, `(function () {
  var f = document.createElement("iframe");
  f.id = "iwdp-storage-events";
  document.body.appendChild(f);
  window.__iwdpEvents = [];
  f.contentWindow.addEventListener("storage", function (e) {
    var area = e.storageArea === f.contentWindow.localStorage ? "local" : "session";
    window.__iwdpEvents.push(area + " " + e.key + " " + JSON.stringify(e.oldValue) + " -> " + JSON.stringify(e.newValue));
  });
})()`)
	t.Cleanup(func() { eval(t, `document.getElementById("iwdp-storage-events").remove()`) })
	return func() []string {
		// Events reach the iframe in a later task of the page.
		time.Sleep(200 * time.Millisecond)
		var events []string
		raw := eval(t, "JSON.stringify(window.__iwdpEvents.splice(0))")
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(text), &events); err != nil {
			t.Fatal(err)
		}
		return events
	}
}

func writeState(t *testing.T, local, session []devtools.StorageEntry) string {
	t.Helper()
	data, err := devtools.EncodeState(devtools.State{
		Version: devtools.StateVersion, Cookies: []devtools.StateCookie{},
		Origins: []devtools.StateOrigin{{Origin: origin, LocalStorage: local, SessionStorage: session}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A state load writes only the difference (brief W4-R3 item 5): another
// document of the origin gets a storage event for each removed, changed
// and added key, and none for a key that keeps its value or for a clear.
func TestDevtoolsStateLoadSignalsOnlyRealChanges(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("same", "1"), localStorage.setItem("gone", "x"), localStorage.setItem("change", "old"), sessionStorage.setItem("keep", "s")`)
	events := storageEventRecorder(t)
	_ = events()
	path := writeState(t,
		[]devtools.StorageEntry{{Name: "same", Value: "1"}, {Name: "change", Value: "new"}, {Name: "add", Value: "v"}},
		[]devtools.StorageEntry{{Name: "keep", Value: "s"}})
	mustRun(t, ps, "state", "load", path)
	want := []string{`local gone "x" -> null`, `local change "old" -> "new"`, `local add null -> "v"`}
	if got := events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("storage events of the load: %q, want %q", got, want)
	}
	// The same state again changes nothing and signals nothing.
	mustRun(t, ps, "state", "load", path)
	if got := events(); len(got) != 0 {
		t.Fatalf("a load without changes signalled %q", got)
	}
}

// A state over the storage quota is refused before any write (brief W4-R3
// item 5): no storage event, the page keeps its storage and cookies.
func TestDevtoolsStateLoadOverTheQuotaSignalsNothing(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	mustRun(t, ps, "cookies", "clear")
	mustRun(t, ps, "cookies", "set", "live", "c1")
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("authToken", "live-session")`)
	before := liveState(t)
	events := storageEventRecorder(t)
	_ = events()
	// 4.8 M ASCII characters: above the load's limit of 4718592 bytes
	// (WebKit's 5 MiB quota at 1 byte per Latin-1 character, 10% kept
	// free).
	path := writeState(t, []devtools.StorageEntry{{Name: "a", Value: "1"}, {Name: "big", Value: strings.Repeat("x", 4_800_000)}}, nil)
	code, out, errOut := run(t, ps, "state", "load", path)
	if code == 0 || !strings.Contains(errOut, "localStorage") || !strings.Contains(errOut, "not changed") {
		t.Fatalf("state load: exit %d, %q, %q; want a refusal that names localStorage and says nothing changed", code, out, errOut)
	}
	if got := events(); len(got) != 0 {
		t.Fatalf("a refused load signalled %q", got)
	}
	if after := liveState(t); after != before {
		t.Fatalf("a refused load changed the page: %s, want %s", after, before)
	}
}

// Brief W4-R3 item 6 (storage review finding state.go:178): a state with a
// 140,000-byte value, above WebKitGTK's 127 KiB inspector message limit,
// saves and loads; the load is sent in several messages.
func TestDevtoolsStateWithAValueAboveTheMessageLimit(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("payload", "x".repeat(140000)), sessionStorage.setItem("saved", "v")`)
	path := filepath.Join(t.TempDir(), "state.json")
	mustRun(t, ps, "state", "save", path)
	want := liveState(t)
	eval(t, `localStorage.clear(), localStorage.setItem("live", "keep"), sessionStorage.clear(), sessionStorage.setItem("live", "keep")`)
	events := storageEventRecorder(t)
	_ = events()
	mustRun(t, ps, "state", "load", path)
	if got := liveState(t); got != want {
		t.Fatalf("after state load the storage differs from the saved state (%d bytes, want %d)", len(got), len(want))
	}
	got := events()
	sort.Strings(got)
	if wantEvents := []string{`local live "keep" -> null`, `local payload null -> "` + strings.Repeat("x", 140000) + `"`, `session live "keep" -> null`, `session saved null -> "v"`}; !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("storage events of the load: %d events, want one per changed key (%d)", len(got), len(wantEvents))
	}
	if n := eval(t, "typeof window.__iwdpStateStage"); string(n) != `"undefined"` {
		t.Fatalf("the load left its staged parts in the page: %s", n)
	}
}

// storageIs reports whether the page's localStorage holds exactly the
// items of want, each value given as a JavaScript expression (the values
// are too large to compare as JSON text).
func storageIs(t *testing.T, want map[string]string) bool {
	t.Helper()
	conds := []string{"localStorage.length === " + strconv.Itoa(len(want))}
	for k, v := range want {
		conds = append(conds, "localStorage.getItem("+strconv.Quote(k)+") === "+v)
	}
	return string(eval(t, strings.Join(conds, " && "))) == "true"
}

// localLengths returns the lengths of the page's localStorage values, as
// JSON text, for failure messages.
func localLengths(t *testing.T) string {
	t.Helper()
	return string(eval(t, "JSON.stringify(Object.keys(localStorage).map(function (k) { return [k.length, localStorage.getItem(k).length]; }))"))
}

// W4 repair round 4, storage review finding restore.go:138 (the
// reviewer's reproduction): WebKit's quota counts bytes, 1 per character
// of Latin-1 text and 2 per UTF-16 code unit of other text. The page holds
// a = 3,000,000 and z = 2,000,000 ASCII characters (5,000,002 bytes); the
// state has a = 2,000,000 and z = 300,000 U+0100 characters (4,600,002
// bytes). Both fit the 5 MiB quota, but a is shorter and larger: written
// before z shrinks, it needs 6,000,002 bytes. The load must shrink z
// first.
func TestDevtoolsStateLoadOrdersWritesByWebKitBytes(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("a", "x".repeat(3000000)), localStorage.setItem("z", "x".repeat(2000000))`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := devtools.State{Version: devtools.StateVersion, Cookies: []devtools.StateCookie{}, Origins: []devtools.StateOrigin{{
		Origin: origin, LocalStorage: []devtools.StorageEntry{{Name: "a", Value: strings.Repeat("\u0100", 2_000_000)}, {Name: "z", Value: strings.Repeat("\u0100", 300_000)}},
	}}}
	if _, err := devtools.LoadState(ctx, sess, st); err != nil {
		t.Fatalf("load of a state that fits the quota: %v; value lengths now %s", err, localLengths(t))
	}
	if !storageIs(t, map[string]string{"a": `"\u0100".repeat(2000000)`, "z": `"\u0100".repeat(300000)`}) {
		t.Fatalf("after the load the value lengths are %s; want a 2000000 and z 300000 U+0100", localLengths(t))
	}
}

// W4 repair round 4, storage review finding restore.go:333 (the
// reviewer's reproduction): the undo of a failed load must also free
// space before it uses space. The page holds a = 2,000,000 U+0100 and b =
// 200,000 ASCII characters (4,200,002 bytes). The state has a = 100,000
// and b = 2,000,000 U+0100 characters and a new zz; a page-side setItem
// that throws for zz stands in for a failure late in the load. The undo
// must shrink b before a grows back (in the load's order it needs
// 8,000,002 bytes), and the page must end with its earlier values.
func TestDevtoolsStateLoadUndoOrdersWritesByWebKitBytes(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("a", "\u0100".repeat(2000000)), localStorage.setItem("b", "x".repeat(200000))`)
	eval(t, `(function () {
  var set = Storage.prototype.setItem;
  window.__iwdpUndoSetItem = set;
  Storage.prototype.setItem = function (k, v) {
    if (k === "zz") { throw new DOMException("injected late failure", "QuotaExceededError"); }
    return set.call(this, k, v);
  };
})()`)
	t.Cleanup(func() { eval(t, "Storage.prototype.setItem = window.__iwdpUndoSetItem") })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := devtools.State{Version: devtools.StateVersion, Cookies: []devtools.StateCookie{}, Origins: []devtools.StateOrigin{{
		Origin: origin, LocalStorage: []devtools.StorageEntry{
			{Name: "a", Value: strings.Repeat("\u0100", 100_000)},
			{Name: "b", Value: strings.Repeat("\u0100", 2_000_000)},
			{Name: "zz", Value: strings.Repeat("z", 200_000)},
		},
	}}}
	_, err := devtools.LoadState(ctx, sess, st)
	if err == nil || !strings.Contains(err.Error(), "injected late failure") || !strings.Contains(err.Error(), "the earlier storage values were put back") {
		t.Fatalf("load: %v; want the injected failure and the earlier values put back", err)
	}
	if !storageIs(t, map[string]string{"a": `"\u0100".repeat(2000000)`, "b": `"x".repeat(200000)`}) {
		t.Fatalf("after the failed load the value lengths are %s; want the earlier a (2000000) and b (200000)", localLengths(t))
	}
}

// W4 repair round 4: the strings that a state load writes count 1 byte
// per character in WebKit's quota when they hold only Latin-1 characters
// (restore.go webkitBytes relies on it). 4,500,000 Latin-1 characters,
// some of them escaped in the page script (<), fit the 5 MiB quota only
// at 1 byte per character; at 2 bytes they would need 9,000,002.
func TestDevtoolsStateLoadCountsLatin1AsOneByte(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	eval(t, `localStorage.clear(), sessionStorage.clear()`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := devtools.State{Version: devtools.StateVersion, Cookies: []devtools.StateCookie{}, Origins: []devtools.StateOrigin{{
		Origin: origin, LocalStorage: []devtools.StorageEntry{{Name: "k", Value: strings.Repeat("\u00e9\u00ffa<", 1_125_000)}},
	}}}
	if _, err := devtools.LoadState(ctx, sess, st); err != nil {
		t.Fatalf("load of 4500000 Latin-1 characters: %v", err)
	}
	if !storageIs(t, map[string]string{"k": `"\u00e9\u00ffa<".repeat(1125000)`}) {
		t.Fatalf("after the load the value lengths are %s", localLengths(t))
	}
}

// W4 repair round 4, storage review finding state.go:110: a key larger
// than one inspector message (WebKitGTK's 127 KiB) saves and loads; keys
// are sent in parts as values are. The session key also holds
// characters that the page script escapes.
func TestDevtoolsStateWithAKeyAboveTheMessageLimit(t *testing.T) {
	navigate(t, "forms.html", "")
	storageCleanup(t)
	ps := pageSession("d1", "w4b", 1)
	eval(t, `localStorage.clear(), sessionStorage.clear(), localStorage.setItem("k".repeat(140000), "v"), sessionStorage.setItem("\u00e9<\"" + "k".repeat(140000), "\u4e2d".repeat(50000))`)
	path := filepath.Join(t.TempDir(), "state.json")
	mustRun(t, ps, "state", "save", path)
	want := liveState(t)
	eval(t, `localStorage.clear(), localStorage.setItem("live", "keep"), localStorage.setItem("k".repeat(140001), "x"), sessionStorage.clear()`)
	mustRun(t, ps, "state", "load", path)
	if got := liveState(t); got != want {
		t.Fatalf("after state load the storage differs from the saved state (%d bytes, want %d)", len(got), len(want))
	}
	if n := eval(t, "typeof window.__iwdpStateStage"); string(n) != `"undefined"` {
		t.Fatalf("the load left its staged parts in the page: %s", n)
	}
}
