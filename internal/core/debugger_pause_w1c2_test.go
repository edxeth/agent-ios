package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W1c-2 (backlog C02): Debugger pauses are tracked per target and resumed
// on their own target. WebKit pauses the JavaScript of the backend that sent
// Debugger.paused, and only a Debugger.resume sent to that backend resumes
// it; another backend answers "Must be paused"
// (InspectorDebuggerAgent::assertPaused). During a cross-site navigation a
// breakpoint can pause the provisional target, which keeps its id when it
// commits (docs/notes/w1b.md, "WebKit facts behind the model"). A resume
// sent to the active target instead left the new page paused, and the
// release barrier counted the pause as answered.
//
// Oracle: pauseModel, the test's per-target model of WebKit's pauses, not
// the scheduler.

// webkitNotPaused is WebKit's error for a Debugger.resume on a backend that
// is not paused (InspectorDebuggerAgent::assertPaused).
const webkitNotPaused = "Must be paused"

// pauseModel answers Debugger.resume on f like WebKit: a target is paused
// from its Debugger.paused event until a resume reaches that target.
type pauseModel struct {
	f       *fakewebkit.Server
	mu      sync.Mutex
	paused  map[string]bool
	resumes []string      // target of every resume that ended a pause, in order
	wrong   []string      // targets that got a resume while not paused
	calls   int           // Debugger.resume calls received
	gate    chan struct{} // when set, a resume waits until it is closed
	// repause is the number of later resumes after which the target pauses
	// again before the reply (a newer pause that WebKit reports before the
	// reply to the resume of the older one).
	repause int
}

func newPauseModel(f *fakewebkit.Server) *pauseModel {
	m := &pauseModel{f: f, paused: map[string]bool{}}
	f.Handle("Debugger.resume", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		m.calls++
		gate := m.gate
		m.mu.Unlock()
		if gate != nil {
			<-gate
		}
		m.mu.Lock()
		if !m.paused[c.Target] {
			m.wrong = append(m.wrong, c.Target)
			m.mu.Unlock()
			return nil, &fakewebkit.Error{Code: -32000, Message: webkitNotPaused}
		}
		delete(m.paused, c.Target)
		m.resumes = append(m.resumes, c.Target)
		again := m.repause > 0
		if again {
			m.repause--
		}
		m.mu.Unlock()
		if again {
			m.pause(c.Target) // the event goes out before the reply
		}
		return nil, nil
	})
	return m
}

// pause makes target report a breakpoint pause.
func (m *pauseModel) pause(target string) {
	m.mu.Lock()
	m.paused[target] = true
	m.mu.Unlock()
	m.f.EmitFrom(target, "Debugger.paused", map[string]any{"reason": "Breakpoint", "callFrames": []any{}})
}

// gone forgets target: WebKit destroyed it with its paused JavaScript.
func (m *pauseModel) gone(target string) {
	m.mu.Lock()
	delete(m.paused, target)
	m.mu.Unlock()
}

func (m *pauseModel) state() (paused, resumes, wrong []string, calls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t := range m.paused {
		paused = append(paused, t)
	}
	slices.Sort(paused)
	return paused, slices.Clone(m.resumes), slices.Clone(m.wrong), m.calls
}

func resumeOn(target string) ControlMsg {
	return ControlMsg{Kind: ControlDebuggerResume, Session: testSession, Epoch: 1, TargetID: target}
}

// checkPauses fails t unless the model holds exactly the paused targets
// and the resumes given, and no resume reached a target that was not paused.
func checkPauses(t *testing.T, m *pauseModel, paused, resumes []string) {
	t.Helper()
	gotPaused, gotResumes, wrong, _ := m.state()
	if len(wrong) != 0 {
		t.Fatalf("Debugger.resume reached %v, which were not paused", wrong)
	}
	if !slices.Equal(gotPaused, paused) || !slices.Equal(gotResumes, resumes) {
		t.Fatalf("paused %v, resumed on %v; want paused %v, resumed on %v", gotPaused, gotResumes, paused, resumes)
	}
}

