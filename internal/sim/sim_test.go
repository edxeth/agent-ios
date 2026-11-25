package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/quick"
	"time"
)

const (
	udidA = "E485E2B7-A0F9-413B-AB85-EFF4F63398F5"
	udidB = "5375738B-C40D-44C3-9A34-101B41F58EB7"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "simulator.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Load names a missing file, unknown fields and a missing destination, and
// expands ~/ in sshConfig.
func TestLoad(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); !errors.Is(err, ErrNoConfig) || !strings.Contains(err.Error(), "none.json") {
		t.Fatalf("missing file: got %v, want ErrNoConfig naming the path", err)
	}
	if _, err := Load(writeConfig(t, `{"ssh":"m","devcie":"x"}`)); err == nil || !strings.Contains(err.Error(), "devcie") {
		t.Fatalf("unknown field: got %v", err)
	}
	if _, err := Load(writeConfig(t, `{"start":["x"]}`)); err == nil || !strings.Contains(err.Error(), "no ssh destination") {
		t.Fatalf("no destination: got %v", err)
	}
	if _, err := Load(writeConfig(t, `{"ssh":"-oProxyCommand=x"}`)); err == nil {
		t.Fatal("a destination that ssh would read as an option was accepted")
	}
	home, _ := os.UserHomeDir()
	c, err := Load(writeConfig(t, `{"ssh":"m","sshConfig":"~/x/config","device":"iPhone 17 Pro"}`))
	if err != nil || c.SSHConfig != filepath.Join(home, "x/config") || c.Device != "iPhone 17 Pro" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

const devicesJSON = `{"devices":{
 "com.apple.CoreSimulator.SimRuntime.iOS-26-5":[
  {"udid":"` + udidA + `","name":"iPhone 17 Pro","state":"Booted","isAvailable":true},
  {"udid":"` + udidB + `","name":"iPhone 17 Pro Max","state":"Shutdown","isAvailable":true}],
 "com.apple.CoreSimulator.SimRuntime.iOS-26-10":[
  {"udid":"11111111-2222-3333-4444-555555555555","name":"iPhone 17 Pro","state":"Shutdown","isAvailable":true}],
 "com.apple.CoreSimulator.SimRuntime.watchOS-26-0":[
  {"udid":"66666666-2222-3333-4444-555555555555","name":"Apple Watch","state":"Booted","isAvailable":true}],
 "com.apple.CoreSimulator.SimRuntime.iOS-26-4":[
  {"udid":"not-a-udid; rm -rf /","name":"Evil","state":"Shutdown","isAvailable":true}]}}`

// Pick takes a UDID in any case, a name on its newest runtime by version
// number (26.10 after 26.5), or the one booted iOS simulator; it ignores
// other platforms and entries whose UDID could not reach a shell safely.
func TestPick(t *testing.T) {
	ds, err := parseDevices([]byte(devicesJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("parsed %d iOS devices, want 3: %+v", len(ds), ds)
	}
	for _, tc := range []struct{ want, udid string }{
		{strings.ToLower(udidB), udidB},
		{"iPhone 17 Pro", "11111111-2222-3333-4444-555555555555"},
		{"", udidA},
	} {
		d, err := Pick(ds, tc.want)
		if err != nil || d.UDID != tc.udid {
			t.Errorf("Pick(%q) = %+v, %v; want %s", tc.want, d, err, tc.udid)
		}
	}
	if _, err := Pick(ds, "iPhone 99"); !errors.Is(err, ErrNoDevice) || !strings.Contains(err.Error(), "iPhone 17 Pro Max") {
		t.Errorf("unknown name: %v, want ErrNoDevice listing the devices", err)
	}
	for i := range ds {
		ds[i].State = "Shutdown"
	}
	if _, err := Pick(ds, ""); !errors.Is(err, ErrNoDevice) {
		t.Errorf("no booted device: %v, want ErrNoDevice", err)
	}
}

// newer is a strict order on runtimes that agrees with the numbers of the
// versions, so that Pick takes the newest runtime of a name.
func TestNewerProperty(t *testing.T) {
	rt := func(a, b uint8) string { return fmt.Sprintf("iOS %d.%d", a, b) }
	f := func(a1, b1, a2, b2 uint8) bool {
		x, y := rt(a1, b1), rt(a2, b2)
		want := a1 > a2 || (a1 == a2 && b1 > b2)
		antisymmetric := !newer(x, y) || !newer(y, x)
		return newer(x, y) == want && antisymmetric && !newer(x, x)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// OpenURL takes only http and https URLs and passes the URL to the Mac's
// shell as one quoted word, also when it holds a quote.
func TestOpenURL(t *testing.T) {
	fm := &fakeMac{}
	fm.reachable = func() bool { return true }
	mac := NewMac(Config{SSH: "mac"}, fm.run)
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "http://", "not a url"} {
		if err := mac.OpenURL(context.Background(), udidA, bad); err == nil {
			t.Errorf("OpenURL(%q) was accepted", bad)
		}
	}
	if err := mac.OpenURL(context.Background(), udidA, "http://10.0.2.2:8080/a?q='x'; touch /tmp/p"); err != nil {
		t.Fatal(err)
	}
	want := "xcrun simctl openurl " + udidA + " 'http://10.0.2.2:8080/a?q='\\''x'\\''; touch /tmp/p'"
	if got := fm.calls[len(fm.calls)-1]; got != want {
		t.Fatalf("script %q, want %q", got, want)
	}
	out, err := exec.Command("sh", "-c", "printf %s "+shellQuote("a'b; c")).Output()
	if err != nil || string(out) != "a'b; c" {
		t.Fatalf("shellQuote round trip: %q, %v", out, err)
	}
}

// fakeMac answers the ssh calls of Mac and records every call.
type fakeMac struct {
	mu        sync.Mutex
	calls     []string
	reachable func() bool
	loggedIn  bool
	booted    bool
	window    bool
	safari    bool
	safariErr bool // simctl fails when Safari's state is read
	socket    string
	screen    []byte // what the screenshot script prints
	hwKbd     bool   // the Simulator's hardware keyboard setting
	axe       []string
	ui        []byte // what axe describe-ui prints
	other     bool   // another simulator (udidB) is booted
	simKbd    string // the simulator's first keyboard; "" means US, Automatic
	macKbd    string // the Mac's layout; "" means US
	quitErr   bool   // quitting the Simulator app fails
	quits     int    // keyboard switches that quit the app
	watch     bool   // a watchOS simulator is booted
	opened    func() // runs when simctl opens a URL (a tab appears)
}

func (f *fakeMac) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name != "ssh" {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	script := args[len(args)-1]
	f.calls = append(f.calls, script)
	if !f.reachable() {
		return nil, errors.New("ssh: connect: connection refused")
	}
	yes := func(b bool) []byte { return []byte(map[bool]string{true: "yes\n", false: "no\n"}[b]) }
	switch {
	case script == "true":
		return nil, nil
	case strings.HasPrefix(script, "who"):
		return yes(f.loggedIn), nil
	case strings.HasPrefix(script, "xcrun simctl list"):
		state := map[bool]string{true: "Booted", false: "Shutdown"}
		watch := ""
		if f.watch {
			watch = `,"com.apple.CoreSimulator.SimRuntime.watchOS-26-0":[{"udid":"66666666-2222-3333-4444-555555555555","name":"Apple Watch","state":"Booted"}]`
		}
		return []byte(`{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-5":[{"udid":"` + udidA + `","name":"iPhone 17 Pro","state":"` + state[f.booted] + `"},{"udid":"` + udidB + `","name":"iPhone 17 Pro Max","state":"` + state[f.other] + `"}]` + watch + `}}`), nil
	case strings.HasPrefix(script, "xcrun simctl bootstatus"):
		f.booted = true
		return nil, nil
	case strings.HasPrefix(script, "xcrun simctl shutdown"):
		f.booted, f.safari = false, false
		return nil, nil
	case strings.HasPrefix(script, "pgrep -xq Simulator"):
		return yes(f.window), nil
	case strings.HasPrefix(script, "open -a Simulator"):
		f.window = true
		return nil, nil
	case strings.HasPrefix(script, "l=$(xcrun simctl spawn"):
		if f.safariErr {
			return nil, errors.New("exit status 1")
		}
		return yes(f.safari), nil
	case strings.HasPrefix(script, "xcrun simctl launch"):
		f.safari = true
		return nil, nil
	case strings.HasPrefix(script, "xcrun simctl openurl"):
		if f.opened != nil {
			f.opened()
		}
		return nil, nil
	case strings.HasPrefix(script, "defaults read com.apple.iphonesimulator ConnectHardwareKeyboard"):
		return []byte(map[bool]string{true: "1\n", false: "0\n"}[f.hwKbd]), nil
	case strings.HasPrefix(script, "if pgrep -xq Simulator; then osascript"):
		if !strings.Contains(script, "xcrun simctl shutdown "+udidA) || strings.Contains(script, "shutdown all") {
			return nil, fmt.Errorf("fakeMac: the switch would shut down more than the simulator: %q", script)
		}
		if f.quitErr {
			return nil, errors.New("exit status 3: quitting the Simulator app failed")
		}
		f.quits++
		f.hwKbd = strings.HasSuffix(script, "-bool true")
		f.booted, f.window, f.safari = false, false, false
		return nil, nil
	case strings.HasPrefix(script, "k=$(xcrun simctl spawn "+udidA):
		sim, mac := f.simKbd, f.macKbd
		if mac == "" {
			mac = "com.apple.keylayout.US"
		}
		if sim == "-" { // the simulator's keyboards are unreadable
			return []byte("\n" + mac + "\n"), nil
		}
		if sim == "" {
			sim = `    "en_US@sw=QWERTY;hw=Automatic",`
		}
		if mac == "" {
			mac = "com.apple.keylayout.US"
		}
		return []byte(sim + "\n" + mac + "\n"), nil
	case strings.HasPrefix(script, "'/opt/axe dir/axe' "):
		f.axe = append(f.axe, script)
		if strings.Contains(script, "describe-ui") {
			return f.ui, nil
		}
		return nil, nil
	case strings.HasPrefix(script, "d=$(mktemp -d)"):
		if !strings.Contains(script, "xcrun simctl io "+udidA+" screenshot --type=png") {
			return nil, fmt.Errorf("fakeMac: unexpected screenshot script %q", script)
		}
		return f.screen, nil
	case strings.HasPrefix(script, "p=$(pgrep -f 'launchd_sim"):
		if !f.booted {
			return nil, errors.New("exit status 1")
		}
		return []byte(f.socket + "\n"), nil
	}
	return nil, fmt.Errorf("fakeMac: unexpected script %q", script)
}

func (f *fakeMac) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeProc is a process of fakeProcs.
type fakeProc struct {
	start, token string
	listens      bool
	ignoresTERM  bool
}

// fakeProcs is a process table for Forward: Spawn starts an "ssh" that
// listens at once; Signal(SIGTERM) ends a process unless it ignores it.
type fakeProcs struct {
	mu          sync.Mutex
	next        int
	procs       map[int]*fakeProc
	spawned     map[int]fakeProc // every process that Spawn started, as started
	args        [][]string       // the arguments of every Spawn
	terms       []int
	identifyErr error
	probeErr    error        // Signal(pid, 0) fails with it (EPERM: another user's process)
	listenLate  bool         // a spawned ssh does not listen
	ignoreTERM  bool         // a spawned ssh ignores SIGTERM
	stolenPID   bool         // a spawned ssh ends at once and its pid goes to another process
	lateEnv     int          // the first reads of a new process's identity lack its token
	zombies     map[int]bool // processes that ended but are not reaped: signal 0 reaches them, their environment reads as ESRCH
	badTERM     []string
}

func newFakeProcs() *fakeProcs {
	return &fakeProcs{next: 1000, procs: map[int]*fakeProc{}, spawned: map[int]fakeProc{}}
}

func (p *fakeProcs) procsOf() Processes {
	return Processes{
		Spawn: func(token, name string, args ...string) (int, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.next++
			pr := &fakeProc{start: fmt.Sprint("start", p.next), token: token, listens: !p.listenLate, ignoresTERM: p.ignoreTERM}
			p.spawned[p.next] = *pr
			p.args = append(p.args, append([]string{name}, args...))
			if p.stolenPID {
				pr = &fakeProc{start: "other", listens: false}
			}
			p.procs[p.next] = pr
			return p.next, nil
		},
		Identify: func(pid int) (string, string, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.identifyErr != nil {
				return "", "", p.identifyErr
			}
			pr, ok := p.procs[pid]
			if !ok {
				return "", "", errors.New("no such process")
			}
			if p.zombies[pid] {
				return "", "", &os.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/environ", pid), Err: syscall.ESRCH}
			}
			if p.lateEnv > 0 {
				p.lateEnv--
				return pr.start, "", nil // the exec has not set its environment yet
			}
			return pr.start, pr.token, nil
		},
		Signal: func(pid int, sig syscall.Signal) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			pr, ok := p.procs[pid]
			if !ok {
				return syscall.ESRCH
			}
			if sig == 0 && p.probeErr != nil {
				return p.probeErr
			}
			if sig == syscall.SIGTERM {
				p.terms = append(p.terms, pid)
				if s, mine := p.spawned[pid]; !mine || s.start != pr.start || s.token != pr.token {
					p.badTERM = append(p.badTERM, fmt.Sprintf("pid %d %+v", pid, *pr))
				}
				if !pr.ignoresTERM {
					delete(p.procs, pid)
				}
			}
			return nil
		},
		Endpoint: func(_ context.Context, pgid int) (bool, bool, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			own, foreign := false, false
			for pid, pr := range p.procs {
				if pr.listens {
					own = own || pid == pgid
					foreign = foreign || pid != pgid
				}
			}
			return own && !foreign, foreign, nil
		},
		NewToken: func() string {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.next++
			return fmt.Sprint("token", p.next)
		},
	}
}

// liveSpawned counts the processes that Spawn started and that still run
// as started.
func (p *fakeProcs) liveSpawned() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for pid, s := range p.spawned {
		if pr, ok := p.procs[pid]; ok && pr.start == s.start && pr.token == s.token {
			n++
		}
	}
	return n
}

// reuse gives pid to another process, one that this program did not start.
func (p *fakeProcs) reuse(pid int, listens bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.procs[pid] = &fakeProc{start: "other", token: "", listens: listens}
}

func (p *fakeProcs) kill(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.procs, pid)
}

