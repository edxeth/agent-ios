package proxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Recovery of an iwdp that no longer sees the USB devices.
//
// ios_webkit_debug_proxy 1.9.2 subscribes to usbmuxd's device events once,
// at its start, through usbmuxd's socket, and never subscribes again (its
// handler of a closed subscription, iwdp_idl_close, does nothing; upstream
// issue #28). Where usbmuxd exits when the last device is unplugged and
// starts again when one is plugged in (the udev and systemd setup of Linux
// distributions, "usbmuxd --systemd"), an iwdp that runs across an unplug
// lists no device from then on (device check of 2026-11-11, iPhone X on
// iOS 16.7). Only a new iwdp sees the device again.
//
// Recover tells such an iwdp at the stop boundary, under the locks, from
// the iwdp process itself: it holds no Unix-domain socket. iwdp 1.9.2 on
// Linux talks to usbmuxd, and through usbmuxd to every device, over Unix
// sockets only (its listeners and its SIMULATOR connection are TCP), and
// it keeps its usbmuxd subscription open while it runs (device check of
// 2026-11-11: a working iwdp held two Unix sockets, the deaf one none). An
// answering iwdp without one has lost usbmuxd for good, and has no device
// connection whose page a restart could end (the amendment of decision
// D10, docs/guide.md). Recover also requires that the socket through which
// iwdp subscribes (usbmuxdSubscriptionSocket) accepts a connection, so
// that the new iwdp can subscribe. Every fact that cannot
// be read (another user's process, no /proc) counts as "do nothing". It
// runs on Linux only: there no iOS Simulator runs, and macOS keeps usbmuxd
// running under launchd. It restarts only the iwdp that this program
// started and recorded (stopOwn's identity and ownership checks, decision
// L02); the check and the signal are separate system calls, as in Restart.
//
// An iwdp can also lose one device while it keeps its usbmuxd
// subscription. iwdp 1.9.2 ends its whole connection to a device when it
// fails to pass one inspector message to a WebSocket client that is
// closing or gone (iwdp_on_applicationSentData returns the failed
// ws_send_frame, and rpc_recv_msg then reports "Invalid message" and the
// device connection closes), and it connects to a device only on usbmuxd's
// attach event, so it never sees that device again (device check of
// 2026-11-12: "Invalid message _rpc_applicationSentData", then
// "Disconnected :9224 from iPhone X", an empty listing for good, while
// idevice_id -l listed the iPhone). Recover restarts such an iwdp too: an
// empty listing while usbmuxd reports a USB device (Starter.attached). A
// device can refuse the inspector connection for a while after it lost
// one, and a locked device or one with Web Inspector off never accepts
// it, so Recover restarts for this reason only an iwdp that started
// deviceRestartBackoff or longer ago (the age of its record): no restart
// loop, one attempt per back-off while commands run.

// ErrDeviceNotListed marks Recover's error when the restarted iwdp lists no
// device within recoveredDeviceWait.
var ErrDeviceNotListed = errors.New("the restarted ios_webkit_debug_proxy lists no device")

// ErrUSBLost marks Recover's error for an iwdp that lost usbmuxd but that
// Recover may not restart, or whose restart failed.
var ErrUSBLost = errors.New("ios_webkit_debug_proxy lost its usbmuxd connection, so it no longer sees USB devices")

// ErrDeviceLost marks Recover's error for an iwdp that lists no device
// while usbmuxd reports one, which Recover may not restart, or whose
// restart failed.
var ErrDeviceLost = errors.New("ios_webkit_debug_proxy lists no device, although usbmuxd reports a USB device: it lost its connection to the device, and ios_webkit_debug_proxy 1.9.2 never connects to it again by itself")

// deviceRestartBackoff is the youngest age of an iwdp that Recover
// restarts because it lists no attached device: the device check of
// 2026-11-12 saw an iPhone refuse the restart right after the loss and
// accept one 30 s later.
const deviceRestartBackoff = StartBackoff

// lostKind is what Recover found an iwdp to have lost.
type lostKind int

const (
	lostNothing lostKind = iota
	lostUSBMuxd          // its usbmuxd subscription: every device
	lostDevice           // its connection to a device that usbmuxd reports
)

// recoverOS is the system on which Recover acts (runtime.GOOS; tests of
// other systems set it).
var recoverOS = runtime.GOOS

// recoveredDeviceWait bounds the wait after a recovery restart for the new
// iwdp to list a device; device check 3 saw iwdp list a device within a
// second of its start.
const recoveredDeviceWait = 3 * time.Second

// recoveredDevicePoll is the interval of the listing reads of that wait.
const recoveredDevicePoll = 200 * time.Millisecond

// lockPoll is the interval at which a wait for a lock within a context
// tries the lock (LockContext).
const lockPoll = 10 * time.Millisecond

// usbmuxdDial bounds the connection that proves usbmuxd's socket is live.
const usbmuxdDial = time.Second

// DeviceAdvice tells the user what makes an attached device appear in
// iwdp's listing.
const DeviceAdvice = "Unlock the device and keep it unlocked, tap Trust if it asks, and check Settings > Safari > Advanced > Web Inspector; if it stays unlisted, unplug it and plug it in again"