// The P1: a pause that the provisional target of a cross-site navigation
// reported is resumed on that target, named or as the only pause ("").
func TestProvisionalPauseIsResumedOnItsTarget(t *testing.T) {
	for _, name := range []string{"page-2", ""} {
		t.Run(fmt.Sprintf("target %q", name), func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{})
			m := newPauseModel(h.f)
			ctx, _ := timeoutCtx(t, 10*time.Second)
			h.f.Provisional("page-2")
			m.pause("page-2")
			syncPage(t, h.sess, "page-1")
			if _, err := h.s.Control(ctx, resumeOn(name)); err != nil {
				t.Fatalf("resume of the provisional target's pause: %v", err)
			}
			checkPauses(t, m, nil, []string{"page-2"})
			// The pause is answered: after the commit nothing is left to resume.
			h.f.Commit("page-2")
			syncPage(t, h.sess, "page-2")
			if _, err := h.s.Control(ctx, resumeOn("")); !errors.Is(err, ErrControlRefused) {
				t.Fatalf("second resume: err = %v, want ErrControlRefused", err)
			}
			if _, _, _, calls := m.state(); calls != 1 {
				t.Fatalf("%d Debugger.resume sent, want 1", calls)
			}
		})
	}
}

// A resume without a target answers the active target's pause first, then
// the pause of another target; a resume that names a target that is not
// paused is refused and never sent.
func TestUnnamedResumeTakesTheActiveTargetFirst(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	m.pause("page-1")
	syncPage(t, h.sess, "page-1")
	if _, err := h.s.Control(ctx, resumeOn("page-9")); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("resume of a target that is not paused: err = %v, want ErrControlRefused", err)
	}
	for i := range 2 {
		if _, err := h.s.Control(ctx, resumeOn("")); err != nil {
			t.Fatalf("resume %d: %v", i+1, err)
		}
	}
	if _, err := h.s.Control(ctx, resumeOn("")); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("third resume: err = %v, want ErrControlRefused", err)
	}
	checkPauses(t, m, nil, []string{"page-1", "page-2"})
	if _, _, _, calls := m.state(); calls != 2 {
		t.Fatalf("%d Debugger.resume sent, want 2 (refused resumes are never sent)", calls)
	}
}

// Design 2.10: the release barrier resumes every paused target, each on
// its own target.
func TestReleaseResumesEveryPausedTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	m.pause("page-1")
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// The barrier resumes the targets in no particular order.
	paused, resumes, wrong, _ := m.state()
	slices.Sort(resumes)
	if want := []string{"page-1", "page-2"}; len(paused) != 0 || len(wrong) != 0 || !slices.Equal(resumes, want) {
		t.Fatalf("paused %v, resumed on %v, resumes to targets that were not paused %v; want none paused, resumed on %v", paused, resumes, wrong, want)
	}
}

// A destroyed target's pause is dropped (the core-events rule for holds):
// WebKit destroyed its JavaScript with it, so neither a client nor the
// release barrier resumes it, and nothing is sent to another target.
func TestDestroyedTargetDropsItsPause(t *testing.T) {
	t.Run("abandoned provisional target", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		m := newPauseModel(h.f)
		ctx, _ := timeoutCtx(t, 10*time.Second)
		h.f.Provisional("page-2")
		m.pause("page-2")
		syncPage(t, h.sess, "page-1")
		h.f.DestroyTarget("page-2")
		m.gone("page-2")
		syncPage(t, h.sess, "page-1")
		if _, err := h.s.Control(ctx, resumeOn("")); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("resume after the abort: err = %v, want ErrControlRefused", err)
		}
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		checkPauses(t, m, nil, nil)
		if _, _, _, calls := m.state(); calls != 0 {
			t.Fatalf("%d Debugger.resume sent, want none", calls)
		}
	})
	t.Run("target replaced by a commit", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		m := newPauseModel(h.f)
		ctx, _ := timeoutCtx(t, 10*time.Second)
		m.pause("page-1")
		h.f.Provisional("page-2")
		syncPage(t, h.sess, "page-1")
		h.f.Commit("page-2")
		m.gone("page-1")
		syncPage(t, h.sess, "page-2")
		if _, err := h.s.Control(ctx, resumeOn("")); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("resume after the commit: err = %v, want ErrControlRefused", err)
		}
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		checkPauses(t, m, nil, nil)
		if _, _, _, calls := m.state(); calls != 0 {
			t.Fatalf("%d Debugger.resume sent, want none", calls)
		}
	})
}