func newForward(t *testing.T, mac Mac, p *fakeProcs) *Forward {
	t.Helper()
	return &Forward{Mac: mac, Dir: t.TempDir(), Addr: "127.0.0.1:27753", Procs: p.procsOf(), Wait: 200 * time.Millisecond}
}

func recordedPID(t *testing.T, f *Forward) int {
	t.Helper()
	data, err := os.ReadFile(f.path())
	var r forwardRecord
	if err != nil || json.Unmarshal(data, &r) != nil {
		t.Fatalf("no forward record: %v", err)
	}
	return r.PID
}

// fakeIWDP stands in for iwdp: a start or restart connects it to the
// simulator.
type fakeIWDP struct {
	st       IWDPState
	starts   int
	restarts int
	leases   []int // ActiveLeases answers these in turn, then the last one
	pagesErr error
	events   *[]string
	foreign  bool     // a start or restart leaves another program's iwdp
	urls     []string // the simulator's page URLs, page i with id "p<i>"; nil: one page
}

func (f *fakeIWDP) State(context.Context) (IWDPState, error) { return f.st, nil }
func (f *fakeIWDP) Start(context.Context) error {
	f.starts++
	f.st = IWDPState{Running: true, Own: !f.foreign, Listed: true}
	return nil
}

func (f *fakeIWDP) Restart(context.Context) error {
	f.restarts++
	if f.events != nil {
		*f.events = append(*f.events, "restart")
	}
	f.st = IWDPState{Running: true, Own: true, Listed: true}
	return nil
}

func (f *fakeIWDP) ActiveLeases(context.Context) (int, error) {
	if len(f.leases) == 0 {
		return 0, nil
	}
	n := f.leases[0]
	if len(f.leases) > 1 {
		f.leases = f.leases[1:]
	}
	return n, nil
}

func (f *fakeIWDP) SimulatorPages(context.Context) ([]Page, error) {
	if f.pagesErr != nil {
		return nil, f.pagesErr
	}
	if !f.st.Listed {
		return nil, nil
	}
	urls := f.urls
	if urls == nil {
		urls = []string{"http://example.test/"}
	}
	pages := make([]Page, len(urls))
	for i, u := range urls {
		pages[i] = Page{ID: fmt.Sprintf("p%d", i), URL: u}
	}
	return pages, nil
}

func newTestSim(t *testing.T, fm *fakeMac, iw *fakeIWDP, cfg Config) (*Sim, *fakeProcs) {
	t.Helper()
	mac := NewMac(cfg, fm.run)
	p := newFakeProcs()
	return &Sim{Cfg: cfg, Mac: mac, Run: fm.run, Forward: newForward(t, mac, p), IWDP: iw, Poll: time.Millisecond, PageWait: 50 * time.Millisecond}, p
}

func readyMac() *fakeMac {
	fm := &fakeMac{socket: "/s/com.apple.webinspectord_sim.socket", loggedIn: true, booted: true, window: true, safari: true}
	fm.reachable = func() bool { return true }
	return fm
}

// From a stopped Mac, Up runs the start command, waits for ssh and the
// login, boots, opens the window, starts Safari, forwards the inspector
// socket and starts iwdp.
func TestUpFromStoppedMac(t *testing.T) {
	started := false
	fm := &fakeMac{socket: "/private/var/tmp/x/com.apple.webinspectord_sim.socket", loggedIn: true}
	fm.reachable = func() bool { return started }
	cfg := Config{SSH: "mac", Start: []string{"vmctl", "start", "mac"}, Device: "iPhone 17 Pro"}
	iw := &fakeIWDP{}
	s, p := newTestSim(t, fm, iw, cfg)
	s.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		started = true
		return fm.run(ctx, name, args...)
	}
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.MacStarted || !r.Booted || !r.WindowOpened || !r.SafariStarted || !r.ForwardStarted || !r.IWDPStarted || r.IWDPRestarted || r.Pages != 1 || r.Device.UDID != udidA {
		t.Fatalf("report %+v: want every step done and iwdp started, not restarted", r)
	}
	if fm.count("vmctl start mac") != 1 || p.liveSpawned() != 1 {
		t.Fatalf("calls %q, %d live forwards", fm.calls, p.liveSpawned())
	}
	// The forward reaches the inspector socket that the Mac named.
	if want := "127.0.0.1:27753:" + fm.socket; !slices.Contains(p.args[0], want) || p.args[0][0] != "ssh" || !slices.Contains(p.args[0], "mac") {
		t.Fatalf("forward %q, want ssh ... -L %s ... mac", p.args[0], want)
	}
}

// When everything runs, Up changes nothing: no boot, no window, no Safari
// launch (which would bring Safari to the front), the same forward, and no
// iwdp start or restart. A new boot's socket replaces the forward and
// restarts iwdp.
func TestUpIdempotent(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{}
	s, p := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if _, err := s.Up(context.Background(), "", "", false); err != nil {
		t.Fatal(err)
	}
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.MacStarted || r.Booted || r.WindowOpened || r.SafariStarted || r.ForwardStarted || r.IWDPStarted || r.IWDPRestarted {
		t.Fatalf("second Up did steps: %+v", r)
	}
	if fm.count("xcrun simctl launch") != 0 || len(p.spawned) != 1 || iw.starts != 1 || iw.restarts != 0 {
		t.Fatalf("%d Safari launches, %d forwards, %d starts, %d restarts", fm.count("xcrun simctl launch"), len(p.spawned), iw.starts, iw.restarts)
	}
	fm.socket = "/t/com.apple.webinspectord_sim.socket"
	iw.st.Listed = false
	r, err = s.Up(context.Background(), "", "", true)
	if err != nil || !r.ForwardStarted || !r.IWDPRestarted || len(p.spawned) != 2 || p.liveSpawned() != 1 {
		t.Fatalf("after a new socket: %+v, %v, %d forwards, %d live", r, err, len(p.spawned), p.liveSpawned())
	}
}

// A forward that serves the socket while iwdp still lists the simulator
// through an older connection is not enough when the forward is new: Up
// restarts iwdp whenever it replaced the forward, also when iwdp lists the
// simulator.
func TestUpRestartsIWDPAfterANewForward(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{st: IWDPState{Running: true, Own: true, Listed: true}}
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil || !r.ForwardStarted || !r.IWDPRestarted {
		t.Fatalf("%+v, %v: a listing from before the new forward must not count", r, err)
	}
}

// Up refuses, before it changes the forward or iwdp, an iwdp that agent-ios
// did not start, and a restart that would end other devices' leases.
func TestUpRefusesBeforeAnyChange(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{st: IWDPState{Running: true, Own: false, Listed: true}}
	s, p := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if _, err := s.Up(context.Background(), "", "", false); !errors.Is(err, ErrNotOwnIWDP) {
		t.Fatalf("foreign iwdp: %v", err)
	}
	if len(p.spawned) != 0 {
		t.Fatal("a forward started before the refusal")
	}
	iw.st.Own = true
	if _, err := s.Up(context.Background(), "", "", false); err != nil { // forward, restart
		t.Fatal(err)
	}
	old := recordedPID(t, s.Forward)
	fm.socket = "/t/com.apple.webinspectord_sim.socket"
	iw.leases = []int{2}
	if _, err := s.Up(context.Background(), "", "", false); err == nil || !strings.Contains(err.Error(), "2 active page lease") {
		t.Fatalf("leases: %v", err)
	}
	if recordedPID(t, s.Forward) != old || len(p.spawned) != 1 || iw.restarts != 1 {
		t.Fatalf("the refused Up changed the forward or iwdp: pid %d (was %d), %d forwards, %d restarts", recordedPID(t, s.Forward), old, len(p.spawned), iw.restarts)
	}
	s.RestartAnyway = true
	if r, err := s.Up(context.Background(), "", "", false); err != nil || !r.IWDPRestarted {
		t.Fatalf("--restart-iwdp: %+v, %v", r, err)
	}
}

