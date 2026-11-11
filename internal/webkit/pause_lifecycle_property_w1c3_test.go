package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// The pause-on-start lifecycle property (W1c-3 R1, F2): generated
// sequences of connections and reconnects, pause-on-start support,
// provisional targets with a setup that succeeds, fails or stalls, commits,
// destroys, Close and retired connections, run against fakewebkit through
// pauseConn.

type setupOutcome int

const (
	setupOK setupOutcome = iota
	setupFails
	setupStalls // blocks until the target or the session ends
)

func (o setupOutcome) String() string { return [...]string{"ok", "fails", "stalls"}[o] }

type targetEnd int

const (
	endCommit  targetEnd = iota
	endDestroy           // abandoned without commit
	endPending           // still provisional when the connection ends
)

func (e targetEnd) String() string { return [...]string{"commit", "destroy", "pending"}[e] }

// targetSched orders the plan's steps against the session's at the Close
// and retirement boundaries (review F9), with gates instead of sleeps.
type targetSched int

const (
	schedFree    targetSched = iota // no gate
	schedHold                       // its setup returns only after its connection ended
	schedResumed                    // pending: the connection ends after its Target.resume arrived
	schedWriting                    // pending: the connection ends while its Target.resume is held in the write
	// pending: the connection ends after the fake read its Target.resume
	// and before the fake records it (review F12)
	schedLateRecord
)

func (s targetSched) String() string {
	return [...]string{"free", "setup held", "resumed first", "resume in write", "recorded late"}[s]
}

type lifecycleTarget struct {
	Setup setupOutcome
	End   targetEnd
	Sched targetSched
}

type lifecycleConn struct {
	Mode    pauseMode // pausePass, pauseRefused or pauseSilent
	Targets []lifecycleTarget
	Retire  bool // the connection is cut by the far side; else Close
}

type pausePlan struct{ Conns []lifecycleConn }

func (p pausePlan) String() string {
	var b strings.Builder
	for i, c := range p.Conns {
		mode := map[pauseMode]string{pausePass: "supported", pauseRefused: "refused", pauseSilent: "no reply"}[c.Mode]
		fmt.Fprintf(&b, "conn %d (%s):", i+1, mode)
		for _, t := range c.Targets {
			fmt.Fprintf(&b, " [setup %s, %s, %s]", t.Setup, t.End, t.Sched)
		}
		if c.Retire {
			b.WriteString(" retired; ")
		} else {
			b.WriteString(" Close; ")
		}
	}
	return b.String()
}

// Generate makes 1 to 3 connections with 0 to 3 provisional targets each.
// Only the last target of a connection can still be pending when it ends,
// and only a pending target waits for its resume or its resume write.
func (pausePlan) Generate(r *rand.Rand, _ int) reflect.Value {
	var p pausePlan
	for range 1 + r.Intn(3) {
		c := lifecycleConn{Mode: []pauseMode{pausePass, pauseRefused, pauseSilent}[r.Intn(3)], Retire: r.Intn(2) == 0}
		n := r.Intn(4)
		for i := range n {
			t := lifecycleTarget{Setup: setupOutcome(r.Intn(3)), End: targetEnd(r.Intn(2))}
			if i == n-1 && r.Intn(2) == 0 {
				t.End = endPending
			}
			t.Sched = targetSched(r.Intn(2))
			if t.End == endPending {
				t.Sched = targetSched(r.Intn(5))
			}
			c.Targets = append(c.Targets, t)
		}
		p.Conns = append(p.Conns, c)
	}
	return reflect.ValueOf(p)
}

// Timing of the property's sessions: a short call budget bounds the
// wait of a silent Target.setPauseOnStart, and a short ResumeAfter the
// resume of a stalled setup.
const (
	lifecycleCallTimeout = 200 * time.Millisecond
	lifecycleResumeAfter = 150 * time.Millisecond
	lifecycleStep        = 5 * time.Second // bound of every wait in a plan
	// lifecycleWriteHeld is how long Close finds a held Target.resume
	// write (schedWriting) in progress: well within closeResumeBudget.
	lifecycleWriteHeld = 50 * time.Millisecond
)

