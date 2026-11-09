package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
)

// Regression tests for the iwdp auto-start of the first device smoke test
// (decision D10): on a loaded host the daemon took a slow listing for a
// dead iwdp and started a copy on every call. Every listing here is a
// private loopback address; no test reads port 9221 or runs the real iwdp.

// silentIWDPListing accepts connections and never answers, like an iwdp
// whose event loop is stuck.
func silentIWDPListing(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return l.Addr().String()
}

// slowIWDPListing answers GET /json after delay.
func slowIWDPListing(t *testing.T, delay time.Duration) string {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-time.After(delay):
		case <-done:
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return strings.TrimPrefix(srv.URL, "http://")
}

// countingStarts is a RunOptions.StartIWDP that counts its calls.
func countingStarts(n *atomic.Int32) func(context.Context) error {
	return func(context.Context) error {
		n.Add(1)
		return nil
	}
}

func TestEnsureIWDPNeverStartsWhileTheListingPortIsOwned(t *testing.T) {
	// A process owns the port and does not answer within the budget: no
	// start, and the error says why.
	t.Run("silent", func(t *testing.T) {
		t.Parallel()
		addr := silentIWDPListing(t)
		var starts atomic.Int32
		ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: countingStarts(&starts)}, logf: t.Logf}
		err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true)
		if err == nil {
			t.Fatal("ensureIWDP passed a listing that never answers")
		}
		for _, want := range []string{addr, "does not answer", "busy or stuck", "no second"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		if n := starts.Load(); n != 0 {
			t.Fatalf("iwdp started %d times while %s was owned, want 0", n, addr)
		}
	})
	// The shared 5 s budget: a listing answering after 3 s is running.
	t.Run("slow", func(t *testing.T) {
		t.Parallel()
		addr := slowIWDPListing(t, 3*time.Second)
		var starts atomic.Int32
		ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: countingStarts(&starts)}, logf: t.Logf}
		if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err != nil {
			t.Fatalf("a listing answering after 3s: %v", err)
		}
		if n := starts.Load(); n != 0 {
			t.Fatalf("iwdp started %d times while it answered within the budget, want 0", n)
		}
	})
}

// With nothing on the port and no StartIWDP (or a custom address), the
// error tells how to start iwdp.
func TestEnsureIWDPReportsAStoppedIWDP(t *testing.T) {
	addr := deadListing(t)
	ts := &transports{o: RunOptions{Transport: "iwdp"}, logf: t.Logf}
	err := ts.ensureIWDP(ctxFor(t, 10*time.Second), addr, true)
	if err == nil || !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), "ios_webkit_debug_proxy --no-frontend") {
		t.Fatalf("ensureIWDP without iwdp = %v, want how to start it on %s", err, addr)
	}
}

// Concurrent calls on a free port start iwdp once.
func TestEnsureIWDPStartsOnceForConcurrentCalls(t *testing.T) {
	addr := deadListing(t)
	var starts atomic.Int32
	start := func(context.Context) error {
		starts.Add(1)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }), ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
		return nil
	}
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: start}, logf: t.Logf}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent ensureIWDP: %v", err)
		}
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("%d concurrent calls started iwdp %d times, want 1", callers, n)
	}
}

// The daemon with the binaries' start (proxy's Starter): after a failed
// start, N quick calls fail fast and run ios_webkit_debug_proxy no more.
func TestEnsureIWDPBacksOffAfterAFailedStart(t *testing.T) {
	addr := deadListing(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "starts.log")
	fakes := map[string]string{
		"ios_webkit_debug_proxy": "#!/bin/sh\necho \"$$ $*\" >> '" + log + "'\nexit 1\n",
		"idevice_id":             "#!/bin/sh\nexit 1\n",
	}
	for name, script := range fakes {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // the started fake's log (proxy.LogPath)
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: proxy.NewStarter(addr).Start}, logf: t.Logf}
	if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err == nil {
		t.Fatal("ensureIWDP passed with a fake iwdp that exits 1")
	}
	const quick = 5
	begin := time.Now()
	for i := range quick {
		err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true)
		if err == nil || !strings.Contains(err.Error(), "not trying again") {
			t.Fatalf("call %d after the failed start = %v, want the back-off error", i, err)
		}
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("%d calls in the back-off took %s, want a fast failure", quick, d)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Fatalf("a failed start and %d calls ran ios_webkit_debug_proxy %d times (%q), want 1", quick, len(lines), lines)
	}
}

