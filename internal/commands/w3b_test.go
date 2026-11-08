package commands

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/refs"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Tests of the W3b command files on the fake WebKit endpoint (the page
// side is scripted; the runtime JavaScript runs in the WebKitGTK suite).

// newActionHarness is newHarness with a scheduler and a lease, as the
// daemon gives an action command (design 2.6 attempt loop).
func newActionHarness(t *testing.T) *rtHarness {
	t.Helper()
	return newActionHarnessWith(t, transport.Limits{Routed: true})
}

// newActionHarnessWith is newActionHarness on a connection with the
// limits lim.
func newActionHarnessWith(t *testing.T, lim transport.Limits) *rtHarness {
	t.Helper()
	h := newHarnessWith(t, lim)
	s := core.NewScheduler(h.sess, core.SchedulerOptions{Runtime: h.ps.Runtime.RuntimeCaller()})
	t.Cleanup(s.Close)
	s.SetLease("agent", 1)
	h.ps.Sched = s
	h.page.set("clock", okEnv(map[string]any{"now": 1000, "doc": "doc-1"}))
	h.page.set("dialogs", okEnv(map[string]any{"doc": "doc-1", "used": []any{}, "pending": []any{}, "withdrawn": []any{}, "on": true}))
	return h
}

func attempt(status, reason string, value any) map[string]any {
	r := map[string]any{"status": status, "reason": reason, "now": 1001, "doc": "doc-1"}
	if value != nil {
		r["value"] = value
	}
	return okEnv(r)
}

// Design 3.5: a dispatched action reports trusted false and names each
// emulated default action; the first action of a session prints the
// synthetic-event note once.
func TestActionReportsSyntheticAndEmulatedDefaults(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"trusted": false, "d": []string{"implicit submission: clicked <button id=\"go\">"}}))
	out := h.mustRun(t, "press", "Enter")
	if !strings.Contains(out, "emulated default action: implicit submission: clicked <button id=\"go\">") || !strings.Contains(out, "synthetic event") {
		t.Fatalf("press output = %q", out)
	}
	a := h.page.last("act")
	args := a["args"].(map[string]any)
	keys := args["keys"].([]any)
	if args["k"] != "press" || keys[0].(map[string]any)["key"] != "Enter" || keys[0].(map[string]any)["kc"] != float64(13) {
		t.Fatalf("attempt args = %v", args)
	}
	if out := h.mustRun(t, "press", "Tab"); strings.Contains(out, "synthetic event") {
		t.Fatalf("second action repeated the synthetic note: %q", out)
	}
}

// Design 3.5 and 7.6 item 3: a covered element fails before dispatch, and
// the error names the covering element.
func TestCoveredClickFailsNamingTheCover(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("retry", "covered by <div class=\"modal\">", nil))
	code, _, errOut := h.run("--timeout", "700ms", "click", "#buy")
	if code == 0 || !strings.Contains(errOut, "covered by <div class=\"modal\">") || strings.Contains(errOut, "may have run") {
		t.Fatalf("exit %d, stderr %q; want a failure before dispatch that names the cover", code, errOut)
	}
}

// A target the page refuses before the fence (ambiguous) is the runtime
// error itself, not an ambiguous step.
func TestRefusedTargetIsTheRuntimeError(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("failed", "ambiguous: \"button\" matches 3 elements", nil))
	code, _, errOut := h.run("click", "button")
	if code != 1 || !strings.Contains(errOut, "matches 3 elements") || strings.Contains(errOut, "page answered") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

// type --delay: one character per step, in order (design 2.6 steps).
func TestTypeWithDelaySendsOneCharacterPerStep(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"v": "abc"}))
	h.mustRun(t, "type", "#q", "abc", "--delay", "10ms")
	var steps []string
	for _, c := range h.page.calls {
		if c.Fn == "act" {
			k := c.Args["args"].(map[string]any)["keys"].([]any)
			if len(k) != 1 {
				t.Fatalf("step %v carries %d keys, want 1", c.Args["step"], len(k))
			}
			steps = append(steps, k[0].(map[string]any)["key"].(string))
		}
	}
	if !reflect.DeepEqual(steps, []string{"a", "b", "c"}) {
		t.Fatalf("steps = %v", steps)
	}
}

