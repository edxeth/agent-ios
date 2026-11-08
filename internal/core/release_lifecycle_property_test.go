package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// The release and attach lifecycle property (core-events round 3,
// release_lifecycle.go). A generated plan runs its steps concurrently, each
// from its own start offset, against one fake page: changes of the current
// and of the previous session, takeover, plain attach (reopen, or the
// background recovery of a stalled page), release, lease break, a lost
// connection, a stalled connection, delayed replies of setup methods, and a
// change that holds the change lock. Every command has a generated
// deadline. The invariants (from the core-events round 3 brief):
//
//   - no connection is installed that carries the state of a lease whose
//     release began before the install;
//   - when a release ends, and from then on until the session gets a lease
//     again, the model holds no owner of the released session;
//   - no lease is granted and no connection installed while a release runs;
//   - every command returns by its deadline (with slack for the loaded
//     host), and an error caused by the deadline is a timeout (exit 124).
//
// Core-events round 4 adds the pending-cleanup transaction: a session
// granted again after a takeover kept its state (touch-first), provisional
// targets that commit while their setup runs (swap), held requests
// (intercept), cleanup and setup sends that WebKit refuses for a while
// (refuse), and a check of the held requests (holds). The fake keeps a
// model of each backend (connection and target): its extra headers, with
// WebKit's merge and Network.disable clearing them, and its bootstrap
// script. The invariants (from the core-events round 4 brief):
//
//   - after every release, the released state is gone from the installed
//     connection's active backend, or a cleanup is pending: the Network
//     agent is unknown (d.unknown), or the target's setup still runs or
//     failed (checked at every lifecycle event and at the end); while the
//     session has committed a target and the model has not processed that
//     commit yet, a Network cleanup that the model holds for that target
//     counts too (networkCleanupOwed, core-events round 8);
//   - no held request is continued by a reset without being recorded as
//     reset: every request the scheduler lists is held by the backend
//     (checked twice, 200 ms apart, so a reply in flight does not count);
//   - a gated send (the DOM.getDocument of a use) reaches a target only
//     after a setup sent its whole bootstrap script there; and after the
//     refusals end, the next command applies a failed setup again;
//   - the round-3 invariants above.
//
// At the end of a run the refusals and delays end, a lease break and one
// more command of a new session run, and the backend must then hold
// exactly the headers of the model's owners.
//
// Each session's state is the extra header X-Sess-<session> with the lease
// epoch as its value, so the state a connection's setup sent is read from
// the fake's records of that connection.

const (
	stepUse        = iota // Begin, a header change, End, by the current session
	stepUsePrev           // the same by the previous session: a change in flight across its release
	stepTakeover          // Touch --takeover by a new session
	stepTouch             // Touch by the current session: reopen, or wait for the recovery
	stepRelease           // Release of the current session
	stepBreak             // lease break
	stepLost              // another inspector takes the page
	stepStall             // the newest connection stops answering
	stepDelay             // delayed replies of a setup method
	stepContend           // a change that holds the change lock
	stepTouchFirst        // Touch by s1, whose state a takeover kept
	stepSwap              // a provisional target that commits Param ms later
	stepIntercept         // the active target holds a new request
	stepRefuse            // WebKit refuses a cleanup or setup method for Param ms
	stepHolds             // check the held requests
	numLifecycleSteps
)

var stepNames = [...]string{"use", "use-prev", "takeover", "touch", "release", "break", "lost", "stall", "delay", "contend", "touch-first", "swap", "intercept", "refuse", "holds"}

type lifecycleStep struct {
	Kind    int
	StartMS int // offset from the start of the run
	DeadMS  int // deadline of the step's command
	Param   int // delay or hold in ms; the delayed method
}

func (s lifecycleStep) String() string {
	return fmt.Sprintf("%s@%dms(deadline %dms, param %d)", stepNames[s.Kind], s.StartMS, s.DeadMS, s.Param)
}

type lifecyclePlan struct{ Steps []lifecycleStep }

// lifecycleDeadlines are the generated command deadlines: shorter than a
// setup with delayed replies, about one, and longer than all of them.
var lifecycleDeadlines = []int{60, 250, 1000, 3000}

