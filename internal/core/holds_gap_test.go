package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Held requests across failed completions and new connections (docs/notes/
// w1b.md, "Review repairs, closure: held requests and the reconnect gap").
//
// WebKit's InspectorNetworkAgent takes a hold from its pending requests or
// responses before anything else in each of the five completion methods
// (webkitgtk-2.54.2 InspectorNetworkAgent.cpp:1180-1353), and its generated
// dispatcher refuses a call with missing or mistyped parameters
// (InvalidParams, -32602) before the agent runs. So a dispatcher refusal
// leaves the request paused, and every agent error means the hold is gone.

// The closure reviewer's reproduction: WebKit's dispatcher refuses a
// response-stage completion without base64Encoded (Network.json requires
// it), so the request stays paused, and a valid continue afterwards must
// still reach WebKit.
func TestRefusedCompletionKeepsTheHold(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Emit("Network.responseIntercepted", map[string]string{"requestId": "r1"})
	waitUntil(t, "the response hold", func() bool { return len(h.s.PendingRequests()) == 1 })
	msg := ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptWithResponse", Params: map[string]any{"content": "x"}}
	_, err := h.s.Control(ctx, msg)
	var pe *webkit.ErrorData
	if !errors.As(err, &pe) || pe.Code != -32602 {
		t.Fatalf("completion without base64Encoded: %v, want the dispatcher's InvalidParams refusal", err)
	}
	if p := h.s.PendingRequests(); !reflect.DeepEqual(p, []string{"r1"}) {
		t.Fatalf("pending after the dispatcher refusal = %v, want r1 still held", p)
	}
	msg.Method, msg.Params = "Network.interceptContinue", nil
	if _, err := h.s.Control(ctx, msg); err != nil {
		t.Fatalf("valid continue after the refusal: %v", err)
	}
	want := []map[string]any{{"requestId": "r1", "stage": "response"}}
	if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("interceptContinue params = %v, want %v", got, want)
	}
	if p := h.s.PendingRequests(); len(p) != 0 {
		t.Fatalf("pending after the continue = %v, want none", p)
	}
}

// The release barrier answers a hold that a refused completion left open,
// with a plain continue at the stage of the hold.
func TestReleaseContinuesAHoldARefusedCompletionLeft(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Emit("Network.responseIntercepted", map[string]string{"requestId": "r1"})
	waitUntil(t, "the response hold", func() bool { return len(h.s.PendingRequests()) == 1 })
	_, err := h.s.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptWithResponse", Params: map[string]any{"content": "x"}})
	if err == nil {
		t.Fatal("completion without base64Encoded succeeded, want the dispatcher's refusal")
	}
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	want := []map[string]any{{"requestId": "r1", "stage": "response"}}
	if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("interceptContinue params = %v, want %v", got, want)
	}
}

// Outcomes of one completion at holdConn, as WebKit and the transport
// produce them.
type holdOutcome int

const (
	outOK           holdOutcome = iota // the agent takes the hold and answers it
	outInvalid                         // the dispatcher refuses the parameters; the hold stays
	outTakenRefused                    // the agent takes the hold, then refuses (a decode error)
	outLostTaken                       // the agent answers, the reply is lost
	outLostNotTaken                    // the call never reached the agent, no reply
	outNotSent                         // the call was never written
	numHoldOutcomes
)

func (o holdOutcome) String() string {
	return [...]string{"ok", "invalid", "taken-refused", "lost-taken", "lost-not-taken", "not-sent"}[o]
}

// holdConn is a page connection with a model of WebKit's pending
// intercepts. A completion follows the next scripted outcome (outOK when
// none is left): the agent takes the hold at the stage the method names
// and answers "Missing pending intercept ..." when it has none at that
// stage. Events are delivered synchronously through hold.
type holdConn struct {
	mu       sync.Mutex
	handlers map[string]webkit.EventHandler
	pending  map[string]string // request id -> stage WebKit holds it at
	instance map[string]int    // request id -> number of its latest hold
	answered map[string]int    // "id#instance" -> times WebKit took that hold
	script   []holdOutcome
	sent     []sentCall
	gate     func(method string) // runs before a completion is applied, outside mu
}

func newHoldConn() *holdConn {
	return &holdConn{handlers: map[string]webkit.EventHandler{}, pending: map[string]string{}, instance: map[string]int{}, answered: map[string]int{}}
}

