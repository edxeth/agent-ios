package cdp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/dialogpolicy"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The Page domain (design docs/design/agentic-parity.md, sections 5.2 and
// 5.3; acceptance 7.8 items 5 and 6). Frame ids, loader ids and the
// document generations come from the attachment's pageState (events.go).

func init() {
	register(
		Method{Name: "Page.enable", Levels: OnPage, Handle: pageEnable},
		Method{Name: "Page.disable", Levels: OnPage, Handle: pageDisable},
		Method{Name: "Page.getFrameTree", Levels: OnPage, Handle: getFrameTree},
		Method{Name: "Page.getResourceTree", Levels: OnPage, Handle: getResourceTree},
		Method{Name: "Page.navigate", Levels: OnPage, Handle: pageNavigate},
		Method{Name: "Page.reload", Levels: OnPage, Handle: pageReload},
		Method{Name: "Page.getNavigationHistory", Levels: OnPage, Handle: getNavigationHistory},
		Method{Name: "Page.navigateToHistoryEntry", Levels: OnPage, Handle: navigateToHistoryEntry},
		Method{Name: "Page.captureScreenshot", Levels: OnPage, Handle: captureScreenshot},
		Method{Name: "Page.getLayoutMetrics", Levels: OnPage, Handle: getLayoutMetrics},
		Method{Name: "Page.addScriptToEvaluateOnNewDocument", Levels: OnPage, Handle: addScriptToEvaluateOnNewDocument},
		Method{Name: "Page.removeScriptToEvaluateOnNewDocument", Levels: OnPage, Handle: removeScriptToEvaluateOnNewDocument},
		Method{Name: "Page.createIsolatedWorld", Levels: OnPage, Handle: createIsolatedWorld},
		Method{Name: "Page.setLifecycleEventsEnabled", Levels: OnPage, Handle: setLifecycleEventsEnabled},
		Method{Name: "Page.handleJavaScriptDialog", Levels: OnPage, Handle: handleJavaScriptDialog},
		Method{Name: "Page.bringToFront", Levels: OnPage, Handle: bringToFront},
		Method{Name: "Page.stopLoading", Levels: OnPage, Handle: stopLoading},
		Method{Name: "Page.setBypassCSP", Levels: OnPage, Handle: setBypassCSP},
	)
	registerEvents("Page", "frameNavigated", "navigatedWithinDocument", "domContentEventFired", "loadEventFired",
		"lifecycleEvent", "frameAttached", "frameDetached", "frameStartedLoading", "frameStoppedLoading")
	onPageEvent("Page.frameNavigated", frameNavigated)
	onPageEvent("Page.frameAttached", frameAttached)
	onPageEvent("Page.frameDetached", frameDetached)
	onPageEvent("Page.frameStartedLoading", frameLoading("Page.frameStartedLoading"))
	onPageEvent("Page.frameStoppedLoading", frameLoading("Page.frameStoppedLoading"))
	onPageEvent("Page.domContentEventFired", documentEvent("Page.domContentEventFired", "DOMContentLoaded"))
	onPageEvent("Page.loadEventFired", documentEvent("Page.loadEventFired", "load"))
	onPageEvent("Page.loadEventFired", func(ps *pageState, target string, _ json.RawMessage) {
		if ps.main != nil && reportedBy(ps.main, target) {
			ps.networkIdleLocked()
		}
	})
}

// Errors of the design (5.2, 5.3).
var (
	errDialogs   = serverError("dynamic dialog handling is not supported on iOS WebKit; pre-arm answers with agent-ios dialog accept|dismiss")
	errBypassCSP = serverError("bypassing the Content Security Policy is not supported on iOS WebKit")
	errNoFrame   = serverError("No frame for given id found")
)

// navigateWait bounds the wait of Page.navigate for the new document (or
// the same-document navigation) after location.href was set.
const navigateWait = 30 * time.Second

// ---- helpers for the page domains ----

// send sends one WIP call of the attachment in an operation under its
// lease (design 2.10).
func (s *session) send(ctx context.Context, class core.Class, method string, params any) (json.RawMessage, error) {
	var res json.RawMessage
	err := s.do(ctx, method, class, func(ctx context.Context, mo *core.ManagedOp) error {
		var err error
		res, err = mo.Op.Caller().Send(ctx, method, params)
		return err
	})
	return res, err
}

// owned sends method with the attachment as its desired-state owner
// (ownedCall), inside the operation mo.
func (s *session) owned(ctx context.Context, mo *core.ManagedOp, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s: encoding params: %w", method, err)
	}
	_, err = s.c.s.o.Manager.RawAs(ctx, mo, s.owner, method, raw)
	return err
}

