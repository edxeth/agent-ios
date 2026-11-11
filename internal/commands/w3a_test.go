package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Tests of the W3a command files (snapshot, diff snapshot, get, is, find,
// hit-test, read) on the fake WebKit endpoint: the page side is scripted,
// so these tests check the Go paths (arguments, ref table, output). The
// page runtime itself runs in the WebKitGTK suite (e2e/webkitgtk).

// fakePage answers runtime calls with scripted envelopes and records them.
type fakePage struct {
	mu      sync.Mutex
	calls   []pageCall
	replies map[string]any // fn -> envelope
}

type pageCall struct {
	Fn   string
	Args map[string]any
}

func (p *fakePage) set(fn string, envelope any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replies[fn] = envelope
}

func (p *fakePage) handler(_ fakewebkit.Call, fn string, args json.RawMessage) any {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pageCall{fn, a})
	if r, ok := p.replies[fn]; ok {
		return r
	}
	if fn == "ping" {
		return okEnv(true)
	}
	return map[string]any{"ok": false, "error": map[string]string{"code": "unsupported", "message": "fake page has no " + fn}}
}

// last returns the arguments of the last call of fn, or nil.
func (p *fakePage) last(fn string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.calls) - 1; i >= 0; i-- {
		if p.calls[i].Fn == fn {
			return p.calls[i].Args
		}
	}
	return nil
}

func (p *fakePage) fns() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		out = append(out, c.Fn)
	}
	return out
}

func okEnv(v any) map[string]any { return map[string]any{"ok": true, "value": v} }

func errEnv(code, msg string) map[string]any {
	return map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}}
}

type pageEnv struct{ ps *core.PageSession }

func (e pageEnv) Page(context.Context) (*core.PageSession, error) { return e.ps, nil }
func (pageEnv) Emit(string)                                       {}
func (pageEnv) ResolvePath(p string) string                       { return p }

// rtHarness is one fake page with a page session on it.
type rtHarness struct {
	page *fakePage
	ps   *core.PageSession
	gen  uint64
	mu   sync.Mutex
	f    *fakewebkit.Server
	sess *webkit.Session
}

func newHarness(t *testing.T) *rtHarness {
	t.Helper()
	return newHarnessWith(t, transport.Limits{Routed: true})
}

// newHarnessWith is newHarness on a connection with the limits lim.
func newHarnessWith(t *testing.T, lim transport.Limits) *rtHarness {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	h := &rtHarness{page: &fakePage{replies: map[string]any{}}, gen: 1, f: f}
	f.SetRuntime(h.page.handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, lim)
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h.sess = s
	rt := runtime.New(s, func() uint64 { h.mu.Lock(); defer h.mu.Unlock(); return h.gen })
	h.ps = &core.PageSession{Caller: s, Runtime: rt, Refs: refs.NewTable()}
	return h
}

func (h *rtHarness) setGen(g uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen = g
}

func (h *rtHarness) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), append([]string{"--timeout", "5s"}, args...), &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return pageEnv{h.ps}, nil })

	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

func (h *rtHarness) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 {
		t.Fatalf("%s: exit %d: %s", strings.Join(args, " "), code, errOut)
	}
	return out
}

// snapshotReply is the page's snapshot value: two refs of a document.
func snapshotReply(next int, doc string) map[string]any {
	return okEnv(map[string]any{
		"tree": []any{
			map[string]any{"role": "button", "name": "Save", "ref": "e1"},
			map[string]any{"role": "link", "name": "Home", "ref": "e2", "url": "http://x.test/"},
		},
		"refs": []any{
			map[string]any{"ref": "e1", "role": "button", "name": "Save", "unique": true},
			map[string]any{"ref": "e2", "role": "link", "name": "Home", "unique": false},
		},
		"nextRef": next, "doc": doc, "url": "http://x.test/", "title": "X",
	})
}

