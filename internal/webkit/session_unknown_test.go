package webkit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// agentFake makes f answer enables and disables like WebKit agents whose
// enable is refused while enabled (InspectorPageAgent, InspectorDebuggerAgent:
// "<Domain> domain already enabled"). on lists the agents that start
// enabled: a backend that an earlier connection left enabled, for example
// a page restored from the back/forward cache after that connection closed.
func agentFake(f *fakewebkit.Server, on ...string) {
	var mu sync.Mutex
	enabled := map[string]bool{}
	for _, d := range on {
		enabled[d] = true
	}
	for _, d := range []string{"Page", "Debugger", "Timeline"} {
		f.Handle(d+".enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			mu.Lock()
			defer mu.Unlock()
			if enabled[d] {
				return nil, &fakewebkit.Error{Code: -32000, Message: d + " domain already enabled"}
			}
			enabled[d] = true
			return struct{}{}, nil
		})
		f.Handle(d+".disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			mu.Lock()
			defer mu.Unlock()
			if d == "Timeline" && !enabled[d] {
				return nil, &fakewebkit.Error{Code: -32000, Message: "Timeline domain already disabled"}
			}
			enabled[d] = false
			return struct{}{}, nil
		})
	}
}

// targetMethods returns the methods target received, in order.
func targetMethods(f *fakewebkit.Server, target string) []string {
	var out []string
	for _, r := range f.Received() {
		if r.Target == target {
			out = append(out, r.Method)
		}
	}
	return out
}

func testAgentReset(dom string) []Call {
	switch dom {
	case "Debugger":
		return []Call{{"Debugger.setPauseOnExceptions", map[string]string{"state": "none"}}, {Method: "Debugger.disable"}}
	case "Page":
		return []Call{{Method: "Page.disable"}, {"Page.setBootstrapScript", map[string]string{"source": ""}}}
	}
	return []Call{{Method: dom + ".disable"}}
}

// W1b closure review (tree ef4d5221): a backend that an earlier connection
// left enabled refused the new connection's first Page.enable ("Page
// domain already enabled"), because the session's empty record counted as
// a fresh backend. Now the first enable of an agent whose state the
// session does not know goes out after the agent's reset
// (SessionOptions.AgentReset), and the reset and the enable are written
// before any reply is read: WebKit holds the reset's reply here until the
// enable arrives, so a sequential reset would run out of its budget. An
// agent the session reset or disabled itself is known: its next enable
// gets no reset.
func TestUnknownAgentIsResetBeforeItsFirstEnable(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	agentFake(f, "Page", "Debugger")
	f.HoldUntil("Page.disable", "Page.enable")
	s := newFakeSession(t, f, SessionOptions{AgentReset: testAgentReset, CallTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, m := range []string{"Page.enable", "Debugger.enable", "Runtime.enable", "Page.disable", "Page.enable", "Runtime.enable"} {
		if _, err := s.Send(ctx, m, nil); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	want := []string{
		"Page.disable", "Page.setBootstrapScript", "Page.enable",
		"Debugger.setPauseOnExceptions", "Debugger.disable", "Debugger.enable",
		"Runtime.disable", "Runtime.enable", // nil AgentReset entry: the disable alone
		"Page.disable", "Page.enable", // known now: no reset
		"Runtime.enable",
	}
	if got := targetMethods(f, "page-1"); !slices.Equal(got, want) {
		t.Fatalf("page-1 received %v, want %v", got, want)
	}
}

// The reset tolerates only a refusal because the agent is not enabled. Any
// other failure of a reset call fails the enable, without the protocol
// error type: the reset changed the backend, so the caller must not take
// the enable for one without effect.
func TestAgentResetToleratesOnlyNotEnabled(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	agentFake(f)
	f.Handle("Memory.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Memory agent is broken"}
	})
	s := newFakeSession(t, f, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Send(ctx, "Timeline.enable", nil); err != nil {
		t.Fatalf("Timeline.enable after \"Timeline domain already disabled\": %v", err)
	}
	_, err := s.Send(ctx, "Memory.enable", nil)
	var pe *ErrorData
	if err == nil || !strings.Contains(err.Error(), "Memory agent is broken") || errors.As(err, &pe) || errors.Is(err, ErrNotSent) {
		t.Fatalf("Memory.enable after a failed reset: %v, want the reset's failure without *ErrorData and not ErrNotSent", err)
	}
	if got := s.AgentStates("page-1"); got["Memory"] != AgentEnabled {
		t.Fatalf("AgentStates = %v, want Memory enabled (its enable succeeded)", got)
	}
}

// SendPipelined writes a batch in order before it reads a reply: WebKit
// holds the first reply here until the second call arrives.
func TestSendPipelinedWritesTheBatchFirst(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.HoldUntil("DOM.hideHighlight", "DOM.setInspectModeEnabled")
	f.Handle("Overlay.fail", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "refused"}
	})
	s := newFakeSession(t, f, SessionOptions{CallTimeout: time.Second})
	errs := s.SendPipelined(context.Background(), []Call{
		{Method: "DOM.hideHighlight"},
		{"DOM.setInspectModeEnabled", map[string]bool{"enabled": false}},
		{Method: "Overlay.fail"},
	})
	if len(errs) != 3 || errs[0] != nil || errs[1] != nil || errs[2] == nil {
		t.Fatalf("SendPipelined = %v, want nil, nil and the refusal", errs)
	}
	if got, want := targetMethods(f, "page-1"), []string{"DOM.hideHighlight", "DOM.setInspectModeEnabled", "Overlay.fail"}; !slices.Equal(got, want) {
		t.Fatalf("page-1 received %v, want %v", got, want)
	}
}

