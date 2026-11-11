package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W1c: held requests are tracked per target and answered on their own
// target. WebKit holds an intercepted request in the backend of the target
// that reported it (Target.dispatchMessageFromTarget names it); during a
// cross-site navigation that is the provisional target, which keeps its id
// when it commits (WebKitGTK and WebKit main: WebPageInspectorController
// didCommitProvisionalTarget; docs/notes/w1b.md, W1c). A completion sent to
// another target gets "Missing pending intercept ..." and the request stays
// paused, so the new page hangs.
//
// Oracle: the fake's model of WebKit's pending intercepts per target
// (fakewebkit Intercept, HeldRequests, Answers), not the scheduler.

// syncPage waits until every event that the fake sent so far has passed the
// session's read loop: the fake writes its events before the reply. The
// sync call goes to target, a live target that the session already knows,
// so that a commit that the read loop has not handled yet cannot fail it.
func syncPage(t *testing.T, sess *webkit.Session, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.SendControl(webkit.WithTarget(ctx, target), "Test.sync", nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func completeOn(target, id string) ControlMsg {
	return ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, TargetID: target, RequestID: id, Method: "Network.interceptContinue"}
}

// completionsSent counts the completion commands that the fake received.
func completionsSent(f *fakewebkit.Server) int {
	n := 0
	for _, r := range f.Received() {
		if isInterceptCompletion(r.Method) {
			n++
		}
	}
	return n
}

// The P1: a request paused on a provisional target is completed on that
// target, before its commit and after it.
func TestProvisionalHoldIsCompletedOnItsTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	h.f.Intercept("page-2", "r1", "request")
	h.f.Intercept("page-2", "r2", "request")
	h.f.Intercept("page-2", "r3", "response")
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 3 })

	// Before the commit: the active target is page-1.
	if _, err := h.s.Control(ctx, completeOn("page-2", "r1")); err != nil {
		t.Fatalf("completion of r1 before the commit: %v", err)
	}
	h.f.Commit("page-2")
	waitUntil(t, "the commit", func() bool { return h.sess.ActiveTarget() == "page-2" })
	// After the commit: by its target id, and as the active target ("").
	if _, err := h.s.Control(ctx, completeOn("page-2", "r2")); err != nil {
		t.Fatalf("completion of r2 after the commit: %v", err)
	}
	if _, err := h.s.Control(ctx, completeOn("", "r3")); err != nil {
		t.Fatalf("completion of r3 after the commit, on the active target: %v", err)
	}
	want := []fakewebkit.Answer{
		{Target: "page-2", RequestID: "r1", Stage: "request", Method: "Network.interceptContinue"},
		{Target: "page-2", RequestID: "r2", Stage: "request", Method: "Network.interceptContinue"},
		{Target: "page-2", RequestID: "r3", Stage: "response", Method: "Network.interceptContinue"},
	}
	if got := h.f.Answers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers = %+v, want %+v", got, want)
	}
	if held := h.f.HeldRequests("page-2"); len(held) != 0 {
		t.Fatalf("page-2 still holds %v", held)
	}
	if n := completionsSent(h.f); n != 3 {
		t.Fatalf("%d completions sent, want 3 (none to a target that does not hold the request)", n)
	}
	if p := h.s.PendingHolds(); len(p) != 0 {
		t.Fatalf("pending holds = %v, want none", p)
	}
}

