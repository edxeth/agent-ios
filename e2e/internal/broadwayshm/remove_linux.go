package broadwayshm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// RemoveIfSame removes c's object, if its name still denotes it, and
// never removes another object. A name that denotes another object (or
// none) is not touched at all. Only a name that denotes c's object is
// moved to a private name (renameat2 RENAME_NOREPLACE, atomic), checked
// again there and removed. Linux removes a name only by name, so between
// the first check and the move another process could replace c's object
// under the same name: that needs c's object to go and another process to
// create the same random 28-character name within microseconds. Then the
// object goes back to its name (RENAME_NOREPLACE); when the name was taken
// meanwhile, the object stays at the private name and the error names it.
// Known limit, accepted by the user on 2026-11-12 (review finding F8): in
// that interleaving the other process's buffer is moved aside for a moment
// (an open of its name fails then, and broadwayd skips one update), or
// stays at the private name; it is never removed.
func RemoveIfSame(c Created) error {
	if same, err := denotes(c.Path, c); err != nil || !same {
		return err
	}
	if afterCheck != nil {
		afterCheck()
	}
	tmp := privateName()
	err := unix.Renameat2(unix.AT_FDCWD, c.Path, unix.AT_FDCWD, tmp, unix.RENAME_NOREPLACE)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // gone meanwhile: broadwayd or GTK removed it
	}
	if err != nil {
		return fmt.Errorf("moving %s aside to remove it: %w", c.Path, err)
	}
	if afterMove != nil {
		afterMove(tmp)
	}
	same, err := denotes(tmp, c)
	if same {
		return os.Remove(tmp)
	}
	if rerr := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, c.Path, unix.RENAME_NOREPLACE); rerr != nil {
		return fmt.Errorf("%s became another process's object; moving it back failed (%v), so it stays at %s", c.Path, rerr, tmp)
	}
	return err
}

// afterCheck and afterMove let a test act in RemoveIfSame's two windows:
// after the first check, and after the move to the private name tmp. Both
// are nil outside tests.
var (
	afterCheck func()
	afterMove  func(tmp string)
)

// denotes reports whether path names c's object (device and inode); a
// path that does not exist names none.
func denotes(path string, c Created) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == c.Dev && uint64(st.Ino) == c.Ino, nil
}