// Design 3.2 text format and 3.3 allocation: the snapshot passes the
// table's counter as nextRef, renders the page's nodes, and records each
// ref with its role, name, uniqueness and document for later targets.
func TestSnapshotRendersTreeAndRecordsRefs(t *testing.T) {
	h := newHarness(t)
	h.page.set("snapshot", snapshotReply(3, "doc-1"))
	got := h.mustRun(t, "snapshot")
	if want := "- button \"Save\" [ref=e1]\n- link \"Home\" [ref=e2]"; got != want {
		t.Fatalf("snapshot:\n%s\nwant:\n%s", got, want)
	}
	if a := h.page.last("snapshot"); a["nextRef"] != float64(1) {
		t.Fatalf("first snapshot passed %v, want nextRef 1", a)
	}
	if n := h.ps.Refs.Next(); n != 3 {
		t.Fatalf("table counter after the snapshot = %d, want the page's nextRef 3", n)
	}
	got = h.mustRun(t, "snapshot", "-u")
	if !strings.Contains(got, "[ref=e2, url=http://x.test/]") {
		t.Fatalf("snapshot -u = %q, want url= on the link", got)
	}
	if a := h.page.last("snapshot"); a["nextRef"] != float64(3) {
		t.Fatalf("second snapshot passed %v, want nextRef 3", a)
	}
	h.page.set("get", okEnv("Save"))
	if got := h.mustRun(t, "get", "text", "@e1"); got != "Save" {
		t.Fatalf("get text @e1 = %q", got)
	}
	want := map[string]any{"ref": "e1", "doc": "doc-1", "role": "button", "name": "Save", "unique": true}
	if tg := h.page.last("get")["target"]; fmt.Sprint(tg) != fmt.Sprint(want) {
		t.Fatalf("get text @e1 sent target %v, want %v", tg, want)
	}
	h.mustRun(t, "get", "text", "e2") // agent-browser accepts "e2" and "ref=e2"
	if tg := h.page.last("get")["target"].(map[string]any); tg["ref"] != "e2" || tg["unique"] != nil {
		t.Fatalf("get text e2 sent target %v, want ref e2 that is not unique", tg)
	}
}

// --json returns the node tree plus url, title, generation and refs (design 3.2).
func TestSnapshotJSON(t *testing.T) {
	h := newHarness(t)
	h.page.set("snapshot", snapshotReply(3, "doc-1"))
	h.setGen(4)
	var v struct {
		Snapshot   string
		Tree       []runtime.Node
		URL, Title string
		Generation uint64
		Refs       map[string]struct{ Role, Name string }
	}
	if err := json.Unmarshal([]byte(h.mustRun(t, "--json", "snapshot")), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Tree) != 2 || v.URL != "http://x.test/" || v.Title != "X" || v.Generation != 4 ||
		v.Refs["e1"].Role != "button" || v.Refs["e2"].Name != "Home" || !strings.HasPrefix(v.Snapshot, "- button") {
		t.Fatalf("snapshot --json = %+v", v)
	}
}

// Snapshot flags reach the renderer and the page; -s takes a selector or a
// ref; a negative depth is a usage error before any page call.
func TestSnapshotFlags(t *testing.T) {
	h := newHarness(t)
	h.page.set("snapshot", okEnv(map[string]any{
		"tree": []any{map[string]any{"role": "list", "children": []any{
			map[string]any{"role": "listitem", "children": []any{map[string]any{"role": "StaticText", "name": "Alpha"}}},
			map[string]any{"role": "listitem", "children": []any{map[string]any{"role": "link", "name": "Beta", "ref": "e1"}}},
		}}},
		"refs": []any{map[string]any{"ref": "e1", "role": "link", "name": "Beta", "unique": true}}, "nextRef": 2, "doc": "d",
	}))
	if got := h.mustRun(t, "snapshot", "-i"); got != "- link \"Beta\" [ref=e1]" {
		t.Fatalf("snapshot -i = %q", got)
	}
	if got := h.mustRun(t, "snapshot", "--depth", "1"); got != "- list\n  - listitem\n  - listitem" {
		t.Fatalf("snapshot -d 1 = %q", got)
	}
	if got := h.mustRun(t, "snapshot", "-c", "-s", "#nav"); got != "- list\n  - listitem\n    - StaticText \"Alpha\"\n  - listitem\n    - link \"Beta\" [ref=e1]" {
		t.Fatalf("snapshot -c = %q", got)
	}
	if s := h.page.last("snapshot")["scope"]; fmt.Sprint(s) != fmt.Sprint(map[string]any{"selector": "#nav"}) {
		t.Fatalf("snapshot -s #nav sent scope %v", s)
	}
	h.mustRun(t, "snapshot", "-s", "@e1")
	if s := h.page.last("snapshot")["scope"].(map[string]any); s["ref"] != "e1" || s["doc"] != "d" {
		t.Fatalf("snapshot -s @e1 sent scope %v", s)
	}
	if code, _, errOut := h.run("snapshot", "-s", "@e99"); code != ExitError || !strings.Contains(errOut, "no snapshot produced @e99") {
		t.Fatalf("snapshot -s with an unknown ref: exit %d, %q", code, errOut)
	}
	n := len(h.page.fns())
	if code, _, errOut := h.run("snapshot", "-d", "-1"); code != ExitUsage || !strings.Contains(errOut, "depth") {
		t.Fatalf("snapshot -d -1: exit %d, %q", code, errOut)
	}
	if len(h.page.fns()) != n {
		t.Fatal("a usage error reached the page")
	}
}

