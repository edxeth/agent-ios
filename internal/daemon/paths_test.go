package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 7.3 test 11 [W1b]: a socket directory that is a symlink, owned by
// another uid or not mode 0700 makes the daemon refuse to start (design 2.2).
func TestCheckDir(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	if err := EnsureDir(good); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := CheckDir(good, os.Getuid()); err != nil {
		t.Fatalf("good dir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		dir string
		uid int
	}{
		"symlink":     {link, os.Getuid()},
		"mode 0755":   {loose, os.Getuid()},
		"other owner": {good, os.Getuid() + 1},
		"missing":     {filepath.Join(base, "missing"), os.Getuid()},
	}
	for name, c := range cases {
		if err := CheckDir(c.dir, c.uid); !errors.Is(err, ErrUnsafeDir) {
			t.Errorf("%s: CheckDir = %v, want ErrUnsafeDir", name, err)
		}
	}
	if err := EnsureDir(loose); !errors.Is(err, ErrUnsafeDir) {
		t.Errorf("EnsureDir on a 0755 directory = %v, want ErrUnsafeDir", err)
	}
	if err := EnsureDir(link); !errors.Is(err, ErrUnsafeDir) {
		t.Errorf("EnsureDir on a symlink = %v, want ErrUnsafeDir", err)
	}
}