// Device check 3, P3-E: the first "tab list" right after the daemon
// started iwdp printed "No tabs.", and 9 s later the device's pages were
// listed: iwdp lists a device's pages a moment after it starts (upstream
// issue #5). After its own start, ensureIWDP waits a bounded time
// (startedPagesWait) for a device page list that is not empty.

// startedListing is the iwdp that start (a RunOptions.StartIWDP) brings up
// on addr: a device list with one device whose page list is empty for the
// first emptyFor requests and then lists one page. emptyFor < 0: always
// empty. pageReads counts the page list requests.
func startedListing(t *testing.T, addr string, emptyFor int32, pageReads *atomic.Int32) func(context.Context) error {
	t.Helper()
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := pageReads.Add(1); emptyFor < 0 || n <= emptyFor {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte(`[{"title":"Fixture","url":"https://fixture.example/","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/1"}]`))
	}))
	t.Cleanup(pages.Close)
	devices := `[{"deviceId":"dev1","deviceName":"iPhone","url":"` + strings.TrimPrefix(pages.URL, "http://") + `"}]`
	return func(context.Context) error {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(devices)) }), ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
		return nil
	}
}

func TestEnsureIWDPWaitsForThePagesOfAStartedIWDP(t *testing.T) {
	addr := deadListing(t)
	var reads atomic.Int32
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: startedListing(t, addr, 3, &reads)}, logf: t.Logf}
	if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err != nil {
		t.Fatal(err)
	}
	pages, err := iwdp.New(iwdp.Options{ListingAddr: addr}).Pages(ctxFor(t, 5*time.Second), "dev1")
	if err != nil || len(pages) != 1 {
		t.Fatalf("the first page list after the start = %v, %v; want the device's page (%d page list reads)", pages, err, reads.Load())
	}
}

// A device page list that stays empty is reported as before, after the
// bounded wait; an iwdp that was already running is not waited for.
func TestEnsureIWDPBoundsTheWaitForPages(t *testing.T) {
	addr := deadListing(t)
	var reads atomic.Int32
	var log syncLog
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: startedListing(t, addr, -1, &reads)}, logf: log.logf}
	begin := time.Now()
	if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d < startedPagesWait || d > startedPagesWait+2*time.Second {
		t.Errorf("ensureIWDP with an empty page list took %s, want the bounded wait of %s", d, startedPagesWait)
	}
	if n := reads.Load(); n < 2 {
		t.Errorf("the page list was read %d times during the wait, want a poll", n)
	}
	if !strings.Contains(log.String(), "lists no page") {
		t.Errorf("daemon log %q, want a line that the started iwdp lists no page", log.String())
	}
	before := reads.Load()
	begin = time.Now()
	if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err != nil {
		t.Fatal(err)
	}
	if d, n := time.Since(begin), reads.Load()-before; d > time.Second || n != 0 {
		t.Errorf("ensureIWDP with iwdp running took %s and read the page list %d times, want no wait", d, n)
	}
	// The wait ends with the command's deadline.
	addr2 := deadListing(t)
	ts2 := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: startedListing(t, addr2, -1, new(atomic.Int32))}, logf: t.Logf}
	begin = time.Now()
	_ = ts2.ensureIWDP(ctxFor(t, time.Second), addr2, true)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("ensureIWDP under a 1 s deadline took %s", d)
	}
}

// syncLog collects log lines (a transports.logf) safely across goroutines.
type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