// Design 3.3 resolve order steps 1 and 2 run in the daemon table: an
// unknown ref and a ref from an older generation fail without a page call
// that reads the element. One ping picks up a reset the page reports, so
// a daemon restart gives its own message.
func TestRefLookupErrors(t *testing.T) {
	h := newHarness(t)
	code, _, errOut := h.run("get", "text", "@e7")
	if code != ExitError || !strings.Contains(errOut, "unknown_ref: no snapshot produced @e7") {
		t.Fatalf("unknown ref: exit %d, %q", code, errOut)
	}
	if fns := h.page.fns(); strings.Join(fns, ",") != "ping" {
		t.Fatalf("unknown ref made page calls %v, want one ping", fns)
	}
	h.page.set("ping", map[string]any{"ok": true, "value": true, "reset": map[string]any{"reason": "daemon", "maxRef": 9}})
	if code, _, errOut := h.run("get", "text", "@e7"); code != ExitError || !strings.Contains(errOut, "@e7: daemon restarted; run snapshot") {
		t.Fatalf("ref after a daemon restart: exit %d, %q", code, errOut)
	}
	if n := h.ps.Refs.Next(); n != 10 {
		t.Fatalf("table counter after the reset report = %d, want 10 (past the page's e9)", n)
	}
	h.page.set("ping", okEnv(true))
	h.page.set("snapshot", snapshotReply(12, "doc-1"))
	h.mustRun(t, "snapshot")
	h.setGen(2)
	before := len(h.page.fns())
	if code, _, errOut := h.run("is", "visible", "@e1"); code != ExitError || !strings.Contains(errOut, "stale_ref: page navigated since the snapshot that produced @e1; run snapshot") {
		t.Fatalf("ref after navigation: exit %d, %q", code, errOut)
	}
	if len(h.page.fns()) != before {
		t.Fatalf("a stale ref reached the page: %v", h.page.fns()[before:])
	}
}

