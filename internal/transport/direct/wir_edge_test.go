package direct

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Application updates and frames that carry no message: a new app is
// listed after _rpc_applicationConnected:, an app missing from a new
// application list loses its pages, socket data sent as a string is
// delivered, an empty frame is skipped, and a frame that is not a plist is
// reported to every page as a dropped message.
func TestWIRApplicationUpdatesAndOddFrames(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{ListingWait: 500 * time.Millisecond})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	fake := d.fake(0)

	fake.mu.Lock()
	fake.apps = append(fake.apps, fakeApp{id: "PID:400", bundle: "com.example.new", pages: []fakePage{{id: 3, title: "New app page"}}})
	newApp := fake.apps[len(fake.apps)-1]
	fake.mu.Unlock()
	fake.send(selAppConnected, fake.appDict(newApp))
	fake.send(selAppSentListing, map[string]any{keyAppID: "PID:999", keyListing: map[string]any{"1": map[string]any{keyPageID: 1, keyTitle: "unannounced app"}}})
	pages := listPages(t, tr)
	if p := findPage(t, pages, "PID:400/3"); p.AppID != "com.example.new" || p.Title != "New app page" {
		t.Fatalf("page of the connected app: %+v", p)
	}
	for _, p := range pages {
		if strings.HasPrefix(p.ID, "PID:999/") {
			t.Fatalf("listed a page of an app that was never announced: %+v", p)
		}
	}

	c := openRaw(t, tr, "PID:100/1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sender := c.(*pageConn).sender
	fake.send(selAppSentData, map[string]any{keyDestination: sender, keyMessageData: `{"method":"Console.messageAdded"}`})
	if got, err := c.Read(ctx); err != nil || string(got) != `{"method":"Console.messageAdded"}` {
		t.Fatalf("string socket data: %q, %v", got, err)
	}
	fake.sendRaw([]byte{0, 0, 0, 0})
	fake.sendRaw(rawFrame([]byte("not a plist")))
	got, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(got), EventMessageDropped) || !strings.Contains(string(got), "not a valid property list") {
		t.Fatalf("after an empty frame and a bad frame: %q, %v; want only the dropped-message event", got, err)
	}

	// A new application list without Safari ends Safari's page connections.
	fake.send(selReportAppList, map[string]any{keyAppDictionary: map[string]any{"PID:400": fake.appDict(newApp)}})
	if _, err := c.Read(ctx); !errors.Is(err, webkit.ErrPageClosed) {
		t.Fatalf("Read after Safari left the application list: %v, want ErrPageClosed", err)
	}
	pages = listPages(t, tr)
	if len(pages) != 1 || pages[0].ID != "PID:400/3" {
		t.Fatalf("pages after the new application list: %+v", pages)
	}
}

// Pages returns the caller's deadline error when the context ends while it
// waits for listings.
func TestPagesHonoursTheContextDuringTheListingWait(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{ListingWait: 10 * time.Second})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	fake := d.fake(0)
	fake.mu.Lock()
	fake.listingHold["PID:100"] = 5 * time.Second
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := tr.Pages(ctx, testUDID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pages: %v, want the deadline", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Pages returned after %s", el)
	}
}

// writeConn wraps the client end of the pipe so tests can block or fail
// writes after the session is open.
type writeConn struct {
	net.Conn
	mu    sync.Mutex
	block chan struct{} // non-nil: writes wait for it to close
	fail  error
}

func (w *writeConn) Write(p []byte) (int, error) {
	w.mu.Lock()
	block, fail := w.block, w.fail
	w.mu.Unlock()
	if block != nil {
		<-block
	}
	if fail != nil {
		return 0, fail
	}
	return w.Conn.Write(p)
}

