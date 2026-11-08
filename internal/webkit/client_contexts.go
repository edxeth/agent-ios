package webkit

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"
)

// clientContexts is the execution-context inventory of a Client, per
// target ("" on a connection without Target routing). The read loop
// records each event before any handler runs, from the connection's start,
// so a frame's context that WebKit reported before a subscriber existed is
// listed. It applies the same events as Session.ExecutionContexts: a
// created context is added, the CDP events Runtime.executionContextDestroyed
// and executionContextsCleared remove, and a frame's detach or navigation
// drops its contexts (a main-frame navigation drops every context of the
// target), because WIP reports no context end. Unlike a Session, a Client
// does not replay the contexts of a document restored from the
// back/forward cache, and knows no agent states.
type clientContexts struct {
	mu       sync.Mutex
	byTarget map[string]map[int]ExecutionContext
}

// track applies the event method of target to the inventory.
func (c *clientContexts) track(target, method string, params json.RawMessage) {
	switch method {
	case "Runtime.executionContextCreated", "Runtime.executionContextDestroyed",
		"Runtime.executionContextsCleared", "Page.frameDetached", "Page.frameNavigated":
	default:
		return
	}
	var p struct {
		Context            ExecutionContext `json:"context"`
		ExecutionContextID int              `json:"executionContextId"`
		FrameID            string           `json:"frameId"`
		Frame              struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.byTarget[target]
	switch method {
	case "Runtime.executionContextCreated":
		if c.byTarget == nil {
			c.byTarget = map[string]map[int]ExecutionContext{}
		}
		if rec == nil {
			rec = map[int]ExecutionContext{}
			c.byTarget[target] = rec
		}
		rec[p.Context.ID] = p.Context
	case "Runtime.executionContextDestroyed":
		delete(rec, p.ExecutionContextID)
	case "Runtime.executionContextsCleared":
		delete(c.byTarget, target)
	case "Page.frameDetached":
		maps.DeleteFunc(rec, func(_ int, e ExecutionContext) bool { return e.FrameID == p.FrameID })
	case "Page.frameNavigated":
		if p.Frame.ParentID == "" {
			delete(c.byTarget, target)
			return
		}
		maps.DeleteFunc(rec, func(_ int, e ExecutionContext) bool { return e.FrameID == p.Frame.ID })
	}
}

// drop forgets the contexts of target.
func (c *clientContexts) drop(target string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byTarget, target)
}

// list returns the contexts of target, ordered by id.
func (c *clientContexts) list(target string) []ExecutionContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := slices.Collect(maps.Values(c.byTarget[target]))
	slices.SortFunc(out, func(a, b ExecutionContext) int { return a.ID - b.ID })
	return out
}

// ContextInventory returns the target that the client sends its commands
// to ("" without Target routing) and the execution contexts that this
// target reported and that are not gone, ordered by id (clientContexts).
func (c *Client) ContextInventory() (target string, contexts []ExecutionContext) {
	c.mu.Lock()
	target = c.targetID
	c.mu.Unlock()
	return target, c.contexts.list(target)
}
