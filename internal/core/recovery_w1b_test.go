package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Design 2.6 step 3: background recovery reconnects once. When the new
// connection does not answer the liveness probe either, the page stays
// stalled (and the status says so); there is no second reconnect.
func TestRecoveryWithADeadPageStaysStalled(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	r.f.Stall("Runtime.evaluate") // every connection, the probe included
	if _, err := r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`)); err == nil {
		t.Fatal("Raw on a stalled page succeeded")
	}
	r.m.End(mo.ID)
	pg := mo.Page
	eventually(t, 15*time.Second, "the recovery attempt to fail", func() bool {
		pg.conn.mu.Lock()
		defer pg.conn.mu.Unlock()
		return pg.conn.state == ConnStalled && errors.Is(pg.conn.stallErr, ErrRecoveryFailed)
	})
	time.Sleep(500 * time.Millisecond)
	if n := r.f.ConnectionCount(); n != 2 {
		t.Errorf("connections = %d, want 2 (one reconnect)", n)
	}
	st := r.m.Status()
	if len(st) != 1 || st[0].State != "stalled" {
		t.Fatalf("status %+v, want the page stalled", st)
	}
	if !strings.Contains(pg.conn.stallErr.Error(), "does not answer") {
		t.Errorf("stall error %q does not say the new connection does not answer", pg.conn.stallErr)
	}
}
