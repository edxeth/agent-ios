package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

func dialFake(t *testing.T, f *fakewebkit.Server) func(context.Context) (transport.MessageConn, error) {
	t.Helper()
	return func(ctx context.Context) (transport.MessageConn, error) {
		return DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}
}

func newFakeSession(t *testing.T, f *fakewebkit.Server, o SessionOptions) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Dial(ctx, dialFake(t, f), o)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 7.3 test 1: Target routing with no direct fallback.
func TestSessionConnectsWhenTargetIsAnnouncedLate(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{TargetDelay: 2 * time.Second})
	defer f.Close()
	start := time.Now()
	s := newFakeSession(t, f, SessionOptions{})
	if el := time.Since(start); el < 2*time.Second {
		t.Fatalf("connected after %s, before the target was announced", el)
	}
	if _, err := s.Send(context.Background(), "Runtime.evaluate", map[string]string{"expression": "1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != 1 || got[0].Target != "page-1" {
		t.Fatalf("Runtime.evaluate records = %+v, want one routed to page-1", got)
	}
}

func TestSessionFailsAfterThreeSecondsWithoutTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{NoTarget: true})
	defer f.Close()
	conn, err := DialWebSocket(context.Background(), f.URL, transport.Limits{Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = NewSession(context.Background(), conn, SessionOptions{})
	el := time.Since(start)
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget", err)
	}
	if !strings.Contains(err.Error(), "page did not announce its target; it may be held by another inspector or not visible") {
		t.Fatalf("err = %q, want the documented message", err)
	}
	if el < 3*time.Second || el > 3500*time.Millisecond {
		t.Fatalf("failed after %s, want about 3 s", el)
	}
	for _, r := range f.Received() {
		t.Fatalf("fake received %q without a target; there is no direct fallback", r.Method)
	}
}

func TestDialRetriesOnceAfterConnectionClosedBeforeTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{CloseBeforeTarget: true})
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Dial(ctx, dialFake(t, f), SessionOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()
	if n := f.ConnectionCount(); n != 2 {
		t.Fatalf("connections = %d, want 2 (one failed attempt, one success)", n)
	}
	if _, err := s.Send(ctx, "Runtime.evaluate", nil); err != nil {
		t.Fatalf("Send after retry: %v", err)
	}
}

func TestDialGivesUpAfterTwoAttempts(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{NoTarget: true})
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Dial(ctx, dialFake(t, f), SessionOptions{TargetWait: 200 * time.Millisecond})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget", err)
	}
	if n := f.ConnectionCount(); n != 2 {
		t.Fatalf("connections = %d, want exactly 2 attempts", n)
	}
}

// seqOf returns the Seq of the first record matching pred, or 0.
func seqOf(log []fakewebkit.Record, pred func(fakewebkit.Record) bool) int {
	for _, r := range log {
		if pred(r) {
			return r.Seq
		}
	}
	return 0
}

// 7.3 test 2: provisional setup and swap.
func TestSessionSetsUpProvisionalTargetBeforeCommit(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Stall("Test.pending")
	setupMethods := []string{"Page.enable", "Page.setBootstrapScript", "Network.enable", "Network.setInterceptionEnabled", "Network.addInterception"}
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		for _, m := range setupMethods {
			if _, err := tc.Send(ctx, m, map[string]string{}); err != nil {
				return err
			}
		}
		return nil
	}})
	if g := s.Generation(); g != 1 {
		t.Fatalf("initial generation = %d, want 1", g)
	}

	pendingErr := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), "Test.pending", nil)
		pendingErr <- err
	}()
	waitFor(t, func() bool { return len(f.ReceivedMethod("Test.pending")) == 1 })

	f.Swap("page-2", 200*time.Millisecond)

	select {
	case err := <-pendingErr:
		if !errors.Is(err, ErrTargetSwapped) {
			t.Fatalf("pending call err = %v, want ErrTargetSwapped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call on the old target did not fail at commit")
	}

	log := f.Log()
	commit := seqOf(log, func(r fakewebkit.Record) bool { return r.Method == "Target.didCommitProvisionalTarget" })
	for _, m := range setupMethods {
		seq := seqOf(log, func(r fakewebkit.Record) bool { return r.In && r.Method == m && r.Target == "page-2" })
		if seq == 0 || seq > commit {
			t.Fatalf("%s to page-2 at seq %d, commit at %d: setup must reach the provisional target before commit", m, seq, commit)
		}
	}
	if a := s.ActiveTarget(); a != "page-2" {
		t.Fatalf("active target = %q, want page-2", a)
	}
	if _, err := s.Send(context.Background(), "Runtime.evaluate", nil); err != nil {
		t.Fatalf("Send after swap: %v", err)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != 1 || got[0].Target != "page-2" {
		t.Fatalf("next call records = %+v, want one to page-2", got)
	}

	// Commit and the main-frame frameNavigated of the same document count once.
	if g := s.Generation(); g != 2 {
		t.Fatalf("generation after commit = %d, want 2", g)
	}
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "f1", "loaderId": "L1"}})
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "f2", "parentId": "f1", "loaderId": "L9"}})
	f.Emit("Page.navigatedWithinDocument", map[string]any{"frameId": "f1", "url": "http://a/#x"})
	syncEvents(t, s, f)
	if g := s.Generation(); g != 2 {
		t.Fatalf("generation after the commit's frameNavigated, a child frame and a same-document navigation = %d, want 2", g)
	}
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "f1", "loaderId": "L2"}})
	syncEvents(t, s, f)
	if g := s.Generation(); g != 3 {
		t.Fatalf("generation after a same-process navigation = %d, want 3", g)
	}
}