// A target's destruction drops its holds: WebKit destroyed them with the
// target. An abandoned provisional target, and the target that a commit
// replaced, leave nothing for a completion or the release barrier.
func TestDestroyedTargetDropsItsHolds(t *testing.T) {
	t.Run("abandoned provisional target", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		ctx, _ := timeoutCtx(t, 10*time.Second)
		h.f.Provisional("page-2")
		h.f.Intercept("page-2", "r1", "request")
		h.f.Intercept("page-1", "r2", "request")
		waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
		h.f.DestroyTarget("page-2")
		syncPage(t, h.sess, "page-1")
		if got, want := h.s.PendingHolds(), []HeldRequest{{Target: "page-1", RequestID: "r2"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pending holds after the abort = %+v, want %+v", got, want)
		}
		if _, err := h.s.Control(ctx, completeOn("page-2", "r1")); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("completion for the abandoned target: err = %v, want ErrControlRefused", err)
		}
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		want := []fakewebkit.Answer{{Target: "page-1", RequestID: "r2", Stage: "request", Method: "Network.interceptContinue"}}
		if got := h.f.Answers(); !reflect.DeepEqual(got, want) {
			t.Fatalf("answers = %+v, want %+v", got, want)
		}
		if n := completionsSent(h.f); n != 1 {
			t.Fatalf("%d completions sent, want 1 (nothing for the destroyed target)", n)
		}
	})
	t.Run("target replaced by a commit", func(t *testing.T) {
		h := newHarness(t, SchedulerOptions{})
		ctx, _ := timeoutCtx(t, 10*time.Second)
		h.f.Intercept("page-1", "r1", "request")
		h.f.Provisional("page-2")
		h.f.Intercept("page-2", "r2", "response")
		waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
		h.f.Commit("page-2")
		syncPage(t, h.sess, "page-2")
		if got, want := h.s.PendingHolds(), []HeldRequest{{Target: "page-2", RequestID: "r2"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pending holds after the commit = %+v, want %+v", got, want)
		}
		if err := h.s.Release(ctx, nil); err != nil {
			t.Fatalf("Release: %v", err)
		}
		want := []fakewebkit.Answer{{Target: "page-2", RequestID: "r2", Stage: "response", Method: "Network.interceptContinue"}}
		if got := h.f.Answers(); !reflect.DeepEqual(got, want) {
			t.Fatalf("answers = %+v, want %+v", got, want)
		}
		if n := completionsSent(h.f); n != 1 {
			t.Fatalf("%d completions sent, want 1", n)
		}
	})
}

// Design 2.10: the release barrier answers every live hold, each on its own
// target and at its own stage.
func TestReleaseAnswersEachHoldOnItsTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	h.f.Provisional("page-2")
	h.f.Intercept("page-2", "r2", "response")
	h.f.Intercept("page-2", "r3", "request")
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 3 })
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, target := range []string{"page-1", "page-2"} {
		if held := h.f.HeldRequests(target); len(held) != 0 {
			t.Fatalf("%s still holds %v after the release", target, held)
		}
	}
	got := map[string]fakewebkit.Answer{}
	for _, a := range h.f.Answers() {
		got[a.RequestID] = a
	}
	want := map[string]fakewebkit.Answer{
		"r1": {Target: "page-1", RequestID: "r1", Stage: "request", Method: "Network.interceptContinue"},
		"r2": {Target: "page-2", RequestID: "r2", Stage: "response", Method: "Network.interceptContinue"},
		"r3": {Target: "page-2", RequestID: "r3", Stage: "request", Method: "Network.interceptContinue"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("answers = %+v, want %+v", got, want)
	}
	if n := completionsSent(h.f); n != 3 {
		t.Fatalf("%d completions sent, want 3", n)
	}
}

// Design 2.4 step 2: the Network reinitialization barrier continues each
// hold on its own target, and a later completion of one fails with
// ErrRequestReset on that target only.
func TestNetworkResetAnswersEachHoldOnItsTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	h.f.Provisional("page-2")
	h.f.Intercept("page-2", "r2", "request")
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
	op := h.begin(t, ctx, ClassExclusive)
	defer op.End()
	if err := NetworkReset(ctx, h.s, op.Caller(), NewDesiredState()); err != nil {
		t.Fatalf("NetworkReset: %v", err)
	}
	got := map[string]string{}
	for _, a := range h.f.Answers() {
		got[a.RequestID] = a.Target
	}
	if want := map[string]string{"r1": "page-1", "r2": "page-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("answered on = %v, want %v", got, want)
	}
	if _, err := h.s.Control(ctx, completeOn("page-2", "r2")); !errors.Is(err, ErrRequestReset) {
		t.Fatalf("completion of the reset r2 on page-2: err = %v, want ErrRequestReset", err)
	}
	if _, err := h.s.Control(ctx, completeOn("page-1", "r2")); err == nil || errors.Is(err, ErrRequestReset) {
		t.Fatalf("completion of r2 on page-1, which never held it: err = %v, want a refusal without ErrRequestReset", err)
	}
}