// Design 3.6: an expired wait exits 3 with the last observed state.
func TestWaitTimeoutExitsThreeWithLastState(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("wait", okEnv(map[string]any{"ok": false, "state": "URL is still http://a/b"}))
	start := time.Now()
	code, _, errOut := h.run("--timeout", "800ms", "wait", "--url", "**/done")
	if code != ExitWaitTimeout || !strings.Contains(errOut, "URL is still http://a/b") || time.Since(start) > 2*time.Second {
		t.Fatalf("exit %d after %s, stderr %q; want 3 with the last state", code, time.Since(start), errOut)
	}
	if a := h.page.last("wait"); a["re"] != `^[\s\S]*/done$` || a["k"] != "url" {
		t.Fatalf("wait slice args = %v", a)
	}
}

func TestWaitSucceeds(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("wait", okEnv(map[string]any{"ok": true, "state": "the element is visible"}))
	if out := h.mustRun(t, "wait", "#ready"); !strings.Contains(out, "done: #ready --state visible") {
		t.Fatalf("wait output %q", out)
	}
}

// Glob property (design 3.6): a glob without "*" matches exactly itself;
// "*" never crosses "/", "**" does. Checked with Go's regexp, whose
// syntax for these constructs equals JavaScript's.
func TestGlobRegexpProperties(t *testing.T) {
	clean := func(s string) string { return strings.ReplaceAll(s, "*", "") }
	literal := func(a, b string) bool {
		a, b = clean(a), clean(b)
		re := regexp.MustCompile(globRegexp(a))
		return re.MatchString(a) && (a == b || !re.MatchString(b))
	}
	star := func(p, mid, s string) bool {
		p, mid, s = clean(p), clean(mid), clean(s)
		one := regexp.MustCompile(globRegexp(p + "*" + s)).MatchString(p + mid + s)
		two := regexp.MustCompile(globRegexp(p + "**" + s)).MatchString(p + mid + s)
		return two && one == !strings.Contains(mid, "/")
	}
	// Hand-written cases (quick rarely generates "/" in the middle part).
	for glob, cases := range map[string]map[string]bool{
		"https://a.test/*/c":  {"https://a.test/b/c": true, "https://a.test/b/x/c": false, "https://a.test//c": true},
		"**/done":             {"http://a/b/done": true, "http://a/b/done?x": false},
		"http://a.test/p?q=1": {"http://a.test/p?q=1": true, "http://a.test/pxq=1": false},
	} {
		for url, want := range cases {
			if got := regexp.MustCompile(globRegexp(glob)).MatchString(url); got != want {
				t.Fatalf("glob %q on %q = %v, want %v", glob, url, got, want)
			}
		}
	}
	if err := quick.Check(literal, nil); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(star, nil); err != nil {
		t.Fatal(err)
	}
}

// W3b-R2 finding 5: "**" matches any text, line breaks included. The
// counterexample of the round-1 gate run reduces to glob "**" (or "**X")
// with an input that holds a line feed; ".*" does not match one in Go or
// in JavaScript without the s flag.
func TestGlobDoubleStarMatchesLineBreaks(t *testing.T) {
	for glob, in := range map[string]string{
		"**":             "\n",
		"**X":            "\nX",
		"https://a/**/c": "https://a/b\r\nd\u2028e/c",
		"x**":            "x\n\n",
	} {
		if !regexp.MustCompile(globRegexp(glob)).MatchString(in) {
			t.Fatalf("glob %q does not match %q (regexp %s)", glob, in, globRegexp(glob))
		}
	}
	if regexp.MustCompile(globRegexp("*")).MatchString("a/\n") {
		t.Fatal(`glob "*" crosses "/"`)
	}
}

// Key parsing property: every printable ASCII character is its own key
// and text, with or without modifiers; named keys keep their codes.
func TestParseKeyProperties(t *testing.T) {
	f := func(b byte, mods uint8) bool {
		r := rune(' ' + b%95)
		name := string(r)
		prefix := ""
		for i, m := range []string{"Shift", "Control", "Alt", "Meta"} {
			if mods&(1<<i) != 0 {
				prefix += m + "+"
			}
		}
		k, m, err := parseKey(prefix + name)
		want := modifiers{S: mods&1 != 0, C: mods&2 != 0, A: mods&4 != 0, M: mods&8 != 0}
		return err == nil && k.Key == name && k.Text == name && k.CC == int(r) && m == want
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]int{"Enter": 13, "Tab": 9, "esc": 27, "ArrowLeft": 37, "a": 65, "7": 55, "Space": 32} {
		if k, _, err := parseKey(in); err != nil || k.KC != want {
			t.Fatalf("parseKey(%q) = %+v, %v; want keyCode %d", in, k, err, want)
		}
	}
	if _, _, err := parseKey("Hyper+a"); err == nil {
		t.Fatal("unknown modifier accepted")
	}
}

