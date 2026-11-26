package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// instantCaller answers the arm with a promise and Runtime.awaitPromise
// at once, after its write notice, as a page that settles the armed
// promise before the arming goroutine is scheduled again.
type instantCaller struct{}

func (instantCaller) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	webkit.MarkWritten(ctx)
	if method == "Runtime.awaitPromise" {
		return json.RawMessage(`{"result":{"type":"object","value":{"ok":true}}}`), nil
	}
	return json.RawMessage(`{"result":{"type":"object","subtype":"promise","objectId":"armed"}}`), nil
}
func (instantCaller) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (instantCaller) IsTargetRouted() bool                         { return true }
func (instantCaller) Done() <-chan struct{}                        { return nil }

// FIX-U race: a Runtime.awaitPromise reply implies its write (the sender
// marks the write before it waits for the reply), so an await that is
// answered before arm looks at it still arms the reply, whichever ready
// channel arm's select picks; it never fails as "answered without a write
// notice".
func TestArmAcceptsAnAwaitAnsweredBeforeItLooked(t *testing.T) {
	// Many arms at once load the scheduler, so that an arming goroutine
	// can run before arm reaches its select.
	r := New(instantCaller{}, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			for range 300 {
				a, err := r.arm(context.Background(), instantCaller{}, "wait")
				if err != nil {
					errs <- err
					return
				}
				if res := <-a.awaited; res.err != nil {
					errs <- res.err
					return
				}
				a.cancel()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