// Getter outputs (design 4.2 rows of get): plain text, numbers, the box as
// compact JSON and styles as "name: value" lines; page warnings follow as
// "warning:" lines; a page error keeps its code.
func TestGetOutputs(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		args  []string
		reply any
		want  string
		sent  map[string]any
	}{
		{[]string{"get", "title"}, okEnv("Probes"), "Probes", map[string]any{"what": "title"}},
		{[]string{"get", "url"}, okEnv("http://x.test/"), "http://x.test/", map[string]any{"what": "url"}},
		{[]string{"get", "count", "input"}, okEnv(5), "5", map[string]any{"what": "count", "target": map[string]any{"selector": "input"}}},
		{[]string{"get", "attr", "#link", "href"}, okEnv("#x"), "#x", map[string]any{"what": "attr", "target": map[string]any{"selector": "#link"}, "name": "href"}},
		{[]string{"get", "attr", "#link", "rel"}, okEnv(nil), "", nil},
		{
			[]string{"get", "box", "#box"},
			okEnv(map[string]any{"x": 10, "y": 20.5, "width": 100, "height": 50, "visible": true, "enabled": false}),
			`{"x":10,"y":20.5,"width":100,"height":50,"visible":true,"enabled":false}`, nil,
		},
		{
			[]string{"get", "styles", "#box", "position, width"},
			okEnv([]any{[]any{"position", "absolute"}, []any{"width", "100px"}}),
			"position: absolute\nwidth: 100px",
			map[string]any{"what": "styles", "target": map[string]any{"selector": "#box"}, "name": "position, width"},
		},
		{[]string{"get", "text", "#a"}, map[string]any{"ok": true, "value": "Sign In", "warnings": []string{"@e2 was replaced"}}, "Sign In\nwarning: @e2 was replaced", nil},
	} {
		h.page.set("get", c.reply)
		if got := h.mustRun(t, c.args...); got != c.want {
			t.Errorf("%s = %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
		if c.sent != nil {
			if a := h.page.last("get"); fmt.Sprint(a) != fmt.Sprint(c.sent) {
				t.Errorf("%s sent %v, want %v", strings.Join(c.args, " "), a, c.sent)
			}
		}
	}
	h.page.set("get", errEnv("ambiguous", "\"input\" matches 5 elements"))
	if code, _, errOut := h.run("get", "text", "input"); code != ExitError || !strings.Contains(errOut, "ambiguous: \"input\" matches 5 elements") {
		t.Fatalf("page error: exit %d, %q", code, errOut)
	}
	var v map[string]any
	h.page.set("get", okEnv(5))
	if err := json.Unmarshal([]byte(h.mustRun(t, "--json", "get", "count", "li")), &v); err != nil || v["value"] != float64(5) {
		t.Fatalf("get count --json = %v, %v", v, err)
	}
}

// Wrong getter arguments are usage errors (exit 2) that never reach the page.
func TestGetIsFindUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"get", "bogus", "#a"},
		{"get", "text"},
		{"get", "attr", "#a"},
		{"get", "title", "#a"},
		{"get", "text", "#a", "extra"},
		{"get", "count"},
		{"is", "bogus", "#a"},
		{"find", "bogus", "x"},
		{"find", "nth", "x", "input"},
		{"find", "nth", "1"},
		// find with an action is valid since W3b (TestFindWithAction); an
		// unknown action or a missing action argument is a usage error.
		{"find", "text", "Under", "explode"},
		{"find", "first", "input", "fill"},
		{"hit-test", "a", "1"},
		{"hit-test", "1", "NaN"},
		{"read", "ftp://x.test/"},
	} {
		if code, _, errOut := h.run(args...); code != ExitUsage {
			t.Errorf("%s: exit %d (%s), want %d", strings.Join(args, " "), code, errOut, ExitUsage)
		}
	}
	if fns := h.page.fns(); len(fns) != 0 {
		t.Fatalf("usage errors made page calls %v", fns)
	}
}

func TestIsOutputs(t *testing.T) {
	h := newHarness(t)
	h.page.set("is", okEnv(true))
	if got := h.mustRun(t, "is", "visible", "#box"); got != "true" {
		t.Fatalf("is visible = %q", got)
	}
	if a := h.page.last("is"); fmt.Sprint(a) != fmt.Sprint(map[string]any{"state": "visible", "target": map[string]any{"selector": "#box"}}) {
		t.Fatalf("is visible sent %v", a)
	}
	h.page.set("is", okEnv(false))
	for _, state := range []string{"enabled", "checked", "editable", "focused"} {
		if got := h.mustRun(t, "is", state, "#x"); got != "false" {
			t.Fatalf("is %s = %q", state, got)
		}
	}
}