func (lifecyclePlan) Generate(r *rand.Rand, _ int) reflect.Value {
	var p lifecyclePlan
	for range 3 + r.Intn(8) {
		p.Steps = append(p.Steps, lifecycleStep{
			Kind:    r.Intn(numLifecycleSteps),
			StartMS: r.Intn(900),
			DeadMS:  lifecycleDeadlines[r.Intn(len(lifecycleDeadlines))],
			Param:   r.Intn(600),
		})
	}
	return reflect.ValueOf(p)
}

// lifecycleSlack is how late after its deadline a command may return on a
// loaded host (race detector, shared CI machine).
const lifecycleSlack = 2 * time.Second

var delayedSetupMethods = []string{"Network.setExtraHTTPHeaders", "Network.enable", "Page.enable"}

// refusedMethods are the methods that a refuse step makes WebKit refuse
// with a protocol error: cleanup sends (Network.disable,
// Network.setExtraHTTPHeaders) and a setup send (Page.setBootstrapScript).
var refusedMethods = []string{"Network.disable", "Network.setExtraHTTPHeaders", "Page.setBootstrapScript"}

// runtimeMarker is in the runtime part of every setup's bootstrap script.
const runtimeMarker = "__iwdp"

// lifecycleRun is the state of one run of a plan.
type lifecycleRun struct {
	t   *testing.T
	r   *rig
	pg  *Page
	mu  sync.Mutex
	n   int             // the current session is "s<n>"
	bad []string        // violations
	rel map[string]bool // "<session>|<epoch>" of each lease whose release began
	// ended holds the sessions whose release ended and that got no lease
	// since.
	ended map[string]bool
	// installed is the fake's connection id of the installed connection.
	installed int
	seq       int  // names of provisional targets and held requests
	disturbed bool // a stall or lost step ran: the final command may find no usable connection

	// The backend model, kept by the fake's handlers (installBackend),
	// by "<conn>|<target>"; fmu guards it and refusing (method -> end).
	fmu      sync.Mutex
	hdr      map[string]map[string]string
	boot     map[string]string
	refusing map[string]time.Time
}

func backendKey(conn int, target string) string { return strconv.Itoa(conn) + "|" + target }

func (lr *lifecycleRun) refused(method string) *fakewebkit.Error {
	lr.fmu.Lock()
	defer lr.fmu.Unlock()
	if time.Now().Before(lr.refusing[method]) {
		return &fakewebkit.Error{Code: -32000, Message: "refused by the lifecycle property"}
	}
	return nil
}

// installBackend makes the fake keep the backend model and refuse the
// refused methods.
func (lr *lifecycleRun) installBackend() {
	f := lr.r.f
	f.Handle("Network.setExtraHTTPHeaders", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if e := lr.refused(c.Method); e != nil {
			return nil, e
		}
		var p struct {
			Headers map[string]string `json:"headers"`
		}
		_ = json.Unmarshal(c.Params, &p)
		lr.fmu.Lock()
		defer lr.fmu.Unlock()
		k := backendKey(c.Conn, c.Target)
		if lr.hdr[k] == nil {
			lr.hdr[k] = map[string]string{}
		}
		for n, v := range p.Headers { // WebKit merges the map
			lr.hdr[k][n] = v
		}
		return nil, nil
	})
	f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if e := lr.refused(c.Method); e != nil {
			return nil, e
		}
		lr.fmu.Lock()
		delete(lr.hdr, backendKey(c.Conn, c.Target))
		lr.fmu.Unlock()
		f.ContinueHolds(c.Target)
		return nil, nil
	})
	f.Handle("Page.setBootstrapScript", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if e := lr.refused(c.Method); e != nil {
			return nil, e
		}
		var p struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(c.Params, &p)
		lr.fmu.Lock()
		defer lr.fmu.Unlock()
		lr.boot[backendKey(c.Conn, c.Target)] = p.Source
		return nil, nil
	})
	f.Handle("DOM.getDocument", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		lr.fmu.Lock()
		script := lr.boot[backendKey(c.Conn, c.Target)]
		lr.fmu.Unlock()
		if !strings.Contains(script, runtimeMarker) {
			lr.violation("a gated send reached page target %s of connection %d, which has no complete setup (bootstrap script %.40q)", c.Target, c.Conn, script)
		}
		return map[string]any{"root": map[string]any{"nodeId": 1}}, nil
	})
}

