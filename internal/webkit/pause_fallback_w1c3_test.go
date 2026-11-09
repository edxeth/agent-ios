package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// pauseMode is how pauseConn treats Target.setPauseOnStart.
type pauseMode int

const (
	pauseRefused pauseMode = iota // answer with a protocol error, never forward
	pauseSilent                   // drop it: no reply ever
	pauseLate                     // forward it; hold the reply for lateBy
	pauseWireCut                  // close the connection when it is written
	pausePass                     // forward it: the endpoint supports it
)

// pauseConn sits between a Session and fakewebkit and plays an endpoint
// whose answer to Target.setPauseOnStart is refused, missing, late or a
// closed connection. fakewebkit itself always acknowledges the method.
type pauseConn struct {
	transport.MessageConn
	mode   pauseMode
	lateBy time.Duration
	// forwardOff: for pauseRefused and pauseSilent, send the fake
	// Target.setPauseOnStart(false) instead and drop its reply, so that a
	// setting left by an earlier connection does not pause targets (WebKit
	// resets it when its last frontend disconnects; the fake does not).
	forwardOff bool

	in     chan readResult
	inject chan []byte

	mu     sync.Mutex
	lateID int64
	dropID int64       // the forwarded setPauseOnStart(false), whose reply is dropped
	late   atomic.Bool // the late reply was delivered
	// offAcked: the fake acknowledged the forwarded setPauseOnStart(false).
	offAcked atomic.Bool
}

type readResult struct {
	msg []byte
	err error
}

func newPauseConn(c transport.MessageConn, mode pauseMode, lateBy time.Duration) *pauseConn {
	p := &pauseConn{MessageConn: c, mode: mode, lateBy: lateBy, in: make(chan readResult, 64), inject: make(chan []byte, 4)}
	go func() {
		for {
			msg, err := c.Read(context.Background())
			if err == nil && p.holdLate(msg) {
				continue
			}
			p.in <- readResult{msg, err}
			if err != nil {
				return
			}
		}
	}()
	return p
}

// holdLate holds the fake's reply to the forwarded Target.setPauseOnStart
// and delivers it lateBy later.
func (p *pauseConn) holdLate(msg []byte) bool {
	var m struct{ ID int64 }
	_ = json.Unmarshal(msg, &m)
	p.mu.Lock()
	id, drop := p.lateID, p.dropID
	p.mu.Unlock()
	if drop != 0 && m.ID == drop {
		p.offAcked.Store(true) // the fake acknowledges after it stored the setting
		return true
	}
	if p.mode != pauseLate || id == 0 || m.ID != id {
		return false
	}
	time.AfterFunc(p.lateBy, func() {
		p.inject <- msg
		p.late.Store(true)
	})
	return true
}