// find (design 4.2): the locator and its options go to the page, every
// match is a snapshot line with a ref, and the refs resolve later.
// agent-browser's nth is 0-based ("find --help" of agent-browser 0.41.2:
// "nth <index> <selector>  Nth matching element (0-based)").
func TestFind(t *testing.T) {
	h := newHarness(t)
	h.page.set("find", okEnv(map[string]any{
		"nodes":   []any{map[string]any{"ref": "e4", "role": "link", "name": "Go"}},
		"refs":    []any{map[string]any{"ref": "e4", "role": "link", "name": "Go", "unique": true}},
		"nextRef": 5, "doc": "doc-f",
	}))
	if got := h.mustRun(t, "find", "role", "link", "--name", "Go", "--exact"); got != "- link \"Go\" [ref=e4]" {
		t.Fatalf("find role = %q", got)
	}
	want := map[string]any{"kind": "role", "value": "link", "name": "Go", "exact": true, "nextRef": float64(1)}
	if a := h.page.last("find"); fmt.Sprint(a) != fmt.Sprint(want) {
		t.Fatalf("find role sent %v, want %v", a, want)
	}
	h.mustRun(t, "find", "nth", "1", "input")
	want = map[string]any{"kind": "nth", "value": "input", "index": float64(1), "nextRef": float64(5)}
	if a := h.page.last("find"); fmt.Sprint(a) != fmt.Sprint(want) {
		t.Fatalf("find nth sent %v, want %v", a, want)
	}
	h.mustRun(t, "find", "nth", "--", "-1", "input")
	if a := h.page.last("find"); a["index"] != float64(-1) {
		t.Fatalf("find nth -1 sent %v, want index -1 (last, as agent-browser)", a)
	}
	h.page.set("get", okEnv("Go"))
	h.mustRun(t, "get", "text", "@e4")
	if tg := h.page.last("get")["target"].(map[string]any); tg["doc"] != "doc-f" || tg["unique"] != true {
		t.Fatalf("ref from find resolves with %v", tg)
	}
	h.page.set("find", errEnv("not_found", "no element matches find text \"zzz\""))
	if code, _, errOut := h.run("find", "text", "zzz"); code != ExitError || !strings.Contains(errOut, "not_found") {
		t.Fatalf("find without a match: exit %d, %q", code, errOut)
	}
}

func TestHitTest(t *testing.T) {
	h := newHarness(t)
	h.page.set("hitTest", okEnv(map[string]any{
		"nodes":   []any{map[string]any{"ref": "e3", "role": "generic"}},
		"refs":    []any{map[string]any{"ref": "e3", "role": "generic", "name": "", "unique": false}},
		"nextRef": 4, "doc": "d", "element": "<div id=\"box\">", "box": map[string]any{"x": 10, "y": 20, "width": 100.5, "height": 50},
	}))
	if got := h.mustRun(t, "hit-test", "50", "45.5"); got != "- generic [ref=e3] <div id=\"box\"> x=10 y=20 width=100.5 height=50" {
		t.Fatalf("hit-test = %q", got)
	}
	if a := h.page.last("hitTest"); a["x"] != float64(50) || a["y"] != 45.5 || a["nextRef"] != float64(1) {
		t.Fatalf("hit-test sent %v", a)
	}
	if h.ps.Refs.Next() != 4 {
		t.Fatalf("hit-test did not record its ref; counter %d", h.ps.Refs.Next())
	}
}

func TestReadPage(t *testing.T) {
	h := newHarness(t)
	h.page.set("read", okEnv("# Title\n## Part"))
	if got := h.mustRun(t, "read", "--outline"); got != "# Title\n## Part" {
		t.Fatalf("read --outline = %q", got)
	}
	if a := h.page.last("read"); a["outline"] != true {
		t.Fatalf("read --outline sent %v", a)
	}
}

