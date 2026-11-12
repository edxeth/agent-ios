package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
)

// Recovery of an iwdp that lost usbmuxd (Recover; device check of
// 2026-11-11: after usbmuxd restarted on a replug, the running iwdp listed
// [] for good). Every iwdp here is a fake on a private loopback address;
// its device list is the content of a file (fakeListingEnv). usbmuxd's
// socket and the Unix sockets of a process are injected
// (Starter.usbmuxdLive, Starter.unixSockets).

const (
	listedDevice    = `[{"deviceId":"00008020-0000000000000001","deviceName":"iPhone X","url":"localhost:9224"}]`
	listedSimulator = `[{"deviceId":"SIMULATOR","deviceName":"SIMULATOR","url":"localhost:9224"}]`
)

// oneAttached is a Starter.attached that reports one USB device.
func oneAttached(context.Context) ([]string, error) {
	return []string{"00008020-0000000000000001"}, nil
}

// noneAttached is a Starter.attached that reports no USB device.
func noneAttached(context.Context) ([]string, error) { return nil, nil }

// fakeListing makes the fake iwdp of this test serve body as its device
// list and returns a function that changes it.
func fakeListing(t *testing.T, body string) func(string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "listing.json")
	set := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set(body)
	t.Setenv(fakeListingEnv, path)
	return set
}

// liveUSBMuxd is a Starter.usbmuxdLive whose usbmuxd accepts connections;
// noUSBMuxd one without a usbmuxd.
func liveUSBMuxd(context.Context) error { return nil }

func noUSBMuxd(context.Context) error {
	return errors.New("usbmuxd's socket /var/run/usbmuxd: connection refused")
}

// noUnixSockets is a Starter.unixSockets of processes that lost usbmuxd;
// subscribed one of processes that hold their usbmuxd connection.
func noUnixSockets(int) (int, error) { return 0, nil }

func subscribed(int) (int, error) { return 2, nil }

// deafOnly is a Starter.unixSockets in which only the processes deaf
// hold no Unix socket.
func deafOnly(deaf ...int) func(int) (int, error) {
	return func(pid int) (int, error) {
		for _, d := range deaf {
			if pid == d {
				return 0, nil
			}
		}
		return 2, nil
	}
}

// onLinux makes Recover act as on Linux for this test, also on macOS, where
// the tests inject the Linux-only evidence (usbmuxdLive, unixSockets).
func onLinux(t *testing.T) {
	t.Helper()
	old := recoverOS
	recoverOS = "linux"
	t.Cleanup(func() { recoverOS = old })
}

// startedIWDP starts the fake iwdp through a Starter for a free address,
// as iwdp-cli's auto-start does. Its device list holds listing from the
// start number listFrom on (0: never) and is empty before. It returns the
// Starter, the fake, the address and the setter of the listing file.
func startedIWDP(t *testing.T, listing string, listFrom int) (*Starter, *fakeIWDP, string, func(string)) {
	t.Helper()
	onLinux(t)
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	set := fakeListing(t, listing)
	from := 1 << 30
	if listFrom > 0 {
		from = listFrom
	}
	t.Setenv(fakeListingFromEnv, strconv.Itoa(from))
	s := testStarter(addr, 10*time.Second)
	s.attached = oneAttached
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, f, addr, set
}

func TestRecoverRestartsTheRecordedIWDPThatLostUSBMuxd(t *testing.T) {
	s, f, _, _ := startedIWDP(t, listedDevice, 2)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	old := f.pids()
	restarted, err := s.Recover(context.Background())
	if err != nil || !restarted {
		t.Fatalf("Recover of the recorded iwdp that holds no usbmuxd connection = %v, %v; want a restart", restarted, err)
	}
	for _, pid := range old {
		if alive(pid) {
			t.Errorf("the old iwdp %d still runs after the recovery", pid)
		}
	}
	if n := len(f.starts()); n != 2 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 2 (the start and the recovery)", n)
	}
	if calls := pkillCalls(t); len(calls) != 0 {
		t.Errorf("Recover ran pkill: %q", calls)
	}
}

