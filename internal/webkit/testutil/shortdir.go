package testutil

import (
	"os"
	"testing"
)

// shortDirBase is where ShortTempDir makes its directories: /tmp, whose
// path is short on every supported system. $TMPDIR is not: on macOS it is
// a directory below /var/folders (49 bytes on GitHub's macOS runner), and a
// test's t.TempDir() below it adds the test's name.
const shortDirBase = "/tmp"

// ShortTempDir makes a new private directory (mode 0700) directly under
// /tmp, with a short name, for a daemon's runtime directory
// (XDG_RUNTIME_DIR) in tests. The daemon's socket, <dir>/agent-ios/daemon.sock,
// must fit a Unix socket path, at most 103 bytes on macOS and 107 on Linux
// (daemon.checkSocketPath), which a directory below $TMPDIR or t.TempDir()
// can pass. The caller removes the directory.
func ShortTempDir() (string, error) {
	return os.MkdirTemp(shortDirBase, "aios")
}

// ShortTestDir is ShortTempDir for one test; the test's cleanup removes
// the directory. Every test that runs a daemon in a runtime directory of
// its own takes that directory from here.
func ShortTestDir(t testing.TB) string {
	t.Helper()
	dir, err := ShortTempDir()
	if err != nil {
		t.Fatalf("a short runtime directory under %s: %v", shortDirBase, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing the runtime directory %s: %v", dir, err)
		}
	})
	return dir
}
