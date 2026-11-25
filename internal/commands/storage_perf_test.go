package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/devtoolstest"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of the W4-2b command files: cookies clear and set (import),
// storage, state save and load, trace, profiler, vitals and diff
// screenshot (design docs/design/agentic-parity.md, section 4.2).

// spCommands are the W4-2b commands (design 4.2). TestCookiesSetHelpAndFlags
// covers "cookies set".
var spCommands = []string{
	"cookies clear", "storage", "state save", "state load", "trace start", "trace stop",
	"profiler start", "profiler stop", "vitals", "diff screenshot",
}

// spRun runs a command line against page session ps (no daemon).
func spRun(ps *core.PageSession, dir string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), append([]string{"--timeout", "10s"}, args...), &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return spEnv{ps: ps, dir: dir}, nil })

	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

// spEnv is a command environment: a page session, an operation when the
// command runs inside the manager, and a working directory for paths.
type spEnv struct {
	ps  *core.PageSession
	mo  *core.ManagedOp
	dir string
}

func (e spEnv) Page(context.Context) (*core.PageSession, error) { return e.ps, nil }
func (spEnv) Emit(string)                                       {}
func (e spEnv) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.dir, p)
}

func (e spEnv) Op() *core.Op {
	if e.mo == nil {
		return nil
	}
	return e.mo.Op
}

// Design 2.11 and 4.2: every W4-2b command has help and refuses an
// unknown flag with exit 2 before it builds an environment (no
// connection).
func TestStoragePerfCommandsHelpAndFlags(t *testing.T) {
	for _, name := range spCommands {
		c, n := Lookup(strings.Fields(name))
		if c == nil || n != len(strings.Fields(name)) || c.Name != name {
			t.Errorf("%s is not registered", name)
			continue
		}
		var out, errb bytes.Buffer
		called := false
		newEnv := func(context.Context, *Invocation) (Env, error) { called = true; return nil, fmt.Errorf("no env") }
		args := append(strings.Fields(name), "--help")
		if code := RunCLIStarted(context.Background(), args, &out, &errb, nil, newEnv); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-ios "+name) || called {
			t.Errorf("%s --help: exit %d, called %v, out %q", name, code, called, out.String())
		}
		out.Reset()
		errb.Reset()
		args = append(strings.Fields(name), "--no-such-flag")
		if code := RunCLIStarted(context.Background(), args, &out, &errb, nil, newEnv); code != ExitUsage || called || !strings.Contains(errb.String(), "unknown flag --no-such-flag") {
			t.Errorf("%s --no-such-flag: exit %d, called %v, stderr %q; want exit 2 before any connection", name, code, called, errb.String())
		}
	}
}

func jarNames(p *devtoolstest.Page) []string {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var out []string
	for _, c := range p.Cookies {
		out = append(out, c.Name+"@"+c.Domain)
	}
	sort.Strings(out)
	return out
}

// Design 4.2 "cookies clear [--domain d]": Page.deleteCookie per cookie,
// read back; --domain keeps cookies of other domains. Values never show.
func TestCookiesClear(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.Cookies = []webkit.Cookie{
		{Name: "a", Value: "valueA1", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "b", Value: "valueB2", Domain: ".app.test", Path: "/x", Secure: true, Session: true, SameSite: "None"},
		{Name: "c", Value: "valueC3", Domain: "cdn.other.test", Path: "/", Session: true, SameSite: "Lax"},
	}
	ps := &core.PageSession{Caller: p.Conn}
	code, out, errOut := spRun(ps, "", "cookies", "clear", "--domain", "other.test")
	if code != 0 || !reflect.DeepEqual(jarNames(p), []string{"a@app.test", "b@.app.test"}) || !strings.Contains(out, "Deleted 1 cookie") || !strings.Contains(out, "c (domain cdn.other.test, path /)") {
		t.Fatalf("clear --domain other.test: exit %d, out %q, stderr %q, jar %v", code, out, errOut, jarNames(p))
	}
	code, out, _ = spRun(ps, "", "--json", "cookies", "clear")
	if code != 0 || len(jarNames(p)) != 0 || !strings.Contains(out, `"deleted":2`) {
		t.Fatalf("clear: exit %d, out %q, jar %v", code, out, jarNames(p))
	}
	if strings.Contains(out, "value") {
		t.Fatalf("the output shows cookie values: %q", out)
	}
	if code, out, _ = spRun(ps, "", "cookies", "clear"); code != 0 || out != "No cookies to delete." {
		t.Fatalf("clear of an empty jar: exit %d, %q", code, out)
	}
}