// Up fails with the reason when the Mac is down and nothing can start it,
// when nobody logs in, when the URL is not http(s) (before any call), and
// when the page list keeps failing; another program on the forward's port
// is refused.
func TestUpFailures(t *testing.T) {
	fm := &fakeMac{}
	fm.reachable = func() bool { return false }
	s, p := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac"})
	if _, err := s.Up(context.Background(), "", "", false); err == nil || !strings.Contains(err.Error(), "no start command") {
		t.Fatalf("down Mac without start: %v", err)
	}
	before := fm.count("")
	if _, err := s.Up(context.Background(), "", "file:///etc/passwd", false); err == nil || fm.count("") != before {
		t.Fatalf("bad URL: %v after %d calls; want a refusal before any call", err, fm.count("")-before)
	}
	fm.reachable = func() bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Up(ctx, "", "", false); err == nil || !strings.Contains(err.Error(), "logged in") {
		t.Fatalf("no login: %v", err)
	}
	fm.loggedIn, fm.booted, fm.safari, fm.socket = true, true, true, "/s/com.apple.webinspectord_sim.socket"
	p.reuse(4242, true) // another program listens on the port
	if _, err := s.Up(context.Background(), udidA, "", true); err == nil || !strings.Contains(err.Error(), "another program") {
		t.Fatalf("foreign listener: %v", err)
	}
	p.kill(4242)
	s.IWDP = &fakeIWDP{pagesErr: errors.New("connection refused")}
	if _, err := s.Up(context.Background(), udidA, "", true); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("a failing page list: %v, want its error, not 0 tabs", err)
	}
}

// Screenshot returns the simulator's PNG, refuses output that is not a PNG,
// and refuses a simulator that is not booted without starting anything.
func TestScreenshot(t *testing.T) {
	fm := readyMac()
	fm.screen = append([]byte("\x89PNG\r\n\x1a\n"), "rest"...)
	s, _ := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac", Device: udidA})
	d, png, err := s.Screenshot(context.Background(), "")
	if err != nil || d.UDID != udidA || string(png) != string(fm.screen) {
		t.Fatalf("Screenshot = %v, %q, %v", d.UDID, png, err)
	}
	fm.screen = []byte("Wrote screenshot to: -")
	if _, _, err := s.Screenshot(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("non-PNG output: %v", err)
	}
	fm.booted = false
	calls := len(fm.calls)
	if _, _, err := s.Screenshot(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "sim up") {
		t.Fatalf("not booted: %v", err)
	}
	for _, c := range fm.calls[calls:] {
		if strings.Contains(c, "bootstatus") || strings.Contains(c, "open -a") {
			t.Fatalf("Screenshot started something: %q", c)
		}
	}
}

// The touch commands quote every argument for the Mac's shell, name the
// device, and need a configured axe.
func TestTouchRunsAXe(t *testing.T) {
	fm := readyMac()
	s, _ := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac", Device: udidA})
	if _, err := s.Touch(context.Background(), "", "tap", "-x", "1", "-y", "2"); !errors.Is(err, ErrNoAXe) {
		t.Fatalf("no axe configured: %v", err)
	}
	s.Cfg.AXe = "/opt/axe dir/axe"
	s.Mac = NewMac(s.Cfg, fm.run)
	if _, err := s.Touch(context.Background(), "", "tap", "--label", "it's $(x)"); err != nil {
		t.Fatal(err)
	}
	want := "'/opt/axe dir/axe' 'tap' '--label' 'it'\\''s $(x)' --udid " + udidA
	if len(fm.axe) != 1 || fm.axe[0] != want {
		t.Fatalf("axe calls %q, want %q", fm.axe, want)
	}
	// Text that looks like an axe option stays text: it comes after "--".
	fm.hwKbd = true
	if _, err := s.Type(context.Background(), "", "--stdin"); err != nil {
		t.Fatal(err)
	}
	if want := "'/opt/axe dir/axe' 'type' --udid " + udidA + " -- '--stdin'"; fm.axe[1] != want {
		t.Fatalf("type --stdin: %q, want %q", fm.axe[1], want)
	}
}

// With the on-screen keyboard, sim type refuses text that needs Shift
// before typing anything; with the hardware keyboard it types any text.
func TestTypeRefusesShiftWithTheOnscreenKeyboard(t *testing.T) {
	fm := readyMac()
	cfg := Config{SSH: "mac", Device: udidA, AXe: "/opt/axe dir/axe"}
	s, _ := newTestSim(t, fm, &fakeIWDP{}, cfg)
	for _, text := range []string{"A", "a@b", "ü", "x!"} {
		if _, err := s.Type(context.Background(), "", text); err == nil || len(fm.axe) != 0 {
			t.Fatalf("onscreen %q: %v, %d axe calls", text, err, len(fm.axe))
		}
	}
	if _, err := s.Type(context.Background(), "", "hello.world-2/x"); err != nil || len(fm.axe) != 1 || !strings.Contains(fm.axe[0], "'type' '--layout' 'us' --udid "+udidA+" -- 'hello.world-2/x'") {
		t.Fatalf("unshifted text: %v, %q", err, fm.axe)
	}
	for _, kb := range []struct{ sim, mac string }{{`"de_DE@sw=QWERTZ;hw=Automatic",`, ""}, {"", "com.apple.keylayout.German"}, {"-", ""}} {
		fm.simKbd, fm.macKbd = kb.sim, kb.mac
		if _, err := s.Type(context.Background(), "", "abc"); err == nil || len(fm.axe) != 1 {
			t.Fatalf("non-US layout %+v: %v, %d axe calls", kb, err, len(fm.axe))
		}
	}
	fm.simKbd, fm.macKbd = "", ""
	fm.hwKbd = true
	if _, err := s.Type(context.Background(), "", "A@b!"); err != nil || len(fm.axe) != 2 || strings.Contains(fm.axe[1], "--layout") {
		t.Fatalf("hardware keyboard: %v, %q", err, fm.axe)
	}
}

// sim keyboard changes nothing when the mode is already set; checks every
// refusal before it quits the Simulator (an unknown device, another booted
// simulator, a foreign iwdp, an active lease); brings back the simulator
// that was booted when none is configured; and reports a failed quit.
func TestKeyboard(t *testing.T) {
	newKbd := func() (*fakeMac, *fakeIWDP, *Sim) {
		fm := readyMac()
		fm.hwKbd = true
		iw := &fakeIWDP{st: IWDPState{Running: true, Own: true, Listed: true}}
		s, _ := newTestSim(t, fm, iw, Config{SSH: "mac"})
		return fm, iw, s
	}
	_, _, s := newKbd()
	if changed, _, err := s.Keyboard(context.Background(), "", false); err != nil || changed {
		t.Fatalf("already hardware: %v, %v", changed, err)
	}
	for _, refuse := range []struct {
		name string
		set  func(*fakeMac, *fakeIWDP) string
	}{
		{"unknown device", func(*fakeMac, *fakeIWDP) string { return "iPhone 99" }},
		{"other booted", func(f *fakeMac, _ *fakeIWDP) string { f.other = true; return udidA }},
		{"watch booted", func(f *fakeMac, _ *fakeIWDP) string { f.watch = true; return udidA }},
		{"foreign iwdp", func(_ *fakeMac, i *fakeIWDP) string { i.st.Own = false; return "" }},
		{"active lease", func(_ *fakeMac, i *fakeIWDP) string { i.leases = []int{1}; return "" }},
	} {
		fm, iw, s := newKbd()
		dev := refuse.set(fm, iw)
		if _, _, err := s.Keyboard(context.Background(), dev, true); err == nil || fm.quits != 0 || !fm.booted || !fm.hwKbd {
			t.Errorf("%s: %v; quits %d, booted %v, hardware %v; want a refusal before any change", refuse.name, err, fm.quits, fm.booted, fm.hwKbd)
		}
	}
	fm, _, s := newKbd()
	fm.quitErr = true
	if changed, _, err := s.Keyboard(context.Background(), "", true); err == nil || changed || !fm.hwKbd {
		t.Fatalf("failed quit: %v, %v; the setting must stay", changed, err)
	}
	fm, _, s = newKbd()
	changed, r, err := s.Keyboard(context.Background(), "", true)
	if err != nil || !changed || fm.hwKbd || !fm.booted || r.Device.UDID != udidA || !r.Booted || !r.WindowOpened {
		t.Fatalf("switch with no configured device: %v, %+v, %v, hardware %v, booted %v", changed, r, err, fm.hwKbd, fm.booted)
	}
}