// Dialog bridge invariants (design 3.7) over random operation sequences:
// an answer is issued again only after an acknowledged withdrawal
// returned it to the queue; acknowledged and expired answers never
// change; a new epoch issues none of the old epoch's answers.
func TestDialogBridgeAtMostOnceIssuance(t *testing.T) {
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		var b dialogBridge
		b.newEpoch(1)
		requeued := map[string]bool{}
		issuedOnce := map[string]bool{}
		final := map[string]answerState{}
		old := map[string]bool{}
		for i := 0; i < 60; i++ {
			switch rng.Intn(6) {
			case 0, 1:
				b.arm(rng.Intn(2) == 0, nil)
			case 2:
				for _, a := range b.toIssue() {
					if old[a.ID] || issuedOnce[a.ID] && !requeued[a.ID] {
						return false
					}
					issuedOnce[a.ID], requeued[a.ID] = true, false
				}
			case 3: // a reply: some issued answers used, some pending, some withdrawn
				var r dialogsReply
				for _, a := range b.answers {
					if a.state != answerIssued {
						continue
					}
					switch rng.Intn(4) {
					case 0:
						r.Used = append(r.Used, dialogUse{ID: a.ID, Type: "confirm"})
					case 1:
						r.Pending = append(r.Pending, a.ID)
					case 2:
						r.Withdrawn = append(r.Withdrawn, a.ID)
						requeued[a.ID] = true
					}
				}
				b.apply(r)
			case 4:
				b.withdrawLost()
			case 5:
				for _, a := range b.answers {
					old[a.ID] = true
				}
				b.newEpoch(uint64(i + 2))
			}
			for _, a := range b.answers {
				if s, ok := final[a.ID]; ok && s != a.state {
					return false
				}
				if a.state == answerAcknowledged || a.state == answerExpired {
					final[a.ID] = a.state
				}
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Design 3.7: arming an answer issues it into the current document in the
// same command; a later reply without it and without its use expires it,
// and the next result says so.
func TestDialogAcceptIssuesAndExpiresWithItsDocument(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("dialogs", okEnv(map[string]any{"doc": "doc-1", "used": []any{}, "pending": []any{"d1"}, "withdrawn": []any{}, "on": true}))
	out := h.mustRun(t, "dialog", "accept", "x")
	if !strings.Contains(out, "armed dialog answer d1: accept \"x\"") {
		t.Fatalf("dialog accept output %q", out)
	}
	issue, _ := json.Marshal(h.page.last("dialogs")["issue"])
	if string(issue) != `[{"accept":true,"id":"d1","text":"x"}]` {
		t.Fatalf("issued %s", issue)
	}
	h.page.set("dialogs", okEnv(map[string]any{"doc": "doc-2", "used": []any{}, "pending": []any{}, "withdrawn": []any{}, "on": true}))
	out = h.mustRun(t, "dialog", "status")
	if !strings.Contains(out, "dialog answer d1 expired with its document") {
		t.Fatalf("status after the document changed: %q", out)
	}
	if n := countFn(h.page, "dialogs"); n != 2 {
		t.Fatalf("dialogs calls = %d, want 2 (issue, status); the answer must not be written again", n)
	}
}

func countFn(p *fakePage, fn string) int {
	n := 0
	for _, f := range p.fns() {
		if f == fn {
			n++
		}
	}
	return n
}

func statesOf(t *refs.Table) int {
	statesMu.Lock()
	defer statesMu.Unlock()
	n := 0
	for k, byS := range states {
		if k.Value() == t {
			n += len(byS)
		}
	}
	return n
}

func stateRecords() int {
	statesMu.Lock()
	defer statesMu.Unlock()
	return len(states)
}

// W3b decision 6 (leak): the per-session W3b state goes away with its page
// record. A record's ref table is the key; once the record and its table
// are unreachable, the states of every session on it are dropped. While
// the table lives, a session keeps its state across leases.
func TestW3bStateIsReleasedWithThePageRecord(t *testing.T) {
	before := stateRecords()
	func() {
		ps := &core.PageSession{Refs: refs.NewTable()}
		a := stateFor(ps, "a", 1)
		stateFor(ps, "b", 1)
		if again := stateFor(ps, "a", 2); again != a {
			t.Fatal("a new lease epoch replaced the session's state while the page record lives")
		}
		if n := statesOf(ps.Refs); n != 2 {
			t.Fatalf("page record has %d session states, want 2", n)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for stateRecords() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d page records keep W3b state after their ref tables were dropped (before: %d)", stateRecords(), before)
		}
		goruntime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// W3b decision 2 (design 4.1): the existing MCP tools keep their
// parameters. click takes the legacy "selector" or the new "target" (which
// takes @eN); fill also "value"; type_text without a target types into the
// focused element; highlight_node takes node_id. Giving a parameter and
// its legacy name together is a clear error.
func TestMCPToolsKeepLegacyParameters(t *testing.T) {
	cases := []struct {
		tool, args string
		wantArgs   []string
		wantFlags  map[string]any
	}{
		{"click", `{"selector":"#buy"}`, []string{"#buy"}, nil},
		{"click", `{"target":"@e3"}`, []string{"@e3"}, nil},
		{"fill", `{"selector":"#q","value":"hi"}`, []string{"#q", "hi"}, nil},
		{"fill", `{"target":"@e2","text":"hi"}`, []string{"@e2", "hi"}, nil},
		{"type_text", `{"text":"abc"}`, []string{"abc"}, nil},
		{"type_text", `{"selector":"#q","text":"abc"}`, []string{"#q", "abc"}, nil},
		{"highlight_node", `{"node_id":7}`, nil, map[string]any{"node-id": 7}},
		{"highlight_node", `{"selector":"#a"}`, []string{"#a"}, nil},
	}
	for _, c := range cases {
		cmd := byMCPName(t, c.tool)
		in, err := mcpInput(cmd, json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s %s: %v", c.tool, c.args, err)
		}
		if !slices.Equal(in.Args, c.wantArgs) {
			t.Fatalf("%s %s: args %q, want %q", c.tool, c.args, in.Args, c.wantArgs)
		}
		for k, v := range c.wantFlags {
			if in.Flags[k] != v {
				t.Fatalf("%s %s: flag %s = %v, want %v", c.tool, c.args, k, in.Flags[k], v)
			}
		}
		props := InputSchema(cmd)["properties"].(map[string]any)
		var raw map[string]any
		_ = json.Unmarshal([]byte(c.args), &raw)
		for k := range raw {
			if props[k] == nil {
				t.Fatalf("%s: schema lacks the parameter %q", c.tool, k)
			}
		}
	}
	for _, c := range []struct{ tool, args string }{
		{"click", `{"selector":"#a","target":"@e1"}`},
		{"fill", `{"target":"#a","value":"x","text":"y"}`},
		{"highlight_node", `{"selector":"#a","target":"#b"}`},
	} {
		if _, err := mcpInput(byMCPName(t, c.tool), json.RawMessage(c.args)); err == nil || !strings.Contains(err.Error(), "not both") {
			t.Fatalf("%s %s: err = %v, want a clear error for both names", c.tool, c.args, err)
		}
	}
	if _, err := mcpInput(byMCPName(t, "click"), json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("click without a target: err = %v", err)
	}
	if req, _ := InputSchema(byMCPName(t, "click"))["required"].([]any); len(req) != 0 {
		t.Fatalf("click requires %v; a legacy call gives only selector", req)
	}
}

func byMCPName(t *testing.T, name string) *Command {
	t.Helper()
	for _, c := range All() {
		if c.MCPName == name {
			return c
		}
	}
	t.Fatalf("no MCP tool %s", name)
	return nil
}

// The legacy forms reach the page: click by selector, type into the
// focused element (no target), highlight by node id.
func TestLegacyFormsRunTheAction(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"v": "abc"}))
	h.mustRun(t, "type", "abc")
	if a := h.page.last("act")["args"].(map[string]any); a["t"] != nil || a["k"] != "type" {
		t.Fatalf("type <text> sent %v, want no target (the focused element)", a)
	}
	h.mustRun(t, "click", "--", "#buy")
	if a := h.page.last("act")["args"].(map[string]any); a["t"] == nil || a["k"] != "click" {
		t.Fatalf("click sent %v", a)
	}
	h.f.Result("DOM.highlightNode", map[string]any{})
	if out := h.mustRun(t, "highlight", "--node-id", "7"); !strings.Contains(out, "node 7") {
		t.Fatalf("highlight --node-id: %q", out)
	}
	if got := h.f.ReceivedMethod("DOM.highlightNode"); len(got) != 1 || !strings.Contains(string(got[0].Params), `"nodeId":7`) {
		t.Fatalf("DOM.highlightNode params = %v", got)
	}
	if code, _, errOut := h.run("highlight", "#a", "--node-id", "7"); code != ExitUsage || !strings.Contains(errOut, "exactly one") {
		t.Fatalf("highlight with a target and a node id: exit %d %q", code, errOut)
	}
}

// Design 4.2: tap <x> <y> taps the element at a viewport point (runtime
// kind tapAt, no target); a non-numeric pair is a usage error.
func TestTapAtPoint(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"v": map[string]any{"at": "<button id=\"go\">"}}))
	out := h.mustRun(t, "tap", "120", "40.5")
	a := h.page.last("act")["args"].(map[string]any)
	if a["k"] != "tapAt" || a["x"] != float64(120) || a["y"] != 40.5 || a["t"] != nil {
		t.Fatalf("tap 120 40.5 sent %v", a)
	}
	if !strings.Contains(out, "tapped <button id=\"go\"> at (120, 40.5)") {
		t.Fatalf("tap output %q", out)
	}
	h.mustRun(t, "tap", "#go")
	if a := h.page.last("act")["args"].(map[string]any); a["k"] != "tap" || a["t"] == nil {
		t.Fatalf("tap <target> sent %v", a)
	}
	if code, _, _ := h.run("tap", "#go", "x"); code != ExitUsage {
		t.Fatalf("tap #go x: exit %d, want a usage error", code)
	}
}

