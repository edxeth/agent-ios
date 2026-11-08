package devtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W4 repair round 3, library delta review
// /home/devkit/.pi/artifacts/reviewer/w4-lib-delta-review-20261107-055336.md
// (counterexamples in /home/devkit/.pi/artifacts/reviewer/w4-lib-r2-repro/):
// cleanup ownership and request handlers must survive a partial failure of
// a change.

// provisionalRefuses starts a cross-process swap to target id, waits until
// its setup got the dark media override (so the swap is in progress), and
// makes the provisional target refuse method. The swap commits 2 s later;
// the returned function waits for that.
func provisionalRefuses(t *testing.T, r *rig, id, method string) (wait func()) {
	t.Helper()
	return provisionalRefusesWhen(t, r, id, method, darkMarker, func(fakewebkit.Call) bool { return true })
}

// provisionalRefusesWhen is provisionalRefuses for a method that the setup
// itself sends: it waits until target id received the setup message that
// ready matches, and the provisional target refuses only the calls of
// method that refuse matches, so the setup cannot fail on a refusal meant
// for the change under test (review round 3, P2: a refusal of the setup's
// own call makes the setup fail, which drops the change's forwarding).
func provisionalRefusesWhen(t *testing.T, r *rig, id, method string, ready func(fakewebkit.Record) bool, refuse func(fakewebkit.Call) bool) (wait func()) {
	t.Helper()
	r.must(t, mediaDark)
	done := make(chan struct{})
	go func() { r.f.Swap(id, 2*time.Second); close(done) }()
	eventually(t, "provisional setup", func() bool {
		for _, rec := range sentTo(r.f, id) {
			if ready(rec) {
				return true
			}
		}
		return false
	})
	r.f.Handle(method, func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == id && refuse(c) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "provisional target refused " + method}
		}
		return nil, nil
	})
	return func() { <-done }
}

// completions counts the completions of intercepted request id.
func completions(f *fakewebkit.Server, id string) []string {
	var out []string
	for _, m := range []string{"Network.interceptWithRequest", "Network.interceptContinue", "Network.interceptRequestWithError", "Network.interceptRequestWithResponse"} {
		for _, rec := range received(f, m) {
			if strings.Contains(string(rec.Params), `"requestId":"`+id+`"`) {
				out = append(out, m)
			}
		}
	}
	return out
}