// resumeGate sits between the session and its connection. It notes, in
// one order, each Target.resume write that the session begins and the
// session's Close of the connection; it holds the resume writes of chosen
// targets until the plan releases them (schedWriting), so that a
// connection can end while a resume is being written.
type resumeGate struct {
	transport.MessageConn
	mu      sync.Mutex
	held    map[string]chan struct{} // target -> closed when its resume write is held
	release chan struct{}
	once    sync.Once
	writes  []resumeWrite
	closed  bool // the session closed the connection
}

// resumeWrite is a Target.resume write that the session began.
type resumeWrite struct {
	target      string
	at          time.Time
	beforeClose bool // begun before the session closed the connection
}

// firstResume returns the first resume write of target, or nil.
func (g *resumeGate) firstResume(target string) *resumeWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.writes {
		if g.writes[i].target == target {
			w := g.writes[i]
			return &w
		}
	}
	return nil
}

func (g *resumeGate) Close() error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	return g.MessageConn.Close()
}

func newResumeGate() *resumeGate {
	return &resumeGate{held: map[string]chan struct{}{}, release: make(chan struct{})}
}

// hold makes the Target.resume write of id wait for releaseAll. The
// returned channel closes when that write is held.
func (g *resumeGate) hold(id string) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[id] = make(chan struct{})
	return g.held[id]
}

func (g *resumeGate) releaseAll() { g.once.Do(func() { close(g.release) }) }

func (g *resumeGate) Write(ctx context.Context, msg []byte) error {
	var m struct {
		Method string
		Params struct{ TargetID string }
	}
	_ = json.Unmarshal(msg, &m)
	if m.Method == "Target.resume" {
		g.mu.Lock()
		g.writes = append(g.writes, resumeWrite{target: m.Params.TargetID, at: time.Now(), beforeClose: !g.closed})
		held := g.held[m.Params.TargetID]
		delete(g.held, m.Params.TargetID) // a second resume is not held
		g.mu.Unlock()
		if held != nil {
			close(held)
			<-g.release
		}
	}
	return g.MessageConn.Write(ctx, msg)
}

// lateRecord makes the fake hold its bookkeeping of one target's
// Target.resume after it read the message in full (schedLateRecord).
type lateRecord struct {
	f       *fakewebkit.Server
	id      string
	read    chan struct{} // closed when the fake read the resume
	process chan struct{} // closed to let the fake record and handle it
	once    sync.Once
}

// hold starts holding the resume of id. The returned channel closes when
// the fake read it.
func (l *lateRecord) hold(id string) <-chan struct{} {
	l.id, l.read, l.process = id, make(chan struct{}), make(chan struct{})
	var readOnce sync.Once
	l.f.BeforeRecord("Target.resume", func(params json.RawMessage) {
		var v struct{ TargetID string }
		_ = json.Unmarshal(params, &v)
		if v.TargetID == id {
			readOnce.Do(func() { close(l.read) })
			<-l.process
		}
	})
	return l.read
}

// unblock lets the fake go on.
func (l *lateRecord) unblock() {
	if l.process != nil {
		l.once.Do(func() {
			l.f.BeforeRecord("Target.resume", nil)
			close(l.process)
		})
	}
}

// release lets the fake go on and, when it had read the resume, waits
// until it recorded it. It reports false when the record did not come.
func (l *lateRecord) release() bool {
	if l.process == nil {
		return true
	}
	l.unblock()
	select {
	case <-l.read:
	default:
		return true // the resume never reached the fake
	}
	for end := time.Now().Add(lifecycleStep); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		for _, r := range l.f.ReceivedMethod("Target.resume") {
			var v struct{ TargetID string }
			_ = json.Unmarshal(r.Params, &v)
			if v.TargetID == l.id {
				return true
			}
		}
	}
	return false
}

// setupRecord is what Setup saw of one target.
type setupRecord struct {
	started bool
	endedAt time.Time // zero while it runs
}