// Property: over any preconditions, sim keyboard quits the Simulator only
// when no refusal holds, never shuts down another simulator, and reports a
// change only when the setting flipped and the simulator is booted again.
func TestKeyboardProperty(t *testing.T) {
	f := func(hw, other, foreign, lease, quitErr, onscreen, unknown bool) bool {
		fm := readyMac()
		fm.hwKbd, fm.other, fm.quitErr = hw, other, quitErr
		iw := &fakeIWDP{st: IWDPState{Running: true, Own: !foreign, Listed: true}}
		if lease {
			iw.leases = []int{1}
		}
		s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
		dev := ""
		if unknown {
			dev = "iPhone 99"
		}
		changed, _, err := s.Keyboard(context.Background(), dev, onscreen)
		needed := hw == onscreen
		refused := unknown || other || foreign || lease
		switch {
		case !needed:
			return err == nil && !changed && fm.quits == 0
		case refused:
			return err != nil && !changed && fm.quits == 0 && fm.hwKbd == hw
		case quitErr:
			return err != nil && !changed && fm.hwKbd == hw
		}
		return err == nil && changed && fm.hwKbd == !onscreen && fm.booted && fm.other == other
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// sim ui keeps the elements with a label, value or identifier, in order,
// with their frames.
func TestParseUI(t *testing.T) {
	out := []byte(`[{"type":"Application","AXLabel":"Safari","frame":{"x":0,"y":0,"width":402,"height":874},"children":[
	 {"type":"Group","AXLabel":null,"frame":{"x":0,"y":0,"width":1,"height":1},"children":[
	  {"type":"Button","AXLabel":"Done","AXUniqueId":"done","frame":{"x":341,"y":513,"width":40,"height":38},"children":[]}]},
	 {"type":"Slider","AXLabel":"Scroll","AXValue":"43%","frame":{"x":369,"y":62,"width":30,"height":404}}]}]`)
	els, err := parseUI(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(els) != 3 || els[1].Label != "Done" || els[1].ID != "done" || els[1].X != 341 || els[1].H != 38 || els[2].Value != "43%" {
		t.Fatalf("parseUI = %+v", els)
	}
	if _, err := parseUI([]byte("Error: no simulator")); err == nil {
		t.Fatal("parseUI accepted text that is not JSON")
	}
	// A number keeps all its digits.
	els, err = parseUI([]byte(`[{"type":"Cell","AXLabel":"id","AXValue":12345678901234567890,"frame":{}}]`))
	if err != nil || len(els) != 1 || els[0].Value != "12345678901234567890" {
		t.Fatalf("big number: %+v, %v", els, err)
	}
	// AXe reports numeric and boolean values too.
	els, err = parseUI([]byte(`[{"type":"Slider","AXLabel":"Volume","AXValue":0.5,"frame":{"x":0,"y":0,"width":1,"height":1}},{"type":"Switch","AXLabel":"Wi-Fi","AXValue":true,"frame":{}},{"type":"Cell","AXLabel":"x","AXValue":{"a":1},"frame":{}}]`))
	if err != nil || len(els) != 3 || els[0].Value != "0.5" || els[1].Value != "true" || els[2].Value != "" {
		t.Fatalf("scalar values: %+v, %v", els, err)
	}
}

// Property: parseUI lists, in pre-order, exactly the nodes of any tree that
// have a label, a value or an identifier, with their own fields.
func TestParseUIProperty(t *testing.T) {
	type node map[string]any
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		var want []string
		var gen func(depth int) node
		n := 0
		gen = func(depth int) node {
			n++
			nd := node{"type": fmt.Sprint("T", n), "frame": map[string]any{"x": n, "y": 0, "width": 1, "height": 1}}
			named := false
			if rng.Intn(2) == 0 {
				nd["AXLabel"] = fmt.Sprint("L", n)
				named = true
			}
			switch rng.Intn(4) {
			case 0:
				nd["AXValue"] = fmt.Sprint("V", n)
				named = true
			case 1:
				nd["AXValue"] = n
				named = true
			case 2:
				nd["AXValue"] = nil
			}
			if named {
				want = append(want, fmt.Sprint("T", n))
			}
			var kids []node
			if depth < 4 {
				for range rng.Intn(4) {
					kids = append(kids, gen(depth+1))
				}
			}
			nd["children"] = kids
			return nd
		}
		var roots []node
		for range 1 + rng.Intn(3) {
			roots = append(roots, gen(0))
		}
		data, _ := json.Marshal(roots)
		els, err := parseUI(data)
		if err != nil || len(els) != len(want) {
			return false
		}
		for i, e := range els {
			if e.Type != want[i] || (e.Label != "" && e.Label != "L"+e.Type[1:]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// The switch's script, run by a real shell against stub programs: it
// fails when the simulator's state cannot be read, and then writes no
// setting; when the state reads booted, it shuts down only that simulator
// and writes the setting last.
func TestSetHardwareKeyboardScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatal(err)
	}
	run := func(listOK bool) (string, error) {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		stub := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> "+log+"\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		stub("pgrep", "exit 1")
		stub("osascript", "exit 0")
		stub("defaults", "exit 0")
		list := "echo \"    iPhone 17 Pro (" + udidA + ") (Booted)\""
		if !listOK {
			list = "exit 1"
		}
		stub("xcrun", "case \"$1 $2\" in \"simctl list\") "+list+" ;; esac")
		var got []byte
		m := NewMac(Config{SSH: "mac"}, func(_ context.Context, _ string, args ...string) ([]byte, error) {
			cmd := exec.Command("sh", "-c", args[len(args)-1])
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
			out, err := cmd.CombinedOutput()
			got = out
			return out, err
		})
		err := m.SetHardwareKeyboard(context.Background(), udidA, false)
		b, _ := os.ReadFile(log)
		return string(b) + string(got), err
	}
	out, err := run(false)
	if err == nil || strings.Contains(out, "defaults write") || !strings.Contains(out, "reading the simulator state failed") {
		t.Fatalf("a failed state read: %v\n%s", err, out)
	}
	out, err = run(true)
	if err != nil || !strings.Contains(out, "xcrun simctl shutdown "+udidA) || strings.Contains(out, "shutdown all") ||
		strings.Index(out, "defaults write") < strings.Index(out, "xcrun simctl shutdown") {
		t.Fatalf("a booted simulator: %v\n%s", err, out)
	}
}

// One failed ssh login (a busy Mac) is not a down Mac: Reachable asks
// again before it answers no.
func TestReachableAsksTwice(t *testing.T) {
	calls := 0
	m := NewMac(Config{SSH: "mac"}, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("ssh: connect to host: Connection timed out")
		}
		return nil, nil
	})
	if ok, err := m.Reachable(context.Background()); !ok || err != nil || calls != 2 {
		t.Fatalf("Reachable = %v, %v after %d calls; want true after a second try", ok, err, calls)
	}
	calls = 0
	down := NewMac(Config{SSH: "mac"}, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("connection refused")
	})
	if ok, err := down.Reachable(context.Background()); ok || err != nil || calls != 2 {
		t.Fatalf("down Mac: %v, %v after %d calls", ok, err, calls)
	}
}

// With --url, Up returns once Safari lists a new tab of that URL, compared
// as tab find compares it: in the browser's form of scheme, host, port and
// path (a missing path is "/", a default port and dot segments do not
// count), with the query and fragment as given. It fails, saying how to
// check the server, when no new tab shows it within PageWait.
func TestUpWaitsForTheURLsTab(t *testing.T) {
	cases := []struct {
		target, listed string
		want           bool
	}{
		{"http://10.0.2.2:8791/", "http://10.0.2.2:18080/", false},
		{"http://10.0.2.2:8791/index.html?x=1#top", "http://10.0.2.2:8791/index.html?x=1#top", true},
		{"http://10.0.2.2:8791/index.html?x=1", "http://10.0.2.2:8791/index.html?x=1#top", false},
		{"http://10.0.2.2:8791/index.html", "http://10.0.2.2:8791/index.html?x=1", false},
		{"http://10.0.2.2:8791/index.html?x=2", "http://10.0.2.2:8791/index.html?x=1", false},
		{"http://10.0.2.2:8791/?", "http://10.0.2.2:8791/", false},
		{"http://10.0.2.2:8791", "http://10.0.2.2:8791/", true},
		{"http://10.0.2.2:80/a/../b", "http://10.0.2.2/b", true},
		{"https://Example.TEST:443/./a", "https://example.test/a", true},
	}
	for _, c := range cases {
		fm := readyMac()
		iw := &fakeIWDP{urls: []string{"http://10.0.2.2:18080/"}}
		fm.opened = func() { iw.urls = append(iw.urls, c.listed) }
		s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
		r, err := s.Up(context.Background(), "", c.target, false)
		if c.want && (err != nil || !r.TabOpened || r.Pages != 2) {
			t.Errorf("Up(%s) with the new tab %s = %+v, %v; want its tab", c.target, c.listed, r, err)
		}
		if !c.want && (!errors.Is(err, ErrNoURLTab) || !strings.Contains(err.Error(), "127.0.0.1")) {
			t.Errorf("Up(%s) with the new tab %s = %v; want ErrNoURLTab that says how to check the server", c.target, c.listed, err)
		}
	}
}

// A tab that was open before Up, an earlier run's with the same URL, is
// no proof that Safari opened the URL now (review F7): Up waits for a new
// tab and fails with ErrNoURLTab, naming the earlier tab, when none comes.
func TestUpNeedsANewTabOfTheURL(t *testing.T) {
	const u = "http://10.0.2.2:8791/"
	fm := readyMac()
	iw := &fakeIWDP{urls: []string{u}}
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	_, err := s.Up(context.Background(), "", u, false)
	if !errors.Is(err, ErrNoURLTab) || !strings.Contains(err.Error(), "open before") {
		t.Fatalf("only an earlier tab shows the URL: %v, want ErrNoURLTab naming the earlier tab", err)
	}
	// Review F17: the earlier tab is another turn's; the error never
	// advises closing it, and points to a URL of the caller's own.
	if strings.Contains(strings.ToLower(err.Error()), "close") || !strings.Contains(err.Error(), "of your own") {
		t.Fatalf("the error advises closing another turn's tab, or no unique URL: %v", err)
	}
	fm.opened = func() { iw.urls = append(iw.urls, u) }
	if r, err := s.Up(context.Background(), "", u, false); err != nil || !r.TabOpened || r.Pages != 2 {
		t.Fatalf("a new tab of the URL beside the earlier one: %+v, %v", r, err)
	}
}

// A page list that fails is not proof that no tab shows the URL (review
// F8): Up returns the listing error, not ErrNoURLTab (exit 7), whether the
// list fails before or after the open.
func TestUpListingFailureIsNotANoURLTab(t *testing.T) {
	listingErr := errors.New("listing unavailable")
	for _, failBefore := range []bool{true, false} {
		fm := readyMac()
		iw := &fakeIWDP{}
		if failBefore {
			iw.pagesErr = listingErr
		} else {
			fm.opened = func() { iw.pagesErr = listingErr }
		}
		s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
		_, err := s.Up(context.Background(), "", "http://10.0.2.2:8791/", false)
		if errors.Is(err, ErrNoURLTab) || !errors.Is(err, listingErr) {
			t.Errorf("listing fails (before the open: %t): %v, want the listing error", failBefore, err)
		}
	}
}

// Status and Down return the end of the deadline as an error, not as an
// unreachable Mac.
func TestDeadlineIsAnError(t *testing.T) {
	fm := readyMac()
	s, _ := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac", Device: udidA})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Status(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Status: %v, want context.Canceled", err)
	}
	if _, err := s.Down(ctx, "", false); !errors.Is(err, context.Canceled) {
		t.Errorf("Down: %v, want context.Canceled", err)
	}
}

// Down stops the forward, shuts the booted simulator down, and runs the
// stop command only with mac; mac without a stop command fails first.
func TestDown(t *testing.T) {
	fm := readyMac()
	s, p := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac", Device: udidA})
	if _, err := s.Down(context.Background(), "", true); err == nil || !strings.Contains(err.Error(), "no stop command") {
		t.Fatalf("--mac without stop: %v", err)
	}
	if _, err := s.Up(context.Background(), "", "", false); err != nil {
		t.Fatal(err)
	}
	s.Cfg.Stop = []string{"vmctl", "stop", "mac"}
	r, err := s.Down(context.Background(), "", true)
	if err != nil || r.ShutDown == nil || !r.MacStopped || fm.booted || fm.count("vmctl stop mac") != 1 {
		t.Fatalf("down: %+v, %v, booted %v", r, err, fm.booted)
	}
	if p.liveSpawned() != 0 {
		t.Fatal("the forward still runs after Down")
	}
}

