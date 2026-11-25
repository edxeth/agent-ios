package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport/direct"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// L07, tab part (w1c backlog): when the page connection ends during the
// probe of "tab <id>" (another inspector took the page, or the device
// ended the connection), the error names the lost connection and
// --takeover, not a hidden Safari tab. The tab is not selected; exit 4.
func TestTabSelectNamesTakeoverWhenTheProbeLosesThePage(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	lost := f.add("Lost", "http://lost.test/")
	lost.Stall("Runtime.evaluate")
	endOnProbe(t, lost, lost.CloseConnections)
	_, err := runIn(t, m, "a", "tab", "t2")
	if err == nil {
		t.Fatal("tab t2 succeeded on a page whose connection ended during the probe")
	}
	msg := err.Error()
	if strings.Contains(msg, "not the visible Safari tab") || !strings.Contains(msg, "--takeover") || !strings.Contains(msg, "tab t2") {
		t.Fatalf("error %q: want the lost connection of tab t2 and --takeover, not a hidden tab", msg)
	}
	if code := ExitCodeOf(err); code != ExitBusy {
		t.Fatalf("exit code %d, want %d", code, ExitBusy)
	}
	if _, ok := m.CurrentPage("a"); ok {
		t.Fatal("the lost tab became the current page")
	}
}

// L07, same class: a tab whose page closes (its target is destroyed)
// during the probe is reported as closed, not as a hidden Safari tab.
func TestTabSelectReportsAClosedTab(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	closing := f.add("Closing", "http://closing.test/")
	closing.Stall("Runtime.evaluate")
	endOnProbe(t, closing, closing.DestroyActive)
	_, err := runIn(t, m, "a", "tab", "t2")
	if err == nil {
		t.Fatal("tab t2 succeeded on a page that closed during the probe")
	}
	msg := err.Error()
	if strings.Contains(msg, "not the visible Safari tab") || !strings.Contains(msg, "tab t2 closed") || !strings.Contains(msg, "tab list") {
		t.Fatalf("error %q: want tab t2 closed and 'tab list', not a hidden tab", msg)
	}
	if code := ExitCodeOf(err); code != ExitBusy {
		t.Fatalf("exit code %d, want %d", code, ExitBusy)
	}
	if _, ok := m.CurrentPage("a"); ok {
		t.Fatal("the closed tab became the current page")
	}
}

