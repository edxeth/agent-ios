package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
)

// opEnv is the daemon's Env of a command that runs inside an operation
// (daemon runEnv): the page session and the admitted operation.
type opEnv struct {
	pageEnv
	op *core.Op
}

func (e opEnv) Op() *core.Op { return e.op }

// Design 2.5 and 2.10: a command's runtime calls go through its
// operation's fenced caller (core.Op.Caller), so the scheduler counts them
// against the per-page limit and checks the operation and its lease epoch
// before every send. A call after the operation ended, or after the lease
// moved to another epoch, is never sent to the page.
func TestRuntimeCallsAreFencedByTheOperation(t *testing.T) {
	h := newHarness(t)
	h.page.set("get", okEnv("X"))
	sched := core.NewScheduler(h.sess, core.SchedulerOptions{})
	t.Cleanup(sched.Close)
	h.ps.Sched = sched
	sched.SetLease("s1", 1)
	begin := func() *core.Op {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		op, err := sched.Begin(ctx, core.OpSpec{Name: "get title", Class: core.ClassRead, Session: "s1", Epoch: 1})
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		return op
	}
	run := func(op *core.Op) (int, string) {
		var out, errb bytes.Buffer
		code := RunCLIStarted(context.Background(), []string{"--timeout", "5s", "get", "title"}, &out, &errb, nil,
			func(context.Context, *Invocation) (Env, error) { return opEnv{pageEnv{h.ps}, op}, nil })

		return code, errb.String()
	}

	// Within an active operation the call reaches the page.
	op := begin()
	if code, errOut := run(op); code != 0 {
		t.Fatalf("get title in an active operation: exit %d: %s", code, errOut)
	}
	if n := countCalls(h.page, "get"); n != 1 {
		t.Fatalf("the page received %d get calls, want 1", n)
	}

	// After the operation ended: refused before sending.
	op.End()
	if code, errOut := run(op); code == 0 || !strings.Contains(errOut, core.ErrOpEnded.Error()) {
		t.Fatalf("get title after the operation ended: exit %d, stderr %q; want %q", code, errOut, core.ErrOpEnded)
	}
	// After the lease moved to another epoch: refused before sending.
	op = begin()
	sched.SetLease("s2", 2)
	if code, errOut := run(op); code == 0 || !strings.Contains(errOut, core.ErrStaleEpoch.Error()) {
		t.Fatalf("get title after the lease epoch changed: exit %d, stderr %q; want %q", code, errOut, core.ErrStaleEpoch)
	}
	op.End()
	if n := countCalls(h.page, "get"); n != 1 {
		t.Fatalf("the page received %d get calls, want 1: the fenced calls were sent", n)
	}
}

func countCalls(p *fakePage, fn string) int {
	n := 0
	for _, f := range p.fns() {
		if f == fn {
			n++
		}
	}
	return n
}