// newestSetUpConn is the newest fake connection that got Page.enable: the
// one being installed, because attaches run one at a time.
func (lr *lifecycleRun) newestSetUpConn() int {
	conn := 0
	for _, rec := range lr.r.f.ReceivedMethod("Page.enable") {
		conn = max(conn, rec.Conn)
	}
	return conn
}

// cleanupPending reports whether pg has a pending cleanup for target, the
// connection's active target: a Network cleanup the model owes target
// (networkCleanupOwed), or an active target whose setup still runs or
// failed.
func cleanupPending(pg *Page, target string) bool {
	if networkCleanupOwed(pg.PS.State, target) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	return pg.conn.ActiveSetup(ctx) != nil
}

// networkCleanupOwed reports, in one critical section, whether the Network
// agent is unknown, or whether the session made target its active target
// before the model processed that commit while target's record holds a
// Network cleanup: one that failed there (unclean) or one still on its
// way (in flight or queued). The session switches its active target in
// its read loop and then runs its subscribers, among them the model's
// commit (followTargets, commitTarget), which turns exactly these into an
// unknown Network agent (markUnsentCleanupLocked). Every production path
// reads the model's active target (beginChange), so only a check that
// reads the session's active target sees this step.
func networkCleanupOwed(d *DesiredState, target string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unknown["Network"] != 0 {
		return true
	}
	ts := d.syncs[target]
	if target == d.active || ts == nil {
		return false
	}
	if ts.unclean["Network"] {
		return true
	}
	return slices.ContainsFunc(append([]*queued{ts.sending}, ts.queue...), func(q *queued) bool {
		return q != nil && q.cleanup == "Network"
	})
}

// checkBackendLocked: no header of a session whose release ended in the
// installed connection's active backend, unless a cleanup is pending.
func (lr *lifecycleRun) checkBackendLocked(pg *Page, when string) {
	if pg.conn.State() != ConnOK {
		return // design 2.4: no reconnect only to clean up; a new connection's setup resets
	}
	target := pg.conn.ActiveTarget()
	lr.fmu.Lock()
	hdr := maps.Clone(lr.hdr[backendKey(lr.installed, target)])
	lr.fmu.Unlock()
	for s := range lr.ended {
		if v, ok := hdr["X-Sess-"+s]; ok && !cleanupPending(pg, target) {
			lr.bad = append(lr.bad, fmt.Sprintf("at %s: the release of %s ended, its header (epoch %s) is in the backend of target %s of connection %d, and no cleanup is pending", when, s, v, target, lr.installed))
		}
	}
}

// checkHolds: every request the scheduler lists is held by the backend.
// A pair counts only when it is found in two checks 200 ms apart, so a
// reply on its way (a Network.disable that the fake ran) is no violation.
func (lr *lifecycleRun) checkHolds(when string) {
	stray := func() map[HeldRequest]bool {
		out := map[HeldRequest]bool{}
		if lr.pg.ConnState() != ConnOK {
			return out
		}
		for _, h := range lr.pg.PS.Sched.PendingHolds() {
			if !slices.Contains(lr.r.f.HeldRequests(h.Target), h.RequestID) {
				out[h] = true
			}
		}
		return out
	}
	first := stray()
	if len(first) == 0 {
		return
	}
	time.Sleep(200 * time.Millisecond)
	for h := range stray() {
		if first[h] {
			lr.violation("at %s: the scheduler lists held request %s of target %s, which the backend no longer holds: a reset continued it without recording it", when, h.RequestID, h.Target)
		}
	}
}

func (lr *lifecycleRun) next(prefix string) string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.seq++
	return prefix + strconv.Itoa(lr.seq)
}

func (lr *lifecycleRun) violation(format string, a ...any) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.bad = append(lr.bad, fmt.Sprintf(format, a...))
}

func (lr *lifecycleRun) session(delta int) string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.n += delta
	return "s" + strconv.Itoa(lr.n)
}

func (lr *lifecycleRun) prevSession() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return "s" + strconv.Itoa(max(lr.n-1, 1))
}

