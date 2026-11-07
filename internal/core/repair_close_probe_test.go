package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the W1b close-probe delta (docs/notes/w1b.md,
// "Review repairs, closure: a canceled probe is no stall"): a liveness
// probe that its owner cancels (Manager.Close, the operation's end or
// cancel, the client's cancel) says nothing about the page. Only a probe
// that gets no reply within its time, or fails on the wire, marks the page
// stalled (design 2.6).

// consoleOwner attaches the rig's page and leaves Console enabled for
// agent's session, so that the release cleanup sends Console.disable.
func consoleOwner(t *testing.T, r *rig) *Page {
	t.Helper()
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "console", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctxFor(t, 5*time.Second), mo.ID, "Console.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	return mo.Page
}

// closeAndCountCleanup closes the manager within the shutdown budget and
// returns how many Console.disable the page received after Close started.
func closeAndCountCleanup(t *testing.T, r *rig) int {
	t.Helper()
	mark := lastSeq(r.f)
	if err := r.m.Close(ctxFor(t, 5*time.Second)); err != nil {
		t.Logf("Close: %v", err)
	}
	return consoleDisablesAfter(r.f, mark)
}

// lastSeq is the sequence number of the fake's newest log record.
func lastSeq(f *fakewebkit.Server) int {
	log := f.Log()
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].Seq
}

// consoleDisablesAfter counts the Console.disable the fake received after
// the log record mark. These tests count only the release cleanup that
// Close sends. A new connection's setup also sends a Console.disable
// before the first Console.enable on a target, because it resets every
// agent whose state the connection does not know (docs/notes/w1b.md,
// "Review repairs, unknown backends"). That reset goes out before Close
// starts, so counting from a mark taken just before Close excludes it and
// keeps the contract: exactly one cleanup on a live page, none on a
// stalled page.
func consoleDisablesAfter(f *fakewebkit.Server, mark int) int {
	n := 0
	for _, rec := range f.ReceivedMethod("Console.disable") {
		if rec.Seq > mark {
			n++
		}
	}
	return n
}

