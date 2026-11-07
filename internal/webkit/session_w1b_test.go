package webkit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// failingReadConn is a pipeConn whose next Read fails with readErr, the way
// a transport reports that the device closed the page connection.
type failingReadConn struct {
	*pipeConn
	readErr chan error
}

func (c *failingReadConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case m := <-c.in:
		return m, nil
	case err := <-c.readErr:
		return nil, err
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

// W2 handoff (docs/notes/w2.md): a read error of the transport must keep
// its identity, so a direct page connection that ends with ErrPageClosed
// reports ErrPageClosed through the Session as well as ErrSessionClosed.
func TestSessionReadErrorKeepsTransportIdentity(t *testing.T) {
	conn := &failingReadConn{pipeConn: newPipeConn(1 << 20), readErr: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := NewSession(ctx, conn, SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = s.Close() }()
	conn.readErr <- fmt.Errorf("application disconnected: %w", ErrPageClosed)
	select {
	case <-s.Done():
	case <-ctx.Done():
		t.Fatal("session did not end after the read error")
	}
	if err := s.Err(); !errors.Is(err, ErrSessionClosed) || !errors.Is(err, ErrPageClosed) {
		t.Fatalf("Err() = %v, want both ErrSessionClosed and ErrPageClosed", err)
	}
	if _, err := s.Send(ctx, "Runtime.evaluate", nil); !errors.Is(err, ErrPageClosed) {
		t.Fatalf("Send after the read error = %v, want ErrPageClosed", err)
	}
}