// evaluateValue evaluates expr in the main world of the main frame and
// returns its value; a thrown value is an error.
func evaluateValue(ctx context.Context, c webkit.Caller, expr string) (json.RawMessage, error) {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding the WIP Runtime.evaluate reply: %w", err)
	}
	if r.WasThrown {
		return nil, fmt.Errorf("the page threw %s", r.Result.Description)
	}
	return r.Result.Value, nil
}

// evalInto evaluates expr (see evaluateValue) in an operation of s and decodes
// its value into out.
func (s *session) evalInto(ctx context.Context, name, expr string, out any) error {
	return s.do(ctx, name, core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		v, err := evaluateValue(ctx, mo.Op.Caller(), expr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := json.Unmarshal(v, out); err != nil {
			return fmt.Errorf("%s: the page returned %s: %w", name, v, err)
		}
		return nil
	})
}

// activeTarget is the WIP target that receives the page's commands.
func activeTarget(pg *core.Page) string {
	if at, ok := pg.PS.Caller.(interface{ ActiveTarget() string }); ok {
		return at.ActiveTarget()
	}
	return ""
}

// readTree reads the WIP resource tree in mo and seeds the attachment's
// document from it (pageState.seedLocked) at the place of the reply in the
// event stream (staging.go, "Tree reads"): it returns after the attachment
// applied the tree there. A tree read during a reset (staging.go: a change
// of connection, lost events) is not seeded; the reset's own read seeds
// the document. The attachment can then have no document (ps.main is
// nil), so callers read the document with documentLocked under the lock of
// its use.
func (ps *pageState) readTree(ctx context.Context, mo *core.ManagedOp) (wipTree, error) {
	rd := ps.startTreeRead()
	raw, err := mo.Op.Caller().Send(core.WithReplyPosition(ctx, rd.replied), "Page.getResourceTree", nil)
	if err != nil {
		return wipTree{}, err
	}
	var r struct {
		FrameTree wipTree `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return wipTree{}, fmt.Errorf("decoding the WIP Page.getResourceTree reply: %w", err)
	}
	if err := rd.applied(ctx); err != nil {
		return wipTree{}, err
	}
	return r.FrameTree, nil
}

// treeRead is one tree read of the attachment: its reset count (connSeq)
// at the start, and the place of its reply in the event queue.
type treeRead struct {
	ps  *pageState
	seq uint64
	at  atomic.Uint64 // eventQueue.push of the reply; 0 until the reply is read
}

func (ps *pageState) startTreeRead() *treeRead {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return &treeRead{ps: ps, seq: ps.connSeq}
}

// replied runs in the read loop when the connection read the reply
// (core.WithReplyPosition): it queues the reply at its position, as the
// goroutine's item for this read (pageState.treeLocked).
func (rd *treeRead) replied(pos core.EventPosition, target string, result json.RawMessage) {
	ev := eventAt(pos, target, evTreeReply, result)
	ev.tree = rd
	n, _ := rd.ps.q.push(ev)
	rd.at.Store(n)
}

// applied waits until the attachment has handled the reply's item, so
// the tree was applied at its place or not at all. The connection read
// the reply before the call returned it, so its place is known then.
func (rd *treeRead) applied(ctx context.Context) error {
	return rd.ps.caughtUpTo(ctx, rd.at.Load())
}

// documentLocked returns the main frame of the attachment's document.
// After a reset it has none until a tree read seeds the new one
// (readTree); method then fails with a transient error.
func (ps *pageState) documentLocked(method string) (*frameInfo, error) {
	if ps.main == nil {
		return nil, serverError("%s: the page's document is being read again after a reconnect or lost events; retry the command", method)
	}
	return ps.main, nil
}

// refresh reads the resource tree in its own operation (readTree).
func (ps *pageState) refresh(ctx context.Context) (tree wipTree, err error) {
	err = ps.s.do(ctx, "Page.getResourceTree", core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		tree, err = ps.readTree(ctx, mo)
		return err
	})
	return tree, err
}

// ---- the attachment's bootstrap part ----

// initScript is a Page.addScriptToEvaluateOnNewDocument script.
type initScript struct {
	id, source string
}

// bootSourceLocked is the attachment's bootstrap part: the navigation
// hook and the bindings of 66-cdp-page.js while Page events are on or a
// binding exists, then the client's scripts, each run as its own script
// by indirect eval so that its var and function declarations are global
// and an error does not stop the next one.
func (ps *pageState) bootSourceLocked() string {
	var b strings.Builder
	if ps.enabled[domPage] {
		b.WriteString(`window.__iwdp && window.__iwdp.call("cdpNav", {});` + "\n")
	}
	if len(ps.bindings) > 0 {
		args, _ := json.Marshal(map[string]any{"add": ps.bindingListLocked()}) // strings only
		fmt.Fprintf(&b, "window.__iwdp && window.__iwdp.call(\"cdpBind\", %s);\n", args)
	}
	for _, sc := range ps.scripts {
		src, _ := json.Marshal(sc.source) // a string
		fmt.Fprintf(&b, "try { (0, eval)(%s); } catch (e) {}\n", src)
	}
	return b.String()
}

// bindingListLocked lists the bindings as the runtime function cdpBind
// takes them.
func (ps *pageState) bindingListLocked() []map[string]string {
	names := make([]string, 0, len(ps.bindings))
	for n := range ps.bindings {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]map[string]string, len(names))
	for i, n := range names {
		out[i] = map[string]string{"n": n, "k": ps.bindings[n]}
	}
	return out
}

// installBoot sends the attachment's bootstrap part (an empty part
// removes it) as the attachment's owned state, so that its detach removes
// it (core legacyCall, Page.setBootstrapScript).
func (ps *pageState) installBoot(ctx context.Context, mo *core.ManagedOp) error {
	ps.mu.Lock()
	src := ps.bootSourceLocked()
	ps.mu.Unlock()
	return ps.s.owned(ctx, mo, "Page.setBootstrapScript", map[string]string{"source": src})
}

// runtimeCall runs a function of the agent runtime in the current
// document (installing the runtime when it is missing).
func runtimeCall(ctx context.Context, mo *core.ManagedOp, fn string, args any) error {
	return mo.Page.PS.Runtime.Via(mo.Op.Caller()).Call(ctx, fn, args, nil)
}

// ---- dialogs ----

// ensureDialogPolicy makes the logical session's dialog policy answer
// dialogs while a CDP client drives the page (5.2: otherwise a native
// dialog would block the page). A session that has its override keeps
// it; otherwise the default policy is installed as the session's own
// desired state (dialogpolicy.Install, as the dialog command does: the
// dialog policy belongs to the logical session, 5.2, so a detach keeps it
// and the session's release removes it) and in the current document.
func (s *session) ensureDialogPolicy(ctx context.Context, mo *core.ManagedOp) error {
	st := s.pg.PS.State
	if slices.Contains(st.Boot.Names(), dialogpolicy.PartName) {
		return nil
	}
	if err := dialogpolicy.Install(ctx, st, mo.Op.Caller(), s.c.rec.session, dialogpolicy.Default); err != nil {
		return err
	}
	return runtimeCall(ctx, mo, "cdpDialogs", map[string]string{"policy": dialogpolicy.Default})
}

// handleJavaScriptDialog: dialogs are answered by the session's policy;
// no answer of a CDP client is kept for a later dialog (5.2).
func handleJavaScriptDialog(context.Context, *Call) (any, Later, error) {
	return nil, nil, errDialogs
}

func setBypassCSP(context.Context, *Call) (any, Later, error) {
	return nil, nil, errBypassCSP
}

// ---- enable, frames ----

// pageEnable turns on the attachment's Page events: Page and Console (for
// the hooks' messages) are owned by the attachment, the navigation hook
// is part of its bootstrap script and runs in the current document, and
// the session's dialog policy is installed.
func pageEnable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	ps := s.page
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := s.owned(ctx, mo, "Page.enable", nil); err != nil {
			return err
		}
		if err := s.owned(ctx, mo, "Console.enable", nil); err != nil {
			return err
		}
		if _, err := ps.readTree(ctx, mo); err != nil {
			return err
		}
		ps.setEnabled(domPage, true)
		if err := ps.installBoot(ctx, mo); err != nil {
			return err
		}
		if err := runtimeCall(ctx, mo, "cdpNav", nil); err != nil {
			return err
		}
		return s.ensureDialogPolicy(ctx, mo)
	})
	if err != nil {
		ps.setEnabled(domPage, false)
		return nil, nil, err
	}
	return nil, nil, nil
}

// pageDisable stops the attachment's Page events and gives up its Page
// ownership; the navigation hook stays in documents that have it.
func pageDisable(ctx context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	ps.setEnabled(domPage, false)
	err := call.session.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := ps.installBoot(ctx, mo); err != nil {
			return err
		}
		return call.session.owned(ctx, mo, "Page.disable", nil)
	})
	return nil, nil, err
}

// frameTreeLocked is the CDP FrameTree of the current document; with
// resources, the WIP resources of tree are added per frame.
func (ps *pageState) frameTreeLocked(tree *wipTree) map[string]any {
	res := map[string][]json.RawMessage{}
	var walk func(ts []wipTree)
	if tree != nil {
		walk = func(ts []wipTree) {
			for _, t := range ts {
				res[t.Frame.ID] = t.Resources
				walk(t.ChildFrames)
			}
		}
		walk([]wipTree{*tree})
	}
	var node func(f *frameInfo) map[string]any
	node = func(f *frameInfo) map[string]any {
		n := map[string]any{"frame": ps.cdpFrame(f)}
		var kids []any
		for _, id := range ps.framesLocked()[1:] {
			if c := ps.frames[id]; c.wip.ParentID == f.wip.ID {
				kids = append(kids, node(c))
			}
		}
		if len(kids) > 0 {
			n["childFrames"] = kids
		}
		if tree != nil {
			n["resources"] = cdpResources(res[f.wip.ID])
		}
		return n
	}
	return node(ps.main)
}

// cdpResources converts WIP FrameResources (url, type, mimeType, failed,
// canceled) to CDP; both use the same resource type names.
func cdpResources(rs []json.RawMessage) []map[string]any {
	out := []map[string]any{}
	for _, raw := range rs {
		var r struct {
			URL      string `json:"url"`
			Type     string `json:"type"`
			MimeType string `json:"mimeType"`
			Failed   bool   `json:"failed"`
			Canceled bool   `json:"canceled"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		e := map[string]any{"url": r.URL, "type": r.Type, "mimeType": r.MimeType}
		if r.Failed {
			e["failed"] = true
		}
		if r.Canceled {
			e["canceled"] = true
		}
		out = append(out, e)
	}
	return out
}

