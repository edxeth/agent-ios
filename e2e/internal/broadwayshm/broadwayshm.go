//go:build unix

// Package broadwayshm removes the GTK Broadway buffers that a killed
// MiniBrowser leaves in /dev/shm.
//
// A GTK client under broadwayd (GDK_BACKEND=broadway) keeps each window
// buffer in a POSIX shared memory object /dev/shm/bdw-<28 random
// characters> (GTK 3.24 gdk/broadway/gdkbroadway-server.c,
// create_random_shm and map_named_shm: shm_open with O_CREAT|O_EXCL). The
// client removes the name only when GTK frees the surface
// (shm_data_destroy), and broadwayd removes it when it maps the buffer of
// an update (broadway-server.c, map_named_shm). A client that is killed
// with SIGKILL keeps every name that it created and never sent to
// broadwayd, and /dev/shm is tmpfs: each name holds 3 MiB of RAM or swap
// until a reboot.
//
// The harness ends MiniBrowser with SIGKILL, so it records each buffer as
// MiniBrowser creates it: a preloaded library (Recorder, shimSource)
// replaces shm_open for the exclusive creation of a Broadway name. It
// creates the object without a name (O_TMPFILE in Dir), appends its device,
// inode and name to a log, and only then gives it the name (linkat): a
// process killed before the link leaves no object (the kernel frees an
// unnamed one), and every named object is in the log. After every process
// that loaded the library ended, End removes each recorded object: it
// moves the name to a private name (renameat2 RENAME_NOREPLACE), compares
// the identity there, and removes only a match; another process's object
// goes back to its name. A snapshot of the process (its mappings and
// descriptors) could not do this: GTK closes the descriptor after mmap and
// calls munmap before shm_unlink, so a name in that interval is in
// neither.
//
// Only objects that the harness's own processes created are removed:
// other test runs and agents run MiniBrowser on the same host, and
// removing the buffer of a live browser breaks its rendering. Nothing here
// removes /dev/shm/bdw-* by pattern, age or owner. When the cleanup cannot
// complete, the caller keeps the log (Keep) so that the leftovers can be
// removed later by it. One interleaving can move another process's buffer
// for a moment, never remove it (RemoveIfSame: a known limit that the user
// accepted).
package broadwayshm

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Dir is where POSIX shared memory names live on Linux.
const Dir = "/dev/shm"

// namePrefix starts every Broadway buffer name that shm_open receives.
const namePrefix = "/bdw-"

// LogEnv names the log file to which the preloaded library appends.
const LogEnv = "IWDP_BROADWAY_SHM_LOG"

// DirEnv, when set, names the directory in which NewRecorder makes its own
// directory, instead of the caller's: a parent test passes it to a child
// test process whose browser outlives the child, so that the parent can
// end the recording (EndIn) after the child was killed. Only the
// recorder's own directory is ever removed (Keep), never this one.
const DirEnv = "IWDP_BROADWAY_SHM_DIR"

// PIDNSEnv holds the harness's PID namespace (the target of
// /proc/self/ns/pid). A process in another PID namespace (a sandbox such
// as WebKit's bwrap) cannot be followed through the harness's /proc: the
// library marks no such process and refuses its Broadway creations (EIO).
const PIDNSEnv = "IWDP_BROADWAY_SHM_PIDNS"

// Wait bounds End's wait for the processes that loaded the library.
const Wait = 5 * time.Second

// wakePoll is the interval of that wait.
const wakePoll = 10 * time.Millisecond

// dirPrefix names a recorder's own directory, and ownerFile marks it as
// one (EndIn touches no other directory, whatever its name); logPrefix
// and logSuffix name its log there.
const (
	dirPrefix = "bdwshm-"
	ownerFile = ".broadwayshm-recorder"
	logPrefix = "bdwshm-"
	logSuffix = ".log"
)