// The forward signals only the process it started, as it started it: a
// reused pid is never signaled, a failed identification keeps the record,
// and so does a forward that does not end after SIGTERM.
func TestForwardOwnership(t *testing.T) {
	ctx := context.Background()
	p := newFakeProcs()
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err != nil {
		t.Fatal(err)
	}
	pid := recordedPID(t, f)
	p.reuse(pid, false)
	if err := f.Stop(ctx); err != nil || len(p.terms) != 0 {
		t.Fatalf("reused pid: %v, signaled %v", err, p.terms)
	}
	if _, err := f.Ensure(ctx, "/a.socket"); err != nil {
		t.Fatal(err)
	}
	pid = recordedPID(t, f)
	p.identifyErr = errors.New("permission denied")
	if err := f.Stop(ctx); err == nil || len(p.terms) != 0 || recordedPID(t, f) != pid {
		t.Fatalf("failed identification: %v, signaled %v; want an error and the record kept", err, p.terms)
	}
	p.identifyErr = nil
	p.mu.Lock()
	p.procs[pid].ignoresTERM = true
	p.mu.Unlock()
	if err := f.Stop(ctx); err == nil || recordedPID(t, f) != pid {
		t.Fatalf("a forward that outlives SIGTERM: %v; want an error and the record kept", err)
	}
	p.kill(pid)
	if err := f.Stop(ctx); err != nil {
		t.Fatalf("after it ended: %v", err)
	}
	if len(p.badTERM) != 0 {
		t.Fatalf("signaled processes it did not start: %v", p.badTERM)
	}
}

// The forward serves only while its own process listens on the port, and
// alone: when another program holds the port, Serves says no and Ensure
// refuses, so iwdp never reaches that program as if it were the simulator.
func TestForwardServesOnlyWhenItListensAlone(t *testing.T) {
	ctx := context.Background()
	p := newFakeProcs()
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err != nil {
		t.Fatal(err)
	}
	pid := recordedPID(t, f)
	p.mu.Lock()
	p.procs[pid].listens = false // its listener is gone, the process still runs
	p.mu.Unlock()
	p.reuse(4242, true)
	if ok, err := serves(f, ctx, "/a.socket"); ok || err != nil {
		t.Fatalf("Serves = %v, %v with another program on the port", ok, err)
	}
	if _, err := f.Ensure(ctx, "/a.socket"); err == nil || !strings.Contains(err.Error(), "another program") {
		t.Fatalf("Ensure with another program on the port: %v", err)
	}
	if p.liveSpawned() != 0 || len(p.badTERM) != 0 {
		t.Fatalf("%d forwards left, bad signals %v", p.liveSpawned(), p.badTERM)
	}
}

// serves is Forward.State's Serving.
func serves(f *Forward, ctx context.Context, socket string) (bool, error) {
	st, err := f.State(ctx, socket)
	return st.Serving, err
}

// A restart refused after the forward was replaced leaves iwdp listing the
// simulator through the old forward: the next Up must not take that
// listing as the new forward's, and restarts iwdp.
func TestUpDoesNotTrustAListingFromBeforeTheForward(t *testing.T) {
	fm := readyMac()
	iw := &fakeIWDP{}
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if _, err := s.Up(context.Background(), "", "", false); err != nil {
		t.Fatal(err)
	}
	fm.socket = "/t/com.apple.webinspectord_sim.socket"
	iw.leases = []int{0, 1, 0} // a lease appears between the two checks
	if _, err := s.Up(context.Background(), "", "", false); err == nil {
		t.Fatal("the second check let the restart through")
	}
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil || !r.IWDPRestarted {
		t.Fatalf("%+v, %v: want a restart, the listing predates the forward", r, err)
	}
}

// An iwdp that another program started first, while Up started its own,
// is refused; so is a Safari whose state cannot be read, rather than
// launched (and brought forward) blindly.
func TestUpRefusesAForeignWinnerAndAnUnreadableSafari(t *testing.T) {
	fm := readyMac()
	s, _ := newTestSim(t, fm, &fakeIWDP{foreign: true}, Config{SSH: "mac", Device: udidA})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Up(ctx, "", "", false); !errors.Is(err, ErrNotOwnIWDP) {
		t.Fatalf("foreign winner: %v", err)
	}
	fm.safariErr = true
	s.IWDP = &fakeIWDP{}
	if _, err := s.Up(context.Background(), "", "", false); err == nil || fm.count("xcrun simctl launch") != 0 {
		t.Fatalf("unreadable Safari: %v, %d launches", err, fm.count("xcrun simctl launch"))
	}
}

// A new ssh whose environment the kernel shows without the token for a
// moment after its start is still the forward (live run, 2026-11-24: the
// forward ran, unrecorded, and the next sim up called it another program).
func TestForwardWaitsForTheNewProcesssEnvironment(t *testing.T) {
	p := newFakeProcs()
	p.lateEnv = 3
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if started, err := f.Ensure(context.Background(), "/a.socket"); err != nil || !started {
		t.Fatalf("Ensure: %v, %v", started, err)
	}
	if ok, _ := serves(f, context.Background(), "/a.socket"); !ok || len(p.terms) != 0 {
		t.Fatalf("serves %v, signaled %v", ok, p.terms)
	}
}

// A forward that ended but is not reaped yet (a zombie: signal 0 reaches
// it, its environment reads as ESRCH) counts as gone, so a new forward
// replaces it instead of failing (live run, 2026-11-24).
func TestForwardTreatsAZombieAsGone(t *testing.T) {
	ctx := context.Background()
	p := newFakeProcs()
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err != nil {
		t.Fatal(err)
	}
	pid := recordedPID(t, f)
	p.mu.Lock()
	p.zombies = map[int]bool{pid: true}
	p.procs[pid].listens = false
	p.mu.Unlock()
	if started, err := f.Ensure(ctx, "/b.socket"); err != nil || !started || recordedPID(t, f) == pid {
		t.Fatalf("Ensure over a zombie: %v, %v", started, err)
	}
	if len(p.terms) != 0 {
		t.Fatalf("signaled %v: a zombie needs no signal", p.terms)
	}
}

// A spawned ssh that cannot be shown to be the process just started (its
// pid went to another process) is not signaled; a pid that answers EPERM
// is not taken for gone, so the record stays; a cancel during the start
// stops the new forward within seconds, not after Wait.
func TestForwardStartAndProbeFailures(t *testing.T) {
	ctx := context.Background()
	p := newFakeProcs()
	p.stolenPID = true
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err == nil || len(p.terms) != 0 {
		t.Fatalf("stolen pid: %v, signaled %v", err, p.terms)
	}
	p = newFakeProcs()
	f = newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err != nil {
		t.Fatal(err)
	}
	pid := recordedPID(t, f)
	p.identifyErr, p.probeErr = errors.New("permission denied"), syscall.EPERM
	if err := f.Stop(ctx); err == nil || recordedPID(t, f) != pid || len(p.terms) != 0 {
		t.Fatalf("EPERM: %v, signaled %v; want an error and the record kept", err, p.terms)
	}
	p = newFakeProcs()
	p.listenLate, p.ignoreTERM = true, true // the stop of the new forward must not outlast the cancel by much
	f = newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	f.Wait = 15 * time.Second
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := f.Ensure(cctx, "/a.socket"); err == nil {
		t.Fatal("Ensure succeeded without a listener")
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("a canceled start took %s", d)
	}
}

// A forward whose ssh ends before it listens, or whose port another
// program takes first, is an error, and leaves no forward running.
func TestForwardThatDoesNotListen(t *testing.T) {
	ctx := context.Background()
	p := newFakeProcs()
	p.listenLate = true
	f := newForward(t, NewMac(Config{SSH: "mac"}, nil), p)
	if _, err := f.Ensure(ctx, "/a.socket"); err == nil || p.liveSpawned() != 0 {
		t.Fatalf("an ssh that never listens: %v, %d live", err, p.liveSpawned())
	}
	if ok, _ := serves(f, ctx, ""); ok {
		t.Fatal("Serves after a failed start")
	}
}

