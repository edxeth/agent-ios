package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"time"
	"weak"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/refs"
	"github.com/nnemirovsky/iwdp-mcp/internal/runtime"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// frame: the session's frame scope (design docs/design/agentic-parity.md,
// section 3.8; agent-browser "frame"). "frame <iframe>" makes the frame's
// execution context the scope of the session's later commands on this
// page: the runtime is installed and called there (core.Op
// SetExecutionContext), so snapshot, get, is, find, wait and the actions
// work in the frame's document, same-origin or cross-origin. The scope
// belongs to the session, its lease epoch and the top document: a new
// lease or a navigation of the top document ends it, and "frame main"
// returns to the top document. Navigation commands and screenshot always
// work on the top document.
//
// WIP names a frame's execution context only in Runtime.executionContextCreated
// (Runtime.enable reports every existing context). The page's scheduler
// finds the newest main-world context of each frame id in its
// connection's inventory, bound to the target that reported it
// (core.Scheduler.FrameContext, core.ExecContext): the session's commands
// in the frame send to that target only, and fail when a navigation or a
// reconnect replaced it. A frame that navigates gets a new context under
// the same frame id. The frame command holds the Runtime
// domain for the session as an owner in the desired state (design 2.4), so
// the reports keep coming; it never disables Runtime under another owner.
// The runtime state that the session leaves in frame contexts is released
// with the lease (core.Scheduler.ReleaseExecutionContexts), or at the next
// owner's first runtime command when that release did not reach it
// (releaseEarlierFrames). The iframe element becomes a frame id through its index in
// window.frames, which is the order of the frame's children in
// Page.getResourceTree. A frame without a context in this page's target
// (iOS 26 and later can put cross-origin frames in their own targets) is
// not supported (design 3.8, 4.3).
func init() {
	Register(&Command{
		Name: "frame", MCPName: "select_frame", Profile: ProfileCore,
		Summary: "Make an iframe (@eN, CSS or locator) the frame that this session's later commands work in, or return to the top document with main.",
		Args:    []Arg{{Name: "target", Help: "main, or the iframe: @eN ref, CSS selector or locator (in the current frame)", Required: true}},
		Page:    LeasedPage,
		Mutates: true, // it changes the session's scope and the Runtime domain
		Output:  OutputText,
		Run:     runFrame,
	})
}

// frameContextWait bounds the wait for the execution context of a frame
// after Runtime.enable: WebKit reports the contexts before its reply, and
// the handler can run just after the reply arrives.
const frameContextWait = time.Second

// frameScope is the frame one session selected on one page record.
type frameScope struct {
	epoch   uint64 // lease epoch of the selection
	gen     uint64 // top document generation of the selection
	frameID string
	target  string // as the user gave it, for messages
}

// frameContexts holds the frame scopes of one page record.
type frameContexts struct {
	mu     sync.Mutex
	scopes map[string]frameScope // by session
}

var (
	framesMu sync.Mutex
	frames   = map[weak.Pointer[refs.Table]]*frameContexts{}
)

func dropFrames(k weak.Pointer[refs.Table]) {
	framesMu.Lock()
	defer framesMu.Unlock()
	delete(frames, k)
}

// framesOf returns the frame bookkeeping of ps's page record; create
// makes it when it is missing.
func framesOf(ps *core.PageSession, create bool) *frameContexts {
	framesMu.Lock()
	defer framesMu.Unlock()
	k := weak.Make(ps.Refs)
	f := frames[k]
	if f == nil && create {
		f = &frameContexts{scopes: map[string]frameScope{}}
		frames[k] = f
		if ps.Refs != nil {
			goruntime.AddCleanup(ps.Refs, dropFrames, k)
		}
	}
	return f
}

func frameOwner(session string) core.Owner { return core.SessionOwner("frame", session) }

// errFrameGone: the selected frame has no execution context now.
var errFrameGone = errors.New("the selected frame has no execution context now; run 'frame <target>' again, or 'frame main'")

// applyFrameScope sets op's execution context to the session's frame
// scope on ps, when it has one for the current lease epoch and top
// document; a scope of an earlier epoch or document is dropped. It
// returns the scope's frame id ("" for the top document).
func applyFrameScope(ps *core.PageSession, op *core.Op, gen uint64) (string, error) {
	f := framesOf(ps, false)
	if f == nil || ps.Sched == nil || op == nil {
		return "", nil
	}
	session, epoch := ps.Sched.Lease()
	f.mu.Lock()
	sc, ok := f.scopes[session]
	if ok && (sc.epoch != epoch || sc.gen != gen) {
		delete(f.scopes, session)
		ok = false
	}
	f.mu.Unlock()
	if !ok {
		return "", nil
	}
	ec := ps.Sched.FrameContext(sc.frameID)
	if ec.ID == 0 {
		return "", fmt.Errorf("frame %s: %w", sc.target, errFrameGone)
	}
	op.SetExecutionContext(ec)
	return sc.frameID, nil
}