// shimSource is the preloaded library. A process marks itself in the log
// with "@<pid> <start time>" (field 22 of /proc/self/stat) when it loads
// the library and, after a fork, before its first creation, but only in
// the harness's PID namespace (PIDNSEnv); End waits for each marked
// process. A creation that shm_open would make with
// O_CREAT|O_EXCL of a Broadway name ("/bdw-" without another "/") and
// O_RDWR is made without a name (O_TMPFILE in /dev/shm), recorded as
// "+<dev> <ino> <name>", then linked to the name (linkat through
// /proc/self/fd; it fails with EEXIST when the name exists, as shm_open
// does). Whenever the marker or the record cannot be written, the call
// fails with EIO and leaves nothing. Other calls go to shm_open unchanged.
const shimSource = `#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

static pid_t marked_pid;

static int append(const char *line, int n) {
	const char *path = getenv("IWDP_BROADWAY_SHM_LOG");
	if (path == NULL || n <= 0) return -1;
	int fd = open(path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0600);
	if (fd < 0) return -1;
	int ok = write(fd, line, (size_t)n) == n;
	if (close(fd) != 0) ok = 0;
	return ok ? 0 : -1;
}

/* same_pidns reports whether this process is in the harness's PID
   namespace (IWDP_BROADWAY_SHM_PIDNS). */
static int same_pidns(void) {
	const char *want = getenv("IWDP_BROADWAY_SHM_PIDNS");
	char ns[128];
	ssize_t n = readlink("/proc/self/ns/pid", ns, sizeof ns - 1);
	if (want == NULL || n <= 0) return 0;
	ns[n] = 0;
	return strcmp(ns, want) == 0;
}

/* mark writes "@<pid> <start>" once per process, only in the harness's
   PID namespace, where the pid means the same process to the harness. */
static int mark(void) {
	/* The namespace first: a child forked into another PID namespace can
	   get the number that its parent had, and inherits marked_pid. */
	if (!same_pidns()) return -1;
	pid_t pid = getpid();
	if (marked_pid == pid) return 0;
	char stat[1024];
	int fd = open("/proc/self/stat", O_RDONLY | O_CLOEXEC);
	if (fd < 0) return -1;
	ssize_t n = read(fd, stat, sizeof stat - 1);
	close(fd);
	if (n <= 0) return -1;
	stat[n] = 0;
	char *p = strrchr(stat, ')');
	if (p == NULL) return -1;
	/* fields after the command: state is field 3, start time field 22 */
	char *start = NULL, *save = NULL, *tok = strtok_r(p + 1, " ", &save);
	for (int field = 3; tok != NULL; field++, tok = strtok_r(NULL, " ", &save)) {
		if (field == 22) { start = tok; break; }
	}
	if (start == NULL) return -1;
	char line[128];
	int m = snprintf(line, sizeof line, "@%ld %s\n", (long)pid, start);
	if (m <= 0 || m >= (int)sizeof line || append(line, m) != 0) return -1;
	marked_pid = pid;
	return 0;
}

__attribute__((constructor)) static void loaded(void) { mark(); }

int shm_open(const char *name, int oflag, mode_t mode) {
	static int (*real)(const char *, int, mode_t);
	if (real == NULL) real = (int (*)(const char *, int, mode_t))dlsym(RTLD_NEXT, "shm_open");
	if (real == NULL) { errno = ENOSYS; return -1; }
	if ((oflag & (O_CREAT | O_EXCL)) != (O_CREAT | O_EXCL) || strncmp(name, "/bdw-", 5) != 0 || name[5] == 0 || strchr(name + 1, '/') != NULL) {
		return real(name, oflag, mode);
	}
	if ((oflag & O_ACCMODE) != O_RDWR || mark() != 0) { errno = EIO; return -1; }
	int fd = open("/dev/shm", O_TMPFILE | O_RDWR | O_CLOEXEC, mode & 07777);
	if (fd < 0) return -1;
	struct stat st;
	char line[512], self[64], path[512];
	int n = -1;
	if (fstat(fd, &st) == 0) {
		n = snprintf(line, sizeof line, "+%llu %llu %s\n", (unsigned long long)st.st_dev, (unsigned long long)st.st_ino, name);
	}
	if (n <= 0 || n >= (int)sizeof line || append(line, n) != 0) { close(fd); errno = EIO; return -1; }
	snprintf(self, sizeof self, "/proc/self/fd/%d", fd);
	snprintf(path, sizeof path, "/dev/shm%s", name);
	if (linkat(AT_FDCWD, self, AT_FDCWD, path, AT_SYMLINK_FOLLOW) != 0) {
		int e = errno;
		close(fd);
		errno = e;
		return -1;
	}
	return fd;
}
`

// Recorder is the preloaded library and the log of one process group, in
// a directory of its own.
type Recorder struct {
	Dir string // holds Lib and Log
	Lib string // the library for LD_PRELOAD
	Log string // the log that it appends to
}

