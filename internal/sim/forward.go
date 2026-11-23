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
	"slices"
	"strconv"
	"syscall"
	"time"
)

// InspectorAddr is where ios_webkit_debug_proxy looks for a simulator (its
// -s address on Linux, proxy.simulatorArgs); sim forwards it to the Mac.
const InspectorAddr = "127.0.0.1:27753"

// forwardRecord names the ssh process of the forward that sim started: its
// pid, and the start time and start token that tell it from a later
// process with the same pid, and the socket it forwards to.
type forwardRecord struct {
	PID    int    `json:"pid"`
	Start  string `json:"start"`
	Token  string `json:"token"`
	Socket string `json:"socket"` // the webinspectord_sim socket on the Mac
}

// Processes is what Forward needs of this host's processes. The real one
// is proxy's (internal/commands/sim.go); tests use a fake process table.
type Processes struct {
	// Spawn starts name with args in a session of its own, with token in
	// its environment (proxy.StartTokenEnv), and does not wait for it.
	Spawn func(token, name string, args ...string) (pid int, err error)
	// Identify returns the start time and start token of a running pid.
	Identify func(pid int) (start, token string, err error)
	// Signal sends sig; syscall.ESRCH means that no such process runs.
	Signal func(pid int, sig syscall.Signal) error
	// Listeners returns the pids that listen on the TCP port.
	Listeners func(ctx context.Context, port uint16) ([]int, error)
	// NewToken returns a fresh random start token.
	NewToken func() string
}

// Forward manages the ssh forward from Addr to the simulator's Web
// Inspector socket. It signals only the ssh process that it started and
// recorded, after it checked that process's start time and start token,
// and it takes a lock around every change of the record.
type Forward struct {
	Mac   Mac
	Dir   string // a private directory for the record and its lock
	Addr  string // InspectorAddr; tests use a private port
	Procs Processes
	Wait  time.Duration // how long a new forward has to listen, and a stopped one to end
}

func (f *Forward) path() string { return filepath.Join(f.Dir, "sim-forward.json") }

// SpawnDetached starts a program in a session of its own, with no standard
// streams and with token in env under tokenEnv, and does not wait for it.
func SpawnDetached(tokenEnv string) func(token, name string, args ...string) (int, error) {
	return func(token, name string, args ...string) (int, error) {
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), tokenEnv+"="+token)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		pid := cmd.Process.Pid
		go func() { _ = cmd.Wait() }() // reap it while this process lives
		return pid, nil
	}
}

