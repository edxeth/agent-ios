//go:build webkitgtk

package webkitgtk

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/edxeth/agent-ios/e2e/internal/broadwayshm"
)

// broadwaySettle is how long after the end of a MiniBrowser the test
// waits before it checks its buffers: broadwayd may still handle updates
// that the client queued before it died, and removes the names that it
// maps.
const broadwaySettle = 500 * time.Millisecond

// identity is a device and inode.
type identity struct{ dev, ino uint64 }

// mappedShm returns the identities of the Broadway buffers that process
// pid maps (/proc/<pid>/maps: dev as major:minor in hex, then the inode).
// The recorder creates GTK's buffers without a name and links them later,
// so such a mapping shows "/dev/shm/#<inode> (deleted)" rather than the
// buffer's name; a buffer made another way shows "/dev/shm/bdw-...".
// Other /dev/shm objects (of other libraries) are not Broadway buffers
// and are left out. Assumption: no other library of MiniBrowser creates
// unnamed objects in /dev/shm; one that did would fail this test, not
// pass it wrongly.
func mappedShm(t *testing.T, pid int) []identity {
	t.Helper()
	maps, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "maps"))
	if err != nil {
		t.Fatalf("reading the mappings of MiniBrowser %d: %v", pid, err)
	}
	var out []identity
	for _, line := range strings.Split(string(maps), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !isBroadwayMapping(f[5]) {
			continue
		}
		major, minor, ok := strings.Cut(f[3], ":")
		ma, err1 := strconv.ParseUint(major, 16, 32)
		mi, err2 := strconv.ParseUint(minor, 16, 32)
		ino, err3 := strconv.ParseUint(f[4], 10, 64)
		if !ok || err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("unexpected mapping line %q", line)
		}
		id := identity{unix.Mkdev(uint32(ma), uint32(mi)), ino}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// isBroadwayMapping reports whether a mapped path can be a Broadway
// buffer: "/dev/shm/bdw-..." or the recorder's unnamed "/dev/shm/#<inode>".
func isBroadwayMapping(path string) bool {
	if strings.HasPrefix(path, broadwayshm.Dir+"/bdw-") {
		return true
	}
	ino, ok := strings.CutPrefix(path, broadwayshm.Dir+"/#")
	_, err := strconv.ParseUint(ino, 10, 64)
	return ok && err == nil
}

// sameObject reports whether path denotes c's object; any error other
// than "does not exist" fails the test.
func sameObject(t *testing.T, c broadwayshm.Created) bool {
	t.Helper()
	fi, err := os.Lstat(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("checking the Broadway buffer %s: %v", c.Path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return uint64(st.Dev) == c.Dev && st.Ino == c.Ino
}

// A MiniBrowser that the harness ends leaves no GTK Broadway buffer in
// /dev/shm. Before the fix, every MiniBrowser killed with SIGKILL left
// the 3 MiB buffer that broadwayd never mapped (2,190 of them, 6.4 GiB,
// after two days of e2e runs). The test checks the browser's own mappings
// (never a listing of /dev/shm, which other runs on the host change at
// the same time): every /dev/shm object that the browser maps must be in
// its recorder log, and after the end no recorded name may still denote
// its recorded object. An object that another process created later under
// such a name is not this browser's and is not touched.
func TestEndedMiniBrowserLeavesNoBroadwayBuffers(t *testing.T) {
	display := broadway(t)
	addr := freeAddr(t)
	cmd, log, end := startOwned(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr}, miniBrowser, "about:blank")
	pid := cmd.Process.Pid
	// MiniBrowser creates its buffers at its start (two in the device
	// check); broadwayd maps and removes some of them.
	var held []identity
	for deadline := time.Now().Add(browserStart); len(held) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("MiniBrowser %d mapped no /dev/shm object within %s", pid, browserStart)
		}
		held = mappedShm(t, pid)
	}
	time.Sleep(time.Second) // the start's buffers, also the one broadwayd never maps
	for _, id := range mappedShm(t, pid) {
		if !slices.Contains(held, id) {
			held = append(held, id)
		}
	}
	rec, err := broadwayshm.ReadLog(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range held {
		if !slices.ContainsFunc(rec.Created, func(c broadwayshm.Created) bool { return c.Dev == id.dev && c.Ino == id.ino }) {
			t.Errorf("MiniBrowser %d maps the /dev/shm object %+v, which its recorder log %+v lacks", pid, id, rec.Created)
		}
	}
	t.Logf("MiniBrowser %d maps %+v; its log records %+v", pid, held, rec.Created)
	end()
	time.Sleep(broadwaySettle)
	for _, c := range rec.Created {
		if sameObject(t, c) {
			t.Errorf("the Broadway buffer %s of the ended MiniBrowser still exists", c.Path)
			if err := broadwayshm.RemoveIfSame(c); err != nil { // this test's own leftover
				t.Error(err)
			}
		}
	}
}
