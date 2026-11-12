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

// ErrDeviceNotListed marks Recover's error when the restarted iwdp lists no
// device within recoveredDeviceWait.
var ErrDeviceNotListed = errors.New("the restarted ios_webkit_debug_proxy lists no device")

// ErrUSBLost marks Recover's error for an iwdp that lost usbmuxd but that
// Recover may not restart, or whose restart failed.
var ErrUSBLost = errors.New("ios_webkit_debug_proxy lost its usbmuxd connection, so it no longer sees USB devices")

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

// deviceAdvice tells the user what makes an attached device appear in
// iwdp's listing.
const deviceAdvice = "Unlock the device and keep it unlocked, tap Trust if it asks, and check Settings > Safari > Advanced > Web Inspector; if it stays unlisted, unplug it and plug it in again"

// Recover restarts the iwdp on the default listing address when it lost
// usbmuxd (Starter.Recover).
func Recover(ctx context.Context) (bool, error) { return std.Recover(ctx) }

// Recover restarts the iwdp on s's listing address when it lost usbmuxd
// (see above) and this program started and recorded it. It reports
// whether it restarted iwdp.
//
// It returns false and nil when nothing is to be done or the evidence
// cannot be read: not on Linux, a listing that has an entry or does not
// answer, a subscription socket that accepts no connection, a listening process
// that holds a Unix socket or whose sockets cannot be read, or nothing on
// the address any more (Start's job). For an iwdp
// that lost usbmuxd but is not the recorded one, it returns an ErrUSBLost
// refusal and signals nothing; when the restart fails, an ErrUSBLost
// error. When the restarted iwdp lists no device but iwdp's SIMULATOR
// entry within recoveredDeviceWait, it returns true and an
// ErrDeviceNotListed error with what to do on the device. Its waits for
// locks end with ctx.
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
	if !s.lostUSBMuxd(ctx, ep) || !s.listingEmpty(ctx) {
		return false, nil
	}
	stopped, err := s.stopOwn(ctx, ep)
	if err != nil {
		return false, fmt.Errorf("%w (%s): only a new ios_webkit_debug_proxy sees the devices. %w", ErrUSBLost, s.addr, err)
	}
	if !stopped {
		return false, nil
	}
	if err := s.startLocked(ctx, true); err != nil {
		return false, fmt.Errorf("%w (%s), so iwdp-cli restarted it, and the start failed: %w", ErrUSBLost, s.addr, err)
	}
	listed, err := s.awaitDevice(ctx)
	switch {
	case err != nil:
		return true, fmt.Errorf("iwdp-cli restarted ios_webkit_debug_proxy on %s because it had lost usbmuxd, and the new one's listing failed: %w", s.addr, err)
	case !listed:
		return true, fmt.Errorf("%w on %s after %s, although a usbmuxd runs. %s", ErrDeviceNotListed, s.addr, recoveredDeviceWait, deviceAdvice)
	}
	return true, nil
}

// listingEmpty reports whether s's listing answers with no entry at all. A
// listing that fails is not known to be empty.
func (s *Starter) listingEmpty(ctx context.Context) bool {
	devices, err := listDevicesFromURL(ctx, "http://"+s.addr+"/json")
	return err == nil && len(devices) == 0
}

// lostUSBMuxd reports whether usbmuxd's socket accepts a connection and
// no process that listens on ep holds a Unix-domain socket. Every failed
// read counts as false.
func (s *Starter) lostUSBMuxd(ctx context.Context, ep netip.AddrPort) bool {
	if s.usbmuxdLive(ctx) != nil {
		return false
	}
	pids, err := s.listeners(ctx, ep.Port())
	if err != nil || len(pids) == 0 {
		return false
	}
	for _, pid := range pids {
		if n, err := s.unixSockets(pid); err != nil || n > 0 {
			return false
		}
	}
	return true
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
// variable only so that tests can name a private socket.
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