// A device that the restarted iwdp does not list (locked, untrusted, Web
// Inspector off), also when it lists iwdp's SIMULATOR entry, gets the
// device advice from the call that restarted iwdp. The new iwdp started
// after the socket, so no later call restarts it again: no restart loop.
func TestRecoverRestartsOnceAndAdvisesOnAnUnlistedDevice(t *testing.T) {
	for name, listing := range map[string]string{"empty": listedDevice, "SIMULATOR only": listedSimulator} {
		t.Run(name, func(t *testing.T) {
			from := 0
			if listing == listedSimulator {
				from = 2
			}
			s, f, addr, _ := startedIWDP(t, listing, from)
			// Only the first fake lost usbmuxd; the restarted one holds
			// its connection.
			s.usbmuxdLive, s.unixSockets = liveUSBMuxd, deafOnly(f.pids()...)
			restarted, err := s.Recover(context.Background())
			if !restarted || !errors.Is(err, ErrDeviceNotListed) {
				t.Fatalf("Recover whose new iwdp lists no device = %v, %v; want a restart and ErrDeviceNotListed", restarted, err)
			}
			for _, want := range []string{"Unlock the device", "Web Inspector", "plug it in again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			other := testStarter(addr, 10*time.Second) // another process of iwdp-cli
			other.usbmuxdLive, other.unixSockets = s.usbmuxdLive, s.unixSockets
			if restarted, err := other.Recover(context.Background()); restarted || err != nil {
				t.Fatalf("Recover of the restarted iwdp = %v, %v; want nothing done", restarted, err)
			}
			if n := len(f.starts()); n != 2 {
				t.Fatalf("ios_webkit_debug_proxy ran %d times, want 2", n)
			}
		})
	}
}

// An iwdp that holds a Unix socket still sees the devices, one whose
// sockets cannot be read is not known to be deaf, without a live usbmuxd
// a new iwdp could not subscribe, an iwdp whose listing
// has an entry (a device, or the SIMULATOR entry of an iOS Simulator)
// serves it, also when the entry appears while Recover decides, and
// outside Linux Recover never acts: none of them is restarted.
func TestRecoverLeavesAnIWDPThatSeesUSBAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usb     func(context.Context) error
		sockets func(int) (int, error)
		listing string // "" keeps the empty list
		appear  bool   // the listing gains a device while Recover decides
		os      string
	}{
		{"holds a Unix socket", liveUSBMuxd, subscribed, "", false, "linux"},
		{"sockets unreadable", liveUSBMuxd, func(int) (int, error) { return 0, os.ErrPermission }, "", false, "linux"},
		{"no usbmuxd", noUSBMuxd, noUnixSockets, "", false, "linux"},
		{"lists a device", liveUSBMuxd, noUnixSockets, listedDevice, false, "linux"},
		{"lists SIMULATOR", liveUSBMuxd, noUnixSockets, listedSimulator, false, "linux"},
		{"device appears", liveUSBMuxd, noUnixSockets, "", true, "linux"},
		{"darwin", liveUSBMuxd, noUnixSockets, "", false, "darwin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listing, from := listedDevice, 0
			switch {
			case tc.appear:
				listing, from = "[]", 1
			case tc.listing != "":
				listing, from = tc.listing, 1
			}
			s, f, _, set := startedIWDP(t, listing, from)
			s.usbmuxdLive, s.unixSockets = tc.usb, tc.sockets
			if tc.appear {
				// The device appears after the fast path, while the
				// evidence is read under the locks.
				s.usbmuxdLive = func(ctx context.Context) error {
					set(listedDevice)
					return tc.usb(ctx)
				}
			}
			old := recoverOS
			recoverOS = tc.os
			t.Cleanup(func() { recoverOS = old })
			pids, starts := f.pids(), len(f.starts())
			if restarted, err := s.Recover(context.Background()); err != nil || restarted {
				t.Fatalf("Recover = %v, %v; want nothing done", restarted, err)
			}
			for _, pid := range pids {
				if !alive(pid) {
					t.Errorf("iwdp %d was stopped", pid)
				}
			}
			if n := len(f.starts()); n != starts {
				t.Fatalf("ios_webkit_debug_proxy ran %d times, want %d", n, starts)
			}
		})
	}
}

