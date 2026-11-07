package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Regression tests for the sixth W1b review (reviewer counterexamples in
// /home/devkit/.pi/artifacts/reviewer/w1b-r6-evidence-20261107-033226):
// R6-1, a forwarding failure on a live provisional target reaches the
// change's caller; R6-2, each change waits for its own forwarded work and
// gets its own result, whoever sends the target's queue; R6-3 and R6-4, an
// owner's <Domain>.disable drops every contribution of that owner to the
// domain (entries, shared values and the specialized Network state),
// whether or not it holds a reference.

// scriptedTarget is a provisional target whose Page.overrideUserAgent
// sends are scripted by value: blockValue holds its send until release is
// closed (blocked is closed when it starts), and failValue is rejected
// with failure. value is the last accepted value.
type scriptedTarget struct {
	blockValue, failValue string
	blocked, release      chan struct{}
	failure               error

	mu    sync.Mutex
	value string
}

func (q *scriptedTarget) TargetID() string { return "page-1" }

func (q *scriptedTarget) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method != "Page.overrideUserAgent" {
		return json.RawMessage("{}"), nil
	}
	raw, _ := json.Marshal(params)
	var v struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.Value == q.blockValue {
		close(q.blocked)
		<-q.release
	}
	if v.Value == q.failValue {
		return nil, q.failure
	}
	q.mu.Lock()
	q.value = v.Value
	q.mu.Unlock()
	return json.RawMessage("{}"), nil
}

// queuedFor is the number of changes queued for target.
func queuedFor(d *DesiredState, target string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ts := d.syncs[target]; ts != nil {
		return len(ts.queue)
	}
	return 0
}

// R6-1: a live provisional target rejects the forwarded change; the change
// returns that failure.
func TestForwardingFailureReachesTheCaller(t *testing.T) {
	_, c, ps := ownersPage(t)
	bad := errors.New("page-1 rejected B")
	q := &scriptedTarget{failValue: "B", failure: bad}
	if err := ps.State.Apply(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	_, err := legacyCall(ctxFor(t, 3*time.Second), ps, SessionOwner("legacy", "s1"), c, "Page.overrideUserAgent", json.RawMessage(`{"value":"B"}`))
	if !errors.Is(err, bad) {
		t.Fatalf("the forwarding failure on page-1 was hidden: legacyCall returned %v, want %v", err, bad)
	}
}

// R6-2: change A sends page-1's queue and is held there; change B queues
// its value behind A's. Each change gets the result of its own send to
// page-1, and page-1 ends with the value of the change that succeeded.
func TestEachChangeGetsItsOwnForwardingResult(t *testing.T) {
	for _, tc := range []struct {
		name, fail, want string
	}{
		{"the queued change fails", "B", "A"},
		{"the sending change fails", "A", "B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, ps := ownersPage(t)
			bad := errors.New("page-1 rejected " + tc.fail)
			q := &scriptedTarget{blockValue: "A", failValue: tc.fail, failure: bad, blocked: make(chan struct{}), release: make(chan struct{})}
			ctx := ctxFor(t, 5*time.Second)
			if err := ps.State.Apply(ctx, q); err != nil {
				t.Fatal(err)
			}
			set := func(value string, out chan<- error) {
				_, err := legacyCall(ctx, ps, SessionOwner("legacy", "s1"), c, "Page.overrideUserAgent", json.RawMessage(`{"value":"`+value+`"}`))
				out <- err
			}
			first, second := make(chan error, 1), make(chan error, 1)
			go set("A", first)
			select {
			case <-q.blocked:
			case <-ctx.Done():
				t.Fatal("page-1 never received A")
			}
			go set("B", second)
			var errB error
			eventually(t, 2*time.Second, "B queued behind A or returned", func() bool {
				select {
				case errB = <-second:
					second <- errB
					return true
				default:
					return queuedFor(ps.State, "page-1") > 0
				}
			})
			close(q.release)
			results := map[string]error{}
			for name, ch := range map[string]chan error{"A": first, "B": second} {
				select {
				case results[name] = <-ch:
				case <-ctx.Done():
					t.Fatalf("change %s did not return", name)
				}
			}
			for _, name := range []string{"A", "B"} {
				if name == tc.fail && !errors.Is(results[name], bad) {
					t.Errorf("change %s returned %v, want its own failure %v", name, results[name], bad)
				}
				if name != tc.fail && results[name] != nil {
					t.Errorf("change %s returned %v, want success (the failure belongs to %s)", name, results[name], tc.fail)
				}
			}
			ps.State.commitTarget("page-1")
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.value != tc.want {
				t.Errorf("the committed backend has %q, want %q of the change that succeeded", q.value, tc.want)
			}
		})
	}
}