// The closure reviewer's reproduction (tree ef4d5221): Close cancels a
// background liveness probe that is waiting for its reply. The canceled
// probe marked the page stalled, so Close's release skipped the cleanup
// (Console.disable) and Close returned nil with the page state left behind.
// The test holds pg.mu only to order Close's lease inspection after the
// canceled probe returned.
func TestShutdownCancellationDoesNotLoseCleanup(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	pg := consoleOwner(t, r)
	before := evalsOf(r.f, "1")
	r.f.Stall("Runtime.evaluate")
	if !r.m.goBackground(func(bg context.Context) { _ = pg.conn.probeNow(bg) }) {
		t.Fatal("the background probe did not start")
	}
	eventually(t, 5*time.Second, "the background probe on the page", func() bool { return evalsOf(r.f, "1") > before })
	pg.mu.Lock()
	locked := true
	defer func() {
		if locked {
			pg.mu.Unlock()
		}
	}()
	// Count only what Close sends (consoleDisablesAfter).
	mark := lastSeq(r.f)
	ended := make(chan error, 1)
	go func() { ended <- r.m.Close(ctxFor(t, 5*time.Second)) }()
	select {
	case <-r.m.bgCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the background goroutines")
	}
	eventually(t, 5*time.Second, "the canceled probe returns", func() bool {
		pg.conn.mu.Lock()
		defer pg.conn.mu.Unlock()
		return !pg.conn.probing
	})
	got := pg.conn.State()
	pg.mu.Unlock()
	locked = false
	var err error
	select {
	case err = <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if got != ConnOK {
		t.Errorf("state after Close canceled the probe = %s, want attached", got)
	}
	if n := consoleDisablesAfter(r.f, mark); n != 1 {
		t.Fatalf("Close sent Console.disable %d times, want 1 (Close error %v)", n, err)
	}
}

// The operation's own cancellation (here the client goes away) during the
// probe that follows a call without a reply in its call budget (design 2.6
// step 2): the call returns its own error, not a stall, the page stays
// attached and the release cleanup still runs.
func TestOperationCancelDuringTheProbeIsNotAStall(t *testing.T) {
	// The session keeps its default call budget, so the attach, the replay
	// of the desired state and Close's cleanup are bounded only by their
	// deadlines. Only the stalled call gets the short budget below.
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	pg := consoleOwner(t, r)
	ctx := ctxFor(t, 5*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "read", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.f.Stall("DOM.getDocument")
	r.f.Stall("Runtime.evaluate")
	before := evalsOf(r.f, "1")
	sent := make(chan error, 1)
	go func() {
		// The probe that follows inherits this budget (Session.Probe).
		_, err := mo.Op.Caller().Send(webkit.WithCallBudget(ctx, 200*time.Millisecond), "DOM.getDocument", nil)
		sent <- err
	}()
	eventually(t, 5*time.Second, "the probe after the call budget", func() bool { return evalsOf(r.f, "1") > before })
	mo.Op.Cancel(errors.New("the client went away"))
	select {
	case err = <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return after its operation was canceled")
	}
	if !errors.Is(err, webkit.ErrCallTimeout) || errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) == CodeBusy {
		t.Errorf("call = %v (exit %d), want its ErrCallTimeout, not a stall", err, exitCode(err))
	}
	if st := pg.ConnState(); st != ConnOK {
		t.Errorf("state after the canceled probe = %s, want attached", st)
	}
	r.m.End(mo.ID)
	if n := closeAndCountCleanup(t, r); n != 1 {
		t.Errorf("Console.disable sent %d times by Close, want 1", n)
	}
}

// The client's cancellation of a command during the idle probe (design
// 2.6 step 4): the command ends with the cancellation, not with "did not
// answer the liveness probe", and the page stays attached.
func TestCanceledIdleProbeIsNotAStall(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{IdleProbe: 100 * time.Millisecond})
	r.reserve(t, "agent")
	pg := consoleOwner(t, r)
	time.Sleep(250 * time.Millisecond)
	r.f.Stall("Runtime.evaluate")
	before := evalsOf(r.f, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && evalsOf(r.f, "1") == before; {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := r.m.Begin(ctx, r.req, agent, "b", ClassExclusive)
	if !errors.Is(err, context.Canceled) || errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) == CodeBusy {
		t.Errorf("Begin canceled during the idle probe: %v (exit %d), want the cancellation, not a stall", err, exitCode(err))
	}
	if n := evalsOf(r.f, "1"); n != before+1 {
		t.Errorf("probes = %d, want 1", n-before)
	}
	if st := pg.ConnState(); st != ConnOK {
		t.Errorf("state after the canceled idle probe = %s, want attached", st)
	}
	if n := closeAndCountCleanup(t, r); n != 1 {
		t.Errorf("Console.disable sent %d times by Close, want 1", n)
	}
}

// The client's cancellation during the probe of a fresh attach (design
// 2.6 step 4, hidden tab): the command ends with the cancellation, not
// with "tab is not answering".
func TestCanceledAttachProbeIsNoHiddenTab(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	r.f.Stall("Runtime.evaluate")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && evalsOf(r.f, "1") == 0; {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := r.m.Begin(ctx, r.req, agent, "a", ClassExclusive)
	if !errors.Is(err, context.Canceled) || strings.Contains(fmt.Sprint(err), "not answering") || exitCode(err) == CodeBusy {
		t.Errorf("Begin canceled during the attach probe: %v (exit %d), want the cancellation, not a hidden tab", err, exitCode(err))
	}
}

// The other side of the rule: a probe that gets no reply within its time
// marks the page stalled, also when the time is the operation's deadline
// (the deadline ends the call context with a cancel whose cause is
// ErrDeadline) and also when Close follows. A stalled page gets no cleanup
// (design 2.4: no reconnect just to clean up).
func TestProbeTimeoutStillStalls(t *testing.T) {
	t.Run("own budget, then Close", func(t *testing.T) {
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
		r.reserve(t, "agent")
		pg := consoleOwner(t, r)
		r.m.recoveryHold = func(ctx context.Context) { <-ctx.Done() } // keep the stalled connection's page stalled
		r.f.Stall("Runtime.evaluate")
		probed := make(chan error, 1)
		if !r.m.goBackground(func(bg context.Context) {
			pctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
			defer cancel()
			probed <- pg.conn.probeNow(pctx)
		}) {
			t.Fatal("the background probe did not start")
		}
		var err error
		select {
		case err = <-probed:
		case <-time.After(5 * time.Second):
			t.Fatal("the probe did not end at its budget")
		}
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errProbeCanceled) {
			t.Errorf("probe = %v, want its deadline", err)
		}
		if st := pg.ConnState(); st != ConnStalled {
			t.Fatalf("state after the probe timed out = %s, want stalled", st)
		}
		if n := closeAndCountCleanup(t, r); n != 0 {
			t.Errorf("Console.disable sent %d times to a stalled page, want 0", n)
		}
	})
	t.Run("operation deadline during the probe", func(t *testing.T) {
		// The call budget (900 ms) ends the call within the operation's
		// deadline (1.5 s); the probe that follows has the same budget, so
		// the operation's deadline ends it first.
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Session: webkit.SessionOptions{CallTimeout: 900 * time.Millisecond}})
		r.reserve(t, "agent")
		r.m.recoveryHold = func(ctx context.Context) { <-ctx.Done() }
		mo, err := r.m.Begin(ctxFor(t, 1500*time.Millisecond), r.req, agent, "read", ClassExclusive)
		if err != nil {
			t.Fatal(err)
		}
		defer r.m.End(mo.ID)
		r.f.Stall("DOM.getDocument")
		r.f.Stall("Runtime.evaluate")
		// The send context outlives the operation: the operation's deadline
		// cancels the call context with a deadline cause.
		_, err = mo.Op.Caller().Send(ctxFor(t, 5*time.Second), "DOM.getDocument", nil)
		if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy {
			t.Errorf("call = %v (exit %d), want ErrSessionStalled (exit 4)", err, exitCode(err))
		}
		// "context canceled": the operation's deadline ended the probe, not
		// the probe's own budget ("did not answer").
		pg := mo.Page
		pg.conn.mu.Lock()
		stallErr := pg.conn.stallErr
		pg.conn.mu.Unlock()
		if stallErr == nil || !strings.Contains(stallErr.Error(), "context canceled") {
			t.Errorf("stall error %v, want the probe ended by the operation's deadline", stallErr)
		}
		if st := mo.Page.ConnState(); st != ConnStalled {
			t.Errorf("state after the probe ran into the operation deadline = %s, want stalled", st)
		}
	})
}