// The release barrier and the commit of a paused provisional target, in
// both orders and with the commit while the barrier's resume is in flight:
// the pause is resumed exactly once, on the provisional target, which keeps
// its id at the commit; the replaced target's pause is not resumed.
func TestReleaseAndCommitOfAPausedProvisionalTarget(t *testing.T) {
	t.Run("commit then release", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		m := newPauseModel(h.f)
		ctx, _ := timeoutCtx(t, 10*time.Second)
		m.pause("page-1")
		h.f.Provisional("page-2")
		m.pause("page-2")
		syncPage(t, h.sess, "page-1")
		h.f.Commit("page-2")
		m.gone("page-1")
		syncPage(t, h.sess, "page-2")
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		checkPauses(t, m, nil, []string{"page-2"})
	})
	t.Run("release then commit", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		m := newPauseModel(h.f)
		ctx, _ := timeoutCtx(t, 10*time.Second)
		h.f.Provisional("page-2")
		m.pause("page-2")
		syncPage(t, h.sess, "page-1")
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		checkPauses(t, m, nil, []string{"page-2"})
		h.f.Commit("page-2")
		syncPage(t, h.sess, "page-2")
		h.s.SetLease(testSession, 2)
		if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlDebuggerResume, Session: testSession, Epoch: 2}); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("resume after the commit: err = %v, want ErrControlRefused", err)
		}
		if _, _, _, calls := m.state(); calls != 1 {
			t.Fatalf("%d Debugger.resume sent, want 1", calls)
		}
	})
	t.Run("commit while the resume is in flight", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		m := newPauseModel(h.f)
		ctx, _ := timeoutCtx(t, 10*time.Second)
		gate := make(chan struct{})
		m.gate = gate
		h.f.Provisional("page-2")
		m.pause("page-2")
		syncPage(t, h.sess, "page-1")
		released := make(chan error, 1)
		go func() { released <- h.s.Release(ctx, nil) }()
		waitUntil(t, "the barrier's resume", func() bool { _, _, _, calls := m.state(); return calls == 1 })
		h.f.Commit("page-2")
		waitUntil(t, "the commit", func() bool { return h.sess.ActiveTarget() == "page-2" })
		close(gate)
		if err := <-released; err != nil {
			t.Fatalf("Release: %v", err)
		}
		checkPauses(t, m, nil, []string{"page-2"})
		if _, _, _, calls := m.state(); calls != 1 {
			t.Fatalf("%d Debugger.resume sent, want 1", calls)
		}
	})
}

// faultyResume is the daemon's page connection with faults in the resumes
// that the scheduler sends: the next unsent resumes fail before they are
// written (webkit.ErrNotSent), and a held resume waits in flight until hold
// is closed, then fails unsent or is sent.
type faultyResume struct {
	*pageConn
	mu         sync.Mutex
	unsent     int // the next resumes that fail unsent
	injected   int // resumes that failed unsent so far
	hold       chan struct{}
	entered    chan struct{}
	holdUnsent bool
}

func (c *faultyResume) SendControl(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "Debugger.resume" {
		c.mu.Lock()
		hold, entered, holdUnsent := c.hold, c.entered, c.holdUnsent
		c.hold = nil
		unsent := hold == nil && c.unsent > 0
		if unsent {
			c.unsent--
		}
		c.mu.Unlock()
		if hold != nil {
			close(entered)
			<-hold
			unsent = holdUnsent
		}
		if unsent {
			c.mu.Lock()
			c.injected++
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: test: Debugger.resume was not written", webkit.ErrNotSent)
		}
	}
	return c.pageConn.SendControl(ctx, method, params)
}

func (c *faultyResume) injectedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.injected
}

