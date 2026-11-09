package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/logcause"
)

// tokenEnv carries the start token of an iwdp that Starter starts in that
// process's environment. Only this program sets it, so a live process
// with the recorded token is the iwdp this program started (decision L02,
// review round 1, F3).
const tokenEnv = "IWDP_MCP_START_TOKEN"

// record is an ios_webkit_debug_proxy that a Starter started and saw serve
// its listing address (decision L02). The started process leads its own
// process group, so PID and PGID are equal. Start and Token identify the
// process incarnation (identity), so a reused pid never matches.
type record struct {
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Addr  string `json:"addr"`  // the listing address, host:port
	Start string `json:"start"` // identity.start of the started process
	Token string `json:"token"` // the tokenEnv value it was started with
}

// identity tells one process incarnation from another process with the
// same pid.
type identity struct {
	start string // its start time (procIdentity, psStart)
	token string // its tokenEnv value, "" without one
}

// matches reports whether id is the process that rec names: the recorded
// start time and the recorded start token, both required. The token
// proves that this program started the process, so a record alone, which
// another program of this user can write, never authorizes a signal
// (review round 2, F3).
func (rec *record) matches(id identity) bool {
	return rec.Start != "" && rec.Token != "" && id.start == rec.Start && id.token == rec.Token
}

// readIdentity returns the identity of process pid: from /proc on Linux
// (procIdentity); on darwin the start time from ps (psStart) and the token
// from the process's exec environment, read with sysctl KERN_PROCARGS2
// (argsIdentity, kernProcargs2). Both systems show another process's
// environment only to its user; any failed read is an error, and then
// nothing is signaled.
func readIdentity(pid int) (identity, error) {
	switch runtime.GOOS {
	case "linux":
		return procIdentity(pid)
	case "darwin":
		return argsIdentity(pid, psStart, kernProcargs2)
	}
	return identity{}, fmt.Errorf("cannot identify process %d on %s", pid, runtime.GOOS)
}

// tokenIn returns the tokenEnv value of the environment env ("NAME=value"
// strings), or "" without one.
func tokenIn(env []string) string {
	token := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, tokenEnv+"="); ok {
			token = v
		}
	}
	return token
}

// procIdentity (Linux) reads the start time of process pid, field 22 of
// /proc/<pid>/stat in clock ticks after boot, and its tokenEnv value from
// /proc/<pid>/environ, which only the same user can read.
func procIdentity(pid int) (identity, error) {
	stat, err := os.ReadFile(filepath.Join(procFS, strconv.Itoa(pid), "stat"))
	if err != nil {
		return identity{}, err
	}
	// pid (comm) state ppid ...: comm can hold spaces and parentheses, so
	// the fields after it start at field 3.
	s := string(stat)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	const startField = 22 - 3
	if len(f) <= startField {
		return identity{}, fmt.Errorf("%s/%d/stat has %d fields after the command name", procFS, pid, len(f))
	}
	env, err := os.ReadFile(filepath.Join(procFS, strconv.Itoa(pid), "environ"))
	if err != nil {
		return identity{}, err
	}
	return identity{start: f[startField], token: tokenIn(strings.Split(string(env), "\x00"))}, nil
}

// psStart (darwin) reads the start time of process pid with
// 'ps -o lstart= -p <pid>' (one-second resolution) in the C locale, so a
// reader with another locale reads the same text.
func psStart(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	start := strings.Join(strings.Fields(string(out)), " ")
	if err != nil || start == "" {
		return "", logcause.Errorf("ps found no process %d (%v)", pid, err)
	}
	return start, nil
}

// argsIdentity (darwin) returns the identity of process pid: its start
// time from start (psStart) and its tokenEnv value from the
// KERN_PROCARGS2 buffer that procargs reads (kernProcargs2). darwin gives
// that buffer only for this user's processes. A failed or malformed read
// is an error, so the caller refuses to signal.
func argsIdentity(pid int, start func(int) (string, error), procargs func(int) ([]byte, error)) (identity, error) {
	st, err := start(pid)
	if err != nil {
		return identity{}, err
	}
	buf, err := procargs(pid)
	if err != nil {
		return identity{}, fmt.Errorf("reading the environment of process %d (sysctl KERN_PROCARGS2): %w", pid, err)
	}
	env, err := procargsEnv(buf)
	if err != nil {
		return identity{}, fmt.Errorf("reading the environment of process %d: %w", pid, err)
	}
	return identity{start: st, token: tokenIn(env)}, nil
}

