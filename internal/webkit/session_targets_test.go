package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W1c: every event reaches its subscribers with its source target. Oracle:
// the targetId of the Target.dispatchMessageFromTarget that the fake sent
// (WebKit names the sender there); a provisional target keeps its id after
// its commit; events of the connection itself (Target domain) have "".

type sourced struct{ target, method string }

type sourceLog struct {
	mu  sync.Mutex
	got []sourced
}

func (l *sourceLog) add(target, method string, _ json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, sourced{target, method})
}

func (l *sourceLog) events() []sourced {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sourced(nil), l.got...)
}

// syncOn is syncEvents with a sync call to target, a live target that the
// session knows: a commit that the read loop has not handled yet cannot
// fail it.
func syncOn(t *testing.T, s *Session, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.SendControl(WithTarget(ctx, target), "Test.sync", nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestSessionDeliversEachEventWithItsSourceTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	// NewSession returns when the read loop has recorded the initial
	// Target.targetCreated, which it dispatches afterwards. The sync reply
	// comes after that dispatch, so the observers below see only the
	// events that the test makes.
	syncOn(t, s, "page-1")
	var queued, inLoop sourceLog
	var legacy []string
	var legacyMu sync.Mutex
	s.SubscribeTarget(AllEvents, queued.add)
	s.SubscribeTargetSync(AllEvents, inLoop.add)
	s.Subscribe("Test.b", func(method string, _ json.RawMessage) {
		legacyMu.Lock()
		legacy = append(legacy, method)
		legacyMu.Unlock()
	})

	f.EmitFrom("page-1", "Test.a", map[string]any{})
	f.Provisional("page-2")
	syncOn(t, s, "page-1") // the session knows page-2
	f.EmitFrom("page-2", "Test.b", map[string]any{})
	f.Commit("page-2")
	f.EmitFrom("page-2", "Test.c", map[string]any{})
	syncOn(t, s, "page-2")

	want := []sourced{
		{"page-1", "Test.a"},
		{"", "Target.targetCreated"},
		{"page-2", "Test.b"},
		{"", "Target.didCommitProvisionalTarget"},
		{"", "Target.targetDestroyed"},
		{"page-2", "Test.c"},
	}
	if got := inLoop.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SubscribeTargetSync got %v, want %v", got, want)
	}
	waitFor(t, func() bool { return len(queued.events()) == len(want) })
	if got := queued.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SubscribeTarget got %v, want %v", got, want)
	}
	// Subscribe delivers through its own queue too.
	waitFor(t, func() bool {
		legacyMu.Lock()
		defer legacyMu.Unlock()
		return len(legacy) > 0
	})
	legacyMu.Lock()
	defer legacyMu.Unlock()
	if !reflect.DeepEqual(legacy, []string{"Test.b"}) {
		t.Fatalf("Subscribe got %v, want [Test.b]: the existing API keeps working", legacy)
	}
}

// SubscribeTarget on a connection that does not know event sources reports
// "" (the connection's own target).
func TestSubscribeTargetWithoutSourcesReportsTheConnection(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	c, err := NewClient(context.Background(), f.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	var l sourceLog
	SubscribeTarget(c, "Test.a", l.add)
	f.EmitFrom("page-1", "Test.a", map[string]any{})
	waitFor(t, func() bool { return len(l.events()) == 1 })
	if got := l.events(); got[0] != (sourced{"", "Test.a"}) {
		t.Fatalf("got %v, want the event with target \"\"", got)
	}
}

// Design 2.10: a control-lane message reaches the target that WithTarget
// names, a provisional target included (a held request is answered on the
// target that holds it).
func TestSendControlAddressesWithTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	f.Provisional("page-2")
	syncOn(t, s, "page-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.SendControl(WithTarget(ctx, "page-2"), "Test.control", nil); err != nil {
		t.Fatalf("SendControl to page-2: %v", err)
	}
	if _, err := s.SendControl(ctx, "Test.control", nil); err != nil {
		t.Fatalf("SendControl to the active target: %v", err)
	}
	var targets []string
	for _, r := range f.ReceivedMethod("Test.control") {
		targets = append(targets, r.Target)
	}
	if want := []string{"page-2", "page-1"}; !reflect.DeepEqual(targets, want) {
		t.Fatalf("Test.control went to %v, want %v", targets, want)
	}
	f.DestroyTarget("page-2")
	syncEvents(t, s, f)
	if _, err := s.SendControl(WithTarget(ctx, "page-2"), "Test.control", nil); !errors.Is(err, ErrTargetAbandoned) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("SendControl to the abandoned page-2: err = %v, want ErrTargetAbandoned and ErrNotSent", err)
	}
	// The liveness probe asks the active target, whatever target the
	// context of the call that preceded it addressed.
	if err := s.Probe(WithTarget(ctx, "page-2")); err != nil {
		t.Fatalf("Probe with a context addressed to the abandoned page-2: %v", err)
	}
}