// ownersOf lists the model's owners of session.
func ownersOf(d *DesiredState, session string) []Owner {
	var out []Owner
	for _, o := range d.AllOwners() {
		if strings.HasSuffix(string(o), ":"+session) {
			out = append(out, o)
		}
	}
	return out
}

// hook checks the invariants at each lifecycle event; it runs under pg.mu.
func (lr *lifecycleRun) hook(ev lifecycleEvent) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	switch ev.kind {
	case "grant":
		if ev.pg.releasing {
			lr.bad = append(lr.bad, fmt.Sprintf("lease granted to %s while a release runs", ev.l.session))
		}
		delete(lr.ended, ev.l.session)
	case "install":
		if ev.pg.releasing {
			lr.bad = append(lr.bad, "connection installed while a release runs")
		}
		lr.checkInstalledLocked()
		lr.installed = lr.newestSetUpConn()
	case "release-begin":
		lr.rel[ev.l.session+"|"+strconv.FormatUint(ev.l.epoch, 10)] = true
	case "release-end":
		lr.ended[ev.l.session] = true
	}
	lr.checkEndedLocked(ev.pg.PS.State, ev.kind)
	lr.checkBackendLocked(ev.pg, ev.kind)
}

// checkEndedLocked: no owner of a session whose release ended.
func (lr *lifecycleRun) checkEndedLocked(d *DesiredState, when string) {
	for s := range lr.ended {
		if o := ownersOf(d, s); len(o) > 0 {
			lr.bad = append(lr.bad, fmt.Sprintf("at %s: the release of %s ended, and the model holds its owners %v", when, s, o))
		}
	}
}

// checkInstalledLocked reads the headers that the newest set-up connection
// (the one being installed: attaches run one at a time) got, and reports a
// header of a lease whose release began.
func (lr *lifecycleRun) checkInstalledLocked() {
	recs := lr.r.f.Received()
	conn := 0
	for _, rec := range recs {
		if rec.Method == "Page.enable" && rec.Conn > conn {
			conn = rec.Conn
		}
	}
	hdr := map[string]string{}
	for _, rec := range recs {
		if rec.Conn != conn {
			continue
		}
		switch rec.Method {
		case "Network.disable":
			hdr = map[string]string{}
		case "Network.setExtraHTTPHeaders":
			var p struct {
				Headers map[string]string `json:"headers"`
			}
			_ = json.Unmarshal(rec.Params, &p)
			for k, v := range p.Headers {
				hdr[k] = v
			}
		}
	}
	for k, v := range hdr {
		if s, ok := strings.CutPrefix(k, "X-Sess-"); ok && lr.rel[s+"|"+v] {
			lr.bad = append(lr.bad, fmt.Sprintf("installed connection %d carries the state of lease %s (epoch %s), whose release began before the install", conn, s, v))
		}
	}
}

// timed runs a command of step st with its deadline and checks how it ended.
func (lr *lifecycleRun) timed(st lifecycleStep, what string, fn func(ctx context.Context) error) error {
	deadline := time.Duration(st.DeadMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	if took := time.Since(start); took > deadline+lifecycleSlack {
		lr.violation("%s (%s) returned after %v, past its deadline %v (err %v)", what, st, took.Round(time.Millisecond), deadline, err)
	}
	// Ended by the command's own deadline: the deadline passed, and the
	// error names a deadline. Other budgets (the liveness probe's, a call
	// budget) end with their own codes, for example a stall (exit 4).
	if err != nil && ctx.Err() != nil && strings.Contains(err.Error(), "deadline exceeded") && !errors.Is(err, context.DeadlineExceeded) && exitCode(err) != CodeTimeout {
		lr.violation("%s (%s) ended by its deadline without exit 124: %v", what, st, err)
	}
	return err
}

func (lr *lifecycleRun) use(st lifecycleStep, s string) {
	_ = lr.timed(st, "change of "+s, func(ctx context.Context) error {
		mo, err := lr.r.m.Begin(ctx, lr.r.req, AcquireOptions{Session: s, Reservation: "agent"}, "use", ClassExclusive)
		if err != nil {
			return err
		}
		defer lr.r.m.End(mo.ID)
		p, _ := json.Marshal(map[string]any{"headers": map[string]string{"X-Sess-" + s: strconv.FormatUint(mo.Epoch, 10)}})
		if _, err = lr.r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", p); err != nil {
			return err
		}
		_, err = lr.r.m.Raw(ctx, mo.ID, "DOM.getDocument", nil) // a gated send
		return err
	})
}

