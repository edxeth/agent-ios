package daemon

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// docs/known-limits.md, "iwdp does not list pages while a page is
// connected": an iwdp that closes listing requests without a reply owns
// its port, so the daemon never takes it for dead and never starts a
// second iwdp, also on the default address. The error is a listing error
// (core.ListingError), which core turns into advice while a page is
// connected.
func TestEnsureIWDPNeverStartsOnAnEmptyReply(t *testing.T) {
	f := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10").AddPage("Fixture", "https://fixture.example/", f.URL)
	l.EmptyReplyWhileConnected(f)
	// A page WebSocket is open, as the daemon's own page connection is.
	c, _, err := websocket.DefaultDialer.Dial(f.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Dial returns after the handshake, before the fake counts the
	// connection; the fake announces the page target only after it counted
	// it, so the announcement shows that the listing now sees it open.
	if _, msg, err := c.ReadMessage(); err != nil || !strings.Contains(string(msg), "Target.targetCreated") {
		t.Fatalf("fixture: the page announced %q, %v; want its Target.targetCreated", msg, err)
	}
	var starts atomic.Int32
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: countingStarts(&starts)}, logf: t.Logf}
	err = ts.ensureIWDP(ctxFor(t, 10*time.Second), l.Addr(), true)
	var le *core.ListingError
	if !errors.As(err, &le) || le.Kind != transport.KindIWDP {
		t.Fatalf("ensureIWDP on an empty reply = %v, want a core.ListingError of iwdp", err)
	}
	for _, want := range []string{l.Addr(), "closed the connection without an answer", "no second iwdp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "not running") {
		t.Errorf("error %q calls the iwdp not running", err)
	}
	if n := starts.Load(); n != 0 {
		t.Fatalf("iwdp started %d times on an empty reply, want 0", n)
	}
}
