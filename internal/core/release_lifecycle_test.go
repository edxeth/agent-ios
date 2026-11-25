package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// The core-events reviewer's round-2 reproductions
// (/home/devkit/.pi/artifacts/reviewer/core-events-r2-repro/lifecycle_test.go),
// kept as regression tests.

// A release can start and end while a takeover is still collecting the
// setup replies of its new connection. That setup sent the released
// session's state, so the connection must not be installed.
func TestReviewReleaseCompletesDuringConnect(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	enabled := map[int]bool{}
	r.f.Handle("Network.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		mu.Lock()
		enabled[c.Conn] = true
		mu.Unlock()
		return nil, nil
	})
	r.f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		mu.Lock()
		enabled[c.Conn] = false
		mu.Unlock()
		return nil, nil
	})
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lost", func() bool { return pg.ConnState() == ConnLost })
	r.f.DelayReplies("Network.enable", 700*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, _, err := r.m.Touch(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent", Takeover: true})
		done <- err
	}()
	eventually(t, 3*time.Second, "takeover setup", func() bool {
		for _, rec := range r.f.ReceivedMethod("Network.enable") {
			if rec.Conn == 3 {
				return true
			}
		}
		return false
	})
	if err := r.m.Break(ctx, pg.Key); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pg.PS.State.Needs("Network") {
		t.Fatal("released ownership remains in model")
	}
	mu.Lock()
	leaked := enabled[3]
	mu.Unlock()
	if leaked && r.f.ConnectionCount() == 3 {
		t.Fatal("installed pre-release snapshot after release completed: s1 Network enabled on s2 connection")
	}
}

// Ending the lease must not leave a released owner's model entries behind
// when another change occupies the change slot past the release deadline.
//
// The reviewer's version also required the release to report the
// change-slot deadline (err != nil). That described the round-2 design,
// in which the release waited for the slot; it is no requirement
// (orchestrator decision, core-events round 3). The release now removes
// the owners without the slot, so it returns before its deadline, the
// lease ends and the model holds no owner of s1.
func TestReviewLostReleaseForgetsOwnersAtDeadline(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lost", func() bool { return pg.ConnState() == ConnLost })
	_, end, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const releaseDeadline = 80 * time.Millisecond
	short, cancel := context.WithTimeout(context.Background(), releaseDeadline)
	start := time.Now()
	err = r.m.Break(short, pg.Key)
	took := time.Since(start)
	cancel()
	if err != nil {
		t.Errorf("release of a lost page failed: %v", err)
	}
	if took >= releaseDeadline {
		t.Errorf("release took %v, not less than its deadline %v: it waited for the change slot", took, releaseDeadline)
	}
	if err := end(nil); err != nil {
		t.Fatal(err)
	}
	pg.mu.Lock()
	ended := pg.lease == nil && !pg.releasing
	pg.mu.Unlock()
	if !ended {
		t.Fatal("release did not end")
	}
	if pg.PS.State.Needs("Network") {
		t.Fatalf("ended lease retained released owner after deadline: %v", pg.PS.State.AllOwners())
	}
}

// headerHolder reports whether the records of target (all targets: "")
// after seq set header name and no Network.disable followed on target.
func headerAfter(f *fakewebkit.Server, target, name string) (set bool, last int) {
	for _, rec := range f.Received() {
		if target != "" && rec.Target != target {
			continue
		}
		switch rec.Method {
		case "Network.disable":
			set = false
		case "Network.setExtraHTTPHeaders":
			var p struct {
				Headers map[string]string `json:"headers"`
			}
			_ = json.Unmarshal(rec.Params, &p)
			if _, ok := p.Headers[name]; ok {
				set, last = true, rec.Seq
			}
		}
	}
	return set, last
}

// A change of the released session that waits for the change lock while
// the release runs adds nothing to the model when it gets the lock: its
// operation ended (release_lifecycle.go, refusedLocked). The release
// itself could not take the lock before its deadline, so it reports the
// timeout, removes s1's owners anyway, and its page-side cleanup stays
// pending until the background reset runs once the lock is free.
func TestStaleChangeOfAReleasedSessionAddsNoOwner(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-A":"1"}}`)); err != nil {
		t.Fatal(err)
	}
	pg := mo.Page
	_, end, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The bootstrap part is added before its send, so without the fence it
	// would stay in the model although the send is refused.
	stale := make(chan error, 1)
	go func() {
		_, err := r.m.Raw(ctx, mo.ID, "Page.setBootstrapScript", json.RawMessage(`{"source":"window.stale = 1"}`))
		stale <- err
	}()
	time.Sleep(300 * time.Millisecond) // the call waits for the change lock
	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err = r.m.Break(short, pg.Key)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("release whose cleanup could not take the change lock: err %v, want the deadline (exit 124)", err)
	}
	if o := ownersOf(pg.PS.State, "s1"); len(o) > 0 {
		t.Fatalf("the release ended with s1's owners in the model: %v", o)
	}
	disablesBefore := len(r.f.ReceivedMethod("Network.disable"))
	if err := end(nil); err != nil {
		t.Fatal(err)
	}
	if err := <-stale; err == nil {
		t.Error("the change of the released session reported success")
	}
	if o := ownersOf(pg.PS.State, "s1"); len(o) > 0 {
		t.Fatalf("the change of the released session added owners after its release: %v", o)
	}
	// The pending cleanup: Network lost its only owner, so the background
	// reset disables it once the lock is free.
	eventually(t, 5*time.Second, "the reset of the unknown agents", func() bool {
		return len(pg.PS.State.unknownDomains()) == 0 && len(r.f.ReceivedMethod("Network.disable")) > disablesBefore
	})
	if set, _ := headerAfter(r.f, "", "X-A"); set {
		t.Error("the released session's header is still set on the page after the reset")
	}
}

