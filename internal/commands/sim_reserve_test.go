package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
)

// While an owner reserves the simulator, its pages are that owner's run:
// another owner's tab list hides them and its page commands fail with exit
// 6; the owner itself, and everyone once the reservation ends, use them.
// A reservation of every physical device (*) does not take the simulator.
func TestSimulatorPagesBelongToTheReservingOwner(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Shop", "http://10.0.2.2:18080/")
	if _, err := res.Reserve(core.AnyDevice, "phone-agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	if out := mustIn(t, m, "b", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("a reservation of every physical device hid the simulator:\n%s", out)
	}
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	r, err := runIn(t, m, "b", "tab", "list")
	if err != nil || strings.Contains(r.Text, "Shop") || r.Data.(map[string]any)["hidden"] != 1 {
		t.Fatalf("another owner's tab list: %q, %v, %v", r.Text, r.Data, err)
	}
	if _, err := runIn(t, m, "b", "eval", "--page", "t1", "1+1"); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
		t.Fatalf("another owner's page command: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
	if out := mustIn(t, m, "a", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("the owner's tab list:\n%s", out)
	}
	if _, err := runIn(t, m, "a", "eval", "--page", "t1", "1+1"); err != nil {
		t.Fatalf("the owner's page command: %v", err)
	}
	if err := res.Release(core.SimulatorDevice, "a"); err != nil {
		t.Fatal(err)
	}
	if out := mustIn(t, m, "b", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("after the release:\n%s", out)
	}
}

func withSimReservations(t *testing.T) *core.Reservations {
	t.Helper()
	res := &core.Reservations{Dir: t.TempDir()}
	old, oldPoll := SimReservations, simReservePoll
	SimReservations = func() (*core.Reservations, error) { return res, nil }
	simReservePoll = 10 * time.Millisecond
	t.Cleanup(func() { SimReservations, simReservePoll = old, oldPoll })
	return res
}

func simInput(owner string, wait time.Duration) Input {
	return Input{Flags: map[string]any{"owner": owner, "ttl": 30 * time.Minute, "wait": wait}}
}

// sim reserve takes a free simulator at once, waits while another owner
// holds it and takes it when that owner releases it, and after --wait
// fails with exit 3 and a message that says to run it again.
func TestSimReserveWaitsForTheOtherOwner(t *testing.T) {
	res := withSimReservations(t)
	ctx := context.Background()
	if _, err := runSimReserve(ctx, nil, simInput("a", 0)); err != nil {
		t.Fatal(err)
	}
	_, err := runSimReserve(ctx, nil, simInput("b", 50*time.Millisecond))
	if ExitCodeOf(err) != ExitWaitTimeout || !strings.Contains(err.Error(), "'agent-ios sim reserve' again") {
		t.Fatalf("a held simulator: %v (exit %d), want exit 3 that says to run it again", err, ExitCodeOf(err))
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = res.Release(core.SimulatorDevice, "a")
	}()
	r, err := runSimReserve(ctx, nil, simInput("b", 5*time.Second))
	if err != nil || !r.Data.(simReservation).Reserved {
		t.Fatalf("after a's release: %+v, %v", r, err)
	}
	if mine, _, _ := res.Check(core.SimulatorDevice, "b"); mine == nil {
		t.Fatal("b holds no reservation after sim reserve")
	}
}

// lineEnv records the lines a command emits.
type lineEnv struct {
	Env
	lines []string
}

func (e *lineEnv) Emit(line string) { e.lines = append(e.lines, line) }

// While it waits, sim reserve says at once, once, that this is another
// agent's turn and to run it again if it is cut off, naming no owner: an
// agent whose tool timed out before the wait ended stopped instead (live
// run, 2026-11-24). A free simulator prints no such line.
func TestSimReserveSaysAtOnceThatItWaits(t *testing.T) {
	res := withSimReservations(t)
	env := &lineEnv{}
	if _, err := runSimReserve(context.Background(), env, simInput("a", 0)); err != nil || len(env.lines) != 0 {
		t.Fatalf("free simulator: %v, lines %q", err, env.lines)
	}
	_, _ = runSimReserve(context.Background(), env, simInput("b", 100*time.Millisecond))
	if len(env.lines) != 1 || !strings.Contains(env.lines[0], "run 'agent-ios sim reserve' again") || strings.Contains(env.lines[0], `"a"`) {
		t.Fatalf("waiting lines: %q", env.lines)
	}
	_ = res
}

// sim reserve refuses the default session as owner: every agent that sets
// no session is "default", and one such agent's forgotten reservation held
// the simulator for the others (live run, 2026-11-24). It refuses qa-login,
// the session of the guides' examples, too: agents that copy an example
// unchanged would share one turn (closure review of the skill rewrite).
func TestSimReserveNeedsAnOwnerOfItsOwn(t *testing.T) {
	res := withSimReservations(t)
	for _, in := range []Input{
		{Flags: map[string]any{"ttl": 10 * time.Minute, "wait": time.Duration(0)}, Global: Globals{Session: "default"}},
		{Flags: map[string]any{"owner": "default", "ttl": 10 * time.Minute, "wait": time.Duration(0)}},
		{Flags: map[string]any{"ttl": 10 * time.Minute, "wait": time.Duration(0)}, Global: Globals{Session: "qa-login"}},
		{Flags: map[string]any{"owner": "qa-login", "ttl": 10 * time.Minute, "wait": time.Duration(0)}, Global: Globals{Session: "agent-7"}},
	} {
		var u *UsageError
		if _, err := runSimReserve(context.Background(), nil, in); !errors.As(err, &u) {
			t.Fatalf("%+v: %v, want a usage error", in, err)
		}
	}
	if l, _ := res.List(); len(l) != 0 {
		t.Fatalf("a refused sim reserve left %+v", l)
	}
	if _, err := runSimReserve(context.Background(), nil, Input{Flags: map[string]any{"ttl": 10 * time.Minute, "wait": time.Duration(0)}, Global: Globals{Session: "agent-7"}}); err != nil {
		t.Fatalf("a named session: %v", err)
	}
}

// simGate refuses another owner's sim command while the simulator is
// reserved, renews the owner's own reservation, and admits everyone on a
// free simulator.
func TestSimGate(t *testing.T) {
	res := withSimReservations(t)
	gate := func(owner string) error {
		_, end, err := simGate(context.Background(), simInput(owner, 0))
		if err == nil {
			end(&err)
		}
		return err
	}
	if err := gate("x"); err != nil {
		t.Fatalf("free simulator: %v", err)
	}
	r, err := res.Reserve(core.SimulatorDevice, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate("b"); ExitCodeOf(err) != ExitReservation {
		t.Fatalf("another owner: %v, want exit 6", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := gate("a"); err != nil {
		t.Fatal(err)
	}
	mine, _, _ := res.Check(core.SimulatorDevice, "a")
	if mine == nil || !mine.Expires.After(r.Expires) {
		t.Fatalf("the owner's sim command did not renew its reservation: %+v (was until %s)", mine, r.Expires)
	}
}

// In private mode the turn-taking errors still tell the agent what to do.
func TestPrivateSimReservationLines(t *testing.T) {
	if l := privateErrorLine("sim reserve", ExitWaitTimeout); !strings.Contains(l, "run 'agent-ios sim reserve' again") {
		t.Errorf("sim reserve exit 3: %q", l)
	}
	// sim up's page that never shows: private mode still says what to check.
	if l := privateErrorLine("sim up", ExitNoMatch); !strings.Contains(l, "10.0.2.2") {
		t.Errorf("sim up exit 7 in private mode: %q", l)
	}
	if l := privateErrorLine("sim tap", ExitReservation); !strings.Contains(l, "reserved by another agent") {
		t.Errorf("sim tap exit 6: %q", l)
	}
}

// resEnv is mgrEnv with a reservations store, as the daemon's env has.
type resEnv struct {
	*mgrEnv
	res *core.Reservations
}

func (e *resEnv) Reservations() *core.Reservations { return e.res }

// sim release ends the turn: it closes the session's page leases, so the
// next agent's commands never meet them, and releases the reservation; it
// refuses another owner's reservation and changes nothing then (live run,
// 2026-11-24: an agent released without close, and its lease stayed on
// its tab).
func TestSimReleaseClosesTheSession(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Shop", "http://10.0.2.2:18080/")
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustIn(t, m, "a", "tab", "list")
	if _, err := runIn(t, m, "a", "eval", "--page", "t1", "1+1"); err != nil {
		t.Fatal(err)
	}
	release := func(session string) (Result, error) {
		inv, err := Parse([]string{"sim", "release"}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		inv.Input.Global.Session = session
		return inv.Cmd.Run(context.Background(), &resEnv{&mgrEnv{m: m}, res}, inv.Input)
	}
	if _, err := release("b"); ExitCodeOf(err) != ExitReservation {
		t.Fatalf("b released a's reservation: %v", err)
	}
	if len(sessionPages(m, "a")) != 1 {
		t.Fatal("b's refused release touched a's lease")
	}
	r, err := release("a")
	if err != nil || r.Data.(map[string]any)["leases"] != 1 {
		t.Fatalf("release: %+v, %v", r, err)
	}
	if len(sessionPages(m, "a")) != 0 {
		t.Fatal("a's lease outlived sim release")
	}
	if mine, other, _ := res.Check(core.SimulatorDevice, "a"); mine != nil || other != nil {
		t.Fatal("the reservation outlived sim release")
	}
}

// While the simulator is reserved (agents take turns), a session that
// chose no tab gets no page of a simulator that shows several tabs: the
// others may be other agents' work (live run, 2026-11-24: an agent's first
// open navigated another run's tab). With one tab, or once the session
// selected its tab, commands run. Without a reservation, a session that
// chose no tab gets no page of several either (any device: exit 5).
func TestSimulatorNeedsAChosenTab(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Earlier run", "http://10.0.2.2:18080/")
	if _, err := res.Reserve(core.SimulatorDevice, "b", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "b", "eval", "1+1"); err != nil {
		t.Fatalf("one tab: %v", err)
	}
	f.add("Mine", "http://10.0.2.2:8791/")
	_, err := runIn(t, m, "c", "--reservation", "b", "eval", "1+1")
	if ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), "tab find") {
		t.Fatalf("two tabs, none chosen: %v (exit %d)", err, ExitCodeOf(err))
	}
	if list := mustIn(t, m, "b", "tab", "list"); !strings.Contains(list, "t2") {
		t.Fatalf("tab list:\n%s", list)
	}
	mustIn(t, m, "b", "tab", "t2")
	if _, err := runIn(t, m, "b", "eval", "1+1"); err != nil {
		t.Fatalf("after tab t2: %v", err)
	}
	if err := res.Release(core.SimulatorDevice, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "d", "eval", "1+1"); ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), "tab new") {
		t.Fatalf("no reservation, two tabs, none chosen: %v (exit %d); want exit 5 with the advice", err, ExitCodeOf(err))
	}
}

// A page lease on the simulator keeps its owner's simulator reservation
// alive: the daemon's sweep renews it, as for a physical device, so a
// turn that works on its page alone does not lose the simulator after the
// TTL.
func TestSimulatorLeaseRenewsTheReservation(t *testing.T) {
	now := time.Now()
	res := &core.Reservations{Dir: t.TempDir(), Now: func() time.Time { return now }}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Mine", "http://10.0.2.2:8791/")
	r, err := res.Reserve(core.SimulatorDevice, "a", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "a", "eval", "1+1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	m.Sweep(time.Now())
	mine, _, _ := res.Check(core.SimulatorDevice, "a")
	if mine == nil || !mine.Expires.After(r.Expires) {
		t.Fatalf("the sweep did not renew the simulator reservation of a leasing owner: %+v (was until %s)", mine, r.Expires)
	}
}

// device release by an owner that holds only the simulator releases
// nothing, so it fails and names sim release, keeping the reservation
// (live run 13: an agent ended its turn so and the others waited).
func TestDeviceReleaseNamesSimRelease(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, _ := newTabManagerWith(t, res)
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	inv, err := Parse([]string{"device", "release", "--owner", "a"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.Cmd.Run(context.Background(), &resEnv{&mgrEnv{m: m}, res}, inv.Input)
	if ExitCodeOf(err) != ExitReservation || !strings.Contains(err.Error(), "sim release") {
		t.Fatalf("device release of a simulator holder: %v", err)
	}
	if mine, _, _ := res.Check(core.SimulatorDevice, "a"); mine == nil {
		t.Fatal("the refused device release ended the simulator reservation")
	}
	if l := privateErrorLine("device release", ExitReservation); !strings.Contains(l, "sim release") {
		t.Errorf("private line: %q", l)
	}
}