func (lr *lifecycleRun) step(st lifecycleStep) {
	m, key := lr.r.m, lr.pg.Key
	switch st.Kind {
	case stepUse:
		lr.use(st, lr.session(0))
	case stepUsePrev:
		lr.use(st, lr.prevSession())
	case stepTakeover:
		s := lr.session(1)
		_ = lr.timed(st, "takeover by "+s, func(ctx context.Context) error {
			_, _, err := m.Touch(ctx, lr.r.req, AcquireOptions{Session: s, Reservation: "agent", Takeover: true})
			return err
		})
	case stepTouch:
		s := lr.session(0)
		_ = lr.timed(st, "attach of "+s, func(ctx context.Context) error {
			_, _, err := m.Touch(ctx, lr.r.req, AcquireOptions{Session: s, Reservation: "agent"})
			return err
		})
	case stepRelease:
		s := lr.session(0)
		lr.session(1)
		_ = lr.timed(st, "release of "+s, func(ctx context.Context) error { return m.Release(ctx, s, key) })
	case stepBreak:
		lr.session(1)
		_ = lr.timed(st, "lease break", func(ctx context.Context) error { return m.Break(ctx, key) })
	case stepLost:
		lr.mu.Lock()
		lr.disturbed = true
		lr.mu.Unlock()
		stealPage(lr.t, lr.r.f)
	case stepStall:
		lr.mu.Lock()
		lr.disturbed = true
		lr.mu.Unlock()
		lr.r.f.StallConnection(lr.r.f.ConnectionCount())
	case stepDelay:
		lr.r.f.DelayReplies(delayedSetupMethods[st.Param%len(delayedSetupMethods)], time.Duration(st.Param)*time.Millisecond)
	case stepContend:
		_ = lr.timed(st, "contended change", func(ctx context.Context) error {
			_, end, err := lr.pg.PS.State.beginChange(ctx, nil)
			if err != nil {
				return err
			}
			time.Sleep(time.Duration(st.Param) * time.Millisecond)
			return end(nil)
		})
	case stepTouchFirst:
		_ = lr.timed(st, "attach of s1", func(ctx context.Context) error {
			_, _, err := m.Touch(ctx, lr.r.req, AcquireOptions{Session: "s1", Reservation: "agent"})
			return err
		})
	case stepSwap:
		id := lr.next("p")
		<-lr.r.f.Provisional(id)
		time.Sleep(time.Duration(st.Param) * time.Millisecond)
		lr.r.f.Commit(id)
	case stepIntercept:
		lr.r.f.Intercept(lr.r.f.ActiveTarget(), lr.next("r"), "request")
	case stepRefuse:
		method := refusedMethods[st.Param%len(refusedMethods)]
		lr.fmu.Lock()
		lr.refusing[method] = time.Now().Add(time.Duration(st.Param) * time.Millisecond)
		lr.fmu.Unlock()
	case stepHolds:
		lr.checkHolds(st.String())
	}
}