func (c *holdConn) hold(id, stage string) {
	c.mu.Lock()
	c.pending[id] = stage
	c.instance[id]++
	c.answered[fmt.Sprintf("%s#%d", id, c.instance[id])] = 0
	h := c.handlers["Network."+stage+"Intercepted"]
	c.mu.Unlock()
	h("Network."+stage+"Intercepted", json.RawMessage(fmt.Sprintf("{\"requestId\":%q}", id)))
}

func (c *holdConn) setScript(s ...holdOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.script = s
}

func (c *holdConn) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if !isInterceptCompletion(method) {
		return json.RawMessage("{}"), nil
	}
	if c.gate != nil {
		c.gate(method)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, sentCall{method, p})
	out := outOK
	if len(c.script) > 0 {
		out, c.script = c.script[0], c.script[1:]
	}
	switch out {
	case outNotSent:
		return nil, fmt.Errorf("%s: %w", method, webkit.ErrNotSent)
	case outInvalid:
		return nil, &webkit.ErrorData{Code: -32602, Message: fmt.Sprintf("Some arguments of method '%s' can't be processed", method)}
	case outLostNotTaken:
		return nil, fmt.Errorf("%s: %w", method, webkit.ErrCallTimeout)
	}
	id, _ := p["requestId"].(string)
	stage := interceptCompletions[method]
	if method == "Network.interceptContinue" {
		stage, _ = p["stage"].(string)
	}
	if c.pending[id] != stage || stage == "" {
		return nil, &webkit.ErrorData{Code: -32000, Message: "Missing pending intercept " + stage + " for given requestId"}
	}
	delete(c.pending, id)
	c.answered[fmt.Sprintf("%s#%d", id, c.instance[id])]++
	switch out {
	case outTakenRefused:
		return nil, &webkit.ErrorData{Code: -32000, Message: "Unable to decode given content"}
	case outLostTaken:
		return nil, fmt.Errorf("%s: %w", method, webkit.ErrCallTimeout)
	}
	return json.RawMessage("{}"), nil
}

func (c *holdConn) OnEvent(method string, h webkit.EventHandler) { c.Subscribe(method, h) }
func (c *holdConn) Subscribe(method string, h webkit.EventHandler) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[method] = h
	return func() {}
}
func (c *holdConn) IsTargetRouted() bool  { return true }
func (c *holdConn) Done() <-chan struct{} { return nil }

// held returns the ids WebKit still holds, sorted.
func (c *holdConn) held() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for id := range c.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func newHoldScheduler(t *testing.T) (*holdConn, *Scheduler) {
	t.Helper()
	c := newHoldConn()
	s := NewScheduler(c, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)
	return c, s
}

func completion(id, method string, params map[string]any) ControlMsg {
	return ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: id, Method: method, Params: params}
}

// Per outcome of a client's completion: whether the scheduler keeps the
// hold, and whether the release barrier then continues it. Expected
// results come from WebKit's InspectorNetworkAgent (above): only the
// outcomes where the agent did not take the hold leave the request paused.
func TestCompletionOutcomeDecidesTheHold(t *testing.T) {
	for _, tc := range []struct {
		out  holdOutcome
		kept bool // the request may still be paused in WebKit
	}{
		{outOK, false},
		{outInvalid, true},
		{outTakenRefused, false},
		{outLostTaken, true},
		{outLostNotTaken, true},
		{outNotSent, true},
	} {
		t.Run(tc.out.String(), func(t *testing.T) {
			c, s := newHoldScheduler(t)
			ctx, _ := timeoutCtx(t, 5*time.Second)
			c.hold("r1", stageRequest)
			c.setScript(tc.out)
			_, err := s.Control(ctx, completion("r1", "Network.interceptWithRequest", nil))
			if (err == nil) != (tc.out == outOK) {
				t.Fatalf("completion with outcome %s: err %v", tc.out, err)
			}
			if kept := len(s.PendingRequests()) == 1; kept != tc.kept {
				t.Fatalf("hold kept = %v after %s, want %v", kept, tc.out, tc.kept)
			}
			before := len(c.sent)
			if err := s.Release(ctx, nil); err != nil {
				t.Fatalf("Release: %v", err)
			}
			barrier := c.sent[before:]
			if tc.kept != (len(barrier) == 1) {
				t.Fatalf("barrier sent %v after %s, want one continue only when the hold was kept", barrier, tc.out)
			}
			if len(barrier) == 1 && (barrier[0].method != "Network.interceptContinue" || barrier[0].params["stage"] != stageRequest) {
				t.Fatalf("barrier sent %v, want a plain continue at the request stage", barrier[0])
			}
			if h := c.held(); len(h) != 0 {
				t.Fatalf("WebKit still holds %v after the release", h)
			}
			if n := c.answered["r1#1"]; n != 1 {
				t.Fatalf("WebKit took the hold %d times, want exactly once", n)
			}
		})
	}
}