// Design 4.2 "cookies set --curl <file>" (agent-browser: JSON, cURL or a
// Cookie header, auto-detected; --domain scopes) and the single-cookie
// form "cookies set <name> <value> [flags]" with read-back (W0).
func TestCookiesSet(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ps := &core.PageSession{Caller: p.Conn}
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("c.curl", "curl 'https://api.app.test/' -H 'Cookie: sid=topsecret1; theme=dark'")
	code, out, errOut := spRun(ps, dir, "cookies", "set", "--curl", "c.curl")
	if code != 0 || !reflect.DeepEqual(jarNames(p), []string{"sid@api.app.test", "theme@api.app.test"}) || !strings.Contains(out, "Imported 2 cookies") || !strings.Contains(out, "(curl)") {
		t.Fatalf("--curl: exit %d, out %q, stderr %q, jar %v", code, out, errOut, jarNames(p))
	}
	write("c.json", `[{"name":"j","value":"topsecret2","sameSite":"Strict","path":"/p"}]`)
	if code, out, errOut = spRun(ps, dir, "cookies", "set", "--curl", "c.json", "--domain", "app.test"); code != 0 || !strings.Contains(out, "(json)") {
		t.Fatalf("--curl json: exit %d, out %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = spRun(ps, dir, "cookies", "set", "one", "topsecret3", "--path", "/q", "--same-site", "strict", "--secure", "--http-only", "--expires", "2030-01-02T03:04:05Z")
	if code != 0 || !strings.Contains(out, "Cookie one set and verified") {
		t.Fatalf("single cookie: exit %d, out %q, stderr %q", code, out, errOut)
	}
	p.Mu.Lock()
	var one webkit.Cookie
	for _, c := range p.Cookies {
		if c.Name == "one" {
			one = c
		}
	}
	p.Mu.Unlock()
	want := webkit.Cookie{
		Name: "one", Value: "topsecret3", Domain: "app.test", Path: "/q", Expires: float64(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()),
		HTTPOnly: true, Secure: true, SameSite: "Strict",
	}
	if one != want {
		t.Fatalf("stored cookie %+v, want %+v (the page's host as the default domain)", one, want)
	}
	usage := [][]string{
		{"cookies", "set"},
		{"cookies", "set", "only-name"},
		{"cookies", "set", "--curl", "c.curl", "extra", "arg"},
		{"cookies", "set", "--curl", "c.curl", "--secure"},
		{"cookies", "set", "a", "b", "--same-site", "sometimes"},
		{"cookies", "set", "a", "b", "--expires", "next week"},
	}
	for _, args := range usage {
		if code, _, errOut := spRun(ps, dir, args...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want 2", args, code, errOut)
		}
	}
	write("bad.txt", "a=topsecret4; =topsecret5")
	if code, _, errOut := spRun(ps, dir, "cookies", "set", "--curl", "bad.txt"); code != ExitError || strings.Contains(errOut, "topsecret") || !strings.Contains(errOut, "pair 2") {
		t.Fatalf("bad file: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := spRun(ps, dir, "cookies", "set", "--curl", "missing.txt"); code != ExitError || !strings.Contains(errOut, "missing.txt") {
		t.Fatalf("missing file: exit %d, stderr %q", code, errOut)
	}
	for _, args := range [][]string{{"cookies", "set", "--curl", "c.curl"}, {"cookies", "set", "a", "topsecret6"}} {
		if _, out, errOut := spRun(ps, dir, args...); strings.Contains(out+errOut, "topsecret") {
			t.Fatalf("%v prints a cookie value: %q %q", args, out, errOut)
		}
	}
}

// Design 4.2 "storage local|session": read all, one key, set, clear.
// Values of password and token keys are masked unless --show-values
// (design 7.9, W4 brief security).
func TestStorageCommand(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ps := &core.PageSession{Caller: p.Conn}
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"storage", "local"}, "localStorage of https://app.test is empty."},
		{[]string{"storage", "local", "set", "theme", "dark"}, "Set localStorage theme on https://app.test."},
		{[]string{"storage", "local", "set", "authToken", "tok123"}, "Set localStorage authToken on https://app.test."},
		{[]string{"storage", "session", "set", "step", "2"}, "Set sessionStorage step on https://app.test."},
		{[]string{"storage", "local"}, "authToken=[masked]\ntheme=dark\n(values of password and token keys are masked; pass --show-values to see them)"},
		{[]string{"storage", "local", "--show-values"}, "authToken=tok123\ntheme=dark"},
		{[]string{"storage", "local", "theme"}, "dark"},
		{[]string{"storage", "local", "get", "theme"}, "dark"},
		{[]string{"storage", "local", "authToken"}, "[masked]\n(values of password and token keys are masked; pass --show-values to see them)"},
		{[]string{"storage", "session"}, "step=2"},
		{[]string{"storage", "local", "clear"}, "Cleared 2 localStorage items on https://app.test."},
	}
	for _, s := range steps {
		code, out, errOut := spRun(ps, "", s.args...)
		if code != 0 || out != s.want {
			t.Fatalf("%v: exit %d, out %q, stderr %q; want %q", s.args, code, out, errOut, s.want)
		}
	}
	if len(p.LocalStorage) != 0 || p.SessionStorage["step"] != "2" {
		t.Fatalf("storage after the steps: local %v, session %v", p.LocalStorage, p.SessionStorage)
	}
	code, out, _ := spRun(ps, "", "--json", "storage", "session", "step")
	var data map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &data) != nil || data["value"] != "2" || data["found"] != true || data["origin"] != "https://app.test" {
		t.Fatalf("--json get: exit %d, %q", code, out)
	}
	if code, _, errOut := spRun(ps, "", "storage", "local", "missing"); code != ExitError || !strings.Contains(errOut, `no key "missing"`) {
		t.Fatalf("missing key: exit %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"storage", "cookie"}, {"storage", "local", "set", "k"}, {"storage", "local", "clear", "x"}, {"storage", "local", "a", "b"}, {"storage"}} {
		if code, _, errOut := spRun(ps, "", args...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want 2", args, code, errOut)
		}
	}
}

