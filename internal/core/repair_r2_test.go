package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the second W1b review
// (docs/notes/w1b.md, "Review repairs, round 2").

// daemonDefaults is the daemon's TransportFunc for a daemon started with
// "--transport iwdp --addr <listing>": a request without transport or
// address ("" or "auto") gets the configured transport and listing. It
// records whether each call allowed a transport start.
type daemonDefaults struct {
	listing string
	mu      sync.Mutex
	starts  []bool
}

func (d *daemonDefaults) get(ctx context.Context, kind, addr string) (transport.Transport, error) {
	d.mu.Lock()
	d.starts = append(d.starts, TransportStartAllowed(ctx))
	d.mu.Unlock()
	if kind == "" || kind == "auto" {
		kind = "iwdp"
		if addr == "" {
			addr = d.listing
		}
	}
	if kind != "iwdp" || addr == "" {
		return nil, fmt.Errorf("unexpected transport %q at %q", kind, addr)
	}
	return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
}

// R2-2, D11 and design 2.5: an explicit page URL in a request with the
// production defaults (no transport, no address) is resolved against the
// daemon's configured listing, here on a custom port outside iwdp's
// default device ports. A listed physical page then needs the reservation
// before any connection, and classifying the URL never starts iwdp.
func TestDefaultRequestURLUsesTheDaemonListing(t *testing.T) {
	d := &daemonDefaults{}
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: d.get})
	d.listing = r.l.Addr()
	q := PageRequest{Page: r.f.URL}
	if iwdpDevicePort(q.Page) {
		t.Fatalf("fixture URL %s is on a default iwdp port; the test needs a custom port", q.Page)
	}
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "a", ClassRead)
	if !errors.Is(err, ErrNoReservation) || exitCode(err) != CodeReservation {
		t.Fatalf("Begin by URL without a reservation: %v (exit %d), want ErrNoReservation (exit 6)", err, exitCode(err))
	}
	if n := r.f.ConnectionCount(); n != 0 {
		t.Fatalf("connections = %d, want 0: the refused attach dialed the page", n)
	}
	d.mu.Lock()
	for i, allowed := range d.starts {
		if allowed {
			t.Errorf("transport call %d for the URL allowed an iwdp start", i)
		}
	}
	d.mu.Unlock()
	r.reserve(t, "agent")
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "b", ClassRead)
	if err != nil {
		t.Fatalf("Begin by URL with the reservation: %v", err)
	}
	r.m.End(mo.ID)
	if !strings.HasPrefix(mo.Page.Key, "iwdp|"+r.dev.ID+"|") || !mo.Page.target.physical {
		t.Fatalf("page key %q (physical %v), want the listed page of device %s", mo.Page.Key, mo.Page.target.physical, r.dev.ID)
	}
}

// A URL that the daemon's listing does not have stays a local endpoint
// (WebKitGTK, the legacy mock): no reservation, and it attaches.
func TestDefaultRequestUnlistedURLIsLocal(t *testing.T) {
	d := &daemonDefaults{}
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: d.get})
	d.listing = r.l.Addr()
	local := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(local.Close)
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), PageRequest{Page: local.URL}, agent, "a", ClassRead)
	if err != nil {
		t.Fatalf("Begin on an unlisted local URL: %v", err)
	}
	r.m.End(mo.ID)
	if mo.Page.target.physical || !strings.HasPrefix(mo.Page.Key, "url|") {
		t.Fatalf("page key %q (physical %v), want a local URL record", mo.Page.Key, mo.Page.target.physical)
	}
}

// countingIWDP is iwdp's default-listing transport whose device list is
// never read for real: the test counts the reads and fails them.
type countingIWDP struct {
	*iwdp.Transport
	reads *int
}

func (c countingIWDP) Devices(context.Context) ([]transport.Device, error) {
	*c.reads++
	return nil, errors.New("the test does not read the default iwdp listing")
}

// selfBusyRig is a physical page behind a listing with iwdp's frontend on,
// where the page is listed busy while any client (the daemon included) is
// connected; a second page keeps the frontend hint on.
func selfBusyRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, fakewebkit.Options{}, true, ManagerOptions{})
	r.reserve(t, "agent")
	r.dev.AddPage("Other", "https://other.example/", "ws://127.0.0.1:1/x")
	r.dev.ServedBy(r.pg, r.f)
	return r
}