// Finding intercept.go:652: when the active target accepts a new
// interception rule and a provisional target refuses the forwarded one,
// the change fails, but the active rule pauses matching requests; its
// handler must stay and answer them.
func TestInterceptionHandlersSurviveAForwardingFailure(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
		add             func(ctx context.Context, p *Page, c webkit.Caller) error
	}{
		{"credentials", "https://a.example/private", "Network.interceptWithRequest", func(ctx context.Context, p *Page, c webkit.Caller) error {
			_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "u", "pw")
			return err
		}},
		{"route", "https://fixture.example/blocked/x", "Network.interceptRequestWithError", func(ctx context.Context, p *Page, c webkit.Caller) error {
			_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/blocked/*", Abort: true})
			return err
		}},
		{"offline", "https://fixture.example/any", "Network.interceptRequestWithError", func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetOffline(ctx, c, "s1", true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			wait := provisionalRefuses(t, r, "r3-"+tc.name, "Network.addInterception")
			defer wait()
			err := r.do(t, tc.add)
			if err == nil || !strings.Contains(err.Error(), "provisional target refused") {
				t.Fatalf("want the forwarding failure, got %v", err)
			}
			if strings.Contains(err.Error(), "pw") && tc.name == "credentials" {
				t.Fatalf("the error shows the password: %v", err)
			}
			// The active target holds the rule, so it pauses the next match.
			intercept(r.f, "r3-req", tc.url)
			eventually(t, "a completion of the paused request", func() bool { return len(completions(r.f, "r3-req")) > 0 })
			if got := completions(r.f, "r3-req"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("completions %v, want one %s", got, tc.want)
			}
		})
	}
}

// Finding intercept.go:652, the rejection case: a rule that the active
// target refused is not installed, so its handler goes and a retry sends
// the rule again (as TestReviewOfflineRetryAfterRuleFailure for offline).
func TestCredentialsRuleRefusedByThePageIsRolledBack(t *testing.T) {
	r := newRig(t)
	credentialsVerified[transport.KindIWDP] = true
	t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
	refuse := true
	r.f.Handle("Network.addInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuse {
			refuse = false
			return nil, &fakewebkit.Error{Code: -32000, Message: "rule refused"}
		}
		return nil, nil
	})
	set := func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "u", "pw")
		return err
	}
	if err := r.do(t, set); err == nil || strings.Contains(err.Error(), "stay in place") {
		t.Fatalf("want the plain refusal, got %v", err)
	}
	before := len(received(r.f, "Network.addInterception"))
	r.must(t, set)
	if len(received(r.f, "Network.addInterception")) == before {
		t.Fatal("the retry did not send the refused rule again")
	}
}

// Finding profile.go:181: the profiler's cleanup entry is recorded before
// startTracking is sent. A start that cannot enter its change sends
// nothing and leaves no record; a start whose outcome is unknown is
// stopped by the lease release.
func TestProfilerStartRecordsItsCleanupBeforeTheSend(t *testing.T) {
	r := newRig(t)
	p := r.page(t)
	inside, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error { close(inside); <-release; return nil })
	}()
	<-inside
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		return p.StartProfiler(short, c, "s1")
	})
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if err == nil {
		t.Fatal("want the change admission timeout")
	}
	if n := len(received(r.f, "ScriptProfiler.startTracking")); n != 0 {
		t.Fatalf("startTracking was sent %d times before the change began", n)
	}

	// An unknown outcome: the reply comes after the deadline.
	r.f.DelayReplies("ScriptProfiler.startTracking", 500*time.Millisecond)
	err = r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		return p.StartProfiler(short, c, "s1")
	})
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("want an unknown start, got %v", err)
	}
	r.release(t)
	if len(received(r.f, "ScriptProfiler.stopTracking")) == 0 {
		t.Fatal("the lease release did not stop a sampling whose start had an unknown outcome")
	}
}

// Finding trace.go:271: a trace start whose forwarded Timeline.enable a
// provisional target refused stops what it started on the active target
// (both recordings), so nothing keeps recording without an owner.
func TestTraceStartForwardingFailureStopsTheRecording(t *testing.T) {
	r := newRig(t)
	wait := provisionalRefuses(t, r, "r3-trace", "Timeline.enable")
	defer wait()
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
	if err == nil || !strings.Contains(err.Error(), "provisional target refused") {
		t.Fatalf("want the forwarding failure, got %v", err)
	}
	if len(received(r.f, "Timeline.start")) != 1 || len(received(r.f, "ScriptProfiler.startTracking")) != 1 {
		t.Fatal("the active recordings did not start")
	}
	r.release(t)
	if len(received(r.f, "Timeline.stop")) == 0 || len(received(r.f, "ScriptProfiler.stopTracking")) == 0 {
		t.Fatalf("the recordings were not stopped: Timeline.stop %d, ScriptProfiler.stopTracking %d",
			len(received(r.f, "Timeline.stop")), len(received(r.f, "ScriptProfiler.stopTracking")))
	}
}

// Finding trace.go:271, unknown outcome: a Timeline.start whose reply is
// lost and whose stop fails too keeps the record and the entry, so the
// lease release stops the recording. The fake never answers the start,
// and the command's context ends once the fake has received it (W4 round
// 7: a 100 ms deadline also ended the Timeline reset before the start
// under host load).
func TestTraceStartWithAnUnknownOutcomeIsStoppedAtRelease(t *testing.T) {
	r := newRig(t)
	r.f.Stall("Timeline.start")
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		short, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			for len(received(r.f, "Timeline.start")) == 0 && short.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		return p.StartTrace(short, c, "s1", false)
	})
	if err == nil || !strings.Contains(err.Error(), "end of the lease stops it") {
		t.Fatalf("want an unknown start that the release stops, got %v", err)
	}
	stops := len(received(r.f, "Timeline.stop"))
	r.release(t)
	if len(received(r.f, "Timeline.stop")) == stops {
		t.Fatal("the lease release did not stop a recording whose start had an unknown outcome")
	}
}

// Finding overrides.go:140: a failed reset keeps the cleanup entry, so the
// lease release resets the override that is still active. The same holds
// for a reset that a provisional target refused.
func TestClearOverrideKeepsItsCleanupWhenTheResetFails(t *testing.T) {
	resetCalls := func(f *fakewebkit.Server) int {
		n := 0
		for _, rec := range received(f, "Page.overrideUserAgent") {
			var v map[string]any
			if json.Unmarshal(rec.Params, &v) == nil && v["value"] == nil {
				n++
			}
		}
		return n
	}
	t.Run("active target", func(t *testing.T) {
		r := newRig(t)
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetUserAgent(ctx, c, "s1", "custom")
		})
		r.f.Handle("Page.overrideUserAgent", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
			if !strings.Contains(string(c.Params), `"value"`) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "reset refused"}
			}
			return nil, nil
		})
		if err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetUserAgent(ctx, c, "s1", "") }); err == nil {
			t.Fatal("want the reset failure")
		}
		r.f.Handle("Page.overrideUserAgent", func(fakewebkit.Call) (any, *fakewebkit.Error) { return nil, nil })
		before := resetCalls(r.f)
		r.release(t)
		if resetCalls(r.f) == before {
			t.Fatal("the failed clear dropped the cleanup entry; the release does not reset the user agent")
		}
	})
	t.Run("provisional target", func(t *testing.T) {
		r := newRig(t)
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetUserAgent(ctx, c, "s1", "custom")
		})
		// The setup sends the custom user agent; only the reset (no
		// value) is refused.
		customUA := func(rec fakewebkit.Record) bool {
			return is(rec, "Page.overrideUserAgent", map[string]any{"value": "custom"})
		}
		reset := func(c fakewebkit.Call) bool { return !strings.Contains(string(c.Params), `"value"`) }
		wait := provisionalRefusesWhen(t, r, "r3-ua", "Page.overrideUserAgent", customUA, reset)
		err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetUserAgent(ctx, c, "s1", "") })
		if err == nil || !strings.Contains(err.Error(), "provisional target refused") {
			t.Fatalf("want the forwarding failure, got %v", err)
		}
		wait()
		r.f.Handle("Page.overrideUserAgent", func(fakewebkit.Call) (any, *fakewebkit.Error) { return nil, nil })
		before := resetCalls(r.f)
		r.release(t)
		if resetCalls(r.f) == before {
			t.Fatal("the refused reset dropped the cleanup entry; the release does not reset the committed target")
		}
	})
}

// Finding mask.go:105: a network-path redirect reference leaked its
// password in the default HAR's Location header and redirectURL.
func TestHARMasksANetworkPathRedirect(t *testing.T) {
	reqs, _ := harFixture()
	reqs[0].ResponseHeaders["Location"] = "//user:r3-secret@a.test/callback"
	raw, err := json.Marshal(BuildHAR(reqs[:1], nil, nil, HAROptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "r3-secret") || !strings.Contains(string(raw), "//user:%5Bmasked%5D@a.test/callback") {
		t.Fatalf("network-path Location in the default HAR: %s", raw)
	}
}