// The user's or another agent's iwdp is never signaled: when it lost
// usbmuxd, Recover refuses and says so, also when a record names another
// process; otherwise it says nothing.
func TestRecoverNeverSignalsAnotherProgramsIWDP(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale record %v", stale), func(t *testing.T) {
			onLinux(t)
			addr := freeAddr(t)
			f := installFakeIWDP(t, fakeServe, addr)
			pid := foreignIWDP(t, addr)
			if stale {
				other := sleeper(t) // a process of this user that started later
				writeRecord(t, addr, other, other)
			}
			s := testStarter(addr, 10*time.Second)
			s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
			restarted, err := s.Recover(context.Background())
			if restarted || !errors.Is(err, ErrUSBLost) {
				t.Fatalf("Recover of another program's iwdp that lost usbmuxd = %v, %v; want ErrUSBLost", restarted, err)
			}
			for _, want := range []string{"restart refused", fmt.Sprintf("pid %d", pid), "nothing was stopped"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			s.unixSockets = subscribed
			if restarted, err := s.Recover(context.Background()); restarted || err != nil {
				t.Fatalf("Recover of another program's iwdp that sees usbmuxd = %v, %v; want nothing done", restarted, err)
			}
			if !alive(pid) {
				t.Fatalf("the foreign iwdp %d was stopped", pid)
			}
			if n := len(f.starts()); n != 0 {
				t.Fatalf("ios_webkit_debug_proxy ran %d times, want 0", n)
			}
			if calls := pkillCalls(t); len(calls) != 0 {
				t.Errorf("Recover ran pkill: %q", calls)
			}
		})
	}
}

// A replacement iwdp that does not start is reported as an ErrUSBLost
// error that names the failed start; nothing claims a restart.
func TestRecoverReportsAFailedRestart(t *testing.T) {
	s, f, _, _ := startedIWDP(t, listedDevice, 0)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	t.Setenv(fakeExitFromEnv, "2") // the restart's fake exits
	old := f.pids()
	restarted, err := s.Recover(context.Background())
	if restarted || !errors.Is(err, ErrUSBLost) || !strings.Contains(err.Error(), "the start failed") || !strings.Contains(err.Error(), "exited during its start") {
		t.Fatalf("Recover whose restart fails = %v, %v; want ErrUSBLost with the failed start", restarted, err)
	}
	for _, pid := range old {
		if alive(pid) {
			t.Errorf("the old iwdp %d still runs", pid)
		}
	}
	if n := len(f.starts()); n != 2 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 2", n)
	}
}

// A restarted iwdp whose listing never answers usably is reported with the
// listing's error, not as an unlisted device.
func TestRecoverReportsAListingThatNeverAnswers(t *testing.T) {
	s, _, _, _ := startedIWDP(t, "not a device list", 2)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	restarted, err := s.Recover(context.Background())
	if !restarted || err == nil || errors.Is(err, ErrDeviceNotListed) || !strings.Contains(err.Error(), "listing failed") || !strings.Contains(err.Error(), "decoding device list") {
		t.Fatalf("Recover whose new iwdp lists no usable list = %v, %v; want a restart and the listing's error", restarted, err)
	}
}

// A restarted iwdp that never replies to its listing within the device
// wait is reported as an error, not as a successful empty wait.
func TestAwaitDeviceReportsAListingWithoutAReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(srv.Close)
	s := testStarter(strings.TrimPrefix(srv.URL, "http://"), time.Second)
	listed, err := s.awaitDevice(context.Background())
	if listed || err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("awaitDevice on a listing that never replies = %v, %v; want no device and an error", listed, err)
	}
}

