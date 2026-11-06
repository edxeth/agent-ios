package direct

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// gateConn wraps the client end of the device pipe. While held, writes block
// until release and report on entered when they start; a deadline error
// makes SetWriteDeadline fail.
type gateConn struct {
	net.Conn
	mu          sync.Mutex
	hold        chan struct{}
	entered     chan struct{}
	deadlineErr error
}

func (g *gateConn) Write(p []byte) (int, error) {
	g.mu.Lock()
	hold, entered := g.hold, g.entered
	g.mu.Unlock()
	if hold != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
	}
	return g.Conn.Write(p)
}

func (g *gateConn) SetWriteDeadline(t time.Time) error {
	g.mu.Lock()
	err := g.deadlineErr
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return g.Conn.SetWriteDeadline(t)
}

// block makes the next device write wait in the write loop until the
// returned release runs. It returns once a frame is blocked there.
func (g *gateConn) block(t *testing.T, startWrite func()) (release func()) {
	t.Helper()
	hold, entered := make(chan struct{}), make(chan struct{}, 1)
	g.mu.Lock()
	g.hold, g.entered = hold, entered
	g.mu.Unlock()
	go startWrite()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no device write started")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.hold = nil
			g.mu.Unlock()
			close(hold)
		})
	}
}

func gatedDevice(t *testing.T) (*pipeDialer, *Transport, func() *gateConn) {
	t.Helper()
	d, _, _ := twoPageDevice(t)
	var mu sync.Mutex
	var gc *gateConn
	d.wrap = func(c net.Conn) deviceConn {
		mu.Lock()
		defer mu.Unlock()
		gc = &gateConn{Conn: c}
		return gc
	}
	tr := newWithDialer(d, Options{MaxIncoming: testCap})
	t.Cleanup(func() { _ = tr.Close() })
	return d, tr, func() *gateConn {
		mu.Lock()
		defer mu.Unlock()
		return gc
	}
}

func evalMsg(id int) []byte {
	return []byte(`{"id":` + strconv.Itoa(id) + `,"method":"Runtime.evaluate","params":{"expression":"1"}}`)
}

func wantNotWritten(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, transport.ErrNotWritten) {
		t.Fatalf("%s: %v, want an error that wraps transport.ErrNotWritten", what, err)
	}
}

// transport.MessageConn contract: a Write refused before anything of the
// message reached the device wraps transport.ErrNotWritten and keeps its
// cause. webkit.Session turns it into webkit.ErrNotSent, which keeps paused
// targets paused and held requests pending.
func TestPreWriteRefusalsAreNotWritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("page connection closed", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		_ = c.Close()
		err := c.Write(ctx, evalMsg(1))
		wantNotWritten(t, "Write after Close", err)
		if !errors.Is(err, errPageConnClosed) {
			t.Fatalf("Write after Close: %v, want the close cause", err)
		}
	})
	t.Run("transport closed", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		_ = tr.Close()
		err := c.Write(ctx, evalMsg(1))
		wantNotWritten(t, "Write after transport Close", err)
		if !errors.Is(err, webkit.ErrPageClosed) || !errors.Is(err, ErrTransportClosed) {
			t.Fatalf("Write after transport Close: %v, want ErrPageClosed and ErrTransportClosed", err)
		}
	})
	t.Run("above MaxOutgoing", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		err := c.Write(ctx, make([]byte, MaxOutgoing+1))
		wantNotWritten(t, "Write above MaxOutgoing", err)
		if !strings.Contains(err.Error(), "the direct transport sends at most") {
			t.Fatalf("Write above MaxOutgoing: %v, want the size refusal", err)
		}
	})
	t.Run("result above the frame cap", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		err := c.Write(ctx, []byte(`{"id":1,"method":"Page.snapshotRect","params":{"x":0,"y":0,"width":375,"height":1000,"coordinateSystem":"Page"}}`))
		wantNotWritten(t, "large snapshot", err)
		var tooLarge *ResultTooLargeError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("large snapshot: %v, want a *ResultTooLargeError", err)
		}
	})
	t.Run("context already ended", func(t *testing.T) {
		d, tr, _ := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		ended, cancelEnded := context.WithCancel(context.Background())
		cancelEnded()
		// With an idle write loop, a check after the handoff select would
		// write the message about half of the time.
		for i := range 50 {
			err := c.Write(ended, evalMsg(100+i))
			wantNotWritten(t, "Write with an ended context", err)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Write with an ended context: %v, want context.Canceled", err)
			}
		}
		// Frames arrive in order: once a later valid message is in, every
		// refused one would have arrived before it.
		if err := c.Write(ctx, evalMsg(1)); err != nil {
			t.Fatal(err)
		}
		got := waitMessages(t, d.fake(0), selForwardSocketData, 1)
		if len(got) != 1 || !strings.Contains(string(got[0][keySocketData].([]byte)), `"id":1,`) {
			t.Fatalf("the device received %d messages, want only the one written with a live context", len(got))
		}
	})
	t.Run("deadline while another frame is written", func(t *testing.T) {
		_, tr, gc := gatedDevice(t)
		listPages(t, tr)
		a := openRaw(t, tr, "PID:100/1")
		b := openRaw(t, tr, "PID:100/2")
		inFlight := make(chan error, 1)
		aCtx, cancelA := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancelA()
		release := gc().block(t, func() { inFlight <- a.Write(aCtx, evalMsg(1)) })
		defer release()
		short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancelShort()
		err := b.Write(short, evalMsg(2))
		wantNotWritten(t, "Write that waited for the write loop", err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Write that waited for the write loop: %v, want the deadline", err)
		}
		// The frame handed to the write loop may still arrive: not marked.
		if err := <-inFlight; err == nil || errors.Is(err, transport.ErrNotWritten) || !strings.Contains(err.Error(), "may still arrive") {
			t.Fatalf("Write whose frame was being written at the deadline: %v, want 'may still arrive' without ErrNotWritten", err)
		}
	})
	t.Run("device connection ended", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		dev := tr.devices[testUDID]
		cause := errors.New("test: device gone")
		dev.fail(cause)
		err := dev.send(ctx, selForwardSocketData, nil)
		wantNotWritten(t, "send on an ended device", err)
		if !errors.Is(err, cause) {
			t.Fatalf("send on an ended device: %v, want its cause", err)
		}
	})
	t.Run("message does not encode", func(t *testing.T) {
		_, tr, _ := gatedDevice(t)
		listPages(t, tr)
		err := tr.devices[testUDID].send(ctx, selForwardSocketData, map[string]any{"bad": func() {}})
		wantNotWritten(t, "send of a message that does not encode", err)
	})
	t.Run("write deadline cannot be set", func(t *testing.T) {
		_, tr, gc := gatedDevice(t)
		listPages(t, tr)
		c := openRaw(t, tr, "PID:100/1")
		gc().mu.Lock()
		gc().deadlineErr = errors.New("test: deadline refused")
		gc().mu.Unlock()
		err := c.Write(ctx, evalMsg(1))
		wantNotWritten(t, "Write whose deadline could not be set", err)
	})
}

