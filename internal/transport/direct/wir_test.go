package direct

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

const testUDID = "00008020-001A2B3C4D5E6F70"

// twoPageDevice returns a fake device with Safari (pages 1 and 2) and a
// proxy web content process of Safari whose page id 1 collides with Safari's.
func twoPageDevice(t *testing.T) (*pipeDialer, *fakewebkit.Server, *fakewebkit.Server) {
	t.Helper()
	a := fakewebkit.New(fakewebkit.Options{TargetID: "target-A"})
	b := fakewebkit.New(fakewebkit.Options{TargetID: "target-B"})
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	a.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": "from A"}}, nil
	})
	b.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": "from B"}}, nil
	})
	d := &pipeDialer{t: t, udid: testUDID, apps: []fakeApp{
		{id: "PID:100", bundle: "com.apple.mobilesafari", pages: []fakePage{
			{id: 1, title: "Page A", url: "https://a.example/", server: a},
			{id: 2, title: "Page B", url: "https://b.example/", server: b},
		}},
		{id: "PID:200", bundle: "com.apple.WebKit.WebContent", host: "PID:100", proxy: true, pages: []fakePage{
			{id: 1, title: "Proxy page", url: "https://c.example/"},
			{id: 9, typ: typeAutomation, title: "automation"},
		}},
	}}
	return d, a, b
}

func evalValue(t *testing.T, s *webkit.Session) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1"})
	if err != nil {
		t.Fatalf("Runtime.evaluate: %v", err)
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return r.Result.Value
}