// For any interleaving of pauses on the active and a provisional target,
// client resumes (naming the target or not), resumes that fail unsent,
// pauses that come back before the resume's reply, provisional targets
// that commit or are abandoned, and a final release barrier, through the
// daemon's page connection over a Session: no Debugger.resume reaches a
// target that is not paused, a client resume succeeds exactly when a live
// target is paused and its resume is written, and after the release no
// live target is paused. The release runs with a client resume in flight
// that ends unsent or sent, with unsent barrier resumes and with a pause
// that comes back during the release, with a pause during the release's
// cleanup, and with a held request whose continue fails. With
// maxBarrierContinues unsent resumes of one pause (the bound) the release
// fails and names every target that is still paused; a failed held
// request fails it too, joined with the paused targets; otherwise it
// succeeds.
//
// Oracle: pauseModel.
func TestEveryPauseIsResumedOnItsTargetProperty(t *testing.T) {
	var reach struct {
		provisionalResumes, unnamed, commits, aborts, refused, clientUnsent  int
		inflightUnsent, inflightSent, barrierUnsent, bound, repauseInRelease int
		cleanupPause, holdFailWithPause                                      int
	}
	prop := func(ops []uint8, rel uint8) bool {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		m := newPauseModel(f)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
			return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		}, webkit.SessionOptions{})
		if err != nil {
			t.Logf("dial: %v", err)
			return false
		}
		pc := newPageConn(sess, nil, func(fn func(context.Context)) bool { go fn(context.Background()); return true }, t.Logf)
		defer pc.close()
		s := NewScheduler(pc, SchedulerOptions{})
		defer s.Close()
		fc := &faultyResume{pageConn: pc}
		s.c = fc // the scheduler's sends; its subscriptions stay on pc
		s.SetLease(testSession, 1)

		active, provisional := "page-1", ""
		next := 0
		sync := func() { syncPage(t, sess, active) }
		// resume sends a client resume and checks its result: refused
		// when nothing is paused, ErrNotSent when the fault hit it, else
		// sent.
		resume := func(target string, want bool) (sent, ok bool) {
			before := fc.injectedCount()
			_, err := s.Control(ctx, resumeOn(target))
			if fc.injectedCount() != before {
				if !errors.Is(err, webkit.ErrNotSent) {
					t.Logf("resume of %q failed unsent: err = %v, want ErrNotSent", target, err)
					return false, false
				}
				reach.clientUnsent++
				return false, true
			}
			if want != (err == nil) {
				t.Logf("resume of %q (want sent %v): err = %v", target, want, err)
				return false, false
			}
			return err == nil, true
		}
		for _, op := range ops {
			paused, _, _, _ := m.state()
			switch op % 7 {
			case 0, 1: // a pause on the active or the provisional target
				target := active
				if op%2 == 1 && provisional != "" {
					target = provisional
				}
				if slices.Contains(paused, target) {
					continue // paused JavaScript cannot pause again
				}
				m.pause(target)
				sync()
			case 2: // a client resume naming a target
				target := active
				if op/7%2 == 1 && provisional != "" {
					target = provisional
				}
				sent, ok := resume(target, slices.Contains(paused, target))
				if !ok {
					return false
				}
				if sent && target == provisional {
					reach.provisionalResumes++
				}
				sync() // a pause that came back before the reply
			case 3: // a client resume without a target
				sent, ok := resume("", len(paused) > 0)
				if !ok {
					return false
				}
				switch {
				case sent:
					reach.unnamed++
				case len(paused) == 0:
					reach.refused++
				}
				sync()
			case 4: // a provisional target appears, commits, or is abandoned
				switch {
				case provisional == "":
					next++
					provisional = fmt.Sprintf("page-p%d", next)
					f.Provisional(provisional)
				case op/7%2 == 0:
					f.Commit(provisional)
					m.gone(active)
					active, provisional = provisional, ""
					reach.commits++
				default:
					f.DestroyTarget(provisional)
					m.gone(provisional)
					provisional = ""
					reach.aborts++
				}
				sync()
			case 5: // the next resumes fail unsent
				fc.mu.Lock()
				fc.unsent = 1 + int(op/7%2)
				fc.mu.Unlock()
			case 6: // the next resume's target pauses again before the reply
				m.mu.Lock()
				m.repause = 1
				m.mu.Unlock()
			}
			if _, _, wrong, _ := m.state(); len(wrong) != 0 {
				t.Logf("Debugger.resume reached %v, which were not paused", wrong)
				return false
			}
		}

		// The release: rel bit 0 holds a client resume in flight (bit 1:
		// it ends unsent), bits 2-3 are the barrier's unsent resumes (3 is
		// the bound), bit 4 makes a resumed target pause again, bit 5 pauses
		// the active target during the cleanup, and bit 6 holds a request
		// whose continue fails.
		holdFail := rel&64 != 0
		if holdFail {
			f.Handle("Network.interceptContinue", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32602, Message: "Invalid stage"}
			})
			f.Intercept(active, "rh", "request")
			sync()
			if got := s.PendingRequests(); !slices.Contains(got, "rh") {
				t.Logf("held request rh not recorded: %v", got)
				return false
			}
		}
		var cleanup func(context.Context, webkit.Caller) error
		if rel&32 != 0 {
			cleanup = func(context.Context, webkit.Caller) error {
				if now, _, _, _ := m.state(); !slices.Contains(now, active) {
					m.pause(active)
					sync()
					reach.cleanupPause++
				}
				return nil
			}
		}
		paused, _, _, _ := m.state()
		barrierUnsent := int(rel>>2) & 3
		fc.mu.Lock()
		fc.unsent = barrierUnsent
		fc.mu.Unlock()
		if rel&16 != 0 {
			m.mu.Lock()
			m.repause = 1
			m.mu.Unlock()
			reach.repauseInRelease++
		}
		var client chan error
		var hold chan struct{}
		if rel&1 != 0 && len(paused) > 0 {
			hold = make(chan struct{})
			entered := make(chan struct{})
			fc.mu.Lock()
			fc.hold, fc.entered, fc.holdUnsent = hold, entered, rel&2 != 0
			fc.mu.Unlock()
			client = make(chan error, 1)
			go func() { _, err := s.Control(ctx, resumeOn(paused[0])); client <- err }()
			<-entered
			if rel&2 != 0 {
				reach.inflightUnsent++
			} else {
				reach.inflightSent++
			}
		}
		released := make(chan error, 1)
		go func() { released <- s.Release(ctx, cleanup) }()
		if hold != nil {
			time.Sleep(time.Millisecond) // the release usually starts first; either order is valid
			close(hold)
			if err := <-client; (rel&2 != 0) != errors.Is(err, webkit.ErrNotSent) {
				t.Logf("held client resume (unsent %v): err = %v", rel&2 != 0, err)
				return false
			}
		}
		err = <-released
		left, _, wrong, _ := m.state()
		if len(wrong) != 0 {
			t.Logf("Debugger.resume reached %v, which were not paused", wrong)
			return false
		}
		switch {
		case holdFail && (err == nil || !strings.Contains(err.Error(), `intercepted request "rh"`)):
			t.Logf("Release with a failing held request: err = %v, want the hold's failure", err)
			return false
		case !holdFail && err != nil && len(left) == 0:
			t.Logf("Release: %v with nothing paused (rel %#x)", err, rel)
			return false
		}
		if len(left) != 0 {
			if barrierUnsent < maxBarrierContinues || err == nil {
				t.Logf("Release returned %v with %v paused (barrier unsent %d, rel %#x)", err, left, barrierUnsent, rel)
				return false
			}
			for _, target := range left {
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", target)) {
					t.Logf("Release error %q does not name the paused target %s", err, target)
					return false
				}
			}
			reach.bound++
			if holdFail {
				reach.holdFailWithPause++
			}
			return true
		}
		if barrierUnsent > 0 && barrierUnsent < maxBarrierContinues && len(paused) > 0 {
			reach.barrierUnsent++
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
	if reach.provisionalResumes == 0 || reach.unnamed == 0 || reach.commits == 0 || reach.aborts == 0 || reach.refused == 0 ||
		reach.clientUnsent == 0 || reach.inflightUnsent == 0 || reach.inflightSent == 0 || reach.barrierUnsent == 0 ||
		reach.bound == 0 || reach.repauseInRelease == 0 || reach.cleanupPause == 0 || reach.holdFailWithPause == 0 {
		t.Fatalf("generator reach %+v: want each case", reach)
	}
}
