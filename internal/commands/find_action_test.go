package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design 4.2 "find <kind> <value> [action [args]]": the action runs on the
// single match through the action command (here click and fill, whose
// attempts the fake page answers); several matches are refused before any
// action, naming the count.
func TestFindWithAction(t *testing.T) {
	h := newActionHarness(t)
	one := func(ref string) map[string]any {
		return okEnv(map[string]any{
			"nodes":   []any{map[string]any{"ref": ref, "role": "button", "name": "Save"}},
			"refs":    []any{map[string]any{"ref": ref, "role": "button", "name": "Save", "unique": true}},
			"nextRef": 9, "doc": "doc-1",
		})
	}
	h.page.set("find", one("e8"))
	h.page.set("act", attempt("dispatched", "", map[string]any{}))
	out := h.mustRun(t, "find", "role", "button", "click", "--name", "Save")
	if !strings.HasPrefix(out, "@e8: ") {
		t.Fatalf("find ... click output %q, want it to name @e8", out)
	}
	a := h.page.last("act")["args"].(map[string]any)
	if a["k"] != "click" || a["t"].(map[string]any)["ref"] != "e8" {
		t.Fatalf("click attempt args %v, want kind click on ref e8", a)
	}

	h.page.set("find", one("e9"))
	h.mustRun(t, "find", "nth", "0", "input", "fill", "hello")
	if f := h.page.last("find"); f["value"] != "input" || f["index"] != float64(0) {
		t.Fatalf("find nth sent %v", f)
	}
	a = h.page.last("act")["args"].(map[string]any)
	if a["t"].(map[string]any)["ref"] != "e9" || !strings.Contains(mustJSON(t, a), "hello") {
		t.Fatalf("fill attempt args %v, want ref e9 and the text", a)
	}

	acts := len(callsOf(h, "act"))
	h.page.set("find", okEnv(map[string]any{
		"nodes":   []any{map[string]any{"ref": "e10", "role": "button", "name": "A"}, map[string]any{"ref": "e11", "role": "button", "name": "B"}},
		"refs":    []any{map[string]any{"ref": "e10", "role": "button", "name": "A"}, map[string]any{"ref": "e11", "role": "button", "name": "B"}},
		"nextRef": 12, "doc": "doc-1",
	}))
	code, _, errOut := h.run("find", "role", "button", "click")
	if code != ExitError || !strings.Contains(errOut, "matched 2 elements (@e10, @e11)") {
		t.Fatalf("ambiguous find click: exit %d, %q", code, errOut)
	}
	if n := len(callsOf(h, "act")); n != acts {
		t.Fatalf("an ambiguous find dispatched %d attempts", n-acts)
	}
	for _, bad := range [][]string{{"find", "role", "button", "explode"}, {"find", "role", "button", "fill"}, {"find", "text", "x", "click", "extra"}} {
		if code, _, errOut := h.run(bad...); code != ExitUsage {
			t.Fatalf("%v: exit %d (%q), want the usage exit", bad, code, errOut)
		}
	}
}