// lock takes the lock of the record (flock), so that concurrent sim
// commands change the forward one at a time.
func (f *Forward) lock(ctx context.Context) (func(), error) {
	fd, err := os.OpenFile(filepath.Join(f.Dir, "sim-forward.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the forward's lock: %w", err)
	}
	for {
		err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN); _ = fd.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = fd.Close()
			return nil, fmt.Errorf("taking the forward's lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = fd.Close()
			return nil, fmt.Errorf("waiting for another sim command's forward lock: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// state is the recorded forward: rec is nil without a record; live is
// true when its process runs as recorded (pid, start time and token).
type state struct {
	rec  *forwardRecord
	live bool
}

func (f *Forward) read() (state, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("reading the forward record: %w", err)
	}
	var r forwardRecord
	if err := json.Unmarshal(data, &r); err != nil || r.PID <= 0 || r.Start == "" || r.Token == "" {
		return state{rec: &forwardRecord{}}, nil // not a record of ours: never signaled
	}
	live, err := f.is(r)
	return state{rec: &r, live: live}, err
}

// is reports whether r's process runs: false when no process has its pid
// or another one does (start time or token differ); an identification that
// fails for another reason is an error.
func (f *Forward) is(r forwardRecord) (bool, error) {
	if err := f.Procs.Signal(r.PID, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	start, token, err := f.Procs.Identify(r.PID)
	if err != nil {
		if f.Procs.Signal(r.PID, 0) != nil { // it ended meanwhile
			return false, nil
		}
		return false, fmt.Errorf("identifying the forward's ssh, pid %d: %w", r.PID, err)
	}
	return start == r.Start && token == r.Token, nil
}

func (f *Forward) port() (uint16, error) {
	_, p, err := net.SplitHostPort(f.Addr)
	if err != nil {
		return 0, fmt.Errorf("forward address %q: %w", f.Addr, err)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("forward address %q: %w", f.Addr, err)
	}
	return uint16(n), nil
}

// listeners returns the pids that listen on Addr.
func (f *Forward) listeners(ctx context.Context) ([]int, error) {
	port, err := f.port()
	if err != nil {
		return nil, err
	}
	pids, err := f.Procs.Listeners(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("checking which process listens on %s: %w", f.Addr, err)
	}
	return pids, nil
}

// serving reports whether st's process runs and is the only listener of Addr.
func (f *Forward) serving(ctx context.Context, st state) (bool, error) {
	if !st.live {
		return false, nil
	}
	pids, err := f.listeners(ctx)
	if err != nil {
		return false, err
	}
	return len(pids) == 1 && pids[0] == st.rec.PID, nil
}

// Serves reports whether the recorded forward runs, alone listens on Addr,
// and reaches socket ("" for any socket); it changes nothing.
func (f *Forward) Serves(ctx context.Context, socket string) (bool, error) {
	st, err := f.read()
	if err != nil {
		return false, err
	}
	ok, err := f.serving(ctx, st)
	return ok && (socket == "" || st.rec.Socket == socket), err
}

// Ensure makes the forward reach socket: it keeps a forward that serves
// socket, replaces its own forward to another socket, and starts one when
// none runs. It reports whether it started a forward. Another program that
// listens on Addr is an error, and so is a process that it cannot identify.
func (f *Forward) Ensure(ctx context.Context, socket string) (bool, error) {
	unlock, err := f.lock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	st, err := f.read()
	if err != nil {
		return false, err
	}
	serving, err := f.serving(ctx, st)
	if err != nil {
		return false, err
	}
	if serving && st.rec.Socket == socket {
		return false, nil
	}
	if err := f.stopLocked(ctx, st); err != nil {
		return false, err
	}
	pids, err := f.listeners(ctx)
	if err != nil {
		return false, err
	}
	if len(pids) > 0 {
		return false, fmt.Errorf("another program (pid %v) listens on %s, where the simulator's Web Inspector must be forwarded: stop it, then run sim up again", pids, f.Addr)
	}
	args := f.Mac.sshArgs([]string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-L", f.Addr + ":" + socket}, "")
	token := f.Procs.NewToken()
	pid, err := f.Procs.Spawn(token, "ssh", args...)
	if err != nil {
		return false, fmt.Errorf("starting the ssh forward of %s: %w", f.Addr, err)
	}
	start, got, err := f.Procs.Identify(pid)
	if err != nil || got != token {
		_ = f.Procs.Signal(pid, syscall.SIGTERM)
		return false, fmt.Errorf("identifying the new ssh forward, pid %d: token %q, %v", pid, got, err)
	}
	rec := forwardRecord{PID: pid, Start: start, Token: token, Socket: socket}
	if err := f.save(rec); err != nil {
		_ = f.Procs.Signal(pid, syscall.SIGTERM)
		return false, err
	}
	// The forward counts only when its own process listens, alone: a
	// program that took the port meanwhile is not the forward.
	deadline := time.Now().Add(f.Wait)
	for {
		pids, err := f.listeners(ctx)
		if err != nil {
			return false, err
		}
		if slices.Contains(pids, pid) {
			if len(pids) > 1 {
				_ = f.stopLocked(ctx, state{rec: &rec, live: true})
				return false, fmt.Errorf("other processes (pids %v) also listen on %s; the forward was stopped", pids, f.Addr)
			}
			return true, nil
		}
		live, err := f.is(rec)
		if err != nil {
			return false, err
		}
		if !live || len(pids) > 0 || time.Now().After(deadline) {
			_ = f.stopLocked(ctx, state{rec: &rec, live: live})
			if len(pids) > 0 {
				return false, fmt.Errorf("another program (pid %v) took %s before the ssh forward listened", pids, f.Addr)
			}
			return false, fmt.Errorf("the ssh forward of %s to %s did not listen within %s (ssh ended: %t)", f.Addr, f.Mac.cfg.SSH, f.Wait, !live)
		}
		select {
		case <-ctx.Done():
			_ = f.stopLocked(context.WithoutCancel(ctx), state{rec: &rec, live: true})
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop stops the recorded forward when it runs, and removes the record.
func (f *Forward) Stop(ctx context.Context) error {
	unlock, err := f.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := f.read()
	if err != nil {
		return err
	}
	return f.stopLocked(ctx, st)
}

// stopLocked stops st's process when it runs as recorded and removes the
// record once that process is gone. A process that does not end within
// Wait is an error, and the record stays, so a later stop finds it. A
// record whose process is gone, or whose pid another process has now, is
// removed without a signal.
func (f *Forward) stopLocked(ctx context.Context, st state) error {
	if st.rec == nil {
		return nil
	}
	if st.live {
		if err := f.Procs.Signal(st.rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stopping the ssh forward (pid %d): %w", st.rec.PID, err)
		}
		deadline := time.Now().Add(f.Wait)
		for {
			live, err := f.is(*st.rec)
			if err != nil {
				return err
			}
			if !live {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the ssh forward (pid %d) did not end within %s after SIGTERM; its record stays", st.rec.PID, f.Wait)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("waiting for the ssh forward (pid %d) to end: %w", st.rec.PID, ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if err := os.Remove(f.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the forward record: %w", err)
	}
	return nil
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
