package sim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// fakeMac answers the ssh calls of Mac and records every call.
type fakeMac struct {
	mu        sync.Mutex
	calls     []string
	reachable func() bool
	loggedIn  bool
	booted    bool
	window    bool
	socket    string
}

func (f *fakeMac) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	if name != "ssh" {
		f.calls = append(f.calls, line)
		return nil, nil
	}
	script := args[len(args)-1]
	f.calls = append(f.calls, script)
	if !f.reachable() {
		return nil, errors.New("ssh: connect: connection refused")
	}
	switch {
	case script == "true":
		return nil, nil
	case strings.HasPrefix(script, "who"):
		return []byte(map[bool]string{true: "yes\n", false: "no\n"}[f.loggedIn]), nil
	case strings.HasPrefix(script, "xcrun simctl list"):
		state := map[bool]string{true: "Booted", false: "Shutdown"}[f.booted]
		return []byte(`{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-5":[{"udid":"` + udidA + `","name":"iPhone 17 Pro","state":"` + state + `"}]}}`), nil
	case strings.HasPrefix(script, "xcrun simctl bootstatus"):
		f.booted = true
		return nil, nil
	case strings.HasPrefix(script, "xcrun simctl shutdown"):
		f.booted = false
		return nil, nil
	case strings.HasPrefix(script, "pgrep -xq Simulator"):
		return []byte(map[bool]string{true: "yes\n", false: "no\n"}[f.window]), nil
	case strings.HasPrefix(script, "open -a Simulator"):
		f.window = true
		return nil, nil
	case strings.HasPrefix(script, "xcrun simctl openurl"):
		return nil, nil
	case strings.HasPrefix(script, "xcrun simctl launch"):
		return nil, nil
	case strings.HasPrefix(script, "p=$(pgrep -f 'launchd_sim"):
		if !f.booted {
			return nil, errors.New("exit status 1")
		}
		return []byte(f.socket + "\n"), nil
	}
	return nil, fmt.Errorf("fakeMac: unexpected script %q", script)
}

func (f *fakeMac) did(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// fakeForward and fakeIWDP stand in for the forward and iwdp.
type fakeIWDP struct {
	listed   bool
	restarts int
	onStart  bool // a restart makes iwdp list the simulator
}

func (f *fakeIWDP) SimulatorListed(context.Context) (bool, error) { return f.listed, nil }
func (f *fakeIWDP) SimulatorPages(context.Context) (int, error) {
	if f.listed {
		return 1, nil
	}
	return 0, nil
}

func (f *fakeIWDP) Restart(context.Context) error {
	f.restarts++
	f.listed = f.onStart
	return nil
}

// forwardFor returns a Forward whose ssh is a real sleep process (so that
// pids and signals are real) next to a TCP listener on a private address.
func forwardFor(t *testing.T, mac Mac) (*Forward, *[]string) {
	t.Helper()
	var spawned []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	var listeners []net.Listener
	args := map[int][]string{}
	f := &Forward{
		Mac: mac, Dir: t.TempDir(), Addr: addr, Wait: 5 * time.Second,
		Run: func(_ context.Context, name string, a ...string) ([]byte, error) {
			pid := 0
			_, _ = fmt.Sscan(a[len(a)-1], &pid)
			if err := exec.Command("kill", "-0", a[len(a)-1]).Run(); err != nil {
				return nil, errors.New("exit status 1")
			}
			return []byte(strings.Join(args[pid], " ") + "\n"), nil
		},
	}
	f.Spawn = func(name string, a ...string) (int, error) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return 0, err
		}
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			_ = l.Close()
			return 0, err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		listeners = append(listeners, l)
		go func() { // the "ssh" stops listening when it ends, as ssh -L does
			_ = cmd.Wait()
			_ = l.Close()
		}()
		args[cmd.Process.Pid] = append([]string{name}, a...)
		spawned = append(spawned, strings.Join(a, " "))
		return cmd.Process.Pid, nil
	}
	t.Cleanup(func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	})
	return f, &spawned
}

func newTestSim(t *testing.T, fm *fakeMac, iw *fakeIWDP, cfg Config) (*Sim, *[]string) {
	t.Helper()
	mac := NewMac(cfg, fm.run)
	fwd, spawned := forwardFor(t, mac)
	return &Sim{Cfg: cfg, Mac: mac, Run: fm.run, Forward: fwd, IWDP: iw, Poll: time.Millisecond, PageWait: 50 * time.Millisecond}, spawned
}