// NewRecorder makes its own new directory below parent (or below the
// directory of DirEnv), compiles the library there with gcc and chooses
// the log.
// It fails for a directory whose path the loader would split
// (LD_PRELOAD separates libraries at spaces and colons) or that holds a
// control character. Unlike a test's TempDir, the directory stays until
// the caller removes it (Keep), so that a failed cleanup keeps the log.
func NewRecorder(parent string) (*Recorder, error) {
	if d := os.Getenv(DirEnv); d != "" {
		parent = d
	}
	dir, err := os.MkdirTemp(parent, dirPrefix)
	if err != nil {
		return nil, err
	}
	if strings.ContainsFunc(dir, func(r rune) bool { return r == ' ' || r == ':' || r < ' ' }) {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("the Broadway buffer recorder cannot live in %q: LD_PRELOAD would split its path; set TMPDIR to a path without spaces and colons", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile), nil, 0o600); err != nil {
		return nil, err
	}
	src, err := os.CreateTemp(dir, logPrefix+"*.c")
	if err != nil {
		return nil, err
	}
	_, err = src.WriteString(shimSource)
	if err = errors.Join(err, src.Close()); err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(src.Name(), ".c")
	if out, err := exec.Command("gcc", "-shared", "-fPIC", "-o", base+".so", src.Name(), "-ldl").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("compiling the Broadway buffer recorder with gcc: %v: %s", err, out)
	}
	return &Recorder{Dir: dir, Lib: base + ".so", Log: base + logSuffix}, nil
}

// Keep removes dir, the recorder's directory, when err is nil; otherwise
// it keeps it and returns err with the directory's path, so that its logs
// can end the recording later.
func Keep(dir string, err error) error {
	if err != nil {
		return fmt.Errorf("%w (the recorder's logs stay in %s)", err, dir)
	}
	return os.RemoveAll(dir)
}

// Env returns env with the recorder: its library added to LD_PRELOAD
// (after a library that env already preloads), LogEnv and PIDNSEnv (this
// process's PID namespace; without it the library records nothing).
func (r *Recorder) Env(env []string) []string {
	out := make([]string, 0, len(env)+3)
	ns, _ := os.Readlink("/proc/self/ns/pid")
	preload := r.Lib
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "LD_PRELOAD="); ok {
			preload = v + " " + r.Lib
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LD_PRELOAD="+preload, LogEnv+"="+r.Log, PIDNSEnv+"="+ns)
}

// Created is one recorded object: its name and identity.
type Created struct {
	Path     string // below Dir
	Dev, Ino uint64
}

// Marked is one process that loaded the library.
type Marked struct {
	PID   int
	Start string // field 22 of /proc/<pid>/stat
}

// Record is the content of a log.
type Record struct {
	Procs   []Marked
	Created []Created
}

// maxLogLine bounds a line of the log; the library writes at most 512
// bytes.
const maxLogLine = 4096

// ReadLog reads a log. A log that does not exist, or that marks no
// process, is an error: the library did not run, and its processes'
// buffers are unknown. A line of another form, also one that is too long,
// is an error.
func ReadLog(log string) (Record, error) {
	data, err := os.ReadFile(log)
	if err != nil {
		return Record{}, fmt.Errorf("the Broadway buffer recorder did not run (LD_PRELOAD): %w", err)
	}
	var rec Record
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 512), maxLogLine)
	bad := func(line string) error {
		return fmt.Errorf("%s: %q is no record of the Broadway buffer recorder", log, line)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "@"):
			f := strings.Split(line[1:], " ")
			pid, err := strconv.Atoi(f[0])
			if len(f) != 2 || err != nil || pid <= 0 {
				return Record{}, bad(line)
			}
			if _, err := strconv.ParseUint(f[1], 10, 64); err != nil {
				return Record{}, bad(line)
			}
			rec.Procs = append(rec.Procs, Marked{PID: pid, Start: f[1]})
		case strings.HasPrefix(line, "+"):
			f := strings.SplitN(line[1:], " ", 3)
			if len(f) != 3 || !validName(f[2]) {
				return Record{}, bad(line)
			}
			dev, err1 := strconv.ParseUint(f[0], 10, 64)
			ino, err2 := strconv.ParseUint(f[1], 10, 64)
			if err1 != nil || err2 != nil {
				return Record{}, bad(line)
			}
			rec.Created = append(rec.Created, Created{Path: Dir + f[2], Dev: dev, Ino: ino})
		default:
			return Record{}, bad(line)
		}
	}
	if err := sc.Err(); err != nil {
		return Record{}, fmt.Errorf("%s: %w", log, err)
	}
	if len(rec.Procs) == 0 {
		return Record{}, fmt.Errorf("%s: no process loaded the Broadway buffer recorder (LD_PRELOAD)", log)
	}
	return rec, nil
}