// syncEvents waits until every event the fake emitted so far has passed the
// session's read loop: the read loop handles messages in order, so a reply
// that arrives after the events proves it.
func syncEvents(t *testing.T, s *Session, f *fakewebkit.Server) {
	t.Helper()
	if _, err := s.Send(context.Background(), "Test.sync", nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestSessionDropsAbandonedProvisionalTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	lateErr := make(chan error, 1)
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		if _, err := tc.Send(ctx, "Page.enable", nil); err != nil {
			return err
		}
		<-ctx.Done() // canceled when the target is destroyed
		_, err := tc.Send(context.Background(), "Page.setBootstrapScript", map[string]string{"source": "x"})
		lateErr <- err
		return err
	}})
	f.AbortProvisional("page-9", 100*time.Millisecond)
	select {
	case err := <-lateErr:
		if !errors.Is(err, ErrTargetAbandoned) {
			t.Fatalf("send after destroy err = %v, want ErrTargetAbandoned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("setup was not canceled by the destroy")
	}
	log := f.Log()
	destroyed := seqOf(log, func(r fakewebkit.Record) bool { return r.Method == "Target.targetDestroyed" })
	for _, r := range log {
		if r.In && r.Target == "page-9" && r.Seq > destroyed {
			t.Fatalf("abandoned target received %s after its destroy", r.Method)
		}
	}
	if a := s.ActiveTarget(); a != "page-1" {
		t.Fatalf("active target = %q, want page-1 (the abandoned target is dropped)", a)
	}
	if _, err := s.Send(context.Background(), "Runtime.evaluate", nil); err != nil {
		t.Fatalf("Send after abandoned swap: %v", err)
	}
	if g := s.Generation(); g != 1 {
		t.Fatalf("generation = %d, want 1", g)
	}
}

func TestSessionResumesPausedTargetOnEveryPath(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ctx context.Context, tc TargetCaller) error
		after func(s *Session)
	}{
		{"success", func(ctx context.Context, tc TargetCaller) error {
			_, err := tc.Send(ctx, "Page.enable", nil)
			return err
		}, nil},
		{"error", func(ctx context.Context, tc TargetCaller) error {
			return errors.New("setup failed")
		}, nil},
		{"setup too slow", func(ctx context.Context, tc TargetCaller) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil},
		{"close", func(ctx context.Context, tc TargetCaller) error {
			<-ctx.Done()
			return ctx.Err()
		}, func(s *Session) { _ = s.Close() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			defer f.Close()
			resumeAfter := 5 * time.Second
			if tc.name == "setup too slow" {
				resumeAfter = 300 * time.Millisecond
			}
			s := newFakeSession(t, f, SessionOptions{PauseOnStart: true, ResumeAfter: resumeAfter, Setup: tc.setup})
			if got := f.ReceivedMethod("Target.setPauseOnStart"); len(got) != 1 {
				t.Fatalf("Target.setPauseOnStart sent %d times, want 1", len(got))
			}
			swapped := make(chan struct{})
			go func() { f.Swap("page-2", 0); close(swapped) }()
			if tc.after != nil {
				waitFor(t, func() bool { return len(s.PausedTargets()) == 1 })
				tc.after(s)
			}
			select {
			case <-swapped:
			case <-time.After(3 * time.Second):
				t.Fatal("the paused target was never resumed")
			}
			res := f.ReceivedMethod("Target.resume")
			if len(res) != 1 || !strings.Contains(string(res[0].Params), "page-2") {
				t.Fatalf("Target.resume records = %+v, want exactly one for page-2", res)
			}
		})
	}
}

