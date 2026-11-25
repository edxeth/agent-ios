package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/sim"
	"github.com/edxeth/agent-ios/internal/transport"
)

// Regression tests of the simulator's turns (review of 2026-11-24,
// F1-F10): a turn holds on every way to the simulator and its pages, for
// as long as the work runs.

// simClock is a reservations clock that a test moves while other
// goroutines read it.
type simClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *simClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// waitUntil waits for cond, at most until the test's deadline.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end, ok := t.Deadline()
	for !cond() {
		if ok && time.Now().After(end.Add(-time.Second)) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockedSimUp starts runSimUp for owner with a fake Mac whose first ssh
// call blocks until the command's context ends or release is closed. It
// returns once that call runs, and the channel of runSimUp's error.
func blockedSimUp(t *testing.T, res *core.Reservations, owner string) (release chan struct{}, result chan error) {
	t.Helper()
	prevRes, prevSim, prevHold := SimReservations, newSim, simHoldEvery
	SimReservations = func() (*core.Reservations, error) { return res, nil }
	simHoldEvery = time.Millisecond
	entered, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	newSim = func(Env) (*sim.Sim, error) {
		cfg := sim.Config{SSH: "fake"}
		return &sim.Sim{Cfg: cfg, Mac: sim.NewMac(cfg, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return nil, errors.New("fake ssh: released")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})}, nil
	}
	t.Cleanup(func() { SimReservations, newSim, simHoldEvery = prevRes, prevSim, prevHold })
	go func() {
		_, err := runSimUp(context.Background(), nil, simInput(owner, 0))
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("sim up ended before its first ssh call: %v", err)
	}
	return release, result
}

// F1: a running sim command keeps its owner's turn for its whole run: it
// renews the reservation while it runs (a sim up can run 15 minutes, the
// turn's TTL is 2), so another owner cannot reserve the simulator after
// the TTL measured from the command's start.
func TestSimCommandRenewsItsTurnWhileItRuns(t *testing.T) {
	clock := &simClock{now: time.Now()}
	res := &core.Reservations{Dir: t.TempDir(), Now: clock.Now}
	r, err := res.Reserve(core.SimulatorDevice, "a", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	release, result := blockedSimUp(t, res, "a")
	clock.add(time.Minute)
	waitUntil(t, "the running sim up renews the reservation", func() bool {
		mine, _, err := res.Check(core.SimulatorDevice, "a")
		return err == nil && mine != nil && mine.Expires.After(r.Expires)
	})
	clock.add(time.Minute + time.Second) // past the TTL from the command's start
	if _, err := res.Reserve(core.SimulatorDevice, "b", 2*time.Minute); !errors.Is(err, core.ErrReserved) {
		t.Fatalf("another owner reserved the simulator while a's sim up ran: %v", err)
	}
	close(release)
	if err := <-result; errors.Is(err, errSimTurnLost) {
		t.Fatalf("sim up lost its own turn: %v", err)
	}
}

// F1: a running sim command stops acting when its turn ends: when its
// owner's reservation is released (or expires), and, admitted on a free
// simulator, when another owner reserves it. It fails with exit 6.
func TestSimCommandStopsWhenItsTurnEnds(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		_, result := blockedSimUp(t, res, "a")
		if err := res.Release(core.SimulatorDevice, "a"); err != nil {
			t.Fatal(err)
		}
		if err := <-result; ExitCodeOf(err) != ExitReservation || !errors.Is(err, errSimTurnLost) {
			t.Fatalf("sim up after its reservation was released: %v (exit %d), want exit 6", err, ExitCodeOf(err))
		}
		if mine, other, _ := res.Check(core.SimulatorDevice, "a"); mine != nil || other != nil {
			t.Fatal("the running command took the released turn again")
		}
	})
	t.Run("taken while free", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		_, result := blockedSimUp(t, res, "a")
		if _, err := res.Reserve(core.SimulatorDevice, "b", time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := <-result; ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
			t.Fatalf("sim up on the free simulator after b reserved it: %v (exit %d), want exit 6", err, ExitCodeOf(err))
		}
	})
}