// validName reports whether name is a Broadway buffer name: "/bdw-" and at
// least one more character, without another "/".
func validName(name string) bool {
	rest, ok := strings.CutPrefix(name, namePrefix)
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// End ends the recording of log. It reads the log, waits up to Wait until
// every marked process has ended (its own and its children's; call it
// after the process group was killed), and reads the log again, until a
// read finds no process that it did not wait for. Then it removes each
// recorded object whose name still denotes it (removeRecorded). When a
// process is still running after Wait, or its state cannot be read, End
// removes nothing and returns an error.
func End(log string) error {
	deadline := time.Now().Add(Wait)
	waited := map[Marked]bool{}
	for {
		rec, err := ReadLog(log)
		if err != nil {
			return err
		}
		fresh := false
		for _, p := range rec.Procs {
			if !waited[p] {
				fresh = true
			}
		}
		if !fresh {
			return removeRecorded(rec.Created)
		}
		if err := awaitEnd(log, rec.Procs, deadline); err != nil {
			return err
		}
		for _, p := range rec.Procs {
			waited[p] = true
		}
	}
}

// EndIn ends every recording in the recorders' own directories in dir
// (DirEnv): the subdirectories that NewRecorder made, which hold its owner
// file. It removes each of those whose recording ended (Keep); a dir that
// holds no log is an error. dir itself and every other entry stay.
func EndIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	found := false
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), dirPrefix) {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(filepath.Join(sub, ownerFile)); errors.Is(err, fs.ErrNotExist) {
			continue // not a recorder's directory: never touched
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		logs, err := os.ReadDir(sub)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var subErrs []error
		for _, l := range logs {
			if name := l.Name(); strings.HasPrefix(name, logPrefix) && strings.HasSuffix(name, logSuffix) {
				found = true
				subErrs = append(subErrs, End(filepath.Join(sub, name)))
			}
		}
		errs = append(errs, Keep(sub, errors.Join(subErrs...)))
	}
	if !found {
		errs = append(errs, fmt.Errorf("%s holds no log of the Broadway buffer recorder", dir))
	}
	return errors.Join(errs...)
}

// awaitEnd waits until deadline until no process of procs runs.
func awaitEnd(log string, procs []Marked, deadline time.Time) error {
	for {
		var running []string
		for _, p := range procs {
			if alive, err := runs(p); alive || err != nil {
				running = append(running, fmt.Sprintf("%d (%v)", p.PID, err))
			}
		}
		if len(running) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			slices.Sort(running)
			return fmt.Errorf("processes %v that record Broadway buffers in %s still run, or their state cannot be read, after %s; nothing was removed", running, log, Wait)
		}
		time.Sleep(wakePoll)
	}
}

// procFS is where processes are read; tests use a fixture.
var procFS = "/proc"

// runs reports whether the marked process still runs: its /proc entry
// exists with the marked start time and it is no zombie. A gone entry, or
// one with another start time (a reused pid), has ended. Any other read
// failure is returned: it is no proof of an end.
func runs(p Marked) (bool, error) {
	stat, err := os.ReadFile(filepath.Join(procFS, strconv.Itoa(p.PID), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return false, fmt.Errorf("%s/%d/stat: no command name", procFS, p.PID)
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state), f[19] field 22
	if len(f) < 20 {
		return false, fmt.Errorf("%s/%d/stat: %d fields after the command name", procFS, p.PID, len(f))
	}
	return f[19] == p.Start && f[0] != "Z", nil
}

// removeRecorded removes each recorded object whose name still denotes
// it, through RemoveIfSame. A recorded name that is gone, or that denotes
// another object, is left as it is.
func removeRecorded(created []Created) error {
	var errs []error
	done := map[Created]bool{}
	for _, c := range created {
		if done[c] {
			continue
		}
		done[c] = true
		errs = append(errs, RemoveIfSame(c))
	}
	return errors.Join(errs...)
}

// privateName returns a fresh name in Dir that only this process uses.
func privateName() string { return Dir + "/bdw-iwdp-end-" + strings.ToLower(rand.Text()) }