func callsOf(h *rtHarness, fn string) []pageCall {
	h.page.mu.Lock()
	defer h.page.mu.Unlock()
	var out []pageCall
	for _, c := range h.page.calls {
		if c.Fn == fn {
			out = append(out, c)
		}
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// frameHarness is the fake page of the frame tests: a top document with
// two child frames (f-same, f-kid); Runtime.enable reports the main
// world of main and f-kid (7) and a user world of f-kid (8); the "pick"
// expression of frameExpr answers info.
type frameHarness struct {
	t       *testing.T
	h       *rtHarness
	mu      sync.Mutex
	info    map[string]any
	lastCtx any
	// evals records the Runtime.evaluate calls of runtime functions:
	// the function, its contextId and its args.
	evals []frameEval
}

type frameEval struct {
	fn   string
	ctx  any
	args string
}

func newFrameHarness(t *testing.T) *frameHarness {
	fh := &frameHarness{t: t, h: newActionHarness(t)}
	h := fh.h
	h.ps.State = core.NewDesiredState()
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p map[string]any
		_ = json.Unmarshal(c.Params, &p)
		expr, _ := p["expression"].(string)
		fh.mu.Lock()
		fh.lastCtx = p["contextId"]
		cur := fh.info
		fh.mu.Unlock()
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(expr); m != nil {
			fh.mu.Lock()
			fh.evals = append(fh.evals, frameEval{fn: m[1], ctx: p["contextId"], args: m[2]})
			fh.mu.Unlock()
			return map[string]any{"result": map[string]any{"type": "object", "value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		if strings.Contains(expr, `window.__iwdp.call("pick"`) {
			return map[string]any{"result": map[string]any{"type": "object", "value": cur}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0}}, nil
	})
	h.f.Handle("Runtime.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		h.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
		h.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "f-kid"}})
		h.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 8, "type": "user", "frameId": "f-kid"}})
		return map[string]any{}, nil
	})
	h.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame":       map[string]any{"id": "main"},
		"childFrames": []any{map[string]any{"frame": map[string]any{"id": "f-same"}}, map[string]any{"frame": map[string]any{"id": "f-kid"}}},
	}})
	return fh
}