// R6-3: s2's baseline active:false did not acquire Debugger; s2's own
// Debugger.disable still removes it, so s1's active:true is effective again.
func TestDisableReleasesABaselineValueWithoutTheDomain(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Debugger.enable", "")
	ownerCall(t, ps, c, "s1", "Debugger.setBreakpointsActive", `{"active":true}`)
	ownerCall(t, ps, c, "s2", "Debugger.setBreakpointsActive", `{"active":false}`)
	from := len(p.wire)
	ownerCall(t, ps, c, "s2", "Debugger.disable", "")
	if got := sentSince(p, from, "Debugger.disable"); len(got) != 0 {
		t.Fatalf("s2's disable reached the page while s1 holds Debugger: %q", got)
	}
	if tg := p.live["page-0"]; !tg.enabled || !tg.active {
		t.Fatalf("after s2's disable: enabled %v active %v; want s1's state (enabled, active)", tg.enabled, tg.active)
	}
	if left := ps.State.AllOwners(); !slices.Equal(left, []Owner{SessionOwner("legacy", "s1")}) {
		t.Fatalf("owners after s2's disable: %v, want only s1", left)
	}
}

// networkState is a target's Network state as the oracle compares it.
func networkState(tg *wkTarget) string {
	b, _ := json.Marshal(map[string]any{
		"enabled": tg.net, "headers": tg.headers, "intercept": tg.intercept,
		"rules": slices.Sorted(maps.Keys(tg.rules)), "caching": tg.caching, "conds": tg.conds,
	})
	return string(b)
}

// R6-4 (design 2.4): s1's Network.disable drops its headers, interception
// rule and enablement, caching override and emulated conditions while s2
// keeps Network. Removing the header name needs the Network
// reinitialization barrier, which reports the gap to s2 only. The active
// target and a provisional target whose setup predates the disable both
// end with s2's Network state, and a later target never gets s1's.
func TestNetworkDisableDropsTheOwnersNetworkState(t *testing.T) {
	p, c, ps := ownersPage(t)
	ctx := context.Background()
	var gaps []string
	for _, o := range []string{"s1", "s2"} {
		ps.State.WatchNetworkReset(SessionOwner("legacy", o), func(NetworkGap) { gaps = append(gaps, o) })
		ownerCall(t, ps, c, o, "Network.enable", "")
	}
	ownerCall(t, ps, c, "s2", "Network.setExtraHTTPHeaders", `{"headers":{"X-Kept":"two"}}`)
	ownerCall(t, ps, c, "s1", "Network.setExtraHTTPHeaders", `{"headers":{"X-Owned":"one"}}`)
	ownerCall(t, ps, c, "s1", "Network.addInterception", `{"url":"https://fixture.example/api","stage":"request"}`)
	ownerCall(t, ps, c, "s1", "Network.setResourceCachingDisabled", `{"disabled":true}`)
	ownerCall(t, ps, c, "s1", "Network.setEmulatedConditions", `{"bytesPerSecondLimit":1000}`)
	p.live["page-1"] = &wkTarget{bps: map[string]string{}}
	if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.live["page-1"].headers["X-Owned"]; got != "one" {
		t.Fatalf("page-1's setup did not get s1's header (got %q); the test needs it there", got)
	}
	ownerCall(t, ps, c, "s1", "Network.disable", "")
	want := networkState(&wkTarget{net: true, headers: map[string]string{"X-Kept": "two"}})
	for _, id := range []string{"page-0", "page-1"} {
		if got := networkState(p.live[id]); got != want {
			t.Errorf("%s after s1's Network.disable: %s, want %s", id, got, want)
		}
	}
	if h := ps.State.EffectiveHeaders(); !maps.Equal(h, map[string]string{"X-Kept": "two"}) {
		t.Errorf("effective headers %v, want only s2's", h)
	}
	if !slices.Equal(gaps, []string{"s2"}) {
		t.Errorf("gap reported to %v, want the remaining Network owner s2 only", gaps)
	}
	if left := ps.State.AllOwners(); !slices.Equal(left, []Owner{SessionOwner("legacy", "s2")}) {
		t.Errorf("owners after s1's disable: %v, want only s2", left)
	}
	p.live["page-2"] = &wkTarget{bps: map[string]string{}}
	if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-2"}); err != nil {
		t.Fatal(err)
	}
	if got := networkState(p.live["page-2"]); got != want {
		t.Errorf("a later target got %s, want %s", got, want)
	}
}