// The probe failures that the transports produce, and the advice of each
// (tabSelectError, causes 1 to 5). The error shapes are the ones the code
// builds:
//   - iwdp and WebKitGTK (webkit.Session over WebSocket): a destroyed
//     target fails calls with ErrPageClosed (handleTargetDestroyed); a read
//     error ends the session with "ErrSessionClosed: <read error>"
//     (readLoop); a failed socket write ends it with "ErrSessionClosed:
//     ErrWriteFailed: ErrWriteTimeout|ErrWriteBroken (...)" (retiredBy).
//   - direct (webkit.Session over the WIR page connection): an application
//     or device end, and the transport's Close, end the page connection
//     with ErrPageClosed, which readLoop keeps inside ErrSessionClosed
//     (wir.go endApps and fail, direct.go Close). Its writes are no
//     WebSocket writes, so ErrWriteTimeout and ErrWriteBroken do not occur.
//   - both: pageConn refuses a lost page with ErrLost and a page the daemon
//     closed with ErrSessionClosed, as "not sent" (currentLocked); the
//     daemon's own Session.Close fails a pending call with ErrSessionClosed
//     and leaves the page ConnClosed (pageConn.close, only at shutdown while
//     an operation runs); at shutdown the daemon first ends the operation,
//     so the call ends with "context canceled: the operation has ended"
//     (core.ErrOpEnded, TestTabSelectDuringDaemonShutdown); a call that the
//     liveness probe ended gets ErrSessionStalled (stallCut, send); the
//     probe budget and an operation deadline (core.ErrDeadline) are
//     deadlines.
//
// state is the page's connection state read after the failure; watch marks
// an unexpected end lost asynchronously, so a peer end or a closed page is
// tested with the page still attached and already lost.
func TestTabSelectErrorAdvice(t *testing.T) {
	const (
		closed   = "tab t2 closed during the probe"
		daemon   = "the daemon ended the probe"
		takeover = "--takeover"
		hidden   = "not the visible Safari tab"
	)
	call := func(err error) error { return fmt.Errorf("Runtime.evaluate: %w", err) }
	notSent := func(err error) error {
		return fmt.Errorf("Runtime.evaluate: not sent: %w", fmt.Errorf("%w: %w", webkit.ErrNotSent, &core.CodedError{Code: core.CodeBusy, Err: err}))
	}
	readEnd := func(cause error) error { return call(fmt.Errorf("%w: %w", webkit.ErrSessionClosed, cause)) }
	writeEnd := func(category error) error {
		return call(fmt.Errorf("%w: %w: %w (write tcp: broken pipe)", webkit.ErrSessionClosed, webkit.ErrWriteFailed, category))
	}
	stalled := &core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf("%w: %w", webkit.ErrSessionStalled, webkit.ErrCallTimeout)}
	deadline := call(fmt.Errorf("no response: %w", context.DeadlineExceeded))
	attachedOrLost := []core.ConnState{core.ConnOK, core.ConnLost}
	for _, tc := range []struct {
		transport, cause string
		err              error
		states           []core.ConnState
		want             string
	}{
		{"iwdp", "closed page (target destroyed)", call(webkit.ErrPageClosed), attachedOrLost, closed},
		{"iwdp", "peer end, read error", readEnd(errors.New("websocket: close 1006 (abnormal closure): unexpected EOF")), attachedOrLost, takeover},
		{"iwdp", "peer end, ErrWriteBroken", writeEnd(webkit.ErrWriteBroken), attachedOrLost, takeover},
		{"iwdp", "peer end, ErrLost", notSent(core.ErrLost), []core.ConnState{core.ConnLost}, takeover},
		{"iwdp", "peer end, stall wording (r9d)", stalled, []core.ConnState{core.ConnLost}, takeover},
		{"iwdp", "write timeout", writeEnd(webkit.ErrWriteTimeout), []core.ConnState{core.ConnStalled, core.ConnOK}, hidden},
		{"iwdp", "session closed by the daemon", call(webkit.ErrSessionClosed), []core.ConnState{core.ConnClosed}, daemon},
		{"iwdp", "operation ended by the daemon", call(fmt.Errorf("no response after 2ms: %w: %w", context.Canceled, core.ErrOpEnded)), []core.ConnState{core.ConnOK, core.ConnClosed}, daemon},
		{"iwdp", "operation deadline", call(fmt.Errorf("no response after 2s: %w: %w", context.Canceled, core.ErrDeadline)), []core.ConnState{core.ConnOK}, hidden},
		{"iwdp", "session closed by the daemon, not sent", notSent(webkit.ErrSessionClosed), []core.ConnState{core.ConnClosed}, daemon},
		{"iwdp", "context deadline", deadline, []core.ConnState{core.ConnOK}, hidden},
		{"iwdp", "stalled", stalled, []core.ConnState{core.ConnStalled}, hidden},
		{"direct", "closed page (application disconnected)", readEnd(fmt.Errorf("%w: application PID:1 disconnected", webkit.ErrPageClosed)), attachedOrLost, closed},
		{"direct", "closed page (device connection ended)", readEnd(fmt.Errorf("%w: the connection to device 00008020 ended: %w", webkit.ErrPageClosed, io.EOF)), attachedOrLost, closed},
		{"direct", "peer end (reader fell behind)", readEnd(errors.New("direct: page connection ended: its reader fell 256 messages behind")), attachedOrLost, takeover},
		{"direct", "peer end, ErrLost", notSent(core.ErrLost), []core.ConnState{core.ConnLost}, takeover},
		{"direct", "session closed by the daemon", call(webkit.ErrSessionClosed), []core.ConnState{core.ConnClosed}, daemon},
		{"direct", "transport closed at daemon shutdown", readEnd(fmt.Errorf("%w: %w", webkit.ErrPageClosed, direct.ErrTransportClosed)), []core.ConnState{core.ConnClosed}, daemon},
		{"direct", "operation ended by the daemon", call(fmt.Errorf("no response after 2ms: %w: %w", context.Canceled, core.ErrOpEnded)), []core.ConnState{core.ConnOK, core.ConnClosed}, daemon},
		{"direct", "context deadline", deadline, []core.ConnState{core.ConnOK}, hidden},
		{"direct", "stalled", stalled, []core.ConnState{core.ConnStalled}, hidden},
	} {
		for _, state := range tc.states {
			got := tabSelectError("t2", true, state, tc.err)
			msg := got.Error()
			ok := strings.Contains(msg, tc.want) && errors.Is(got, tc.err) && ExitCodeOf(got) == ExitBusy
			for _, other := range []string{closed, daemon, takeover, hidden} {
				if other != tc.want && strings.Contains(msg, other) {
					ok = false
				}
			}
			if !ok {
				t.Errorf("%s, %s, state %s: %v (exit %d); want only %q, the cause kept and exit %d", tc.transport, tc.cause, state, got, ExitCodeOf(got), tc.want, ExitBusy)
			}
		}
	}
	// Ended or refused operations (core opCaller.Send and Op.active; the
	// same on both transports, because the fence runs before the
	// connection). Refusals carry ErrNotSent and no context.Canceled; a call
	// in flight ends with "context canceled: <operation cause>" (webkit
	// ctxErr). The page is still attached when the operation ends first
	// (Manager.Close and releasePage end operations before they close or
	// clean up the connection). Not reachable by the probe: ErrAttemptClosed
	// (RunSteps only), ErrControlRefused (control lane only) and the
	// control-lane cancel (no production sender of ControlCancel).
	fence := func(cause error) error { return fmt.Errorf("Runtime.evaluate: %w: %w", webkit.ErrNotSent, cause) }
	inFlight := func(cause error) error {
		return call(fmt.Errorf("no response after 2ms: %w: %w", context.Canceled, cause))
	}
	released := fmt.Errorf("%w: released", core.ErrLeaseEnded) // Scheduler.Release cancels running operations with it
	for _, transport := range []string{"iwdp", "direct"} {
		for _, tc := range []struct {
			cause string
			err   error
			want  string
		}{
			{"operation ended before admission (Op.active, ErrOpEnded)", fence(core.ErrOpEnded), daemon},
			{"operation ended while waiting for a send slot (cause of the operation)", fence(core.ErrOpEnded), daemon},
			{"operation ended in flight", inFlight(core.ErrOpEnded), daemon},
			{"lease ended before admission (Op.active)", fence(core.ErrLeaseEnded), daemon},
			{"lease released: operation canceled, refused", fence(released), daemon},
			{"lease released: operation canceled in flight", inFlight(released), daemon},
			{"lease epoch changed before admission", fence(core.ErrStaleEpoch), daemon},
			{"caller canceled while waiting for a send slot", fence(context.Canceled), daemon},
			{"operation deadline before admission (Op.active, bare ErrDeadline)", fence(core.ErrDeadline), hidden},
			{"operation deadline in flight", inFlight(core.ErrDeadline), hidden},
			{"probe budget while waiting for a send slot", fence(context.DeadlineExceeded), hidden},
		} {
			got := tabSelectError("t2", true, core.ConnOK, tc.err)
			msg := got.Error()
			ok := strings.Contains(msg, tc.want) && errors.Is(got, tc.err) && ExitCodeOf(got) == ExitBusy
			for _, other := range []string{closed, daemon, takeover, hidden} {
				if other != tc.want && strings.Contains(msg, other) {
					ok = false
				}
			}
			if !ok {
				t.Errorf("%s, %s: %v (exit %d); want only %q, the cause kept and exit %d", transport, tc.cause, got, ExitCodeOf(got), tc.want, ExitBusy)
			}
		}
	}
	// Before the probe (acquiring the page): a deadline or a stall gets
	// the hidden-tab advice; any other error is returned unchanged (a lost
	// page's ErrLost already names --takeover).
	for _, err := range []error{deadline, stalled} {
		if got := tabSelectError("t2", false, core.ConnOK, err); !strings.Contains(got.Error(), hidden) || ExitCodeOf(got) != ExitBusy {
			t.Errorf("before the probe, %v: %v", err, got)
		}
	}
	for _, err := range []error{&core.CodedError{Code: core.CodeBusy, Err: core.ErrLost}, webkit.ErrSessionClosed, errors.New("acquire failed")} {
		if got := tabSelectError("t2", false, core.ConnLost, err); got != err {
			t.Errorf("before the probe, %v: %v, want the error unchanged", err, got)
		}
	}
}