func getFrameTree(ctx context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	if _, err := ps.refresh(ctx); err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, err := ps.documentLocked(call.Method); err != nil {
		return nil, nil, err
	}
	return map[string]any{"frameTree": ps.frameTreeLocked(nil)}, nil, nil
}

func getResourceTree(ctx context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	tree, err := ps.refresh(ctx)
	if err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, err := ps.documentLocked(call.Method); err != nil {
		return nil, nil, err
	}
	return map[string]any{"frameTree": ps.frameTreeLocked(&tree)}, nil, nil
}

// ---- navigation ----

// sameDocumentURL reports whether navigating from cur to next stays in
// the document: next has a fragment and differs from cur only in it.
func sameDocumentURL(cur, next string) bool {
	c, err1 := url.Parse(cur)
	n, err2 := url.Parse(next)
	if err1 != nil || err2 != nil || !strings.Contains(next, "#") {
		return false
	}
	c.Fragment, c.RawFragment, n.Fragment, n.RawFragment = "", "", "", ""
	return c.String() == n.String()
}

// pageNavigate sets location.href in the main frame and waits until the
// bridge sees the new document (or, for a fragment change, the
// same-document navigation). It returns the main frame id and the new
// document's loader id; a same-document navigation returns no loader id
// (5.2 "Navigation identity").
func pageNavigate(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		URL     string `json:"url"`
		FrameID string `json:"frameId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	ps := s.page
	if p.FrameID != "" && p.FrameID != s.targetID() {
		return nil, nil, serverError("Page.navigate of a child frame is not supported on iOS WebKit; navigate the main frame")
	}
	if u, err := url.Parse(p.URL); err != nil || u.Scheme == "" {
		return nil, nil, serverError("Cannot navigate to invalid URL")
	}
	if _, err := ps.refresh(ctx); err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	main, err := ps.documentLocked(call.Method)
	var cur string
	if err == nil {
		cur = main.wip.URL
	}
	ps.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	same := sameDocumentURL(cur, p.URL)
	frame := map[string]any{"frameId": s.targetID()}
	seq, changed := ps.navState()
	expr := fmt.Sprintf("location.href = %s", jsString(p.URL))
	if _, err := s.send(ctx, core.ClassMutate, "Runtime.evaluate", map[string]any{"expression": expr}); err != nil {
		return nil, nil, err
	}
	if same && cur == p.URL {
		return frame, nil, nil // no hashchange for the same URL: nothing to wait for
	}
	return nil, func(ctx context.Context) (any, error) {
		wctx, cancel := context.WithTimeout(ctx, navigateWait)
		defer cancel()
		for {
			select {
			case <-changed:
			case <-wctx.Done():
				return nil, serverError("Page.navigate to %s: the page did not navigate within %v; it may have refused the navigation or failed to load", p.URL, navigateWait)
			}
			ps.mu.Lock()
			n, loader := ps.navSeq, ps.navLoader
			changed = ps.navChanged
			ps.mu.Unlock()
			if n <= seq {
				continue
			}
			seq = n
			switch {
			case same && loader == "":
				return frame, nil
			case !same && loader != "":
				return map[string]any{"frameId": s.targetID(), "loaderId": loader}, nil
			}
		}
	}, nil
}

// jsString is s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s) // a string always encodes
	return string(b)
}

func pageReload(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		IgnoreCache            bool   `json:"ignoreCache"`
		ScriptToEvaluateOnLoad string `json:"scriptToEvaluateOnLoad"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.ScriptToEvaluateOnLoad != "" {
		return nil, nil, serverError("Page.reload: scriptToEvaluateOnLoad is not supported on iOS WebKit; use Page.addScriptToEvaluateOnNewDocument")
	}
	_, err := call.session.send(ctx, core.ClassMutate, "Page.reload", map[string]any{"ignoreCache": p.IgnoreCache})
	return nil, nil, err
}