// F1: sim reserve refuses a --ttl that the renewals of a turn in use
// cannot keep (the sweep renews every 5 s, only a live reservation).
func TestSimReserveRefusesATTLShorterThanTheRenewals(t *testing.T) {
	for _, c := range []struct {
		ttl string
		ok  bool
	}{{"1s", false}, {"29s", false}, {"30s", true}, {"2m", true}} {
		_, err := Parse([]string{"sim", "reserve", "--ttl", c.ttl}, func(string) string { return "" })
		if c.ok != (err == nil) || (!c.ok && ExitCodeOf(err) != ExitUsage) {
			t.Errorf("sim reserve --ttl %s: %v (exit %d), accepted %t", c.ttl, err, ExitCodeOf(err), c.ok)
		}
	}
}

// F2: the reservation applies to a page that the daemon is already
// attached to: a session that used the simulator's page in its turn gets
// exit 6 on it, by its id, once another owner reserved the simulator.
func TestSimulatorTurnOnAnAttachedPage(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Earlier", "http://10.0.2.2:8791/")
	if _, err := res.Reserve(core.SimulatorDevice, "old", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustIn(t, m, "old", "eval", "1+1")
	if _, err := m.ReleaseSession(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := res.Release(core.SimulatorDevice, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve(core.SimulatorDevice, "new", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"eval", "--page", "page-1", "1+1"}, {"eval", "1+1"}} {
		if _, err := runIn(t, m, "old", argv...); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
			t.Errorf("old session %v on the attached page in new's turn: %v (exit %d), want exit 6", argv, err, ExitCodeOf(err))
		}
	}
}

// Review F15: an owner refused on a reserved simulator page gets no page
// item: the page observer, which the daemon streams to the client, never
// sees the page's title or key.
func TestRefusedOwnerObservesNoSimulatorPage(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Private page", "http://10.0.2.2:8791/")
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	var seen []string
	ctx := core.WithPageObserver(context.Background(), func(key, title string) { seen = append(seen, key, title) })
	if _, _, err := m.Touch(ctx, core.PageRequest{Page: "Private"}, core.AcquireOptions{Session: "b"}); !errors.Is(err, core.ErrReserved) {
		t.Fatalf("another owner: %v, want ErrReserved", err)
	}
	if len(seen) != 0 {
		t.Fatalf("the refused owner observed %q", seen)
	}
}

// F2: a lease on the simulator's page that its owner took on the free
// simulator ends in the daemon's sweep once another owner reserves it, so
// nothing bound to the lease goes on in that owner's turn; the owner's own
// lease stays.
func TestSweepEndsAnotherOwnersSimulatorLease(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Page", "http://10.0.2.2:8791/")
	mustIn(t, m, "a", "eval", "1+1")
	if _, err := res.Reserve(core.SimulatorDevice, "b", time.Hour); err != nil {
		t.Fatal(err)
	}
	m.Sweep(time.Now())
	if n := len(sessionPages(m, "a")); n != 0 {
		t.Fatalf("a's lease outlived b's reservation by a sweep: %d lease(s)", n)
	}
	mustIn(t, m, "b", "eval", "1+1")
	m.Sweep(time.Now())
	if n := len(sessionPages(m, "b")); n != 1 {
		t.Fatalf("the sweep ended the reserving owner's own lease: %d lease(s)", n)
	}
}

// F2: an operation under a lease held from before (a CDP attachment's
// BeginLeased) sends nothing once another owner reserves the simulator,
// before any sweep ends the lease.
func TestHeldSimulatorLeaseSendsNothingInAnotherOwnersTurn(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Page", "http://10.0.2.2:8791/")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	pg, epoch, err := m.Touch(ctx, core.PageRequest{}, core.AcquireOptions{Session: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	mo, err := m.BeginLeased(ctx, pg, "bob", epoch, "cdp probe", core.ClassRead)
	if err != nil {
		t.Fatalf("bob's held lease on the free simulator: %v", err)
	}
	m.End(mo.ID)
	if _, err := res.Reserve(core.SimulatorDevice, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginLeased(ctx, pg, "bob", epoch, "cdp probe", core.ClassRead); !errors.Is(err, core.ErrReserved) {
		t.Fatalf("bob's held lease in alice's turn: %v, want ErrReserved", err)
	}
}

// failingSimPages is a tabFake whose page list fails while fail is set.
type failingSimPages struct {
	*tabFake
	fail atomic.Bool
}

func (f *failingSimPages) Pages(ctx context.Context, id string) ([]transport.Page, error) {
	if f.fail.Load() {
		return nil, errors.New("fake: the page list failed")
	}
	return f.tabFake.Pages(ctx, id)
}

// F4: while the simulator is reserved, the owner's session that chose no
// tab is refused (exit 5) whenever the simulator shows several tabs: also
// with a default page that it used while there was one, and when the other
// tab is not debuggable. A count that cannot be read is no proof of one
// tab. Another owner gets exit 6, before any tab choice.
func TestSimulatorTabChoiceOnEveryAdmission(t *testing.T) {
	setup := func(t *testing.T) (*core.Manager, *tabFake, *core.Reservations) {
		res := &core.Reservations{Dir: t.TempDir()}
		m, f := newTabManagerWith(t, res)
		f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
		if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		return m, f, res
	}
	t.Run("default page of one tab", func(t *testing.T) {
		m, f, _ := setup(t)
		f.add("Earlier", "http://10.0.2.2:18080/")
		mustIn(t, m, "a", "eval", "1+1")
		f.add("Mine", "http://10.0.2.2:8791/")
		if _, err := runIn(t, m, "a", "eval", "1+1"); ExitCodeOf(err) != ExitNoPage {
			t.Fatalf("the default page after a second tab appeared: %v (exit %d), want exit 5", err, ExitCodeOf(err))
		}
	})
	t.Run("tab that is not debuggable", func(t *testing.T) {
		m, f, _ := setup(t)
		f.add("Earlier", "http://10.0.2.2:18080/")
		f.add("Mine", "http://10.0.2.2:8791/")
		f.mu.Lock()
		f.pages[1].Debuggable = false
		f.mu.Unlock()
		if _, err := runIn(t, m, "a", "eval", "1+1"); ExitCodeOf(err) != ExitNoPage {
			t.Fatalf("two tabs, one not debuggable: %v (exit %d), want exit 5", err, ExitCodeOf(err))
		}
	})
	t.Run("count that fails", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		f := &failingSimPages{tabFake: &tabFake{t: t, kind: transport.KindIWDP, device: core.SimulatorDevice}}
		m := core.NewManager(core.ManagerOptions{Reservations: res, Transport: func(context.Context, string, string) (transport.Transport, error) { return f, nil }})
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		f.add("Earlier", "http://10.0.2.2:18080/")
		mustIn(t, m, "a", "eval", "1+1") // a default page while the simulator is free
		if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		f.fail.Store(true)
		if _, err := runIn(t, m, "a", "eval", "1+1"); err == nil {
			t.Fatal("a failed count of the simulator's tabs admitted the default page")
		}
	})
	t.Run("another owner", func(t *testing.T) {
		m, f, _ := setup(t)
		f.add("A", "http://10.0.2.2:18080/")
		f.add("B", "http://10.0.2.2:8791/")
		if _, err := runIn(t, m, "b", "eval", "1+1"); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
			t.Fatalf("another owner, two tabs: %v (exit %d), want exit 6", err, ExitCodeOf(err))
		}
	})
}

