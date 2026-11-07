package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

// contextBarrierGroup is the object group of the barrier call that
// refreshContexts sends (Runtime.releaseObjectGroup of a group that holds
// no object: no effect on the page).
const contextBarrierGroup = "iwdp-context-barrier"

// ExecutionContext is one JavaScript execution context of a target, as
// Runtime.executionContextCreated describes it (WIP
// ExecutionContextDescription).
type ExecutionContext struct {
	ID      int    `json:"id"`
	FrameID string `json:"frameId"`
	Name    string `json:"name"`
	// Type is "normal" (a frame's main world), "user" or "internal".
	Type string `json:"type"`
}

// ExecutionContexts returns the execution contexts that target reported
// and that are not gone, ordered by id. The session records them from its
// start, in the read loop, before any subscriber runs: contexts that a
// provisional target reported during its setup (SessionOptions.Setup, for
// example after Runtime.enable) are listed although no subscriber existed
// then. WIP has no event for a context that ends (Runtime.json of iOS 16,
// WebKitGTK 2.54 and WebKit main define only executionContextCreated), so
// the session drops a frame's contexts when the frame detaches
// (Page.frameDetached) or navigates (Page.frameNavigated, which WebKit
// sends before the new document's executionContextCreated), as WebKit's
// own frontend does (NetworkManager.frameDidNavigate and frameDidDetach);
// a main-frame navigation drops every context of the target. The CDP
// events Runtime.executionContextDestroyed and executionContextsCleared
// are applied too, for endpoints that send them. A destroyed target has no
// contexts, also the target that a commit replaced.
//
// A document restored from the back/forward cache comes back with its
// Page.frameNavigated but without Runtime.executionContextCreated, and the
// context ids that the session recorded for it before are no longer valid
// (WebKitGTK 2.54: "Missing injected script for given executionContextId"
// after a same-process and after a cross-process restore). After a
// main-frame navigation the session therefore checks, while the target's
// Runtime agent is enabled, whether the new document reported its context,
// and replays the contexts when it did not (refreshContexts), through
// SessionOptions.ContextReplay; without it, the inventory of a restored
// document stays empty. The
// inventory of a restored document fills shortly after its
// Page.frameNavigated (docs/notes/w1b.md, "core-events r1: contexts after
// a back/forward cache restore").
func (s *Session) ExecutionContexts(target string) []ExecutionContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Collect(maps.Values(s.contexts[target]))
	slices.SortFunc(out, func(a, b ExecutionContext) int { return a.ID - b.ID })
	return out
}

// trackContexts applies a context event, or a frame's detach, to target's
// inventory.
func (s *Session) trackContexts(target, method string, params json.RawMessage) {
	var p struct {
		Context            ExecutionContext `json:"context"`
		ExecutionContextID int              `json:"executionContextId"`
		FrameID            string           `json:"frameId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.contexts[target]
	switch method {
	case "Runtime.executionContextCreated":
		if rec == nil {
			rec = make(map[int]ExecutionContext)
			s.contexts[target] = rec
		}
		rec[p.Context.ID] = p.Context
	case "Runtime.executionContextDestroyed":
		delete(rec, p.ExecutionContextID)
	case "Runtime.executionContextsCleared":
		delete(s.contexts, target)
	case "Page.frameDetached":
		s.dropFrameContextsLocked(target, p.FrameID)
	}
}

// trackFrameNavigated drops the contexts of the frame that navigated, or
// every context of target for a main-frame navigation; a new document's
// contexts are reported after Page.frameNavigated. After a main-frame
// navigation of a target whose Runtime agent is enabled, it starts
// refreshContexts for the main frame when SessionOptions.ContextReplay is
// set.
func (s *Session) trackFrameNavigated(target string, params json.RawMessage) {
	var p struct {
		Frame struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if json.Unmarshal(params, &p) != nil || p.Frame.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Frame.ParentID != "" {
		s.dropFrameContextsLocked(target, p.Frame.ID)
		return
	}
	delete(s.contexts, target)
	t := s.targets[target]
	t.navSeq++ // handleFromTarget passes live targets only
	if s.opts.ContextReplay != nil && s.agents[target]["Runtime"] == AgentEnabled {
		go s.refreshContexts(t, p.Frame.ID, t.navSeq)
	}
}

// refreshContexts restores the inventory of a document that came back from
// the back/forward cache: such a restore sends Page.frameNavigated for the
// main frame and no Runtime.executionContextCreated, because the document
// and its JavaScript windows are reused (WebKit FrameLoader
// loadProvisionalItemFromCachedPage, ScriptCachedFrameData::restore). A
// new document reports its main-world context in the commit's own task,
// right after frameNavigated. So after a barrier call, whose reply WebKit
// sends after every event of the commit (a target answers its commands in
// order, on the thread that dispatched the commit), a main frame without a
// context is a restored document. Its contexts are then replayed:
// Runtime.enable reports every frame's contexts with valid ids only when
// the agent was disabled (PageRuntimeAgent::enable returns at once
// otherwise), so the session sends Runtime.disable and Runtime.enable in
// one batch; both only switch the agent (InspectorRuntimeAgent), and
// their replies update the AgentState record.
//
// The batch disables an agent that owners outside the session hold, so it
// runs inside SessionOptions.ContextReplay, which runs it only while an
// owner needs Runtime and never at the same time as an owner's
// Runtime.enable or Runtime.disable. Without that, the last owner's
// disable could complete between the check and the batch, whose enable
// would then leave Runtime enabled with no owner (core-events r2). The
// replay is checked before ContextReplay and again inside it, because
// waiting for the owners can take long. Nothing is sent when the target is
// gone, navigated again (seq), has the frame's context already, or its
// Runtime agent is no longer known to be enabled. A failed call leaves the
// inventory as WebKit reported it and the record as the replies say (a
// lost reply makes the agent AgentMaybeEnabled, so its next enable resets
// and replays it).
func (s *Session) refreshContexts(t *targetState, frame string, seq uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.CallTimeout)
	defer cancel()
	if _, err := s.sendTo(ctx, t.id, true, "Runtime.releaseObjectGroup", map[string]string{"objectGroup": contextBarrierGroup}); err != nil {
		return
	}
	if !s.replayNeeded(t, frame, seq) {
		return
	}
	// The wait for the owners has its own budget, and so has the batch: a
	// deadline that the wait used up must not cut the batch between its
	// disable and its enable.
	wctx, wcancel := context.WithTimeout(context.Background(), s.opts.CallTimeout)
	defer wcancel()
	_ = s.opts.ContextReplay(wctx, func(ctx context.Context) error {
		if !s.replayNeeded(t, frame, seq) {
			return nil
		}
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.CallTimeout)
		defer cancel()
		return errors.Join(s.sendPipelined(bctx, t.id, true, []Call{{Method: "Runtime.disable"}, {Method: "Runtime.enable"}})...)
	})
}

// replayNeeded reports whether frame of target t, after its navigation
// seq, still needs the context replay (refreshContexts).
func (s *Session) replayNeeded(t *targetState, frame string, seq uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets[t.id] == t && !t.destroyed && t.navSeq == seq && s.agents[t.id]["Runtime"] == AgentEnabled &&
		!slices.ContainsFunc(slices.Collect(maps.Values(s.contexts[t.id])), func(c ExecutionContext) bool { return c.FrameID == frame })
}

func (s *Session) dropFrameContextsLocked(target, frame string) {
	maps.DeleteFunc(s.contexts[target], func(_ int, c ExecutionContext) bool { return c.FrameID == frame })
}