func (fh *frameHarness) begin(t *testing.T) *core.Op {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, epoch := fh.h.ps.Sched.Lease()
	op, err := fh.h.ps.Sched.Begin(ctx, core.OpSpec{Name: "test", Class: core.ClassMutate, Session: session, Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// run runs a command inside an operation of the current lease, as the
// daemon does.
func (fh *frameHarness) run(args ...string) (int, string, string) {
	t := fh.t
	t.Helper()
	op := fh.begin(t)
	defer op.End()
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), append([]string{"--timeout", "5s"}, args...), &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return opEnv{pageEnv{fh.h.ps}, op}, nil })

	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

func (fh *frameHarness) setInfo(m map[string]any) { fh.mu.Lock(); fh.info = m; fh.mu.Unlock() }

// scopeOf applies the session's frame scope to a new operation and pings
// the runtime: the scope's frame id, the operation's context and the
// contextId the ping was sent with.
func (fh *frameHarness) scopeOf() (string, int64, any) {
	t := fh.t
	t.Helper()
	op := fh.begin(t)
	defer op.End()
	id, err := applyFrameScope(fh.h.ps, op, fh.h.ps.Runtime.Generation())
	if err != nil {
		return "error: " + err.Error(), 0, nil
	}
	if err := fh.h.ps.Runtime.Via(op.Caller()).Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return id, op.ExecutionContext().ID, fh.lastCtx
}

// Design 3.8 (review finding 3): frame selects the iframe's execution
// context as the session's scope; later operations of the session send
// their Runtime.evaluate calls there, per lease epoch and top document;
// frame main ends it; a frame without a context, or a frame list that
// does not match the protocol's, is unsupported with the reason; a target
// that is no frame is a usage error.
func TestFrame(t *testing.T) {
	fh := newFrameHarness(t)
	h, run, setInfo, scopeOf := fh.h, fh.run, fh.setInfo, fh.scopeOf

	if code, out, _ := run("frame", "main"); code != 0 || out != "commands run in the main document" {
		t.Fatalf("frame main: %d %q", code, out)
	}
	setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, out, errOut := run("frame", "#kid"); code != 0 || !strings.Contains(out, "frame #kid (child.html): later commands") {
		t.Fatalf("frame #kid: %d %q %q", code, out, errOut)
	}
	if id, ctxID, sent := scopeOf(); id != "f-kid" || ctxID != 7 || sent != float64(7) {
		t.Fatalf("after frame #kid: scope %q, context %d, sent contextId %v; want f-kid, 7 (the main world, not the user world 8)", id, ctxID, sent)
	}
	if n := len(h.f.ReceivedMethod("Runtime.enable")); n != 1 {
		t.Fatalf("Runtime.enable sent %d times, want once", n)
	}
	// Another lease epoch (another owner) does not inherit the scope.
	h.ps.Sched.SetLease("other", 2)
	if id, ctxID, sent := scopeOf(); id != "" || ctxID != 0 || sent != nil {
		t.Fatalf("another lease: scope %q, context %d, sent %v; want the main document", id, ctxID, sent)
	}
	// A new lease of the same session (epochs only grow) starts without it.
	h.ps.Sched.SetLease("agent", 3)
	if id, _, _ := scopeOf(); id != "" {
		t.Fatalf("the scope of an earlier epoch came back: %q", id)
	}
	// A new top document ends the scope.
	if code, _, errOut := run("frame", "#kid"); code != 0 {
		t.Fatalf("frame #kid again: %q", errOut)
	}
	h.setGen(2)
	if id, _, _ := scopeOf(); id != "" {
		t.Fatalf("after a navigation the scope is %q, want the main document", id)
	}
	// A frame whose context went away fails until frame is run again.
	h.setGen(1)
	if code, _, errOut := run("frame", "#kid"); code != 0 {
		t.Fatalf("frame #kid again: %q", errOut)
	}
	h.f.Emit("Page.frameDetached", map[string]any{"frameId": "f-kid"})
	for end := time.Now().Add(2 * time.Second); h.ps.Sched.FrameContext("f-kid").ID != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("Page.frameDetached did not remove the frame's context")
		}
	}
	if id, _, _ := scopeOf(); !strings.Contains(id, "no execution context now") {
		t.Fatalf("a frame without its context: %q", id)
	}
	if code, out, _ := run("frame", "main"); code != 0 || out != "commands run in the main document" {
		t.Fatalf("frame main: %d %q", code, out)
	}
	if n := ownerCalls(h, "Runtime.disable"); n != 1 {
		t.Fatalf("frame main sent Runtime.disable %d times, want once (the session's Runtime ownership ends)", n)
	}
	if id, _, _ := scopeOf(); id != "" {
		t.Fatalf("after frame main the scope is %q", id)
	}

	setInfo(map[string]any{"tag": "iframe", "src": "https://other.test/", "index": 0, "frames": 2})
	if code, _, errOut := run("frame", "#ad"); code != ExitError || !strings.Contains(errOut, "unsupported") || !strings.Contains(errOut, "no execution context for this frame") {
		t.Fatalf("frame without a context: exit %d, %q", code, errOut)
	}
	setInfo(map[string]any{"tag": "iframe", "src": "x.html", "index": 0, "frames": 3})
	if code, _, errOut := run("frame", "#x"); code != ExitError || !strings.Contains(errOut, "the page lists 3 frames here") {
		t.Fatalf("frame list mismatch: exit %d, %q", code, errOut)
	}
	setInfo(map[string]any{"tag": "iframe", "src": "", "index": -1, "frames": 2})
	if code, _, errOut := run("frame", "#detached"); code != ExitError || !strings.Contains(errOut, "has no window") {
		t.Fatalf("frame without a window: exit %d, %q", code, errOut)
	}
	setInfo(map[string]any{"tag": "div", "src": "", "index": -1, "frames": 2})
	if code, _, errOut := run("frame", "#box"); code != ExitUsage || !strings.Contains(errOut, "not an iframe") {
		t.Fatalf("frame on a div: exit %d, %q", code, errOut)
	}
	// Without the daemon's operation there is no scope to keep.
	if code, _, errOut := h.run("frame", "#kid"); code == 0 || !strings.Contains(errOut, "scheduler") {
		t.Fatalf("frame outside an operation: exit %d, %q", code, errOut)
	}
}

// A frame without a src (srcdoc, or filled by a script) is named without
// an empty "()" (device check of 2026-11-11).
func TestFrameWithoutASrc(t *testing.T) {
	fh := newFrameHarness(t)
	fh.setInfo(map[string]any{"tag": "iframe", "src": "", "index": 1, "frames": 2})
	if code, out, errOut := fh.run("frame", "#doc"); code != 0 || !strings.HasPrefix(out, "frame #doc: later commands") {
		t.Fatalf("frame #doc without a src: %d %q %q", code, out, errOut)
	}
}