// getNavigationHistory lists only the current entry (WIP and the page
// cannot read the session history); its id is 0.
func getNavigationHistory(ctx context.Context, call *Call) (any, Later, error) {
	var v [2]string
	if err := call.session.evalInto(ctx, call.Method, "[location.href, document.title]", &v); err != nil {
		return nil, nil, err
	}
	return map[string]any{"currentIndex": 0, "entries": []any{map[string]any{
		"id": 0, "url": v[0], "userTypedURL": v[0], "title": v[1], "transitionType": "typed",
	}}}, nil, nil
}

// navigateToHistoryEntry runs history.go(entryId): entry ids are offsets
// from the current entry, the only listed one (id 0).
func navigateToHistoryEntry(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		EntryID int `json:"entryId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	_, err := call.session.send(ctx, core.ClassMutate, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("history.go(%d)", p.EntryID)})
	return nil, nil, err
}

func stopLoading(ctx context.Context, call *Call) (any, Later, error) {
	_, err := call.session.send(ctx, core.ClassMutate, "Runtime.evaluate", map[string]any{"expression": "window.stop()"})
	return nil, nil, err
}

// bringToFront is a probe: WIP cannot raise a tab, and only the visible
// Safari tab answers (design 3.8).
func bringToFront(ctx context.Context, call *Call) (any, Later, error) {
	pctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	if _, err := call.session.send(pctx, core.ClassRead, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		if errors.Is(err, core.ErrLeased) {
			return nil, nil, err
		}
		return nil, nil, serverError("tab %q is not answering; it is probably not the visible Safari tab. Bring it to the front on the device; WIP cannot raise a tab (%v)", call.session.t.title, err)
	}
	return nil, nil, nil
}

// ---- screenshots and metrics ----

// defaultJPEGQuality is Chrome's default for format jpeg.
const defaultJPEGQuality = 80

// viewportExpr reads the layout and visual viewport and the content size.
const viewportExpr = `(function () {
  var d = document.documentElement, v = window.visualViewport || {};
  return {iw: innerWidth, ih: innerHeight, sx: scrollX, sy: scrollY, cw: d.clientWidth, ch: d.clientHeight,
    w: Math.max(d.scrollWidth, document.body ? document.body.scrollWidth : 0),
    h: Math.max(d.scrollHeight, document.body ? document.body.scrollHeight : 0),
    vx: v.offsetLeft || 0, vy: v.offsetTop || 0, vpx: v.pageLeft === undefined ? scrollX : v.pageLeft,
    vpy: v.pageTop === undefined ? scrollY : v.pageTop, vw: v.width || innerWidth, vh: v.height || innerHeight, sc: v.scale || 1};
})()`

type viewport struct {
	IW, IH, SX, SY, CW, CH, W, H, VX, VY, VPX, VPY, VW, VH, SC float64
}

func (v *viewport) UnmarshalJSON(b []byte) error {
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*v = viewport{m["iw"], m["ih"], m["sx"], m["sy"], m["cw"], m["ch"], m["w"], m["h"], m["vx"], m["vy"], m["vpx"], m["vpy"], m["vw"], m["vh"], m["sc"]}
	return nil
}

func getLayoutMetrics(ctx context.Context, call *Call) (any, Later, error) {
	var v viewport
	if err := call.session.evalInto(ctx, call.Method, viewportExpr, &v); err != nil {
		return nil, nil, err
	}
	layout := map[string]any{"pageX": v.SX, "pageY": v.SY, "clientWidth": v.CW, "clientHeight": v.CH}
	visual := map[string]any{"offsetX": v.VX, "offsetY": v.VY, "pageX": v.VPX, "pageY": v.VPY, "clientWidth": v.VW, "clientHeight": v.VH, "scale": v.SC, "zoom": 1}
	content := map[string]any{"x": 0, "y": 0, "width": v.W, "height": v.H}
	return map[string]any{
		"layoutViewport": layout, "visualViewport": visual, "contentSize": content,
		"cssLayoutViewport": layout, "cssVisualViewport": visual, "cssContentSize": content,
	}, nil, nil
}

// captureScreenshot uses WIP Page.snapshotRect: the viewport without a
// clip; a clip (CDP page coordinates) in WebKit's Page coordinates with
// captureBeyondViewport, else in Viewport coordinates after subtracting
// the scroll offset. WebKit returns PNG; jpeg is encoded on the host.
// clip.scale is not applied: the image has the device's pixel ratio.
func captureScreenshot(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Format  string `json:"format"`
		Quality *int   `json:"quality"`
		Clip    *struct {
			X, Y, Width, Height float64
		} `json:"clip"`
		CaptureBeyondViewport bool `json:"captureBeyondViewport"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Format != "" && p.Format != "png" && p.Format != "jpeg" {
		return nil, nil, serverError("Page.captureScreenshot: format %q is not supported on iOS WebKit; use png or jpeg", p.Format)
	}
	s := call.session
	var data string
	err := s.do(ctx, call.Method, core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		raw, err := evaluateValue(ctx, mo.Op.Caller(), viewportExpr)
		if err != nil {
			return fmt.Errorf("reading the viewport: %w", err)
		}
		var v viewport
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("reading the viewport: %w", err)
		}
		rect := map[string]any{"x": 0, "y": 0, "width": int(v.IW), "height": int(v.IH), "coordinateSystem": "Viewport"}
		if c := p.Clip; c != nil {
			x, y := c.X, c.Y
			cs := "Page"
			if !p.CaptureBeyondViewport {
				x, y, cs = x-v.SX, y-v.SY, "Viewport"
			}
			rect = map[string]any{"x": int(math.Floor(x)), "y": int(math.Floor(y)), "width": int(math.Ceil(c.Width)), "height": int(math.Ceil(c.Height)), "coordinateSystem": cs}
		}
		if rect["width"].(int) <= 0 || rect["height"].(int) <= 0 {
			return serverError("Page.captureScreenshot: the area %vx%v is empty", rect["width"], rect["height"])
		}
		res, err := mo.Op.Caller().Send(ctx, "Page.snapshotRect", rect)
		if err != nil {
			return err
		}
		var snap struct {
			DataURL string `json:"dataURL"`
		}
		if err := json.Unmarshal(res, &snap); err != nil {
			return fmt.Errorf("decoding the WIP Page.snapshotRect reply: %w", err)
		}
		_, b64, ok := strings.Cut(snap.DataURL, ",")
		if !ok {
			return fmt.Errorf("Page.snapshotRect returned no data URL")
		}
		data = b64
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if p.Format == "jpeg" {
		q := defaultJPEGQuality
		if p.Quality != nil {
			q = min(max(*p.Quality, 0), 100)
		}
		if data, err = pngToJPEG(data, q); err != nil {
			return nil, nil, err
		}
	}
	return map[string]any{"data": data}, nil, nil
}