// procargsEnv returns the strings after the arguments of a KERN_PROCARGS2
// buffer (darwin): the argument count as a native-endian 32-bit integer,
// the executable path, NUL padding, the arguments, the environment, NUL
// padding and darwin's own strings (pfz=..., ptr_munge=...), each string
// NUL-terminated. No empty string reliably ends the environment, so the
// result is every nonempty string after the arguments: the environment
// and darwin's strings, which never name tokenEnv. A buffer that ends
// inside a string is an error.
func procargsEnv(buf []byte) ([]string, error) {
	const countSize = 4
	if len(buf) < countSize {
		return nil, fmt.Errorf("the KERN_PROCARGS2 buffer has %d bytes, too few for its argument count", len(buf))
	}
	argc := binary.NativeEndian.Uint32(buf)
	rest := buf[countSize:]
	// next returns the NUL-terminated string at the start of rest.
	next := func(what string) (string, error) {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return "", fmt.Errorf("the KERN_PROCARGS2 buffer ends inside %s", what)
		}
		s := string(rest[:i])
		rest = rest[i+1:]
		return s, nil
	}
	if _, err := next("the executable path"); err != nil {
		return nil, err
	}
	rest = bytes.TrimLeft(rest, "\x00")
	// Each argument takes at least its NUL, so the loop ends with the buffer.
	for n := uint32(1); n <= argc; n++ {
		if _, err := next(fmt.Sprintf("argument %d of %d", n, argc)); err != nil {
			return nil, err
		}
	}
	var env []string
	for len(rest) > 0 {
		kv, err := next("the environment")
		if err != nil {
			return nil, err
		}
		if kv != "" {
			env = append(env, kv)
		}
	}
	return env, nil
}

// recordDir returns this user's private directory of iwdp records,
// creating it: XDG_RUNTIME_DIR/iwdp-mcp when XDG_RUNTIME_DIR is a
// directory of this user that others cannot write, else
// <temporary directory>/iwdp-mcp-<uid>. The directory must be this user's,
// not a symlink, with mode 0700 (checkPrivate), on every use: another user
// can create names in a shared temporary directory.
func recordDir() (string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("iwdp-mcp-%d", os.Getuid()))
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" && checkPrivate(x, true, 0o022) == nil {
		dir = filepath.Join(x, "iwdp-mcp")
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("creating the iwdp record directory: %w", err)
	}
	return dir, checkPrivate(dir, true, 0o077)
}

// checkPrivate returns an error unless path (not followed when it is a
// symlink) is a directory (dir) or a regular file of this user without
// any of the permission bits forbidden.
func checkPrivate(path string, dir bool, forbidden fs.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkInfo(path, fi, dir, forbidden)
}

// checkInfo is checkPrivate for the file information fi of path.
func checkInfo(path string, fi fs.FileInfo, dir bool, forbidden fs.FileMode) error {
	st, _ := fi.Sys().(*syscall.Stat_t) // always a Stat_t on Linux and darwin
	switch {
	case dir && !fi.IsDir():
		return fmt.Errorf("%s is not a directory (mode %v)", path, fi.Mode())
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	case st == nil || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s is not owned by this user (uid %d)", path, os.Getuid())
	case fi.Mode().Perm()&forbidden != 0:
		return fmt.Errorf("%s has mode %v; others must not have the access %v", path, fi.Mode().Perm(), forbidden)
	}
	return nil
}

// recordName is the name of the record file of the listing address addr
// in recordDir, named like the start lock.
func recordName(addr string) string { return runtimeName("started", addr, "json") }

// saveRecord records the started iwdp with pid and start token in s and
// in the record file, so that a restart from this or another process of
// this program can find it. The file is written to a new temporary file
// (mode 0600) and renamed, so a reader never sees a partial record. The
// caller holds the start lock.
func (s *Starter) saveRecord(pid int, token string) error {
	id, err := s.identify(pid)
	dir := ""
	if err == nil {
		dir, err = recordDir()
	}
	path := filepath.Join(dir, recordName(s.addr))
	if err == nil {
		rec := record{PID: pid, PGID: pid, Addr: s.addr, Start: id.start, Token: token}
		data, _ := json.Marshal(rec) // ints and strings always encode
		var f *os.File
		f, err = os.CreateTemp(dir, recordName(s.addr)+".*")
		if err == nil {
			_, err = f.Write(data)
			err = errors.Join(err, f.Close())
			if err == nil {
				err = os.Rename(f.Name(), path)
			}
			if err != nil {
				_ = os.Remove(f.Name())
			}
		}
		if err == nil {
			s.own = &rec
		}
	}
	if err != nil {
		return fmt.Errorf("recording the started ios_webkit_debug_proxy in %s: %w; it was stopped, because without a record no restart could tell it from another program's iwdp", path, err)
	}
	return nil
}