// Property: over any sequence of ensures, stops, deaths, pid reuses and
// foreign listeners, the forward signals only processes that it started (as
// started), runs at most one of them, serves the asked socket after every
// successful Ensure, and runs none after a successful Stop.
func TestForwardLifecycleProperty(t *testing.T) {
	f := func(ops []byte) bool {
		ctx := context.Background()
		p := newFakeProcs()
		fw := &Forward{Mac: NewMac(Config{SSH: "mac"}, nil), Dir: t.TempDir(), Addr: "127.0.0.1:27753", Procs: p.procsOf(), Wait: 20 * time.Millisecond}
		const foreign = 7
		for _, op := range ops {
			switch op % 7 {
			case 0, 1:
				sock := fmt.Sprint("/s", op%2)
				_, err := fw.Ensure(ctx, sock)
				p.mu.Lock()
				_, foreignUp := p.procs[foreign]
				p.mu.Unlock()
				if err == nil {
					if ok, _ := serves(fw, ctx, sock); !ok {
						t.Logf("Ensure(%s) succeeded without serving it", sock)
						return false
					}
				} else if !foreignUp {
					t.Logf("Ensure failed without a foreign listener: %v", err)
					return false
				}
			case 2:
				if err := fw.Stop(ctx); err != nil || p.liveSpawned() != 0 {
					t.Logf("Stop: %v, %d live", err, p.liveSpawned())
					return false
				}
			case 3:
				if st, _ := fw.read(); st.rec != nil {
					p.kill(st.rec.PID)
				}
			case 4:
				if st, _ := fw.read(); st.rec != nil && st.rec.PID > 0 {
					p.reuse(st.rec.PID, false)
				}
			case 5:
				p.reuse(foreign, true)
			case 6:
				p.kill(foreign)
			}
			if p.liveSpawned() > 1 || len(p.badTERM) > 0 {
				t.Logf("%d live forwards, bad signals %v", p.liveSpawned(), p.badTERM)
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Property: shellQuote makes any string without NUL one word that a POSIX
// shell reads back unchanged.
func TestShellQuoteProperty(t *testing.T) {
	f := func(s string) bool {
		s = strings.ReplaceAll(s, "\x00", "")
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(s)).Output()
		return err == nil && string(out) == s
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

// Safari opens a URL in an empty tab it has: a tab listed before the open
// with another URL that now shows openURL is the open's tab, not an
// earlier turn's (live run, 2026-11-24: after a Safari reset its one
// about:blank tab took the URL and sim up failed with exit 7).
func TestUpTakesAReusedEmptyTab(t *testing.T) {
	const u = "http://10.0.2.2:8791/"
	fm := readyMac()
	iw := &fakeIWDP{urls: []string{"about:blank"}}
	s, _ := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	fm.opened = func() { iw.urls[0] = u }
	r, err := s.Up(context.Background(), "", u, false)
	if err != nil || !r.TabOpened || r.NewPage != "p0" || r.SharedURL {
		t.Fatalf("the reused empty tab: %+v, %v", r, err)
	}
}

// optValues returns the values of the "-o key=value" options of args for
// key, in their order.
func optValues(args []string, key string) []string {
	var v []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" {
			if k, val, ok := strings.Cut(args[i+1], "="); ok && strings.EqualFold(k, key) {
				v = append(v, val)
			}
		}
	}
	return v
}

// The forward keeps an ssh connection of its own, never a shared one: a -L
// asked of a shared master lives in the master and survives the forward's
// process, so sim down could not end it; and it does not fork.
func TestForwardHasItsOwnConnection(t *testing.T) {
	p := newFakeProcs()
	f := newForward(t, sharedMac(t, Config{SSH: "mac", SSHConfig: "/x/config"}, nil), p)
	if _, err := f.Ensure(context.Background(), "/a.socket"); err != nil {
		t.Fatal(err)
	}
	args := p.args[0]
	if cm, cp := optValues(args, "ControlMaster"), optValues(args, "ControlPath"); !slices.Equal(cm, []string{"no"}) || !slices.Equal(cp, []string{"none"}) {
		t.Fatalf("forward ControlMaster %q, ControlPath %q; want [no] and [none]: %q", cm, cp, args)
	}
	if v := optValues(args, "ControlPersist"); len(v) != 0 {
		t.Fatalf("forward carries ControlPersist %q: %q", v, args)
	}
	// A user's ForkAfterAuthentication would detach the listener from the
	// process that sim down ends.
	if v := optValues(args, "ForkAfterAuthentication"); !slices.Equal(v, []string{"no"}) {
		t.Fatalf("forward ForkAfterAuthentication %q, want [no]: %q", v, args)
	}
	if dest := slices.Index(args, "mac"); dest < 0 || dest < slices.Index(args, "ControlPath=none") {
		t.Fatalf("the forward's options do not come before its destination: %q", args)
	}
}

// shortDir is a private temporary directory short enough for control
// sockets, also where t.TempDir is not (macOS's $TMPDIR).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "iwdp-t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// sharedMac is the Mac of cfg whose read-only steps share a connection in
// a short private directory; its route (ssh -G) is plain and already read,
// so run answers only the other calls.
func sharedMac(t *testing.T, cfg Config, run Runner) Mac {
	t.Helper()
	m := NewMac(cfg, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "-G") {
			return []byte("host mac\nhostname 10.0.2.2\nport 22\n"), nil
		}
		return run(ctx, name, args...)
	}).Shared(shortDir(t))
	if c := m.control(context.Background()); c == "" {
		t.Fatalf("Shared: no control socket for a plain route")
	}
	m.run = run
	return m
}

// recordingRunner records every argv and answers it with answer.
type recordingRunner struct {
	mu     sync.Mutex
	calls  [][]string
	answer func(ctx context.Context, args []string) ([]byte, error)
}

func (r *recordingRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return r.answer(ctx, args)
}

// sshRole tells what an ssh call is: a control command ("-O stop"), a step
// over the shared connection ("shared"), or one on a connection of its own
// ("own"). Anything else is "other".
func sshRole(args []string) string {
	if i := slices.Index(args, "-O"); i >= 0 {
		return "-O " + args[i+1]
	}
	cm, cp := optValues(args, "ControlMaster"), optValues(args, "ControlPath")
	switch {
	case slices.Equal(cm, []string{"no"}) && slices.Equal(cp, []string{"none"}):
		return "own " + args[len(args)-1]
	case slices.Equal(cm, []string{"auto"}) && len(cp) == 1 && cp[0] != "none":
		return "shared " + args[len(args)-1]
	}
	return "other " + strings.Join(args, " ")
}

// Without a usable control directory, no sharing means no sharing: every
// call has a connection of its own, also when the user's ssh configuration
// asks for ControlMaster, and Reachable asks with fresh logins only.
func TestNoSharingMeansNoSharing(t *testing.T) {
	long := "/tmp/" + strings.Repeat("d", 80)
	for _, dir := range []string{"", "/tmp/iwdp space", "/tmp/iwdp%h", "/tmp/iwdp$HOME", "relative/dir", long} {
		rr := &recordingRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
			if slices.Contains(args, "-G") {
				return []byte("host mac\nhostname 10.0.2.2\n"), nil
			}
			return []byte(devicesJSON), nil
		}}
		m := NewMac(Config{SSH: "mac"}, rr.run)
		if dir != "" {
			m = m.Shared(dir)
		}
		rr.calls = nil
		if _, err := m.Devices(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ok, err := m.Reachable(context.Background()); !ok || err != nil {
			t.Fatalf("Reachable = %v, %v", ok, err)
		}
		for _, c := range rr.calls {
			if r := sshRole(c[1:]); !strings.HasPrefix(r, "own ") || len(optValues(c, "ControlMaster")) != 1 {
				t.Fatalf("dir %q: call %q is %s, want a connection of its own", dir, c, r)
			}
		}
	}
}

// routeRunner answers ssh -G with g (when g is "", a failure whose output
// is a plain route: a failed ssh -G is no route, whatever it printed), and
// every other call, which it records, with devicesJSON.
func routeRunner(g string) *recordingRunner {
	return &recordingRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "-G") {
			if g == "" {
				return []byte("host mac\nhostname 10.0.2.2\n"), errors.New("ssh: exit status 255: bad configuration")
			}
			return []byte(g), nil
		}
		return []byte(devicesJSON), nil
	}}
}

// sharesFor runs a read-only step (the device list) of a Mac that may
// share, whose ssh -G answers g, and reports whether it shared; the step
// itself must succeed: an unsuitable route is no error, only no sharing.
func sharesFor(t *testing.T, g string) bool {
	t.Helper()
	rr := routeRunner(g)
	m := NewMac(Config{SSH: "mac"}, rr.run).Shared(shortDir(t))
	if _, err := m.Devices(context.Background()); err != nil {
		t.Errorf("ssh -G %q: the device list failed: %v", g, err)
	}
	shared := false
	for _, c := range rr.calls {
		switch r := sshRole(c[1:]); {
		case slices.Contains(c, "-G") && !strings.HasPrefix(r, "own "):
			t.Errorf("ssh -G %q: the route was read as %s, want a connection of its own", g, r)
		case strings.HasPrefix(r, "shared "):
			shared = true
		case !strings.HasPrefix(r, "own "):
			t.Errorf("ssh -G %q: call %q is %s", g, c, r)
		}
	}
	return shared
}

// A route through a proxy (ProxyCommand or ProxyJump in ssh -G), whose
// target ssh -G does not show, shares nothing, and neither does a route
// that ssh -G cannot tell: a shared connection could reach another Mac.
// The step still runs, on a connection of its own, without an error.
func TestRouteWithAProxySharesNothing(t *testing.T) {
	for _, g := range []string{
		"host mac\nhostname 10.0.2.2\nproxycommand nc %h %p\n",
		"host mac\nhostname 10.0.2.2\nProxyJump hop\n",
		"", // ssh -G fails
	} {
		if sharesFor(t, g) {
			t.Errorf("ssh -G %q: the step shared; want no sharing", g)
		}
	}
	if !sharesFor(t, "host mac\nhostname 10.0.2.2\nproxycommand none\n") {
		t.Fatalf("a plain route did not share")
	}
}

// slowRoute is a Mac whose ssh -G answers a plain route only after 3 s,
// also when its context ends (a Match exec child that keeps ssh's pipes),
// and whose other calls answer devicesJSON at once; lookups counts the
// ssh -G calls.
func slowRoute(t *testing.T) (m Mac, lookups *atomic.Int32) {
	lookups = new(atomic.Int32)
	m = NewMac(Config{SSH: "mac"}, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if slices.Contains(args, "-G") {
			lookups.Add(1)
			time.Sleep(3 * time.Second)
			return []byte("host mac\nhostname 10.0.2.2\n"), nil
		}
		return []byte(devicesJSON), nil
	}).Shared(shortDir(t))
	return m, lookups
}

// Reading the route (ssh -G, which can run Match exec commands of the ssh
// configuration) ends at the caller's deadline and within its own bound of
// 1 s, also while ssh does not return; a route not read in time shares
// nothing for the rest of the command, is read once however many steps ask
// at the same time, and is no error of the step.
func TestRouteEndsAtTheCallersDeadline(t *testing.T) {
	m, lookups := slowRoute(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Devices(ctx)
	if took := time.Since(start); took > 100*time.Millisecond+readGrace+100*time.Millisecond || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a step with a 100 ms deadline took %s: %v", took, err)
	}
	// The command's next steps: no sharing, no new lookup, no error.
	if _, err := m.Devices(context.Background()); err != nil || m.knownControl() != "" || lookups.Load() != 1 {
		t.Fatalf("after a lookup cut off: %v, control %q, %d lookups; want no error, no sharing, 1 lookup", err, m.knownControl(), lookups.Load())
	}

	m, lookups = slowRoute(t)
	start = time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, errs[i] = m.Devices(context.Background()) })
	}
	wg.Wait()
	if took := time.Since(start); took > time.Second+300*time.Millisecond {
		t.Errorf("4 steps without a deadline waited %s for the route; want its bound of 1 s", took)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("4 steps at once read the route %d times; want once", n)
	}
	for _, err := range errs {
		if err != nil {
			t.Errorf("a step after a lookup past its bound: %v; want no error", err)
		}
	}
	if c := m.knownControl(); c != "" {
		t.Errorf("a lookup past its bound shares through %q; want no sharing", c)
	}
}

// sim down --mac's ssh -O stop (CloseShared) uses only a route that a step
// found already: it runs nothing, ssh -G included, to find one.
func TestCloseSharedRunsNothingToFindARoute(t *testing.T) {
	rr := routeRunner("host mac\nhostname 10.0.2.2\n")
	m := NewMac(Config{SSH: "mac"}, rr.run).Shared(shortDir(t))
	m.CloseShared(context.Background())
	if len(rr.calls) != 0 {
		t.Fatalf("CloseShared before any step ran %q; want nothing", rr.calls)
	}
	if _, err := m.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	rr.calls = nil
	m.CloseShared(context.Background())
	if len(rr.calls) != 1 || sshRole(rr.calls[0][1:]) != "-O stop" {
		t.Fatalf("CloseShared after a shared step ran %q; want one ssh -O stop", rr.calls)
	}
}