// leaveFrame makes the command work on the top document and, with end,
// ends the session's frame scope (a navigation of the top document).
func leaveFrame(ps *core.PageSession, op *core.Op, end bool) {
	if op != nil {
		op.SetExecutionContext(core.ExecContext{})
	}
	if f := framesOf(ps, false); f != nil && end && ps.Sched != nil {
		session, _ := ps.Sched.Lease()
		f.mu.Lock()
		delete(f.scopes, session)
		f.mu.Unlock()
	}
}

// frameInfo is what the page reports about a frame element.
type frameInfo struct {
	Tag    string `json:"tag"`
	Src    string `json:"src"`
	Index  int    `json:"index"`  // in window.frames; -1: not found
	Frames int    `json:"frames"` // window.frames.length
}

// frameExpr resolves a target with the runtime's resolver (pick) in the
// current scope and finds its window among window.frames.
const frameExpr = `(function(){var r=window.__iwdp.call("pick",%s);if(!r.ok){throw new Error(r.error.code+": "+r.error.message)}var e=r.value,t=e.localName,i=-1;if(t==="iframe"||t==="frame"){for(var k=0;k<window.frames.length;k++){if(window.frames[k]===e.contentWindow){i=k;break}}}return {tag:t,src:e.getAttribute("src")||"",index:i,frames:window.frames.length}})()`

// frameNode is one frame of Page.getResourceTree.
type frameNode struct {
	Frame struct {
		ID string `json:"id"`
	} `json:"frame"`
	ChildFrames []frameNode `json:"childFrames"`
}

func (n *frameNode) find(id string) *frameNode {
	if id == "" || n.Frame.ID == id {
		return n
	}
	for i := range n.ChildFrames {
		if f := n.ChildFrames[i].find(id); f != nil {
			return f
		}
	}
	return nil
}