// W3b-R2 finding 2 (design 2.4: "the disable command is sent only when the
// count reaches zero"): a frame selection while another owner holds
// Runtime sends neither Runtime.disable nor Runtime.enable and still finds
// the frame's context, which the page reported at that owner's enable;
// "frame main" then leaves Runtime enabled for that owner.
func TestFrameKeepsAnotherRuntimeOwnersDomain(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	legacy := core.SessionOwner("legacy", "agent")
	if !h.ps.State.Acquire(legacy, "Runtime") {
		t.Fatal("the legacy owner is not the first Runtime owner")
	}
	if _, err := h.ps.Caller.Send(t.Context(), "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); h.ps.Sched.FrameContext("f-kid").ID == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the page's context reports did not reach the scheduler")
		}
	}
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, out, errOut := fh.run("frame", "#kid"); code != 0 || !strings.Contains(out, "frame #kid (child.html)") {
		t.Fatalf("frame #kid: %d %q %q", code, out, errOut)
	}
	if id, ctxID, sent := fh.scopeOf(); id != "f-kid" || ctxID != 7 || sent != float64(7) {
		t.Fatalf("scope %q, context %d, sent %v; want f-kid in context 7", id, ctxID, sent)
	}
	if code, _, errOut := fh.run("frame", "main"); code != 0 {
		t.Fatalf("frame main: %q", errOut)
	}
	if n, e := ownerCalls(h, "Runtime.disable"), ownerCalls(h, "Runtime.enable"); n != 0 || e != 1 {
		t.Fatalf("Runtime.disable sent %d times and Runtime.enable %d times; want 0 and only the legacy owner's 1", n, e)
	}
	// The frame owner holds only Runtime, and the legacy owner only
	// Runtime here: with Runtime needed, legacy present and no frame owner
	// left, legacy is the only Runtime owner.
	got := h.ps.State.AllOwners()
	frameLeft := slices.ContainsFunc(got, func(o core.Owner) bool { return strings.HasPrefix(string(o), "frame:") })
	if !h.ps.State.Needs("Runtime") || !slices.Contains(got, legacy) || frameLeft {
		t.Fatalf("owners %v (Runtime needed: %v), want only %s holding Runtime", got, h.ps.State.Needs("Runtime"), legacy)
	}
}

// W3b-R2 finding 1, owner change (design 2.4, 3.1): when a lease ends
// without reaching the runtime state it left in a frame context (here a
// takeover: the lease epoch moves on without a release), the next
// owner's first runtime command releases that epoch in the frame context
// before its own calls; the main context is reset by the page itself.
func TestOwnerChangeReleasesEarlierFrameContexts(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	h.page.set("get", okEnv("Title"))
	h.page.set("release", okEnv(true))
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, _, errOut := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame #kid: %q", errOut)
	}
	if code, _, errOut := fh.run("get", "title"); code != 0 {
		t.Fatalf("get title in the frame: %q", errOut)
	}
	h.ps.Sched.SetLease("agent", 2)
	fh.mu.Lock()
	fh.evals = nil
	fh.mu.Unlock()
	if code, _, errOut := fh.run("get", "title"); code != 0 {
		t.Fatalf("get title after the owner change: %q", errOut)
	}
	fh.mu.Lock()
	evals := append([]frameEval(nil), fh.evals...)
	fh.mu.Unlock()
	if len(evals) < 2 || evals[0].fn != "release" || evals[0].ctx != float64(7) || evals[0].args != `{"epoch":1}` || evals[1].ctx != nil {
		t.Fatalf("runtime calls after the owner change %+v; want release of epoch 1 in context 7 first, then the command in the main document", evals)
	}
	// Once released, the next command sends no release.
	fh.mu.Lock()
	fh.evals = nil
	fh.mu.Unlock()
	if code, _, errOut := fh.run("get", "title"); code != 0 {
		t.Fatalf("get title: %q", errOut)
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	for _, e := range fh.evals {
		if e.fn == "release" {
			t.Fatalf("a second release was sent: %+v", fh.evals)
		}
	}
}