// 7.3 test 5: nudge.
func TestSessionNudgeReleasesWithheldReply(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Withhold("Test.slow")
	s := newFakeSession(t, f, SessionOptions{Nudge: true})
	start := time.Now()
	if _, err := s.Send(context.Background(), "Test.slow", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("call took %s, want at most 300 ms with the nudge", el)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != 1 || !strings.Contains(string(got[0].Params), `"0"`) {
		t.Fatalf("nudge records = %+v, want one Runtime.evaluate \"0\"", got)
	}
}

func TestSessionWithoutNudgeTimesOutOnWithheldReply(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Withhold("Test.slow")
	s := newFakeSession(t, f, SessionOptions{CallTimeout: 400 * time.Millisecond})
	_, err := s.Send(context.Background(), "Test.slow", nil)
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v, want ErrCallTimeout", err)
	}
}

func TestSessionCallDeadlineIsMinOfBudgetAndContext(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Stall("Test.stalled")
	s := newFakeSession(t, f, SessionOptions{CallTimeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Send(ctx, "Test.stalled", nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("returned after %s, want at the 300 ms deadline", el)
	}
}

// 7.3 test 10: event routing and bad replies.
func TestSessionRepliesArriveWhileSubscriberBlocks(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{EventQueue: 64})
	block := make(chan struct{})
	defer close(block)
	s.Subscribe("Test.flood", func(string, json.RawMessage) { <-block })
	var other sync.WaitGroup
	other.Add(1)
	var once sync.Once
	s.Subscribe("Test.other", func(string, json.RawMessage) { once.Do(other.Done) })

	for i := 0; i < 2000; i++ {
		f.Emit("Test.flood", map[string]int{"i": i})
	}
	f.Emit("Test.other", nil)
	start := time.Now()
	if _, err := s.Send(context.Background(), "Test.ping", nil); err != nil {
		t.Fatalf("Send during flood: %v", err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("reply took %s during an event flood, want under 50 ms", el)
	}
	other.Wait() // a blocked subscriber does not block other subscribers
	if s.DroppedEvents() == 0 {
		t.Fatal("DroppedEvents = 0, want the overflow of the blocked subscriber counted")
	}
}

func TestSessionSurvivesBadReplies(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Malformed("Test.wrongShape", `{"id":%d,"result":"not an object"}`)
	f.Malformed("Test.empty", `{"id":%d}`)
	f.Malformed("Test.badError", `{"id":%d,"error":"boom"}`)
	f.Malformed("Test.unparsable", `{"id":%d,`)
	f.Unsupported("Input.dispatchMouseEvent")
	s := newFakeSession(t, f, SessionOptions{CallTimeout: 300 * time.Millisecond})
	ctx := context.Background()
	for _, m := range []string{"Test.wrongShape", "Test.empty", "Test.badError"} {
		if _, err := s.Send(ctx, m, nil); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%s err = %v, want a malformed-reply error", m, err)
		}
	}
	if _, err := s.Send(ctx, "Test.unparsable", nil); !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("unparsable reply err = %v, want ErrCallTimeout (the reply cannot be matched)", err)
	}
	if s.MalformedMessages() != 1 {
		t.Fatalf("MalformedMessages = %d, want 1", s.MalformedMessages())
	}
	_, err := s.Send(ctx, "Input.dispatchMouseEvent", nil)
	var pe *ErrorData
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "'Input.dispatchMouseEvent' was not found") {
		t.Fatalf("unsupported method err = %v, want the protocol error", err)
	}
	if _, err := s.Send(ctx, "Runtime.evaluate", nil); err != nil {
		t.Fatalf("session did not survive bad replies: %v", err)
	}
}

func TestSessionLargeReplyPasses(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	big := strings.Repeat("a", 20<<20)
	f.Result("Test.big", map[string]string{"v": big})
	// The budget covers the fake's JSON-in-JSON encoding of 20 MiB, which
	// takes more than the default 10 s under the race detector.
	s := newFakeSession(t, f, SessionOptions{CallTimeout: time.Minute})
	raw, err := s.Send(context.Background(), "Test.big", nil)
	if err != nil {
		t.Fatalf("20 MiB reply: %v", err)
	}
	var out struct{ V string }
	if err := json.Unmarshal(raw, &out); err != nil || out.V != big {
		t.Fatalf("20 MiB reply did not round-trip (err %v, len %d)", err, len(out.V))
	}
}