// Without a process found on the listing port, or when the lookup fails,
// nothing is known about the iwdp's sockets: nothing is restarted.
func TestRecoverNeedsTheListeningProcess(t *testing.T) {
	for name, lookup := range map[string]func(context.Context, uint16) ([]int, error){
		"none": func(context.Context, uint16) ([]int, error) { return nil, nil },
		"failed": func(context.Context, uint16) ([]int, error) {
			return nil, errors.New("reading /proc/net/tcp: permission denied")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _, _ := startedIWDP(t, listedDevice, 0)
			s.usbmuxdLive, s.unixSockets, s.listeners = liveUSBMuxd, noUnixSockets, lookup
			old := f.pids()
			if restarted, err := s.Recover(context.Background()); restarted || err != nil {
				t.Fatalf("Recover with listener lookup %s = %v, %v; want nothing done", name, restarted, err)
			}
			for _, pid := range old {
				if !alive(pid) {
					t.Errorf("iwdp %d was stopped", pid)
				}
			}
			if n := len(f.starts()); n != 1 {
				t.Fatalf("ios_webkit_debug_proxy ran %d times, want 1", n)
			}
		})
	}
}

// A listener that goes away right before the stop (after the last listing
// read) is not replaced: starting iwdp where none runs is Start's job.
func TestRecoverStartsNothingWhenTheListenerGoesBeforeTheStop(t *testing.T) {
	onLinux(t)
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reads.Add(1) == 2 { // the read under the locks, the last before the stop
			w.Header().Set("Connection", "close")
			_ = l.Close()
		}
		_, _ = w.Write([]byte("[]"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	s := testStarter(addr, 10*time.Second)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	s.listeners = func(context.Context, uint16) ([]int, error) { return []int{os.Getpid()}, nil }
	restarted, err := s.Recover(context.Background())
	if restarted || err != nil {
		t.Fatalf("Recover after the listener went away = %v, %v; want nothing done", restarted, err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times after the listener went away, want 0", n)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("listing read %d times, want 2 (the fast path and the read under the locks)", n)
	}
}

// Recover never starts an iwdp where none runs: that is Start's job, with
// its own back-off.
func TestRecoverStartsNothingWithoutAnIWDP(t *testing.T) {
	onLinux(t)
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	if restarted, err := s.Recover(context.Background()); err != nil || restarted {
		t.Fatalf("Recover without an iwdp = %v, %v; want nothing done", restarted, err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 0", n)
	}
}

// Another process's start or restart holds the cross-process start lock
// (lockStart): Recover then neither reads its evidence nor stops the
// recorded iwdp, and ends with the caller's deadline.
func TestRecoverWaitsForAnotherProcesssStartLock(t *testing.T) {
	s, f, addr, _ := startedIWDP(t, listedDevice, 0)
	var inspected atomic.Bool
	s.usbmuxdLive = func(context.Context) error {
		inspected.Store(true)
		return nil
	}
	s.unixSockets = noUnixSockets
	unlock, err := lockStart(context.Background(), addr) // another process's start
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := f.pids()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	restarted, err := s.Recover(ctx)
	if restarted || !errors.Is(err, context.DeadlineExceeded) || inspected.Load() {
		t.Fatalf("Recover while another process holds the start lock = %v, %v (evidence read: %v); want the deadline, no restart and no evidence read", restarted, err, inspected.Load())
	}
	for _, pid := range old {
		if !alive(pid) {
			t.Errorf("iwdp %d was stopped while another process held the start lock", pid)
		}
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}

// Recover waits for a running start or restart only within its context.
func TestRecoverWaitsForTheStartLockWithinTheDeadline(t *testing.T) {
	onLinux(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	t.Cleanup(srv.Close)
	s := testStarter(strings.TrimPrefix(srv.URL, "http://"), time.Second)
	s.usbmuxdLive, s.unixSockets = liveUSBMuxd, noUnixSockets
	s.mu.Lock() // another start runs
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	restarted, err := s.Recover(ctx)
	if restarted || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recover while another start holds the lock = %v, %v; want the deadline", restarted, err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Recover returned after %s with a 100ms deadline", d)
	}
}

// DeviceUDIDs returns the UDIDs that idevice_id -l prints, one per line,
// without empty lines, and BuildConfig pins the first to the first device
// port.
func TestDeviceUDIDsReadsTheList(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -l ] || exit 2\nprintf '00008020-0000000000000001\\n\\n  00008030-0000000000000002  \\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "idevice_id"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	udids, err := DeviceUDIDs(context.Background())
	want := []string{"00008020-0000000000000001", "00008030-0000000000000002"}
	if err != nil || !reflect.DeepEqual(udids, want) {
		t.Fatalf("DeviceUDIDs = %q, %v; want %q", udids, err, want)
	}
	if got := BuildConfig(udids); !strings.Contains(got, "00008020-0000000000000001:9224") {
		t.Fatalf("BuildConfig = %q, want the first UDID on 9224", got)
	}
}

// usbmuxd's device list is read within the caller's deadline, also when a
// child of idevice_id keeps its output open, and a failed read is an
// error, not an empty list.
func TestDeviceUDIDsEndsWithTheDeadline(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n(sleep 5; echo late) &\nexec sleep 5\n"
	if err := os.WriteFile(filepath.Join(bin, "idevice_id"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if udids, err := DeviceUDIDs(ctx); udids != nil || err == nil {
		t.Fatalf("DeviceUDIDs = %q, %v; want an error after the deadline", udids, err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("DeviceUDIDs returned after %s with a 100ms deadline", d)
	}
}

// The evidence readers: on Linux, unixSockets counts a Unix listener of
// this process, none of a process without sockets, and fails for a gone
// process (so Recover does nothing); other systems read no /proc, since
// Recover does not act there. usbmuxdLive checks iwdp's subscription
// socket only.
func TestRecoverEvidenceReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usbmuxd")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if runtime.GOOS == "linux" {
		before, err := unixSockets(os.Getpid())
		if err != nil {
			t.Fatalf("unixSockets of this process: %v", err)
		}
		extra, err := net.Listen("unix", filepath.Join(t.TempDir(), "extra"))
		if err != nil {
			t.Fatal(err)
		}
		if n, err := unixSockets(os.Getpid()); err != nil || n != before+1 {
			t.Fatalf("unixSockets after a Unix listen = %d, %v; want %d", n, err, before+1)
		}
		_ = extra.Close()
		if n, err := unixSockets(sleeper(t)); err != nil || n != 0 {
			t.Fatalf("unixSockets of sleep = %d, %v; want 0", n, err)
		}
		if _, err := unixSockets(deadPID(t)); err == nil {
			t.Fatal("unixSockets of a gone process: no error")
		}
	}
	// iwdp 1.9.2 subscribes through USBMUXD_FILE_PATH, "/var/run/usbmuxd"
	// (src/device_listener.c:43), whatever USBMUXD_SOCKET_ADDRESS says.
	if usbmuxdSubscriptionSocket != "/var/run/usbmuxd" {
		t.Fatalf("usbmuxdSubscriptionSocket = %q, want iwdp 1.9.2's /var/run/usbmuxd", usbmuxdSubscriptionSocket)
	}
	old := usbmuxdSubscriptionSocket
	t.Cleanup(func() { usbmuxdSubscriptionSocket = old })
	usbmuxdSubscriptionSocket = path
	if err := usbmuxdLive(context.Background()); err != nil {
		t.Fatalf("usbmuxdLive with a listening socket: %v", err)
	}
	// A live socket that only USBMUXD_SOCKET_ADDRESS names is not iwdp's.
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "UNIX:"+path)
	usbmuxdSubscriptionSocket = filepath.Join(t.TempDir(), "absent")
	if err := usbmuxdLive(context.Background()); err == nil {
		t.Fatal("usbmuxdLive without the subscription socket: no error (it followed USBMUXD_SOCKET_ADDRESS)")
	}
	// And a USBMUXD_SOCKET_ADDRESS that names no socket does not hide it.
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "UNIX:"+filepath.Join(t.TempDir(), "absent"))
	usbmuxdSubscriptionSocket = path
	if err := usbmuxdLive(context.Background()); err != nil {
		t.Fatalf("usbmuxdLive with the subscription socket and another USBMUXD_SOCKET_ADDRESS: %v", err)
	}
}

// unixSockets fails, so that Recover does nothing, when /proc/net/unix
// cannot be read or a descriptor link cannot be read; a descriptor that
// closes during the listing is no error. A private /proc fixture.
func TestUnixSocketsFailsOnUnreadableEvidence(t *testing.T) {
	old := procFS
	t.Cleanup(func() { procFS = old })
	fake := t.TempDir()
	procFS = fake
	const pid = 4242
	fdDir := filepath.Join(fake, strconv.Itoa(pid), "fd")
	if err := os.MkdirAll(fdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[777]", filepath.Join(fdDir, "3")); err != nil {
		t.Fatal(err)
	}
	if n, err := unixSockets(pid); err == nil {
		t.Fatalf("unixSockets without /proc/net/unix = %d, nil; want an error", n)
	}
	if err := os.MkdirAll(filepath.Join(fake, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	table := "Num       RefCount Protocol Flags    Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 777 /run/x\n"
	if err := os.WriteFile(filepath.Join(fake, "net", "unix"), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := unixSockets(pid); err != nil || n != 1 {
		t.Fatalf("unixSockets of the fixture = %d, %v; want 1", n, err)
	}
	// A descriptor entry that is not a link cannot be read as one.
	if err := os.WriteFile(filepath.Join(fdDir, "4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := unixSockets(pid); err == nil {
		t.Fatalf("unixSockets with an unreadable descriptor link = %d, nil; want an error", n)
	}
}

// The caller's context that ends during the wait for the restarted iwdp's
// device, after the listing answered empty, is reported as the context's
// error (its cancel or its deadline), not as an unlisted device. A second
// request shows that the first empty reply was read; each case fails as
// invalid without one.
func TestAwaitDeviceReportsTheCallersEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"cancel", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.want == context.DeadlineExceeded {
				// Shorter than recoveredDeviceWait, longer than a reply.
				ctx, cancel = context.WithTimeout(context.Background(), recoveredDeviceWait/3)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			var replies atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if replies.Add(1) == 2 && tc.want == context.Canceled {
					cancel() // the first empty reply was read
				}
				_, _ = w.Write([]byte("[]"))
			}))
			t.Cleanup(srv.Close)
			s := testStarter(strings.TrimPrefix(srv.URL, "http://"), time.Second)
			listed, err := s.awaitDevice(ctx)
			if replies.Load() < 2 {
				t.Fatal("invalid run: the context ended before an empty reply was read")
			}
			if listed || !errors.Is(err, tc.want) {
				t.Fatalf("awaitDevice whose context ends after an empty reply = %v, %v; want %v", listed, err, tc.want)
			}
		})
	}
}

