package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
)

// Executable identifies the executable file that a daemon runs (Hello).
// A client reuses a running daemon only when it runs the same executable
// content as the client (reusable): the same file (FileID) or, when the
// file differs, the same bytes (SHA256). After an upgrade or a rebuild, a
// daemon of the old binary, which can lack commands or stream items, is
// replaced when idle, while identical copies of one binary (the outputs of
// 'make build' and 'make install', a 'go run' build) share a daemon.
type Executable struct {
	FileID
	SHA256 string `json:"sha256"` // hex SHA-256 of the file's bytes
}

// FileID identifies a file without reading it: one stat.
type FileID struct {
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtimeNs"` // modification time, Unix nanoseconds
}

// selfExe is the path of this process's executable. On Linux,
// /proc/self/exe still names the file this process started from after
// that file was replaced or removed at its path; elsewhere os.Executable
// names the path, which then shows the replacement.
func selfExe() (string, error) {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe", nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this process's executable: %w", err)
	}
	return p, nil
}

// fileIDOf is the identity of the file that fi describes.
func fileIDOf(fi os.FileInfo) (FileID, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, fmt.Errorf("no device and inode for %s", fi.Name())
	}
	return FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}, nil
}

// statExe returns the identity of the executable at path.
func statExe(path string) (FileID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileID{}, fmt.Errorf("identifying the executable %s: %w", path, err)
	}
	return fileIDOf(fi)
}

// identify reads the identity and the SHA-256 of the executable at path
// from one open file, so both describe the same file.
func identify(path string) (Executable, error) {
	f, err := os.Open(path)
	if err != nil {
		return Executable{}, fmt.Errorf("identifying the executable %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Executable{}, fmt.Errorf("identifying the executable %s: %w", path, err)
	}
	id, err := fileIDOf(fi)
	if err != nil {
		return Executable{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Executable{}, fmt.Errorf("hashing the executable %s: %w", path, err)
	}
	return Executable{FileID: id, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// selfHash is the SHA-256 of this process's executable, computed once and
// only when a daemon's file identity differs from this process's (the
// 21 MB binary takes about 40 ms).
var selfHash = sync.OnceValues(func() (string, error) {
	p, err := selfExe()
	if err != nil {
		return "", err
	}
	e, err := identify(p)
	return e.SHA256, err
})

// runsThisExecutable reports whether a daemon that runs d runs the same
// executable content as this process: the same file (one stat), else the
// same SHA-256.
func runsThisExecutable(d Executable) (bool, error) {
	if d.SHA256 == "" {
		return false, nil // a daemon of protocol 1 or the in-process core
	}
	p, err := selfExe()
	if err != nil {
		return false, err
	}
	if id, err := statExe(p); err == nil && id == d.FileID {
		return true, nil
	}
	sum, err := selfHash()
	if err != nil {
		return false, err
	}
	return sum == d.SHA256, nil
}
