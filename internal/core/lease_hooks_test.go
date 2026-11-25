package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// hookSession is the only session whose lease starts the probe hook
// records; other tests of the package run with the hook registered too.
const hookSession = "lease-hook"

var (
	hookOnce  sync.Once
	hookMu    sync.Mutex
	hookCalls []uint64 // epochs, in call order
)

func registerHookProbe() {
	hookOnce.Do(func() {
		OnLeaseStart(func(ctx context.Context, ps *PageSession, session string, epoch uint64) error {
			if session != hookSession {
				return nil
			}
			hookMu.Lock()
			hookCalls = append(hookCalls, epoch)
			hookMu.Unlock()
			// The hook's own operation under the new lease: it must be
			// admitted, and its call must reach the page before the
			// operation that acquired the lease sends anything.
			op, err := ps.Sched.Begin(ctx, OpSpec{Name: "hook", Class: ClassMutate, Session: session, Epoch: epoch})
			if err != nil {
				return err
			}
			defer op.End()
			_, err = op.Caller().Send(op.Context(), "Network.enable", nil)
			return err
		})
	})
}

func hookEpochs() []uint64 {
	hookMu.Lock()
	defer hookMu.Unlock()
	return append([]uint64(nil), hookCalls...)
}

// Design 4.2 "network requests" (capture starts with the lease, MAP 1.8)
// through the lease-start hook: it runs once per lease, before the first
// registry operation of the lease sends anything; again for the next
// lease after a release; and again after a takeover. Operations without
// the registry mark (legacy commands, design 7.3 test 13) run no hook.
func TestLeaseHookRunsOncePerLeaseBeforeTheOperation(t *testing.T) {
	registerHookProbe()
	hookMu.Lock()
	hookCalls = nil
	hookMu.Unlock()
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ao := AcquireOptions{Session: hookSession, Reservation: "agent"}
	legacyCtx := ctxFor(t, 5*time.Second)
	ctx := WithLeaseHooks(legacyCtx)
	legacy, err := r.m.Begin(legacyCtx, r.req, ao, "legacy", ClassMutate)
	if err != nil {
		t.Fatalf("Begin (legacy): %v", err)
	}
	r.m.End(legacy.ID)
	if got := hookEpochs(); len(got) != 0 {
		t.Fatalf("hook epochs after a legacy operation = %v, want none", got)
	}
	run := func() *ManagedOp {
		t.Helper()
		mo, err := r.m.Begin(ctx, r.req, ao, "eval", ClassMutate)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err != nil {
			t.Fatalf("Runtime.evaluate: %v", err)
		}
		r.m.End(mo.ID)
		return mo
	}
	first := run()
	run()
	if got := hookEpochs(); len(got) != 1 || got[0] != first.Epoch || first.Epoch != legacy.Epoch {
		t.Fatalf("hook epochs after two registry operations of the legacy operation's lease (epoch %d) = %v, want [%d]", legacy.Epoch, got, first.Epoch)
	}
	enable, eval := -1, -1
	for i, rec := range r.f.Received() {
		switch {
		case rec.Method == "Network.enable" && enable < 0:
			enable = i
		case rec.Method == "Runtime.evaluate" && eval < 0 && string(rec.Params) == `{"expression":"1"}`:
			// The operation's call; the connect probe adds returnByValue.
			eval = i
		}
	}
	if enable < 0 || eval < 0 || enable > eval {
		t.Fatalf("the hook's Network.enable is message %d and the operation's evaluate %d; want the hook first", enable, eval)
	}

	if _, err := r.m.ReleaseSession(ctx, hookSession); err != nil {
		t.Fatalf("release: %v", err)
	}
	second := run()
	if got := hookEpochs(); len(got) != 2 || got[1] != second.Epoch || second.Epoch <= first.Epoch {
		t.Fatalf("hook epochs after a release and a new lease = %v, want [%d %d]", got, first.Epoch, second.Epoch)
	}

	pg := second.Page
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lease lost", func() bool { return pg.ConnState() == ConnLost })
	take := ao
	take.Takeover = true
	mo, err := r.m.Begin(ctx, r.req, take, "eval", ClassMutate)
	if err != nil {
		t.Fatalf("--takeover: %v", err)
	}
	r.m.End(mo.ID)
	if got := hookEpochs(); len(got) != 3 || got[2] != mo.Epoch || mo.Epoch <= second.Epoch {
		t.Fatalf("hook epochs after a takeover = %v, want a third call with epoch %d", got, mo.Epoch)
	}
}
