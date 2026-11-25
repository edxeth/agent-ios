package webkit

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design 2.4 and 2.7 (W1b, page-owner delta): a Session records, per
// target, which domain agents it enabled on that target's backend, from
// the replies to the enables and disables it sent there. A cross-site
// back navigation brings the earlier target id back with its backend and
// every agent it had (WebKit's commit never disconnected it), so the
// record outlives the target's destruction: the setup of the restored
// target sees what its backend kept.
func TestSessionRecordsTheAgentsOfEachBackend(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Handle("Debugger.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Debugger domain already enabled"}
	})
	f.Unsupported("Animation.enable")
	f.Stall("Console.enable")
	seen := make(chan map[string]AgentState, 4)
	s := newFakeSession(t, f, SessionOptions{Setup: func(_ context.Context, tc TargetCaller) error {
		seen <- tc.(interface{ AgentStates() map[string]AgentState }).AgentStates()
		return nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	send := func(method string) error {
		c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err := s.Send(c, method, nil)
		return err
	}
	for _, m := range []string{"Page.enable", "Network.enable", "Network.disable", "Heap.enable", "Runtime.evaluate"} {
		if err := send(m); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if err := send("Debugger.enable"); err == nil {
		t.Fatal("Debugger.enable: no error, want the already-enabled refusal")
	}
	if err := send("Animation.enable"); err == nil {
		t.Fatal("Animation.enable: no error, want the refusal")
	}
	// The first Heap.disable is the reset before Heap.enable (it
	// succeeds); the next one is stalled.
	f.Stall("Heap.disable")
	if err := send("Console.enable"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Console.enable: %v, want the deadline (reply lost)", err)
	}
	if err := send("Heap.disable"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Heap.disable: %v, want the deadline (reply lost)", err)
	}
	want := map[string]AgentState{
		"Page":     AgentEnabled,      // succeeded
		"Debugger": AgentEnabled,      // refused as already enabled: the backend has it
		"Console":  AgentMaybeEnabled, // written, reply lost
		"Heap":     AgentMaybeEnabled, // enabled, then a disable whose reply was lost
		"Network":  AgentDisabled,     // enabled, then disabled
		// Animation: its reset disabled it, then its enable was refused
		// for another reason.
		"Animation": AgentDisabled,
	}
	if got := s.AgentStates("page-1"); !maps.Equal(got, want) {
		t.Fatalf("AgentStates(page-1) = %v, want %v", got, want)
	}

	f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	if got := <-seen; len(got) != 0 {
		t.Fatalf("setup of the new backend page-2 saw %v, want no record", got)
	}
	if err := send("Page.enable"); err != nil {
		t.Fatalf("Page.enable on page-2: %v", err)
	}
	// Back: page-1 returns with the backend it had.
	f.Swap("page-1", 0)
	waitFor(t, func() bool { return s.ActiveTarget() == "page-1" })
	if got := <-seen; !maps.Equal(got, want) {
		t.Fatalf("setup of the restored page-1 saw %v, want %v", got, want)
	}
	if got := s.AgentStates("page-2"); !maps.Equal(got, map[string]AgentState{"Page": AgentEnabled}) {
		t.Fatalf("AgentStates(page-2) after the swap away = %v, want Page enabled", got)
	}
}