func contextEvent(id int, frame string) map[string]any {
	return map[string]any{"context": map[string]any{"id": id, "type": "normal", "name": "", "frameId": frame}}
}

// W3b review P1: the session records the contexts that a provisional
// target reports during its setup, before any consumer subscribes, so a
// consumer that subscribes after the commit still finds them.
func TestExecutionContextsOfProvisionalSetupBeforeAnySubscriber(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	// Like WebKit, Runtime.enable reports the existing contexts before its reply.
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", contextEvent(7, "F-main"))
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", contextEvent(8, "F-child"))
		return nil, nil
	})
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		_, err := tc.Send(ctx, "Runtime.enable", nil)
		return err
	}})
	f.Provisional("page-2")
	waitFor(t, func() bool { return len(f.ReceivedMethod("Runtime.enable")) == 1 })
	f.Commit("page-2")
	syncOn(t, s, "page-2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ActiveSetup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A consumer that subscribes now has missed the events ...
	var late sourceLog
	s.SubscribeTarget("Runtime.executionContextCreated", late.add)
	syncOn(t, s, "page-2")
	if n := len(late.events()); n != 0 {
		t.Fatalf("a late subscriber got %d context events; the test needs the setup to run before it", n)
	}
	// ... and finds them in the inventory, with their frames.
	want := []ExecutionContext{{ID: 7, FrameID: "F-main", Type: "normal"}, {ID: 8, FrameID: "F-child", Type: "normal"}}
	if got := s.ExecutionContexts("page-2"); !reflect.DeepEqual(got, want) {
		t.Fatalf("ExecutionContexts(page-2) = %+v, want %+v", got, want)
	}
	if got := s.ExecutionContexts("page-1"); len(got) != 0 {
		t.Fatalf("ExecutionContexts(page-1) = %+v, want none: contexts belong to their own target", got)
	}
}

// The inventory drops contexts that WebKit's own frontend drops: WIP has no
// event for an ended context (Runtime.json defines only
// executionContextCreated), so a frame's contexts end when the frame
// detaches or navigates, and every context of the target ends with a
// main-frame navigation (WebInspectorUI NetworkManager.frameDidNavigate,
// frameDidDetach; Frame.commitProvisionalLoad). The CDP events
// executionContextDestroyed and executionContextsCleared are applied too.
func TestExecutionContextsFollowFramesAndContextEvents(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	ids := func() []int {
		syncEvents(t, s, f)
		var out []int
		for _, c := range s.ExecutionContexts("page-1") {
			out = append(out, c.ID)
		}
		return out
	}
	expect := func(step string, want ...int) {
		t.Helper()
		if got := ids(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: contexts %v, want %v", step, got, want)
		}
	}
	created := func(id int, frame string) {
		f.EmitFrom("page-1", "Runtime.executionContextCreated", contextEvent(id, frame))
	}
	created(1, "main")
	created(2, "child")
	created(3, "child")
	created(2, "child") // a duplicate report (iOS 16 script-less subframes)
	expect("created", 1, 2, 3)
	f.EmitFrom("page-1", "Page.frameDetached", map[string]any{"frameId": "child"})
	expect("child frame detached", 1)
	created(4, "child2")
	f.EmitFrom("page-1", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "child2", "parentId": "main", "loaderId": "L2"}})
	expect("child frame navigated", 1)
	created(5, "main")
	f.EmitFrom("page-1", "Runtime.executionContextDestroyed", map[string]any{"executionContextId": 1})
	expect("destroyed", 5)
	f.EmitFrom("page-1", "Runtime.executionContextsCleared", map[string]any{})
	expect("cleared")
	created(6, "main")
	created(7, "child")
	f.EmitFrom("page-1", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "L3"}})
	expect("main frame navigated")
	created(8, "main")
	f.EmitFrom("page-1", "Runtime.executionContextCreated", "not an object")               // ignored
	f.EmitFrom("page-1", "Page.frameNavigated", map[string]any{"frame": map[string]any{}}) // no frame id: ignored
	expect("malformed events ignored", 8)
}