// W3b decision 5: fill --stdin reads the value in the client (ClientStep),
// so it is not in argv; the forwarded input carries it as the text
// argument. Without the client step (MCP, the daemon) --stdin is refused.
func TestFillStdinReadsTheValueInTheClient(t *testing.T) {
	c, _ := Lookup([]string{"fill"})
	in := Input{Args: []string{"#pw"}, Flags: map[string]any{"stdin": true}}
	if err := c.ClientStep(t.Context(), &in, strings.NewReader("s3cret -x\n")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(in.Args, []string{"#pw", "s3cret -x"}) || in.Flags["stdin"] != nil {
		t.Fatalf("after the client step: %+v", in)
	}
	if argv := Argv(c, in); !slices.Contains(argv, "s3cret -x") {
		t.Fatalf("forwarded argv %q lacks the value", argv)
	}
	bad := Input{Args: []string{"#pw", "x"}, Flags: map[string]any{"stdin": true}}
	if err := c.ClientStep(t.Context(), &bad, strings.NewReader("y")); ExitCodeOf(err) != ExitUsage {
		t.Fatalf("--stdin with a text argument: %v", err)
	}

	// Through RunCLI: the value read from the standard input reaches the page.
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"v": "s3cret"}))
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	_, _ = w.WriteString("s3cret\n")
	_ = w.Close()
	h.mustRun(t, "fill", "#pw", "--stdin")
	if a := h.page.last("act")["args"].(map[string]any); a["text"] != "s3cret" {
		t.Fatalf("fill --stdin sent %v", a)
	}
	if _, err := runFill(t.Context(), pageEnv{h.ps}, Input{Args: []string{"#pw"}, Flags: map[string]any{"stdin": true}}); ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), "MCP") {
		t.Fatalf("fill --stdin without the client step: %v", err)
	}
}

