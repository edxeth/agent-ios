package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/heapsnap"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// heap snapshot, heap gc and heap diff: the WebKit Heap domain, natively
// (chrome-devtools-mcp's take_heapsnapshot writes a V8 .heapsnapshot; the
// CDP bridge does not translate HeapProfiler, because JavaScriptCore's
// snapshot format differs and converted analyses would mislead).
// Heap.snapshot returns JavaScriptCore's own JSON (internal/heapsnap),
// which heap snapshot writes as it is, with mode 0600, and summarizes by
// class on the host. JavaScriptCore runs a full garbage collection before
// it builds a snapshot (HeapSnapshotBuilder::buildSnapshot), so heap
// snapshot sends no Heap.gc.
func init() {
	topFlag := Flag{Name: "top", Type: FlagInt, Default: "10", Help: "classes per list (1 to 1000)"}
	Register(&Command{
		Name:    "heap snapshot",
		Summary: "Take a JavaScriptCore heap snapshot of the page, write its JSON (mode 0600) to path or a new temp file, and summarize it by class.",
		Args:    []Arg{{Name: "path", Help: "the snapshot file to write (default: a new agent-ios-heap-*.json in the temp directory)"}},
		Flags:   []Flag{topFlag},
		Page:    LeasedPage,
		Mutates: true,
		Timeout: heapTimeout,
		Output:  OutputText,
		Check:   checkHeapTop,
		Run:     runHeapSnapshot,
	})
	Register(&Command{
		Name:    "heap gc",
		Summary: "Run a full garbage collection of the page's JavaScript heap (Heap.gc).",
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runHeapGC,
	})
	Register(&Command{
		Name:    "heap diff",
		Summary: "Compare two files of heap snapshot by class, on the host: the changes of node count and self size from a to b.",
		Args: []Arg{
			{Name: "a", Help: "the earlier snapshot file", Required: true},
			{Name: "b", Help: "the later snapshot file", Required: true},
		},
		Flags:   []Flag{topFlag},
		Local:   true,
		Timeout: heapTimeout,
		Output:  OutputText,
		Check:   checkHeapTop,
		Run:     runHeapDiff,
	})
}

// heapTimeout is the default deadline of heap snapshot and heap diff: a
// snapshot of a heavy page can take a minute to build, send and read.
const heapTimeout = 2 * time.Minute

func checkHeapTop(in Input) error {
	if n, ok := in.Flags["top"].(int); ok && (n < 1 || n > 1000) {
		return usagef("--top %d: want 1 to 1000", n)
	}
	return nil
}

func heapTop(in Input) int { return flagInt(in, "top", 10) }

// heapSummary is the result of heap snapshot.
type heapSummary struct {
	Path       string           `json:"path"`
	Bytes      int              `json:"bytes"`
	Version    int              `json:"version"`
	Nodes      int64            `json:"nodes"`
	TotalSize  int64            `json:"totalSize"`
	Classes    int              `json:"classes"`
	TopByCount []heapsnap.Class `json:"topByCount"`
	TopBySize  []heapsnap.Class `json:"topBySize"`
}

func runHeapSnapshot(ctx context.Context, env Env, in Input) (Result, error) {
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	// The output file is opened before the snapshot: a snapshot can take
	// most of the deadline and costs the page a full collection, so a bad
	// path fails first.
	out, err := openHeapOutput(env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	defer out.abort()
	// The snapshot is one reply that can take most of the deadline: the
	// call budget of the session (10 s) would end it early.
	callCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		callCtx = webkit.WithCallBudget(ctx, time.Until(dl))
	}
	res, err := c.Send(callCtx, "Heap.snapshot", nil)
	if err != nil {
		return Result{}, fmt.Errorf("taking the heap snapshot: %w", err)
	}
	s, size, err := saveHeapSnapshot(res, out)
	if err != nil {
		return Result{}, err
	}
	n := heapTop(in)
	sum := heapSummary{
		Path: out.path, Bytes: size, Version: s.Version, Nodes: s.Nodes, TotalSize: s.TotalSize,
		Classes: len(s.Classes), TopByCount: shortNames(s.TopByCount(n)), TopBySize: shortNames(s.TopBySize(n)),
	}
	return Result{Text: heapSnapshotText(sum), Data: sum}, nil
}

// heapOutput is the snapshot file, open for writing before the snapshot.
type heapOutput struct {
	f       *os.File
	path    string
	created bool // removed again when no snapshot is written
	written bool
}