func openSession(t *testing.T, tr *Transport, p transport.Page, o webkit.SessionOptions) *webkit.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return tr.Open(ctx, p) }, o)
	if err != nil {
		t.Fatalf("opening %s: %v", p.ID, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// waitMessages waits until the fake has read n messages with selector; a
// write returns when the bytes are in the pipe, before the fake decodes them.
func waitMessages(t *testing.T, f *fakeInspector, selector string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := f.messages(selector)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func findPage(t *testing.T, pages []transport.Page, id string) transport.Page {
	t.Helper()
	for _, p := range pages {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("page %s not in %+v", id, pages)
	return transport.Page{}
}

func listPages(t *testing.T, tr *Transport) []transport.Page {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pages, err := tr.Pages(ctx, testUDID)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	return pages
}

// Acceptance 7.4 test 1: identifier report, application list, listing with
// revision counter, socket setup, data routed by WIRDestinationKey to two
// concurrent pages, _rpc_forwardDidClose: on close.
func TestWIRSessionTwoPages(t *testing.T) {
	d, srvA, srvB := twoPageDevice(t)
	tr := newWithDialer(d, Options{ListingWait: 3 * time.Second})
	t.Cleanup(func() { _ = tr.Close() })

	pages := listPages(t, tr)
	fake := d.fake(0)
	ids := fake.messages(selReportIdentifier)
	if len(ids) != 1 || ids[0][keyConnectionID] == "" {
		t.Fatalf("_rpc_reportIdentifier: messages = %v, want one with a connection id", ids)
	}
	connID := ids[0][keyConnectionID]
	if n := len(fake.messages(selGetConnectedApps)); n != 1 {
		t.Fatalf("application list requested %d times, want 1", n)
	}
	for _, sel := range []string{selGetConnectedApps, selForwardGetListing} {
		for _, m := range fake.messages(sel) {
			if m[keyConnectionID] != connID {
				t.Fatalf("%s carries connection id %v, want %v", sel, m[keyConnectionID], connID)
			}
		}
	}
	want := map[string]transport.Page{
		"PID:100/1": {DeviceID: testUDID, ID: "PID:100/1", Title: "Page A", URL: "https://a.example/", AppID: "com.apple.mobilesafari", Debuggable: true},
		"PID:100/2": {DeviceID: testUDID, ID: "PID:100/2", Title: "Page B", URL: "https://b.example/", AppID: "com.apple.mobilesafari", Debuggable: true},
		"PID:200/1": {DeviceID: testUDID, ID: "PID:200/1", Title: "Proxy page", URL: "https://c.example/", AppID: "com.apple.mobilesafari", Debuggable: true},
	}
	if len(pages) != len(want) {
		t.Fatalf("pages = %+v, want the 3 non-automation pages", pages)
	}
	for _, p := range pages {
		if p != want[p.ID] {
			t.Fatalf("page %+v, want %+v", p, want[p.ID])
		}
	}

	// Listing revision: a later listing replaces the earlier one, and Pages
	// waits for an app that answers late instead of sleeping a fixed time.
	fake.setPages("PID:100", []fakePage{{id: 1, title: "Page A2", url: "https://a.example/2", server: srvA}, {id: 2, title: "Page B", url: "https://b.example/", server: srvB}})
	fake.mu.Lock()
	fake.listingHold["PID:100"] = 300 * time.Millisecond
	fake.mu.Unlock()
	start := time.Now()
	pages = listPages(t, tr)
	if got := findPage(t, pages, "PID:100/1").Title; got != "Page A2" {
		t.Fatalf("title after relisting = %q, want Page A2 (the late listing)", got)
	}
	if el := time.Since(start); el < 300*time.Millisecond || el > 2*time.Second {
		t.Fatalf("Pages took %s, want the 300 ms of the late listing, not the 3 s wait budget", el)
	}

	// Two concurrent pages on one device connection.
	sa := openSession(t, tr, findPage(t, pages, "PID:100/1"), webkit.SessionOptions{})
	sb := openSession(t, tr, findPage(t, pages, "PID:100/2"), webkit.SessionOptions{})
	if sa.ActiveTarget() != "target-A" || sb.ActiveTarget() != "target-B" {
		t.Fatalf("targets = %q, %q; want target-A, target-B", sa.ActiveTarget(), sb.ActiveTarget())
	}
	var wg sync.WaitGroup
	results := make([][2]string, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = [2]string{evalValue(t, sa), evalValue(t, sb)}
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r != [2]string{"from A", "from B"} {
			t.Fatalf("results = %v, want each reply routed to its own page", r)
		}
	}
	if d.dials != 1 {
		t.Fatalf("device dialed %d times, want one shared connection", d.dials)
	}

	setups := fake.messages(selForwardSocketSetup)
	if len(setups) != 2 {
		t.Fatalf("socket setups = %v, want 2", setups)
	}
	senders := map[string]bool{}
	for _, s := range setups {
		chunk, _ := plistInt(s[keyChunkSupported])
		if s[keyAppID] != "PID:100" || chunk != 0 || s[keyAutomaticallyPause] != false || s[keyConnectionID] != connID {
			t.Fatalf("socket setup %v: want app PID:100, chunking 0, no auto-pause, our connection id", s)
		}
		senders[s[keySender].(string)] = true
	}
	if len(senders) != 2 {
		t.Fatalf("sender keys %v, want one per page connection", senders)
	}

	// Close sends _rpc_forwardDidClose: for that page only.
	if err := sa.Close(); err != nil {
		t.Fatalf("closing page A: %v", err)
	}
	closes := waitMessages(t, fake, selForwardDidClose, 1)
	if len(closes) != 1 {
		t.Fatalf("_rpc_forwardDidClose: messages = %v, want 1", closes)
	}
	pid, _ := plistInt(closes[0][keyPageID])
	if closes[0][keyAppID] != "PID:100" || pid != 1 || !senders[closes[0][keySender].(string)] {
		t.Fatalf("_rpc_forwardDidClose: %v, want app PID:100 page 1 with its sender key", closes[0])
	}
	if got := evalValue(t, sb); got != "from B" {
		t.Fatalf("page B after closing A: %q", got)
	}
	// The fake may answer a message before it reads the empty frame after it.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		fake.mu.Lock()
		msgs, empty := len(fake.received), fake.emptyFrames
		fake.mu.Unlock()
		if empty == msgs {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d empty frames after %d messages, want one after each", empty, msgs)
		}
	}
}

// Acceptance 7.4 test 1: an application disconnect ends its page
// connections with ErrPageClosed; other apps' pages stay open.
func TestWIRApplicationDisconnectEndsItsPages(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	srv := fakewebkit.New(fakewebkit.Options{TargetID: "target-C"})
	t.Cleanup(srv.Close)
	d.apps = append(d.apps, fakeApp{id: "PID:300", bundle: "com.example.app", pages: []fakePage{{id: 4, title: "Other app", server: srv}}})
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	pages := listPages(t, tr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca, err := tr.Open(ctx, findPage(t, pages, "PID:100/1"))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := tr.Open(ctx, findPage(t, pages, "PID:300/4"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []transport.MessageConn{ca, cc} {
		if _, err := c.Read(ctx); err != nil { // Target.targetCreated
			t.Fatalf("first message: %v", err)
		}
	}
	d.fake(0).disconnectApp("PID:100")
	if _, err := ca.Read(ctx); !errors.Is(err, webkit.ErrPageClosed) {
		t.Fatalf("Read after the app disconnected: %v, want ErrPageClosed", err)
	}
	if err := ca.Write(ctx, []byte(`{"id":1,"method":"Runtime.evaluate"}`)); !errors.Is(err, webkit.ErrPageClosed) {
		t.Fatalf("Write after the app disconnected: %v, want ErrPageClosed", err)
	}
	// The other app's page still works.
	msg := `{"id":7,"method":"Target.sendMessageToTarget","params":{"targetId":"target-C","message":"{\"id\":1,\"method\":\"Runtime.evaluate\",\"params\":{}}"}}`
	if err := cc.Write(ctx, []byte(msg)); err != nil {
		t.Fatalf("other app's page: %v", err)
	}
	if _, err := cc.Read(ctx); err != nil {
		t.Fatalf("other app's page reply: %v", err)
	}
	if _, err := tr.Open(ctx, transport.Page{DeviceID: testUDID, ID: "PID:100/2"}); err == nil {
		t.Fatal("Open on a disconnected app succeeded")
	}
}