// Only read-only, repeatable steps share the connection; every step that
// acts on the Mac or the simulator has a connection of its own, so that a
// full, stopped or vanishing shared connection, after which ssh logs in
// anew by itself, never runs an action twice or fails it.
func TestOnlyReadOnlyStepsShare(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), "x"...)
	rr := &recordingRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
		s := args[len(args)-1]
		switch {
		case slices.Contains(args, "-O"):
			return nil, nil // a master answers
		case strings.HasPrefix(s, "xcrun simctl list"):
			return []byte(devicesJSON), nil
		case strings.HasPrefix(s, "d=$(mktemp -d)"):
			return png, nil
		case strings.HasPrefix(s, "p=$(pgrep -f 'launchd_sim"):
			return []byte("/s/com.apple.webinspectord_sim.socket\n"), nil
		case strings.HasPrefix(s, "k=$("):
			return []byte("hw=U.S.\ncom.apple.keylayout.US\n"), nil
		}
		return []byte("yes\n"), nil
	}}
	cfg := Config{SSH: "mac", AXe: "/opt/axe"}
	m := sharedMac(t, cfg, rr.run)
	ctx := context.Background()
	steps := []struct {
		method string // the Mac method that the step calls
		name   string
		shared bool
		run    func() error
	}{
		{"Reachable", "reachability probe", true, func() error { _, err := m.Reachable(ctx); return err }},
		{"LoggedIn", "desktop login", true, func() error { _, err := m.LoggedIn(ctx); return err }},
		{"BootedAll", "booted list", true, func() error { _, err := m.BootedAll(ctx); return err }},
		{"Devices", "device list", true, func() error { _, err := m.Devices(ctx); return err }},
		{"WindowOpen", "window open", true, func() error { _, err := m.WindowOpen(ctx); return err }},
		{"SafariRunning", "Safari running", true, func() error { _, err := m.SafariRunning(ctx, udidA); return err }},
		{"USKeyboard", "keyboard layout", true, func() error { _, _, err := m.USKeyboard(ctx, udidA); return err }},
		{"HardwareKeyboard", "keyboard setting", true, func() error { _, err := m.HardwareKeyboard(ctx); return err }},
		{"InspectorSocket", "inspector socket", true, func() error { _, err := m.InspectorSocket(ctx, udidA); return err }},
		// The screenshot leaves a file on the Mac when it is cut off, and
		// describe-ui can restart simulator services and write a
		// preference: neither may run twice.
		{"Screenshot", "screenshot", false, func() error { _, err := m.Screenshot(ctx, udidA); return err }},
		{"AXe", "axe describe-ui", false, func() error { _, err := m.AXe(ctx, udidA, "describe-ui", nil); return err }},
		{"AXe", "axe tap", false, func() error { _, err := m.AXe(ctx, udidA, "tap", []string{"-x", "1", "-y", "2"}); return err }},
		{"AXe", "axe type", false, func() error { _, err := m.AXe(ctx, udidA, "type", nil, "hi"); return err }},
		{"AXe", "axe swipe", false, func() error {
			_, err := m.AXe(ctx, udidA, "swipe", []string{"--start-x", "1", "--start-y", "2", "--end-x", "3", "--end-y", "4"})
			return err
		}},
		{"AXe", "axe touch", false, func() error { _, err := m.AXe(ctx, udidA, "touch", nil); return err }},
		{"AXe", "axe key", false, func() error { _, err := m.AXe(ctx, udidA, "key", nil, "40"); return err }},
		{"OpenURL", "open URL", false, func() error { return m.OpenURL(ctx, udidA, "http://10.0.2.2/") }},
		{"Boot", "boot", false, func() error { return m.Boot(ctx, udidA) }},
		{"Shutdown", "shutdown", false, func() error { return m.Shutdown(ctx, udidA) }},
		{"OpenWindow", "open window", false, func() error { return m.OpenWindow(ctx, udidA) }},
		{"LaunchSafari", "launch Safari", false, func() error { return m.LaunchSafari(ctx, udidA) }},
		{"SetHardwareKeyboard", "keyboard change", false, func() error { return m.SetHardwareKeyboard(ctx, udidA, true) }},
	}
	// Guard: every exported method of the Mac type, with a value or a
	// pointer receiver (the method set of *Mac), is in the table, or here
	// with the reason it is not a step. It cannot see unexported methods
	// of Mac, nor helpers outside the Mac type (functions, or methods of
	// other types such as Forward) that run ssh: those are checked only by
	// their own tests and by reading the call sites.
	notSteps := map[string]string{
		"Shared":      "runs nothing; the first read-only step reads ssh -G on a connection of its own (TestRouteWithAProxySharesNothing)",
		"CloseShared": "sends ssh -O stop to the master (TestDownMacStopsSharingFirst)",
	}
	listed := map[string]bool{}
	for _, st := range steps {
		listed[st.method] = true
	}
	mt := reflect.TypeOf(&Mac{})
	for i := 0; i < mt.NumMethod(); i++ {
		if name := mt.Method(i).Name; !listed[name] && notSteps[name] == "" {
			t.Errorf("Mac.%s is not classified as a shared or an own-connection step", name)
		}
	}
	for _, st := range steps {
		rr.calls = nil
		if err := st.run(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		n := 0
		for _, c := range rr.calls {
			r := sshRole(c[1:])
			if strings.HasPrefix(r, "-O ") {
				continue
			}
			n++
			if want := map[bool]string{true: "shared ", false: "own "}[st.shared]; !strings.HasPrefix(r, want) {
				t.Errorf("%s: %q is %s, want %s", st.name, c, r, want)
			}
			if st.shared {
				checkSharedOpts(t, st.name, c[1:], filepath.Dir(m.knownControl()))
			}
		}
		if n == 0 {
			t.Errorf("%s ran no ssh", st.name)
		}
	}
	// The forward and the Mac's start and stop commands: the forward is
	// TestForwardHasItsOwnConnection; start and stop run on this host
	// (Sim.Run), not over ssh.
}

// checkSharedOpts checks the options of a step over the shared
// connection: its socket in the private directory, kept for minutes after
// the last step, with keepalives that end it within about 10 s of a dead
// Mac, all before the destination.
func checkSharedOpts(t *testing.T, name string, args []string, dir string) {
	t.Helper()
	if cp := optValues(args, "ControlPath"); len(cp) != 1 || filepath.Dir(cp[0]) != dir {
		t.Errorf("%s: ControlPath %q, want one path in %s", name, cp, dir)
	}
	persist := optValues(args, "ControlPersist")
	if d, err := time.ParseDuration(strings.Join(persist, "")); len(persist) != 1 || err != nil || d < time.Minute || d > 10*time.Minute {
		t.Errorf("%s: ControlPersist %q, want a few minutes", name, persist)
	}
	iv, cm := optValues(args, "ServerAliveInterval"), optValues(args, "ServerAliveCountMax")
	if len(iv) != 1 || len(cm) != 1 {
		t.Errorf("%s: keepalives %q, %q: want one of each", name, iv, cm)
		return
	}
	i, err1 := strconv.Atoi(iv[0])
	c, err2 := strconv.Atoi(cm[0])
	// ssh ends the connection at the check after CountMax unanswered ones.
	if err1 != nil || err2 != nil || i <= 0 || i*(c+1) > 10 {
		t.Errorf("%s: ServerAliveInterval %q, CountMax %q: want a dead Mac noticed within about 10 s", name, iv, cm)
	}
	if dest := slices.Index(args, "mac"); dest < slices.Index(args, "ControlMaster=auto") {
		t.Errorf("%s: sharing options after the destination: %q", name, args)
	}
}