// finish ends the refusals and delays and runs one more command of a new
// session after a lease break: a failed setup and every pending cleanup
// must be applied by then, and the backend must hold exactly the headers
// of the model's owners.
func (lr *lifecycleRun) finish() {
	lr.fmu.Lock()
	clear(lr.refusing)
	lr.fmu.Unlock()
	for _, m := range delayedSetupMethods {
		lr.r.f.DelayReplies(m, 0)
	}
	lr.checkHolds("the end of the run")
	if lr.pg.ConnState() != ConnOK {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = lr.r.m.Break(ctx, lr.pg.Key)
	mo, err := lr.r.m.Begin(ctx, lr.r.req, AcquireOptions{Session: "final", Reservation: "agent"}, "final", ClassExclusive)
	lr.mu.Lock()
	disturbed := lr.disturbed
	lr.mu.Unlock()
	if errors.As(err, new(*webkit.SetupError)) && !disturbed {
		lr.violation("after the refusals ended, the next command still fails with the setup failure: %v", err)
		return
	}
	if err == nil {
		p, _ := json.Marshal(map[string]any{"headers": map[string]string{"X-Final": "1"}})
		_, err = lr.r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", p)
		if err == nil {
			_, err = lr.r.m.Raw(ctx, mo.ID, "DOM.getDocument", nil)
		}
		lr.r.m.End(mo.ID)
	}
	switch {
	case err != nil && disturbed:
		lr.t.Logf("final command failed on a page that a stall or lost step disturbed: %v", err)
		return
	case err != nil:
		lr.violation("the final command failed after the refusals ended: %v", err)
		return
	}
	lr.pg.mu.Lock()
	installed, target := lr.installed, lr.pg.conn.ActiveTarget()
	lr.pg.mu.Unlock()
	want := lr.pg.PS.State.EffectiveHeaders()
	lr.fmu.Lock()
	got := maps.Clone(lr.hdr[backendKey(installed, target)])
	lr.fmu.Unlock()
	for n, v := range got {
		if want[n] != v {
			lr.violation("at the end: the backend of target %s of connection %d has header %s: %s, and the model's owners have %v", target, installed, n, v, want)
		}
	}
}

// runLifecyclePlan runs p on a new rig and returns the violations.
func runLifecyclePlan(t *testing.T, p lifecyclePlan) []string {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	lr := &lifecycleRun{
		t: t, r: r, n: 1, rel: map[string]bool{}, ended: map[string]bool{},
		hdr: map[string]map[string]string{}, boot: map[string]string{}, refusing: map[string]time.Time{},
	}
	lr.installBackend()
	watchLifecycle(r.m, lr.hook)
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s1", Reservation: "agent"}, "setup", ClassExclusive)
	if err != nil {
		t.Fatalf("setup Begin: %v", err)
	}
	lr.pg = mo.Page
	p0, _ := json.Marshal(map[string]any{"headers": map[string]string{"X-Sess-s1": strconv.FormatUint(mo.Epoch, 10)}})
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", p0); err != nil {
		t.Fatalf("setup header: %v", err)
	}
	r.m.End(mo.ID)
	lr.pg.mu.Lock()
	lr.installed = lr.newestSetUpConn()
	lr.pg.mu.Unlock()

	var wg sync.WaitGroup
	start := time.Now()
	for _, st := range p.Steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Until(start.Add(time.Duration(st.StartMS) * time.Millisecond)))
			lr.step(st)
		}()
	}
	wg.Wait()
	// Background work (recovery, the reset of unknown agents) ends within
	// its budgets; then the model must still hold no released owner.
	eventually(t, 15*time.Second, "quiescence", func() bool {
		lr.pg.mu.Lock()
		defer lr.pg.mu.Unlock()
		return !lr.pg.recovering && !lr.pg.releasing
	})
	lr.pg.mu.Lock()
	lr.mu.Lock()
	lr.checkEndedLocked(lr.pg.PS.State, "the end of the run")
	lr.checkBackendLocked(lr.pg, "the end of the run")
	lr.mu.Unlock()
	lr.pg.mu.Unlock()
	lr.finish()
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.bad
}

