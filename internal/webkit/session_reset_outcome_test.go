package webkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W1b integration (docs/notes/w1b.md, "Review repairs, integration"): a
// reset call of a first enable (sendWithReset) that ends without WebKit's
// answer keeps that outcome in the enable's error. core's hang detection
// probes the page after ErrCallTimeout, and the owner's cancellation of a
// call is no stall (design 2.6 step 2). A protocol refusal stays reported
// without its protocol error type, and ErrNotSent is claimed only when
// nothing of the batch reached the page.
func TestAgentResetKeepsAnOutcomeWithoutAnswer(t *testing.T) {
	t.Run("no reply within the call budget", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		f.Stall("Memory.disable")
		s := newFakeSession(t, f, SessionOptions{CallTimeout: 200 * time.Millisecond})
		_, err := s.Send(context.Background(), "Memory.enable", nil)
		var pe *ErrorData
		if !errors.Is(err, ErrCallTimeout) || errors.As(err, &pe) || errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "resetting the Memory agent") {
			t.Fatalf("Memory.enable after a reset without reply: %v, want the reset's ErrCallTimeout, not a protocol error or ErrNotSent", err)
		}
	})
	t.Run("the caller cancels", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		f.Stall("Memory.disable")
		s := newFakeSession(t, f, SessionOptions{CallTimeout: 5 * time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && len(f.ReceivedMethod("Memory.enable")) == 0; {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		_, err := s.Send(ctx, "Memory.enable", nil)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCallTimeout) {
			t.Fatalf("Memory.enable canceled during its reset: %v, want context.Canceled", err)
		}
	})
	t.Run("a refusal and then no reply", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		f.Handle("Debugger.setPauseOnExceptions", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "pause state refused"}
		})
		f.Stall("Debugger.disable")
		s := newFakeSession(t, f, SessionOptions{AgentReset: testAgentReset, CallTimeout: 200 * time.Millisecond})
		_, err := s.Send(context.Background(), "Debugger.enable", nil)
		var pe *ErrorData
		if !errors.Is(err, ErrCallTimeout) || errors.As(err, &pe) || !strings.Contains(err.Error(), "pause state refused") {
			t.Fatalf("Debugger.enable: %v, want ErrCallTimeout with the refusal in the message and no protocol error type", err)
		}
	})
	t.Run("a later reset call is not written", func(t *testing.T) {
		// The first reset call reaches the page; the second is over the
		// outgoing wire limit and is not written. Something reached the
		// page, so the enable's error is no ErrNotSent.
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		huge := map[string]string{"source": strings.Repeat("x", 4096)}
		s := newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
			return DialWebSocket(ctx, f.URL, transport.Limits{Routed: true, MaxOutgoing: 2048})
		}, SessionOptions{AgentReset: func(dom string) []Call { return []Call{{Method: dom + ".disable"}, {"Page.setBootstrapScript", huge}} }})
		_, err := s.Send(context.Background(), "Page.enable", nil)
		if err == nil || errors.Is(err, ErrNotSent) || len(f.ReceivedMethod("Page.disable")) != 1 || len(f.ReceivedMethod("Page.enable")) != 0 {
			t.Fatalf("Page.enable after a partly written reset: %v, want a failure that is not ErrNotSent, with the disable on the wire and no enable", err)
		}
	})
}
