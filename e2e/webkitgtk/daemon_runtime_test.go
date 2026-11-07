//go:build webkitgtk

package webkitgtk

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
)

// runtimeBrowser starts broadwayd and MiniBrowser (with the DPI shim of
// runSuite, so pages have geometry) on the runtime fixture page and
// returns the inspector address once it lists the page. Its processes end
// at test cleanup.
func runtimeBrowser(t *testing.T, page string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(fixtureDir()))}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	shimDir := t.TempDir()
	shim, err := buildDPIShim(shimDir)
	if err != nil {
		t.Fatal(err)
	}
	display := broadway(t)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "LD_PRELOAD=" + shim},
		miniBrowser, "http://"+l.Addr().String()+"/"+page)
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(browserStart); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 && pages[0].Title != "" {
			return addr
		}
	}
	t.Fatalf("MiniBrowser did not list the page %s at %s within %v", page, addr, browserStart)
	return ""
}

var signInRef = regexp.MustCompile(`button "Sign In" \[ref=e(\d+)\]`)

// snapshotRef runs "snapshot -i" for session and returns the number of the
// Sign In button's ref.
func snapshotRef(t *testing.T, session string) int {
	t.Helper()
	r := cli(t, "--session", session, "snapshot", "-i")
	m := signInRef.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("%s: snapshot -i: exit %d, stdout %q, stderr %q; want the Sign In button with a ref", session, r.code, r.stdout, r.stderr)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// W3a wiring through the daemon (docs/notes/w1b.md "Hooks for W3a";
// design 3.1 owner epoch, 3.3 ref lifetime): a ref from "snapshot" works
// in a later, separate CLI process of the same session, because the
// daemon keeps the page's runtime, ref table and owner; when another
// session takes the lease, the refs of the earlier owner are unknown to
// it, and to the earlier owner when it comes back under a new lease; ref
// ids are never handed out again.
func TestRefsThroughTheDaemonAcrossCLIProcesses(t *testing.T) {
	addr := runtimeBrowser(t, "forms.html")
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	const a, b = "agent-a", "agent-b"
	n := snapshotRef(t, a)
	ref := "@e" + strconv.Itoa(n)

	// A later CLI process of the same session resolves the ref.
	if r := cli(t, "--session", a, "get", "text", ref); r.code != 0 || strings.TrimSpace(r.stdout) != "Sign In" {
		t.Fatalf("get text %s in a later process: exit %d, stdout %q, stderr %q; want Sign In", ref, r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", a, "is", "visible", ref); r.code != 0 || strings.TrimSpace(r.stdout) != "true" {
		t.Fatalf("is visible %s: exit %d, stdout %q, stderr %q", ref, r.code, r.stdout, r.stderr)
	}

	// Session B: refused while A holds the lease, then B takes it.
	if r := cli(t, "--session", b, "get", "text", ref); r.code != 4 || !strings.Contains(r.stderr, `leased by session "agent-a"`) {
		t.Fatalf("%s while %s holds the lease: exit %d, stderr %q; want the lease error (exit 4)", b, a, r.code, r.stderr)
	}
	if r := cli(t, "lease", "break", "Forms"); r.code != 0 {
		t.Fatalf("lease break: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := cli(t, "--session", b, "get", "text", ref); r.code != 1 || !strings.Contains(r.stderr, "no snapshot produced "+ref) {
		t.Fatalf("%s with %s's ref: exit %d, stdout %q, stderr %q; want unknown_ref (no snapshot produced)", b, a, r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", b, "get", "title"); r.code != 0 || strings.TrimSpace(r.stdout) != "Forms" {
		t.Fatalf("%s: get title: exit %d, stdout %q, stderr %q", b, r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", b, "session", "release"); r.code != 0 {
		t.Fatalf("%s: session release: exit %d, stderr %q", b, r.code, r.stderr)
	}

	// A again, under a new lease epoch after B: its old ref stays unknown.
	if r := cli(t, "--session", a, "get", "text", ref); r.code != 1 || !strings.Contains(r.stderr, "no snapshot produced "+ref) {
		t.Fatalf("%s's own ref after %s held the page: exit %d, stdout %q, stderr %q; want unknown_ref", a, b, r.code, r.stdout, r.stderr)
	}
	// A new snapshot hands out new ids; the old one is never reused.
	if m := snapshotRef(t, a); m <= n {
		t.Fatalf("the new snapshot gave the Sign In button e%d, want an id above e%d", m, n)
	}
	if st := cli(t, "daemon", "status"); st.code != 0 || !strings.Contains(st.stdout, "Pages: 1") {
		t.Fatalf("daemon status: exit %d, stdout %q; want one page record", st.code, st.stdout)
	}
}