// recoverCase is a generated input of the recovery decision for an iwdp
// that this program did not start: a listing of entries (each a device or
// the SIMULATOR entry), whether usbmuxd's socket is live, how many Unix
// sockets the listener holds (-1: unreadable), and the system.
type recoverCase struct {
	Entries []bool // true: a device, false: SIMULATOR
	Live    bool
	Sockets int
	Linux   bool
}

// Generate makes listings of 0 to 3 entries, empty ones in about half the
// cases.
func (recoverCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := recoverCase{Live: r.Intn(4) > 0, Linux: r.Intn(4) > 0, Sockets: r.Intn(4) - 1}
	if r.Intn(2) == 1 {
		for range 1 + r.Intn(3) {
			c.Entries = append(c.Entries, r.Intn(2) == 1)
		}
	}
	return reflect.ValueOf(c)
}

// Property: Recover never restarts an iwdp that is not recorded, and it
// reports one only when it lost usbmuxd (Linux, an empty listing, a live
// usbmuxd socket, a listener that holds no Unix socket), with an
// ErrUSBLost refusal. Every other case changes nothing and reports
// nothing.
func TestRecoverDecisionProperty(t *testing.T) {
	f := installFakeIWDP(t, fakeServe, freeAddr(t)) // no real iwdp or idevice_id
	old := recoverOS
	t.Cleanup(func() { recoverOS = old })
	prop := func(c recoverCase) bool {
		var entries []string
		for i, device := range c.Entries {
			id := "SIMULATOR"
			if device {
				id = fmt.Sprintf("00008020-%016d", i)
			}
			entries = append(entries, fmt.Sprintf(`{"deviceId":%q,"url":"localhost:%d"}`, id, 9224+i))
		}
		body := "[" + strings.Join(entries, ",") + "]"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		defer srv.Close()
		s := testStarter(strings.TrimPrefix(srv.URL, "http://"), time.Second)
		s.usbmuxdLive = func(context.Context) error {
			if !c.Live {
				return errors.New("refused")
			}
			return nil
		}
		s.unixSockets = func(int) (int, error) {
			if c.Sockets < 0 {
				return 0, os.ErrPermission
			}
			return c.Sockets, nil
		}
		recoverOS = "darwin"
		if c.Linux {
			recoverOS = "linux"
		}
		restarted, err := s.Recover(context.Background())
		if restarted {
			t.Logf("case %+v: restarted an iwdp that is not recorded", c)
			return false
		}
		lost := c.Linux && len(c.Entries) == 0 && c.Live && c.Sockets == 0
		if lost != (err != nil) || (lost && (!errors.Is(err, ErrUSBLost) || !strings.Contains(err.Error(), "restart refused"))) {
			t.Logf("case %+v: err %v, want a refusal: %v", c, err, lost)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 80}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 0", n)
	}
}