// read <url> fetches on the host and needs no page (design 4.2 row read).
func TestReadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
			return
		case "/plain":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "  just text\n")
			return
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "\x89PNG")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>T</title><style>p{}</style><script>var x = "hidden";</script></head>
<body><h1>Main  title</h1><p>Hello <b>world</b>&nbsp;!</p><div hidden>secret</div><nav aria-hidden="true">menu</nav>
<ul><li>One</li><li>Two</li></ul><h3>Sub</h3><noscript>no js</noscript><template><p>tpl</p></template></body></html>`)
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	noPage := func(context.Context, *Invocation) (Env, error) { return fakeEnv{}, nil }
	if code := RunCLIStarted(context.Background(), []string{"read", srv.URL + "/"}, &out, &errb, nil, noPage); code != 0 {
		t.Fatalf("read url: exit %d: %s", code, errb.String())
	}
	if got, want := strings.TrimRight(out.String(), "\n"), "Main title\n\nHello world !\n\nOne\nTwo\n\nSub"; got != want {
		t.Fatalf("read url =\n%q\nwant\n%q", got, want)
	}
	out.Reset()
	if code := RunCLIStarted(context.Background(), []string{"read", "--outline", srv.URL + "/"}, &out, &errb, nil, noPage); code != 0 {
		t.Fatalf("read --outline url: exit %d: %s", code, errb.String())
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "# Main title\n### Sub" {
		t.Fatalf("read --outline url = %q", got)
	}
	errb.Reset()
	if code := RunCLIStarted(context.Background(), []string{"read", srv.URL + "/missing"}, &out, &errb, nil, noPage); code != ExitError || !strings.Contains(errb.String(), "404") {
		t.Fatalf("read of a 404: exit %d, %q", code, errb.String())
	}
	// Plain text is returned as it is; other content types are an error.
	out.Reset()
	if code := RunCLIStarted(context.Background(), []string{"read", srv.URL + "/plain"}, &out, &errb, nil, noPage); code != 0 || out.String() != "just text\n" {
		t.Fatalf("read of text/plain: exit %d, %q", code, out.String())
	}
	errb.Reset()
	if code := RunCLIStarted(context.Background(), []string{"read", srv.URL + "/image"}, &out, &errb, nil, noPage); code != ExitError || !strings.Contains(errb.String(), "image/png") {
		t.Fatalf("read of an image: exit %d, %q", code, errb.String())
	}
}

// diff snapshot compares the last two snapshots of the session (design
// 4.2): a unified line diff with the counts, as agent-browser prints it.
func TestDiffSnapshot(t *testing.T) {
	h := newHarness(t)
	if code, _, errOut := h.run("diff", "snapshot"); code != ExitError || !strings.Contains(errOut, "run snapshot") {
		t.Fatalf("diff without snapshots: exit %d, %q", code, errOut)
	}
	h.page.set("snapshot", snapshotReply(3, "d"))
	h.mustRun(t, "snapshot")
	h.mustRun(t, "snapshot")
	if got := h.mustRun(t, "diff", "snapshot"); got != "No changes detected" {
		t.Fatalf("diff of equal snapshots = %q", got)
	}
	h.page.set("snapshot", okEnv(map[string]any{
		"tree": []any{map[string]any{"role": "button", "name": "Save", "ref": "e1"}, map[string]any{"role": "button", "name": "Undo", "ref": "e3"}},
		"refs": []any{}, "nextRef": 4, "doc": "d",
	}))
	h.mustRun(t, "snapshot")
	want := "--- before\n+++ after\n@@ -1,2 +1,2 @@\n - button \"Save\" [ref=e1]\n-- link \"Home\" [ref=e2]\n+- button \"Undo\" [ref=e3]\n\n1 additions, 1 removals, 1 unchanged"
	if got := h.mustRun(t, "diff", "snapshot"); got != want {
		t.Fatalf("diff snapshot =\n%s\nwant\n%s", got, want)
	}
}

// A page session without the runtime (a daemon that did not wire W3a) is
// an actionable error, not a nil dereference.
func TestRuntimeCommandsNeedRuntime(t *testing.T) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), []string{"get", "title"}, &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return pageEnv{&core.PageSession{}}, nil })

	if code != ExitError || !strings.Contains(errb.String(), "agent runtime") {
		t.Fatalf("exit %d, %q", code, errb.String())
	}
	errb.Reset()
	code = RunCLIStarted(context.Background(), []string{"diff", "snapshot"}, &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return pageEnv{&core.PageSession{}}, nil })

	if code != ExitError || !strings.Contains(errb.String(), "agent runtime") {
		t.Fatalf("diff snapshot without refs: exit %d, %q", code, errb.String())
	}
}

// With a scheduler, the runtime's owner is the lease holder and epoch
// (design 3.1 owner epoch), so the page resets its maps for a new holder.
func TestOwnerFollowsLease(t *testing.T) {
	h := newHarness(t)
	sched := core.NewScheduler(h.sess, core.SchedulerOptions{})
	defer sched.Close()
	sched.SetLease("agent-1", 4)
	h.ps.Sched = sched
	h.page.set("get", okEnv("T"))
	h.mustRun(t, "get", "title")
	o, ok := h.ps.Runtime.Owner()
	if !ok || o != (runtime.Owner{Daemon: runtime.DaemonID, Session: "agent-1", Epoch: 4}) {
		t.Fatalf("owner = %+v, %v", o, ok)
	}
	evals := h.f.ReceivedMethod("Runtime.evaluate")
	if !strings.Contains(string(evals[len(evals)-1].Params), `\"session\":\"agent-1\",\"epoch\":4`) {
		t.Fatalf("the call does not carry the lease owner: %s", evals[len(evals)-1].Params)
	}
	// A reset report reaches the session's ref table.
	h.page.set("get", map[string]any{"ok": true, "value": "T", "reset": map[string]any{"reason": "epoch", "maxRef": 30}})
	h.mustRun(t, "get", "title")
	if n := h.ps.Refs.Next(); n != 31 {
		t.Fatalf("counter after an epoch reset = %d, want 31", n)
	}
}

