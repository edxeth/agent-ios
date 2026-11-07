package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Design 2.2 (R16): a connection whose peer is not this user's is refused.
// A test cannot run a process of another uid, so the check runs against a
// uid that is not the peer's.
func TestVerifyPeer(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		if c, err := l.Accept(); err == nil {
			defer func() { _ = c.Close() }()
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := verifyPeer(conn, sock, os.Getuid()); err != nil {
		t.Fatalf("verifyPeer with this uid: %v", err)
	}
	if err := verifyPeer(conn, sock, os.Getuid()+1); !errors.Is(err, ErrUnsafePeer) {
		t.Fatalf("verifyPeer with another uid: %v, want ErrUnsafePeer", err)
	}
}