// openHeapOutput resolves p (empty: a new temp file) and opens it for
// writing without changing an existing file yet. It refuses a path that
// is not a regular file (a directory, a named pipe, a device) and a
// directory that does not exist or cannot be written.
func openHeapOutput(env Env, p string) (*heapOutput, error) {
	if p == "" {
		f, err := os.CreateTemp("", "agent-ios-heap-*.json") // mode 0600
		if err != nil {
			return nil, fmt.Errorf("creating an output file in the temp directory: %w", err)
		}
		return &heapOutput{f: f, path: f.Name(), created: true}, nil
	}
	path := env.ResolvePath(p)
	// A named pipe would block the open below until a reader comes.
	if st, err := os.Stat(path); err == nil && !st.Mode().IsRegular() {
		return nil, fmt.Errorf("writing %s: not a regular file (%s)", path, st.Mode().Type())
	}
	created := true
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		created = false
		f, err = os.OpenFile(path, os.O_WRONLY, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return &heapOutput{f: f, path: path, created: created}, nil
}

// write replaces the file's content with data, with mode 0600 (also for
// an existing file, as devtools.WritePrivateFile), and closes it.
func (o *heapOutput) write(data []byte) error {
	o.written = true
	err := o.f.Chmod(0o600)
	if err == nil {
		err = o.f.Truncate(0)
	}
	if err == nil {
		_, err = o.f.Write(data)
	}
	if cerr := o.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", o.path, err)
	}
	return nil
}

// abort closes a file that got no snapshot and removes it when
// openHeapOutput created it.
func (o *heapOutput) abort() {
	if o.written {
		return
	}
	_ = o.f.Close()
	if o.created {
		_ = os.Remove(o.path)
	}
}

// saveHeapSnapshot writes the snapshotData of the Heap.snapshot reply res
// to out and summarizes it. The data is unescaped inside res
// (heapsnap.ReplyData), written from there and parsed from there, so the
// snapshot is held once (review F5); res is overwritten.
func saveHeapSnapshot(res []byte, out *heapOutput) (heapsnap.Snapshot, int, error) {
	data, err := heapsnap.ReplyData(res)
	if err != nil {
		return heapsnap.Snapshot{}, 0, err
	}
	if len(data) == 0 {
		// JavaScriptCore returns an empty string when the JSON outgrew
		// its string builder.
		return heapsnap.Snapshot{}, 0, errors.New("taking the heap snapshot: WebKit returned no snapshot data (the snapshot may be too large to serialize)")
	}
	if err := out.write(data); err != nil {
		return heapsnap.Snapshot{}, 0, err
	}
	s, err := heapsnap.Parse(data)
	if err != nil {
		return heapsnap.Snapshot{}, 0, fmt.Errorf("the snapshot was written to %s, but it cannot be summarized: %w", out.path, err)
	}
	return s, len(data), nil
}

func shortNames(cs []heapsnap.Class) []heapsnap.Class {
	for i := range cs {
		cs[i].Name = heapsnap.ShortName(cs[i].Name)
	}
	return cs
}

// heapName prints a class name of a page on one line: names with quotes,
// control or non-printable characters are quoted.
func heapName(name string) string {
	if q := fmt.Sprintf("%q", name); q[1:len(q)-1] != name {
		return q
	}
	return name
}

func heapSnapshotText(s heapSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Heap snapshot written to %s (%d bytes, JavaScriptCore format version %d).\n", s.Path, s.Bytes, s.Version)
	fmt.Fprintf(&b, "%s, %d bytes self size in total, %s.\n", nounCount(int(s.Nodes), "node"), s.TotalSize, classCount(s.Classes))
	heapClassTable(&b, "Top classes by count:", s.TopByCount)
	heapClassTable(&b, "Top classes by self size:", s.TopBySize)
	return strings.TrimRight(b.String(), "\n")
}

// classCount renders "1 class" or "2 classes".
func classCount(n int) string {
	if n == 1 {
		return "1 class"
	}
	return fmt.Sprintf("%d classes", n)
}

func heapClassTable(b *strings.Builder, title string, cs []heapsnap.Class) {
	fmt.Fprintln(b, title)
	fmt.Fprintf(b, "  %10s  %12s  %s\n", "count", "self bytes", "class")
	for _, c := range cs {
		fmt.Fprintf(b, "  %10d  %12d  %s\n", c.Count, c.Size, heapName(c.Name))
	}
}

func runHeapGC(ctx context.Context, env Env, _ Input) (Result, error) {
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if _, err := c.Send(ctx, "Heap.gc", nil); err != nil {
		return Result{}, fmt.Errorf("garbage collection: %w", err)
	}
	return Result{Text: "Garbage collection done.", Data: map[string]any{"gc": true}}, nil
}

// heapDiffResult is the result of heap diff.
type heapDiffResult struct {
	A          heapFileTotals        `json:"a"`
	B          heapFileTotals        `json:"b"`
	NodesDelta int64                 `json:"nodesDelta"`
	SizeDelta  int64                 `json:"totalSizeDelta"`
	Changed    int                   `json:"changed"`
	Classes    []heapsnap.ClassDelta `json:"classes"`
}