// runPausePlan runs p and returns the violated invariants.
func runPausePlan(t *testing.T, p pausePlan) []string {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	var bad []string
	fail := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }

	var mu sync.Mutex
	outcome := map[string]setupOutcome{}
	holds := map[string]<-chan struct{}{} // schedHold: the setup returns after it closes
	setups := map[string]*setupRecord{}
	started := make(chan string, 16)
	setup := func(ctx context.Context, tc TargetCaller) error {
		id := tc.TargetID()
		mu.Lock()
		o, hold := outcome[id], holds[id]
		setups[id] = &setupRecord{started: true}
		mu.Unlock()
		_, err := tc.Send(ctx, "Page.enable", nil)
		started <- id
		switch {
		case err != nil:
		case o == setupFails:
			err = errors.New("setup refused by the plan")
		case o == setupStalls:
			<-ctx.Done()
			err = ctx.Err()
		}
		if hold != nil {
			<-hold
		}
		mu.Lock()
		setups[id].endedAt = time.Now()
		mu.Unlock()
		return err
	}
	// pendingAtEnd: targets still provisional when their connection ended,
	// by how it ended.
	pendingAtEnd := map[string]bool{} // id -> retired
	planned := map[string]lifecycleTarget{}
	connOf := map[string]pauseMode{}
	connNo := map[string]int{}          // id -> its connection, numbered like the fake's
	announced := map[string]time.Time{} // id -> just before the fake announced it
	closeAt := map[int]time.Time{}      // connection -> just before Close
	gates := map[int]*resumeGate{}      // connection -> what its session wrote and closed
	for ci, cp := range p.Conns {
		var conn *pauseConn
		gate := newResumeGate()
		defer gate.releaseAll()
		gates[ci+1] = gate
		late := &lateRecord{f: f}
		defer late.unblock()
		connEnded := make(chan struct{}) // closed after the connection ended
		endConn := sync.OnceFunc(func() { close(connEnded) })
		defer endConn()
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleStep)
		s, err := Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
			c, err := DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
			if err != nil {
				return nil, err
			}
			conn = newPauseConn(c, cp.Mode, 0)
			conn.forwardOff = true
			gate.MessageConn = conn
			return gate, nil
		}, SessionOptions{PauseOnStart: true, CallTimeout: lifecycleCallTimeout, ResumeAfter: lifecycleResumeAfter, Setup: setup, Logf: func(string, ...any) {}})
		cancel()
		// (a) a connect never fails because of pause on start.
		if err != nil {
			fail("(a) connection %d (%v): Dial failed: %v", ci+1, cp.Mode, err)
			return bad
		}
		// The fake turned its pause setting off before any target of
		// this connection (pauseConn.forwardOff).
		for end := time.Now().Add(lifecycleStep); cp.Mode != pausePass && !conn.offAcked.Load(); time.Sleep(2 * time.Millisecond) {
			if time.Now().After(end) {
				fail("connection %d: the fake did not acknowledge pause on start off", ci+1)
				break
			}
		}
		if got, want := s.PausesOnStart(), cp.Mode == pausePass; got != want {
			fail("connection %d: PausesOnStart() = %v, want %v", ci+1, got, want)
		}
		for ti, tp := range cp.Targets {
			id := fmt.Sprintf("c%d-t%d", ci+1, ti+1)
			mu.Lock()
			outcome[id] = tp.Setup
			if tp.Sched == schedHold {
				holds[id] = connEnded
			}
			mu.Unlock()
			planned[id], connOf[id], connNo[id] = tp, cp.Mode, ci+1
			var writing <-chan struct{}
			if tp.Sched == schedWriting && cp.Mode == pausePass {
				writing = gate.hold(id)
			}
			var read <-chan struct{}
			if tp.Sched == schedLateRecord && cp.Mode == pausePass {
				read = late.hold(id)
			}
			announced[id] = time.Now()
			resumed := f.Provisional(id)
			select {
			case got := <-started:
				if got != id {
					fail("setup started for %s, want %s", got, id)
				}
			case <-time.After(lifecycleStep):
				fail("the setup of %s did not start", id)
				_ = s.Close()
				return bad
			}
			if tp.End == endPending {
				pendingAtEnd[id] = cp.Retire
				switch {
				case tp.Sched == schedResumed:
					select {
					case <-resumed:
					case <-time.After(lifecycleStep):
						fail("(b) pending %s (setup %s) was not resumed", id, tp.Setup)
						_ = s.Close()
						return bad
					}
				case writing != nil:
					select {
					case <-writing:
					case <-time.After(lifecycleStep):
						fail("(b) pending %s (setup %s): no Target.resume write started", id, tp.Setup)
						_ = s.Close()
						return bad
					}
				case read != nil:
					select {
					case <-read:
					case <-time.After(lifecycleStep):
						fail("(b) pending %s (setup %s): the fake read no Target.resume", id, tp.Setup)
						_ = s.Close()
						return bad
					}
				}
				continue
			}
			// A paused target is resumed after its setup ended or failed,
			// or after ResumeAfter for a stalled setup; an unpaused one
			// at once.
			select {
			case <-resumed:
			case <-time.After(lifecycleStep):
				fail("(b) %s (setup %s) was not resumed", id, tp.Setup)
				_ = s.Close()
				return bad
			}
			if tp.End == endCommit {
				f.Commit(id)
				for end := time.Now().Add(lifecycleStep); s.ActiveTarget() != id; time.Sleep(2 * time.Millisecond) {
					if time.Now().After(end) {
						fail("%s did not become active after its commit", id)
						break
					}
				}
			} else {
				f.DestroyTarget(id)
			}
		}
		if cp.Retire {
			f.CloseConnections()
			select {
			case <-s.Done():
			case <-time.After(lifecycleStep):
				fail("connection %d did not end after the far side closed it", ci+1)
			}
		} else {
			closeAt[ci+1] = time.Now()
			// A held resume write ends while Close waits for it.
			time.AfterFunc(lifecycleWriteHeld, gate.releaseAll)
			_ = s.Close()
		}
		gate.releaseAll()
		endConn()
		if !late.release() {
			fail("connection %d: the fake did not record the Target.resume it read", ci+1)
		}
		// (d) nothing stays paused in the session's record.
		if left := s.PausedTargets(); len(left) != 0 {
			fail("(d) connection %d (%s): targets %v still paused after it ended", ci+1, map[bool]string{true: "retired", false: "Close"}[cp.Retire], left)
		}
	}

	// The wire, as the fake saw it.
	paused := map[string]bool{}
	resumes := map[string][]fakewebkit.Record{}
	firstSetup := map[string]int{}
	pauseReqs := map[int]int{} // connection -> Target.setPauseOnStart received
	for _, r := range f.Log() {
		switch {
		case !r.In && r.Method == "Target.targetCreated":
			var v struct{ TargetInfo targetInfo }
			_ = json.Unmarshal(r.Params, &v)
			paused[v.TargetInfo.TargetID] = v.TargetInfo.IsPaused
		case r.In && r.Method == "Target.resume":
			var v struct{ TargetID string }
			_ = json.Unmarshal(r.Params, &v)
			resumes[v.TargetID] = append(resumes[v.TargetID], r)
		case r.In && r.Method == "Target.setPauseOnStart":
			pauseReqs[r.Conn]++
		case r.In && r.Target != "" && firstSetup[r.Target] == 0:
			firstSetup[r.Target] = r.Seq
		}
	}
	for conn := 1; conn <= len(p.Conns); conn++ {
		if pauseReqs[conn] != 1 {
			fail("connection %d asked for pause on start %d times, want once", conn, pauseReqs[conn])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for id, tp := range planned {
		if want := connOf[id] == pausePass; paused[id] != want {
			fail("%s announced paused=%v on a connection with pause on start %v", id, paused[id], want)
		}
		n := len(resumes[id])
		retired, pending := pendingAtEnd[id]
		c := connNo[id]
		// An unpaused target is never resumed: the pause record comes from
		// its Target.targetCreated (session.go:1600).
		if !paused[id] {
			if n != 0 {
				fail("(b) unpaused %s got %d Target.resume", id, n)
			}
			continue
		}
		// At most one Target.resume per target (ResumeTarget,
		// session.go:618-640).
		if n > 1 {
			fail("(b) paused %s (setup %s, %s, %s) got %d Target.resume, want at most 1", id, tp.Setup, tp.End, tp.Sched, n)
			continue
		}
		if n == 0 {
			// A target pending on a retired connection may never be
			// resumed by the session: shutdown clears its pause because
			// WebKit resumes it when the frontend disconnects
			// (session.go:1388-1395). Every other paused target is
			// resumed: the plan waited for the resume of a committed or
			// destroyed one, and Close resumes a pending one or waits for
			// its resume being written (session.go:774-791).
			if !pending || !retired {
				fail("(b) paused %s (setup %s, %s, %s) got no Target.resume, want 1", id, tp.Setup, tp.End, tp.Sched)
			}
			continue
		}
		res := resumes[id][0]
		// The fake received the resume on the target's own connection.
		if res.Conn != c {
			fail("(b) %s of connection %d resumed on connection %d", id, c, res.Conn)
		}
		// The write is observed at the session's side of the connection
		// (resumeGate), not in the fake's log: the fake may record a
		// message that it read in full only after the connection ended
		// (review F12).
		w := gates[c].firstResume(id)
		if w == nil {
			fail("(b) the fake received a Target.resume of %s that the session never wrote", id)
			continue
		}
		// The session began the resume write before it closed the
		// connection: a pending target may have resumed normally before
		// a retirement, but nothing is written after it. shutdown clears
		// every pause record and sets the session's close error, and only
		// then closes the connection (session.go:1374-1400), so a later
		// resume is refused by ResumeTarget or sendOuter
		// (session.go:623-631, 1343-1347); a write that passed those
		// checks just before shutdown fails on the closed connection and
		// reaches no fake. The order is the gate's own: the start of a
		// resume write and the session's Close, under one mutex. A frame
		// that the fake records after the close is still a write from
		// before it.
		if !w.beforeClose {
			fail("(b) %s: its Target.resume write began after the session closed the connection", id)
		}
		// The resume timer may resume a paused target ResumeAfter after
		// its announcement, whatever its setup did (resumeLater,
		// session.go:1640-1654). Otherwise the resume follows its setup.
		timerDue := !w.at.Before(announced[id].Add(lifecycleResumeAfter))
		// Close resumes a paused target at once, while its setup may
		// still run (session.go:774-791); a retired connection has no
		// Close.
		at, closed := closeAt[c]
		byClose := closed && !w.at.Before(at)
		// (c) its setup was sent before its resume. The plan calls Close
		// only after each setup's first call was answered, so this holds
		// for a resume by Close too.
		if !timerDue && (firstSetup[id] == 0 || firstSetup[id] > res.Seq) {
			fail("(c) %s: setup message #%d, Target.resume #%d", id, firstSetup[id], res.Seq)
		}
		// (b) after its setup ended or failed (setupTarget,
		// session.go:1622-1638), unless Close or the resume timer came
		// first.
		rec := setups[id]
		if !timerDue && !byClose && (rec == nil || rec.endedAt.IsZero() || w.at.Before(rec.endedAt)) {
			fail("(b) %s (setup %s) resumed before its setup ended, without Close and before ResumeAfter", id, tp.Setup)
		}
	}
	return bad
}

// pauseSeed is the seed of the generated plans: IWDP_PAUSE_SEED, else the
// clock. It is logged so that a failure can be replayed.
func pauseSeed(t *testing.T) int64 {
	seed := time.Now().UnixNano()
	if s := os.Getenv("IWDP_PAUSE_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("IWDP_PAUSE_SEED=%q: want an integer", s)
		}
		seed = n
	}
	t.Logf("generated plans: IWDP_PAUSE_SEED=%d", seed)
	return seed
}