// Design 2.10: the control lane refuses, without sending it, a completion
// that names a request the given target does not hold.
func TestControlRefusesCompletionOnAnotherTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	h.f.Provisional("page-2")
	h.f.Intercept("page-2", "r2", "request")
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
	for _, m := range []ControlMsg{completeOn("page-2", "r1"), completeOn("page-1", "r2"), completeOn("", "r2"), completeOn("page-9", "r1")} {
		if _, err := h.s.Control(ctx, m); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("completion of %s on %q: err = %v, want ErrControlRefused", m.RequestID, m.TargetID, err)
		}
	}
	if n := completionsSent(h.f); n != 0 {
		t.Fatalf("%d completions sent; a refused completion is never sent", n)
	}
	if got := len(h.s.PendingRequests()); got != 2 {
		t.Fatalf("%d pending requests after the refusals, want 2", got)
	}
}

// For any interleaving of holds on several targets, completions, provisional
// targets that commit or are abandoned, and a final release barrier, through
// the daemon's page connection over a Session: every request that WebKit
// holds is answered exactly once, on its own target, unless its target was
// destroyed first (WebKit dropped it then); no completion reaches a target
// that does not hold its request; and the scheduler tracks exactly the
// holds of live targets.
//
// Oracle: the fake's per-target model of WebKit's pending intercepts.
func TestEveryHoldIsAnsweredOnceOnItsTargetProperty(t *testing.T) {
	var reach struct{ provisionalAnswers, commits, aborts, wrongTarget int }
	prop := func(ops []uint8) bool {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
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
		s.SetLease(testSession, 1)

		active, provisional := "page-1", ""
		next := 0
		var holds []string           // every hold's request id
		model := map[string]string{} // live hold id -> target
		dropped := map[string]bool{} // ids whose target was destroyed before an answer
		sync := func() { syncPage(t, sess, active) }
		for _, op := range ops {
			switch op % 6 {
			case 0, 1: // a hold on the active or the provisional target
				target := active
				if op%2 == 1 && provisional != "" {
					target = provisional
				}
				next++
				id := fmt.Sprintf("r%d", next)
				stage := [2]string{"request", "response"}[op/6%2]
				f.Intercept(target, id, stage)
				holds = append(holds, id)
				model[id] = target
				sync()
			case 2, 3: // a client completes a hold, naming its source target
				if len(model) == 0 {
					continue
				}
				id := fmt.Sprintf("r%d", 1+int(op)%next)
				target, held := model[id]
				if !held {
					continue
				}
				name := target
				if op%2 == 1 && target == active {
					name = "" // the active target
				}
				if _, err := s.Control(ctx, completeOn(name, id)); err != nil {
					t.Logf("completion of %s on %q: %v", id, name, err)
					return false
				}
				if target == provisional {
					reach.provisionalAnswers++
				}
				delete(model, id)
			case 4: // a completion naming a target that does not hold the request
				for id, target := range model {
					other := "page-1"
					if target == other {
						other = "page-x"
					}
					if _, err := s.Control(ctx, completeOn(other, id)); !errors.Is(err, ErrControlRefused) {
						t.Logf("completion of %s (held on %s) on %s: err = %v, want refused", id, target, other, err)
						return false
					}
					reach.wrongTarget++
					break
				}
			case 5: // a provisional target appears, commits, or is abandoned
				switch {
				case provisional == "":
					next++
					provisional = fmt.Sprintf("page-p%d", next)
					f.Provisional(provisional)
				case op/6%2 == 0:
					f.Commit(provisional)
					for id, target := range model {
						if target == active {
							delete(model, id)
							dropped[id] = true
						}
					}
					active, provisional = provisional, ""
					reach.commits++
				default:
					f.DestroyTarget(provisional)
					for id, target := range model {
						if target == provisional {
							delete(model, id)
							dropped[id] = true
						}
					}
					provisional = ""
					reach.aborts++
				}
				sync()
			}
			want := len(model)
			if got := len(s.PendingRequests()); got != want {
				t.Logf("scheduler tracks %d holds, model %v", got, model)
				return false
			}
		}
		if err := s.Release(ctx, nil); err != nil {
			t.Logf("Release: %v", err)
			return false
		}
		answered := map[string]int{}
		for _, a := range f.Answers() {
			answered[a.RequestID]++
		}
		for _, id := range holds {
			n := answered[id]
			switch {
			case dropped[id] && n != 0:
				t.Logf("%s was answered %d times after its target was destroyed", id, n)
				return false
			case !dropped[id] && n != 1:
				t.Logf("%s was answered %d times, want once", id, n)
				return false
			}
		}
		for _, target := range []string{active, provisional} {
			if held := f.HeldRequests(target); len(held) != 0 {
				t.Logf("%s still holds %v after the release", target, held)
				return false
			}
		}
		// Every completion that was sent took a hold on its own target.
		if sent, took := completionsSent(f), len(f.Answers()); sent != took {
			t.Logf("%d completions sent, %d took a hold", sent, took)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 150}); err != nil {
		t.Fatal(err)
	}
	if reach.provisionalAnswers == 0 || reach.commits == 0 || reach.aborts == 0 || reach.wrongTarget == 0 {
		t.Fatalf("generator reach %+v: want each case", reach)
	}
}

