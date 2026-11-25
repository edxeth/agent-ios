package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// longSocketDir returns a private directory whose daemon socket path is
// one byte over the Unix socket limit of this system.
func longSocketDir(t *testing.T) string {
	t.Helper()
	base := shortDir(t)
	pad := maxSocketPath + 1 - len(SocketPath(filepath.Join(base, "x")))
	if pad < 1 {
		t.Fatalf("the temporary directory %s is already too long", base)
	}
	return filepath.Join(base, "x"+strings.Repeat("d", pad))
}

// A socket directory whose daemon.sock would pass the Unix socket limit
// fails with an error that names the path and the limit, in the daemon
// (Run) and in the client (Connect), instead of an EINVAL from bind or
// connect.
func TestSocketPathOverTheLimitNamesPathAndLimit(t *testing.T) {
	dir := longSocketDir(t)
	sock := SocketPath(dir)
	check := func(what string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, ErrSocketPathTooLong) || !strings.Contains(err.Error(), sock) || !strings.Contains(err.Error(), strconv.Itoa(maxSocketPath)) {
			t.Fatalf("%s with a %d-byte socket path: %v; want ErrSocketPathTooLong naming %s and the limit %d", what, len(sock), err, sock, maxSocketPath)
		}
	}
	check("Run", Run(ctxFor(t, 5*time.Second), RunOptions{Dir: dir, Version: "test", Executable: os.Args[0]}))
	_, err := Connect(ctxFor(t, 5*time.Second), Options{Dir: dir, Exe: "/bin/false", Version: "test"})
	check("Connect", err)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused socket directory %s was created: %v", dir, err)
	}
}

// A socket path of exactly the limit is accepted.
func TestSocketPathAtTheLimitIsAccepted(t *testing.T) {
	sock := strings.Repeat("s", maxSocketPath)
	if err := checkSocketPath(sock); err != nil {
		t.Fatalf("a %d-byte socket path: %v", len(sock), err)
	}
}
