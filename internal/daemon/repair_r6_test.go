package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the sixth W1b review (R6-5, item 3: daemons shared by
// tests).

// noDaemonCore makes t run the in-process core of AGENT_IOS_NO_DAEMON=1 and ends
// that core when t ends. The core is one per process, so without the
// cleanup a later test, or the next run of -count, would get this test's
// page records, as the CLI tests did with a daemon shared by the suite
// (review finding R5-4: a closed fake's page stays lost and refuses a later
// fake that gets the same loopback URL).
func noDaemonCore(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_IOS_NO_DAEMON", "1")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownBudget+time.Second)
		defer cancel()
		if err := CloseInProcess(ctx); err != nil {
			t.Errorf("ending the in-process core: %v", err)
		}
	})
}

// Each test gets a fresh in-process core: the second test finds no page of
// the first.
func TestNoDaemonCoreIsPerTest(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		// The page outlives the core (cleanups run last-in first-out),
		// so that the core's cleanup reaches it instead of racing its
		// close.
		f := newFake(t, fakewebkit.Options{})
		noDaemonCore(t)
		c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: filepath.Join(shortDir(t), "agent-ios"), Exe: "/bin/false", Version: "test"})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer c.Close()
		if err := leasePage(t, c, f.URL, Owner{Session: "s1"}, 5*time.Second); err != nil {
			t.Fatalf("leasing the page: %v", err)
		}
	})
	t.Run("second", func(t *testing.T) {
		noDaemonCore(t)
		c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: filepath.Join(shortDir(t), "agent-ios"), Exe: "/bin/false", Version: "test"})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer c.Close()
		st, err := c.Status(ctxFor(t, 3*time.Second))
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if len(st.Pages) != 0 {
			t.Fatalf("the in-process core of this test holds %d pages of an earlier test: %+v", len(st.Pages), st.Pages)
		}
	})
}