// A write error closes the device connection: every page fails with
// ErrPageClosed. A caller whose deadline passes while its frame is being
// written is told the message may still arrive; one still waiting for the
// write loop is told it was not sent.
func TestDeviceWriteBlockedOrFailed(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	var wc *writeConn
	d.wrap = func(c net.Conn) deviceConn {
		wc = &writeConn{Conn: c}
		return wc
	}
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	a := openRaw(t, tr, "PID:100/1")
	b := openRaw(t, tr, "PID:100/2")

	release := make(chan struct{})
	wc.mu.Lock()
	wc.block = release
	wc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	errA := make(chan error, 1)
	go func() { errA <- a.Write(ctx, []byte(`{"id":1,"method":"Runtime.evaluate","params":{}}`)) }()
	time.Sleep(50 * time.Millisecond) // a's frame is in the write loop
	errB := b.Write(ctx, []byte(`{"id":2,"method":"Runtime.evaluate","params":{}}`))
	if errB == nil || !strings.Contains(errB.Error(), "not sent") || !errors.Is(errB, context.DeadlineExceeded) {
		t.Fatalf("write waiting for the loop: %v, want not sent at the deadline", errB)
	}
	if err := <-errA; err == nil || !strings.Contains(err.Error(), "may still arrive") {
		t.Fatalf("write in progress at the deadline: %v, want 'may still arrive'", err)
	}

	wc.mu.Lock()
	wc.block, wc.fail = nil, errors.New("broken pipe")
	wc.mu.Unlock()
	close(release)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	err := a.Write(ctx2, []byte(`{"id":3,"method":"Runtime.evaluate","params":{}}`))
	for err == nil { // the blocked write of a may take the error first
		err = a.Write(ctx2, []byte(`{"id":4,"method":"Runtime.evaluate","params":{}}`))
	}
	for _, c := range []transport.MessageConn{a, b} {
		if _, err := c.Read(ctx2); !errors.Is(err, webkit.ErrPageClosed) || !strings.Contains(err.Error(), "broken pipe") {
			t.Fatalf("Read after the failed write: %v, want ErrPageClosed with the cause", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close after the device failed: %v, want nil (nothing left to close)", err)
	}
}

// A caller that waits for another caller's dial gives up at its own deadline.
func TestWaitingForASharedDialHonoursTheContext(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	release := make(chan struct{})
	slow := &slowDialer{inner: d, release: release}
	tr := newWithDialer(slow, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := tr.Pages(ctx, testUDID)
		first <- err
	}()
	for slow.started() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tr.Pages(ctx, testUDID); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the connection") {
		t.Fatalf("second caller: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first caller: %v", err)
	}
	if slow.started() != 1 {
		t.Fatalf("dials = %d, want 1", slow.started())
	}
}

type slowDialer struct {
	inner   deviceDialer
	release chan struct{}
	mu      sync.Mutex
	n       int
}

func (s *slowDialer) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *slowDialer) Devices(ctx context.Context) ([]transport.Device, error) {
	return s.inner.Devices(ctx)
}

func (s *slowDialer) Dial(ctx context.Context, udid string) (deviceConn, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	<-s.release
	return s.inner.Dial(ctx, udid)
}

// Device input parsing: ids must be integers that fit an int, app ids must
// be present, and page keys must be <app id>/<integer>.
func TestParseDeviceInput(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int
		ok   bool
	}{{uint64(7), 7, true}, {int64(-1), -1, true}, {3, 3, true}, {uint64(1) << 63, 0, false}, {"7", 0, false}, {nil, 0, false}} {
		got, ok := plistInt(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("plistInt(%#v) = %d, %v", c.in, got, ok)
		}
	}
	if _, ok := parseApp(map[string]any{keyAppBundleID: "x"}); ok {
		t.Fatal("parseApp accepted an app without an id")
	}
	if _, ok := parseApp("not a dict"); ok {
		t.Fatal("parseApp accepted a string")
	}
	if _, ok := parsePage(map[string]any{keyTitle: "no id"}); ok {
		t.Fatal("parsePage accepted a page without an id")
	}
	if a, p, ok := parsePageKey("PID:1/2/3"); !ok || a != "PID:1/2" || p != 3 {
		t.Fatalf("parsePageKey splits at the last slash: %q %d %v", a, p, ok)
	}
	if _, _, ok := parsePageKey("/3"); ok {
		t.Fatal("parsePageKey accepted an empty app id")
	}
}