// Recover restarts the iwdp on the default listing address when it lost
// usbmuxd or its device (Starter.Recover).
func Recover(ctx context.Context) (bool, error) { return std.Recover(ctx) }

// AwaitDevice waits up to recoveredDeviceWait, and not past ctx, until the
// default listing has an entry other than iwdp's SIMULATOR entry, as after
// a recovery (Starter.awaitDevice): iwdp lists a device a moment after
// its start.
func AwaitDevice(ctx context.Context) (bool, error) { return std.awaitDevice(ctx) }

// AttachedDevices returns the UDIDs of the USB devices that usbmuxd
// reports (DeviceUDIDs), once usbmuxd's socket accepts a connection
// (usbmuxdLive); without a live usbmuxd it returns that error and no list.
func AttachedDevices(ctx context.Context) ([]string, error) {
	if err := std.usbmuxdLive(ctx); err != nil {
		return nil, err
	}
	return std.attached(ctx)
}

// Recover restarts the iwdp on s's listing address when it lost usbmuxd
// or its device (see above) and this program started and recorded it. It
// reports whether it restarted iwdp.
//
// It returns false and nil when nothing is to be done or the evidence
// cannot be read: not on Linux, a listing that has an entry or does not
// answer, a subscription socket that accepts no connection, a listening
// process whose sockets cannot be read, a listener that holds a Unix
// socket while usbmuxd reports no USB device or its device list cannot be
// read, a recorded iwdp younger than deviceRestartBackoff that holds one,
// or nothing on the address any more (Start's job). For an iwdp that lost
// usbmuxd or its device but is not the recorded one, it returns an
// ErrUSBLost or ErrDeviceLost refusal and signals nothing; when the
// restart fails, such an error. When the restarted iwdp lists no device
// but iwdp's SIMULATOR entry within recoveredDeviceWait, it returns true
// and an ErrDeviceNotListed error with what to do on the device. Its
// waits for locks end with ctx.
func (s *Starter) Recover(ctx context.Context) (bool, error) {
	if recoverOS != "linux" {
		return false, nil
	}
	ep, err := netip.ParseAddrPort(s.addr)
	if err != nil {
		return false, nil // a listing that Restart cannot check is never restarted
	}
	// A fast path; the decision reads everything again under the locks.
	if !s.listingEmpty(ctx) {
		return false, nil
	}
	if err := LockContext(ctx, &s.mu); err != nil {
		return false, err
	}
	defer s.mu.Unlock()
	unlock, err := lockStart(ctx, s.addr)
	if err != nil {
		return false, err
	}
	defer unlock()
	// The listing last, right before the stop.
	lost := s.lost(ctx, ep)
	if lost == lostNothing || (lost == lostDevice && s.ownYoung(ctx, ep)) || !s.listingEmpty(ctx) {
		return false, nil
	}
	kind, what, retry := ErrUSBLost, "usbmuxd", ""
	if lost == lostDevice {
		kind, what = ErrDeviceLost, "its device"
		retry = fmt.Sprintf(" A device can refuse a new connection for a while after it lost one: keep it unlocked, and run the command again after %s, when agent-ios restarts ios_webkit_debug_proxy once more.", deviceRestartBackoff)
	}
	stopped, err := s.stopOwn(ctx, ep)
	if err != nil {
		return false, fmt.Errorf("%w (%s): only a new ios_webkit_debug_proxy sees the devices. %w", kind, s.addr, err)
	}
	if !stopped {
		return false, nil
	}
	if err := s.startLocked(ctx, true); err != nil {
		return false, fmt.Errorf("%w (%s), so agent-ios restarted it, and the start failed: %w", kind, s.addr, err)
	}
	listed, err := s.awaitDevice(ctx)
	switch {
	case err != nil:
		return true, fmt.Errorf("agent-ios restarted ios_webkit_debug_proxy on %s because it had lost %s, and the new one's listing failed: %w", s.addr, what, err)
	case !listed:
		return true, fmt.Errorf("%w on %s after %s, although a usbmuxd runs. %s.%s", ErrDeviceNotListed, s.addr, recoveredDeviceWait, DeviceAdvice, retry)
	}
	return true, nil
}

// lost tells what the iwdp that listens on ep lost, from usbmuxd and the
// listening processes: usbmuxd when none of them holds a Unix socket, its
// device when they hold one and usbmuxd reports a USB device. The caller
// has seen an empty listing. Every failed read counts as lostNothing.
func (s *Starter) lost(ctx context.Context, ep netip.AddrPort) lostKind {
	if s.usbmuxdLive(ctx) != nil {
		return lostNothing
	}
	pids, err := s.listeners(ctx, ep.Port())
	if err != nil || len(pids) == 0 {
		return lostNothing
	}
	holding := 0
	for _, pid := range pids {
		n, err := s.unixSockets(pid)
		if err != nil {
			return lostNothing
		}
		if n > 0 {
			holding++
		}
	}
	switch {
	case holding == 0:
		return lostUSBMuxd
	case holding < len(pids):
		return lostNothing // processes that disagree tell nothing
	}
	if udids, err := s.attached(ctx); err != nil || len(udids) == 0 {
		return lostNothing
	}
	return lostDevice
}