// Design 4.2 "state save <path>", "state load <path>": cookies and storage
// of the current origin, JSON, mode 0600; load restores what save wrote.
func TestStateSaveLoadCommands(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ps := &core.PageSession{Caller: p.Conn}
	dir := t.TempDir()
	p.Cookies = []webkit.Cookie{
		{Name: "sid", Value: "topsecret7", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "ad", Value: "x", Domain: "ads.test", Path: "/", Session: true, SameSite: "Lax"},
	}
	p.LocalStorage["k"] = "v"
	p.SessionStorage["s"] = "w"
	// A wider mode of an existing file is narrowed.
	if err := os.WriteFile(filepath.Join(dir, "st.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := spRun(ps, dir, "state", "save", "st.json")
	if code != 0 || out != "Saved the state of https://app.test to "+filepath.Join(dir, "st.json")+": 1 cookie, 1 localStorage item and 1 sessionStorage item (mode 0600; the file holds live credentials, delete it after use)." {
		t.Fatalf("state save: exit %d, out %q, stderr %q", code, out, errOut)
	}
	fi, err := os.Stat(filepath.Join(dir, "st.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, %v; want 0600", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "st.json"))
	if !strings.Contains(string(raw), "topsecret7") || strings.Contains(string(raw), "ads.test") {
		t.Fatalf("state file %s: want the origin's cookie with its value and no other site's", raw)
	}
	p.Mu.Lock()
	p.Cookies = []webkit.Cookie{{Name: "new", Value: "n", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"}}
	p.LocalStorage = map[string]string{"other": "1"}
	p.Mu.Unlock()
	code, out, errOut = spRun(ps, dir, "state", "load", "st.json")
	if code != 0 || out != "Loaded the state of https://app.test from "+filepath.Join(dir, "st.json")+": 1 cookie set (1 removed), 1 localStorage item and 1 sessionStorage item." {
		t.Fatalf("state load: exit %d, out %q, stderr %q", code, out, errOut)
	}
	if !reflect.DeepEqual(jarNames(p), []string{"sid@app.test"}) || !reflect.DeepEqual(p.LocalStorage, map[string]string{"k": "v"}) {
		t.Fatalf("after load: jar %v, local %v", jarNames(p), p.LocalStorage)
	}
	other := devtoolstest.New(t, "https://elsewhere.test")
	if code, _, errOut := spRun(&core.PageSession{Caller: other.Conn}, dir, "state", "load", "st.json"); code != ExitError || !strings.Contains(errOut, "open a page of https://app.test first") {
		t.Fatalf("load on another origin: exit %d, %q", code, errOut)
	}
	if code, _, errOut := spRun(ps, dir, "state", "load", "nofile.json"); code != ExitError || !strings.Contains(errOut, "nofile.json") {
		t.Fatalf("load of a missing file: exit %d, %q", code, errOut)
	}
}

// spRig is one fake page behind a fake iwdp listing with a daemon manager,
// for the commands that keep DevTools state (trace, profiler).
type spRig struct {
	f   *fakewebkit.Server
	m   *core.Manager
	req core.PageRequest
	ao  core.AcquireOptions
}

func newSPRig(t *testing.T) *spRig {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000002", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(dev.ID, "agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := core.NewManager(core.ManagerOptions{
		Reservations: res, Session: webkit.SessionOptions{TargetWait: time.Second},
		Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
			return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return &spRig{f: f, m: m, req: core.PageRequest{Transport: "iwdp", Addr: l.Addr()}, ao: core.AcquireOptions{Session: "s1", Reservation: "agent"}}
}

// run runs a command line as one operation of session s1, as the daemon
// does.
func (r *spRig) run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := -1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.m.Do(ctx, r.req, r.ao, "test", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
		code = RunCLIStarted(ctx, args, &out, &errb, nil, func(context.Context, *Invocation) (Env, error) {
			return spEnv{ps: mo.Page.PS, mo: mo, dir: dir}, nil
		})

		return nil
	})
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

// traceSlice is a complete event as a trace viewer shows it: its thread,
// nesting depth, start and duration in microseconds.
type traceSlice struct {
	Thread, Name string
	Depth        int
	Ts, Dur      float64
}

// parseChromeTrace loads a trace file the way a trace viewer does, after
// the Trace Event Format (Google, "JSON Object Format"): a traceEvents
// array; every event has name, ph, pid and tid; ph M names threads, X is
// a complete event with ts and dur >= 0, I (or i) an instant event with
// scope g, p or t. Complete events of one thread must nest; the parser
// returns them with their depth, and the instant events, ordered by time.
func parseChromeTrace(data []byte) (slices, instants []traceSlice, err error) {
	var file struct {
		TraceEvents []struct {
			Name string          `json:"name"`
			Ph   string          `json:"ph"`
			Ts   *float64        `json:"ts"`
			Dur  *float64        `json:"dur"`
			Pid  *int            `json:"pid"`
			Tid  *int            `json:"tid"`
			S    string          `json:"s"`
			Args json.RawMessage `json:"args"`
		} `json:"traceEvents"`
		DisplayTimeUnit string `json:"displayTimeUnit"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, err
	}
	if file.TraceEvents == nil || (file.DisplayTimeUnit != "" && file.DisplayTimeUnit != "ms" && file.DisplayTimeUnit != "ns") {
		return nil, nil, fmt.Errorf("no traceEvents array or a bad displayTimeUnit %q", file.DisplayTimeUnit)
	}
	threads := map[int]string{}
	type ev struct {
		tid     int
		name    string
		ts, dur float64
	}
	var xs []ev
	for i, e := range file.TraceEvents {
		if e.Name == "" || e.Ph == "" || e.Pid == nil || e.Tid == nil {
			return nil, nil, fmt.Errorf("event %d lacks name, ph, pid or tid", i)
		}
		switch e.Ph {
		case "M":
			var a struct{ Name string }
			if e.Name == "thread_name" {
				if err := json.Unmarshal(e.Args, &a); err != nil || a.Name == "" {
					return nil, nil, fmt.Errorf("thread_name event %d has no args.name", i)
				}
				threads[*e.Tid] = a.Name
			}
		case "X":
			if e.Ts == nil || e.Dur == nil || *e.Dur < 0 {
				return nil, nil, fmt.Errorf("complete event %d (%s) lacks ts or dur", i, e.Name)
			}
			xs = append(xs, ev{*e.Tid, e.Name, *e.Ts, *e.Dur})
		case "I", "i":
			if e.Ts == nil || (e.S != "" && e.S != "g" && e.S != "p" && e.S != "t") {
				return nil, nil, fmt.Errorf("instant event %d (%s) lacks ts or has scope %q", i, e.Name, e.S)
			}
			instants = append(instants, traceSlice{Thread: fmt.Sprint(*e.Tid), Name: e.Name, Ts: *e.Ts})
		default:
			return nil, nil, fmt.Errorf("event %d has phase %q", i, e.Ph)
		}
	}
	sort.SliceStable(xs, func(i, j int) bool {
		if xs[i].tid != xs[j].tid {
			return xs[i].tid < xs[j].tid
		}
		if xs[i].ts != xs[j].ts {
			return xs[i].ts < xs[j].ts
		}
		return xs[i].dur > xs[j].dur
	})
	stacks := map[int][]ev{}
	for _, x := range xs {
		st := stacks[x.tid]
		for len(st) > 0 && x.ts >= st[len(st)-1].ts+st[len(st)-1].dur {
			st = st[:len(st)-1]
		}
		if len(st) > 0 && x.ts+x.dur > st[len(st)-1].ts+st[len(st)-1].dur {
			return nil, nil, fmt.Errorf("%s overlaps %s partially", x.name, st[len(st)-1].name)
		}
		slices = append(slices, traceSlice{Thread: threads[x.tid], Name: x.name, Depth: len(st), Ts: x.ts, Dur: x.dur})
		stacks[x.tid] = append(st, x)
	}
	for i := range instants {
		var tid int
		if _, err := fmt.Sscan(instants[i].Thread, &tid); err != nil {
			return nil, nil, fmt.Errorf("instant %d: thread %q: %w", i, instants[i].Thread, err)
		}
		instants[i].Thread = threads[tid]
	}
	return slices, instants, nil
}

// Design 4.2 "trace start [--categories]", "trace stop [path]": Timeline
// (and ScriptProfiler) events in Chrome trace JSON, written 0600; the file
// loads in a trace parser (7.7 test 5).
func TestTraceCommands(t *testing.T) {
	r := newSPRig(t)
	r.f.Handle("Timeline.stop", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("Timeline.recordingStopped", map[string]any{"endTime": 11.0})
		return nil, nil
	})
	dir := t.TempDir()
	if code, _, errOut := r.run(t, dir, "trace", "start", "--categories", "timeline,gpu"); code != ExitUsage || !strings.Contains(errOut, "gpu") {
		t.Fatalf("bad category: exit %d, %q", code, errOut)
	}
	if code, _, errOut := r.run(t, dir, "trace", "stop"); code != ExitError || !strings.Contains(errOut, "trace start") {
		t.Fatalf("stop without start: exit %d, %q", code, errOut)
	}
	if code, out, errOut := r.run(t, dir, "trace", "start"); code != 0 || !strings.Contains(out, "Trace recording started") {
		t.Fatalf("trace start: exit %d, %q, %q", code, out, errOut)
	}
	if got := len(r.f.ReceivedMethod("ScriptProfiler.startTracking")); got != 1 {
		t.Fatalf("ScriptProfiler.startTracking sent %d times; the default categories include script", got)
	}
	r.f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
	r.f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{
		"type": "EventDispatch", "data": map[string]any{"type": "click"},
		"startTime": 10.5, "endTime": 10.75, "children": []any{map[string]any{"type": "FunctionCall", "data": map[string]any{}, "startTime": 10.5, "endTime": 10.7, "children": []any{}}},
	}})
	r.f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "TimerInstall", "data": map[string]any{"timerId": 1}, "startTime": 10.6}})
	r.f.Emit("ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"startTime": 10.5, "endTime": 10.6, "type": "API"}})
	code, out, errOut := r.run(t, dir, "trace", "stop", "t.json")
	if code != 0 || out != "Trace written to "+filepath.Join(dir, "t.json")+": 2 Timeline records, 4 trace events." {
		t.Fatalf("trace stop: exit %d, %q, %q", code, out, errOut)
	}
	fi, err := os.Stat(filepath.Join(dir, "t.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("trace file mode %v, %v", fi, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "t.json"))
	slices, instants, err := parseChromeTrace(data)
	if err != nil {
		t.Fatalf("the trace does not load: %v\n%s", err, data)
	}
	// Expected from the emitted records: (time - 10 s) in microseconds;
	// FunctionCall nests in EventDispatch.
	wantSlices := []traceSlice{
		{"Main thread (Timeline)", "EventDispatch click", 0, 500000, 250000},
		{"Main thread (Timeline)", "FunctionCall", 1, 500000, 200000},
		{"JavaScript (ScriptProfiler)", "Script API", 0, 500000, 100000},
	}
	wantInstants := []traceSlice{{"Main thread (Timeline)", "TimerInstall", 0, 600000, 0}}
	if !reflect.DeepEqual(slices, wantSlices) || !reflect.DeepEqual(instants, wantInstants) {
		t.Fatalf("trace\n slices %v\n instants %v\nwant %v\n %v", slices, instants, wantSlices, wantInstants)
	}
	// Without a path the trace goes to a new file in the temp directory;
	// "--categories timeline" leaves the ScriptProfiler off.
	if code, _, errOut := r.run(t, dir, "trace", "start", "--categories", "timeline"); code != 0 {
		t.Fatalf("trace start --categories timeline: %d %q", code, errOut)
	}
	if got := len(r.f.ReceivedMethod("ScriptProfiler.startTracking")); got != 1 {
		t.Fatalf("ScriptProfiler.startTracking sent again for --categories timeline")
	}
	code, out, _ = r.run(t, dir, "--json", "trace", "stop")
	var res struct {
		Path   string `json:"path"`
		Events int    `json:"events"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || filepath.Dir(res.Path) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(res.Path), "agent-ios-trace-") {
		t.Fatalf("trace stop without a path: exit %d, %q", code, out)
	}
	t.Cleanup(func() { _ = os.Remove(res.Path) })
	if fi, err := os.Stat(res.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("default trace file %s: %v %v", res.Path, fi, err)
	}
}

// Design 4.2 "profiler start", "profiler stop [path]": ScriptProfiler
// samples as a .cpuprofile (CDP Profiler.Profile), written 0600.
func TestProfilerCommands(t *testing.T) {
	r := newSPRig(t)
	frame := func(name string, line, col int) map[string]any {
		return map[string]any{"sourceID": "7", "name": name, "line": line, "column": col, "url": "https://fixture.example/app.js"}
	}
	r.f.Handle("ScriptProfiler.startTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("ScriptProfiler.trackingStart", map[string]any{"timestamp": 5.0})
		return nil, nil
	})
	r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("ScriptProfiler.trackingComplete", map[string]any{"timestamp": 5.003, "samples": map[string]any{"stackTraces": []any{
			map[string]any{"timestamp": 5.001, "stackFrames": []any{frame("inner", 3, 5), frame("outer", 1, 1)}},
			map[string]any{"timestamp": 5.002, "stackFrames": []any{frame("outer", 1, 1)}},
		}}})
		return nil, nil
	})
	dir := t.TempDir()
	if code, out, errOut := r.run(t, dir, "profiler", "start"); code != 0 || !strings.Contains(out, "CPU profiling started") {
		t.Fatalf("profiler start: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := r.run(t, dir, "profiler", "start"); code != ExitError || !strings.Contains(errOut, "already profiling") {
		t.Fatalf("second start: %d %q", code, errOut)
	}
	code, out, errOut := r.run(t, dir, "profiler", "stop", "p.cpuprofile")
	if code != 0 || out != "CPU profile written to "+filepath.Join(dir, "p.cpuprofile")+": 2 samples, 3 nodes." {
		t.Fatalf("profiler stop: %d %q %q", code, out, errOut)
	}
	fi, err := os.Stat(filepath.Join(dir, "p.cpuprofile"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode %v %v", fi, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "p.cpuprofile"))
	var prof struct {
		Nodes []struct {
			ID        int `json:"id"`
			CallFrame struct {
				FunctionName string `json:"functionName"`
				LineNumber   int    `json:"lineNumber"`
				ColumnNumber int    `json:"columnNumber"`
			} `json:"callFrame"`
			HitCount int   `json:"hitCount"`
			Children []int `json:"children"`
		} `json:"nodes"`
		StartTime  int64   `json:"startTime"`
		EndTime    int64   `json:"endTime"`
		Samples    []int   `json:"samples"`
		TimeDeltas []int64 `json:"timeDeltas"`
	}
	if err := json.Unmarshal(data, &prof); err != nil {
		t.Fatal(err)
	}
	// Expected (CDP Profiler.Profile; WebKit frames innermost first, 1-based
	// lines): root -> outer -> inner; one hit each; times in microseconds.
	got := fmt.Sprint(prof.Nodes[0].Children, prof.Nodes[1].CallFrame, prof.Nodes[1].HitCount, prof.Nodes[1].Children,
		prof.Nodes[2].CallFrame, prof.Nodes[2].HitCount, prof.Samples, prof.TimeDeltas, prof.StartTime, prof.EndTime)
	want := "[2] {outer 0 0} 1 [3] {inner 2 4} 1 [3 2] [1000 1000] 5000000 5003000"
	if len(prof.Nodes) != 3 || got != want {
		t.Fatalf("profile %s\n got %s\nwant %s", data, got, want)
	}
	if code, _, errOut := r.run(t, dir, "profiler", "stop"); code != ExitError || !strings.Contains(errOut, "profiler start") {
		t.Fatalf("stop without start: %d %q", code, errOut)
	}
}

// Design 4.2 "vitals [--json]".
func TestVitalsCommand(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	reply := map[string]any{
		"ttfb": map[string]any{"value": 20}, "fcp": map[string]any{"value": 99.6},
		"lcp": map[string]any{"unavailable": "this engine has no largest-contentful-paint entries"}, "cls": map[string]any{"unavailable": "this engine has no layout-shift entries"},
		"inp": map[string]any{"unavailable": "this engine has no event timing entries"}, "domContentLoaded": map[string]any{"value": 120}, "load": map[string]any{"value": 130},
	}
	f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p"}}, nil
	})
	f.Handle("Runtime.awaitPromise", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		data, _ := json.Marshal(reply)
		return map[string]any{"result": map[string]any{"type": "string", "value": string(data)}}, nil
	})
	ps := &core.PageSession{Caller: spDial(t, f)}
	code, out, errOut := spRun(ps, "", "vitals")
	want := "LCP: unavailable (this engine has no largest-contentful-paint entries)\nCLS: unavailable (this engine has no layout-shift entries)\nTTFB: 20 ms\nFCP: 100 ms\nINP: unavailable (this engine has no event timing entries)\nDOMContentLoaded: 120 ms\nLoad: 130 ms"
	if code != 0 || out != want {
		t.Fatalf("vitals: exit %d, %q, %q", code, out, errOut)
	}
	code, out, _ = spRun(ps, "", "--json", "vitals")
	if code != 0 || !strings.Contains(out, `"ttfb":{"value":20}`) || !strings.Contains(out, `"lcp":{"unavailable":`) {
		t.Fatalf("vitals --json: exit %d, %q", code, out)
	}
}

func spDial(t *testing.T, f *fakewebkit.Server) *webkit.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Design 4.2 "diff screenshot --baseline <png> [--threshold n]": a host
// pixel diff of a viewport screenshot against the baseline. The page
// shows the hand-made fixture two_px.png (2 of 12 pixels differ from
// base.png, see internal/devtools/imagediff_test.go).
func TestDiffScreenshotCommand(t *testing.T) {
	fixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("..", "devtools", "testdata", "diff", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": `{"width":4,"height":3}`}}, nil
	})
	f.Handle("Page.snapshotRect", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixture("two_px.png"))}, nil
	})
	ps := &core.PageSession{Caller: spDial(t, f)}
	dir := t.TempDir()
	for _, n := range []string{"base.png", "two_px.png", "small.png"} {
		if err := os.WriteFile(filepath.Join(dir, n), fixture(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := spRun(ps, dir, "diff", "screenshot", "--baseline", "base.png")
	if code != 0 || out != "2 of 12 pixels differ (16.67%) at threshold 0.1." {
		t.Fatalf("diff against base: exit %d, %q, %q", code, out, errOut)
	}
	if code, out, _ = spRun(ps, dir, "diff", "screenshot", "--baseline", "two_px.png", "--threshold", "0"); code != 0 || out != "No pixel differs (threshold 0)." {
		t.Fatalf("diff against itself: exit %d, %q", code, out)
	}
	code, out, _ = spRun(ps, dir, "--json", "diff", "screenshot", "--baseline", "base.png")
	var res map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res["different"] != 2.0 || res["total"] != 12.0 || res["match"] != false {
		t.Fatalf("--json: exit %d, %q", code, out)
	}
	if code, _, errOut := spRun(ps, dir, "diff", "screenshot", "--baseline", "small.png"); code != ExitError || !strings.Contains(errOut, "3x3") {
		t.Fatalf("size mismatch: exit %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"diff", "screenshot"}, {"diff", "screenshot", "--baseline", "base.png", "--threshold", "1"}, {"diff", "screenshot", "--baseline", "base.png", "--threshold", "x"}} {
		if code, _, errOut := spRun(ps, dir, args...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want 2", args, code, errOut)
		}
	}
	if code, _, errOut := spRun(ps, dir, "diff", "screenshot", "--baseline", "none.png"); code != ExitError || !strings.Contains(errOut, "none.png") {
		t.Fatalf("missing baseline: exit %d, %q", code, errOut)
	}
}
