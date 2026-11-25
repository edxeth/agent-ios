package webkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// marshalHook runs fn while Send marshals the command's params: after every
// check the caller made before Send, and before Send chooses the target.
type marshalHook struct{ fn func() }

func (h marshalHook) MarshalJSON() ([]byte, error) {
	h.fn()
	return []byte(`{}`), nil
}

// failingSetupSession returns a session whose provisional setup fails
// (Page.setBootstrapScript is unsupported) and a function that runs a swap
// to page-2 and returns once page-2 is the active target, its setup failed.
func failingSetupSession(t *testing.T) (*fakewebkit.Server, *Session, func()) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Unsupported("Page.setBootstrapScript")
	s := newFakeSession(t, f, SessionOptions{PauseOnStart: true, Setup: func(ctx context.Context, tc TargetCaller) error {
		_, err := tc.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": "x"})
		return err
	}})
	swap := func() {
		f.Swap("page-2", 0)
		waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	}
	return f, s, swap
}

// Design 2.7 (review round 3, finding 1): the choice of the target and the
// check of its setup are one step. A swap to a target whose setup failed
// that commits after the caller checked ActiveSetup and before Send chose
// the target still sends nothing to the new target: Send fails as not sent
// with the setup failure.
func TestSessionOrdinarySendNotRoutedToFailedSetupTarget(t *testing.T) {
	f, s, swap := failingSetupSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ActiveSetup(ctx); err != nil {
		t.Fatalf("ActiveSetup before the swap = %v, want nil", err)
	}
	_, err := s.Send(ctx, "Test.x", marshalHook{swap})
	var se *SetupError
	if !errors.Is(err, ErrNotSent) || !errors.As(err, &se) || se.Target != "page-2" {
		t.Fatalf("Send err = %v, want not sent with the setup failure of page-2", err)
	}
	if got := f.ReceivedMethod("Test.x"); len(got) != 0 {
		t.Fatalf("fake received %+v, want nothing", got)
	}
}

// Design 2.7 and 2.10: control-lane messages (SendControl), the liveness
// probe and setup traffic (ReapplySetup, see
// TestSessionSurfacesProvisionalSetupFailure) are not held by the setup
// gate: held requests, pauses and recovery must complete on a target whose
// setup failed. Ordinary commands to it are refused.
func TestSessionControlTrafficBypassesSetupGate(t *testing.T) {
	f, s, swap := failingSetupSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	swap()
	if _, err := s.Send(ctx, "Test.ordinary", nil); !errors.Is(err, ErrNotSent) || !errors.As(err, new(*SetupError)) {
		t.Fatalf("ordinary Send err = %v, want not sent with the setup failure", err)
	}
	if _, err := s.SendControl(ctx, "Debugger.resume", nil); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if err := s.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := f.ReceivedMethod("Test.ordinary"); len(got) != 0 {
		t.Fatalf("fake received %+v, want no ordinary command", got)
	}
	if got := f.ReceivedMethod("Debugger.resume"); len(got) != 1 || got[0].Target != "page-2" {
		t.Fatalf("Debugger.resume records = %+v, want one on page-2", got)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != 1 || got[0].Target != "page-2" {
		t.Fatalf("probe records = %+v, want one on page-2", got)
	}
}

// Design 2.7: without pause on start a target can commit while its setup
// still runs. An ordinary command waits for that setup, at most until its
// context ends (then it is not sent), and is sent once the setup succeeded.
func TestSessionOrdinarySendWaitsForRunningSetup(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	release := make(chan struct{})
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })

	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if _, err := s.Send(short, "Test.early", nil); !errors.Is(err, ErrNotSent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send during the setup err = %v, want not sent at the context deadline", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := s.Send(ctx, "Test.late", nil)
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if got := f.ReceivedMethod("Test.late"); len(got) != 0 {
		t.Fatalf("fake received %+v while the setup runs", got)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatalf("Send after the setup succeeded: %v", err)
	}
	if got := f.ReceivedMethod("Test.early"); len(got) != 0 {
		t.Fatalf("fake received %+v, want no command sent during the setup", got)
	}
	if got := f.ReceivedMethod("Test.late"); len(got) != 1 || got[0].Target != "page-2" {
		t.Fatalf("Test.late records = %+v, want one on page-2", got)
	}
}

// An ordinary command that waits for a running setup is not sent when the
// session closes meanwhile.
func TestSessionOrdinarySendDuringSetupNotSentWhenSessionCloses(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := s.Send(ctx, "Test.x", nil)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = s.Close()
	if err := <-errc; !errors.Is(err, ErrNotSent) || !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send err = %v, want not sent because the session closed", err)
	}
	if got := f.ReceivedMethod("Test.x"); len(got) != 0 {
		t.Fatalf("fake received %+v", got)
	}
}