// loadRecord returns the recorded iwdp of s's address: the record file,
// else the record of this Starter, else nil. The file is newer than s's
// record when another process started an iwdp later. The file is trusted
// only in the private recordDir, opened without following a symlink and
// without blocking on a FIFO, and only as a regular file of this user that
// others cannot write.
func (s *Starter) loadRecord() (*record, error) {
	dir, err := recordDir()
	if err != nil {
		return nil, fmt.Errorf("the iwdp record directory is not private: %w", err)
	}
	path := filepath.Join(dir, recordName(s.addr))
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return s.own, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the iwdp record %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err == nil {
		err = checkInfo(path, fi, false, 0o022)
	}
	var data []byte
	if err == nil {
		data, err = io.ReadAll(f)
	}
	var rec record
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the iwdp record %s: %w", path, err)
	}
	return &rec, nil
}

// problem says why rec cannot name an iwdp process group on addr, or
// returns "" when it can. A group number of 1 or less, or this process's
// own group, would make a group signal reach processes that are not iwdp;
// a record without the identity cannot tell a reused pid.
func (rec *record) problem(addr string) string {
	switch {
	case rec.Addr != addr:
		return fmt.Sprintf("it is for %s", rec.Addr)
	case rec.PGID <= 1 || rec.PID != rec.PGID:
		return fmt.Sprintf("it names no iwdp process group (pid %d, group %d)", rec.PID, rec.PGID)
	case rec.PGID == syscall.Getpgrp():
		return fmt.Sprintf("it names this program's own process group %d", rec.PGID)
	case rec.Start == "" || rec.Token == "":
		return "it holds no process identity (start time and start token)"
	}
	return ""
}

// Restart restarts ios_webkit_debug_proxy on the default listing address
// when this program started it (Starter.Restart); the restart_iwdp tool
// and 'iwdp-cli restart-iwdp' use it.
func Restart(ctx context.Context) error { return std.Restart(ctx) }

// Restart stops the iwdp on the listing address and starts a new one
// (Start), but only when this program started it (decision L02): the iwdp
// must be recorded (loadRecord), the live group leader must be the
// recorded process incarnation (identity), and its process group must own
// the listening socket that receives connections to the address, as the
// start's readiness check requires (owns, before each signal). Otherwise Restart refuses: it signals
// nothing and returns an error that names the address, the owner's pid
// when known and the manual restart command. The user or another agent
// may use that iwdp. Restart never stops processes by name or pattern.
// When the recorded iwdp does not stop, Restart returns an ErrStopFailed
// error, keeps its records and starts nothing. After a stop, only a new
// start completes the restart: an address that another process serves
// by then is an error. When nothing owns the address there is nothing to
// stop, and Restart starts iwdp.
func (s *Starter) Restart(ctx context.Context) error {
	ep, err := netip.ParseAddrPort(s.addr)
	if err != nil {
		return fmt.Errorf("restarting ios_webkit_debug_proxy on %s: the ownership check needs a numeric listing address: %w", s.addr, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockStart(ctx, s.addr)
	if err != nil {
		return err
	}
	defer unlock()
	stopped, err := s.stopOwn(ctx, ep)
	if err != nil {
		return err
	}
	return s.startLocked(ctx, stopped)
}

// stopOwn stops the recorded iwdp when it owns the listener of ep, removes
// its record and returns true; it returns the refusal otherwise (Restart),
// and the stop's error when the iwdp does not stop. It returns false and
// nil when nothing owns the listing address.
func (s *Starter) stopOwn(ctx context.Context, ep netip.AddrPort) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeBudget)
	defer cancel()
	if portFree(ctx, s.addr) == nil {
		return false, nil
	}
	rec, err := s.loadRecord()
	switch {
	case err != nil:
		return false, s.refuse(ctx, ep, err.Error())
	case rec == nil:
		return false, s.refuse(ctx, ep, "no iwdp that iwdp-mcp started is recorded for it")
	}
	if p := rec.problem(s.addr); p != "" {
		return false, s.refuse(ctx, ep, "the record "+recordName(s.addr)+" is not valid: "+p)
	}
	if err := s.owns(ctx, rec, ep); err != nil {
		return false, s.refuse(ctx, ep, err.Error())
	}
	// A failed stop keeps both records: the old iwdp may still run.
	if err := s.stopRecorded(ctx, rec, ep); err != nil {
		return false, err
	}
	s.own = nil
	// A record left behind is harmless: every use checks it against the
	// live process and the port's owner, and the next start replaces it.
	// Failing here would leave iwdp stopped without the new start.
	if dir, err := recordDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, recordName(s.addr)))
	}
	return true, nil
}