// A provisional target whose setup sent a snapshot with the released
// session's state is set up again with the state that remains (Apply),
// also when the release removed the owners without the change lock and so
// queued nothing for the target.
func TestProvisionalSetupRunsAgainAfterARelease(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Sess-s1":"1"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	_, end, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = end(nil) }()
	r.f.DelayReplies("Network.setExtraHTTPHeaders", 400*time.Millisecond)
	r.f.Provisional("page-2")
	eventually(t, 3*time.Second, "the provisional setup's header", func() bool {
		set, _ := headerAfter(r.f, "page-2", "X-Sess-s1")
		return set
	})
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = r.m.Break(short, pg.Key) // the cleanup cannot take the lock; the owners go anyway
	cancel()
	// The lock is still held, so no change reaches page-2: only a new setup.
	eventually(t, 5*time.Second, "the second setup of page-2", func() bool {
		set, _ := headerAfter(r.f, "page-2", "X-Sess-s1")
		return !set
	})
}

// The next change that uses an agent whose state a release left unknown
// resets it first (release_lifecycle.go, resetAgent): Network, still
// needed, through the reinitialization barrier; Page with its reset, its
// enable and the bootstrap script.
func TestChangeResetsAnUnknownAgentFirst(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	d := mo.Page.PS.State
	for _, tc := range []struct {
		dom, method, params string
		want                []string
	}{
		{"Network", "Network.setExtraHTTPHeaders", `{"headers":{"X-B":"1"}}`, []string{"Network.disable", "Network.enable", "Network.setExtraHTTPHeaders"}},
		{"Page", "Page.setBootstrapScript", `{"source":"window.b = 1"}`, []string{"Page.disable", "Page.setBootstrapScript", "Page.enable", "Page.setBootstrapScript", "Page.setBootstrapScript"}},
	} {
		d.mu.Lock()
		d.unknown = map[string]uint64{tc.dom: 1}
		d.mu.Unlock()
		from := len(r.f.Received())
		if _, err := r.m.Raw(ctx, mo.ID, tc.method, json.RawMessage(tc.params)); err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		var got []string
		for _, rec := range r.f.Received()[from:] {
			if strings.HasPrefix(rec.Method, tc.dom+".") {
				got = append(got, rec.Method)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s with %s unknown: sent %v, want %v", tc.method, tc.dom, got, tc.want)
		}
		if u := d.unknownDomains(); len(u) > 0 {
			t.Errorf("%s: still unknown after the reset: %v", tc.method, u)
		}
	}
}

// The context replay runs as a change and checks Needs("Runtime") inside
// it. When a release removes the last Runtime owner while a replay holds
// the change lock, the replay's Runtime.enable lands after the removal;
// the agent is unknown then, and the release's background reset disables
// it once the replay's change ends.
func TestContextReplayDuringAReleaseLeavesNoUnownedRuntime(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "runtime", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	started, proceed := make(chan struct{}), make(chan struct{})
	replayed := make(chan error, 1)
	go func() {
		replayed <- pg.PS.State.replayContexts(ctx, func(ctx context.Context) error {
			close(started)
			<-proceed
			_, err := pg.conn.Send(ctx, "Runtime.enable", nil)
			return err
		})
	}()
	<-started
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = r.m.Break(short, pg.Key) // its cleanup cannot take the lock
	cancel()
	if o := ownersOf(pg.PS.State, "s1"); len(o) > 0 {
		t.Fatalf("the release ended with s1's owners in the model: %v", o)
	}
	close(proceed)
	if err := <-replayed; err != nil {
		t.Fatalf("replay: %v", err)
	}
	eventually(t, 5*time.Second, "Runtime disabled after the replay", func() bool {
		var last string
		for _, rec := range r.f.Received() {
			if strings.HasPrefix(rec.Method, "Runtime.") && strings.HasSuffix(rec.Method, "able") {
				last = rec.Method
			}
		}
		return last == "Runtime.disable"
	})
}

// Each addition of owned state is refused while the change in progress
// belongs to an operation that ended (release_lifecycle.go,
// refusedLocked): a release ends the page's operations before it removes
// its session's owners, and nothing may add them back. The change's end
// reports the refusal.
func TestEndedOperationChangeRecordsNoAddition(t *testing.T) {
	d := NewDesiredState()
	cs, end, err := d.beginChange(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.ended = func() bool { return true }
	o := Owner("legacy:s1")
	if d.Acquire(o, "Runtime") {
		t.Error("Acquire reported a first enable")
	}
	if ch := d.SetHeaders(o, map[string]string{"X-A": "1"}); ch.Send != nil || ch.NeedsNetworkReset {
		t.Errorf("SetHeaders asked for a send: %+v", ch)
	}
	d.AddInterception(o, InterceptRule{URL: "x"})
	d.Put(Entry{Key: "k1", Owner: o})
	cs.put(Entry{Key: "k2", Owner: o}, nil)
	if err := d.SetPart(o, "part", RankInit, "window.x = 1"); err != nil {
		t.Errorf("SetPart: %v", err)
	}
	if v := d.SetInterception(o, true); v != nil {
		t.Errorf("SetInterception asked for a send: %v", *v)
	}
	if v := d.SetCachingDisabled(o, true); v != nil {
		t.Errorf("SetCachingDisabled asked for a send: %v", *v)
	}
	if v := d.SetEmulatedConditions(o, json.RawMessage(`{"bytesPerSecondLimit":1}`)); v != nil {
		t.Errorf("SetEmulatedConditions asked for a send: %s", v)
	}
	d.setShared(cs, o, "legacy:Debugger.setPauseOnExceptions", "Debugger.setPauseOnExceptions", "Debugger.setPauseOnExceptions", map[string]any{"state": "all"}, false)
	if owners := d.AllOwners(); len(owners) > 0 {
		t.Errorf("an ended operation's change added owners: %v", owners)
	}
	if !d.Boot.Empty() {
		t.Errorf("an ended operation's change added a bootstrap part: %q", d.Boot.Script())
	}
	if err := end(nil); !errors.Is(err, errChangeRefused) {
		t.Errorf("end: %v, want errChangeRefused", err)
	}
	// The next change, of no operation, records again.
	_, end, err = d.beginChange(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Acquire(o, "Runtime") {
		t.Error("Acquire of a later change was refused")
	}
	if err := end(nil); err != nil {
		t.Fatal(err)
	}
}

// The reset of an unknown Debugger agent that is still needed sends the
// agent reset, the enable, and the Debugger state that remains: the URL
// breakpoint and the shared pause setting, in rank order. The breakpoint at
// a script location named the old agent's script, so it leaves the model.
// A refused reset fails the change that needed it.
func TestResetOfAnUnknownDebuggerReappliesItsState(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	r.f.Result("Debugger.setBreakpointByUrl", map[string]any{"breakpointId": "u1", "locations": []any{}})
	r.f.Result("Debugger.setBreakpoint", map[string]any{"breakpointId": "s1", "actualLocation": map[string]any{"scriptId": "9", "lineNumber": 1}})
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "debugger", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	for _, c := range []struct{ method, params string }{
		{"Debugger.enable", ""},
		{"Debugger.setBreakpointByUrl", `{"lineNumber":3,"url":"https://fixture.example/a.js"}`},
		{"Debugger.setBreakpoint", `{"location":{"scriptId":"9","lineNumber":1}}`},
		{"Debugger.setPauseOnExceptions", `{"state":"all"}`},
	} {
		var p json.RawMessage
		if c.params != "" {
			p = json.RawMessage(c.params)
		}
		if _, err := r.m.Raw(ctx, mo.ID, c.method, p); err != nil {
			t.Fatalf("%s: %v", c.method, err)
		}
	}
	d := mo.Page.PS.State
	d.mu.Lock()
	d.unknown = map[string]uint64{"Debugger": 1}
	d.mu.Unlock()
	from := len(r.f.Received())
	if _, err := r.m.Raw(ctx, mo.ID, "Debugger.setBreakpointsActive", json.RawMessage(`{"active":true}`)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rec := range r.f.Received()[from:] {
		got = append(got, rec.Method)
	}
	want := []string{
		"Debugger.setPauseOnExceptions", "Debugger.setPauseOnDebuggerStatements", "Debugger.disable",
		"Debugger.enable", "Debugger.setBreakpointByUrl", "Debugger.setPauseOnExceptions",
		"Debugger.setBreakpointsActive",
	}
	if !slices.Equal(got, want) {
		t.Errorf("reset of the unknown Debugger agent sent %v, want %v", got, want)
	}
	d.mu.Lock()
	for _, e := range d.entries {
		if e.Scoped {
			t.Errorf("the reset kept the script-location breakpoint %q", e.Key)
		}
	}
	d.unknown = map[string]uint64{"Page": 1}
	d.mu.Unlock()
	r.f.Handle("Page.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Page.disable failed"}
	})
	if _, err := r.m.Raw(ctx, mo.ID, "Page.setBootstrapScript", json.RawMessage(`{"source":"window.c = 1"}`)); err == nil || !strings.Contains(err.Error(), "resetting Page") {
		t.Errorf("a refused reset: err %v, want the reset's failure", err)
	}
	if !d.isUnknown("Page") {
		t.Error("Page is known after its reset failed")
	}
}
