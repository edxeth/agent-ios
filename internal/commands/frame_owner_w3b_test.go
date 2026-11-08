package commands

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// W3b final repair round 2 (the reviewer's reproduction
// TestReviewOwnerChangeKeepsRefusedFrameCleanup; design 2.4, 3.1, 3.8): the
// owner-change cleanup of earlier frame contexts (releaseEarlierFrames)
// keeps a record whose release core refused because its target was
// replaced. The session selects the child frame on page-1, a commit puts
// page-2 in its place, and the next lease epoch runs a runtime command:
// the old context id is sent nowhere, epoch 1 stays recorded, and a later
// owner-change command releases the frame in its context on page-2.
func TestOwnerChangeKeepsRefusedFrameCleanup(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	h.page.set("get", okEnv("Title"))
	h.page.set("release", okEnv(true))
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, _, msg := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame: %s", msg)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("epochs with recorded contexts before the owner change = %v, want [1]", got)
	}
	h.f.Swap("page-2", 0)
	for end := time.Now().Add(time.Second); h.sess.ActiveTarget() != "page-2"; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("page-2 did not commit")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.sess.ActiveSetup(ctx); err != nil {
		t.Fatal(err)
	}
	h.ps.Sched.SetLease("agent", 2)
	if code, _, msg := fh.run("get", "title"); code != 0 {
		t.Fatalf("owner-change command: exit %d, %s", code, msg)
	}
	if n := len(callsOf(h, "release")); n != 0 {
		t.Fatalf("page release calls after the refusal = %d, want 0 (page-1's context is valid only on page-1)", n)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("epochs with recorded contexts after the refused cleanup = %v, want [1]", got)
	}
	// page-2 reports the frame's context (the daemon's provisional setup
	// enables Runtime there while an owner holds it; this harness session
	// has no setup), and the next owner-change command releases the frame
	// in that context.
	h.f.EmitFrom("page-2", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 27, "type": "normal", "frameId": "f-kid"}})
	for end := time.Now().Add(time.Second); h.ps.Sched.FrameContext("f-kid").ID != 27; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("page-2's context of the frame was not recorded")
		}
	}
	if code, _, msg := fh.run("get", "title"); code != 0 {
		t.Fatalf("second owner-change command: exit %d, %s", code, msg)
	}
	fh.mu.Lock()
	var released []any
	for _, e := range fh.evals {
		if e.fn == "release" {
			released = append(released, e.ctx)
		}
	}
	fh.mu.Unlock()
	if !reflect.DeepEqual(released, []any{float64(27)}) {
		t.Fatalf("release calls with contextId %v, want one in the frame's page-2 context 27", released)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); len(got) != 0 {
		t.Fatalf("epochs with recorded contexts after the second cleanup = %v, want none", got)
	}
}

// The one-attempt rule of releaseEarlierFrames stays for a release that
// reached the frame's context on its current target and failed (W3b final
// repair round 2, orchestrator ruling): the epoch's records are forgotten,
// so a context that fails or never answers does not cost every later
// command its budget.
func TestOwnerChangeForgetsAFrameCleanupThatReachedThePageAndFailed(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	h.page.set("get", okEnv("Title"))
	h.page.set("release", errEnv("busy", "the frame is busy"))
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, _, msg := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame: %s", msg)
	}
	h.ps.Sched.SetLease("agent", 2)
	if code, _, msg := fh.run("get", "title"); code != 0 {
		t.Fatalf("owner-change command: exit %d, %s", code, msg)
	}
	if n := len(callsOf(h, "release")); n != 1 {
		t.Fatalf("page release calls = %d, want 1 (one attempt)", n)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); len(got) != 0 {
		t.Fatalf("epochs with recorded contexts after the failed cleanup = %v, want none", got)
	}
	if code, _, msg := fh.run("get", "title"); code != 0 || len(callsOf(h, "release")) != 1 {
		t.Fatalf("next command: exit %d %s, release calls %d; want no second attempt", code, msg, len(callsOf(h, "release")))
	}
}