// Cause 1 with the fake: the daemon shuts down (Manager.Close) while the
// probe waits. The daemon ends the operation ("context canceled: the
// operation has ended"), so the advice is to run the command again, not
// --takeover and not a hidden tab.
func TestTabSelectDuringDaemonShutdown(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	stalled := f.add("Two", "http://two.test/")
	stalled.Stall("Runtime.evaluate")
	endOnProbe(t, stalled, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	_, err := runIn(t, m, "a", "tab", "t2")
	if err == nil || !strings.Contains(err.Error(), "the daemon ended the probe") || strings.Contains(err.Error(), "--takeover") || ExitCodeOf(err) != ExitBusy {
		t.Fatalf("tab t2 during the daemon's shutdown: %v (exit %d)", err, ExitCodeOf(err))
	}
}

// endOnProbe runs end once the fake page s received the probe's
// Runtime.evaluate (the first one; the fake is a local browser, so the
// connection itself sends no probe).
func endOnProbe(t *testing.T, s *fakewebkit.Server, end func()) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		for limit := time.Now().Add(5 * time.Second); time.Now().Before(limit); time.Sleep(5 * time.Millisecond) {
			if len(s.ReceivedMethod("Runtime.evaluate")) > 0 {
				end()
				return
			}
		}
	}()
}

