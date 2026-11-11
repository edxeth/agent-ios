package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// pauseOnStartConns returns the connections on which the fake received
// Target.setPauseOnStart with pauseOnStart true, in order.
func pauseOnStartConns(t *testing.T, f *fakewebkit.Server) []int {
	t.Helper()
	var conns []int
	for _, r := range f.ReceivedMethod("Target.setPauseOnStart") {
		var p struct {
			PauseOnStart bool `json:"pauseOnStart"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("Target.setPauseOnStart params %s: %v", r.Params, err)
		}
		if r.Target != "" {
			t.Fatalf("Target.setPauseOnStart was routed to target %q; it is a message of the connection", r.Target)
		}
		if p.PauseOnStart {
			conns = append(conns, r.Conn)
		}
	}
	return conns
}

// swapPaused runs a cross-process swap to id and checks that the new
// provisional target was announced paused and resumed only after the
// daemon's setup sent it the early-install state: Page.enable and the
// bootstrap script with the agent runtime (design 2.7).
func swapPaused(t *testing.T, f *fakewebkit.Server, id string) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.Swap(id, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the swap to %s did not commit within 10 s; its provisional target was never resumed", id)
	}
	var paused bool
	resume := 0
	beforeResume := map[string]bool{}
	for _, r := range f.Log() {
		switch {
		case !r.In && r.Method == "Target.targetCreated":
			var p struct {
				TargetInfo struct {
					TargetID string
					IsPaused bool
				}
			}
			_ = json.Unmarshal(r.Params, &p)
			if p.TargetInfo.TargetID == id {
				paused = p.TargetInfo.IsPaused
			}
		case r.In && r.Method == "Target.resume" && resume == 0:
			var p struct{ TargetID string }
			_ = json.Unmarshal(r.Params, &p)
			if p.TargetID == id {
				resume = r.Seq
			}
		case r.In && r.Target == id && resume == 0:
			beforeResume[r.Method] = true
		}
	}
	if !paused {
		t.Fatalf("provisional target %s was announced unpaused; the daemon's connection did not turn on pause on start", id)
	}
	if resume == 0 {
		t.Fatalf("provisional target %s was never resumed", id)
	}
	for _, m := range []string{"Page.enable", "Page.setBootstrapScript"} {
		if !beforeResume[m] {
			t.Fatalf("provisional target %s was resumed before its setup sent %s (sent before the resume: %v)", id, m, beforeResume)
		}
	}
}

// C01 (W1c-3, decision C01; design 2.7 item 3): the daemon turns on pause
// on start for every page connection. WebKit drops the setting when the
// frontend disconnects, so a reconnect (here a takeover after the page
// connection closed) must send it again; a provisional target after the
// reconnect is again set up before it is resumed.
func TestDaemonTurnsOnPauseOnStartOnEveryConnection(t *testing.T) {
	f := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	o := startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()})
	if _, err := (&core.Reservations{Dir: ReservationsDir(o.Dir)}).Reserve(dev.ID, "s1", time.Hour); err != nil {
		t.Fatal(err)
	}
	c := connect(t, o)

	open := func(takeover bool) {
		t.Helper()
		if _, err := c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: []string{"eval", "1"}, PageReq: PageReq{Page: f.URL}, Owner: Owner{Session: "s1", Takeover: takeover}}, nil); err != nil {
			t.Fatalf("eval (takeover %v): %v", takeover, err)
		}
	}

	open(false)
	if got := pauseOnStartConns(t, f); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Target.setPauseOnStart(true) received on connections %v, want [1]", got)
	}
	swapPaused(t, f, "page-2")

	f.CloseConnections()
	waitConns(t, f, 0)
	// The takeover reconnects only a page that the daemon saw lost; before
	// its watch observes the close, the page is still attached and the
	// takeover would reuse the closed connection.
	waitPageState(t, c, core.ConnLost)
	open(true)
	if n := f.ConnectionCount(); n != 2 {
		t.Fatalf("connections = %d, want 2 (the first and the takeover's)", n)
	}
	if got := pauseOnStartConns(t, f); len(got) != 2 || got[1] != 2 {
		t.Fatalf("Target.setPauseOnStart(true) received on connections %v, want [1 2]: the reconnect must send it again", got)
	}
	swapPaused(t, f, "page-3")
}

// waitPageState waits until the daemon's only page is in state want.
func waitPageState(t *testing.T, c *Client, want core.ConnState) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		st, err := c.Status(ctxFor(t, 5*time.Second))
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if len(st.Pages) == 1 && st.Pages[0].State == want.String() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("the daemon's pages are %+v, want one page in state %q", st.Pages, want)
		}
	}
}

func waitConns(t *testing.T, f *fakewebkit.Server, n int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); f.OpenConnections() != n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("open connections = %d, want %d", f.OpenConnections(), n)
		}
	}
}

// F1 (W1c-3 R1): an endpoint that refuses Target.setPauseOnStart keeps its
// page usable. The daemon log has one line per connection that names the
// reason, and a provisional target, announced unpaused, still gets the
// desired state (Page.enable, the bootstrap script) before it commits.
func TestDaemonLogsPauseOnStartOffAndStillSetsUpProvisionalTargets(t *testing.T) {
	f := newFake(t, fakewebkit.Options{})
	wsURL := reviewPauseProxy(t, f.URL, false)
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", wsURL)
	logBuf := &lockedBuffer{}
	o := startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr(), Log: logBuf})
	if _, err := (&core.Reservations{Dir: ReservationsDir(o.Dir)}).Reserve(dev.ID, "s1", time.Hour); err != nil {
		t.Fatal(err)
	}
	c := connect(t, o)
	if _, err := c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: []string{"eval", "1"}, PageReq: PageReq{Page: wsURL}, Owner: Owner{Session: "s1"}}, nil); err != nil {
		t.Fatalf("eval with Target.setPauseOnStart refused: %v", err)
	}
	const line = "pause on start is off: the endpoint refused it"
	if n := strings.Count(logBuf.String(), line); n != 1 {
		t.Fatalf("daemon log has %d lines with %q, want 1:\n%s", n, line, logBuf.String())
	}
	done := make(chan struct{})
	go func() { f.Swap("page-2", 0); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the swap to page-2 did not commit within 10 s")
	}
	sent := map[string]bool{}
	for end := time.Now().Add(5 * time.Second); !sent["Page.enable"] || !sent["Page.setBootstrapScript"]; time.Sleep(10 * time.Millisecond) {
		for _, r := range f.Log() {
			if r.In && r.Target == "page-2" {
				sent[r.Method] = true
			}
		}
		if time.Now().After(end) {
			t.Fatalf("the unpaused provisional target got %v, want Page.enable and Page.setBootstrapScript", sent)
		}
	}
	for _, r := range f.Log() {
		if !r.In && r.Method == "Target.targetCreated" && strings.Contains(string(r.Params), `"isPaused":true`) {
			t.Fatalf("a target was announced paused without pause on start: %s", r.Params)
		}
	}
	if n := len(f.ReceivedMethod("Target.resume")); n != 0 {
		t.Fatalf("%d Target.resume sent without pause on start", n)
	}
}
