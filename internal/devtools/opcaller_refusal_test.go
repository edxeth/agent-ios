package devtools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Contract: an unwritten call changes nothing (devtools.noEffect and
// docs/notes/w4.md, state-change rollback rule). A refused add must not
// retain a handler when no target could have received its rule.
func TestReviewEndedOperationDoesNotKeepUninstalledHandlers(t *testing.T) {
	for _, kind := range []string{"route", "offline", "credentials", "profiler"} {
		t.Run(kind, func(t *testing.T) {
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			r := newRig(t)
			p := r.registryOp(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := r.m.Do(ctx, r.req, r.ao, "refusal repro", core.ClassMutate, false, func(_ context.Context, mo *core.ManagedOp) error {
				mo.Op.End()
				_, sendErr := mo.Op.Caller().Send(ctx, "Network.addInterception", wipRule("unwritten"))
				if !errors.Is(sendErr, webkit.ErrNotSent) {
					t.Errorf("unwritten opCaller refusal lacks ErrNotSent: %v", sendErr)
				}
				mark := lastSeq(r.f)
				var addErr error
				switch kind {
				case "route":
					_, addErr = p.AddRoute(ctx, mo.Op.Caller(), "s1", Route{Pattern: "**/blocked/*", Abort: true})
				case "offline":
					addErr = p.SetOffline(ctx, mo.Op.Caller(), "s1", true)
				case "credentials":
					_, addErr = p.SetCredentials(ctx, mo.Op.Caller(), "s1", "https://fixture.example", "u", "pw")
				case "profiler":
					addErr = p.StartProfiler(ctx, mo.Op.Caller(), "s1")
				}
				if addErr == nil {
					t.Fatal("setup: ended-operation mutation unexpectedly succeeded")
				}
				for _, m := range []string{"Network.addInterception", "ScriptProfiler.startTracking"} {
					if got := receivedAfter(r.f, m, mark); len(got) != 0 {
						t.Fatalf("setup: refused call actually reached page: %v", got)
					}
				}
				t.Logf("refused mutation: %v", addErr)
				if kind == "route" && len(p.Routes("s1")) != 0 {
					t.Error("uninstalled route handler retained")
				}
				p.mu.Lock()
				ic, profile := p.icpt, p.profiles["s1"]
				p.mu.Unlock()
				if kind == "profiler" && profile != nil {
					t.Error("unstarted profiler retained without desired-state entry")
				}
				ic.mu.Lock()
				offline, creds := ic.offline["s1"], len(ic.creds["s1"])
				ic.mu.Unlock()
				if kind == "offline" && offline {
					t.Error("uninstalled offline handler retained")
				}
				if kind == "credentials" && creds != 0 {
					t.Error("uninstalled credentials handler retained")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
