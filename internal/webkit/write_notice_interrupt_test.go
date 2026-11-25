package webkit

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
)

// W3b rebase onto the committed core (docs/notes/w3b.md, "W3b rebase onto
// the committed core"): a write that the stall cut interrupts may have
// written part of its message. It keeps the core-events r6 classification
// (a write error without ErrNotSent: whether the page got it is unknown)
// and sets no write notice (WithWriteNotice: only a completed write sets
// it), so W3b's frame cleanup counts it neither as refused nor as reached
// and keeps the frame record for the next release. A call refused after
// the cut sets no notice either.
func TestInterruptedWriteSetsNoWriteNotice(t *testing.T) {
	noticed := func(send func(context.Context, string, any) (json.RawMessage, error), written *atomic.Bool) func(context.Context, string, any) (json.RawMessage, error) {
		return func(ctx context.Context, method string, params any) (json.RawMessage, error) {
			return send(WithWriteNotice(ctx, written), method, params)
		}
	}
	check := func(t *testing.T, p *blockedPeer, send func(context.Context, string, any) (json.RawMessage, error), interrupt func()) {
		t.Helper()
		large, after := new(atomic.Bool), new(atomic.Bool)
		done := p.startLargeWrite(t, noticed(send, large))
		interrupt()
		checkInterrupted(t, done)
		if large.Load() {
			t.Error("the interrupted write set its write notice; only a completed write may set it")
		}
		checkNeverWrittenAgain(t, noticed(send, after))
		if after.Load() {
			t.Error("a call refused after the interrupt set its write notice")
		}
	}
	t.Run("client", func(t *testing.T) {
		p := newBlockedPeer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := NewClientWithDialer(ctx, p.url, p.dialer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		<-p.remote
		check(t, p, c.Send, c.InterruptWrites)
	})
	t.Run("session", func(t *testing.T) {
		p := newBlockedPeer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := dialWebSocket(ctx, p.dialer, p.url, transport.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewSession(ctx, conn, SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		<-p.remote
		check(t, p, s.Send, s.InterruptWrites)
	})
}