// pngToJPEG re-encodes a base64 PNG as a base64 JPEG of quality q.
func pngToJPEG(b64 string, q int) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decoding the screenshot: %w", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decoding the screenshot PNG: %w", err)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: max(q, 1)}); err != nil {
		return "", fmt.Errorf("encoding the screenshot as JPEG: %w", err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// ---- scripts and worlds ----

// addScriptToEvaluateOnNewDocument adds source to the attachment's
// bootstrap part (it runs in the main world of new documents; a
// worldName does not isolate it) and registers worldName for every frame
// of new documents (worlds.go).
func addScriptToEvaluateOnNewDocument(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Source         string `json:"source"`
		WorldName      string `json:"worldName"`
		RunImmediately bool   `json:"runImmediately"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	ps := s.page
	ps.mu.Lock()
	if p.WorldName != "" {
		ps.addWorldLocked(p.WorldName)
	}
	ps.nextScript++
	id := strconv.Itoa(ps.nextScript)
	if p.Source != "" {
		ps.scripts = append(ps.scripts, initScript{id: id, source: p.Source})
	}
	ps.mu.Unlock()
	if p.Source == "" {
		return map[string]any{"identifier": id}, nil, nil
	}
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := ps.installBoot(ctx, mo); err != nil {
			if wl := wireLimit(err); wl != nil {
				return ps.refuseScript(ctx, mo, id, len(p.Source), wl)
			}
			return err
		}
		if p.RunImmediately {
			_, err := mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": p.Source})
			return err
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"identifier": id}, nil, nil
}

// refuseScript removes the new-document script id of size bytes, which
// made the page's bootstrap script larger than the transport sends (wl),
// and installs the attachment's part again without it: core keeps a part
// whose send failed, and every later send of the bootstrap script would
// fail too. Unlike Runtime.evaluate (large_source.go), such a script
// cannot be staged in the page: it must be there before the page's
// scripts of a new document run, and a new document starts without the
// staged parts.
func (ps *pageState) refuseScript(ctx context.Context, mo *core.ManagedOp, id string, size int, wl *webkit.WireLimitError) error {
	ps.mu.Lock()
	ps.scripts = slices.DeleteFunc(ps.scripts, func(sc initScript) bool { return sc.id == id })
	ps.mu.Unlock()
	msg := fmt.Sprintf("Page.addScriptToEvaluateOnNewDocument: with this %d-byte source the page's bootstrap script is %d bytes on the wire, "+
		"above this page connection's limit of %d bytes. A new-document script cannot be staged in the page as a large Runtime.evaluate is, "+
		"because a new document starts without the staged parts; use a smaller script", size, wl.Size, wl.Limit)
	if err := ps.installBoot(ctx, mo); err != nil {
		return fmt.Errorf("%s. Installing the bootstrap script again without it failed: %w", msg, err)
	}
	return serverError("%s", msg)
}

func removeScriptToEvaluateOnNewDocument(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Identifier string `json:"identifier"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	ps.mu.Lock()
	n := len(ps.scripts)
	ps.scripts = slices.DeleteFunc(ps.scripts, func(sc initScript) bool { return sc.id == p.Identifier })
	removed := len(ps.scripts) < n
	ps.mu.Unlock()
	if !removed {
		return nil, nil, nil
	}
	err := call.session.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return ps.installBoot(ctx, mo)
	})
	return nil, nil, err
}