// ---- W3b repair round 1 (review of the actions part) ----

// Review finding 1 (design 3.5 click --new-tab, 2.6 fence): the open is a
// fenced attempt step of kind newtab (--force passed on). Each attempt
// first probes without a user gesture; only a probe that answers ready is
// followed by the actGesture call with emulateUserGesture, so an attempt
// that cannot dispatch gives the page no user activation.
func TestClickNewTabIsAFencedGestureAttempt(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("actProbe", attempt("retry", "ready to dispatch", nil))
	h.page.set("actGesture", attempt("dispatched", "", map[string]any{"v": map[string]any{"href": "https://example.test/next"}}))
	out := h.mustRun(t, "click", "#link", "--new-tab", "--force")
	if !strings.Contains(out, "opened https://example.test/next with window.open") {
		t.Fatalf("click --new-tab output %q", out)
	}
	if a := h.page.last("actGesture")["args"].(map[string]any); a["k"] != "newtab" || a["f"] != true || a["t"] == nil {
		t.Fatalf("actGesture args %v; want kind newtab with the target and force", a)
	}
	gesture := map[string]bool{}
	for _, c := range h.f.ReceivedMethod("Runtime.evaluate") {
		var p struct {
			Expression string `json:"expression"`
			Gesture    bool   `json:"emulateUserGesture"`
		}
		_ = json.Unmarshal(c.Params, &p)
		for _, fn := range []string{"actProbe", "actGesture"} {
			if strings.Contains(p.Expression, "call(\""+fn+"\"") {
				gesture[fn] = p.Gesture
			}
		}
	}
	if g, ok := gesture["actProbe"]; !ok || g {
		t.Fatalf("the probe was sent %v with emulateUserGesture %v; want sent without it", ok, g)
	}
	if g, ok := gesture["actGesture"]; !ok || !g {
		t.Fatalf("actGesture was sent %v with emulateUserGesture %v; want sent with it", ok, g)
	}

	h2 := newActionHarness(t)
	h2.page.set("actProbe", attempt("retry", "covered by <div class=\"modal\">", nil))
	code, _, errOut := h2.run("--timeout", "700ms", "click", "#link", "--new-tab")
	if code == 0 || !strings.Contains(errOut, "covered by <div class=\"modal\">") || slices.Contains(h2.page.fns(), "actGesture") {
		t.Fatalf("click --new-tab on a covered link: exit %d, stderr %q, page calls %v; want the cover named and no actGesture call", code, errOut, h2.page.fns())
	}
}

