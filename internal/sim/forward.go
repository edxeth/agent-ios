package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// InspectorAddr is where ios_webkit_debug_proxy looks for a simulator (its
// -s address on Linux, proxy.simulatorArgs); sim forwards it to the Mac.
const InspectorAddr = "127.0.0.1:27753"

// forwardRecord is the state of the forward that sim started: its ssh
// process and the socket it forwards to.
type forwardRecord struct {
	PID    int      `json:"pid"`
	Args   []string `json:"args"`   // the ssh command line, to tell this process from a later one with its pid
	Socket string   `json:"socket"` // the webinspectord_sim socket on the Mac
}

// Forward manages the ssh forward from InspectorAddr to the simulator's Web
// Inspector socket. It stops only the ssh process that it started and
// recorded, after it checked that process's command line.
type Forward struct {
	Mac   Mac
	Dir   string // a private directory for the record
	Addr  string // InspectorAddr; tests use a private port
	Run   Runner // ps
	Spawn func(name string, args ...string) (pid int, err error)
	Wait  time.Duration // how long a new forward has to listen
}

func (f *Forward) path() string { return filepath.Join(f.Dir, "sim-forward.json") }

// SpawnDetached starts a program in a session of its own, with no standard
// streams, and does not wait for it.
func SpawnDetached(name string, args ...string) (int, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap it while this process lives
	return pid, nil
}

// current is the recorded forward when its process still runs with the
// recorded command line; ok is false otherwise.
func (f *Forward) current(ctx context.Context) (forwardRecord, bool, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return forwardRecord{}, false, nil
	}
	if err != nil {
		return forwardRecord{}, false, fmt.Errorf("reading the forward record: %w", err)
	}
	var r forwardRecord
	if err := json.Unmarshal(data, &r); err != nil || r.PID <= 0 {
		return forwardRecord{}, false, nil
	}
	out, err := f.Run(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(r.PID))
	if err != nil { // ps exits 1 when no such process runs
		return r, false, nil
	}
	return r, strings.TrimSpace(string(out)) == strings.Join(r.Args, " "), nil
}

// Socket is the socket that the running forward reaches, or "".
func (f *Forward) Socket(ctx context.Context) (string, error) {
	r, ok, err := f.current(ctx)
	if err != nil || !ok {
		return "", err
	}
	return r.Socket, nil
}

// Ensure makes the forward reach socket: it keeps a running forward to it,
// replaces one to another socket, and starts one when none runs. It
// reports whether it started a forward. Another program that listens on
// Addr is an error.
func (f *Forward) Ensure(ctx context.Context, socket string) (bool, error) {
	r, ok, err := f.current(ctx)
	if err != nil {
		return false, err
	}
	if ok && r.Socket == socket && listens(f.Addr) {
		return false, nil
	}
	if ok {
		if err := f.stop(ctx, r); err != nil {
			return false, err
		}
	}
	if listens(f.Addr) {
		return false, fmt.Errorf("another program listens on %s, where the simulator's Web Inspector must be forwarded (ss -ltnp or lsof -i names it): stop it, then run sim up again", f.Addr)
	}
	args := f.Mac.sshArgs([]string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-L", f.Addr + ":" + socket}, "")
	pid, err := f.Spawn("ssh", args...)
	if err != nil {
		return false, fmt.Errorf("starting the ssh forward of %s: %w", f.Addr, err)
	}
	rec := forwardRecord{PID: pid, Args: append([]string{"ssh"}, args...), Socket: socket}
	if err := f.save(rec); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return false, err
	}
	deadline := time.Now().Add(f.Wait)
	for !listens(f.Addr) {
		if time.Now().After(deadline) || syscall.Kill(pid, 0) != nil {
			_ = f.stop(ctx, rec)
			return false, fmt.Errorf("the ssh forward of %s to %s did not start listening within %s", f.Addr, f.Mac.cfg.SSH, f.Wait)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true, nil
}

// Stop stops the recorded forward when it still runs, and removes the record.
func (f *Forward) Stop(ctx context.Context) error {
	r, ok, err := f.current(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return f.remove()
	}
	return f.stop(ctx, r)
}

func (f *Forward) stop(ctx context.Context, r forwardRecord) error {
	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping the ssh forward (pid %d): %w", r.PID, err)
	}
	for i := 0; i < 50 && syscall.Kill(r.PID, 0) == nil; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return f.remove()
}

func (f *Forward) save(r forwardRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := f.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing the forward record: %w", err)
	}
	if err := os.Rename(tmp, f.path()); err != nil {
		return fmt.Errorf("writing the forward record: %w", err)
	}
	return nil
}

func (f *Forward) remove() error {
	if err := os.Remove(f.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the forward record: %w", err)
	}
	return nil
}

// listens reports whether something accepts TCP connections on addr.
func listens(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
