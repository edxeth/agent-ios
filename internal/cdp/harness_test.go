package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// logBuf collects log lines (the daemon log of the endpoint).
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *logBuf) logf(format string, a ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, fmt.Sprintf(format, a...))
}

func (b *logBuf) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}

// rig is two fake devices behind a fake iwdp listing: device A (reserved
// by owner "o1") with pages One and Two, device B (not reserved) with page
// Three, a manager, and a started CDP endpoint.
type rig struct {
	l          *fakewebkit.Listing
	devA, devB *fakewebkit.ListedDevice
	fake       map[string]*fakewebkit.Server     // by page title
	listed     map[string]*fakewebkit.ListedPage // by page title
	res        *core.Reservations
	m          *core.Manager
	srv        *Server
	log        *logBuf
	req        core.PageRequest
	observed   *[]string
	obsMu      *sync.Mutex // guards observed
}

// newRig builds the rig; opts change the endpoint's Options before it
// starts.
func newRig(t *testing.T, opts ...func(*Options)) *rig {
	t.Helper()
	return newRigWrapped(t, nil, opts...)
}

// newRigWrapped builds the rig with wrap (when not nil) around the
// manager's transport.
func newRigWrapped(t *testing.T, wrap func(transport.Transport) transport.Transport, opts ...func(*Options)) *rig {
	t.Helper()
	r := &rig{l: fakewebkit.NewListing(false), fake: map[string]*fakewebkit.Server{}, listed: map[string]*fakewebkit.ListedPage{}, log: &logBuf{}}
	t.Cleanup(r.l.Close)
	r.devA = r.l.AddDevice("00008020-00000000000000A1", "iPhone A", "16.7.10")
	r.devB = r.l.AddDevice("00008020-00000000000000B2", "iPhone B", "16.7.10")
	r.addPage(t, r.devA, "One")
	r.addPage(t, r.devA, "Two")
	r.addPage(t, r.devB, "Three")
	r.res = &core.Reservations{Dir: t.TempDir()}
	if _, err := r.res.Reserve(r.devA.ID, "o1", time.Hour); err != nil {
		t.Fatal(err)
	}
	r.m = core.NewManager(core.ManagerOptions{
		Transport: func(_ context.Context, kind, addr string) (transport.Transport, error) {
			var tr transport.Transport = iwdp.New(iwdp.Options{ListingAddr: addr})
			if wrap != nil {
				tr = wrap(tr)
			}
			return tr, nil
		},
		Reservations: r.res,
		Session:      webkit.SessionOptions{TargetWait: time.Second},
		Logf:         r.log.logf,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.m.Close(ctx)
	})
	var obsMu sync.Mutex
	obs := []string{}
	r.observed, r.obsMu = &obs, &obsMu
	o := Options{
		Manager: r.m, Reservations: r.res, Version: "test", Logf: r.log.logf, PollEvery: 100 * time.Millisecond,
		Observe: func(req *http.Request, status int) {
			obsMu.Lock()
			obs = append(obs, fmt.Sprintf("%s %s %d", req.Method, req.URL.Path, status))
			obsMu.Unlock()
		},
	}
	for _, f := range opts {
		f(&o)
	}
	r.srv = New(o)
	if _, err := r.srv.Start(0, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.srv.Stop() })
	r.req = core.PageRequest{Transport: "iwdp", Addr: r.l.Addr()}
	return r
}

func (r *rig) addPage(t *testing.T, d *fakewebkit.ListedDevice, title string) *fakewebkit.Server {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{TargetID: "wk-" + title})
	t.Cleanup(f.Close)
	r.fake[title] = f
	r.listed[title] = d.AddPage(title, "https://"+strings.ToLower(title)+".example/", f.URL)
	return f
}

// issue returns the endpoint of session for the page title.
func (r *rig) issue(t *testing.T, session, page, scope string) Endpoint {
	t.Helper()
	req := r.req
	req.Page = page
	ep, err := r.srv.Issue(ctxFor(t, 5*time.Second), IssueRequest{Session: session, Reservation: "o1", Page: req, Scope: scope})
	if err != nil {
		t.Fatalf("Issue(%s, %s, %s): %v", session, page, scope, err)
	}
	return ep
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func dial(t *testing.T, url string) *cdptest.Client {
	t.Helper()
	c, _, err := cdptest.Dial(ctxFor(t, 5*time.Second), url, nil)
	if err != nil {
		t.Fatalf("dialing the CDP endpoint: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func call(t *testing.T, c *cdptest.Client, session, method string, params any) json.RawMessage {
	t.Helper()
	res, err := c.Call(ctxFor(t, 10*time.Second), session, method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

// leaseOf returns the session and epoch of the lease on the page title.
func (r *rig) leaseOf(t *testing.T, title string) (string, uint64) {
	t.Helper()
	for _, ps := range r.m.Status() {
		if ps.Title == title {
			return ps.Session, ps.Epoch
		}
	}
	return "", 0
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
}

type attached struct {
	SessionID  string     `json:"sessionId"`
	TargetInfo targetInfo `json:"targetInfo"`
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

func eventually(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("%s: not within %v", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// received lists the methods a fake page received.
func received(f *fakewebkit.Server) []string {
	var out []string
	for _, r := range f.Received() {
		out = append(out, r.Method)
	}
	return out
}
