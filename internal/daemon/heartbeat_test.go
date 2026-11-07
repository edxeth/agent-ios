package daemon

import (
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Design 2.6 heartbeat: an MCP-style caller (AttachPage, then HoldLease)
// keeps its lease past the idle TTL while it lives; after Close the TTL
// releases it.
func TestHoldLeaseKeepsTheLeaseUntilClose(t *testing.T) {
	const ttl = 300 * time.Millisecond
	o := startInProcess(t, RunOptions{Manager: core.ManagerOptions{LeaseTTL: ttl, SweepEvery: 50 * time.Millisecond}})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := c.AttachPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	pc.HoldLease()
	time.Sleep(4 * ttl)
	st, err := c.Status(ctxFor(t, 5*time.Second))
	if err != nil || len(st.Pages) != 1 || st.Pages[0].Session != "mcp" || st.Pages[0].Lease != "active" {
		t.Fatalf("status %+v, %v after %v; want the lease of mcp held", st, err, 4*ttl)
	}
	select {
	case <-pc.Done():
		t.Fatal("Done closed while the page is attached")
	default:
	}
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(3 * time.Second)
	for {
		st, err := c.Status(ctxFor(t, 5*time.Second))
		if err == nil && len(st.Pages) == 1 && st.Pages[0].Lease == "none" {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("lease not released 3 s after Close: %+v, %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