// R6-4 with R6-3: an owner that set a header without enabling Network
// drops it with its own Network.disable, while another owner keeps Network.
func TestNetworkDisableDropsHeadersOfAnOwnerWithoutTheDomain(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Network.enable", "")
	ownerCall(t, ps, c, "s2", "Network.setExtraHTTPHeaders", `{"headers":{"X-Two":"2"}}`)
	from := len(p.wire)
	ownerCall(t, ps, c, "s2", "Network.disable", "")
	if h := ps.State.EffectiveHeaders(); len(h) != 0 {
		t.Fatalf("s2's header %v survives its own Network.disable", h)
	}
	tg := p.live["page-0"]
	if !tg.net || len(tg.headers) != 0 {
		t.Fatalf("page-0 after s2's disable: Network enabled %v, headers %v; want enabled without headers", tg.net, tg.headers)
	}
	if got := sentSince(p, from, "Network.enable"); len(got) != 1 {
		t.Fatalf("want one Network reinitialization barrier on page-0, sent %q", strings.Join(p.wire[from:], "; "))
	}
}

// enablePage is the page for R6-6. <Domain>.enable fails with enableErr,
// or, when enableErr is nil, is written and its reply never comes before
// ctx ends (outcome unknown; written is closed at the write).
// <Domain>.disable answers disableErr. Every call is recorded.
type enablePage struct {
	wkConn
	enableErr, disableErr error
	written               chan struct{}

	mu   sync.Mutex
	sent []string
}

func (c *enablePage) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	c.mu.Lock()
	c.sent = append(c.sent, method)
	c.mu.Unlock()
	switch {
	case strings.HasSuffix(method, ".enable"):
		if c.enableErr != nil {
			return nil, c.enableErr
		}
		close(c.written)
		<-ctx.Done()
		return nil, fmt.Errorf("%s: waiting for the reply: %w", method, ctx.Err())
	case strings.HasSuffix(method, ".disable"):
		if c.disableErr != nil {
			return nil, c.disableErr
		}
	}
	return json.RawMessage("{}"), nil
}

func (c *enablePage) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if m == method {
			n++
		}
	}
	return n
}