// hoverCSSHarness is an action harness whose page answers what hover
// --css sends: pick by reference, DOM.requestNode (node 42) and the CSS
// methods.
func hoverCSSHarness(t *testing.T, lim transport.Limits) *rtHarness {
	t.Helper()
	h := newActionHarnessWith(t, lim)
	h.ps.State = core.NewDesiredState()
	h.page.set("act", attempt("dispatched", "", map[string]any{}))
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "objectId": "obj-1"}}, nil // nodeOf's pick by reference
	})
	h.f.Result("DOM.getDocument", map[string]any{"root": map[string]any{"nodeId": 1}})
	h.f.Result("DOM.requestNode", map[string]any{"nodeId": 42})
	for _, m := range []string{"CSS.enable", "CSS.disable", "CSS.forcePseudoState"} {
		h.f.Result(m, map[string]any{})
	}
	return h
}

// cssCalls lists the CSS methods the page received, in wire order, with
// the forced classes of each CSS.forcePseudoState, without the session's
// agent reset before the first CSS.enable (fakewebkit.WithoutAgentResets).
func cssCalls(h *rtHarness) []string {
	var got []string
	for _, r := range fakewebkit.WithoutAgentResets(h.f.Received()) {
		switch {
		case r.Method == "CSS.forcePseudoState":
			got = append(got, r.Method+" "+string(r.Params))
		case strings.HasPrefix(r.Method, "CSS."):
			got = append(got, r.Method)
		}
	}
	return got
}

// ownerCalls counts the method calls that the page received from domain
// owners, without the session's agent resets (fakewebkit.WithoutAgentResets).
func ownerCalls(h *rtHarness, method string) int {
	n := 0
	for _, r := range fakewebkit.WithoutAgentResets(h.f.Received()) {
		if r.Method == method {
			n++
		}
	}
	return n
}

// Design 3.5 hover --css: WebKit applies CSS.forcePseudoState only while
// the CSS domain is enabled (checked on WebKitGTK, TestActionHoverCSS).
// Where the endpoint answers CSS.enable, the hover owner holds the domain
// (CSS.enable before the forced state), and the next action resets the
// forced state and disables the domain (design 2.4).
func TestHoverCSSForcesAndClearsThePseudoState(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	if out := h.mustRun(t, "hover", "#hov", "--css"); !strings.Contains(out, "CSS :hover forced") {
		t.Fatalf("hover --css output %q", out)
	}
	if !h.ps.State.Needs("CSS") {
		t.Fatal("CSS not needed after hover --css, want it held by the hover owner")
	}
	h.mustRun(t, "press", "Tab")
	want := []string{"CSS.enable", `CSS.forcePseudoState {"forcedPseudoClasses":["hover"],"nodeId":42}`, `CSS.forcePseudoState {"forcedPseudoClasses":[],"nodeId":42}`, "CSS.disable"}
	if got := cssCalls(h); !slices.Equal(got, want) {
		t.Fatalf("CSS calls %q, want %q (set by hover --css, cleared by the next action)", got, want)
	}
	if h.ps.State.Needs("CSS") {
		t.Fatal("CSS still needed after the next action, want no owner")
	}
}

