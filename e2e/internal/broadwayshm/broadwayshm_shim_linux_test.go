//go:build webkitgtk

package broadwayshm

import (
	"bufio"
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
)

// newName returns a fresh Broadway-like name (for shm_open) that no GTK
// client uses.
func newName() string { return namePrefix + "iwdptest" + strings.ToLower(rand.Text())[:12] }

// identity returns the device and inode of path.
func identity(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), uint64(st.Ino)
}

// fileCase is one generated object: whether it exists at the end, and how
// the log records it: 0 not, 1 with its identity, 2 with another inode, 3
// twice with its identity.
type fileCase struct {
	Exists bool
	Record int
}

type removalCase []fileCase

func (removalCase) Generate(r *mrand.Rand, _ int) reflect.Value {
	c := make(removalCase, 1+r.Intn(6))
	for i := range c {
		c[i] = fileCase{Exists: r.Intn(4) > 0, Record: r.Intn(4)}
	}
	return reflect.ValueOf(c)
}

// Property: End removes exactly the objects that exist and that the log
// records with their device and inode; every other object, also one whose
// name the log records with another identity, stays at its name. The
// expectation comes from the case alone, the result from the filesystem.
func TestEndRemovesExactlyTheRecordedObjects(t *testing.T) {
	dir := t.TempDir()
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	prop := func(c removalCase) bool {
		var b strings.Builder
		fmt.Fprintf(&b, "@%d 1\n", gone.Process.Pid)
		paths := make([]string, len(c))
		for i, fc := range c {
			paths[i] = Dir + newName()
			if err := os.WriteFile(paths[i], nil, 0o600); err != nil {
				t.Fatal(err)
			}
			dev, ino := identity(t, paths[i])
			name := strings.TrimPrefix(paths[i], Dir)
			switch fc.Record {
			case 1:
				fmt.Fprintf(&b, "+%d %d %s\n", dev, ino, name)
			case 2:
				fmt.Fprintf(&b, "+%d %d %s\n", dev, ino+1, name)
			case 3:
				fmt.Fprintf(&b, "+%d %d %s\n+%d %d %s\n", dev, ino, name, dev, ino, name)
			}
			if !fc.Exists {
				_ = os.Remove(paths[i])
			}
		}
		defer func() {
			for _, p := range paths {
				_ = os.Remove(p)
			}
		}()
		log := filepath.Join(dir, "case.log")
		if err := os.WriteFile(log, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := End(log); err != nil {
			t.Logf("End: %v", err)
			return false
		}
		for i, fc := range c {
			_, err := os.Lstat(paths[i])
			removed := os.IsNotExist(err)
			if err != nil && !removed {
				t.Logf("Lstat: %v", err)
				return false
			}
			if removed != (!fc.Exists || fc.Record == 1 || fc.Record == 3) {
				t.Logf("case %+v: object %d removed %v", c, i, removed)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 60}); err != nil {
		t.Fatal(err)
	}
}

// Tests of the preloaded library and of the removal. They compile C with
// gcc and create files in /dev/shm, which the WebKitGTK suite needs anyway
// (its DPI shim, its browsers' buffers), so they carry its build tag:
// go test -tags=webkitgtk ./e2e/internal/broadwayshm/.

// clientSource is a GTK-like client of shm_open, compiled for the tests.
// For each argument "<op>:<name>" it calls shm_open and prints
// "<name> <errno>" (0: success). Ops: "map" creates the name as GTK's
// map_named_shm does (O_RDWR|O_CREAT|O_EXCL, ftruncate, mmap, close),
// "fd" creates it and keeps the descriptor, "window" creates and maps it,
// closes the descriptor and unmaps it without shm_unlink (GTK's
// shm_data_destroy stopped between munmap and shm_unlink), and "open"
// opens it with O_RDWR|O_CREAT and no O_EXCL (which can open another
// process's object), and "fork" forks a child that creates the name as
// "map" does, prints its result and waits. Then it prints "ready" and
// waits to be killed.
const clientSource = `#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

int main(int argc, char **argv) {
	for (int i = 1; i < argc; i++) {
		char *sep = strchr(argv[i], ':');
		*sep = 0;
		const char *op = argv[i], *name = sep + 1;
		if (strcmp(op, "fork") == 0) {
			fflush(stdout);
			if (fork() != 0) continue;
			op = "map";
			int fd = shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600);
			printf("%s %d\n", name, fd < 0 ? errno : 0);
			fflush(stdout);
			pause();
			return 0;
		}
		int flags = O_RDWR | O_CREAT | (strcmp(op, "open") == 0 ? 0 : O_EXCL);
		int fd = shm_open(name, flags, 0600);
		printf("%s %d\n", name, fd < 0 ? errno : 0);
		if (fd < 0 || strcmp(op, "open") == 0) continue;
		if (ftruncate(fd, 4096) != 0) return 3;
		if (strcmp(op, "fd") == 0) continue;
		void *p = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
		if (p == MAP_FAILED) return 3;
		close(fd);
		if (strcmp(op, "window") == 0) munmap(p, 4096);
	}
	printf("ready\n");
	fflush(stdout);
	pause();
	return 0;
}
`

// buildClient compiles clientSource into dir.
func buildClient(t *testing.T, dir string) string {
	t.Helper()
	src, bin := filepath.Join(dir, "client.c"), filepath.Join(dir, "client")
	if err := os.WriteFile(src, []byte(clientSource), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("gcc", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("compiling the test client: %v: %s", err, out)
	}
	return bin
}

// runClient starts the client with env in its own process group and
// returns, when it is ready, its results ("<name> <errno>") in output
// order; a forked child's result can come after "ready", so lines up to
// want are read.
func runClient(t *testing.T, bin string, env []string, want int, args ...string) (*exec.Cmd, []string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	var results []string
	sc := bufio.NewScanner(out)
	for len(results) < want && sc.Scan() {
		if sc.Text() != "ready" {
			results = append(results, sc.Text())
		}
	}
	return cmd, results
}

// kill ends the client's group as the harness does.
func kill(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
}

// The library records every exclusive creation, also of a buffer held by a
// descriptor only and of one in GTK's munmap-to-shm_unlink interval, which
// no snapshot of the process shows; End removes exactly those. Another
// process's object stays: one that the client failed to create (EEXIST),
// and one that it opened without O_EXCL. A name that the client created
// and then failed to create again (EEXIST) is still removed, and so is the
// buffer of a forked child, whose process End waited for.
func TestRecorderRemovesEveryCreatedBuffer(t *testing.T) {
	dir := t.TempDir()
	bin := buildClient(t, dir)
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	mapped, held, window, foreign, opened, forked := newName(), newName(), newName(), newName(), newName(), newName()
	for _, n := range []string{foreign, opened} { // other processes' buffers
		if err := os.WriteFile(Dir+n, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{mapped, held, window, foreign, opened, forked} {
			_ = os.Remove(Dir + n)
		}
	})
	cmd, results := runClient(t, bin, rec.Env(os.Environ()), 7, "map:"+mapped, "fd:"+held, "window:"+window, "map:"+foreign, "open:"+opened, "map:"+mapped, "fork:"+forked)
	eexist := fmt.Sprint(int(syscall.EEXIST))
	want := []string{mapped + " 0", held + " 0", window + " 0", foreign + " " + eexist, opened + " 0", mapped + " " + eexist, forked + " 0"}
	slices.Sort(results)
	slices.Sort(want)
	if strings.Join(results, ",") != strings.Join(want, ",") {
		t.Fatalf("client results %q, want %q", results, want)
	}
	if rec2, err := ReadLog(rec.Log); err != nil || len(rec2.Procs) != 2 {
		t.Fatalf("log processes %+v, %v; want the client and its forked child", rec2.Procs, err)
	}
	kill(cmd)
	// Another process's name is never touched, also not moved aside and
	// back: a move would change its status change time.
	before := map[string]syscall.Timespec{}
	for _, n := range []string{foreign, opened} {
		before[n] = ctime(t, Dir+n)
	}
	if err := End(rec.Log); err != nil {
		t.Fatalf("End: %v", err)
	}
	for n, c := range before {
		if got := ctime(t, Dir+n); got != c {
			t.Errorf("another process's name %s was touched: ctime %v, was %v", n, got, c)
		}
	}
	for _, n := range []string{mapped, held, window, forked} {
		if _, err := os.Lstat(Dir + n); !os.IsNotExist(err) {
			t.Errorf("the client's buffer %s exists after End (%v)", n, err)
		}
	}
	for _, n := range []string{foreign, opened} {
		if _, err := os.Lstat(Dir + n); err != nil {
			t.Errorf("another process's buffer %s was removed: %v", n, err)
		}
	}
}

// A process that cannot write its log creates no Broadway buffer: the
// library removes the name it just created and fails with EIO. The marker
// cannot be written either, so End reports that the recorder did not run.
func TestRecorderLeavesNothingItCannotLog(t *testing.T) {
	dir := t.TempDir()
	bin := buildClient(t, dir)
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Log = dir // a directory: the library cannot open it for appending
	name := newName()
	t.Cleanup(func() { _ = os.Remove(Dir + name) })
	cmd, results := runClient(t, bin, rec.Env(os.Environ()), 1, "map:"+name)
	if len(results) != 1 || results[0] != name+" "+fmt.Sprint(int(syscall.EIO)) {
		t.Fatalf("creation without a writable log: %q, want EIO", results)
	}
	if _, err := os.Lstat(Dir + name); !os.IsNotExist(err) {
		t.Fatalf("the name exists although the log could not record it (%v)", err)
	}
	kill(cmd)
	if err := End(rec.Log); err == nil {
		t.Fatal("End of a log that the library could not write: no error")
	}
}

// NewRecorder with DirEnv makes its own directory in that directory; EndIn
// finds the log there and removes only the recorder's own directory, never
// the given one or anything else in it.
func TestRecorderInDirEnv(t *testing.T) {
	shared := t.TempDir()
	unrelated := filepath.Join(shared, "unrelated")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := buildClient(t, t.TempDir())
	t.Setenv(DirEnv, shared)
	rec, err := NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(rec.Dir) != shared || filepath.Dir(rec.Log) != rec.Dir || filepath.Dir(rec.Lib) != rec.Dir {
		t.Fatalf("NewRecorder with %s = %+v, want its own directory in %s", DirEnv, rec, shared)
	}
	name := newName()
	t.Cleanup(func() { _ = os.Remove(Dir + name) })
	cmd, _ := runClient(t, bin, rec.Env(os.Environ()), 1, "map:"+name)
	kill(cmd)
	if err := EndIn(shared); err != nil {
		t.Fatalf("EndIn: %v", err)
	}
	if _, err := os.Lstat(Dir + name); !os.IsNotExist(err) {
		t.Fatalf("EndIn left %s (%v)", name, err)
	}
	if _, err := os.Stat(rec.Dir); !os.IsNotExist(err) {
		t.Fatalf("EndIn left the recorder's directory (%v)", err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "keep" {
		t.Fatalf("the unrelated file in %s = %q, %v; want it untouched", shared, data, err)
	}
	// A successful End and Keep of a recorder never removes the given
	// directory either.
	rec2, err := NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Keep(rec2.Dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("Keep of a recorder removed the given directory's contents: %v", err)
	}
}

// A process in another PID namespace (a sandbox) cannot be followed by the
// harness: it writes no marker, and its Broadway creations fail with EIO
// and leave nothing.
func TestRecorderRefusesAnotherPIDNamespace(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("bwrap (WebKitGTK's sandbox, needed by this suite): %v", err)
	}
	dir := t.TempDir()
	bin := buildClient(t, dir)
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	name := newName()
	t.Cleanup(func() { _ = os.Remove(Dir + name) })
	sandboxed := []string{bwrap, "--unshare-pid", "--die-with-parent", "--dev-bind", "/", "/", "--proc", "/proc", "--", bin}
	cmd, results := runClient(t, sandboxed[0], rec.Env(os.Environ()), 1, append(sandboxed[1:], "map:"+name)...)
	if len(results) != 1 || results[0] != name+" "+fmt.Sprint(int(syscall.EIO)) {
		t.Fatalf("creation in another PID namespace: %q, want EIO", results)
	}
	if _, err := os.Lstat(Dir + name); !os.IsNotExist(err) {
		t.Fatalf("a sandboxed process created %s (%v)", name, err)
	}
	kill(cmd)
	// bwrap itself loads the library before it unshares the namespace and
	// may mark itself; nothing inside the sandbox may.
	rec2, err := ReadLog(rec.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rec2.Procs {
		if p.PID != cmd.Process.Pid {
			t.Fatalf("a sandboxed process marked itself: %+v (bwrap is %d)", p, cmd.Process.Pid)
		}
	}
}

// TestRemoveIfSameRestoresAReplacement covers the accepted known limit of
// RemoveIfSame (review finding F8): another process replaces the recorded
// object under the same name between the first check and the move. The
// replacement is moved back to its name, or, when the name was taken again
// meanwhile, stays at the private name with an error that names it; it is
// never removed. Objects are compared by device and inode, and each stays
// open until the end, so that its inode cannot be reused by a copy.
func TestRemoveIfSameRestoresAReplacement(t *testing.T) {
	for _, retaken := range []bool{false, true} {
		t.Run(fmt.Sprintf("retaken=%v", retaken), func(t *testing.T) {
			type object struct{ dev, ino uint64 }
			// create makes a new object at p, keeps it open until the test
			// ends and returns its identity.
			create := func(p string) object {
				f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
				if err != nil {
					t.Error(err)
					return object{}
				}
				t.Cleanup(func() { _ = f.Close() })
				fi, err := f.Stat()
				if err != nil {
					t.Error(err)
					return object{}
				}
				st := fi.Sys().(*syscall.Stat_t)
				return object{uint64(st.Dev), uint64(st.Ino)}
			}
			// at returns the identity of the object that p names.
			at := func(p string) (object, error) {
				fi, err := os.Lstat(p)
				if err != nil {
					return object{}, err
				}
				st := fi.Sys().(*syscall.Stat_t)
				return object{uint64(st.Dev), uint64(st.Ino)}, nil
			}

			path := Dir + newName()
			t.Cleanup(func() { _ = os.Remove(path) })
			recorded := create(path)
			c := Created{Path: path, Dev: recorded.dev, Ino: recorded.ino}
			var foreign, third object
			var tmp string
			afterCheck = func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				foreign = create(path)
			}
			afterMove = func(p string) {
				tmp = p
				t.Cleanup(func() { _ = os.Remove(tmp) })
				if retaken {
					third = create(path)
				}
			}
			t.Cleanup(func() { afterCheck, afterMove = nil, nil })

			err := RemoveIfSame(c)

			if tmp == "" {
				t.Fatalf("RemoveIfSame did not move %s aside (err %v)", path, err)
			}
			if foreign == recorded || (retaken && (third == recorded || third == foreign)) {
				t.Fatalf("the objects are not distinct: recorded %v, foreign %v, third %v", recorded, foreign, third)
			}
			got, perr := at(path)
			if !retaken {
				if err != nil || perr != nil || got != foreign {
					t.Fatalf("RemoveIfSame = %v; %s names %v (%v), want nil and the foreign object %v back", err, path, got, perr, foreign)
				}
				if _, serr := os.Lstat(tmp); !os.IsNotExist(serr) {
					t.Fatalf("%s left behind: %v", tmp, serr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tmp) {
				t.Fatalf("RemoveIfSame = %v, want an error that names %s", err, tmp)
			}
			kept, kerr := at(tmp)
			if kerr != nil || kept != foreign || perr != nil || got != third {
				t.Fatalf("%s names %v (%v) and %s %v (%v), want the foreign object %v kept at the private name and the third object %v untouched", tmp, kept, kerr, path, got, perr, foreign, third)
			}
		})
	}
}

// ctime returns the status change time of path.
func ctime(t *testing.T, path string) syscall.Timespec {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ctim
}