// W3b final repair round 3 (the reviewer's reproduction
// TestReviewRefusedCleanupSurvivesAnUnsentRetry; orchestrator ruling): a
// frame record is forgotten only after an attempt whose send reached the
// current target. After a refused cleanup, a retry that sends nothing to
// page-2 keeps the record: (a) its budget has already ended; (b) page-2's
// inventory is incomplete (this harness Session has no provisional Setup,
// so the desired state's Runtime owner never enabled Runtime on page-2,
// AgentStates(page-2) has no Runtime entry), the temporary owner joins that
// owner without a Runtime.enable, and the frame has no report.
func TestOwnerChangeKeepsARefusedFrameAcrossARetryThatSendsNothing(t *testing.T) {
	for _, name := range []string{"budget ended", "inventory incomplete"} {
		t.Run(name, func(t *testing.T) {
			fh := newFrameHarness(t)
			h := fh.h
			h.page.set("get", okEnv("Title"))
			h.page.set("release", okEnv(true))
			fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
			if code, _, msg := fh.run("frame", "#kid"); code != 0 {
				t.Fatalf("frame: %s", msg)
			}
			h.f.Swap("page-2", 0)
			for end := time.Now().Add(time.Second); h.sess.ActiveTarget() != "page-2"; time.Sleep(time.Millisecond) {
				if time.Now().After(end) {
					t.Fatal("page-2 did not commit")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := h.sess.ActiveSetup(ctx); err != nil {
				t.Fatal(err)
			}
			h.ps.Sched.SetLease("agent", 2)
			if code, _, msg := fh.run("get", "title"); code != 0 {
				t.Fatalf("owner-change command with the refused cleanup: %s", msg)
			}
			if got := h.ps.Sched.ContextEpochsBefore(2); !reflect.DeepEqual(got, []uint64{1}) {
				t.Fatalf("epochs with recorded contexts after the refusal = %v, want [1]", got)
			}
			if name == "budget ended" {
				op := fh.begin(t)
				ended, stop := context.WithCancel(context.Background())
				stop()
				releaseEarlierFrames(ended, opEnv{pageEnv{h.ps}, op}, h.ps, op)
				op.End()
			} else {
				if st := h.sess.AgentStates("page-2")["Runtime"]; st == webkit.AgentEnabled || h.ps.Sched.FrameContext("f-kid").ID != 0 {
					t.Fatalf("page-2 Runtime state %v, frame context %d; want an incomplete inventory without the frame", st, h.ps.Sched.FrameContext("f-kid").ID)
				}
				if code, _, msg := fh.run("get", "title"); code != 0 {
					t.Fatalf("retry: %s", msg)
				}
			}
			enables := 0
			for _, rec := range h.f.ReceivedMethod("Runtime.enable") {
				if rec.Target == "page-2" {
					enables++
				}
			}
			if enables != 0 || len(callsOf(h, "release")) != 0 {
				t.Fatalf("the retry sent page-2 %d Runtime.enable and %d releases, want none", enables, len(callsOf(h, "release")))
			}
			if got := h.ps.Sched.ContextEpochsBefore(2); !reflect.DeepEqual(got, []uint64{1}) {
				t.Fatalf("epochs with recorded contexts after a retry that sent nothing = %v, want [1]", got)
			}
		})
	}
}

// The same rule for a context recorded on the current target (no
// refusal): an owner-change cleanup whose budget has already ended sends
// nothing and keeps the record, and the next owner-change command
// releases it there.
func TestOwnerChangeKeepsAFrameContextWhenTheBudgetEndedFirst(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	h.page.set("get", okEnv("Title"))
	h.page.set("release", okEnv(true))
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, _, msg := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame: %s", msg)
	}
	h.ps.Sched.SetLease("agent", 2)
	op := fh.begin(t)
	ended, stop := context.WithCancel(context.Background())
	stop()
	releaseEarlierFrames(ended, opEnv{pageEnv{h.ps}, op}, h.ps, op)
	op.End()
	if n := len(callsOf(h, "release")); n != 0 {
		t.Fatalf("release calls with an ended budget = %d, want 0", n)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("epochs with recorded contexts after a cleanup that sent nothing = %v, want [1]", got)
	}
	if code, _, msg := fh.run("get", "title"); code != 0 {
		t.Fatalf("next owner-change command: %s", msg)
	}
	if n := len(callsOf(h, "release")); n != 1 {
		t.Fatalf("release calls after the next command = %d, want 1", n)
	}
	if got := h.ps.Sched.ContextEpochsBefore(2); len(got) != 0 {
		t.Fatalf("epochs with recorded contexts after the release = %v, want none", got)
	}
}
