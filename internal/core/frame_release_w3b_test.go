package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W3b-R4, from the navigation review's reproduction
// TestReviewReconnectAfterFrameMain (design 2.4: a domain is enabled for
// its first owner and disabled when its last owner leaves; 3.7 "Release
// restores the original functions"; 3.8 frame contexts): a frame-scoped
// lease ran "frame main", so no owner holds Runtime when its connection is
// replaced, and the reconnect's re-apply makes WebKit report no frame
// context on the new connection. A release that finds such a recorded
// frame holds Runtime as a temporary owner of the releasing session, waits
// (bounded) for the frame's report, runs the page-side release in the
// frame's new context, and gives the ownership back.

// frameReleaseRig is a lease whose operation selected the frame "kid"
// (context 7 on the first connection) and returned to the top document,
// after a background recovery replaced the first connection.
type frameReleaseRig struct {
	*rig
	ps    *PageSession
	pg    *Page
	epoch uint64

	mu       sync.Mutex
	released []string // "[context,epoch]" of each page-side release; main is 0
	logs     []string
	onFrame  func() // runs in the page-side release of a frame context
}

// newFrameReleaseRig builds the rig. newID is the frame's context on the
// second connection, reported at a Runtime.enable there; 0 reports none.
// holder, when set, is an owner that holds Runtime from before the
// reconnect on and stays.
func newFrameReleaseRig(t *testing.T, newID int64, holder Owner) *frameReleaseRig {
	t.Helper()
	fr := &frameReleaseRig{}
	fr.rig = newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test", Logf: func(f string, a ...any) {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		fr.logs = append(fr.logs, fmt.Sprintf(f, a...))
	}})
	r := fr.rig
	r.reserve(t, "agent")
	r.f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		id := int64(7)
		if c.Conn != 1 {
			id = newID
		}
		if id != 0 {
			r.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "type": "normal", "frameId": "kid"}})
		}
		return map[string]any{}, nil
	})
	r.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string
			ContextID  int64 `json:"contextId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil && m[1] == "release" {
			var a struct{ Epoch int64 }
			_ = json.Unmarshal([]byte(m[2]), &a)
			fr.mu.Lock()
			fr.released = append(fr.released, fmtCtx(p.ContextID, a.Epoch))
			hook := fr.onFrame
			fr.mu.Unlock()
			if p.ContextID != 0 && hook != nil {
				hook()
			}
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": true}}}, nil
	})
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "frame", ClassMutate)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	fr.ps, fr.pg, fr.epoch = mo.Page.PS, mo.Page, mo.Epoch
	if holder != "" && fr.ps.State.Acquire(holder, "Runtime") {
		if _, err := mo.Op.Caller().Send(ctx, "Runtime.enable", nil); err != nil {
			t.Fatalf("Runtime.enable of %s: %v", holder, err)
		}
	}
	// "frame #kid" (the frame owner holds Runtime), then "frame main".
	owner := SessionOwner("frame", agent.Session)
	if fr.ps.State.Acquire(owner, "Runtime") {
		if _, err := mo.Op.Caller().Send(ctx, "Runtime.enable", nil); err != nil {
			t.Fatalf("Runtime.enable: %v", err)
		}
	}
	if id := fr.ps.Sched.FrameContext("kid").ID; id != 7 {
		t.Fatalf("frame context on the first connection = %d, want 7", id)
	}
	mo.Op.SetExecutionContext(fr.ps.Sched.FrameContext("kid"))
	mo.Op.SetExecutionContext(ExecContext{})
	if _, err := fr.ps.State.Release(ctx, owner, mo.Op.Caller()); err != nil {
		t.Fatalf("frame main: %v", err)
	}
	r.m.End(mo.ID)

	// The first connection stops answering; the recovery reconnects.
	r.f.StallConnection(1)
	mo2, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := r.m.Raw(ctxFor(t, time.Second), mo2.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err == nil {
		t.Fatal("Raw on the stalled connection succeeded")
	}
	r.m.End(mo2.ID)
	eventually(t, 15*time.Second, "the recovery to reconnect", func() bool {
		return r.f.ConnectionCount() == 2 && fr.pg.ConnState() == ConnOK
	})
	if id := fr.ps.Sched.FrameContext("kid").ID; id != 0 {
		t.Fatalf("frame context after the reconnect = %d, want none reported yet", id)
	}
	return fr
}

// onSecond counts the owners' calls of method on the second connection,
// without the session's agent reset before the first enable
// (fakewebkit.WithoutAgentResets).
func (fr *frameReleaseRig) onSecond(method string) int {
	n := 0
	for _, rec := range fakewebkit.WithoutAgentResets(fr.f.Received()) {
		if rec.Conn == 2 && rec.Method == method {
			n++
		}
	}
	return n
}

func (fr *frameReleaseRig) releasedCalls() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return slices.Clone(fr.released)
}

func (fr *frameReleaseRig) log() string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return strings.Join(fr.logs, "\n")
}

func (fr *frameReleaseRig) release(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	if err := fr.m.Release(ctxFor(t, 5*time.Second), agent.Session, fr.pg.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	return time.Since(start)
}

func hasOwnerPrefix(owners []Owner, prefix string) bool {
	return slices.ContainsFunc(owners, func(o Owner) bool { return strings.HasPrefix(string(o), prefix) })
}

// The temporary Runtime ownership is reference-counted like any other
// (design 2.4): alone it sends Runtime.enable and, at its end,
// Runtime.disable; with another owner that holds Runtime from before, or
// that acquires it while the release runs, it sends neither disable, and
// the other owner keeps Runtime. It never stays after the release.
func TestFrameReleaseHoldsRuntimeTemporarily(t *testing.T) {
	const holder = Owner("test-holder") // an owner of the page that is not the releasing session's
	t.Run("alone", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 17, "")
		fr.release(t)
		if got, want := fr.releasedCalls(), []string{fmtCtx(0, int64(fr.epoch)), fmtCtx(17, int64(fr.epoch))}; !reflect.DeepEqual(got, want) {
			t.Fatalf("release calls %q, want %q (main, then the frame's context on the new connection)", got, want)
		}
		if en, dis := fr.onSecond("Runtime.enable"), fr.onSecond("Runtime.disable"); en != 1 || dis != 1 {
			t.Fatalf("second connection: Runtime.enable %d, Runtime.disable %d; want 1 and 1 (the temporary owner)", en, dis)
		}
		if o := fr.ps.State.AllOwners(); hasOwnerPrefix(o, "frame-release:") || fr.ps.State.Needs("Runtime") {
			t.Fatalf("after the release: owners %q, Runtime needed %v; want no temporary owner and Runtime not needed", o, fr.ps.State.Needs("Runtime"))
		}
		if e := fr.ps.Sched.ContextEpochsBefore(fr.epoch + 1); len(e) != 0 {
			t.Fatalf("epochs with recorded contexts after the release = %v", e)
		}
	})
	t.Run("joins", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 17, "")
		fr.mu.Lock()
		fr.onFrame = func() {
			if fr.ps.State.Acquire(holder, "Runtime") {
				t.Error("another owner acquired Runtime as its first owner while the temporary owner held it")
			}
		}
		fr.mu.Unlock()
		fr.release(t)
		if got, want := fr.releasedCalls(), []string{fmtCtx(0, int64(fr.epoch)), fmtCtx(17, int64(fr.epoch))}; !reflect.DeepEqual(got, want) {
			t.Fatalf("release calls %q, want %q", got, want)
		}
		if en, dis := fr.onSecond("Runtime.enable"), fr.onSecond("Runtime.disable"); en != 1 || dis != 0 {
			t.Fatalf("second connection: Runtime.enable %d, Runtime.disable %d; want 1 and 0 (%s still holds Runtime)", en, dis, holder)
		}
		if o := fr.ps.State.AllOwners(); hasOwnerPrefix(o, "frame-release:") || !slices.Contains(o, holder) {
			t.Fatalf("owners after the release = %q, want %s and no temporary owner", o, holder)
		}
		if _, err := fr.ps.State.Release(ctxFor(t, time.Second), holder, fr.ps.Caller); err != nil {
			t.Fatalf("release of %s: %v", holder, err)
		}
		if dis := fr.onSecond("Runtime.disable"); dis != 1 {
			t.Fatalf("Runtime.disable after the last owner's release: %d, want 1", dis)
		}
	})
	t.Run("held", func(t *testing.T) {
		// Runtime is enabled on the new connection for holder, and the
		// frame reported nothing: it is gone, which counts as released.
		fr := newFrameReleaseRig(t, 0, holder)
		fr.release(t)
		if got, want := fr.releasedCalls(), []string{fmtCtx(0, int64(fr.epoch))}; !reflect.DeepEqual(got, want) {
			t.Fatalf("release calls %q, want %q", got, want)
		}
		if en, dis := fr.onSecond("Runtime.enable"), fr.onSecond("Runtime.disable"); en != 1 || dis != 0 {
			t.Fatalf("second connection: Runtime.enable %d, Runtime.disable %d; want 1 (the reconnect's re-apply for %s) and 0", en, dis, holder)
		}
		if o := fr.ps.State.AllOwners(); hasOwnerPrefix(o, "frame-release:") || !slices.Contains(o, holder) {
			t.Fatalf("owners after the release = %q, want %s and no temporary owner", o, holder)
		}
		if e := fr.ps.Sched.ContextEpochsBefore(fr.epoch + 1); len(e) != 0 {
			t.Fatalf("epochs with recorded contexts after the release = %v, want none (the gone frame counts as released)", e)
		}
	})
}

// runtimeReleaseLimit is the contract for the page-side runtime release
// of a lease: it waits at most 1 s for the page's answer
// (docs/notes/w3a-wiring.md, "Release budget rule": "waits at most
// min(runtimeReleaseBudget = 1 s, ...)"). It is the tests' own value, not
// the product's runtimeReleaseBudget, so that a change of that constant
// fails them.
const runtimeReleaseLimit = time.Second

// The wait for a frame's report is bounded within the release's budget
// (runtimeReleaseLimit): a frame that reports no context after the
// temporary owner's Runtime.enable is gone and counts as released, and a
// Runtime.enable that the page never answers leaves the frame recorded for
// the next owner. Both end the release within its budget, are logged, and
// leave no temporary owner.
func TestFrameReleaseWaitIsBounded(t *testing.T) {
	// slack covers the release barrier and the main context's release.
	const slack = 300 * time.Millisecond
	t.Run("no report", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 0, "")
		if took := fr.release(t); took > runtimeReleaseLimit+slack {
			t.Fatalf("the release took %v, want at most the runtime release budget %v", took, runtimeReleaseLimit)
		}
		if en, dis := fr.onSecond("Runtime.enable"), fr.onSecond("Runtime.disable"); en != 1 || dis != 1 {
			t.Fatalf("second connection: Runtime.enable %d, Runtime.disable %d; want 1 and 1 (the temporary owner)", en, dis)
		}
		// The log names the frame by its id and the outcome by the name
		// of core's sentinel (logsafe.Errorf, logsafe.Error).
		if l := fr.log(); !strings.Contains(l, "frame kid: core.errFrameGone") {
			t.Fatalf("log %q does not report the frame kid as gone", l)
		}
		if e := fr.ps.Sched.ContextEpochsBefore(fr.epoch + 1); len(e) != 0 {
			t.Fatalf("epochs with recorded contexts after the release = %v, want none", e)
		}
		if o := fr.ps.State.AllOwners(); hasOwnerPrefix(o, "frame-release:") {
			t.Fatalf("owners after the release = %q, want no temporary owner", o)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 17, "")
		fr.f.Stall("Runtime.enable")
		if took := fr.release(t); took > runtimeReleaseLimit+slack {
			t.Fatalf("the release took %v, want at most the runtime release budget %v", took, runtimeReleaseLimit)
		}
		if en := fr.onSecond("Runtime.enable"); en != 1 {
			t.Fatalf("second connection: Runtime.enable %d, want 1", en)
		}
		if l := fr.log(); !strings.Contains(l, "frame kid") {
			t.Fatalf("log %q does not report the frame kid", l)
		}
		if e := fr.ps.Sched.ContextEpochsBefore(fr.epoch + 1); !reflect.DeepEqual(e, []uint64{fr.epoch}) {
			t.Fatalf("epochs with recorded contexts after the release = %v, want [%d] (the frame was not reached)", e, fr.epoch)
		}
		if o := fr.ps.State.AllOwners(); hasOwnerPrefix(o, "frame-release:") {
			t.Fatalf("owners after the release = %q, want no temporary owner", o)
		}
	})
}
