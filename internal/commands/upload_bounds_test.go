package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review finding F6 (W5b-2 closure review; design 3.5): the CLI upload
// refuses a FIFO without blocking, also when a writer holds it open with
// no data, so the command ends at its deadline. The reviewer's
// reproduction, with its assertion unchanged.
func TestCLIUploadHonorsDeadline(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	hold, err := os.OpenFile(fifo, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = runUpload(ctx, fakeEnv{}, Input{Args: []string{"@e1", fifo, filepath.Join(dir, "missing")}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		_ = hold.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("CLI upload did not unblock after releasing the FIFO")
		}
		t.Fatal("CLI upload blocked on a FIFO past its deadline; the same bounded-read requirement as CDP remains unmet")
	}
}

// uploadErr runs the CLI upload of files on @e1 with fakeEnv, which has no
// page: each case fails while reading, before a page is opened.
func uploadErr(ctx context.Context, t *testing.T, files ...string) error {
	t.Helper()
	_, err := runUpload(ctx, fakeEnv{}, Input{Args: append([]string{"@e1"}, files...)})
	if err == nil {
		t.Fatalf("upload %v succeeded, want a read error", files)
	}
	return err
}

// Review finding F6: a file over the 8 MiB limit (design 3.5) is refused
// after at most the limit plus one byte: a sparse file of 1 TiB is refused
// at once, where reading it whole would need 1 TiB of memory.
func TestCLIUploadRefusesALargeFileWithoutReadingIt(t *testing.T) {
	big := filepath.Join(t.TempDir(), "sparse.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 40); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := uploadErr(ctx, t, big); !strings.Contains(err.Error(), "upload: the files are 8388609 bytes or more; the limit is 8388608 bytes (8 MiB)") {
		t.Fatalf("an upload of a 1 TiB sparse file: %v, want the limit error", err)
	}
}

// Review finding F6: the reads stop when the command's context ends.
func TestCLIUploadStopsWhenCanceled(t *testing.T) {
	txt := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uploadErr(ctx, t, txt); !strings.Contains(err.Error(), "upload: reading "+txt+": context canceled") {
		t.Fatalf("an upload with a canceled context: %v", err)
	}
}