// A completion in flight when the release barrier starts can fail and
// leave the request paused: the barrier waits for it and then answers the
// request. When it succeeds, the barrier sends nothing for it.
func TestReleaseWaitsForACompletionInFlight(t *testing.T) {
	for _, out := range []holdOutcome{outInvalid, outOK} {
		t.Run(out.String(), func(t *testing.T) {
			c, s := newHoldScheduler(t)
			ctx, _ := timeoutCtx(t, 5*time.Second)
			c.hold("r1", stageResponse)
			entered, proceed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			c.gate = func(string) {
				once.Do(func() {
					close(entered)
					<-proceed
				})
			}
			c.setScript(out)
			clientDone := make(chan error, 1)
			go func() {
				_, err := s.Control(ctx, completion("r1", "Network.interceptWithResponse", map[string]any{"content": "", "base64Encoded": false}))
				clientDone <- err
			}()
			<-entered
			released := make(chan error, 1)
			go func() { released <- s.Release(ctx, nil) }()
			select {
			case err := <-released:
				t.Fatalf("Release returned %v while a completion was in flight", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(proceed)
			if err := <-clientDone; (err == nil) != (out == outOK) {
				t.Fatalf("client completion with outcome %s: %v", out, err)
			}
			if err := <-released; err != nil {
				t.Fatalf("Release: %v", err)
			}
			if h := c.held(); len(h) != 0 {
				t.Fatalf("WebKit still holds %v after the release", h)
			}
			if n := c.answered["r1#1"]; n != 1 {
				t.Fatalf("WebKit took the hold %d times, want exactly once", n)
			}
			if want := map[holdOutcome]int{outInvalid: 2, outOK: 1}[out]; len(c.sent) != want {
				t.Fatalf("sent %v, want %d completions", c.sent, want)
			}
		})
	}
}

// The release barrier retries a continue that left the hold open, at most
// maxBarrierContinues times, and reports the failure when every attempt
// fails; the request then stays tracked as held.
func TestReleaseRetriesTheContinue(t *testing.T) {
	c, s := newHoldScheduler(t)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	c.hold("r1", stageRequest)
	c.setScript(outLostNotTaken, outNotSent)
	if err := s.Release(ctx, nil); err != nil {
		t.Fatalf("Release after two failed continues: %v, want the third to answer", err)
	}
	if len(c.sent) != 3 || len(c.held()) != 0 {
		t.Fatalf("sent %d continues, WebKit holds %v; want 3 and none", len(c.sent), c.held())
	}

	c, s = newHoldScheduler(t)
	c.hold("r1", stageRequest)
	c.setScript(outLostNotTaken, outLostNotTaken, outLostNotTaken, outOK)
	err := s.Release(ctx, nil)
	if !errors.Is(err, webkit.ErrCallTimeout) {
		t.Fatalf("Release with %d failed continues: %v, want the last failure", maxBarrierContinues, err)
	}
	if len(c.sent) != maxBarrierContinues {
		t.Fatalf("sent %d continues, want %d", len(c.sent), maxBarrierContinues)
	}
	if p := s.PendingRequests(); !reflect.DeepEqual(p, []string{"r1"}) {
		t.Fatalf("pending = %v, want r1 still tracked", p)
	}
}

// The Network reinitialization barrier (design 2.4 step 2) uses the same
// answer as the release barrier: a hold whose continues all fail stays open
// for the release barrier and is not recorded as reset, so a later
// completion is still sent instead of failing with ErrRequestReset.
func TestNetworkResetKeepsAHoldItCouldNotContinue(t *testing.T) {
	c, s := newHoldScheduler(t)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	c.hold("r1", stageRequest)
	c.hold("r2", stageResponse)
	c.setScript(outLostNotTaken, outLostNotTaken, outLostNotTaken) // r1 or r2, whichever goes first
	done, err := s.ResetHeldRequests(ctx)
	if !errors.Is(err, webkit.ErrCallTimeout) || len(done) != 1 {
		t.Fatalf("ResetHeldRequests = %v, %v; want one continued id and the failure of the other", done, err)
	}
	left := s.PendingRequests()
	if len(left) != 1 || left[0] == done[0] || !slices.Equal(c.held(), left) {
		t.Fatalf("pending %v, WebKit holds %v after continuing %v; want the failed hold tracked", left, c.held(), done)
	}
	if _, err := s.Control(ctx, completion(left[0], "Network.interceptContinue", nil)); err != nil {
		t.Fatalf("completion of the hold the reset could not continue: %v", err)
	}
	if _, err := s.Control(ctx, completion(done[0], "Network.interceptContinue", nil)); !errors.Is(err, ErrRequestReset) {
		t.Fatalf("completion of the reset hold: %v, want ErrRequestReset", err)
	}
}

// The release barrier waits for a completion in flight only until its
// context ends, and then reports that.
func TestReleaseWaitForACompletionEndsWithItsContext(t *testing.T) {
	c, s := newHoldScheduler(t)
	c.hold("r1", stageRequest)
	entered, proceed := make(chan struct{}), make(chan struct{})
	c.gate = func(string) { close(entered); <-proceed }
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_, _ = s.Control(context.Background(), completion("r1", "Network.interceptContinue", nil))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := s.Release(ctx, nil)
	close(proceed)
	<-clientDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Release while a completion stays in flight: %v, want the context's deadline", err)
	}
}

// Property: for any interleaving of holds and client completions with any
// outcome, followed by the release barrier with any outcomes of its own
// continues:
//   - after every step, every request WebKit holds is an open hold of the
//     scheduler at the same stage (no paused request is forgotten);
//   - WebKit takes each hold at most once;
//   - when the release succeeds, WebKit holds nothing and every hold was
//     taken exactly once, by a client's completion or by the barrier;
//     when it fails, every request WebKit still holds is still tracked.
//
// Oracle: the holdConn model of InspectorNetworkAgent, not the scheduler.
func TestEveryHeldRequestIsAnsweredOnce(t *testing.T) {
	ids := []string{"r1", "r2", "r3"}
	methods := []string{"Network.interceptContinue", "Network.interceptWithRequest", "Network.interceptWithResponse", "Network.interceptRequestWithError"}
	var keptThenAnswered, releaseFailed, retried int // generator reach
	prop := func(ops []uint16, barrier []uint8) bool {
		c := newHoldConn()
		s := NewScheduler(c, SchedulerOptions{})
		defer s.Close()
		s.SetLease(testSession, 1)
		ctx := context.Background()
		kept := map[string]bool{} // ids a failed client completion left open
		check := func(step string) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			s.mu.Lock()
			defer s.mu.Unlock()
			for id, stage := range c.pending {
				h := s.intercepted[id]
				if h == nil || h.inflight || h.stage != stage {
					t.Logf("%s: WebKit holds %s at %q, scheduler has %+v", step, id, stage, h)
					return false
				}
			}
			for k, n := range c.answered {
				if n > 1 {
					t.Logf("%s: WebKit took hold %s %d times", step, k, n)
					return false
				}
			}
			return true
		}
		for i, op := range ops {
			id := ids[int(op)%len(ids)]
			op /= uint16(len(ids))
			if op%3 == 0 {
				if !slices.Contains(c.held(), id) { // WebKit holds an id once at a time
					stage := stageRequest
					if op&8 != 0 {
						stage = stageResponse
					}
					c.hold(id, stage)
					delete(kept, id)
				}
			} else {
				op /= 3
				m := methods[int(op)%len(methods)]
				out := holdOutcome(int(op/uint16(len(methods))) % int(numHoldOutcomes))
				params := map[string]any{}
				if m == "Network.interceptWithResponse" {
					params = map[string]any{"content": "", "base64Encoded": false}
				}
				before := len(c.sent)
				c.setScript(out)
				_, err := s.Control(ctx, completion(id, m, params))
				c.setScript()
				if len(c.sent) > before && err != nil && slices.Contains(s.PendingRequests(), id) {
					kept[id] = true
				}
			}
			if !check(fmt.Sprintf("step %d", i)) {
				return false
			}
		}
		script := make([]holdOutcome, 0, len(barrier))
		for _, b := range barrier {
			script = append(script, []holdOutcome{outOK, outOK, outLostTaken, outLostNotTaken, outNotSent}[int(b)%5])
		}
		c.setScript(script...)
		before, pendingAtRelease := len(c.sent), len(s.PendingRequests())
		err := s.Release(ctx, nil)
		if !check("release") {
			return false
		}
		held := c.held()
		if err != nil {
			releaseFailed++
			for _, id := range held {
				if !slices.Contains(s.PendingRequests(), id) {
					t.Logf("release failed (%v); WebKit holds %s untracked", err, id)
					return false
				}
			}
			return true
		}
		if len(held) != 0 || len(s.PendingRequests()) != 0 {
			t.Logf("release succeeded; WebKit holds %v, scheduler %v", held, s.PendingRequests())
			return false
		}
		for k, n := range c.answered {
			if n != 1 {
				t.Logf("release succeeded; hold %s taken %d times", k, n)
				return false
			}
		}
		for id := range kept {
			if c.answered[fmt.Sprintf("%s#%d", id, c.instance[id])] == 1 {
				keptThenAnswered++
			}
		}
		if len(c.sent)-before > pendingAtRelease {
			retried++
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
	if keptThenAnswered == 0 || releaseFailed == 0 || retried == 0 {
		t.Fatalf("generator reach: %d kept holds answered later, %d failed releases, %d retried releases; want each", keptThenAnswered, releaseFailed, retried)
	}
}

// Design 2.4: a new connection resets the backend's Network agent (the old
// connection's close and the new connection's Apply both disable it), so
// it reports the gap to every remaining Network owner, as an explicit
// Network reset does, with the held requests it forgot. The reviewer's
// reproduction is the takeover case.
func TestNewConnectionReportsTheNetworkGap(t *testing.T) {
	for _, path := range []string{"takeover", "recovery"} {
		t.Run(path, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{Steal: path == "takeover"}, false, ManagerOptions{})
			r.reserve(t, "agent")
			b := newInterceptBackend(r.f)
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "capture", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			pg := mo.Page
			r.m.End(mo.ID)
			var mu sync.Mutex
			var gaps, otherGaps []NetworkGap
			owner := SessionOwner("legacy", agent.Session)
			defer pg.PS.State.WatchNetworkReset(owner, func(g NetworkGap) { mu.Lock(); defer mu.Unlock(); gaps = append(gaps, g) })()
			// An owner without Network gets no gap.
			defer pg.PS.State.WatchNetworkReset(SessionOwner("netlog", "other"), func(g NetworkGap) { mu.Lock(); defer mu.Unlock(); otherGaps = append(otherGaps, g) })()
			b.hold(r.f, "r1")
			eventually(t, 3*time.Second, "held r1", func() bool { return len(pg.PS.Sched.PendingRequests()) == 1 })
			before := time.Now()
			switch path {
			case "takeover":
				stealPage(t, r.f)
				eventually(t, 3*time.Second, "lost page", func() bool { return pg.ConnState() == ConnLost })
				take := agent
				take.Takeover = true
				mo, err := r.m.Begin(ctx, r.req, take, "after takeover", ClassExclusive)
				if err != nil {
					t.Fatalf("--takeover: %v", err)
				}
				r.m.End(mo.ID)
			case "recovery":
				mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				r.f.StallConnection(1)
				_, _ = r.m.Raw(ctxFor(t, time.Second), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
				r.m.End(mo.ID)
				eventually(t, 15*time.Second, "background recovery", func() bool { return pg.ConnState() == ConnOK && r.f.ConnectionCount() == 2 })
			}
			mu.Lock()
			defer mu.Unlock()
			if len(gaps) != 1 {
				t.Fatalf("gaps reported to the Network owner %s: %+v, want one (owners %v)", owner, gaps, pg.PS.State.AllOwners())
			}
			g := gaps[0]
			if !reflect.DeepEqual(g.Requests, []string{"r1"}) || g.Start.Before(before) || g.End.Before(g.Start) || g.End.After(time.Now()) {
				t.Errorf("gap %+v, want requests [r1] and %v <= start <= end <= now", g, before)
			}
			if len(otherGaps) != 0 {
				t.Errorf("gaps reported to an owner without Network: %+v", otherGaps)
			}
			if n := b.held(); n != 0 {
				t.Errorf("the backend still holds %d requests", n)
			}
		})
	}
}