// Design 3.1 and 3.3: refs of an earlier owner are unknown. The session
// keeps its ref table across a new lease epoch, so the table still knows
// @e1; the owner change must reach the page (one ping, whose reset report
// clears the table) before the table hands the page a spec that the page
// could rebind by role and name.
func TestKnownRefAfterLeaseEpochChangeIsUnknown(t *testing.T) {
	h := newHarness(t)
	sched := core.NewScheduler(h.sess, core.SchedulerOptions{})
	defer sched.Close()
	sched.SetLease("agent-1", 1)
	h.ps.Sched = sched
	h.page.set("snapshot", snapshotReply(3, "doc-1"))
	h.mustRun(t, "snapshot")
	h.page.set("get", okEnv("Save"))
	if got := h.mustRun(t, "get", "text", "@e1"); got != "Save" {
		t.Fatalf("get text @e1 under the snapshot's lease = %q", got)
	}
	sched.SetLease("agent-1", 2)
	h.page.set("ping", map[string]any{"ok": true, "value": true, "reset": map[string]any{"reason": "epoch", "maxRef": 2}})
	before := len(h.page.fns())
	code, _, errOut := h.run("get", "text", "@e1")
	if code != ExitError || !strings.Contains(errOut, "unknown_ref: @e1: the lease of this page changed since the snapshot that produced it; run snapshot") {
		t.Fatalf("ref of the earlier lease epoch: exit %d, %q", code, errOut)
	}
	if fns := h.page.fns()[before:]; strings.Join(fns, ",") != "ping" {
		t.Fatalf("ref of the earlier lease epoch made page calls %v, want one ping", fns)
	}
	// The owner is confirmed now: a known ref of the new epoch needs no ping.
	h.page.set("ping", okEnv(true))
	h.page.set("snapshot", snapshotReply(5, "doc-1"))
	h.mustRun(t, "snapshot")
	before = len(h.page.fns())
	h.mustRun(t, "get", "text", "@e1")
	if fns := h.page.fns()[before:]; strings.Join(fns, ",") != "get" {
		t.Fatalf("known ref under a confirmed owner made page calls %v, want get only", fns)
	}
}

// The W3a commands are registry entries (design 4.2).
func TestW3aCommandsRegistered(t *testing.T) {
	for _, name := range []string{"snapshot", "diff snapshot", "get", "is", "find", "hit-test", "read"} {
		c, n := Lookup(strings.Fields(name))
		if c == nil || n != len(strings.Fields(name)) || c.Name != name {
			t.Errorf("command %q: %+v", name, c)
		}
	}
	c, _ := Lookup([]string{"snapshot"})
	flags := map[string]bool{}
	for _, f := range c.Flags {
		flags[f.Name] = true
	}
	for _, f := range []string{"interactive", "compact", "depth", "selector", "urls"} {
		if !flags[f] {
			t.Errorf("snapshot has no flag --%s (design 4.2)", f)
		}
	}
	for _, c := range []string{"snapshot", "find", "hit-test"} {
		if cmd, _ := Lookup(strings.Fields(c)); !cmd.Mutates {
			t.Errorf("%s allocates refs and must be a mutation (design 2.11)", c)
		}
	}
}

var _ = errors.New
