package devtools

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// A failed stop must keep the same cleanup obligation as a release that
// cannot stop a run: the later release must still stop the backend run.
func TestReviewFailedExplicitStopKeepsReleaseObligation(t *testing.T) {
	for _, kind := range []string{"profiler", "trace"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				if kind == "profiler" {
					return p.StartProfiler(ctx, c, "s1")
				}
				return p.StartTrace(ctx, c, "s1", true)
			})
			var failed atomic.Bool
			r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				failed.Store(true)
				r.f.Handle("ScriptProfiler.stopTracking", nil)
				return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate stop refusal"}
			})
			err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				if kind == "profiler" {
					_, err := p.StopProfiler(ctx, c, "s1")
					return err
				}
				_, _, err := p.StopTrace(ctx, c, "s1")
				return err
			})
			t.Logf("%s stop returned %v", kind, err)
			if err == nil {
				t.Errorf("the failed %s stop reported success", kind)
			}
			if !failed.Load() || !r.f.Tracking("page-1") {
				t.Fatal("setup: failed stop must leave the run tracking")
			}
			mark := lastSeq(r.f)
			r.release(t)
			// No background cleanup for this agent can still be pending if its
			// entry was forgotten without a mark. Wait through the release's
			// other resets with a real following operation.
			r.ao.Session = "s2"
			r.must(t, func(context.Context, *Page, webkit.Caller) error { return nil })
			if r.f.Tracking("page-1") {
				t.Errorf("release lost the failed %s stop obligation; %d stopTracking calls after the failed stop", kind, len(receivedAfter(r.f, "ScriptProfiler.stopTracking", mark)))
			}
		})
	}
}

func rawReview(t *testing.T, r *rig, method string, params string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mo, err := r.m.Begin(ctx, r.req, r.ao, "legacy script profiler", core.ClassMutate)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	raw, err := r.m.RawAs(ctx, mo, core.SessionOwner("legacy", mo.Session), method, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The accepted one-run refusal must also protect a live legacy profiler:
// W4 must not silently stop/restart another valid recording.
func TestReviewW4StartDoesNotReplaceLiveLegacyProfiler(t *testing.T) {
	r := newRig(t)
	rawReview(t, r, "ScriptProfiler.startTracking", `{"includeSamples":true}`)
	r.f.ProfilerSample("legacyOwned")
	mark := lastSeq(r.f)
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
	if err == nil {
		t.Error("W4 start succeeded instead of refusing the live legacy recording")
	}
	if got := receivedAfter(r.f, "ScriptProfiler.stopTracking", mark); len(got) != 0 {
		t.Errorf("W4 silently stopped the live legacy recording: %v", got)
	}
	raw := rawReview(t, r, "ScriptProfiler.stopTracking", "{}")
	_ = raw
}

// A legacy pre-W4 start must not join a released lease's still-pending
// run when the background reset failed.
func TestReviewLegacyStartServicesPendingStop(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
	r.f.ProfilerSample("oldLease")
	p := r.page(t)
	inside, unblock, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		held <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error { close(inside); <-unblock; return nil })
	}()
	<-inside
	var failed atomic.Bool
	r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		failed.Store(true)
		r.f.Handle("ScriptProfiler.stopTracking", nil)
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate background stop refusal"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := r.m.ReleaseSession(ctx, "s1")
	cancel()
	if err == nil {
		t.Fatal("setup: release unexpectedly got change lock")
	}
	close(unblock)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	eventually(t, "background stop refused", failed.Load)
	if !r.f.Tracking("page-1") {
		t.Fatal("setup: old run stopped")
	}
	mark := lastSeq(r.f)
	r.ao.Session = "s2"
	rawReview(t, r, "ScriptProfiler.startTracking", `{"includeSamples":true}`)
	if joined := scriptStarts(r.f, mark); joined != 0 {
		t.Errorf("legacy start joined the previous lease's pending run %d time(s)", joined)
	}
	if got := receivedAfter(r.f, "ScriptProfiler.stopTracking", mark); len(got) == 0 {
		t.Error("legacy start did not service the pending ScriptProfiler stop")
	}
	rawReview(t, r, "ScriptProfiler.stopTracking", "{}")
}