// ownYoung reports whether the iwdp that listens on ep is the one that
// this program started and recorded (a valid record whose process owns
// ep, as stopOwn checks), and its record was written less than
// deviceRestartBackoff ago, at that iwdp's start. Otherwise it reports
// false, and the stop refuses another program's iwdp (review of
// 2026-11-12: a young record of an iwdp that had exited hid that
// refusal).
func (s *Starter) ownYoung(ctx context.Context, ep netip.AddrPort) bool {
	dir, err := recordDir()
	if err != nil {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, recordName(s.addr)))
	if err != nil || time.Since(fi.ModTime()) >= deviceRestartBackoff {
		return false
	}
	rec, err := s.loadRecord()
	if err != nil || rec == nil || rec.problem(s.addr) != "" {
		return false
	}
	return s.owns(ctx, rec, ep) == nil
}

// listingEmpty reports whether s's listing answers with no entry at all. A
// listing that fails is not known to be empty.
func (s *Starter) listingEmpty(ctx context.Context) bool {
	devices, err := listDevicesFromURL(ctx, "http://"+s.addr+"/json")
	return err == nil && len(devices) == 0
}

// awaitDevice waits up to recoveredDeviceWait, and not past ctx, until s's
// listing has an entry other than iwdp's SIMULATOR entry. It reports
// whether one appeared; when ctx ended first it returns ctx's error, and
// when no read of the listing succeeded, the last read's error.
func (s *Starter) awaitDevice(ctx context.Context) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, recoveredDeviceWait)
	defer cancel()
	tick := time.NewTicker(recoveredDevicePoll)
	defer tick.Stop()
	var last error
	answered := false
	for {
		devices, err := listDevicesFromURL(wctx, "http://"+s.addr+"/json")
		if err == nil {
			answered = true
			for _, d := range devices {
				if d.DeviceID != simulatorDeviceID {
					return true, nil
				}
			}
		} else if wctx.Err() == nil {
			last = err // a read that the wait's end cut short tells nothing
		}
		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return false, err // the caller's end, not the wait's
			}
			if !answered {
				if last == nil {
					last = fmt.Errorf("no answer within %s", recoveredDeviceWait)
				}
				return false, last
			}
			return false, nil
		case <-tick.C:
		}
	}
}

// simulatorDeviceID is the device id of iwdp's SIMULATOR entry.
const simulatorDeviceID = "SIMULATOR"

// LockContext locks mu, or returns ctx's error when ctx ends first.
func LockContext(ctx context.Context, mu *sync.Mutex) error {
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for another start of ios_webkit_debug_proxy: %w", ctx.Err())
		case <-time.After(lockPoll):
		}
	}
	return nil
}

// usbmuxdSubscriptionSocket is the socket through which iwdp 1.9.2
// subscribes to usbmuxd's device events: its own device listener
// (dl_connect in src/device_listener.c, USBMUXD_FILE_PATH) always connects
// to this path and ignores libusbmuxd's USBMUXD_SOCKET_ADDRESS. It is a
// variable only so that tests can name a private socket, and so that the
// e2e tests can link binaries that never see the host's usbmuxd (-ldflags
// -X, e2e/listing_w1c6_test.go): those binaries recover no iwdp and
// report no attached device.
var usbmuxdSubscriptionSocket = "/var/run/usbmuxd"

// usbmuxdLive returns nil when usbmuxdSubscriptionSocket accepts a
// connection within usbmuxdDial and ctx (a live usbmuxd listens on it).
func usbmuxdLive(ctx context.Context) error {
	path := usbmuxdSubscriptionSocket
	dctx, cancel := context.WithTimeout(ctx, usbmuxdDial)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dctx, "unix", path)
	if err != nil {
		return fmt.Errorf("usbmuxd's socket %s: %w", path, err)
	}
	return c.Close()
}

// unixSockets returns how many Unix-domain sockets process pid holds: its
// file descriptors that are sockets (/proc/<pid>/fd, "socket:[<inode>]")
// whose inode /proc/net/unix lists. Only the process's own user can read
// its descriptors; any failed read is an error.
func unixSockets(pid int) (int, error) {
	table, err := os.ReadFile(filepath.Join(procFS, "net", "unix"))
	if err != nil {
		return 0, err
	}
	inodes := map[string]bool{}
	for i, line := range strings.Split(string(table), "\n") {
		// Num RefCount Protocol Flags Type St Inode [Path]
		if f := strings.Fields(line); i > 0 && len(f) >= 7 {
			inodes[f[6]] = true
		}
	}
	dir := filepath.Join(procFS, strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // closed meanwhile
			}
			return 0, err
		}
		if inode, ok := strings.CutPrefix(link, "socket:["); ok && inodes[strings.TrimSuffix(inode, "]")] {
			n++
		}
	}
	return n, nil
}