// From a stopped Mac, Up runs the start command, waits for ssh and the
// login, boots, opens the window, starts Safari, forwards the inspector
// socket and restarts iwdp, which lists nothing before.
func TestUpFromStoppedMac(t *testing.T) {
	started := false
	fm := &fakeMac{socket: "/private/var/tmp/x/com.apple.webinspectord_sim.socket", loggedIn: true}
	fm.reachable = func() bool { return started }
	cfg := Config{SSH: "mac", Start: []string{"vmctl", "start", "mac"}, Device: "iPhone 17 Pro"}
	iw := &fakeIWDP{onStart: true}
	s, spawned := newTestSim(t, fm, iw, cfg)
	s.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		started = true
		return fm.run(ctx, name, args...)
	}
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.MacStarted || !r.Booted || !r.WindowOpened || !r.ForwardStarted || !r.IWDPRestarted || r.Device.UDID != udidA {
		t.Fatalf("report %+v: want every step done", r)
	}
	if !fm.did("vmctl start mac") || len(*spawned) != 1 || !strings.Contains((*spawned)[0], ":"+fm.socket) {
		t.Fatalf("calls %q, spawned %q", fm.calls, *spawned)
	}
}

// When everything runs, Up changes nothing: no boot, no window, the same
// forward, and no iwdp restart.
func TestUpIdempotent(t *testing.T) {
	fm := &fakeMac{socket: "/s/com.apple.webinspectord_sim.socket", loggedIn: true, booted: true, window: true}
	fm.reachable = func() bool { return true }
	iw := &fakeIWDP{onStart: true}
	s, spawned := newTestSim(t, fm, iw, Config{SSH: "mac", Device: udidA})
	if _, err := s.Up(context.Background(), "", "", false); err != nil {
		t.Fatal(err)
	}
	r, err := s.Up(context.Background(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.MacStarted || r.Booted || r.WindowOpened || r.ForwardStarted || r.IWDPRestarted {
		t.Fatalf("second Up did steps: %+v", r)
	}
	if len(*spawned) != 1 || iw.restarts != 1 {
		t.Fatalf("spawned %d forwards and %d restarts over two Ups, want 1 and 1", len(*spawned), iw.restarts)
	}
	// A new boot gives a new socket: the forward is replaced and iwdp,
	// which dropped the simulator, restarts.
	fm.socket = "/t/com.apple.webinspectord_sim.socket"
	iw.listed = false
	r, err = s.Up(context.Background(), "", "", true)
	if err != nil || !r.ForwardStarted || !r.IWDPRestarted || len(*spawned) != 2 {
		t.Fatalf("after a new socket: %+v, %v, %d forwards", r, err, len(*spawned))
	}
}

// Up fails with the reason when the Mac is down and nothing can start it,
// when nobody logs in, and when another program holds the forward's port.
func TestUpFailures(t *testing.T) {
	fm := &fakeMac{}
	fm.reachable = func() bool { return false }
	s, _ := newTestSim(t, fm, &fakeIWDP{}, Config{SSH: "mac"})
	if _, err := s.Up(context.Background(), "", "", false); err == nil || !strings.Contains(err.Error(), "no start command") {
		t.Fatalf("down Mac without start: %v", err)
	}
	fm.reachable = func() bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Up(ctx, "", "", false); err == nil || !strings.Contains(err.Error(), "logged in") {
		t.Fatalf("no login: %v", err)
	}
	fm.loggedIn, fm.booted, fm.socket = true, true, "/s/com.apple.webinspectord_sim.socket"
	l, err := net.Listen("tcp", s.Forward.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if _, err := s.Up(context.Background(), udidA, "", true); err == nil || !strings.Contains(err.Error(), "another program listens") {
		t.Fatalf("foreign listener: %v", err)
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

// Down stops the forward, shuts the booted simulator down, and runs the
// stop command only with mac; mac without a stop command fails first.
func TestDown(t *testing.T) {
	fm := &fakeMac{socket: "/s/com.apple.webinspectord_sim.socket", loggedIn: true, booted: true, window: true}
	fm.reachable = func() bool { return true }
	s, _ := newTestSim(t, fm, &fakeIWDP{onStart: true}, Config{SSH: "mac", Device: udidA})
	if _, err := s.Down(context.Background(), "", true); err == nil || !strings.Contains(err.Error(), "no stop command") {
		t.Fatalf("--mac without stop: %v", err)
	}
	if _, err := s.Up(context.Background(), "", "", false); err != nil {
		t.Fatal(err)
	}
	s.Cfg.Stop = []string{"vmctl", "stop", "mac"}
	r, err := s.Down(context.Background(), "", true)
	if err != nil || r.ShutDown == nil || !r.MacStopped || fm.booted || !fm.did("vmctl stop mac") {
		t.Fatalf("down: %+v, %v, booted %v", r, err, fm.booted)
	}
	if sock, _ := s.Forward.Socket(context.Background()); sock != "" {
		t.Fatalf("forward still runs to %q after Down", sock)
	}
}