// R6-6 (parent note, counterexample of the R6-cli worker: SIGINT during
// Console.enable): an enable whose outcome is unknown keeps its owner, so
// the owner's release, or its own disable, sends <Domain>.disable; an
// enable that provably had no effect (never written, or refused by the
// page) releases its owner, and nothing is disabled later. This holds for
// an explicit enable and for the implicit one of a breakpoint set. A
// disable refused because the domain is not enabled is no failure; any
// other disable failure is reported.
func TestEnableWithUnknownOutcomeIsDisabledLater(t *testing.T) {
	refused := &webkit.ErrorData{Code: -32000, Message: "Console domain already disabled"}
	lost := errors.New("connection lost")
	explicit := [2]string{"Console.enable", ""}
	implicit := [2]string{"Debugger.setBreakpointByUrl", bpByURL}
	for _, tc := range []struct {
		name                  string
		call                  [2]string
		enableErr, disableErr error
		ownDisable            bool // the owner disables the domain instead of releasing
		wantDisable           int
		wantErr               error
	}{
		{"reply lost to cancellation", explicit, nil, nil, false, 1, nil},
		{"reply lost, enable never took effect", explicit, nil, refused, false, 1, nil},
		{"reply lost, own disable", explicit, nil, refused, true, 1, nil},
		{"reply lost, disable fails", explicit, nil, lost, false, 1, lost},
		{"reply lost, own disable fails", explicit, nil, lost, true, 1, lost},
		{"implicit enable, reply lost", implicit, nil, nil, false, 1, nil},
		{"never written", explicit, fmt.Errorf("Console.enable: %w", webkit.ErrNotSent), nil, false, 0, nil},
		{"refused by the page", explicit, &webkit.ErrorData{Code: -32601, Message: "'Console.enable' was not found"}, nil, false, 0, nil},
		{"implicit enable refused", implicit, &webkit.ErrorData{Code: -32601, Message: "refused"}, nil, false, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dom, _, _ := strings.Cut(tc.call[0], ".")
			if tc.call == implicit {
				dom = "Debugger"
			}
			c := &enablePage{enableErr: tc.enableErr, disableErr: tc.disableErr, written: make(chan struct{})}
			ps := &PageSession{State: NewDesiredState(), Sched: NewScheduler(c, SchedulerOptions{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.enableErr == nil {
				go func() {
					<-c.written
					cancel()
				}()
			}
			if _, err := legacyCall(ctx, ps, SessionOwner("legacy", "s1"), c, tc.call[0], json.RawMessage(tc.call[1])); err == nil {
				t.Fatalf("%s succeeded; the test needs its enable to fail", tc.call[0])
			}
			var err error
			if tc.ownDisable {
				_, err = legacyCall(ctxFor(t, 3*time.Second), ps, SessionOwner("legacy", "s1"), c, dom+".disable", nil)
			} else {
				_, err = releaseOwners(ctxFor(t, 3*time.Second), ps, "s1", c)
			}
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("the cleanup returned %v, want %v", err, tc.wantErr)
			}
			if n := c.count(dom + ".disable"); n != tc.wantDisable {
				t.Fatalf("the cleanup sent %s.disable %d times, want %d (calls %q)", dom, n, tc.wantDisable, c.sent)
			}
			if left := ps.State.AllOwners(); len(left) != 0 {
				t.Fatalf("owners left after the cleanup: %v", left)
			}
		})
	}
}

// The waits for a target's queue end with their context and name the
// target (desired_sync.go): a change waiting for its own item while
// another sender holds the queue, a change draining a target that another
// sender holds, and a sender whose context ended before the queue did,
// which leaves the rest of the queue to the next sender.
func TestQueueWaitsEndWithTheirContext(t *testing.T) {
	d := NewDesiredState()
	q := &queued{send: func(context.Context, Sender) error { return nil }, done: make(chan struct{})}
	ts := &targetSync{busy: true, idle: make(chan struct{}), queue: []*queued{q}}
	d.syncs = map[string]*targetSync{"page-1": ts}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	for name, err := range map[string]error{
		"await": d.await(short(), pendingItem{"page-1", ts, q}),
		"drain": d.drain(short(), "page-1"),
	} {
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "page-1") {
			t.Errorf("%s while another sender holds page-1: %v, want a deadline error naming page-1", name, err)
		}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	ts.busy, ts.idle = true, make(chan struct{})
	if err := d.pump(ended, "page-1", ts, nil); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "page-1") {
		t.Errorf("pump with an ended context: %v, want a cancellation naming page-1", err)
	}
	if ts.busy || len(ts.queue) != 1 || q.completed {
		t.Errorf("after the ended pump: busy %v, queue %d, item completed %v; want the item left for the next sender", ts.busy, len(ts.queue), q.completed)
	}
}