func (p *pauseConn) Write(ctx context.Context, msg []byte) error {
	var m struct {
		ID     int64
		Method string
	}
	_ = json.Unmarshal(msg, &m)
	if m.Method != "Target.setPauseOnStart" {
		return p.MessageConn.Write(ctx, msg)
	}
	if p.forwardOff && (p.mode == pauseRefused || p.mode == pauseSilent) {
		p.mu.Lock()
		p.dropID = m.ID
		p.mu.Unlock()
		off := []byte(fmt.Sprintf(`{"id":%d,"method":"Target.setPauseOnStart","params":{"pauseOnStart":false}}`, m.ID))
		if err := p.MessageConn.Write(ctx, off); err != nil {
			return err
		}
	}
	switch p.mode {
	case pausePass:
		return p.MessageConn.Write(ctx, msg)
	case pauseRefused:
		p.inject <- []byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"'Target.setPauseOnStart' was not found"}}`, m.ID))
		return nil
	case pauseSilent:
		return nil
	case pauseLate:
		p.mu.Lock()
		p.lateID = m.ID
		p.mu.Unlock()
		return p.MessageConn.Write(ctx, msg)
	default: // pauseWireCut
		return p.Close()
	}
}

func (p *pauseConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case m := <-p.inject:
		return m, nil
	case r := <-p.in:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// logLines collects Logf calls.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// pauseRig dials f through a pauseConn with pause on start and a Setup
// that records the targets it set up.
type pauseRig struct {
	f      *fakewebkit.Server
	conn   *pauseConn
	logs   *logLines
	setups chan string
	opts   SessionOptions
}

func newPauseRig(t *testing.T) *pauseRig {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	r := &pauseRig{f: f, logs: &logLines{}, setups: make(chan string, 8)}
	r.opts = SessionOptions{
		PauseOnStart: true,
		Logf:         r.logs.logf,
		Setup: func(ctx context.Context, tc TargetCaller) error {
			_, err := tc.Send(ctx, "Page.enable", nil)
			r.setups <- tc.TargetID()
			return err
		},
	}
	return r
}

func (r *pauseRig) open(mode pauseMode, lateBy time.Duration) func(context.Context) (transport.MessageConn, error) {
	return func(ctx context.Context) (transport.MessageConn, error) {
		c, err := DialWebSocket(ctx, r.f.URL, transport.Limits{Routed: true})
		if err != nil {
			return nil, err
		}
		r.conn = newPauseConn(c, mode, lateBy)
		return r.conn, nil
	}
}

// wantUsable checks that s answers a page command and sets up a
// provisional target that commits.
func (r *pauseRig) wantUsable(t *testing.T, s *Session, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"}); err != nil {
		t.Fatalf("Runtime.evaluate on the connection: %v", err)
	}
	done := make(chan struct{})
	go func() { r.f.Swap(id, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the swap to %s did not commit within 5 s", id)
	}
	select {
	case got := <-r.setups:
		if got != id {
			t.Fatalf("set up %s, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("provisional target %s was not set up", id)
	}
	for end := time.Now().Add(5 * time.Second); s.ActiveTarget() != id; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("active target %q 5 s after the commit, want %s", s.ActiveTarget(), id)
		}
	}
}

func wantOneLog(t *testing.T, l *logLines, want string) {
	t.Helper()
	lines := l.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "pause on start is off") || !strings.Contains(lines[0], want) {
		t.Fatalf("log lines %q, want one that says pause on start is off and %q", lines, want)
	}
}

// F1 (W1c-3 R1): a protocol error reply to Target.setPauseOnStart leaves
// pause on start off; the connect succeeds, the reason is logged once, and
// a provisional target, which arrives unpaused, is set up as before.
func TestPauseOnStartRefusalKeepsTheConnection(t *testing.T) {
	r := newPauseRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Dial(ctx, r.open(pauseRefused, 0), r.opts)
	if err != nil {
		t.Fatalf("Dial with Target.setPauseOnStart refused: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.PausesOnStart() {
		t.Fatal("PausesOnStart() = true after a refusal")
	}
	// The protocol error's code, not its message (review devfix-R1 F1).
	wantOneLog(t, r.logs, "the endpoint refused it (WebKit protocol error -32601)")
	r.wantUsable(t, s, "page-2")
	if n := len(r.f.ReceivedMethod("Target.resume")); n != 0 {
		t.Fatalf("%d Target.resume sent for an unpaused target", n)
	}
}

// F1: no reply to Target.setPauseOnStart within pauseOnStartWait leaves
// pause on start off; the connect waits that long, not the 10 s call
// budget.
func TestPauseOnStartWithoutReplyIsBoundedAndKeepsTheConnection(t *testing.T) {
	r := newPauseRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	s, err := Dial(ctx, r.open(pauseSilent, 0), r.opts)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("Dial with Target.setPauseOnStart unanswered: %v", err)
	}
	defer func() { _ = s.Close() }()
	if el < pauseOnStartWait || el > pauseOnStartWait+time.Second {
		t.Fatalf("connected after %s, want the %s pause-on-start wait and no more", el, pauseOnStartWait)
	}
	if s.PausesOnStart() {
		t.Fatal("PausesOnStart() = true without a reply")
	}
	wantOneLog(t, r.logs, "no reply within 1s")
	r.wantUsable(t, s, "page-2")
}

// F1: a reply that comes after the wait is dropped. WebKit applied the
// setting, so it pauses the next provisional target; the session still
// resumes it after its setup, because it reads isPaused from the event.
func TestPauseOnStartLateReplyIsIgnored(t *testing.T) {
	const lateBy = pauseOnStartWait + 300*time.Millisecond
	r := newPauseRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	s, err := Dial(ctx, r.open(pauseLate, lateBy), r.opts)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("Dial with a late Target.setPauseOnStart reply: %v", err)
	}
	defer func() { _ = s.Close() }()
	if el > pauseOnStartWait+time.Second/2 {
		t.Fatalf("connected after %s; the connect waited for the late reply", el)
	}
	for end := time.Now().Add(5 * time.Second); !r.conn.late.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the late reply was not delivered")
		}
	}
	time.Sleep(50 * time.Millisecond) // the read loop handles it
	if s.PausesOnStart() {
		t.Fatal("PausesOnStart() = true after a reply that came after the wait")
	}
	wantOneLog(t, r.logs, "no reply within 1s")
	r.wantUsable(t, s, "page-2")
	setupSeq, resumeSeq := 0, 0
	for _, rec := range r.f.Log() {
		switch {
		case rec.In && rec.Target == "page-2" && rec.Method == "Page.enable" && setupSeq == 0:
			setupSeq = rec.Seq
		case rec.In && rec.Method == "Target.resume" && resumeSeq == 0:
			resumeSeq = rec.Seq
		}
	}
	if resumeSeq == 0 || setupSeq == 0 || resumeSeq < setupSeq {
		t.Fatalf("paused provisional target: setup #%d, Target.resume #%d; want the setup, then one resume", setupSeq, resumeSeq)
	}
	if n := len(r.f.ReceivedMethod("Target.resume")); n != 1 {
		t.Fatalf("%d Target.resume sent, want 1", n)
	}
	if n := s.malformed.Load(); n != 0 {
		t.Fatalf("%d messages counted as malformed; the late reply must be dropped quietly", n)
	}
}

// F1: a connection that closes while Target.setPauseOnStart is written is
// a wire failure, not a refusal: the connect fails as before, with no
// retry, and nothing is logged as a degraded connection.
func TestPauseOnStartWireFailureStillFailsTheConnect(t *testing.T) {
	r := newPauseRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var opens atomic.Int32
	open := r.open(pauseWireCut, 0)
	_, err := Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { opens.Add(1); return open(ctx) }, r.opts)
	if err == nil || !strings.Contains(err.Error(), "enabling pause on start") {
		t.Fatalf("Dial with the connection closed at Target.setPauseOnStart: %v, want the connect to fail", err)
	}
	if errors.Is(err, ErrNoTarget) || opens.Load() != 1 {
		t.Fatalf("err %v after %d attempts; a wire failure after the target is not retried", err, opens.Load())
	}
	if lines := r.logs.all(); len(lines) != 0 {
		t.Fatalf("log lines %q for a wire failure, want none", lines)
	}
}

// F1: the caller's deadline still bounds the wait; a deadline during it
// fails the connect with the deadline (exit 124 in the daemon).
func TestPauseOnStartWaitEndsWithTheCallerDeadline(t *testing.T) {
	r := newPauseRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, r.open(pauseSilent, 0), r.opts)
	if el := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || el > pauseOnStartWait {
		t.Fatalf("Dial with a 300 ms deadline and no pause reply: %v after %s, want the deadline before the %s wait ended", err, el, pauseOnStartWait)
	}
	if lines := r.logs.all(); len(lines) != 0 {
		t.Fatalf("log lines %q for a deadline, want none", lines)
	}
}