// owns returns nil when the live leader of rec's process group is the
// process incarnation that rec names (identity) and the group owns the
// listener of ep, else the reason. A process group number can be reused
// after the iwdp exits; the identity tells the reuse.
func (s *Starter) owns(ctx context.Context, rec *record, ep netip.AddrPort) error {
	id, err := s.identify(rec.PID)
	if err != nil {
		return logcause.Errorf("the recorded iwdp, pid %d, cannot be identified: %v", rec.PID, err)
	}
	if !rec.matches(id) {
		return fmt.Errorf("pid %d is not the recorded iwdp: its start time or start token differs (a reused pid)", rec.PID)
	}
	owner, err := s.listens(ctx, rec.PGID, ep)
	if err != nil {
		return logcause.Errorf("checking which process listens on %s: %v", ep, err)
	}
	if owner != listenerOwn {
		return fmt.Errorf("the recorded iwdp, pid %d, no longer owns it", rec.PID)
	}
	return nil
}

// refuse returns Restart's refusal for the listing address, owned by a
// process that this program did not start, for reason.
func (s *Starter) refuse(ctx context.Context, ep netip.AddrPort, reason string) error {
	owner, kill := "owner pid unknown", "kill <pid>"
	pids, err := listenerPIDs(ctx, ep.Port())
	switch {
	case err != nil:
		owner = fmt.Sprintf("owner pid unknown (%v)", err)
	case len(pids) > 0:
		list := make([]string, len(pids))
		for i, pid := range pids {
			list[i] = strconv.Itoa(pid)
		}
		owner = "pid " + strings.Join(list, ", ")
		kill = "kill " + strings.Join(list, " ")
	}
	return fmt.Errorf("restart refused: %s is held by %s, which is not an ios_webkit_debug_proxy that iwdp-mcp started (%s). It may be the iwdp of the user or another agent, so nothing was stopped. If it is yours, restart it by hand: %s, then run 'ios_webkit_debug_proxy --no-frontend'",
		s.addr, owner, reason, kill)
}

// ErrStopFailed marks a restart whose recorded iwdp did not stop: a signal
// was refused, the identity check before SIGKILL failed, or the process
// group did not end after SIGKILL. The records are kept and no new iwdp is
// started, so a later restart can try again.
var ErrStopFailed = errors.New("the recorded ios_webkit_debug_proxy did not stop")

// groupPoll is the interval of the checks whether a stopped process group
// is gone.
const groupPoll = 10 * time.Millisecond

// stopRecorded stops the process group of rec, an iwdp that this or
// another process of this program started and that owns ep (owns):
// SIGTERM, and after stopGrace SIGKILL, but only when owns still holds,
// since the group can exit and its number be reused meanwhile. Then it
// waits up to stopReap more until no process of the group is left. The
// leader's parent reaps it. A signal that finds no process (ESRCH) means
// that the group has exited. Every other failure, and a group that is
// still there after SIGKILL, returns an ErrStopFailed error. The check and
// the signal are separate system calls; the window between them is not
// closed.
func (s *Starter) stopRecorded(ctx context.Context, rec *record, ep netip.AddrPort) error {
	if err := s.kill(-rec.PGID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%w: stopping ios_webkit_debug_proxy (process group %d): SIGTERM: %w", ErrStopFailed, rec.PGID, err)
	}
	if groupGone(rec.PGID, stopGrace) {
		return nil
	}
	if err := s.owns(ctx, rec, ep); err != nil {
		return logcause.Errorf("%w: ios_webkit_debug_proxy (process group %d) did not exit after SIGTERM, and no SIGKILL was sent: %v", ErrStopFailed, rec.PGID, err)
	}
	if err := s.kill(-rec.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%w: ios_webkit_debug_proxy (process group %d) did not exit after SIGTERM, and SIGKILL failed: %w", ErrStopFailed, rec.PGID, err)
	}
	if !groupGone(rec.PGID, stopReap) {
		return fmt.Errorf("%w: ios_webkit_debug_proxy (process group %d) is still running %s after SIGKILL", ErrStopFailed, rec.PGID, stopReap)
	}
	return nil
}

// groupGone waits up to wait until no live process of group pgid is left.
// A zombie has exited and waits until its parent, or the process that
// inherits it, reaps it; at the end of the wait a group of zombies only
// counts as gone (groupLive). When that check fails, the group is not
// gone.
func groupGone(pgid int, wait time.Duration) bool {
	for end := time.Now().Add(wait); ; time.Sleep(groupPoll) {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(end) {
			live, err := groupLive(pgid)
			return err == nil && !live
		}
	}
}

// groupLive reports whether process group pgid has a process that is not
// a zombie (state Z), from 'ps -A -o pgid=,stat=' (Linux and darwin).
func groupLive(pgid int) (bool, error) {
	out, err := exec.Command("ps", "-A", "-o", "pgid=,stat=").Output()
	if err != nil {
		return false, fmt.Errorf("listing the processes of group %d with ps: %w", pgid, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == strconv.Itoa(pgid) && !strings.HasPrefix(f[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}

// newToken returns a random start token (128 bits or more).
func newToken() string { return rand.Text() }