// A batch ends at a call that cannot be written (here: over the outgoing
// wire limit, ErrNotSent): the later calls are not written and fail with
// ErrNotSent. An agent reset whose first call is not written fails the
// enable with ErrNotSent, because nothing reached the page. A batch with a
// method that hangs through Target routing, or on a closed session, sends
// nothing.
func TestPipelinedBatchStopsAtACallThatIsNotWritten(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	huge := map[string]string{"source": strings.Repeat("x", 4096)}
	s := newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
		return DialWebSocket(ctx, f.URL, transport.Limits{Routed: true, MaxOutgoing: 2048})
	}, SessionOptions{AgentReset: func(dom string) []Call { return []Call{{dom + ".disable", huge}} }})
	ctx := context.Background()
	if errs := s.SendPipelined(ctx, nil); errs != nil {
		t.Fatalf("SendPipelined(nil) = %v, want nil", errs)
	}
	errs := s.SendPipelined(ctx, []Call{{Method: "DOM.hideHighlight"}, {"Page.setBootstrapScript", huge}, {Method: "DOM.hideHighlight"}})
	if errs[0] != nil || !errors.Is(errs[1], ErrNotSent) || !errors.Is(errs[2], ErrNotSent) {
		t.Fatalf("SendPipelined = %v, want nil and two ErrNotSent", errs)
	}
	if n := len(f.ReceivedMethod("DOM.hideHighlight")); n != 1 {
		t.Fatalf("DOM.hideHighlight received %d times, want 1 (the call after the failed write was not written)", n)
	}
	if _, err := s.Send(ctx, "Page.enable", nil); !errors.Is(err, ErrNotSent) || len(f.ReceivedMethod("Page.enable")) != 0 {
		t.Fatalf("Page.enable after an unwritten reset: %v, want ErrNotSent and no enable on the wire", err)
	}
	for _, errs := range [][]error{
		s.SendPipelined(ctx, []Call{{Method: "DOM.hideHighlight"}, {Method: "CSS.getAllStyleSheets"}}),
		func() []error { _ = s.Close(); return s.SendPipelined(ctx, []Call{{Method: "DOM.hideHighlight"}}) }(),
	} {
		for _, err := range errs {
			if !errors.Is(err, ErrNotSent) {
				t.Fatalf("batch error %v, want ErrNotSent", err)
			}
		}
	}
	if n := len(f.ReceivedMethod("DOM.hideHighlight")); n != 1 {
		t.Fatalf("DOM.hideHighlight received %d times, want 1 (refused batches send nothing)", n)
	}
}

func newFakeSessionOn(t *testing.T, open func(context.Context) (transport.MessageConn, error), o SessionOptions) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Dial(ctx, open, o)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Send refuses before writing a method that hangs through Target routing
// (ErrNotSent) and parameters that do not encode (the error names the
// method); nothing reaches the page.
func TestSendRefusesBeforeWriting(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	ctx := context.Background()
	if _, err := s.Send(ctx, "CSS.getAllStyleSheets", nil); !errors.Is(err, ErrHangsThroughTarget) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("CSS.getAllStyleSheets: %v, want ErrHangsThroughTarget and ErrNotSent", err)
	}
	if _, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"bad": make(chan int)}); err == nil || !strings.Contains(err.Error(), "Runtime.evaluate: marshaling params") {
		t.Fatalf("Runtime.evaluate with a channel parameter: %v, want the marshaling error naming the method", err)
	}
	if got := targetMethods(f, "page-1"); len(got) != 0 {
		t.Fatalf("page-1 received %v, want nothing", got)
	}
}