// pauseCount is the number of generated plans: IWDP_PAUSE_COUNT, else 25.
func pauseCount(t *testing.T) int {
	if s := os.Getenv("IWDP_PAUSE_COUNT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("IWDP_PAUSE_COUNT=%q: want a positive integer", s)
		}
		return n
	}
	return 25
}

// Fixed plans that cover every invariant whatever the seed.
var pauseEdgePlans = map[string]pausePlan{
	"stalled setup pending at Close, then a reconnect without reply": {Conns: []lifecycleConn{
		{Mode: pausePass, Targets: []lifecycleTarget{{setupOK, endCommit, schedFree}, {setupStalls, endPending, schedFree}}},
		{Mode: pauseSilent, Targets: []lifecycleTarget{{setupFails, endDestroy, schedFree}}},
	}},
	"stalled setup pending on a retired connection, then refused": {Conns: []lifecycleConn{
		{Mode: pausePass, Targets: []lifecycleTarget{{setupFails, endDestroy, schedFree}, {setupStalls, endPending, schedFree}}, Retire: true},
		{Mode: pauseRefused, Targets: []lifecycleTarget{{setupStalls, endCommit, schedFree}}},
		{Mode: pausePass, Targets: []lifecycleTarget{{setupStalls, endCommit, schedFree}, {setupOK, endPending, schedFree}}},
	}},
	"resume being written at Close, then at retirement": {Conns: []lifecycleConn{
		{Mode: pausePass, Targets: []lifecycleTarget{{setupOK, endPending, schedWriting}}},
		{Mode: pausePass, Targets: []lifecycleTarget{{setupFails, endPending, schedWriting}}, Retire: true},
	}},
}