// F6 (review of W1c-4, reviewer reproduction): a direct-transport page that
// closed carries ErrPageClosed inside ErrSessionClosed (Session.readLoop
// keeps both identities, webkit TestSessionReadErrorKeepsTransportIdentity).
// It is a closed tab, not a lost connection that needs --takeover.
func TestReviewClosedTransportPageDoesNotRecommendTakeover(t *testing.T) {
	// This is Session.readLoop's real error shape when the direct transport
	// reports application disconnection: both error identities are retained.
	err := fmt.Errorf("%w: %w: application disconnected", webkit.ErrSessionClosed, webkit.ErrPageClosed)
	for _, state := range []core.ConnState{core.ConnOK, core.ConnLost} { // watch may have marked the end lost
		got := tabSelectError("t2", true, state, err)
		if !strings.Contains(got.Error(), "tab t2 closed") || strings.Contains(got.Error(), "--takeover") || ExitCodeOf(got) != ExitBusy {
			t.Fatalf("closed direct-transport tab: %v (exit %d); want tab closed, tab list, no takeover and exit 4", got, ExitCodeOf(got))
		}
	}
}

// F7 (closure review of W1c-4 round 1, reviewer reproduction): the daemon
// ends the operation (Manager.End, as shutdown and release do) after runTab
// set probed and before the probe's Send passes the operation fence. The
// real scheduler refuses with ErrNotSent wrapping core.ErrOpEnded, without
// context.Canceled, and the page is still attached. The advice is that the
// daemon ended the probe, not a hidden tab.
func TestReviewEndedOperationBeforeProbeWrite(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mo, err := m.Begin(ctx, core.PageRequest{}, core.AcquireOptions{Session: "a"}, "tab select", core.ClassRead)
	if err != nil {
		t.Fatal(err)
	}
	// This is the shutdown interleaving after runTab sets probed=true and
	// before Send's active check. Manager.releasePage also calls m.End
	// before its cleanup and before pageConn.close changes ConnState.
	m.End(mo.ID)
	_, err = mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1", "returnByValue": true})
	state := mo.Page.ConnState()
	if !errors.Is(err, core.ErrOpEnded) || errors.Is(err, context.Canceled) || state != core.ConnOK {
		t.Fatalf("unexpected operation-end shape: state %s, err %v", state, err)
	}
	got := tabSelectError("t1", true, state, err)
	if !strings.Contains(got.Error(), "the daemon ended the probe") || strings.Contains(got.Error(), "not the visible Safari tab") || ExitCodeOf(got) != ExitBusy {
		t.Fatalf("ended operation before the probe write: %v (exit %d); want daemon-ended advice, not hidden-tab advice, and exit 4", got, ExitCodeOf(got))
	}
}
