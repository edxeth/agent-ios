package cdp

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Execution contexts and utility worlds (design docs/design/agentic-parity.md,
// section 5.2, acceptance 7.8 item 4).
//
// Each frame of the current document has one default context. Every world
// name registered for a frame (Page.createIsolatedWorld{frameId}, or
// Page.addScriptToEvaluateOnNewDocument{worldName} for every frame) gets
// a named alias context in each document of that frame. Calls on an alias
// run in the frame's WIP main world: the worlds are not security-isolated,
// they only give clients the named context that Playwright and Puppeteer
// look for (crPage.ts lines 511-518, 732-746; FrameManager.ts lines
// 612-632).
//
// Context ids are the bridge's, not WebKit's: one counter per attachment
// that never repeats. WIP numbers contexts per target, so a WIP id of a
// new target could equal an alias id the client saw before; with the
// bridge's ids a context id is never emitted twice under different names
// (Playwright keys contexts by id). The WIP id of a frame's main world is
// kept for calls into child frames. A main-frame navigation clears all
// contexts with Runtime.executionContextsCleared, a child frame's
// navigation or detach destroys that frame's contexts, and the new
// document gets new ids.

// execContext is a CDP execution context of the current document.
type execContext struct {
	id       int
	wipFrame string // WIP frame id
	world    string // "" for the frame's default context
	wipID    int    // WIP id of the frame's main world; 0 while unknown
}

// worldState is the context part of pageState; ps.mu guards it.
type worldState struct {
	nextCtx     int
	ctxs        []*execContext      // contexts of the current document, in creation order
	worlds      []string            // world names of every frame
	frameWorlds map[string][]string // world names of one frame, by CDP frame id (stable for the main frame across documents)
}

func (w *worldState) init() { w.frameWorlds = map[string][]string{} }

func init() {
	onPageEvent("Runtime.executionContextCreated", contextCreated)
}

// contextCreated maps a WIP main world (type "normal") to the frame's
// default context, and creates the frame's aliases. Other WIP worlds
// (extensions, WebKit's own) are not exposed.
func contextCreated(ps *pageState, target string, params json.RawMessage) {
	var p struct {
		Context struct {
			ID      int    `json:"id"`
			Type    string `json:"type"`
			FrameID string `json:"frameId"`
		} `json:"context"`
	}
	if json.Unmarshal(params, &p) != nil || p.Context.Type != "normal" || p.Context.FrameID == "" || !ps.frameOfLocked(target, p.Context.FrameID) {
		return
	}
	ps.frameContextsLocked(p.Context.FrameID)
	for _, c := range ps.ctxs {
		if c.wipFrame == p.Context.FrameID && c.world == "" {
			c.wipID = p.Context.ID
		}
	}
}

// frameOfLocked reports whether the WIP frame id belongs to the current
// document and an event of target may describe it (reportedBy): WIP
// numbers frames per process, so another target's frame id can equal
// one of the document's.
func (ps *pageState) frameOfLocked(target, wipFrame string) bool {
	f := ps.frameInfoLocked(wipFrame)
	return f != nil && reportedBy(f, target)
}

// frameInfoLocked returns the frame wipFrame of the current document.
func (ps *pageState) frameInfoLocked(wipFrame string) *frameInfo {
	if ps.main != nil && wipFrame == ps.main.wip.ID {
		return ps.main
	}
	return ps.frames[wipFrame]
}

// namesLocked lists the world names of the frame wipFrame.
func (ps *pageState) namesLocked(wipFrame string) []string {
	names := slices.Clone(ps.worlds)
	for _, n := range ps.frameWorlds[ps.frameID(wipFrame)] {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// frameContextsLocked makes sure the frame has its default context and
// one alias per world name, announcing each new one.
func (ps *pageState) frameContextsLocked(wipFrame string) {
	ps.contextLocked(wipFrame, "")
	for _, n := range ps.namesLocked(wipFrame) {
		ps.contextLocked(wipFrame, n)
	}
}

// contextLocked returns the context of world in the frame wipFrame,
// creating and announcing it when the document has none yet.
func (ps *pageState) contextLocked(wipFrame, world string) *execContext {
	for _, c := range ps.ctxs {
		if c.wipFrame == wipFrame && c.world == world {
			return c
		}
	}
	ps.nextCtx++
	c := &execContext{id: ps.nextCtx, wipFrame: wipFrame, world: world}
	ps.ctxs = append(ps.ctxs, c)
	ps.announceLocked(c)
	return c
}

// registerWorldLocked adds a world name for the frame wipFrame and
// returns its alias in the current document, creating the frame's default
// context first. (Page.addScriptToEvaluateOnNewDocument{worldName} adds
// to ps.worlds, the names of every frame of new documents.)
func (ps *pageState) registerWorldLocked(wipFrame, name string) *execContext {
	if id := ps.frameID(wipFrame); !slices.Contains(ps.frameWorlds[id], name) {
		ps.frameWorlds[id] = append(ps.frameWorlds[id], name)
	}
	ps.contextLocked(wipFrame, "")
	return ps.contextLocked(wipFrame, name)
}

// addWorldLocked adds a world name for every frame of new documents
// (Page.addScriptToEvaluateOnNewDocument{worldName}); the current
// document gets no alias for it.
func (ps *pageState) addWorldLocked(name string) {
	if !slices.Contains(ps.worlds, name) {
		ps.worlds = append(ps.worlds, name)
	}
}

// framesLocked lists the WIP frame ids of the current document, the main
// frame first.
func (ps *pageState) framesLocked() []string {
	if ps.main == nil {
		return nil
	}
	out := []string{ps.main.wip.ID}
	for id := range ps.frames {
		out = append(out, id)
	}
	slices.Sort(out[1:])
	return out
}

// announceLocked sends Runtime.executionContextCreated for c.
func (ps *pageState) announceLocked(c *execContext) {
	origin := ""
	if f := ps.frameInfoLocked(c.wipFrame); f != nil {
		origin = f.wip.SecurityOrigin
	}
	aux := map[string]any{"frameId": ps.frameID(c.wipFrame), "isDefault": true, "type": "default"}
	if c.world != "" {
		aux["isDefault"], aux["type"] = false, "isolated"
	}
	ps.emitLocked(domRuntime, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{
		"id": c.id, "origin": origin, "name": c.world, "uniqueId": ps.uniqueID(c.id), "auxData": aux,
	}})
}

// uniqueID is the CDP uniqueId of the context id.
func (ps *pageState) uniqueID(id int) string { return fmt.Sprintf("%d.%s", id, ps.target) }

// announceAllLocked sends executionContextCreated for every context of
// the current document (Runtime.enable).
func (ps *pageState) announceAllLocked() {
	for _, c := range ps.ctxs {
		ps.announceLocked(c)
	}
}

// clearContextsLocked forgets the contexts of the old document and the
// world names of its child frames (the main frame keeps its own); with announce it sends
// Runtime.executionContextsCleared.
func (ps *pageState) clearContextsLocked(announce bool) {
	ps.ctxs = nil
	for f := range ps.frameWorlds {
		if f != ps.target {
			delete(ps.frameWorlds, f)
		}
	}
	if announce {
		ps.emitLocked(domRuntime, "Runtime.executionContextsCleared", map[string]any{})
	}
}

// destroyFrameContextsLocked ends the contexts of the frame wipFrame (a
// child frame navigated or detached).
func (ps *pageState) destroyFrameContextsLocked(wipFrame string) {
	ps.ctxs = slices.DeleteFunc(ps.ctxs, func(c *execContext) bool {
		if c.wipFrame != wipFrame {
			return false
		}
		ps.emitLocked(domRuntime, "Runtime.executionContextDestroyed", map[string]any{
			"executionContextId": c.id, "executionContextUniqueId": ps.uniqueID(c.id),
		})
		return true
	})
}

// lookupContextLocked returns the context id (or the context whose
// uniqueId is unique) of the current document.
func (ps *pageState) lookupContextLocked(id int, unique string) (*execContext, bool) {
	for _, c := range ps.ctxs {
		if (unique != "" && ps.uniqueID(c.id) == unique) || (unique == "" && c.id == id) {
			return c, true
		}
	}
	return nil, false
}

// mainWorldLocked returns the WIP id of the frame's main world in the
// current document, as the attachment's events reported it; 0 while
// unknown.
func (ps *pageState) mainWorldLocked(wipFrame string) int {
	for _, c := range ps.ctxs {
		if c.wipFrame == wipFrame && c.world == "" {
			return c.wipID
		}
	}
	return 0
}

// mainContextLocked returns the default context id of the main frame,
// creating it; 0 before the first document is known.
func (ps *pageState) mainContextLocked() int {
	if ps.main == nil {
		return 0
	}
	return ps.contextLocked(ps.main.wip.ID, "").id
}