// createIsolatedWorld registers worldName for the frame and returns its
// alias in the current document; the alias's executionContextCreated
// goes out before the reply (P2 item 6).
func createIsolatedWorld(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		FrameID   string `json:"frameId"`
		WorldName string `json:"worldName"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	if _, err := ps.refresh(ctx); err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	wipFrame := p.FrameID
	if p.FrameID == ps.target {
		main, err := ps.documentLocked(call.Method)
		if err != nil {
			return nil, nil, err
		}
		wipFrame = main.wip.ID
	} else if ps.frames[p.FrameID] == nil {
		return nil, nil, errNoFrame
	}
	c := ps.registerWorldLocked(wipFrame, p.WorldName)
	return map[string]any{"executionContextId": c.id}, nil, nil
}

// setLifecycleEventsEnabled turns lifecycle events on or off; turning
// them on reports the lifecycle of the current document so far, as
// Chrome does.
func setLifecycleEventsEnabled(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Enabled bool `json:"enabled"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	if !p.Enabled {
		ps.setEnabled(domLifecycle, false)
		return nil, nil, nil
	}
	if err := ps.startNetlog(ctx); err != nil {
		return nil, nil, err
	}
	if _, err := ps.refresh(ctx); err != nil {
		return nil, nil, err
	}
	var state string
	if err := call.session.evalInto(ctx, call.Method, "document.readyState", &state); err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.enabled[domLifecycle] = true
	ps.lifecycleLocked("init")
	if state != "loading" {
		ps.lifecycleLocked("DOMContentLoaded")
	}
	if state == "complete" {
		ps.lifecycleLocked("load")
		ps.networkIdleLocked()
	}
	return nil, nil, nil
}

