package cdp

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// The request-id map property (design 5.2 "Navigation identity", 5.3;
// acceptance 7.8 items 1 and 5): over random page histories with
// navigations, subresources, redirects, provisional swaps and their
// commits, abandoned provisional targets, requests that interception
// paused before a commit, and resets, the CDP Network stream of the
// attachment keeps these rules (checkNetIDs):
//   - no CDP request id names two requests (WIP numbers requests per
//     process, so the run reuses WIP ids across targets on purpose);
//   - the main-frame document request's id is its loader id (unless that
//     id named an earlier request: then "unique" wins), and it is the
//     loader id of the frameNavigated of its document, also when its
//     document is announced by a reset while it was held paused
//     (staging.go resetLocked keeps its loader id; W5c-1a repair round 1,
//     F3);
//   - every event of a request follows its requestWillBeSent and precedes
//     its end; Fetch.requestPaused names a request in flight;
//   - the requests in flight at the end are exactly those that the page
//     still runs: a request whose target went away, or that a reset cut
//     off, has ended for the client (unless it is paused, which the reset
//     keeps).

// netStep is one step of a random page history. Op selects the step; A
// picks a request or a variant.
type netStep struct{ Op, A uint8 }

const (
	nsNavigate    = iota // the main frame's document request, then its document
	nsSubresource        // a subresource request of the current target
	nsRedirect           // a request in flight is redirected (same WIP id)
	nsFinish             // a request in flight finishes
	nsSwap               // a provisional target loads the document (staged) and commits
	nsAbort              // a provisional target sends its document request and is destroyed
	nsPausedSwap         // interception pauses a provisional target's document request; it commits (A even) or is destroyed
	nsReset              // events were lost: a reset (staging.go eventsLostLocked)
	nsRepeatDoc          // the current WIP document reports a document request again
	netSteps
)

const maxNetSteps = 50

// netRun drives a pageState model (worldRun) with WIP Network events and
// keeps the requests that the page still runs.
type netRun struct {
	w        *worldRun
	wipNext  map[string]int  // per-target WIP request counter
	live     map[string]bool // requests the page runs, by URL key
	paused   map[string]bool // requests that interception holds
	resetDue bool            // the next commit publishes a reset (eventsLostLocked)
	redirect map[string]int  // redirects per key
}

func newNetRun(t *testing.T) *netRun {
	n := &netRun{w: newWorldRun(t), wipNext: map[string]int{}, live: map[string]bool{}, paused: map[string]bool{}, redirect: map[string]int{}}
	ps := n.w.ps
	ps.setEnabled(domNetwork, true)
	ps.mu.Lock()
	ps.fetch.on, ps.fetch.used = true, true
	ps.fetch.patterns = []fetchPattern{{rule: core.InterceptRule{URL: globRegex("*", "prop"), Stage: stageRequest, CaseSensitive: true, IsRegex: true}}}
	ps.mu.Unlock()
	return n
}

// netKeyURL is the URL of a request: it names its target and WIP id, so
// the check maps the CDP stream back to the run's requests.
func netKeyURL(target, wipID string) string { return "https://n.example/" + target + "/" + wipID }

// request sends a requestWillBeSent of target and returns its URL key.
func (n *netRun) request(target, typ, loader string) (string, string) {
	n.wipNext[target]++
	id := fmt.Sprintf("0.%d", n.wipNext[target])
	u := netKeyURL(target, id)
	raw, _ := json.Marshal(wipRequest(id, "main", loader, typ, u, nil)) // test maps
	n.w.deliver(target, "Network.requestWillBeSent", raw)
	return u, id
}

func (n *netRun) liveKeys() []string { return slices.Sorted(maps.Keys(n.live)) }

// targetGone ends the run's requests of target.
func (n *netRun) targetGone(target string) {
	for k := range n.live {
		if strings.HasPrefix(k, "https://n.example/"+target+"/") {
			delete(n.live, k)
			delete(n.paused, k)
		}
	}
}