// stallSecondCommand attaches by discovery, runs a second discovery
// command (whose listing shows the page busy: the daemon's own connection)
// and stalls it, which starts the background recovery.
func stallSecondCommand(t *testing.T, r *rig) *Page {
	t.Helper()
	ctx := ctxFor(t, 5*time.Second)
	a, err := r.m.Begin(ctx, r.req, agent, "first", ClassRead)
	if err != nil {
		t.Fatalf("first command: %v", err)
	}
	r.m.End(a.ID)
	mo, err := r.m.Begin(ctx, r.req, agent, "second", ClassExclusive)
	if err != nil {
		t.Fatalf("second command on the daemon's own page: %v", err)
	}
	mo.Page.mu.Lock()
	own := mo.Page.target.page.Ownership
	mo.Page.mu.Unlock()
	if own != transport.OwnershipBusy {
		t.Fatalf("stored busy hint = %v, want busy (the listing shows the daemon's own connection)", own)
	}
	r.f.StallConnection(1)
	_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"hang"}`))
	r.m.End(mo.ID)
	return mo.Page
}

// R2-3, design 2.6 step 3: the recovery does not mistake the daemon's own
// connection for another inspector. It retires that connection, lists the
// page again and reconnects.
func TestRecoveryIgnoresItsOwnBusyHint(t *testing.T) {
	r := selfBusyRig(t)
	pg := stallSecondCommand(t, r)
	eventually(t, 10*time.Second, "the recovery to reconnect", func() bool {
		return r.f.ConnectionCount() == 2 && pg.ConnState() == ConnOK
	})
	// The recovery installs the new connection and only then returns and
	// clears pg.recovering (Manager.recover), so the flag can still be set
	// for a moment after the state shows the connection attached. The
	// recovery must end: the flag clears, and the page is still attached on
	// the second connection afterwards.
	eventually(t, 10*time.Second, "the recovery to end after the reconnect (pg.recovering cleared)", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return !pg.recovering
	})
	if n, st := r.f.ConnectionCount(), pg.ConnState(); n != 2 || st != ConnOK {
		t.Fatalf("after the recovery ended: %d connections, state %s; want 2 and attached", n, st)
	}
}

// Design 2.5 rule 2 stays in force for the recovery: a page that another
// client holds after the daemon closed its own connection is refused, and
// the page stays stalled with the busy reason.
func TestRecoveryRefusesAForeignBusyPage(t *testing.T) {
	r := selfBusyRig(t)
	pg := stallSecondCommand(t, r)
	r.dev.SetBusy(r.pg, true) // another inspector client holds the page
	eventually(t, 10*time.Second, "the recovery to fail", func() bool {
		pg.conn.mu.Lock()
		defer pg.conn.mu.Unlock()
		return errors.Is(pg.conn.stallErr, ErrRecoveryFailed)
	})
	pg.conn.mu.Lock()
	err := pg.conn.stallErr
	pg.conn.mu.Unlock()
	if !errors.Is(err, ErrPageBusy) {
		t.Fatalf("recovery failure %v, want ErrPageBusy", err)
	}
	if n := r.f.ConnectionCount(); n != 1 {
		t.Fatalf("connections = %d, want 1: the recovery dialed a page that another client holds", n)
	}
	if st := pg.ConnState(); st != ConnStalled {
		t.Fatalf("state %s, want stalled", st)
	}
}

// recoverOnce stalls the page's first connection after the calls and waits
// until the background recovery installed the second one.
func recoverOnce(t *testing.T, r *rig, mo *ManagedOp) {
	t.Helper()
	r.f.StallConnection(1)
	_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"hang"}`))
	r.m.End(mo.ID)
	eventually(t, 10*time.Second, "the recovery to reconnect", func() bool {
		return r.f.ConnectionCount() == 2 && mo.Page.ConnState() == ConnOK
	})
}

// onConn returns the params of method that the fake received on
// connection conn.
func onConn(f *fakewebkit.Server, method string, conn int) []string {
	var out []string
	for _, rec := range f.ReceivedMethod(method) {
		if rec.Conn == conn {
			out = append(out, string(rec.Params))
		}
	}
	return out
}

// breakpointRig answers Debugger.setBreakpointByUrl with a new id per call
// (bp-1, bp-2, ...), as a new backend would.
func breakpointRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	var mu sync.Mutex
	n := 0
	r.f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return map[string]any{"breakpointId": fmt.Sprintf("bp-%d", n), "locations": []any{}}, nil
	})
	return r
}