// A simulator reservation that cannot be read admits no page command on
// the simulator (exit 6): it fails closed.
func TestUnreadableSimulatorReservationAdmitsNothing(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Page", "http://10.0.2.2:8791/")
	if err := os.WriteFile(filepath.Join(res.Dir, "device-broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "a", "eval", "1+1"); ExitCodeOf(err) != ExitReservation {
		t.Fatalf("an unreadable reservation: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
}

// F5: a lease renews its owner's reservation of the leased device only: a
// phone's lease does not keep the owner's unused simulator turn alive past
// its TTL, and a simulator lease does not renew the owner's phone.
func TestLeaseRenewsOnlyItsDevicesReservation(t *testing.T) {
	for _, c := range []struct {
		name, leased, unused string
	}{
		{"phone lease", "phone-1", core.SimulatorDevice},
		{"simulator lease", core.SimulatorDevice, "phone-1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := &simClock{now: time.Now()}
			res := &core.Reservations{Dir: t.TempDir(), Now: clock.Now}
			m, f := newTabManagerWith(t, res)
			f.kind, f.device = transport.KindIWDP, c.leased
			f.add("Page", "http://10.0.2.2:8791/")
			leased, err := res.Reserve(c.leased, "a", 2*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			unused, err := res.Reserve(c.unused, "a", 2*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			mustIn(t, m, "a", "eval", "1+1")
			clock.add(time.Minute)
			m.Sweep(time.Now())
			mine, _, err := res.Check(c.unused, "a")
			if err != nil || mine == nil || !mine.Expires.Equal(unused.Expires) {
				t.Errorf("the lease on %s renewed the reservation of %s: %+v, %v (was until %s)", c.leased, c.unused, mine, err, unused.Expires)
			}
			mine, _, err = res.Check(c.leased, "a")
			if err != nil || mine == nil || !mine.Expires.After(leased.Expires) {
				t.Errorf("the lease on %s did not renew its own reservation: %+v, %v", c.leased, mine, err)
			}
		})
	}
}

// simAndPhone lists the simulator of a tabFake and a phone with one page.
type simAndPhone struct{ *tabFake }

func (d simAndPhone) Devices(context.Context) ([]transport.Device, error) {
	return []transport.Device{{ID: core.SimulatorDevice, Transport: transport.KindIWDP}, {ID: "phone-1", Transport: transport.KindIWDP}}, nil
}

func (d simAndPhone) Pages(ctx context.Context, id string) ([]transport.Page, error) {
	if id == "phone-1" {
		return []transport.Page{{ID: "phone-page", DeviceID: id, Title: "Phone", URL: "https://phone.test/", Debuggable: true}}, nil
	}
	return d.tabFake.Pages(ctx, id)
}

// F6: tab find --device searches one device: the simulator's owner finds
// its tab although a phone that it has not reserved hides a page, and a
// phone's owner searches its phone while another owner holds the
// simulator. Without --device the hidden pages still stop tab find (exit
// 6), and a device that the listing does not have is exit 5.
func TestTabFindOnOneDevice(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	f := &tabFake{t: t, kind: transport.KindIWDP, device: core.SimulatorDevice}
	var reads atomic.Int32
	addAt(f, "Simulator", "http://10.0.2.2:8791/", &reads, false)
	m := core.NewManager(core.ManagerOptions{Reservations: res, Transport: func(context.Context, string, string) (transport.Transport, error) { return simAndPhone{f}, nil }})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	if _, err := res.Reserve(core.SimulatorDevice, "sim-agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve("phone-1", "phone-agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "sim-agent", "tab", "find", "http://10.0.2.2:8791/"); ExitCodeOf(err) != ExitReservation || !strings.Contains(err.Error(), "--device SIMULATOR") {
		t.Fatalf("tab find over every device with a hidden phone page: %v (exit %d), want exit 6 naming --device", err, ExitCodeOf(err))
	}
	if out := mustIn(t, m, "sim-agent", "tab", "find", "--device", "SIMULATOR", "http://10.0.2.2:8791/"); !strings.HasPrefix(out, "t") {
		t.Fatalf("tab find --device SIMULATOR: %q", out)
	}
	if _, err := runIn(t, m, "phone-agent", "tab", "find", "--device", "phone-1", "http://10.0.2.2:8791/"); ExitCodeOf(err) != ExitNoMatch {
		t.Fatalf("tab find --device phone-1 while the simulator is another's: %v (exit %d), want exit 7", err, ExitCodeOf(err))
	}
	if _, err := runIn(t, m, "sim-agent", "tab", "find", "--device", "nowhere", "http://10.0.2.2:8791/"); ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("tab find --device of an unlisted device: %v (exit %d), want exit 5", err, ExitCodeOf(err))
	}
	if out := mustIn(t, m, "sim-agent", "tab", "list", "--device", "SIMULATOR"); strings.Contains(out, "not listed") {
		t.Fatalf("tab list --device SIMULATOR counted the phone's page as hidden:\n%s", out)
	}
}

// F9: device reserve and release refuse the simulator (exit 2, naming the
// sim command): they would bypass sim reserve's refusal of the default
// owner and its short TTL, and sim release's closing of the page leases.
func TestDeviceCommandsRefuseTheSimulator(t *testing.T) {
	for _, argv := range [][]string{
		{"device", "reserve", "--device", "SIMULATOR"},
		{"device", "release", "--device", "SIMULATOR", "--owner", "a"},
		{"device", "release", "--device", "simulator"},
	} {
		_, err := Parse(argv, func(string) string { return "" })
		if ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), "agent-ios sim ") {
			t.Errorf("%v: %v (exit %d), want exit 2 naming the sim command", argv, err, ExitCodeOf(err))
		}
	}
}

// F10: with --json, sim reserve's waiting line stays off stdout, which is
// the JSON result alone: it goes to stderr, and in private mode, whose
// stderr is one status line, nowhere.
func TestSimReserveJSONKeepsStdoutJSON(t *testing.T) {
	res := withSimReservations(t)
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	prev := simHintOut
	t.Cleanup(func() { simHintOut = prev })
	for _, private := range []bool{false, true} {
		var stderr bytes.Buffer
		simHintOut = &stderr
		env := &lineEnv{}
		in := simInput("b", 0)
		in.Global.JSON, in.Global.Private = true, private
		if _, err := runSimReserve(context.Background(), env, in); ExitCodeOf(err) != ExitWaitTimeout {
			t.Fatalf("a held simulator: %v", err)
		}
		if len(env.lines) != 0 {
			t.Errorf("--json (private %t) streamed text on stdout: %q", private, env.lines)
		}
		if got := strings.Contains(stderr.String(), "waiting:"); got == private {
			t.Errorf("--json (private %t): stderr %q", private, stderr.String())
		}
	}
}

// The end of a simulator turn after a failed sim up or a failed selection
// (closure review of the skill rewrite, 2026-11-24, GLM F1): the session
// selected no tab, the simulator shows one tab, an earlier run's, and
// "tab close" without an id would close it, as the listing's first page.
// While the session's owner holds the turn it refuses (exit 5) and the tab
// stays; once the session selected the tab, "tab close" closes it.
func TestTabCloseOnTheSimulatorClosesOnlyTheSelectedTab(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Earlier", "http://10.0.2.2:18080/")
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "a", "tab", "close"); ExitCodeOf(err) != ExitNoPage || !errors.Is(err, core.ErrNoPage) {
		t.Fatalf("tab close without a selection in a's turn: %v (exit %d), want exit 5", err, ExitCodeOf(err))
	}
	if n := len(tabsOf(f)); n != 1 {
		t.Fatalf("the refused tab close closed the earlier run's tab: %d tab(s) left", n)
	}
	mustIn(t, m, "a", "tab", "list")
	mustIn(t, m, "a", "tab", "t1")
	mustIn(t, m, "a", "tab", "close")
	if n := len(tabsOf(f)); n != 0 {
		t.Fatalf("tab close of the selected tab left %d tab(s)", n)
	}
}

// tabsOf is the page list of f.
func tabsOf(f *tabFake) []transport.Page {
	p, _ := f.Pages(context.Background(), "")
	return p
}