// A background recovery that Close cancels did not fail: the page is not
// reported as "recovery failed" and the log does not say the new
// connection did not answer.
func TestRecoveryCanceledByCloseIsNoFailure(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Logf: func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	}})
	r.reserve(t, "agent")
	held := make(chan struct{})
	r.m.recoveryHold = func(ctx context.Context) {
		close(held)
		<-ctx.Done()
	}
	pg := consoleOwner(t, r)
	r.f.Stall("Runtime.evaluate")
	if err := pg.conn.probeNow(ctxFor(t, 200*time.Millisecond)); err == nil {
		t.Fatal("the probe of a stalled page succeeded")
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("no background recovery started after the failed probe")
	}
	if err := r.m.Close(ctxFor(t, 5*time.Second)); err != nil {
		t.Logf("Close: %v", err)
	}
	pg.conn.mu.Lock()
	stallErr := pg.conn.stallErr
	pg.conn.mu.Unlock()
	if errors.Is(stallErr, ErrRecoveryFailed) {
		t.Errorf("stall error after Close canceled the recovery: %v, want no recovery failure", stallErr)
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(logs, "\n")
	if strings.Contains(all, "background recovery failed") || !strings.Contains(all, "background recovery canceled") {
		t.Errorf("log:\n%s\nwant the recovery reported as canceled, not failed", all)
	}
}