// startNetlog gives the attachment the session's network log, starting it
// when it does not run (devtools.StartNetlog: owner "netlog:<session>",
// session state like the log of "wait --load networkidle", so a detach
// keeps it and the session's release stops it).
func (ps *pageState) startNetlog(ctx context.Context) error {
	ps.mu.Lock()
	have := ps.netlog != nil
	ps.mu.Unlock()
	if have {
		return nil
	}
	s := ps.s
	return s.do(ctx, "Page.setLifecycleEventsEnabled", core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		p, err := devtools.For(s.pg.PS)
		if err != nil {
			return fmt.Errorf("tracking network activity for networkIdle: %w", err)
		}
		n, _, err := p.StartNetlog(ctx, mo.Op.Caller(), s.c.rec.session)
		if err != nil {
			return fmt.Errorf("starting the network log for networkIdle: %w", err)
		}
		ps.mu.Lock()
		ps.netlog = n
		ps.mu.Unlock()
		return nil
	})
}

// networkIdleLocked reports networkAlmostIdle and networkIdle of the
// current document once it has loaded and the session's network log
// (W4's tracker, the one of "wait --load networkidle") has had no request
// in flight for devtools.DefaultIdleQuiet (Chrome's networkIdle: no
// request for 500 ms). networkAlmostIdle (Chrome: at most two requests)
// comes with networkIdle, because the log counts only idleness. A
// document that another replaces before it is idle gets neither. Without
// a log (lifecycle events were never on) nothing is reported.
func (ps *pageState) networkIdleLocked() {
	if ps.netlog == nil || ps.main == nil {
		return
	}
	gen := ps.gen
	switch gen {
	case ps.idleReached:
		ps.lifecycleLocked("networkAlmostIdle")
		ps.lifecycleLocked("networkIdle")
		return
	case ps.idleWait:
		return // its wait runs
	}
	ps.idleWait = gen
	n, life := ps.netlog, ps.life
	go func() {
		// WaitIdle fails only when life ends: the attachment is gone.
		if _, err := n.WaitIdle(life, devtools.DefaultIdleQuiet); err != nil {
			return
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.gen != gen {
			return
		}
		ps.idleReached = gen
		ps.lifecycleLocked("networkAlmostIdle")
		ps.lifecycleLocked("networkIdle")
	}()
}

// ---- event translators ----

// frameNavigated: a main frame of another document starts a generation
// (events.go); a child frame's navigation ends its contexts. A child frame
// that the attachment does not know yet is announced with frameAttached
// first, as a tree read announces one (seedLocked): WebKit sends no
// frameAttached for the retained child frames of a document restored from
// the back/forward cache.
func frameNavigated(ps *pageState, target string, params json.RawMessage) {
	var p struct {
		Frame wipFrame `json:"frame"`
	}
	if json.Unmarshal(params, &p) != nil || p.Frame.ID == "" {
		return
	}
	f := p.Frame
	if f.ParentID == "" {
		if ps.sameDocument(target, f) {
			ps.main.wip = f
			return
		}
		ps.newDocumentLocked(target, f, true)
		return
	}
	if !ps.frameOfLocked(target, f.ParentID) {
		return
	}
	ps.destroyFrameContextsLocked(f.ID)
	if ps.frames[f.ID] == nil {
		ps.emitLocked(domPage, "Page.frameAttached", map[string]any{"frameId": f.ID, "parentFrameId": ps.frameID(f.ParentID)})
	}
	fi := &frameInfo{wip: f, target: target, loader: ps.loaderFor(target, f.LoaderID)}
	ps.frames[f.ID] = fi
	ps.emitLocked(domPage, "Page.frameNavigated", map[string]any{"frame": ps.cdpFrame(fi), "type": "Navigation"})
}

func frameAttached(ps *pageState, target string, params json.RawMessage) {
	var p struct {
		FrameID       string `json:"frameId"`
		ParentFrameID string `json:"parentFrameId"`
	}
	if json.Unmarshal(params, &p) != nil || p.FrameID == "" || !ps.frameOfLocked(target, p.ParentFrameID) {
		return
	}
	if ps.frames[p.FrameID] == nil {
		ps.frames[p.FrameID] = &frameInfo{wip: wipFrame{ID: p.FrameID, ParentID: p.ParentFrameID}, target: target}
	}
	ps.emitLocked(domPage, "Page.frameAttached", map[string]any{"frameId": p.FrameID, "parentFrameId": ps.frameID(p.ParentFrameID)})
}

func frameDetached(ps *pageState, target string, params json.RawMessage) {
	var p struct {
		FrameID string `json:"frameId"`
	}
	if json.Unmarshal(params, &p) != nil || ps.frames[p.FrameID] == nil || !reportedBy(ps.frames[p.FrameID], target) {
		return
	}
	ps.removeFrameLocked(p.FrameID)
}

// removeFrameLocked removes the child frame id with its contexts and
// world names, and sends Page.frameDetached.
func (ps *pageState) removeFrameLocked(id string) {
	ps.destroyFrameContextsLocked(id)
	delete(ps.frames, id)
	delete(ps.frameWorlds, id)
	ps.emitLocked(domPage, "Page.frameDetached", map[string]any{"frameId": id, "reason": "remove"})
}

func frameLoading(method string) pageEventFn {
	return func(ps *pageState, target string, params json.RawMessage) {
		var p struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(params, &p) != nil || !ps.frameOfLocked(target, p.FrameID) {
			return
		}
		ps.emitLocked(domPage, method, map[string]any{"frameId": ps.frameID(p.FrameID)})
	}
}

// documentEvent forwards a main-document event and reports the lifecycle
// events it implies.
func documentEvent(method string, lifecycle ...string) pageEventFn {
	return func(ps *pageState, target string, params json.RawMessage) {
		if ps.main == nil || !reportedBy(ps.main, target) {
			return
		}
		var p struct {
			Timestamp *float64 `json:"timestamp"`
		}
		_ = json.Unmarshal(params, &p)
		ts := monotonic()
		if p.Timestamp != nil {
			ts = *p.Timestamp
		}
		ps.emitLocked(domPage, method, map[string]any{"timestamp": ts})
		for _, n := range lifecycle {
			ps.lifecycleLocked(n)
		}
	}
}

// sameDocumentLocked reports a same-document navigation of the main
// frame (the hook of 66-cdp-page.js): no new loader, no cleared contexts.
func (ps *pageState) sameDocumentLocked(target, u, kind string) {
	if ps.main == nil || !reportedBy(ps.main, target) {
		return
	}
	ps.main.wip.URL = u
	if kind != "fragment" && kind != "historyApi" {
		kind = "other"
	}
	ps.emitLocked(domPage, "Page.navigatedWithinDocument", map[string]any{"frameId": ps.target, "url": u, "navigationType": kind})
	ps.noteNavigationLocked("")
}