// hover --css when CSS.enable fails on an endpoint that answers it: a
// protocol error changed nothing, so the domain is not held and nothing
// is dispatched; a lost reply leaves the outcome unknown, so the hover
// owner keeps the domain and the next action disables it (design 2.4,
// the rule of review finding R6-6). Without a desired state nothing could
// reset the forced state, so --css is refused.
func TestHoverCSSEnableFailures(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.f.Handle("CSS.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "CSS agent refused"}
	})
	if code, _, errOut := h.run("hover", "#hov", "--css"); code != ExitError || !strings.Contains(errOut, "CSS agent refused") {
		t.Fatalf("hover --css with a refused CSS.enable: exit %d, stderr %q", code, errOut)
	}
	if h.ps.State.Needs("CSS") || h.page.last("act") != nil {
		t.Fatalf("after a refused CSS.enable: CSS needed %v, act %v; want neither", h.ps.State.Needs("CSS"), h.page.last("act"))
	}

	h = hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.f.DropReplies("CSS.enable", 1)
	if code, _, _ := h.run("--timeout", "300ms", "hover", "#hov", "--css"); code == ExitOK {
		t.Fatal("hover --css succeeded although CSS.enable got no reply")
	}
	if !h.ps.State.Needs("CSS") {
		t.Fatal("CSS not needed after a lost CSS.enable reply, want it held by the hover owner")
	}
	h.mustRun(t, "press", "Tab")
	if got := cssCalls(h); !slices.Equal(got, []string{"CSS.enable", "CSS.disable"}) {
		t.Fatalf("CSS calls %q, want CSS.enable then CSS.disable by the next action", got)
	}

	h = hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.ps.State = nil
	if code, _, errOut := h.run("hover", "#hov", "--css"); code != ExitError || !strings.Contains(errOut, "no desired state") || h.page.last("act") != nil {
		t.Fatalf("hover --css without a desired state: exit %d, stderr %q", code, errOut)
	}
}

// hover --css whose hover fails after CSS.enable (the page refuses the
// target): the command fails, and the CSS domain it acquired is still
// disabled by the session's next action (design 2.4), not left enabled.
func TestHoverCSSFailedHoverStillReleasesTheDomain(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.page.set("act", attempt("failed", "ambiguous: #hov matches 2 elements", nil))
	if code, _, errOut := h.run("hover", "#hov", "--css"); code == ExitOK || !strings.Contains(errOut, "ambiguous") {
		t.Fatalf("hover --css on a refused target: exit %d, stderr %q; want the page's refusal", code, errOut)
	}
	h.page.set("act", attempt("dispatched", "", map[string]any{}))
	h.mustRun(t, "press", "Tab")
	if got := cssCalls(h); !slices.Equal(got, []string{"CSS.enable", "CSS.disable"}) {
		t.Fatalf("CSS calls %q, want CSS.enable by hover --css and CSS.disable by the next action, no forced state", got)
	}
	if h.ps.State.Needs("CSS") {
		t.Fatal("CSS still needed after the next action, want no owner")
	}
}

// Design 4.3 and the W3b-R1 brief: where CSS.enable is refused (it hangs
// through iwdp), hover --css cannot have its effect, so it fails with
// exit 2 and the reason before any event, and never reports success.
func TestHoverCSSFailsWhereCSSEnableIsRefused(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true})
	code, out, errOut := h.run("hover", "#hov", "--css")
	if code != ExitUsage || out != "" || !strings.Contains(errOut, "CSS.enable hangs through ios-webkit-debug-proxy") {
		t.Fatalf("hover --css: exit %d, stdout %q, stderr %q; want exit %d naming the CSS.enable refusal", code, out, errOut, ExitUsage)
	}
	if a := h.page.last("act"); a != nil {
		t.Fatalf("hover --css dispatched the hover (%v) although it fails", a)
	}
	if got := cssCalls(h); len(got) != 0 {
		t.Fatalf("CSS calls %q reached the page, want none", got)
	}
	if h.ps.State.Needs("CSS") {
		t.Fatal("CSS still needed after the refusal, want no owner")
	}
	if out := h.mustRun(t, "hover", "#hov"); strings.Contains(out, "CSS :hover forced") {
		t.Fatalf("hover without --css output %q", out)
	}
}

// legacyParam is one parameter of a legacy MCP tool's input struct.
type legacyParam struct{ name, typ string }