// reviewerPlans are the core-events reviewer's round-2 reproductions as
// plans of this property (/home/devkit/.pi/artifacts/reviewer/core-events-r2-repro/).
var reviewerPlans = map[string]lifecyclePlan{
	// A release starts and ends while a takeover's setup waits for its
	// replies; the takeover's snapshot has s1's header.
	"release completes during connect": {Steps: []lifecycleStep{
		{Kind: stepLost, StartMS: 0},
		{Kind: stepDelay, StartMS: 0, Param: 700}, // 700 % 3 = 1: Network.enable
		{Kind: stepTakeover, StartMS: 150, DeadMS: 3000},
		{Kind: stepBreak, StartMS: 450, DeadMS: 3000},
	}},
	// A change holds the change lock past the deadline of the release of
	// a lost page.
	"lost release at the change-lock deadline": {Steps: []lifecycleStep{
		{Kind: stepLost, StartMS: 0},
		{Kind: stepContend, StartMS: 100, DeadMS: 3000, Param: 500},
		{Kind: stepBreak, StartMS: 250, DeadMS: 60},
	}},
	// Round 4, finding 1: the background reset of s2's release sends s1's
	// header (a takeover kept it; its reply is delayed) while s1, granted
	// again, is released without the change lock.
	"reset overlapping a release": {Steps: []lifecycleStep{
		{Kind: stepLost, StartMS: 0},
		{Kind: stepTakeover, StartMS: 100, DeadMS: 3000},
		{Kind: stepUse, StartMS: 300, DeadMS: 3000},
		{Kind: stepContend, StartMS: 500, DeadMS: 3000, Param: 400},
		{Kind: stepBreak, StartMS: 550, DeadMS: 60},
		{Kind: stepTouchFirst, StartMS: 650, DeadMS: 3000},
		{Kind: stepDelay, StartMS: 700, Param: 600}, // 600 % 3 = 0: Network.setExtraHTTPHeaders
		{Kind: stepBreak, StartMS: 1000, DeadMS: 60},
	}},
	// Round 4, finding 2: a release during a provisional setup (its header
	// reply is delayed) makes the setup run again after the commit, while
	// the committed target holds a request.
	"setup retry after the commit with a held request": {Steps: []lifecycleStep{
		{Kind: stepDelay, StartMS: 0, Param: 600},
		{Kind: stepSwap, StartMS: 100, Param: 200},
		{Kind: stepContend, StartMS: 250, DeadMS: 3000, Param: 1500},
		{Kind: stepBreak, StartMS: 400, DeadMS: 60},
		{Kind: stepIntercept, StartMS: 500},
		{Kind: stepHolds, StartMS: 1000},
	}},
	// Round 4, finding 3: WebKit refuses the release's Network.disable.
	"refused cleanup send": {Steps: []lifecycleStep{
		{Kind: stepRefuse, StartMS: 0, Param: 300}, // 300 % 3 = 0: Network.disable
		{Kind: stepBreak, StartMS: 100, DeadMS: 3000},
	}},
	// Round 4, finding 4: WebKit refuses a provisional target's bootstrap
	// script for 500 ms; the target commits with a failed setup.
	"refused provisional setup": {Steps: []lifecycleStep{
		{Kind: stepRefuse, StartMS: 0, Param: 500}, // 500 % 3 = 2: Page.setBootstrapScript
		{Kind: stepSwap, StartMS: 50, Param: 100},
		{Kind: stepUse, StartMS: 300, DeadMS: 3000},
		{Kind: stepUse, StartMS: 700, DeadMS: 3000},
	}},
	// Round 4, part 2 (generated plan #22 of the round-4 full run): the
	// release's cleanup goes to a provisional target through a stalled
	// connection and fails; the target then commits with s1's header.
	"failed cleanup to a provisional target": {Steps: []lifecycleStep{
		{Kind: stepStall, StartMS: 198, DeadMS: 250, Param: 446},
		{Kind: stepUsePrev, StartMS: 363, DeadMS: 1000, Param: 493},
		{Kind: stepSwap, StartMS: 40, DeadMS: 3000, Param: 278},
		{Kind: stepStall, StartMS: 368, DeadMS: 60, Param: 207},
		{Kind: stepRelease, StartMS: 277, DeadMS: 250, Param: 349},
		{Kind: stepStall, StartMS: 887, DeadMS: 60, Param: 401},
		{Kind: stepBreak, StartMS: 528, DeadMS: 60, Param: 466},
	}},
	// Round 4, part 2 (generated plan #15 of the part-2 core run): a lock
	// holder makes the release leave Network unknown; its background reset
	// sends Network.disable to the old target on a stalled connection, a
	// commit cuts it off (ErrTargetSwapped), and the reset must not count
	// that as done while its forwarded send to the committed target is
	// still unanswered.
	"cleanup cut off by a commit": {Steps: []lifecycleStep{
		{Kind: stepSwap, StartMS: 63, DeadMS: 1000, Param: 487},
		{Kind: stepBreak, StartMS: 182, DeadMS: 250, Param: 365},
		{Kind: stepContend, StartMS: 31, DeadMS: 60, Param: 485},
		{Kind: stepIntercept, StartMS: 759, DeadMS: 60, Param: 374},
		{Kind: stepBreak, StartMS: 699, DeadMS: 1000, Param: 340},
		{Kind: stepTakeover, StartMS: 173, DeadMS: 60, Param: 409},
		{Kind: stepStall, StartMS: 400, DeadMS: 60, Param: 58},
		{Kind: stepRelease, StartMS: 657, DeadMS: 1000, Param: 347},
		{Kind: stepUse, StartMS: 653, DeadMS: 60, Param: 548},
	}},
	// Round 5: the release's continue of a held request waits on a
	// connection that the liveness probe declares stalled about 1 s before
	// the release deadline; the release must end then, with the stall
	// classification (exit 4), not at its deadline.
	"stall declared before the release deadline": {Steps: []lifecycleStep{
		{Kind: stepRelease, StartMS: 695, DeadMS: 3000, Param: 158},
		{Kind: stepUse, StartMS: 762, DeadMS: 60, Param: 91},
		{Kind: stepIntercept, StartMS: 202, DeadMS: 3000, Param: 83},
		{Kind: stepStall, StartMS: 21, DeadMS: 1000, Param: 549},
		{Kind: stepContend, StartMS: 5, DeadMS: 1000, Param: 372},
		{Kind: stepUsePrev, StartMS: 138, DeadMS: 3000, Param: 98},
		{Kind: stepUse, StartMS: 832, DeadMS: 60, Param: 415},
	}},
	// Core-events round 8 (generated plan #11 of the core-notsent run):
	// the provisional target p1 gets s1's header from its setup; the
	// release's Network.disable is refused on the active target, so
	// Network is unknown; the next change resets the active target, which
	// ends that mark, and queues the reset for p1, but its delayed header
	// reply holds it past its deadline, so nobody sends the queue. p1
	// commits with the reset still queued: its cleanup must be pending.
	"commit with a queued cleanup": {Steps: []lifecycleStep{
		{Kind: stepDelay, StartMS: 393, DeadMS: 60, Param: 501},
		{Kind: stepTouchFirst, StartMS: 359, DeadMS: 60, Param: 263},
		{Kind: stepBreak, StartMS: 522, DeadMS: 60, Param: 563},
		{Kind: stepUsePrev, StartMS: 307, DeadMS: 1000, Param: 121},
		{Kind: stepRefuse, StartMS: 481, DeadMS: 3000, Param: 147},
		{Kind: stepSwap, StartMS: 318, DeadMS: 250, Param: 539},
		{Kind: stepHolds, StartMS: 69, DeadMS: 60, Param: 513},
		{Kind: stepUse, StartMS: 797, DeadMS: 60, Param: 474},
		{Kind: stepTakeover, StartMS: 755, DeadMS: 60, Param: 31},
	}},
}