// dialSession opens a Session on f.
func dialSession(t *testing.T, f *fakewebkit.Server) *webkit.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// The daemon's page connection and the operation callers pass each
// event's source target through (webkit.TargetSubscriber), also after a
// reconnect, and forward the execution-context inventory and the active
// target of the current connection.
func TestPageConnPassesSourceTargets(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	sess := dialSession(t, f)
	pc := newPageConn(sess, nil, func(fn func(context.Context)) bool { go fn(context.Background()); return true }, t.Logf)
	defer pc.close()
	var mu sync.Mutex
	var got []string
	record := func(tag string) webkit.TargetEventHandler {
		return func(target, method string, _ json.RawMessage) {
			mu.Lock()
			got = append(got, tag+" "+target+" "+method)
			mu.Unlock()
		}
	}
	events := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	s := NewScheduler(pc, SchedulerOptions{})
	defer s.Close()
	s.SetLease(testSession, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.Begin(ctx, OpSpec{Name: "test", Class: ClassRead, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	webkit.SubscribeTarget(pc, "Test.e", record("conn"))
	webkit.SubscribeTarget(op.Caller(), "Test.e", record("op"))
	webkit.SubscribeTarget(attemptCaller{op: op}, "Test.e", record("attempt"))
	pc.SubscribeTargetSync("Test.e", record("sync"))
	legacy := record("legacy-sync") // the existing SubscribeSync keeps working, without the target
	pc.SubscribeSync("Test.e", func(method string, p json.RawMessage) { legacy("", method, p) })

	f.EmitFrom("page-1", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 3, "type": "normal", "name": "", "frameId": "F"}})
	f.EmitFrom("page-1", "Test.e", map[string]any{})
	syncPage(t, sess, "page-1")
	if got := pc.ExecutionContexts("page-1"); len(got) != 1 || got[0].ID != 3 || got[0].FrameID != "F" {
		t.Fatalf("ExecutionContexts(page-1) = %+v, want context 3 of frame F", got)
	}
	if a := pc.ActiveTarget(); a != "page-1" {
		t.Fatalf("ActiveTarget = %q, want page-1", a)
	}

	// A reconnect: the subscriptions move to the new connection.
	sess2 := dialSession(t, f)
	pc.swap(sess2)
	f.EmitFrom("page-1", "Test.e", map[string]any{})
	syncPage(t, sess2, "page-1")
	waitUntil(t, "the queued events", func() bool { return len(events()) == 10 })
	count := map[string]int{}
	for _, e := range events() {
		count[e]++
	}
	want := map[string]int{"conn page-1 Test.e": 2, "op page-1 Test.e": 2, "attempt page-1 Test.e": 2, "sync page-1 Test.e": 2, "legacy-sync  Test.e": 2}
	if !reflect.DeepEqual(count, want) {
		t.Fatalf("events = %v, want %v", count, want)
	}
	if got := pc.ExecutionContexts("page-1"); len(got) != 0 {
		t.Fatalf("ExecutionContexts after the reconnect = %+v, want the new connection's (none)", got)
	}
}

// A page connection over a connection without targets reports "" as the
// active target and no execution contexts; its holds are keyed by "".
func TestPageConnWithoutTargets(t *testing.T) {
	c := &stageConn{handlers: map[string]webkit.EventHandler{}}
	pc := newPageConn(c, nil, func(fn func(context.Context)) bool { go fn(context.Background()); return true }, t.Logf)
	if a := pc.ActiveTarget(); a != "" {
		t.Fatalf("ActiveTarget = %q, want \"\"", a)
	}
	if got := pc.ExecutionContexts("x"); got != nil {
		t.Fatalf("ExecutionContexts = %+v, want nil", got)
	}
	s := NewScheduler(pc, SchedulerOptions{})
	defer s.Close()
	c.emit("Network.requestIntercepted", "r1")
	if got, want := s.PendingHolds(), []HeldRequest{{RequestID: "r1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending holds = %+v, want %+v", got, want)
	}
}

// syncStageConn is a connection with in-loop subscriptions (SubscribeSync)
// that does not know event sources.
type syncStageConn struct{ *stageConn }

func (c syncStageConn) SubscribeSync(method string, h webkit.EventHandler) func() {
	return c.Subscribe(method, h)
}

// The scheduler records holds in the read loop of a connection that offers
// only SubscribeSync, under the connection's own target "".
func TestSchedulerUsesSubscribeSyncWithoutSources(t *testing.T) {
	c := syncStageConn{&stageConn{handlers: map[string]webkit.EventHandler{}}}
	s := NewScheduler(c, SchedulerOptions{})
	defer s.Close()
	c.emit("Network.responseIntercepted", "r1")
	if got, want := s.PendingHolds(), []HeldRequest{{RequestID: "r1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending holds = %+v, want %+v", got, want)
	}
}

// activeStageConn is a connection whose events carry no source target but
// that names an active target.
type activeStageConn struct{ *stageConn }

func (activeStageConn) ActiveTarget() string { return "page-1" }

// The barriers answer a hold by its exact key: a hold that a connection
// without event sources recorded under "" is continued, and the release
// ends, although "" in a client's message means the active target.
func TestReleaseAnswersAHoldWithoutSourceTarget(t *testing.T) {
	c := activeStageConn{&stageConn{handlers: map[string]webkit.EventHandler{}}}
	s := NewScheduler(c, SchedulerOptions{})
	defer s.Close()
	s.SetLease(testSession, 1)
	c.emit("Network.requestIntercepted", "r1")
	done := make(chan error, 1)
	go func() { done <- s.Release(context.Background(), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Release did not end: the hold under \"\" was never answered")
	}
	if len(c.sent) != 1 || c.sent[0].method != "Network.interceptContinue" || c.sent[0].params["requestId"] != "r1" {
		t.Fatalf("sent %+v, want one interceptContinue of r1", c.sent)
	}
}
