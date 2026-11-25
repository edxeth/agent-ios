package commands

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W3b rebase repair round 2, finding F3 (docs/notes/w3b.md; design 2.4):
// hover --css and highlight record the cleanup of their node call before
// they send it, and keep it after an unknown outcome (the call was written
// and its reply lost): the forced hover or the highlight may be on the
// page. Only an error that proves the call had no effect rolls the record
// back: webkit.ErrNotSent (TestNodeSendAfterACommitIsRefused: the frame
// fence refuses the send) or a protocol error reply from WebKit. A refused
// later highlight keeps the entry of an earlier one, which still shows.

// The reviewer's reproduction (TestReviewHoverLostReplyKeepsItsNodeCleanup),
// assertions unchanged: another CSS owner keeps the agent enabled, so only
// the node's reset clears a forced hover whose reply was lost.
func TestHoverLostReplyKeepsItsNodeCleanup(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.ps.State.Acquire(core.SessionOwner("other", "x"), "CSS")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.sess.Send(ctx, "CSS.enable", nil); err != nil {
		t.Fatal(err)
	}
	h.f.DropReplies("CSS.forcePseudoState", 1)
	code, _, msg := h.run("--timeout", "300ms", "hover", "#hov", "--css")
	if code == ExitOK {
		t.Fatalf("lost reply succeeded %q", msg)
	}
	before := len(h.f.ReceivedMethod("CSS.forcePseudoState"))
	if before != 1 {
		t.Fatalf("force calls %d, want one delivered call whose reply was lost", before)
	}
	h.mustRun(t, "press", "Tab")
	after := len(h.f.ReceivedMethod("CSS.forcePseudoState"))
	if after == before {
		t.Fatalf("written hover lost its reply; next action sent no node cleanup while another CSS owner kept the agent enabled (entry=%v, CSS needed=%v)", h.ps.State.HasEntry("w3b:hover|agent"), h.ps.State.Needs("CSS"))
	}
}

// The reviewer's reproduction (TestReviewHighlightLostReplyKeepsCleanup),
// assertions unchanged: a highlight that the page shows but whose reply
// was lost is hidden at the release of the highlight owner.
func TestHighlightLostReplyKeepsCleanup(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	var visible atomic.Bool
	h.f.Handle("DOM.highlightNode", func(fakewebkit.Call) (any, *fakewebkit.Error) { visible.Store(true); return map[string]any{}, nil })
	h.f.Handle("DOM.hideHighlight", func(fakewebkit.Call) (any, *fakewebkit.Error) { visible.Store(false); return map[string]any{}, nil })
	h.f.DropReplies("DOM.highlightNode", 1)
	code, _, msg := h.run("--timeout", "300ms", "highlight", "--node-id", "42")
	if code == ExitOK || !visible.Load() {
		t.Fatalf("fixture code %d visible %v msg %s", code, visible.Load(), msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.ps.ReleaseOwnerChange(ctx, h.ps.Caller, core.SessionOwner("w3b-highlight", "agent")); err != nil {
		t.Fatal(err)
	}
	if visible.Load() {
		t.Fatalf("highlight executed but lost its reply; release left it visible with no cleanup entry (entry=%v)", h.ps.State.HasEntry("w3b:highlight|agent"))
	}
}

// refuseNode makes the page answer method with a protocol error.
func refuseNode(h *rtHarness, method string) {
	h.f.Handle(method, func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Missing node for given nodeId"}
	})
}

// A protocol refusal proves that WebKit processed the call and changed
// nothing, so the cleanup recorded for it is rolled back.
func TestNodeSetterProtocolRefusalRollsBackItsCleanup(t *testing.T) {
	t.Run("hover", func(t *testing.T) {
		h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
		refuseNode(h, "CSS.forcePseudoState")
		if code, _, msg := h.run("hover", "#hov", "--css"); code == ExitOK || !strings.Contains(msg, "Missing node") {
			t.Fatalf("hover --css on a refused node: exit %d, %q; want the refusal", code, msg)
		}
		if h.ps.State.HasEntry("w3b:hover|agent") {
			t.Fatal("the refused forced hover kept its reset entry")
		}
	})
	t.Run("highlight", func(t *testing.T) {
		h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
		refuseNode(h, "DOM.highlightNode")
		if code, _, msg := h.run("highlight", "--node-id", "42"); code == ExitOK || !strings.Contains(msg, "Missing node") {
			t.Fatalf("highlight on a refused node: exit %d, %q; want the refusal", code, msg)
		}
		if h.ps.State.HasEntry("w3b:highlight|agent") {
			t.Fatal("the refused highlight kept its hide entry")
		}
	})
}

// A refused later highlight keeps the entry of the earlier one, which
// still shows, and the release hides it.
func TestRefusedLaterHighlightKeepsTheEarlierEntry(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.f.Result("DOM.hideHighlight", map[string]any{})
	h.mustRun(t, "highlight", "--node-id", "42")
	refuseNode(h, "DOM.highlightNode")
	if code, _, msg := h.run("highlight", "--node-id", "43"); code == ExitOK {
		t.Fatalf("highlight on a refused node succeeded: %q", msg)
	}
	if !h.ps.State.HasEntry("w3b:highlight|agent") {
		t.Fatal("a refused later highlight removed the earlier highlight's hide entry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.ps.ReleaseOwnerChange(ctx, h.ps.Caller, core.SessionOwner("w3b-highlight", "agent")); err != nil {
		t.Fatal(err)
	}
	if n := len(h.f.ReceivedMethod("DOM.hideHighlight")); n != 1 {
		t.Fatalf("DOM.hideHighlight sent %d times at the release, want 1", n)
	}
}
