// Package upload reads the host files of an upload (design 3.5): the CLI
// upload command (internal/commands) and CDP DOM.setFileInputFiles
// (internal/cdp) share this bounded, cancellable reader.
package upload

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
)

// MaxBytes is the total size limit of the files of one upload (design
// 3.5: 8 MiB on iwdp and direct).
const MaxBytes = 8 << 20

// chunk is the read size between Read's cancellation checks.
const chunk = 64 << 10

// Read reads the regular file path for an upload with left bytes of the
// limit left. It reads at most left+1 bytes, so the caller sees that a
// file over the limit is too large without reading it whole, and it stops
// when ctx ends. The file is opened without blocking (O_NONBLOCK;
// O_NOCTTY, so a terminal never becomes the process's), and anything but a
// regular file (a FIFO, a device, a directory) is refused before a read,
// because a read from it can block past the command's deadline.
func Read(ctx context.Context, path string, left int) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", fi.Mode().Type())
	}
	r := io.LimitReader(f, int64(left)+1)
	var data []byte
	buf := make([]byte, chunk)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(buf)
		data = append(data, buf[:n]...)
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
