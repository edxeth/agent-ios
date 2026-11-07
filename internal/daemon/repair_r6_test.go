package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the sixth W1b review (R6-5, item 3: daemons shared by
// tests).

// noDaemonCore makes t run the in-process core of IWDP_NO_DAEMON=1 and ends
// that core when t ends. The core is one per process, so without the
// cleanup a later test, or the next run of -count, would get this test's
// page records, as the CLI tests did with a daemon shared by the suite
// (review finding R5-4: a closed fake's page stays lost and refuses a later
// fake that gets the same loopback URL).
func noDaemonCore(t *testing.T) {
	t.Helper()
	t.Setenv("IWDP_NO_DAEMON", "1")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownBudget+time.Second)
		defer cancel()
		if err := CloseInProcess(ctx); err != nil {
			t.Errorf("ending the in-process core: %v", err)
		}
	})
}

// closeLimit bounds one PageCaller.Close; it normally takes milliseconds.
const closeLimit = 10 * time.Second

// pageCloseRounds is how many operations TestPageCallerCloseReturns opens
// and closes. Before the fix about one Close in a few hundred hung, more
// often under -race and host load.
const pageCloseRounds = 400

// R6-5 (race run of round 6): PageCaller.Close never returned. hold.end
// closed the begin stream's body while its reader goroutine was in a Read
// that ended at EOF; net/http then blocks that Read forever (the read loop
// of the connection has moved on), so Close waited for the reader forever.
// The in-memory connections of IWDP_NO_DAEMON=1 hit it most often.
func TestPageCallerCloseReturns(t *testing.T) {
	noDaemonCore(t)
	c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: filepath.Join(shortDir(t), "iwdp"), Exe: "/bin/false", Version: "test"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	f := newFake(t, fakewebkit.Options{})
	for i := 0; i < pageCloseRounds; i++ {
		pc, err := c.OpenPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}})
		if err != nil {
			t.Fatalf("OpenPage %d: %v", i, err)
		}
		closed := make(chan struct{})
		go func() {
			_ = pc.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(closeLimit):
			t.Fatalf("PageCaller.Close of operation %d did not return within %v", i, closeLimit)
		}
	}
}

// Each test gets a fresh in-process core: the second test finds no page of
// the first.
func TestNoDaemonCoreIsPerTest(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		noDaemonCore(t)
		c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: filepath.Join(shortDir(t), "iwdp"), Exe: "/bin/false", Version: "test"})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer c.Close()
		f := newFake(t, fakewebkit.Options{})
		pc, err := c.OpenPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}})
		if err != nil {
			t.Fatalf("OpenPage: %v", err)
		}
		_ = pc.Close()
	})
	t.Run("second", func(t *testing.T) {
		noDaemonCore(t)
		c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: filepath.Join(shortDir(t), "iwdp"), Exe: "/bin/false", Version: "test"})
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