// The two F9 schedules of the closure review, as permanent tests.

// TestPauseOnStartLifecycleCloseDuringSetup: Close comes while the setup
// of a paused target still runs, so Close resumes the target before Setup
// returns.
func TestPauseOnStartLifecycleCloseDuringSetup(t *testing.T) {
	for _, o := range []setupOutcome{setupOK, setupFails} {
		t.Run(o.String(), func(t *testing.T) {
			p := pausePlan{Conns: []lifecycleConn{{Mode: pausePass, Targets: []lifecycleTarget{{o, endPending, schedHold}}}}}
			if bad := runPausePlan(t, p); len(bad) > 0 {
				t.Errorf("plan %s\n%s", p, strings.Join(bad, "\n"))
			}
		})
	}
}

// TestPauseOnStartLifecycleResumeRecordedAfterRetirement: the fake read a
// pending target's whole Target.resume before the far side closed the
// connection, and recorded it only after the session saw the close
// (review F12).
func TestPauseOnStartLifecycleResumeRecordedAfterRetirement(t *testing.T) {
	for _, o := range []setupOutcome{setupOK, setupStalls} {
		t.Run(o.String(), func(t *testing.T) {
			p := pausePlan{Conns: []lifecycleConn{{Mode: pausePass, Targets: []lifecycleTarget{{o, endPending, schedLateRecord}}, Retire: true}}}
			if bad := runPausePlan(t, p); len(bad) > 0 {
				t.Errorf("plan %s\n%s", p, strings.Join(bad, "\n"))
			}
		})
	}
}

