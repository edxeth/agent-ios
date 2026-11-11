package commands

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W3b rebase repair round 1, finding F2 (docs/notes/w3b.md; the pending
// cleanup of core's release_lifecycle.go, core-events r4): State.Release
// removes an owner and its entries before its cleanup sends, so a W3b
// owner's release keeps each cleanup send that failed pending as an
// unknown domain (core.PageSession.ReleaseOwnerChange), and W3b resets it
// before it uses the domain again.

// The reviewer's reproduction (TestReviewFailedHoverCleanupIsRetried),
// assertions unchanged: a refused reset and disable leave the original
// forced state intact, and the following action must not lose the
// cleanup obligation.
func TestFailedHoverCleanupIsRetried(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.mustRun(t, "hover", "#hov", "--css")
	for _, m := range []string{"CSS.forcePseudoState", "CSS.disable"} {
		h.f.Handle(m, func(fakewebkit.Call) (any, *fakewebkit.Error) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "cleanup refused (review)"}
		})
	}
	code, _, errOut := h.run("press", "Tab")
	if code == ExitOK {
		t.Fatalf("first action ignored cleanup failure: %s", errOut)
	}
	before := len(h.f.ReceivedMethod("CSS.forcePseudoState")) + len(h.f.ReceivedMethod("CSS.disable"))
	for _, m := range []string{"CSS.forcePseudoState", "CSS.disable"} {
		h.f.Result(m, map[string]any{})
	}
	h.mustRun(t, "press", "Tab")
	after := len(h.f.ReceivedMethod("CSS.forcePseudoState")) + len(h.f.ReceivedMethod("CSS.disable"))
	if after == before {
		t.Fatalf("cleanup was refused, but the next action sent no cleanup: CSS needed=%v, hover entry=%v, calls=%v", h.ps.State.Needs("CSS"), h.ps.State.HasEntry("w3b:hover|agent"), cssCalls(h))
	}
}

// refusable answers method with {} or, while refuse is set, refuses it.
func refusable(f *fakewebkit.Server, method string) *atomic.Bool {
	refuse := new(atomic.Bool)
	f.Handle(method, func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuse.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: method + " refused"}
		}
		return map[string]any{}, nil
	})
	return refuse
}

// methodsSince returns the calls of methods that the page received after
// the first n records, in order.
func methodsSince(f *fakewebkit.Server, n int, methods ...string) []string {
	var out []string
	for _, rec := range f.Received()[n:] {
		if slices.Contains(methods, rec.Method) {
			out = append(out, rec.Method)
		}
	}
	return out
}

var (
	runtimeAgent = []string{"Runtime.enable", "Runtime.disable"}
	cssAgent     = []string{"CSS.enable", "CSS.disable", "CSS.forcePseudoState"}
)

// "frame main" whose Runtime.disable the page refuses: the frame owner
// leaves, the Runtime cleanup stays pending, and the session's next
// "frame" resets Runtime before its Runtime.enable. While the reset is
// refused, "frame" fails and sends no Runtime.enable.
func TestFrameMainFailedDisableStaysPending(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	if code, _, msg := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame #kid: %s", msg)
	}
	refuse := refusable(h.f, "Runtime.disable")
	refuse.Store(true)
	if code, _, msg := fh.run("frame", "main"); code == ExitOK || !strings.Contains(msg, "Runtime.disable refused") {
		t.Fatalf("frame main with a refused Runtime.disable: exit %d, %q; want the refusal", code, msg)
	}
	if slices.Contains(h.ps.State.AllOwners(), frameOwner("agent")) || h.ps.State.Needs("Runtime") {
		t.Fatalf("owners %q, Runtime needed %v after frame main; want the frame owner gone", h.ps.State.AllOwners(), h.ps.State.Needs("Runtime"))
	}
	n := len(h.f.Received())
	if code, _, msg := fh.run("frame", "#kid"); code == ExitOK || !strings.Contains(msg, "Runtime.disable refused") {
		t.Fatalf("frame #kid while the pending reset is refused: exit %d, %q; want the refusal", code, msg)
	}
	if got := methodsSince(h.f, n, runtimeAgent...); !slices.Equal(got, []string{"Runtime.disable"}) {
		t.Fatalf("Runtime calls %q while the pending reset is refused, want only the reset (no Runtime.enable)", got)
	}
	refuse.Store(false)
	n = len(h.f.Received())
	if code, _, msg := fh.run("frame", "#kid"); code != 0 {
		t.Fatalf("frame #kid after the refusal ended: %s", msg)
	}
	if got := methodsSince(h.f, n, runtimeAgent...); !slices.Equal(got, []string{"Runtime.disable", "Runtime.enable"}) {
		t.Fatalf("Runtime calls %q, want the pending reset, then the frame owner's enable", got)
	}
	n = len(h.f.Received())
	if code, _, msg := fh.run("frame", "main"); code != 0 {
		t.Fatalf("frame main: %s", msg)
	}
	if got := methodsSince(h.f, n, runtimeAgent...); !slices.Equal(got, []string{"Runtime.disable"}) {
		t.Fatalf("Runtime calls of frame main %q, want one Runtime.disable (no reset left pending)", got)
	}
}

// hover --css resets a CSS cleanup that another owner's release left
// pending before its CSS.enable, in the same change; while that reset is
// refused, hover --css fails and sends no CSS.enable.
func TestHoverCSSResetsAPendingCSSCleanupFirst(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	// The session's CSS agent state is known (enabled once, then disabled).
	h.mustRun(t, "hover", "#hov", "--css")
	h.mustRun(t, "press", "Tab")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	other := core.SessionOwner("other", "x")
	if !h.ps.State.Acquire(other, "CSS") {
		t.Fatal("fixture: CSS already owned")
	}
	refuse := refusable(h.f, "CSS.disable")
	refuse.Store(true)
	if ran, err := h.ps.ReleaseOwnerChange(ctx, h.ps.Caller, other); !ran || err == nil {
		t.Fatalf("fixture: ReleaseOwnerChange = %v, %v; want the refused CSS.disable", ran, err)
	}
	n := len(h.f.Received())
	if code, _, msg := h.run("hover", "#hov", "--css"); code == ExitOK || !strings.Contains(msg, "CSS.disable refused") {
		t.Fatalf("hover --css while the pending reset is refused: exit %d, %q; want the refusal", code, msg)
	}
	if got := methodsSince(h.f, n, cssAgent...); !slices.Equal(got, []string{"CSS.disable"}) {
		t.Fatalf("CSS calls %q while the pending reset is refused, want only the reset (no CSS.enable)", got)
	}
	refuse.Store(false)
	n = len(h.f.Received())
	h.mustRun(t, "hover", "#hov", "--css")
	if got := methodsSince(h.f, n, cssAgent...); !slices.Equal(got, []string{"CSS.disable", "CSS.enable", "CSS.forcePseudoState"}) {
		t.Fatalf("CSS calls %q, want the pending reset, then the hover owner's enable and forced state", got)
	}
}