func lifecycleCount(t *testing.T) int {
	if s := os.Getenv("IWDP_LIFECYCLE_COUNT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("IWDP_LIFECYCLE_COUNT=%q: want a positive integer", s)
		}
		return n
	}
	return 25
}

// lifecycleSeed is the seed of the generated plans: IWDP_LIFECYCLE_SEED,
// or the time. It is logged, so a failing run can be repeated.
func lifecycleSeed(t *testing.T) int64 {
	seed := time.Now().UnixNano()
	if s := os.Getenv("IWDP_LIFECYCLE_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("IWDP_LIFECYCLE_SEED=%q: want an integer", s)
		}
		seed = n
	}
	t.Logf("generated plans: IWDP_LIFECYCLE_SEED=%d", seed)
	return seed
}

func TestReleaseAttachLifecycleProperty(t *testing.T) {
	for name, p := range reviewerPlans {
		t.Run(name, func(t *testing.T) {
			if bad := runLifecyclePlan(t, p); len(bad) > 0 {
				t.Errorf("plan %v:\n%s", p.Steps, strings.Join(bad, "\n"))
			}
		})
	}
	run := 0
	prop := func(p lifecyclePlan) bool {
		run++
		ok := true
		t.Run(fmt.Sprintf("generated-%d", run), func(t *testing.T) {
			if bad := runLifecyclePlan(t, p); len(bad) > 0 {
				ok = false
				t.Errorf("plan %v:\n%s", p.Steps, strings.Join(bad, "\n"))
			}
		})
		return ok
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: lifecycleCount(t), Rand: rand.New(rand.NewSource(lifecycleSeed(t)))}); err != nil {
		t.Error(err)
	}
}
