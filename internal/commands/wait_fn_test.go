package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// evalCaller answers Runtime.evaluate with answer(n), n counting calls
// from 1; a nil answer blocks until the call's context ends, as a page
// that does not answer in time.
type evalCaller struct {
	calls  atomic.Int32
	answer func(n int32) json.RawMessage
}

func (c *evalCaller) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	if raw := c.answer(c.calls.Add(1)); raw != nil {
		return raw, nil
	}
	<-ctx.Done()
	return nil, fmt.Errorf("%s: no response: %w", method, ctx.Err())
}
func (*evalCaller) OnEvent(string, webkit.EventHandler)          {}
func (*evalCaller) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (*evalCaller) IsTargetRouted() bool                         { return false }
func (*evalCaller) Done() <-chan struct{}                        { return nil }
func falsy(int32) json.RawMessage {
	return json.RawMessage(`{"result":{"type":"boolean","value":false}}`)
}

func waitFnWithin(c webkit.Caller, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return (&w3bPage{caller: c}).waitFn(ctx, "window.never === 1")
}

// Design 3.6: a wait that times out reports exit 3 with the last state it
// observed. A poll that the end of the wait cuts short observed nothing,
// so it does not replace that state: the page answered "falsy", and the
// next poll started too close to the end for any answer (seen once in
// TestWaitFormsOnWebKitGTK: "Runtime.evaluate: no response after 1ms").
// A page that does not answer is still reported when nothing else was
// observed, or when a poll's full budget passed without an answer.
func TestWaitFnKeepsTheLastObservationWhenTheEndCutsAPollShort(t *testing.T) {
	// 400 ms deadline: the wait ends at 250 ms (150 ms reserve). The
	// first poll answers falsy; the second, at about 100 ms, gets no
	// answer before the wait ends.
	err := waitFnWithin(&evalCaller{answer: func(n int32) json.RawMessage {
		if n == 1 {
			return falsy(n)
		}
		return nil
	}}, 400*time.Millisecond)
	if ExitCodeOf(err) != ExitWaitTimeout || err == nil || !strings.Contains(err.Error(), "the expression is falsy") {
		t.Fatalf("falsy, then a poll cut short by the end: %v; want exit %d with the falsy state", err, ExitWaitTimeout)
	}

	err = waitFnWithin(&evalCaller{answer: func(int32) json.RawMessage { return nil }}, 400*time.Millisecond)
	if ExitCodeOf(err) != ExitWaitTimeout || err == nil || !strings.Contains(err.Error(), "the page did not answer") {
		t.Fatalf("no answer at all: %v; want exit %d saying that the page did not answer", err, ExitWaitTimeout)
	}

	// 1.5 s wait (1.65 s deadline): the first poll answers falsy, the
	// second waits its full 1 s budget without an answer. That is an
	// observation, and it is newer than the falsy one.
	err = waitFnWithin(&evalCaller{answer: func(n int32) json.RawMessage {
		if n == 1 {
			return falsy(n)
		}
		return nil
	}}, 1650*time.Millisecond)
	if ExitCodeOf(err) != ExitWaitTimeout || err == nil || !strings.Contains(err.Error(), "the page did not answer") {
		t.Fatalf("falsy, then a full poll without an answer: %v; want exit %d saying that the page did not answer", err, ExitWaitTimeout)
	}
}
