// Package daemon is the long-lived iwdp daemon and its client (design
// docs/design/agentic-parity.md, sections 2.1 to 2.3): HTTP/1.1 with NDJSON
// streams on a Unix socket in a private directory, auto-start, idle exit
// and cleanup on shutdown. The daemon is the same binary as the CLI and the
// MCP server, started as "<own executable> daemon run".
package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SocketDir is $XDG_RUNTIME_DIR/iwdp, else $TMPDIR/iwdp-$UID (design 2.2).
func SocketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "iwdp")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("iwdp-%d", os.Getuid()))
}

// SocketPath is the daemon socket in dir.
func SocketPath(dir string) string { return filepath.Join(dir, "daemon.sock") }

// ReservationsDir is the device reservation directory in dir (design 2.5).
func ReservationsDir(dir string) string { return filepath.Join(dir, "reservations") }

// LogPath is $XDG_STATE_HOME/iwdp/daemon.log, else
// ~/.local/state/iwdp/daemon.log (design 2.3).
func LogPath() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "iwdp", "daemon.log"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon log: no XDG_STATE_HOME and no home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "iwdp", "daemon.log"), nil
}

// ErrUnsafeDir: the socket directory fails the lstat check of design 2.2.
var ErrUnsafeDir = errors.New("unsafe daemon socket directory")

// ErrUnsafePeer: the process on the daemon socket is not this user's.
var ErrUnsafePeer = errors.New("untrusted daemon socket peer")

// CheckDir checks dir with lstat (design 2.2, R16): it must be a directory
// and not a symlink, be owned by uid and have mode exactly 0700.
func CheckDir(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w %s: %w", ErrUnsafeDir, dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w %s: it is a symlink; remove it", ErrUnsafeDir, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w %s: not a directory", ErrUnsafeDir, dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w %s: cannot read its owner", ErrUnsafeDir, dir)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%w %s: owned by uid %d, not %d", ErrUnsafeDir, dir, st.Uid, uid)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w %s: mode %v, want 0700 (chmod 700 %s)", ErrUnsafeDir, dir, fi.Mode(), dir)
	}
	return nil
}

// EnsureDir creates dir with mode 0700 when it is missing, then checks it.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return CheckDir(dir, os.Getuid())
}
