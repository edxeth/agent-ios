package commands

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// W3b rebase onto the committed core (docs/notes/w3b.md, "W3b rebase onto
// the committed core"; core rules in docs/notes/w1b.md, "core-events r2"
// and "r3"): every owner's enable, disable and release, and every entry
// that a command records, is a change of the page's desired state
// (core.DesiredState.Change). Changes run one at a time, so the context
// replay's check and its Runtime.disable/enable batch never interleave
// with an owner's Runtime.enable or Runtime.disable, and a change of an
// operation that a release ended records nothing. Each W3b command that
// changes the desired state therefore waits for a change that runs.

// changeWait is how long a command must stay blocked behind a running
// change before the test counts it as waiting.
const changeWait = 150 * time.Millisecond

// holdChange starts a page-state change that runs until the returned
// function is called.
func holdChange(t *testing.T, ps *core.PageSession) func() {
	t.Helper()
	started, done, ended := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ended <- ps.State.Change(context.Background(), ps.Caller, func(webkit.Caller) error {
			close(started)
			<-done
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held change did not start")
	}
	return func() {
		close(done)
		if err := <-ended; err != nil {
			t.Errorf("the held change: %v", err)
		}
	}
}

type cliRun struct {
	code    int
	errText string
}

// runBehindChange runs args in a new operation of h's page while a change
// is held, checks that it does not end within changeWait and that blocked
// reports nothing then, ends the change, and returns the command's result.
func runBehindChange(t *testing.T, h *rtHarness, blocked func() string, args ...string) cliRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, epoch := h.ps.Sched.Lease()
	op, err := h.ps.Sched.Begin(ctx, core.OpSpec{Name: args[0], Class: core.ClassMutate, Session: session, Epoch: epoch, PageWork: true})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer op.End()
	release := holdChange(t, h.ps)
	res := make(chan cliRun, 1)
	go func() {
		var out, errb bytes.Buffer
		code := RunCLIStarted(context.Background(), append([]string{"--timeout", "5s"}, args...), &out, &errb, nil,
			func(context.Context, *Invocation) (Env, error) { return opEnv{pageEnv{h.ps}, op}, nil })

		res <- cliRun{code, errb.String()}
	}()
	select {
	case r := <-res:
		release()
		t.Fatalf("%s ended (exit %d, %q) while another page-state change ran; want it to wait for that change", strings.Join(args, " "), r.code, r.errText)
	case <-time.After(changeWait):
	}
	if why := blocked(); why != "" {
		release()
		t.Fatalf("%s while another page-state change ran: %s", strings.Join(args, " "), why)
	}
	release()
	select {
	case r := <-res:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not end after the other change ended", strings.Join(args, " "))
	}
	return cliRun{}
}

func TestW3bFrameOwnerChangesWaitForARunningChange(t *testing.T) {
	fh := newFrameHarness(t)
	h := fh.h
	fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
	owner := frameOwner("agent")
	r := runBehindChange(t, h, func() string {
		if n := ownerCalls(h, "Runtime.enable"); n != 0 || h.ps.State.Needs("Runtime") {
			return "Runtime.enable sent or Runtime owned (the frame owner's acquisition ran outside a change)"
		}
		return ""
	}, "frame", "#kid")
	if r.code != 0 || !h.ps.State.Needs("Runtime") {
		t.Fatalf("frame #kid after the change: exit %d (%s), Runtime needed %v", r.code, r.errText, h.ps.State.Needs("Runtime"))
	}
	disables := ownerCalls(h, "Runtime.disable")
	r = runBehindChange(t, h, func() string {
		if ownerCalls(h, "Runtime.disable") != disables || !slices.Contains(h.ps.State.AllOwners(), owner) {
			return "the frame owner's release ran outside a change"
		}
		return ""
	}, "frame", "main")
	if r.code != 0 || h.ps.State.Needs("Runtime") || ownerCalls(h, "Runtime.disable") != disables+1 {
		t.Fatalf("frame main after the change: exit %d (%s), Runtime needed %v, Runtime.disable %d (want %d)", r.code, r.errText, h.ps.State.Needs("Runtime"), ownerCalls(h, "Runtime.disable"), disables+1)
	}
}

// The next action's release of a hover --css state is a change; when that
// change cannot begin before the command's deadline, the hover owner
// still holds CSS and the forced state, so the action after it releases
// them (clearHover keeps its flag).
func TestHoverCSSReleaseRetriesAfterTheChangeTimedOut(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	h.mustRun(t, "hover", "#hov", "--css")
	release := holdChange(t, h.ps)
	code, _, errOut := h.run("--timeout", "300ms", "press", "Tab")
	release()
	if code == ExitOK {
		t.Fatal("press Tab succeeded although its change could not begin before its deadline")
	}
	if !h.ps.State.Needs("CSS") {
		t.Fatalf("CSS not needed after the timed-out release (stderr %q); want the hover owner to keep it", errOut)
	}
	h.mustRun(t, "press", "Tab")
	if h.ps.State.Needs("CSS") {
		t.Fatal("CSS still needed after the next action; want the kept hover state released")
	}
	want := []string{"CSS.enable", `CSS.forcePseudoState {"forcedPseudoClasses":["hover"],"nodeId":42}`, `CSS.forcePseudoState {"forcedPseudoClasses":[],"nodeId":42}`, "CSS.disable"}
	if got := cssCalls(h); !slices.Equal(got, want) {
		t.Fatalf("CSS calls %q, want %q", got, want)
	}
}

func TestW3bHoverAndHighlightWaitForARunningChange(t *testing.T) {
	h := hoverCSSHarness(t, transport.Limits{Routed: true, CSSDomain: true})
	r := runBehindChange(t, h, func() string {
		if len(cssCalls(h)) != 0 || h.ps.State.Needs("CSS") {
			return "CSS.enable sent or CSS owned (the hover owner's acquisition ran outside a change)"
		}
		return ""
	}, "hover", "#hov", "--css")
	if r.code != 0 || !h.ps.State.Needs("CSS") {
		t.Fatalf("hover --css after the change: exit %d (%s), CSS needed %v", r.code, r.errText, h.ps.State.Needs("CSS"))
	}
	r = runBehindChange(t, h, func() string {
		if got := cssCalls(h); len(got) != 2 || !h.ps.State.Needs("CSS") {
			return "the forced state was reset or CSS released outside a change (CSS calls " + strings.Join(got, ", ") + ")"
		}
		return ""
	}, "press", "Tab")
	if r.code != 0 || h.ps.State.Needs("CSS") {
		t.Fatalf("press Tab after the change: exit %d (%s), CSS needed %v", r.code, r.errText, h.ps.State.Needs("CSS"))
	}
	highlights := ownerCalls(h, "DOM.highlightNode")
	r = runBehindChange(t, h, func() string {
		if ownerCalls(h, "DOM.highlightNode") != highlights {
			return "DOM.highlightNode sent outside a change"
		}
		return ""
	}, "highlight", "--node-id", "42")
	if r.code != 0 || ownerCalls(h, "DOM.highlightNode") != highlights+1 {
		t.Fatalf("highlight after the change: exit %d (%s), DOM.highlightNode %d (want %d)", r.code, r.errText, ownerCalls(h, "DOM.highlightNode"), highlights+1)
	}
}