// A write that failed after handoff may have reached the device in part: it
// is not marked.
func TestFailedDeviceWriteIsNotMarkedNotWritten(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	d.wrap = func(c net.Conn) deviceConn { return &writeConn{Conn: c, fail: nil} }
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	c := openRaw(t, tr, "PID:100/1")
	wc := tr.devices[testUDID].conn.(*writeConn)
	wc.mu.Lock()
	wc.fail = errors.New("broken pipe")
	wc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Write(ctx, evalMsg(1))
	if err == nil || !strings.Contains(err.Error(), "broken pipe") || errors.Is(err, transport.ErrNotWritten) {
		t.Fatalf("failed device write: %v, want the write error without ErrNotWritten", err)
	}
}

// Integration over the direct transport: a Target.resume that the write
// loop never took leaves the target paused, and a later resume reaches the
// page (review finding: direct errors lacked transport.ErrNotWritten, so
// webkit.Session cleared the pause).
func TestUnsentResumeOverDirectKeepsTargetPaused(t *testing.T) {
	d, tr, gc := gatedDevice(t)
	srvA := d.apps[0].pages[0].server
	pages := listPages(t, tr)
	s := openSession(t, tr, findPage(t, pages, "PID:100/1"), webkit.SessionOptions{
		PauseOnStart: true, ResumeAfter: time.Minute,
		Setup: func(ctx context.Context, tc webkit.TargetCaller) error { <-ctx.Done(); return ctx.Err() },
	})
	go srvA.Swap("page-2", 0)
	waitCond(t, "the paused provisional target", func() bool { return s.IsPaused("page-2") })

	other := openRaw(t, tr, "PID:100/2")
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	release := gc().block(t, func() { _ = other.Write(long, evalMsg(1)) })
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	err := s.ResumeTarget(short, "page-2")
	release()
	if !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("Target.resume refused before the write loop: %v, want webkit.ErrNotSent", err)
	}
	if !s.IsPaused("page-2") {
		t.Fatal("an unsent Target.resume cleared the paused state")
	}
	if err := s.ResumeTarget(long, "page-2"); err != nil {
		t.Fatalf("Target.resume after the unsent one: %v", err)
	}
	waitCond(t, "Target.resume at the page", func() bool { return len(srvA.ReceivedMethod("Target.resume")) == 1 })
}

// Integration over the direct transport with the core scheduler's control
// lane: a request completion that the write loop never took leaves the
// intercepted request pending, and a later completion reaches the page.
func TestUnsentCompletionOverDirectKeepsRequestPending(t *testing.T) {
	d, tr, gc := gatedDevice(t)
	srvA := d.apps[0].pages[0].server
	pages := listPages(t, tr)
	s := openSession(t, tr, findPage(t, pages, "PID:100/1"), webkit.SessionOptions{})
	sch := core.NewScheduler(s, core.SchedulerOptions{})
	t.Cleanup(sch.Close)
	sch.SetLease("s1", 1)
	srvA.Emit("Network.requestIntercepted", map[string]string{"requestId": "r1"})
	waitCond(t, "the intercepted request", func() bool { return len(sch.PendingRequests()) == 1 })

	other := openRaw(t, tr, "PID:100/2")
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	release := gc().block(t, func() { _ = other.Write(long, evalMsg(1)) })
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	cont := core.ControlMsg{Kind: core.ControlCompleteRequest, Session: "s1", Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}
	_, err := sch.Control(short, cont)
	release()
	if !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("completion refused before the write loop: %v, want webkit.ErrNotSent", err)
	}
	if p := sch.PendingRequests(); len(p) != 1 || p[0] != "r1" {
		t.Fatalf("pending = %v after an unsent completion, want [r1]", p)
	}
	if _, err := sch.Control(long, cont); err != nil {
		t.Fatalf("completion after the unsent one: %v", err)
	}
	if n := len(srvA.ReceivedMethod("Network.interceptContinue")); n != 1 {
		t.Fatalf("the page received %d interceptContinue, want 1", n)
	}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