const bpByURL = `{"url":"https://fixture.example/app.js","lineNumber":3}`

// R2-4, design 2.4 and 2.6: a reconnect re-applies the stable breakpoint
// specifications (URL breakpoint, DOMDebugger URL and event breakpoints)
// on the new connection, and the release removes the breakpoint the new
// connection has, not the id of the old one.
func TestRecoveryReplaysStableBreakpoints(t *testing.T) {
	r := breakpointRig(t)
	mo := rawSeq(t, r, ctxFor(t, 5*time.Second),
		[2]string{"Debugger.enable", ""},
		[2]string{"Debugger.setBreakpointByUrl", bpByURL},
		[2]string{"DOMDebugger.setEventBreakpoint", `{"breakpointType":"listener","eventName":"click"}`},
		[2]string{"DOMDebugger.setURLBreakpoint", `{"url":"api","isRegex":false}`})
	recoverOnce(t, r, mo)
	for _, m := range []string{"Debugger.setBreakpointByUrl", "DOMDebugger.setEventBreakpoint", "DOMDebugger.setURLBreakpoint"} {
		if got := onConn(r.f, m, 2); len(got) != 1 {
			t.Errorf("%s on the new connection: %v, want it replayed once", m, got)
		}
	}
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := onConn(r.f, "Debugger.removeBreakpoint", 2); len(got) != 1 || !strings.Contains(got[0], `"bp-2"`) {
		t.Fatalf("Debugger.removeBreakpoint at release: %v, want the new connection's bp-2", got)
	}
	if got := onConn(r.f, "DOMDebugger.removeEventBreakpoint", 2); len(got) != 1 {
		t.Errorf("DOMDebugger.removeEventBreakpoint at release: %v, want 1", got)
	}
	if got := onConn(r.f, "DOMDebugger.removeURLBreakpoint", 2); len(got) != 1 {
		t.Errorf("DOMDebugger.removeURLBreakpoint at release: %v, want 1", got)
	}
}

// R2-4: after a reconnect, a client that removes the breakpoint by the id
// it got (bp-1) removes the breakpoint of the new connection (bp-2), and
// the release then has nothing left to remove.
func TestRemoveBreakpointAfterRecoveryUsesTheCurrentID(t *testing.T) {
	r := breakpointRig(t)
	mo := rawSeq(t, r, ctxFor(t, 5*time.Second),
		[2]string{"Debugger.enable", ""},
		[2]string{"Debugger.setBreakpointByUrl", bpByURL})
	recoverOnce(t, r, mo)
	rm := rawSeq(t, r, ctxFor(t, 5*time.Second), [2]string{"Debugger.removeBreakpoint", `{"breakpointId":"bp-1"}`})
	r.m.End(rm.ID)
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got := onConn(r.f, "Debugger.removeBreakpoint", 2)
	if len(got) != 1 || !strings.Contains(got[0], `"bp-2"`) {
		t.Fatalf("Debugger.removeBreakpoint on the new connection: %v, want one removal of bp-2", got)
	}
}

// R2-4: state bound to the old document's node or script ids (a forced
// pseudo state, a DOM breakpoint, a breakpoint at a script location) does
// not survive a new connection; it is dropped, and no reset with a stale id
// is sent at release.
func TestRecoveryDropsDocumentScopedState(t *testing.T) {
	r := breakpointRig(t)
	r.f.Result("Debugger.setBreakpoint", map[string]any{"breakpointId": "script-bp", "actualLocation": map[string]any{}})
	mo := rawSeq(t, r, ctxFor(t, 5*time.Second),
		[2]string{"Debugger.enable", ""},
		[2]string{"CSS.forcePseudoState", `{"nodeId":7,"forcedPseudoClasses":["hover"]}`},
		[2]string{"DOMDebugger.setDOMBreakpoint", `{"nodeId":9,"type":"subtree-modified"}`},
		[2]string{"Debugger.setBreakpoint", `{"location":{"scriptId":"41","lineNumber":2}}`})
	recoverOnce(t, r, mo)
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, m := range []string{"CSS.forcePseudoState", "DOMDebugger.setDOMBreakpoint", "DOMDebugger.removeDOMBreakpoint", "Debugger.setBreakpoint", "Debugger.removeBreakpoint"} {
		if got := onConn(r.f, m, 2); len(got) != 0 {
			t.Errorf("%s on the new connection: %v, want none (stale node or script ids)", m, got)
		}
	}
}