type heapFileTotals struct {
	Path      string `json:"path"`
	Nodes     int64  `json:"nodes"`
	TotalSize int64  `json:"totalSize"`
}

// heapFileMax bounds a file that heap diff reads: the WebSocket read
// limit (transport.DefaultMaxIncoming, 512 MiB), above the direct
// transport's 256 MiB. A Heap.snapshot reply larger than that never
// reaches agent-ios, and the snapshot is one string inside the reply, so
// no snapshot that heap snapshot wrote is larger.
const heapFileMax = transport.DefaultMaxIncoming

// readHeapFile reads and summarizes one snapshot file. Only a regular file
// of at most heapFileMax bytes is read: a named pipe would block and a
// device (/dev/zero) would not end.
func readHeapFile(env Env, p string) (heapsnap.Snapshot, string, error) {
	path := env.ResolvePath(p)
	st, err := os.Stat(path)
	if err != nil {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot: %w", err)
	}
	if !st.Mode().IsRegular() {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot %s: not a regular file (%s)", path, st.Mode().Type())
	}
	if st.Size() > heapFileMax {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot %s: %d bytes, larger than %d bytes, the largest snapshot a page connection can carry", path, st.Size(), heapFileMax)
	}
	f, err := os.Open(path)
	if err != nil {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot: %w", err)
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing
	// The file may have changed since the Stat: read at most one byte
	// beyond the limit to see it.
	data, err := io.ReadAll(io.LimitReader(f, heapFileMax+1))
	if err != nil {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot: %w", err)
	}
	if len(data) > heapFileMax {
		return heapsnap.Snapshot{}, path, fmt.Errorf("reading the heap snapshot %s: larger than %d bytes, the largest snapshot a page connection can carry", path, heapFileMax)
	}
	s, err := heapsnap.Parse(data)
	if err != nil {
		return heapsnap.Snapshot{}, path, fmt.Errorf("%s: %w", path, err)
	}
	return s, path, nil
}

func runHeapDiff(ctx context.Context, env Env, in Input) (Result, error) {
	// heap diff runs in the client (Local), where nothing else enforces
	// the command deadline: the whole diff (reads, parses, the class diff,
	// its sort and the result) ends with ctx, and a run that ctx ended
	// returns its error (exit 124), never a result. Work still running at
	// the deadline is left to the process exit.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	type result struct {
		res Result
		err error
	}
	done := make(chan result, 1) // the goroutine never blocks on the send
	go func() {
		r, err := heapDiff(env, in)
		done <- result{r, err}
	}()
	select {
	case r := <-done:
		// select picks either case when both are ready: a result that
		// arrives at the deadline is dropped.
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("heap diff: %w", err)
		}
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, fmt.Errorf("heap diff: %w", ctx.Err())
	}
}

// heapDiff reads the two snapshot files of in and compares them.
func heapDiff(env Env, in Input) (Result, error) {
	a, pa, err := readHeapFile(env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	b, pb, err := readHeapFile(env, arg(in, 1))
	if err != nil {
		return Result{}, err
	}
	all := heapsnap.Diff(a, b)
	shown := all
	if n := heapTop(in); n < len(shown) {
		shown = shown[:n]
	}
	for i := range shown {
		shown[i].Name = heapsnap.ShortName(shown[i].Name)
	}
	r := heapDiffResult{
		A: heapFileTotals{pa, a.Nodes, a.TotalSize}, B: heapFileTotals{pb, b.Nodes, b.TotalSize},
		NodesDelta: b.Nodes - a.Nodes, SizeDelta: b.TotalSize - a.TotalSize,
		Changed: len(all), Classes: shown,
	}
	if r.Classes == nil {
		r.Classes = []heapsnap.ClassDelta{}
	}
	return Result{Text: heapDiffText(r), Data: r}, nil
}

func heapDiffText(r heapDiffResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Heap diff %s -> %s: nodes %+d (%d -> %d), self size %+d bytes (%d -> %d), %s changed.\n",
		r.A.Path, r.B.Path, r.NodesDelta, r.A.Nodes, r.B.Nodes, r.SizeDelta, r.A.TotalSize, r.B.TotalSize, classCount(r.Changed))
	if len(r.Classes) > 0 {
		if len(r.Classes) < r.Changed {
			fmt.Fprintf(&b, "Top %d by self size change:\n", len(r.Classes))
		}
		fmt.Fprintf(&b, "  %10s  %12s  %s\n", "count", "self bytes", "class")
		for _, c := range r.Classes {
			fmt.Fprintf(&b, "  %+10d  %+12d  %s\n", c.CountDelta, c.SizeDelta, heapName(c.Name))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
