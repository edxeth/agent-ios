package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design 2.5: read-only operations run in parallel "with at most 8
// protocol calls in flight per page" (core.DefaultMaxInFlight). Twelve
// concurrent "get title" commands from separate clients, with every page
// reply held back, put at most 8 runtime calls on the page before the
// first reply; the others are sent as replies free their slots, and every
// command succeeds.
func TestRuntimeCallsThroughTheDaemonKeepThePerPageLimit(t *testing.T) {
	const commands = 12
	const replyDelay = time.Second
	o := startInProcess(t, RunOptions{})
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	var mu sync.Mutex
	var arrivals []time.Time
	f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		if fn == "get" {
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
		}
		return map[string]any{"ok": true, "value": "Title"}
	})
	run := func() (RunResult, error) {
		c := connect(t, o)
		return c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: []string{"get", "title"}, Owner: Owner{Session: "s1"}, PageReq: PageReq{Page: f.URL}}, nil)
	}
	// Attach and confirm the owner before the measured commands.
	if _, err := run(); err != nil {
		t.Fatalf("first get title: %v", err)
	}
	mu.Lock()
	arrivals = nil
	mu.Unlock()
	f.DelayReplies("runtime:get", replyDelay)

	var wg sync.WaitGroup
	errs := make(chan error, commands)
	for i := 0; i < commands; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := run()
			if err == nil && res.Text != "Title" {
				err = fmt.Errorf("get title = %q, want Title", res.Text)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent get title: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != commands {
		t.Fatalf("the page received %d get calls, want %d", len(arrivals), commands)
	}
	// No reply leaves the fake before arrivals[0]+replyDelay, so every call
	// that arrived earlier was in flight at the same time.
	first, before := arrivals[0], 0
	for _, a := range arrivals {
		if a.Before(first.Add(replyDelay)) {
			before++
		}
	}
	if before > core.DefaultMaxInFlight {
		t.Fatalf("%d runtime calls reached the page before any reply, want at most %d (design 2.5)", before, core.DefaultMaxInFlight)
	}
}

// "diff snapshot" compares "the last two snapshots of this session"
// (command summary; design 4.2). The page's ref table is shared by the
// lease owners of the page, but the snapshot history is not: session B's
// first diff after A's lease ended finds no snapshot of B, and does not
// show A's page contents.
func TestDiffSnapshotUsesOnlyTheSessionsOwnSnapshots(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	var mu sync.Mutex
	n := 0
	f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		switch fn {
		case "snapshot":
			mu.Lock()
			n++
			name := fmt.Sprintf("Save-%d", n)
			mu.Unlock()
			return map[string]any{"ok": true, "value": map[string]any{
				"tree":    []any{map[string]any{"role": "button", "name": name, "ref": "e1"}},
				"refs":    []any{map[string]any{"ref": "e1", "role": "button", "name": name, "unique": true}},
				"nextRef": 2, "doc": "d1", "url": "http://x.test/", "title": "X",
			}}
		case "ping", "release":
			return map[string]any{"ok": true, "value": true}
		}
		return map[string]any{"ok": false, "error": map[string]string{"code": "unsupported", "message": "fake page has no " + fn}}
	})
	runAs := func(session string, argv ...string) (RunResult, error) {
		c := connect(t, o)
		return c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: argv, Owner: Owner{Session: session}, PageReq: PageReq{Page: f.URL}}, nil)
	}
	for i := 0; i < 2; i++ {
		if _, err := runAs("a", "snapshot"); err != nil {
			t.Fatalf("snapshot %d of a: %v", i+1, err)
		}
	}
	diff, err := runAs("a", "diff", "snapshot")
	if err != nil || !strings.Contains(diff.Text, "+- button \"Save-2\"") {
		t.Fatalf("a's diff snapshot = %q, %v; want the change from Save-1 to Save-2", diff.Text, err)
	}
	if _, err := runAs("a", "session", "release"); err != nil {
		t.Fatalf("session release of a: %v", err)
	}
	res, err := runAs("b", "diff", "snapshot")
	if err == nil || !strings.Contains(err.Error(), "it has 0") {
		t.Fatalf("b's first diff snapshot = %q, %v; want the error that b has no snapshots", res.Text, err)
	}
	if strings.Contains(res.Text, "Save-") {
		t.Fatalf("b's diff snapshot shows a's page contents: %q", res.Text)
	}
}

// Design 2.4 and 2.6 through the daemon (the review's diagnostic): session
// B enabled Network, then releases with a 150 ms request deadline. The
// session's cleanup reaches the page and the release succeeds before the
// client's own deadline, whether or not the page answers the best-effort
// runtime release.
func TestShortLeaseReleaseCleansUpWhenThePageRuntimeIsSilent(t *testing.T) {
	const deadline = 150 * time.Millisecond
	for _, silent := range []bool{true, false} {
		t.Run(fmt.Sprintf("silent runtime %v", silent), func(t *testing.T) {
			o := startInProcess(t, RunOptions{})
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			var mu sync.Mutex
			releases := 0
			f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
				if fn == "release" {
					mu.Lock()
					releases++
					mu.Unlock()
					if silent {
						return fakewebkit.NoReply
					}
				}
				return map[string]any{"ok": true, "value": true}
			})
			c := connect(t, o)
			b := Owner{Session: "B"}
			if err := leasePage(t, c, f.URL, b, 5*time.Second, "Network.enable"); err != nil {
				t.Fatalf("Network.enable of B: %v", err)
			}
			start := time.Now()
			_, err := c.Run(ctxFor(t, deadline), RunRequest{Argv: []string{"session", "release"}, Deadline: time.Now().Add(deadline), Owner: b}, nil)
			if err != nil {
				t.Fatalf("release of B with a %v deadline after %v: %v, want success", deadline, time.Since(start), err)
			}
			if n := len(f.ReceivedMethod("Network.disable")); n != 1 {
				t.Fatalf("Network.disable reached the page %d times, want 1: B's cleanup did not run", n)
			}
			mu.Lock()
			defer mu.Unlock()
			if releases != 1 {
				t.Fatalf("runtime release calls = %d, want 1", releases)
			}
		})
	}
}
