package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// gatedWriter holds the first Write of an event stream (its "subscribed"
// item) until open is closed, so a test can end the page and the lease
// while the stream's handler is not yet waiting in its select.
type gatedWriter struct {
	http.ResponseWriter
	once    sync.Once
	entered chan struct{}
	open    chan struct{}
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.open
	})
	return g.ResponseWriter.Write(p)
}

func (g *gatedWriter) Flush() { g.ResponseWriter.(http.Flusher).Flush() }

// shutdownRaces is the number of shutdowns TestShutdownReasonWinsOverThe
// PageAndLeaseEnd runs. Without the shutdown's priority (endStream), each
// run picks the shutdown item with a probability of 1/3 (Go's select picks
// one of its ready cases at random), so all of them pass by chance with a
// probability of 3^-10.
const shutdownRaces = 10

// shutdownSetupBound bounds each setup step of a shutdown run (the page
// attach, the stream's start). It only turns a hang into a failure, and
// it is not part of the oracle: with 16 busy loops on 4 CPUs and the test
// at nice 19, ten shutdown runs took up to 27 s (robust3 stress run).
const shutdownSetupBound = 30 * time.Second

// R2-7, design 2.6: a daemon that shuts down ends every event stream with
// a final "closed" item that names the shutdown. Run closes the server's
// stopping channel (Server.Stop) before it releases the leases and closes
// the page connections (Manager.Close). A stream whose handler wakes only
// after Manager.Close, as under host load, finds three ended events at
// once: the shutdown, the page connection and the lease. The final item
// must still name the shutdown, because the shutdown ended the other two.
// The handler is held in its first write while Stop and Manager.Close
// run, which is the order that host load produced in full test runs
// (TestShutdownSendsAFinalClosedEvent, "want closed with the shutdown
// reason").
func TestShutdownReasonWinsOverThePageAndLeaseEnd(t *testing.T) {
	for i := range shutdownRaces {
		last := shutdownWithAStreamHeldInItsFirstWrite(t)
		if last.Type != "closed" || last.Error == nil || !strings.Contains(last.Error.Message, "shutting down") {
			msg := ""
			if last.Error != nil {
				msg = last.Error.Message
			}
			t.Fatalf("shutdown %d: last item %+v (error %q), want closed with the shutdown reason", i, last, msg)
		}
	}
}

// shutdownWithAStreamHeldInItsFirstWrite runs the daemon's shutdown order
// (Server.Stop, then Manager.Close) while an event stream of the leased
// page is held in its first write, then lets the stream go on and returns
// its last item.
func shutdownWithAStreamHeldInItsFirstWrite(t *testing.T) Item {
	t.Helper()
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	dir := filepath.Join(base, "iwdp")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	// The page is a device page of an iwdp listing, so the daemon opens it
	// as it opens a device page (a webkit.Session, which waits for the
	// page's target), not as a local endpoint.
	f := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	if _, err := (&core.Reservations{Dir: ReservationsDir(dir)}).Reserve(dev.ID, "mcp", time.Hour); err != nil {
		t.Fatal(err)
	}
	m, srv, tr := newCore(RunOptions{Dir: dir, Version: "test", Transport: "iwdp", Addr: l.Addr(), Log: io.Discard}, t.Logf)
	t.Cleanup(tr.close)
	gate := &gatedWriter{entered: make(chan struct{}), open: make(chan struct{})}
	var openGate sync.Once
	release := func() { openGate.Do(func() { close(gate.open) }) }
	t.Cleanup(release)
	h := srv.Handler()
	sock, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			gate.ResponseWriter = w
			w = gate
		}
		h.ServeHTTP(w, r)
	})}
	go func() { _ = hs.Serve(sock) }()
	t.Cleanup(func() { _ = hs.Close() })

	c := connect(t, Options{Dir: dir, NoStart: true, Version: "test"})
	if _, err := c.AttachPage(ctxFor(t, shutdownSetupBound), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "mcp"}}); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		resp *http.Response
		err  error
	}
	got := make(chan reply, 1)
	go func() {
		resp, err := c.request(ctxFor(t, shutdownSetupBound), http.MethodGet, "/v1/events?methods=&session=mcp&page="+url.QueryEscape(f.URL), nil)
		got <- reply{resp, err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(shutdownSetupBound):
		t.Fatal("the event stream did not write its first item")
	}
	srv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownBudget)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("manager close: %v", err)
	}
	if n := m.ActiveLeases(); n != 0 {
		t.Fatalf("setup: %d leases after the manager closed, want 0", n)
	}
	release()
	rp := <-got
	if rp.err != nil {
		t.Fatal(rp.err)
	}
	defer func() { _ = rp.resp.Body.Close() }()
	sc := bufio.NewScanner(rp.resp.Body)
	var items []Item
	for sc.Scan() {
		var it Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			t.Fatalf("item %q: %v", sc.Bytes(), err)
		}
		items = append(items, it)
	}
	if len(items) < 2 || items[0].Type != "subscribed" {
		t.Fatalf("stream items %+v, want subscribed and a final item", items)
	}
	return items[len(items)-1]
}