func TestSessionPageClosedFailsPendingCalls(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Stall("Test.pending")
	s := newFakeSession(t, f, SessionOptions{})
	errc := make(chan error, 1)
	go func() { _, err := s.Send(context.Background(), "Test.pending", nil); errc <- err }()
	waitFor(t, func() bool { return len(f.ReceivedMethod("Test.pending")) == 1 })
	f.DestroyActive()
	if err := <-errc; !errors.Is(err, ErrPageClosed) {
		t.Fatalf("err = %v, want ErrPageClosed", err)
	}
	if _, err := s.Send(context.Background(), "Runtime.evaluate", nil); !errors.Is(err, ErrPageClosed) {
		t.Fatalf("Send after close err = %v, want ErrPageClosed", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var _ = fmt.Sprint

// Design 2.7 rule 3 and 2.10: a Target.resume that was never written does
// not clear the paused state, so Close (and the release barrier) still
// resume the target.
func TestResumeTargetFailedWriteKeepsTargetPaused(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{PauseOnStart: true, ResumeAfter: time.Minute, Setup: func(ctx context.Context, tc TargetCaller) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	go f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.IsPaused("page-2") })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ResumeTarget(canceled, "page-2"); err == nil {
		t.Fatal("Target.resume with a canceled context succeeded")
	}
	if !s.IsPaused("page-2") || len(s.PausedTargets()) != 1 {
		t.Fatal("a Target.resume that was never written cleared the paused state")
	}
	if n := len(f.ReceivedMethod("Target.resume")); n != 0 {
		t.Fatalf("fake received %d Target.resume from a canceled context", n)
	}
	_ = s.Close()
	if res := f.ReceivedMethod("Target.resume"); len(res) != 1 || !strings.Contains(string(res[0].Params), "page-2") {
		t.Fatalf("Target.resume records after Close = %+v, want one for page-2", res)
	}
}

// Design 2.7 rule 2: a provisional setup that fails still resumes the
// target, and its error stays observable on the committed target until the
// desired state is applied again.
func TestSessionSurfacesProvisionalSetupFailure(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Unsupported("Page.setBootstrapScript")
	s := newFakeSession(t, f, SessionOptions{PauseOnStart: true, Setup: func(ctx context.Context, tc TargetCaller) error {
		_, err := tc.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": "x"})
		return err
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ActiveSetup(ctx); err != nil {
		t.Fatalf("initial target setup error = %v, want nil", err)
	}
	f.Swap("page-2", 0)
	if n := len(f.ReceivedMethod("Target.resume")); n != 1 {
		t.Fatalf("Target.resume sent %d times after a failed setup, want 1", n)
	}
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	err := s.ActiveSetup(ctx)
	var se *SetupError
	if !errors.As(err, &se) || se.Target != "page-2" || !strings.Contains(err.Error(), "Page.setBootstrapScript") {
		t.Fatalf("ActiveSetup after the commit = %v, want the setup failure of page-2", err)
	}
	f.Result("Page.setBootstrapScript", map[string]any{})
	if err := s.ReapplySetup(ctx); err != nil {
		t.Fatalf("ReapplySetup: %v", err)
	}
	if err := s.ActiveSetup(ctx); err != nil {
		t.Fatalf("ActiveSetup after a successful reapply = %v, want nil", err)
	}
	if got := f.ReceivedMethod("Page.setBootstrapScript"); len(got) != 2 || got[1].Target != "page-2" {
		t.Fatalf("setBootstrapScript records = %+v, want the reapply on page-2", got)
	}
}

// Subscriber workers end with the session: Close stops every worker, and a
// subscription made after Close starts none.
func TestSessionCloseStopsSubscriberWorkers(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	before := runtime.NumGoroutine()
	const n = 50
	for i := 0; i < n; i++ {
		s.OnEvent("Test.event", func(string, json.RawMessage) {})
	}
	if g := runtime.NumGoroutine(); g < before+n {
		t.Fatalf("goroutines = %d after %d subscriptions, want at least %d", g, n, before+n)
	}
	_ = s.Close()
	waitFor(t, func() bool { return runtime.NumGoroutine() < before })
	after := runtime.NumGoroutine()
	unsub := s.Subscribe("Test.event", func(string, json.RawMessage) {})
	defer unsub()
	time.Sleep(20 * time.Millisecond)
	if g := runtime.NumGoroutine(); g > after {
		t.Fatalf("goroutines = %d after a subscription on a closed session, want at most %d", g, after)
	}
}