// TestPauseOnStartLifecycleResumeBeforeRetirement: a target that is still
// provisional was resumed normally before the far side closed the
// connection.
func TestPauseOnStartLifecycleResumeBeforeRetirement(t *testing.T) {
	for _, o := range []setupOutcome{setupOK, setupFails, setupStalls} {
		t.Run(o.String(), func(t *testing.T) {
			p := pausePlan{Conns: []lifecycleConn{{Mode: pausePass, Targets: []lifecycleTarget{{o, endPending, schedResumed}}, Retire: true}}}
			if bad := runPausePlan(t, p); len(bad) > 0 {
				t.Errorf("plan %s\n%s", p, strings.Join(bad, "\n"))
			}
		})
	}
}

func TestPauseOnStartLifecycleProperty(t *testing.T) {
	for name, p := range pauseEdgePlans {
		t.Run(name, func(t *testing.T) {
			if bad := runPausePlan(t, p); len(bad) > 0 {
				t.Errorf("plan %s\n%s", p, strings.Join(bad, "\n"))
			}
		})
	}
	run := 0
	prop := func(p pausePlan) bool {
		run++
		ok := true
		t.Run(fmt.Sprintf("generated-%d", run), func(t *testing.T) {
			if bad := runPausePlan(t, p); len(bad) > 0 {
				ok = false
				t.Errorf("plan %s\n%s", p, strings.Join(bad, "\n"))
			}
		})
		return ok
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: pauseCount(t), Rand: rand.New(rand.NewSource(pauseSeed(t)))}); err != nil {
		t.Error(err)
	}
}
