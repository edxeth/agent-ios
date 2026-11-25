package cdp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// Page.disable and Runtime.disable (design 5.3; W5b-1 review round 1,
// F3): disabling stops the attachment's events of the domain, enabling
// again resumes them, and another attachment that owns the same WIP
// domain keeps it (the WIP domain is turned off only with its last
// owner; WIP Page also stays on for the session's dialog policy, a part
// of the bootstrap script, design 5.2).

// secondPage attaches page One a second time, on a new connection of the
// same session s1, as another owner.
func secondPage(t *testing.T, r *rig) (*cdptest.Client, string) {
	t.Helper()
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, sid := puppeteerAttach(t, c)
	return c, sid
}

// syncLog emits a console message that the Log domain reports and waits
// for its Log.entryAdded on sid: the attachment has then translated every
// earlier event of the page.
func syncLog(t *testing.T, r *rig, c *cdptest.Client, after int, sid string) {
	t.Helper()
	r.fake["One"].Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "other", "level": "log", "type": "log", "text": "sync-log",
	}})
	waitEvent(t, c, after, sid, "Log.entryAdded", func(p json.RawMessage) bool { return strings.Contains(string(p), "sync-log") })
}

func TestPageDisable(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	c2, sid2 := secondPage(t, r)
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Page.enable", nil)
	call(t, c2, sid2, "Page.enable", nil)

	mark := len(f.ReceivedMethod("Page.disable")) // the agent reset before the first enable
	call(t, c, sid, "Page.disable", nil)
	if got := f.ReceivedMethod("Page.disable")[mark:]; len(got) != 0 {
		t.Fatalf("Page.disable of one attachment turned off WIP Page of the other: %v", got)
	}
	seq, seq2 := c.Seq(), c2.Seq()
	fp.navigate("https://one.example/two")
	waitEvent(t, c2, seq2, sid2, "Page.frameNavigated", nil)
	syncLog(t, r, c, seq, sid)
	if got := sessionEvents(c, seq, sid, "Page.frameNavigated"); len(got) != 0 {
		t.Fatalf("Page events after Page.disable: %v", got)
	}

	call(t, c, sid, "Page.enable", nil)
	seq = c.Seq()
	fp.navigate("https://one.example/three")
	if nav := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params).Frame; nav.URL != "https://one.example/three" {
		t.Fatalf("frameNavigated after enabling again %+v", nav)
	}
}

func TestRuntimeDisable(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	c2, sid2 := secondPage(t, r)
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	call(t, c2, sid2, "Runtime.enable", nil)

	mark := len(f.ReceivedMethod("Runtime.disable")) // the agent reset before the first enable
	call(t, c, sid, "Runtime.disable", nil)
	if got := f.ReceivedMethod("Runtime.disable")[mark:]; len(got) != 0 {
		t.Fatalf("Runtime.disable of one attachment turned off WIP Runtime of the other: %v", got)
	}
	seq, seq2 := c.Seq(), c2.Seq()
	fp.navigate("https://one.example/two")
	fp.f.Emit("Console.messageAdded", consoleFrom("after-disable"))
	waitEvent(t, c2, seq2, sid2, "Runtime.executionContextCreated", nil)
	waitEvent(t, c2, seq2, sid2, "Runtime.consoleAPICalled", func(p json.RawMessage) bool { return strings.Contains(string(p), "after-disable") })
	syncLog(t, r, c, seq, sid)
	for _, m := range []string{"Runtime.executionContextsCleared", "Runtime.executionContextCreated", "Runtime.consoleAPICalled"} {
		if got := sessionEvents(c, seq, sid, m); len(got) != 0 {
			t.Fatalf("%s after Runtime.disable: %v", m, got)
		}
	}

	seq = c.Seq()
	call(t, c, sid, "Runtime.enable", nil)
	if ctxs := contextsOf(t, c, seq, sid); len(ctxs) != 1 || !ctxs[0].AuxData.IsDefault {
		t.Fatalf("contexts after enabling again %+v, want the current document's default context", ctxs)
	}
	fp.f.Emit("Console.messageAdded", consoleFrom("after-enable"))
	waitEvent(t, c, seq, sid, "Runtime.consoleAPICalled", func(p json.RawMessage) bool { return strings.Contains(string(p), "after-enable") })

	call(t, c, sid, "Runtime.disable", nil)
	call(t, c2, sid2, "Runtime.disable", nil)
	if got := f.ReceivedMethod("Runtime.disable")[mark:]; len(got) != 1 {
		t.Fatalf("WIP Runtime.disable sent %d times after both attachments disabled Runtime, want once", len(got))
	}
}

// Page.frameStartedLoading and frameStoppedLoading (design 5.3): the main
// frame is named by the page's target id, a child frame by its WIP id;
// the events go to the attachment that enabled Page, on its session, and
// a frame of no current document is not reported.
func TestFrameLoadingEvents(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	f := r.fake["One"]
	fp.mu.Lock()
	fp.children = []wipTree{{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/child"}}}
	fp.mu.Unlock()
	c2, sid2 := secondPage(t, r)
	call(t, c, sid, "Page.enable", nil)
	call(t, c2, sid2, "Runtime.enable", nil) // the other attachment knows the frames, without Page events
	call(t, c2, sid2, "Log.enable", nil)

	seq, seq2 := c.Seq(), c2.Seq()
	for _, m := range []string{"Page.frameStartedLoading", "Page.frameStoppedLoading"} {
		for _, fr := range []string{"main", "child", "elsewhere"} {
			f.Emit(m, map[string]any{"frameId": fr})
		}
	}
	syncLog(t, r, c2, seq2, sid2)
	waitEvent(t, c, seq, sid, "Page.frameStoppedLoading", func(p json.RawMessage) bool { return strings.Contains(string(p), `"child"`) })
	var got []string
	for _, e := range c.Events() {
		if e.Seq > seq && (e.Method == "Page.frameStartedLoading" || e.Method == "Page.frameStoppedLoading") {
			if e.SessionID != sid {
				t.Fatalf("%s on session %q, want %q", e.Method, e.SessionID, sid)
			}
			got = append(got, e.Method+" "+decode[struct{ FrameID string }](t, e.Params).FrameID)
		}
	}
	want := []string{
		"Page.frameStartedLoading " + target, "Page.frameStartedLoading child",
		"Page.frameStoppedLoading " + target, "Page.frameStoppedLoading child",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("loading events %v, want %v", got, want)
	}
	for _, m := range []string{"Page.frameStartedLoading", "Page.frameStoppedLoading"} {
		if got := sessionEvents(c2, seq2, sid2, m); len(got) != 0 {
			t.Fatalf("%s on the attachment without Page events: %v", m, got)
		}
	}
}
