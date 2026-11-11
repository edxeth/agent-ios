package cdp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design 5.2 with the core release lifecycle (W5a rebase onto
// core-events; core release_lifecycle.go: no release ends while the model
// holds an owner it removes): a detach whose cleanup operation cannot open
// before cleanupTimeout, because another operation of the session holds
// the page, still removes the attachment's desired state, so no later
// setup applies the detached attachment's header again. The page-side
// cleanup stays pending and runs once the page is free.
func TestDetachBehindABlockerForgetsItsState(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	_, s := r.pageSession(t, sid)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Detached": "1"}})
	if h := s.pg.PS.State.EffectiveHeaders(); h["X-Detached"] != "1" {
		t.Fatalf("fixture: effective headers %v, want X-Detached", h)
	}
	mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "test blocker", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	defer func() {
		if !ended {
			r.m.End(mo.ID)
		}
	}()
	f := r.fake["One"]
	mark := len(f.Received())
	start := time.Now()
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	if took := time.Since(start); took < cleanupTimeout {
		t.Fatalf("fixture: the detach returned after %v, before its cleanup could time out behind the blocker (%v)", took, cleanupTimeout)
	}
	if slices.Contains(s.pg.PS.State.AllOwners(), s.owner) {
		t.Fatalf("the owner %v of the detached attachment is still in the desired state: %v", s.owner, s.pg.PS.State.AllOwners())
	}
	if h := s.pg.PS.State.EffectiveHeaders(); h["X-Detached"] != "" {
		t.Fatalf("the detached attachment left effective headers %v", h)
	}
	r.m.End(mo.ID)
	ended = true
	eventually(t, 5*time.Second, "the pending cleanup disables Network once the page is free", func() bool {
		for _, rec := range f.Received()[mark:] {
			if rec.Method == "Network.disable" {
				return true
			}
		}
		return false
	})
	for _, rec := range f.Received()[mark:] {
		if strings.Contains(string(rec.Params), "X-Detached") {
			t.Fatalf("the page received the detached attachment's header again: %s %s", rec.Method, rec.Params)
		}
	}
}

// Review W5a rebase F3 (TestReviewDetachDoesNotStrandSibling): two CDP
// clients of session s1 hold attachments on page One, and A's attachment
// owns a header. The connection stalls, and the background recovery's
// setup snapshot still holds A's header while A detaches; the detach waits
// behind the recovery's barrier past cleanupTimeout and removes A's owner
// without an operation (ForgetOwner), which changes the release
// generation. The recovery must not install that snapshot, but the lease
// and its epoch are unchanged, so it sets up again with the current state
// and installs, and B's attachment works again without another command.
func TestDetachDuringRecoveryDoesNotStrandSibling(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	a, b := dial(t, ep.WS), dial(t, ep.WS)
	attach := func(c *cdptest.Client) string {
		call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
		return decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	}
	sa, sb := attach(a), attach(b)
	call(t, a, sa, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Departed": "1"}})
	var pg *core.Page
	var epoch uint64
	for _, s := range r.m.Status() {
		if s.Title == "One" {
			epoch, pg = s.Epoch, r.m.LookupPage(s.Key)
		}
	}
	held, release := make(chan struct{}), make(chan struct{})
	var seen, freed sync.Once
	unblock := func() { freed.Do(func() { close(release) }) }
	defer unblock()
	f := r.fake["One"]
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 1}})
	var hmu sync.Mutex
	headers := map[int]string{} // connection -> last header map sent on it
	f.Handle("Network.setExtraHTTPHeaders", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		hmu.Lock()
		headers[c.Conn] = string(c.Params)
		hmu.Unlock()
		if c.Conn == 2 { // the recovery's first setup snapshot
			seen.Do(func() { close(held) })
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		return nil, nil
	})
	f.StallConnection(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	mo, err := r.m.BeginLeased(ctx, pg, "s1", epoch, "test stall", core.ClassRead)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, err = r.m.RawAs(ctx, mo, core.SessionOwner("legacy", mo.Session), "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`))
	r.m.End(mo.ID)
	cancel()
	if err == nil {
		t.Fatal("fixture: a call on the stalled connection succeeded")
	}
	select {
	case <-held:
	case <-time.After(8 * time.Second):
		t.Fatal("fixture: the recovery did not send the attachment's header in its setup")
	}
	call(t, a, "", "Target.detachFromTarget", map[string]any{"sessionId": sa})
	unblock()
	eventually(t, 5*time.Second, "the recovery ends", func() bool {
		l := r.log.text()
		return strings.Contains(l, "background recovery failed") || strings.Contains(l, "background recovery reconnected")
	})
	if _, err := b.Call(ctxFor(t, 2*time.Second), sb, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatalf("the backend is healthy and the lease unchanged, but the surviving attachment is stranded after its sibling's detach: %v; log:\n%s", err, r.log.text())
	}
	if s, e := r.leaseOf(t, "One"); s != "s1" || e != epoch {
		t.Fatalf("lease after the recovery %s/%d, want s1/%d", s, e, epoch)
	}
	hmu.Lock()
	defer hmu.Unlock()
	for conn, h := range headers {
		if conn > 2 && strings.Contains(h, "X-Departed") {
			t.Fatalf("connection %d got the detached attachment's header: %s", conn, h)
		}
	}
}