func runFrame(ctx context.Context, env Env, in Input) (Result, error) {
	t := arg(in, 0)
	ps, err := env.Page(ctx)
	if err != nil {
		return Result{}, err
	}
	var op *core.Op
	if oe, ok := env.(interface{ Op() *core.Op }); ok {
		op = oe.Op()
	}
	if op == nil || ps.Sched == nil || ps.State == nil {
		return Result{}, errNoScheduler
	}
	session, epoch := ps.Sched.Lease()
	if t == "main" {
		leaveFrame(ps, op, true)
		// One change of the desired state (holdRuntime). A refused
		// Runtime.disable stays pending after the owner left: Runtime is
		// unknown until a reset succeeds, and the next holdRuntime resets
		// it first (core.PageSession.ReleaseOwnerChange). When the change
		// cannot begin, the owner keeps Runtime for the next "frame main"
		// or the lease release.
		if _, err := ps.ReleaseOwnerChange(ctx, op.Caller(), frameOwner(session)); err != nil {
			return Result{}, fmt.Errorf("frame main: releasing the Runtime domain: %w", err)
		}
		return Result{Text: "commands run in the main document", Data: map[string]any{"frame": "main"}}, nil
	}
	p, err := openRuntime(ctx, env) // in the current frame scope
	if err != nil {
		return Result{}, err
	}
	parent := ""
	if f := framesOf(ps, false); f != nil {
		f.mu.Lock()
		parent = f.scopes[session].frameID
		f.mu.Unlock()
	}
	spec, err := p.target(ctx, t)
	if err != nil {
		return Result{}, err
	}
	// The runtime is installed and the owner confirmed by this ping.
	if err := p.rt.Call(ctx, "ping", nil, nil); err != nil {
		return Result{}, err
	}
	args, err := json.Marshal(map[string]any{"t": spec})
	if err != nil {
		return Result{}, err
	}
	c := op.Caller()
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(frameExpr, args), "returnByValue": true})
	if err != nil {
		return Result{}, err
	}
	info, err := evalValue[frameInfo](raw)
	if err != nil {
		return Result{}, fmt.Errorf("frame %s: %w", t, err)
	}
	if info.Tag != "iframe" && info.Tag != "frame" {
		return Result{}, usagef("frame %s: the target is a <%s>, not an iframe", t, info.Tag)
	}
	if info.Index < 0 {
		return Result{}, fmt.Errorf("frame %s: the iframe has no window (it is not in the document, or has no content yet)", t)
	}
	f := framesOf(ps, true)
	if err := holdRuntime(ctx, ps, session, c); err != nil {
		return Result{}, err
	}
	raw, err = c.Send(ctx, "Page.getResourceTree", nil)
	if err != nil {
		return Result{}, fmt.Errorf("frame %s: %w", t, err)
	}
	var tree struct {
		FrameTree frameNode `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return Result{}, fmt.Errorf("frame %s: decoding Page.getResourceTree: %w", t, err)
	}
	at := tree.FrameTree.find(parent)
	if at == nil || len(at.ChildFrames) != info.Frames {
		return Result{}, &runtime.Error{Code: "unsupported", Message: fmt.Sprintf("frame %s: the page lists %d frames here and the protocol another number; a frame in its own target (iOS 26 and later) or inside a shadow root is not supported (design 3.8, 4.3)", t, info.Frames)}
	}
	frameID := at.ChildFrames[info.Index].Frame.ID
	var ec core.ExecContext
	for end := time.Now().Add(frameContextWait); ; sleepUntil(ctx, time.Now().Add(20*time.Millisecond)) {
		if ec = ps.Sched.FrameContext(frameID); ec.ID != 0 || time.Now().After(end) || ctx.Err() != nil {
			break
		}
	}
	if ec.ID == 0 {
		return Result{}, &runtime.Error{Code: "unsupported", Message: fmt.Sprintf("frame %s (%s): the page reports no execution context for this frame; a cross-origin frame in its own target (iOS 26 and later) is not supported (design 3.8, 4.3). Open its URL with 'open' to work in it", t, info.Src)}
	}
	op.SetExecutionContext(ec)
	if err := p.rt.Call(ctx, "ping", nil, nil); err != nil {
		return Result{}, fmt.Errorf("frame %s: installing the runtime in the frame: %w", t, err)
	}
	f.mu.Lock()
	f.scopes[session] = frameScope{epoch: epoch, gen: p.rt.Generation(), frameID: frameID, target: t}
	f.mu.Unlock()
	return Result{
		Text: fmt.Sprintf("frame %s (%s): later commands of session %q work in this frame until 'frame main' or a navigation", t, info.Src, session),
		Data: map[string]any{"frame": t, "src": info.Src, "frameId": frameID},
	}, nil
}

// holdRuntime makes the session's frame owner hold the Runtime domain in
// the desired state (design 2.4). Only the first owner sends Runtime.enable;
// its context reports reach the scheduler's inventory before the reply.
// When another owner holds Runtime already, the inventory has every
// context since that owner's enable, and nothing is sent. A Runtime.enable
// that was not sent or that the page refused gives the ownership back; one
// with an unknown outcome (a lost reply) keeps it, so "frame main" or the
// release disables the domain when no other owner holds it. The
// acquisition is one change of the desired state
// (core.DesiredState.Change): the context replay checks whether Runtime
// is owned and sends its Runtime.disable and Runtime.enable inside a
// change, so no owner's enable or disable may run between the two, and an
// operation that a release ended acquires nothing. A Runtime cleanup that
// an earlier release left pending (a refused Runtime.disable of "frame
// main") is reset first, in the same change, while no owner holds Runtime
// (core.ResetPendingRuntime).
func holdRuntime(ctx context.Context, ps *core.PageSession, session string, c webkit.Caller) error {
	return ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if err := core.ResetPendingRuntime(ctx, ps, c); err != nil {
			return fmt.Errorf("frame: %w", err)
		}
		if !ps.State.Acquire(frameOwner(session), "Runtime") {
			return nil
		}
		_, err := c.Send(ctx, "Runtime.enable", nil)
		if err == nil {
			return nil
		}
		var pe *webkit.ErrorData
		if errors.Is(err, webkit.ErrNotSent) || errors.As(err, &pe) {
			ps.State.ReleaseDomain(frameOwner(session), "Runtime")
		}
		return fmt.Errorf("frame: Runtime.enable: %w", err)
	})
}

// frameReleaseBudget bounds the release of one earlier lease epoch's frame
// contexts at an owner change (releaseEarlierFrames), as the manager's
// runtime release of a lease is bounded (core runtimeReleaseBudget).
const frameReleaseBudget = time.Second

// releaseEarlierFrames is the owner-change half of the page-side release
// (design 2.4, 3.1) for frame contexts: before the first runtime call of
// op's lease epoch, it releases the runtime state that earlier epochs left
// in frame contexts because their lease release did not reach them (a
// takeover after a lost connection, a release without time left or with a
// failed call). The main context needs no such step: the page resets it
// at the new owner's first call. A frame whose context the new connection
// has not reported yet ("frame main" left no Runtime owner before a
// reconnect) is reached through a temporary Runtime ownership of op's
// session (core.PageSession.ReleaseFrameContexts). A failure is logged,
// and the records whose attempt reached the page's current target and
// failed or got no answer are forgotten, so a context that never answers
// does not cost every later command its budget
// (core.Scheduler.ForgetFailedExecutionContexts). Every other record stays
// for the next owner-change command or the lease release, which finds the
// frame on the current target: a send that core refused because the
// target or the connection that reported its context was replaced, a
// budget that ended before the send, and a frame without a context in an
// incomplete inventory sent nothing to the page.
func releaseEarlierFrames(ctx context.Context, env Env, ps *core.PageSession, op *core.Op) {
	if ps.Sched == nil {
		return
	}
	for _, e := range ps.Sched.ContextEpochsBefore(op.Spec.Epoch) {
		cctx, cancel := context.WithTimeout(ctx, frameReleaseBudget)
		err := ps.ReleaseFrameContexts(cctx, e, op.Spec.Session, op.Caller(), func(ctx context.Context, c webkit.Caller) error {
			return ps.Runtime.Release(ctx, c, e)
		})
		cancel()
		if err != nil {
			ps.Sched.ForgetFailedExecutionContexts(e)
			if m, merr := managerOf(env); merr == nil {
				m.Logf("frame: page-side runtime release of lease epoch %d in frames at an owner change: %v", e, err)
			}
		}
	}
}