// A failed start explains a device that refused the inspector connection
// (the iwdp log line of a locked device), an iwdp that exited while
// usbmuxd reports no device, and an iwdp that does not answer while
// usbmuxd reports one; a failed read of usbmuxd's devices gives no device
// advice.
func TestStartAdviceNamesTheDeviceOrUsbmuxd(t *testing.T) {
	failedRead := func(context.Context) ([]string, error) { return nil, errors.New("idevice_id: exit status 1") }
	for _, tc := range []struct {
		name     string
		mode     string
		wait     time.Duration
		attached func(context.Context) ([]string, error)
		want     []string
		notWant  []string
	}{
		{"locked device", fakeLocked, 2 * time.Second, oneAttached, []string{"did not answer", "did not accept the Web Inspector connection", "Unlock the device"}, nil},
		{"no usbmuxd device", fakeFail, 5 * time.Second, noneAttached, []string{"exited during its start", "usbmuxd reports no USB device", "connect the device"}, nil},
		{"slow device connection", fakeHang, time.Second, oneAttached, []string{"did not answer", "while it connects to a USB device", "keep the device unlocked"}, nil},
		{"exit with a device", fakeFail, 5 * time.Second, oneAttached, []string{"exited during its start"}, []string{"usbmuxd", "Web Inspector"}},
		{"exit, devices unknown", fakeFail, 5 * time.Second, failedRead, []string{"exited during its start"}, []string{"usbmuxd reports"}},
		{"no answer, devices unknown", fakeHang, time.Second, failedRead, []string{"did not answer"}, []string{"connects to a USB device"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := freeAddr(t)
			installFakeIWDP(t, tc.mode, addr)
			s := testStarter(addr, tc.wait)
			s.attached = tc.attached
			err := s.Start(context.Background())
			if err == nil {
				t.Fatal("Start of a failing fake passed")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Start = %v, want %q", err, want)
				}
			}
			for _, not := range tc.notWant {
				if strings.Contains(err.Error(), not) {
					t.Errorf("Start = %v, want no %q", err, not)
				}
			}
		})
	}
}