// The control directory is private and usable: owned by this user, not a
// symlink, mode exactly 0700 (also under a umask that would make it
// less), short enough for the socket; anything else is refused, and a
// base too long for a socket uses the short fallback.
func TestControlDirIsPrivateAndShort(t *testing.T) {
	old := controlFallback
	defer func() { controlFallback = old }()
	controlFallback = func() string { t.Fatal("the fallback was used for a short base"); return "" }
	base := shortDir(t)
	mask := syscall.Umask(0o277)
	dir, err := ControlDir(base)
	syscall.Umask(mask)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 || filepath.Dir(dir) != base {
		t.Fatalf("ControlDir %s: %v, %v; want a directory in %s with mode 0700", dir, fi.Mode(), err, base)
	}
	for _, mode := range []os.FileMode{0o500, 0o000, 0o711, 0o700 | os.ModeSticky} {
		b := shortDir(t)
		if err := os.Mkdir(filepath.Join(b, "ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(b, "ssh"), mode); err != nil {
			t.Fatal(err)
		}
		if dir, err := ControlDir(b); err == nil {
			t.Errorf("ControlDir accepted %s with mode %v", dir, mode)
		}
		_ = os.Chmod(filepath.Join(b, "ssh"), 0o700)
	}
	b := shortDir(t)
	if err := os.Symlink(shortDir(t), filepath.Join(b, "ssh")); err != nil {
		t.Fatal(err)
	}
	if dir, err := ControlDir(b); err == nil {
		t.Errorf("ControlDir accepted the symlink %s", dir)
	}

	long := filepath.Join(t.TempDir(), strings.Repeat("d", 60))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(shortDir(t), "s")
	controlFallback = func() string { return short }
	if dir, err := ControlDir(long); err != nil || dir != short {
		t.Fatalf("ControlDir of a long base = %q, %v; want the short fallback %s", dir, err, short)
	}
}

// Reachable stays truthful with a shared connection: a master that listens
// but whose Mac is gone (the VM killed or frozen) is no answer. The probe
// gives up on it within its own bound, stops that master (other commands'
// sessions end on their own) so that later commands open a new one, and
// asks with logins that open a new master, which decide.

// safeDirRE is the oracle of the directories that a control socket may be
// in: absolute, of characters that neither ssh's option parser nor its
// %, ~ and ${} expansions read.
var safeDirRE = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*$`)

// Property: for any directory and plain route, the control socket's path
// is either none (no sharing) or a path inside that directory, of safe
// characters only, under the unix socket limit with the 17 bytes ssh adds;
// a short safe directory always shares; and two destinations (another
// alias, configuration file or resolved route) never share a socket.
func TestControlPathProperty(t *testing.T) {
	f := func(dirBytes []byte, short bool, r1, r2 string, alias2 bool) bool {
		dir := "/" + string(dirBytes)
		if short {
			dir = "/tmp/" + strings.Map(func(r rune) rune {
				if r < 128 && safeDirRE.MatchString("/"+string(r)) && r != '/' {
					return r
				}
				return 'x'
			}, string(dirBytes))
			if len(dir) > 20 {
				dir = dir[:20]
			}
		}
		cfg := Config{SSH: "mac", SSHConfig: "/x/config"}
		p := controlAt(cfg, dir, r1)
		if p == "" {
			if safeDirRE.MatchString(dir) && filepath.Clean(dir) == dir && len(dir) <= 20 {
				t.Logf("a short safe directory %q does not share", dir)
				return false
			}
			return true
		}
		if !safeDirRE.MatchString(p) || filepath.Dir(p) != filepath.Clean(dir) || len(p)+17 >= 104 {
			t.Logf("dir %q: control path %q", dir, p)
			return false
		}
		cfg2 := cfg
		if alias2 {
			cfg2.SSH = "mac2"
		} else {
			cfg2.SSHConfig = "/y/config"
		}
		other := []string{controlAt(cfg2, dir, r1)}
		if r1 != r2 {
			other = append(other, controlAt(cfg, dir, r2))
		}
		if slices.Contains(other, p) || controlAt(cfg, dir, r1) != p {
			t.Logf("dir %q: destinations share %q (%q)", dir, p, other)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Reachable stays truthful with a shared connection: a master that answers
// but whose Mac is gone (the VM killed or frozen) is no answer. The probe
// gives up on it within its own bound, stops that master (other steps'
// sessions end on their own; later read-only steps open a new one) and
// asks with fresh logins on connections of their own, which decide.
func TestReachableWithAStaleMaster(t *testing.T) {
	var roles []string
	var mu sync.Mutex
	macUp := false
	rr := &recordingRunner{answer: func(ctx context.Context, args []string) ([]byte, error) {
		role := sshRole(args)
		mu.Lock()
		roles = append(roles, role)
		mu.Unlock()
		switch role {
		case "-O check", "-O stop":
			return nil, nil
		case "shared true":
			// Its connection is dead: no answer until the master's
			// keepalives end it (9 s; shortened here).
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				return nil, errors.New("ssh: exit status 255: Timeout, server not responding")
			}
		case "own true":
			if macUp {
				return nil, nil
			}
			return nil, errors.New("ssh: connect to host: Connection refused")
		}
		return nil, fmt.Errorf("unexpected call %q", role)
	}}
	m := sharedMac(t, Config{SSH: "mac"}, rr.run)
	m.probeWait = 100 * time.Millisecond
	start := time.Now()
	ok, err := m.Reachable(context.Background())
	if ok || err != nil {
		t.Fatalf("Reachable = %v, %v with a stale master and a gone Mac; want false", ok, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Reachable took %s with a stale master", took)
	}
	want := []string{"-O check", "shared true", "-O stop", "own true", "own true"}
	if !slices.Equal(roles, want) {
		t.Fatalf("calls %q, want %q", roles, want)
	}
	roles, macUp = nil, true
	if ok, err := m.Reachable(context.Background()); !ok || err != nil {
		t.Fatalf("Reachable = %v, %v with a stale master and a Mac that answers a fresh login", ok, err)
	}
	if want := []string{"-O check", "shared true", "-O stop", "own true"}; !slices.Equal(roles, want) {
		t.Fatalf("calls %q, want %q", roles, want)
	}
}

// With a master that answers, the probe is one step over it; without one,
// it is a fresh login on a connection of its own, as before.
func TestReachableUsesALiveMaster(t *testing.T) {
	var roles []string
	master := true
	rr := &recordingRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
		role := sshRole(args)
		roles = append(roles, role)
		if role == "-O check" && !master {
			return nil, errors.New("Control socket connect: No such file or directory")
		}
		return nil, nil
	}}
	m := sharedMac(t, Config{SSH: "mac"}, rr.run)
	if ok, err := m.Reachable(context.Background()); !ok || err != nil || !slices.Equal(roles, []string{"-O check", "shared true"}) {
		t.Fatalf("live master: %v, %v, calls %q", ok, err, roles)
	}
	roles, master = nil, false
	if ok, err := m.Reachable(context.Background()); !ok || err != nil || !slices.Equal(roles, []string{"-O check", "own true"}) {
		t.Fatalf("no master: %v, %v, calls %q", ok, err, roles)
	}
}

// sim down --mac stops the shared connection from taking new steps before
// it runs the stop command, and ends no step that runs over it (no ssh -O
// exit): those end with the Mac, or finish when the stop fails.
func TestDownMacStopsSharingFirst(t *testing.T) {
	var calls []string
	rr := &recordingRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
		role := sshRole(args)
		calls = append(calls, role)
		if strings.HasSuffix(role, "simctl list devices available -j") {
			return []byte(devicesJSON), nil
		}
		return nil, nil
	}}
	cfg := Config{SSH: "mac", Device: udidB, Stop: []string{"vmctl", "stop", "mac"}}
	mac := sharedMac(t, cfg, rr.run)
	s := &Sim{
		Cfg: cfg, Mac: mac, Forward: newForward(t, mac, newFakeProcs()), IWDP: &fakeIWDP{}, Poll: time.Millisecond,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil, nil
		},
	}
	if _, err := s.Down(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	stop, vm := slices.Index(calls, "-O stop"), slices.Index(calls, "vmctl stop mac")
	if stop < 0 || vm < 0 || stop > vm || slices.Contains(calls, "-O exit") {
		t.Fatalf("calls %q: want -O stop before the stop command, and no -O exit", calls)
	}
}

// Property: over any sequence of a Mac that goes down and up, masters that
// end, probes, read-only and acting steps and sim down --mac, a model of
// OpenSSH (ControlMaster=auto attaches to a master that answers and
// otherwise logs in anew and becomes the master; a master whose Mac is
// gone hangs its sessions; -O stop takes the master's name) shows that
// Reachable tells whether the Mac answers and leaves no stale master to
// the next step, that an acting step never runs over a master and runs
// once when the Mac answers, and that a read-only step ends at its deadline.
func TestSharedLifecycleProperty(t *testing.T) {
	f := func(ops []byte) bool {
		var (
			mu     sync.Mutex
			master bool // a master answers on the socket
			stale  bool // its connection is dead
			up     = true
			acted  int // how often the last acting step ran on the Mac
		)
		run := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			role := sshRole(args)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case role == "-O check":
				if master {
					return nil, nil
				}
				return nil, errors.New("Control socket connect: No such file or directory")
			case role == "-O stop":
				if !master {
					return nil, errors.New("no master")
				}
				master, stale = false, false
				return nil, nil
			case strings.HasPrefix(role, "shared "):
				if master && stale {
					mu.Unlock()
					<-ctx.Done()
					mu.Lock()
					return nil, ctx.Err()
				}
				if !up {
					return nil, errors.New("ssh: exit status 255: Connection refused")
				}
				master = true // attached, or logged in anew as the master
				return nil, nil
			case strings.HasPrefix(role, "own "):
				if !up {
					return nil, errors.New("ssh: exit status 255: Connection refused")
				}
				if role == "own act" {
					acted++
				}
				return nil, nil
			}
			t.Errorf("unexpected call %q", args)
			return nil, errors.New("unexpected")
		}
		mac := sharedMac(t, Config{SSH: "mac"}, run)
		mac.probeWait = 20 * time.Millisecond
		for _, op := range ops {
			mu.Lock()
			switch op % 7 {
			case 0:
				up = false
				stale = master
			case 1:
				up = true
			case 2:
				master, stale = false, false // the master ended (ControlPersist, a crash)
			}
			mu.Unlock()
			switch op % 7 {
			case 3:
				ok, err := mac.Reachable(context.Background())
				mu.Lock()
				bad := err != nil || ok != up || (master && stale)
				mu.Unlock()
				if bad {
					t.Logf("Reachable = %v, %v with the Mac up %v, stale master %v", ok, err, up, stale)
					return false
				}
			case 4:
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				start := time.Now()
				_, _ = mac.read(ctx, "look")
				cancel()
				if time.Since(start) > time.Second {
					t.Logf("a read-only step outlived its deadline")
					return false
				}
			case 5:
				mu.Lock()
				acted = 0
				wantUp := up
				mu.Unlock()
				_, err := mac.shell(context.Background(), "act")
				mu.Lock()
				n := acted
				mu.Unlock()
				if n > 1 || (wantUp && (n != 1 || err != nil)) {
					t.Logf("an acting step ran %d times (Mac up %v): %v", n, wantUp, err)
					return false
				}
			case 6:
				mac.CloseShared(context.Background())
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

// A master keeps the address it connected to while a fresh login resolves
// the name again, so the shared queries and the own-connection actions
// could reach different Macs after a DNS change: only a destination that
// ssh -G resolves to an IP literal shares.
func TestSharingNeedsALiteralAddress(t *testing.T) {
	for _, tc := range []struct {
		host  string
		share bool
	}{{"127.0.0.1", true}, {"::1", true}, {"10.0.2.2", true}, {"localhost", false}, {"mac.example.test", false}, {"", false}} {
		g := "host mac\nuser devkit\nport 22\n"
		if tc.host != "" {
			g += "hostname " + tc.host + "\n"
		}
		if got := sharesFor(t, g); got != tc.share {
			t.Errorf("hostname %q: sharing %v, want %v", tc.host, got, tc.share)
		}
	}
}

// A shared step whose deadline ends returns within readGrace of it, also
// when ssh keeps running past it (a master holds the step's output pipes,
// which ExecRunner waits up to 2 s for).
func TestCancelledSharedStepReturnsWithinGrace(t *testing.T) {
	if readGrace > 250*time.Millisecond {
		t.Fatalf("readGrace %s, want 250 ms at most", readGrace)
	}
	m := sharedMac(t, Config{SSH: "mac"}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		time.Sleep(2 * time.Second) // the wait on the master's pipes
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Devices(ctx)
	if took := time.Since(start); took > 100*time.Millisecond+readGrace+100*time.Millisecond || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a shared step with a 100 ms deadline returned after %s: %v", took, err)
	}
}

// F6: a shared query whose deadline has ended returns the deadline's
// error, also when ssh ends with success in the same moment (a
// ProxyCommand's stderr that closes late, or empty output that would read
// as a default).
func TestSharedQueryAfterItsDeadlineIsAnError(t *testing.T) {
	m := sharedMac(t, Config{SSH: "mac"}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return []byte("1\n"), nil // success, just after the deadline
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if hw, err := m.HardwareKeyboard(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HardwareKeyboard after its deadline = %v, %v; want the deadline's error", hw, err)
	}
}

// Property: the route parser shares exactly when ssh -G's output names an
// IP literal as hostname and no ProxyCommand or ProxyJump (other than
// "none"), whatever the order of the lines, other lines, the case of keys
// and the spaces between key and value; output without a hostname or a
// proxy line is covered too.
func TestRouteParserProperty(t *testing.T) {
	f := func(v4 [4]byte, v6 [16]byte, kind, proxy uint8, seed int64, extra uint8, upper, wide bool) bool {
		var host string
		literal := true
		switch kind % 4 {
		case 0:
			host = netip.AddrFrom4(v4).String()
		case 1:
			host = netip.AddrFrom16(v6).String()
		case 2:
			host, literal = fmt.Sprintf("mac%d.example.test", v4[0]), false
		default:
			literal = false // no hostname line
		}
		line := func(k, v string) string {
			if upper {
				k = strings.ToUpper(k[:1]) + k[1:]
			}
			sep := " "
			if wide {
				sep = "   "
			}
			return k + sep + v
		}
		lines := []string{line("host", "mac"), line("user", "devkit"), line("port", "22")}
		if host != "" {
			lines = append(lines, line("hostname", host))
		}
		hasProxy := false
		switch proxy % 5 {
		case 1:
			lines, hasProxy = append(lines, line("proxycommand", "nc %h %p")), true
		case 2:
			lines, hasProxy = append(lines, line("proxyjump", "hop")), true
		case 3:
			lines = append(lines, line("proxycommand", "none"))
		case 4:
			lines = append(lines, line("proxyjump", "NONE"))
		}
		for i := 0; i < int(extra%5); i++ {
			lines = append(lines, line(fmt.Sprintf("option%d", i), "yes"))
		}
		r := rand.New(rand.NewSource(seed))
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		out := strings.Join(lines, "\n") + "\n"
		if got, want := shareableRoute(out), literal && !hasProxy; got != want {
			t.Logf("ssh -G output %q: share %v, want %v", out, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
}