// legacyMCPParams reads cmd/iwdp-mcp/main.go: every addTool call's tool
// name and the json names and types of its input struct's fields.
func legacyMCPParams(t *testing.T) map[string][]legacyParam {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, filepath.Join("..", "..", "cmd", "iwdp-mcp", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	structs := map[string][]legacyParam{}
	tools := map[string][]legacyParam{}
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok {
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, fld := range st.Fields.List {
					if fld.Tag == nil {
						continue
					}
					tag, _ := strconv.Unquote(fld.Tag.Value)
					name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
					if name != "" && name != "-" {
						structs[ts.Name.Name] = append(structs[ts.Name.Name], legacyParam{name, types.ExprString(fld.Type)})
					}
				}
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "addTool" {
			return true
		}
		lit, _ := call.Args[1].(*ast.UnaryExpr)
		h, _ := call.Args[2].(*ast.FuncLit)
		if lit == nil || h == nil || len(h.Type.Params.List) != 3 {
			return true
		}
		var name string
		for _, e := range lit.X.(*ast.CompositeLit).Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok && kv.Key.(*ast.Ident).Name == "Name" {
				name, _ = strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			}
		}
		in, _ := h.Type.Params.List[2].Type.(*ast.Ident)
		if name != "" && in != nil {
			tools[name] = structs[in.Name]
		}
		return true
	})
	if len(tools) < 100 {
		t.Fatalf("read %d legacy tools from main.go; the parser missed the addTool calls", len(tools))
	}
	return tools
}

// W3b decision 2, design 4.1: an existing MCP tool keeps its parameters.
// For every legacy tool of cmd/iwdp-mcp/main.go that an action command or
// the registry screenshot now provides, the registry schema has each
// legacy parameter, and a call with all of them is accepted and delivers
// each value to an argument or flag. The call has the legacy JSON type of
// each parameter and passes the registry's input schema, as the MCP SDK
// checks it. navigate and reload are the
// navigation part's (its own test).
func TestTakenOverMCPToolsAcceptEveryLegacyParameter(t *testing.T) {
	legacy := legacyMCPParams(t)
	navigation := map[string]bool{"navigate": true, "reload": true}
	var checked []string
	for _, c := range All() {
		params, ok := legacy[c.MCPName]
		if c.MCPName == "" || !ok || navigation[c.MCPName] {
			continue
		}
		checked = append(checked, c.MCPName)
		props := InputSchema(c)["properties"].(map[string]any)
		call, values := map[string]any{}, []string{}
		for i, p := range params {
			if props[p.name] == nil {
				t.Fatalf("%s: the registry schema lacks the legacy parameter %q", c.MCPName, p.name)
			}
			var v any
			var want string // the text the command gets (mcpText)
			switch p.typ {
			case "string":
				v = fmt.Sprintf("value-%d", i)
				want = v.(string)
			case "int":
				v = 7 + i
				want = fmt.Sprint(v)
			case "bool":
				v, want = true, "true"
			case "float64":
				// A number in decimal notation without exponent: set_cookie's
				// expires in seconds since the epoch (W4 MCPSchema).
				v, want = 1893456000.5+float64(i), fmt.Sprintf("1893456%03d.5", i)
			case "map[string]string":
				// An object as its JSON: set_extra_headers' headers (W4
				// MCPSchema).
				v, want = map[string]any{"X-Test": fmt.Sprintf("value-%d", i)}, fmt.Sprintf(`{"X-Test":"value-%d"}`, i)
			default:
				t.Fatalf("%s: legacy parameter %q has type %s; extend this test", c.MCPName, p.name, p.typ)
			}
			call[p.name] = v
			values = append(values, want)
		}
		raw, _ := json.Marshal(call)
		// The MCP SDK validates every call against the tool's input
		// schema before the handler runs, so each legacy JSON type must
		// pass the schema too, not only mcpInput.
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := resolvedSchema(t, strings.Fields(c.Name)...).Validate(decoded); err != nil {
			t.Fatalf("%s %s: the registry schema refuses the legacy call: %v", c.MCPName, raw, err)
		}
		in, err := mcpInput(c, raw)
		if err != nil {
			t.Fatalf("%s %s: the legacy call is refused: %v", c.MCPName, raw, err)
		}
		got := slices.Clone(in.Args)
		for _, f := range in.Flags {
			got = append(got, fmt.Sprint(f))
		}
		for _, v := range values {
			if !slices.Contains(got, v) {
				t.Fatalf("%s %s: the value %s did not reach an argument or flag (args %q, flags %v)", c.MCPName, raw, v, in.Args, in.Flags)
			}
		}
	}
	slices.Sort(checked)
	// W3b's actions and screenshot, and W4's console, network, cookie
	// and emulation tools.
	if want := []string{"clear_console", "click", "fill", "get_console_messages", "highlight_node", "list_network_requests", "set_cookie", "set_emulated_conditions", "set_extra_headers", "take_screenshot", "type_text"}; !slices.Equal(checked, want) {
		t.Fatalf("the registry provides the legacy tools %v, want %v; update this list with the new tool's parameters checked", checked, want)
	}
}
