package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// newHeapHarness is newDTHarness with a protocol call budget of
// callTimeout, so that a test can make a reply outlast the budget.
func newHeapHarness(t *testing.T, callTimeout time.Duration) *dtHarness {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(dev.ID, "default", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := core.NewManager(core.ManagerOptions{
		Reservations: res, Session: webkit.SessionOptions{TargetWait: time.Second, CallTimeout: callTimeout},
		Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
			return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return &dtHarness{f: f, m: m, req: core.PageRequest{Transport: "iwdp", Addr: l.Addr()}, dir: t.TempDir()}
}

func heapFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../heapsnap/testdata/webkitgtk-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// heap snapshot writes the snapshotData of Heap.snapshot unchanged with
// mode 0600 and prints the class summary of the file (expected values:
// internal/heapsnap/testdata/README.md), bounded by --top. The reply comes
// after the session's call budget (here 300 ms) and within the command
// deadline: a snapshot may take most of the deadline.
func TestHeapSnapshotWritesAndSummarizes(t *testing.T) {
	h := newHeapHarness(t, 300*time.Millisecond)
	snap := heapFixture(t)
	h.f.Handle("Heap.snapshot", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		time.Sleep(700 * time.Millisecond)
		return map[string]any{"timestamp": 1.5, "snapshotData": string(snap)}, nil
	})
	out := h.mustRun(t, "heap", "snapshot", "a.json", "--top", "2")
	path := filepath.Join(h.dir, "a.json")
	want := "Heap snapshot written to " + path + " (" + itoa(len(snap)) + " bytes, JavaScriptCore format version 3).\n" +
		"260 nodes, 33496 bytes self size in total, 71 classes.\n" +
		"Top classes by count:\n" +
		"       count    self bytes  class\n" +
		"          40          1280  HeapProbe045\n" +
		"           5          8852  FunctionCodeBlock\n" +
		"Top classes by self size:\n" +
		"       count    self bytes  class\n" +
		"           5          8852  FunctionCodeBlock\n" +
		"           4          8375  Window"
	if out != want {
		t.Fatalf("heap snapshot:\n%s\nwant:\n%s", out, want)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot file: %v, %v; want mode 0600", err, st)
	}
	if got, _ := os.ReadFile(path); string(got) != string(snap) {
		t.Fatalf("the written file differs from snapshotData (%d bytes, want %d)", len(got), len(snap))
	}
	if n := len(h.f.ReceivedMethod("Heap.gc")); n != 0 {
		t.Fatalf("heap snapshot sent Heap.gc %d times; JavaScriptCore collects before a snapshot", n)
	}

	var data struct {
		Path       string `json:"path"`
		Nodes      int64  `json:"nodes"`
		TopByCount []struct {
			Name  string `json:"name"`
			Count int64  `json:"count"`
		} `json:"topByCount"`
	}
	if err := json.Unmarshal([]byte(h.mustRun(t, "--json", "heap", "snapshot", "b.json", "--top", "1")), &data); err != nil {
		t.Fatal(err)
	}
	if data.Path != filepath.Join(h.dir, "b.json") || data.Nodes != 260 || len(data.TopByCount) != 1 || data.TopByCount[0].Name != "HeapProbe045" || data.TopByCount[0].Count != 40 {
		t.Fatalf("--json: %+v", data)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A snapshot of an unknown format version is still written, and the
// command fails (exit 1) naming the file and the version; an empty
// snapshotData (JavaScriptCore's answer to a snapshot that outgrew its
// string builder) fails and writes nothing.
func TestHeapSnapshotRefusesUnknownVersion(t *testing.T) {
	h := newDTHarness(t)
	bad := `{"version":9,"type":"Inspector","nodes":[0,0,0,0],"nodeClassNames":["<root>"]}`
	h.f.Result("Heap.snapshot", map[string]any{"timestamp": 1, "snapshotData": bad})
	code, _, errOut := h.run("heap", "snapshot", "v9.json")
	path := filepath.Join(h.dir, "v9.json")
	if code != ExitError || !strings.Contains(errOut, path) || !strings.Contains(errOut, "version 9 is not supported") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got, _ := os.ReadFile(path); string(got) != bad {
		t.Fatalf("written file = %q", got)
	}
	h.f.Result("Heap.snapshot", map[string]any{"timestamp": 1, "snapshotData": ""})
	code, _, errOut = h.run("heap", "snapshot", "empty.json")
	if code != ExitError || !strings.Contains(errOut, "no snapshot data") {
		t.Fatalf("empty snapshot: exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "empty.json")); !os.IsNotExist(err) {
		t.Fatalf("empty snapshot wrote a file: %v", err)
	}
}

// heap gc sends Heap.gc once; --top outside 1 to 1000 is a usage error.
func TestHeapGCAndTopRange(t *testing.T) {
	h := newDTHarness(t)
	if out := h.mustRun(t, "heap", "gc"); out != "Garbage collection done." {
		t.Fatalf("heap gc = %q", out)
	}
	if n := len(h.f.ReceivedMethod("Heap.gc")); n != 1 {
		t.Fatalf("Heap.gc sent %d times", n)
	}
	for _, v := range []string{"0", "1001", "-3"} {
		if code, _, errOut := h.run("heap", "snapshot", "--top", v); code != ExitUsage {
			t.Fatalf("--top %s: exit %d: %s", v, code, errOut)
		}
	}
}

// heap diff compares two files on the host: node and size totals, and the
// classes whose count or size changed, largest size change first. The
// expected values are summed by hand from the two files.
func TestHeapDiff(t *testing.T) {
	h := newDTHarness(t)
	a := `{"version":2,"type":"Inspector","nodes":[0,0,0,0, 1,32,1,0, 2,32,1,0, 3,100,2,0, 4,8,3,0],"nodeClassNames":["<root>","Widget","Window","string"],"edges":[],"edgeTypes":[],"edgeNames":[]}`
	b := `{"version":3,"type":"Inspector","nodes":[0,0,0,0, 3,100,1,0, 5,8,2,0, 6,8,2,0, 7,8,2,0, 8,500,3,0],"nodeClassNames":["<root>","Window","string","Cache"],"edges":[],"edgeTypes":[],"edgeNames":[]}`
	for name, s := range map[string]string{"a.json": a, "b.json": b} {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := h.mustRun(t, "heap", "diff", "a.json", "b.json")
	want := "Heap diff " + filepath.Join(h.dir, "a.json") + " -> " + filepath.Join(h.dir, "b.json") +
		": nodes +1 (4 -> 5), self size +452 bytes (172 -> 624), 3 classes changed.\n" +
		"       count    self bytes  class\n" +
		"          +1          +500  Cache\n" +
		"          -2           -64  Widget\n" +
		"          +2           +16  string"
	if out != want {
		t.Fatalf("heap diff:\n%s\nwant:\n%s", out, want)
	}
	if out := h.mustRun(t, "heap", "diff", "a.json", "b.json", "--top", "1"); !strings.Contains(out, "Top 1 by self size change:") || strings.Contains(out, "Widget") {
		t.Fatalf("--top 1:\n%s", out)
	}
	if len(h.forwarded) != 0 {
		t.Fatalf("heap diff went to the daemon: %d requests", len(h.forwarded))
	}
	if code, _, errOut := h.run("heap", "diff", "a.json", "missing.json"); code != ExitError || !strings.Contains(errOut, "missing.json") {
		t.Fatalf("missing file: exit %d: %s", code, errOut)
	}
}