// reset keeps only the paused requests in flight.
func (n *netRun) reset() {
	for k := range n.live {
		if !n.paused[k] {
			delete(n.live, k)
		}
	}
}

// commit commits the provisional target nt over the current one, which
// then reports the main frame (a reset's tree read reads it); the
// document request u of nt is in flight afterwards unless a reset that
// was due dropped its staged events (then only a paused one stays).
func (n *netRun) commit(nt, u string, early bool) {
	w := n.w
	old := w.target
	w.target = nt
	w.emitOuter("Target.didCommitProvisionalTarget", map[string]any{"oldTargetId": old, "newTargetId": nt})
	w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": old})
	n.targetGone(old)
	if n.resetDue {
		n.resetDue = false
		n.reset()
		if !early {
			return
		}
	}
	n.live[u] = true
}

func (n *netRun) step(s netStep) {
	w := n.w
	switch s.Op % netSteps {
	case nsNavigate:
		l := w.nextLoader()
		u, _ := n.request(w.target, "Document", l)
		n.live[u] = true
		w.mainDocument(l)
	case nsSubresource:
		u, _ := n.request(w.target, []string{"Script", "Fetch", "Image"}[int(s.A)%3], w.main)
		n.live[u] = true
	case nsRedirect, nsFinish:
		keys := slices.DeleteFunc(n.liveKeys(), func(k string) bool { return n.paused[k] })
		if len(keys) == 0 {
			return
		}
		k := keys[int(s.A)%len(keys)]
		parts := strings.Split(strings.TrimPrefix(k, "https://n.example/"), "/")
		target, id := parts[0], parts[1]
		if s.Op%netSteps == nsFinish {
			raw, _ := json.Marshal(map[string]any{"requestId": id, "timestamp": 101.0}) // test maps
			w.deliver(target, "Network.loadingFinished", raw)
			delete(n.live, k)
			return
		}
		n.redirect[k]++
		raw, _ := json.Marshal(wipRequest(id, "main", w.main, "Script", fmt.Sprintf("%s?r=%d", k, n.redirect[k]), map[string]any{ // test maps
			"redirectResponse": map[string]any{"url": k, "status": 302, "statusText": "Found", "headers": map[string]string{}, "mimeType": "", "source": "network"},
		}))
		w.deliver(target, "Network.requestWillBeSent", raw)
	case nsSwap:
		w.targets++
		nt := fmt.Sprintf("wk-%d", w.targets)
		w.provisional(nt, false)
		l := w.loaders[len(w.loaders)-1] // the same WIP loader string, another target (2.7)
		u, _ := n.request(nt, "Document", l)
		old := w.target
		w.target = nt
		w.mainDocument(l)
		w.target = old
		n.commit(nt, u, false)
	case nsAbort:
		w.targets++
		nt := fmt.Sprintf("wk-%d", w.targets)
		w.provisional(nt, false)
		n.request(nt, "Document", "abandoned")
		w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": nt})
	case nsPausedSwap:
		w.targets++
		nt := fmt.Sprintf("wk-%d", w.targets)
		w.provisional(nt, false)
		l := w.nextLoader()
		u, id := n.request(nt, "Document", l)
		raw, _ := json.Marshal(map[string]any{"requestId": id, "request": map[string]any{"url": u, "method": "GET", "headers": map[string]string{}}}) // test maps
		w.deliver(nt, evRequestIntercepted, raw)
		if s.A%2 == 1 {
			w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": nt})
			return
		}
		n.paused[u] = true
		old := w.target
		w.target = nt
		w.mainDocument(l)
		w.target = old
		n.commit(nt, u, true)
	case nsReset:
		w.forgetDocument(w.main)
		w.ps.mu.Lock()
		w.ps.eventsLostLocked()
		w.rereadLocked()
		w.ps.mu.Unlock()
		n.reset()
		n.resetDue = true
	case nsRepeatDoc:
		u, _ := n.request(w.target, "Document", w.main)
		n.live[u] = true
		w.mainDocument(w.main)
	}
}