// The validating fake: a completion takes a hold only on the target that
// holds the request and at its stage; a target's holds end with the
// target and with its Network.disable (WebKit InspectorNetworkAgent).
func TestFakeHoldsRequestsPerTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cont := func(target, id, stage string) error {
		_, err := s.SendControl(WithTarget(ctx, target), "Network.interceptContinue", map[string]string{"requestId": id, "stage": stage})
		return err
	}
	missing := func(err error) bool {
		var pe *ErrorData
		return errors.As(err, &pe) && (pe.Message == "Missing pending intercept request for given requestId" || pe.Message == "Missing pending intercept response for given requestId")
	}
	f.Provisional("page-2")
	f.Intercept("page-2", "r1", "request")
	f.Intercept("page-1", "r2", "response")
	f.Intercept("page-1", "r0", "request")
	syncEvents(t, s, f)
	if got := f.HeldRequests("page-1"); !reflect.DeepEqual(got, []string{"r0", "r2"}) {
		t.Fatalf("page-1 holds %v, want [r0 r2]", got)
	}
	send := func(method, id string, params map[string]any) error {
		params["requestId"] = id
		_, err := s.SendControl(WithTarget(ctx, "page-1"), method, params)
		return err
	}
	// The fixed-stage completions take only a hold at their stage.
	if err := send("Network.interceptWithRequest", "r2", map[string]any{}); !missing(err) {
		t.Fatalf("interceptWithRequest of the response-stage r2: err = %v, want Missing pending intercept", err)
	}
	if err := send("Network.interceptWithResponse", "r2", map[string]any{"content": "", "base64Encoded": false}); err != nil {
		t.Fatalf("interceptWithResponse of r2: %v", err)
	}
	if err := cont("page-1", "r1", "request"); !missing(err) {
		t.Fatalf("continue of r1 on page-1: err = %v, want Missing pending intercept", err)
	}
	if err := cont("page-2", "r1", "response"); !missing(err) {
		t.Fatalf("continue of r1 at the response stage: err = %v, want Missing pending intercept", err)
	}
	if err := cont("page-2", "r1", "request"); err != nil {
		t.Fatalf("continue of r1 on page-2: %v", err)
	}
	if err := cont("page-2", "r1", "request"); !missing(err) {
		t.Fatalf("second continue of r1: err = %v, want Missing pending intercept", err)
	}
	want := []fakewebkit.Answer{
		{Target: "page-1", RequestID: "r2", Stage: "response", Method: "Network.interceptWithResponse"},
		{Target: "page-2", RequestID: "r1", Stage: "request", Method: "Network.interceptContinue"},
	}
	if got := f.Answers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers = %+v, want %+v", got, want)
	}
	if _, err := s.SendControl(ctx, "Network.disable", nil); err != nil {
		t.Fatalf("Network.disable: %v", err)
	}
	if held := f.HeldRequests("page-1"); len(held) != 0 {
		t.Fatalf("page-1 holds %v after Network.disable, want none", held)
	}
	f.Intercept("page-2", "r3", "request")
	f.DestroyTarget("page-2")
	if held := f.HeldRequests("page-2"); len(held) != 0 {
		t.Fatalf("destroyed page-2 holds %v", held)
	}
}
