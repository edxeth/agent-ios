package webkit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil"
)

// tokenWaitContext reports, through reached, that Client.Send evaluated
// Done in its wait for the write token; Done never fires, so only the
// token or the client's close ends the wait.
type tokenWaitContext struct {
	context.Context
	reached chan struct{}
	once    *sync.Once
}

func (c tokenWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return nil
}

// W3b final repair round 5 (round 4, acceptance criterion 3): a Client
// closed while a call waits for the write token (another call holds the
// socket) fails that call before its write: ErrNotSent with the text
// "connection closed", nothing written and no write notice.
func TestClientClosedWhileWaitingForTheSocket(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	c.writeSem <- struct{}{} // another call holds the socket
	reached := make(chan struct{})
	written := new(atomic.Bool)
	result := make(chan error, 1)
	go func() {
		_, err := c.Send(WithWriteNotice(tokenWaitContext{Context: ctx, reached: reached, once: &sync.Once{}}, written), "Runtime.enable", nil)
		result <- err
	}()
	<-reached // the call waits for the write token
	_ = c.Close()
	err = <-result
	if !errors.Is(err, ErrNotSent) || err.Error() != "connection closed" {
		t.Fatalf("send: %v; want ErrNotSent with the text \"connection closed\"", err)
	}
	if written.Load() {
		t.Fatal("the write notice is set")
	}
	if got := mock.ReceivedMethods(); len(got) != 0 {
		t.Fatalf("the page received %v, want nothing", got)
	}
}