// checkNetIDs replays the attachment's CDP events and reports the first
// violation of the rules above.
func checkNetIDs(n *netRun) error {
	type req struct {
		key   string
		ended bool
	}
	reqs := map[string]*req{}    // by CDP request id
	byKey := map[string]string{} // URL key -> CDP id
	// The last main-frame document request since the last main
	// frameNavigated, while it is in flight: that frameNavigated is its
	// document's.
	docReq, docLoader := "", ""
	for i, e := range n.w.events {
		var p struct {
			RequestID        string          `json:"requestId"`
			LoaderID         string          `json:"loaderId"`
			FrameID          string          `json:"frameId"`
			Type             string          `json:"type"`
			NetworkID        string          `json:"networkId"`
			RedirectResponse json.RawMessage `json:"redirectResponse"`
			Request          struct {
				URL string `json:"url"`
			} `json:"request"`
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
				LoaderID string `json:"loaderId"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(e.params, &p); err != nil {
			return fmt.Errorf("event %d %s: %v", i, e.method, err)
		}
		switch e.method {
		case "Network.requestWillBeSent":
			key, _, _ := strings.Cut(p.Request.URL, "?")
			if len(p.RedirectResponse) > 0 {
				if r := reqs[p.RequestID]; r == nil || r.key != key || r.ended {
					return fmt.Errorf("event %d: redirect of %s under id %s, which names %+v", i, key, p.RequestID, r)
				}
				continue
			}
			if r := reqs[p.RequestID]; r != nil {
				return fmt.Errorf("event %d: request id %s names %s and %s", i, p.RequestID, r.key, key)
			}
			if _, seen := byKey[key]; seen {
				return fmt.Errorf("event %d: request %s started twice", i, key)
			}
			if p.Type == "Document" && p.FrameID == "PAGE" {
				if p.RequestID != p.LoaderID && !slices.ContainsFunc(slices.Collect(maps.Keys(reqs)), func(id string) bool { return id == p.LoaderID }) {
					return fmt.Errorf("event %d: document request %s has id %s, want its loader id %s", i, key, p.RequestID, p.LoaderID)
				}
				docReq, docLoader = p.RequestID, p.LoaderID
			}
			reqs[p.RequestID] = &req{key: key}
			byKey[key] = p.RequestID
		case "Network.responseReceived", "Network.dataReceived", "Network.loadingFinished", "Network.loadingFailed":
			r := reqs[p.RequestID]
			if r == nil || r.ended {
				return fmt.Errorf("event %d: %s of request %s, which is %+v", i, e.method, p.RequestID, r)
			}
			r.ended = e.method != "Network.responseReceived" && e.method != "Network.dataReceived"
			if r.ended && p.RequestID == docReq {
				docReq, docLoader = "", ""
			}
		case "Fetch.requestPaused":
			if r := reqs[p.NetworkID]; r == nil || r.ended {
				return fmt.Errorf("event %d: requestPaused of request %s, which is %+v", i, p.NetworkID, r)
			}
		case "Page.frameNavigated":
			if p.Frame.ParentID != "" {
				continue
			}
			if docLoader != "" && docLoader != p.Frame.LoaderID {
				return fmt.Errorf("event %d: frameNavigated has loader %s, its document request %s", i, p.Frame.LoaderID, docLoader)
			}
			docReq, docLoader = "", ""
		}
	}
	var inFlight []string
	for _, r := range reqs {
		if !r.ended {
			inFlight = append(inFlight, r.key)
		}
	}
	slices.Sort(inFlight)
	if want := n.liveKeys(); !slices.Equal(inFlight, want) {
		return fmt.Errorf("requests in flight for the client %v, want %v", inFlight, want)
	}
	return nil
}

func TestNetworkIDProperty(t *testing.T) {
	prop := func(steps []netStep) bool {
		if len(steps) > maxNetSteps {
			steps = steps[:maxNetSteps]
		}
		n := newNetRun(t)
		for _, s := range steps {
			n.step(s)
		}
		if err := checkNetIDs(n); err != nil {
			t.Logf("steps %v: %v", steps, err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 400}); err != nil {
		t.Fatal(err)
	}
}
