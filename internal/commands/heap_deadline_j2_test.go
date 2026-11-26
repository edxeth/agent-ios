package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeClassSnapshot writes a heap snapshot with n classes, each with one
// node of self size size.
func writeClassSnapshot(t *testing.T, path string, n, size int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"version":3,"type":"Inspector","nodes":[0,0,0,0`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, ",%d,%d,%d,0", i, size, i)
	}
	b.WriteString(`],"nodeClassNames":["root"`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `,"class%d"`, i)
	}
	b.WriteString("]}")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Review2 F15: the whole heap diff (reads, parses, heapsnap.Diff, its sort
// and the result) runs under the command deadline: a run whose deadline
// passes before it returns gives the deadline error (exit 124), never a
// result. Deadlines are swept across the run of a diff of many classes, so
// that some of them pass while the classes are compared and sorted.
func TestHeapDiffDeadlineCoversTheWholeDiff(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	writeClassSnapshot(t, a, 150000, 1)
	writeClassSnapshot(t, b, 150000, 2)
	in := Input{Args: []string{a, b}}
	full := time.Duration(1<<63 - 1)
	for range 2 {
		start := time.Now()
		if _, err := runHeapDiff(context.Background(), spEnv{}, in); err != nil {
			t.Fatal(err)
		}
		full = min(full, time.Since(start))
	}
	// slack is the time between the last deadline check of a successful
	// run and the clock read after it returns.
	const slack = 5 * time.Millisecond
	expired := 0
	for pct := 46; pct <= 106; pct += 6 {
		ctx, cancel := context.WithTimeout(context.Background(), full*time.Duration(pct)/100)
		deadline, _ := ctx.Deadline()
		start := time.Now()
		_, err := runHeapDiff(ctx, spEnv{}, in)
		end := time.Now()
		cancel()
		switch {
		case err == nil && end.After(deadline.Add(slack)):
			t.Fatalf("deadline %v (%d%% of a full run of %v): success returned %v after the deadline", deadline.Sub(start), pct, full, end.Sub(deadline))
		case err != nil && !errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("deadline %d%% of %v: %v; want the deadline error", pct, full, err)
		case err != nil:
			expired++
		}
	}
	if expired == 0 {
		t.Fatalf("no run met its deadline (a full run takes %v): the sweep tested nothing", full)
	}
}

// expiringCtx is a context whose deadline passes after its first Err
// call, while its Done channel has not been seen closed: the moment a
// result and the deadline are both ready.
type expiringCtx struct {
	context.Context
	calls int
}

func (c *expiringCtx) Err() error {
	c.calls++
	if c.calls > 1 {
		return context.DeadlineExceeded
	}
	return nil
}

// Review2 F15: a result that arrives as the deadline passes is dropped for
// the deadline error (select may pick the result).
func TestHeapDiffDropsAResultAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	writeClassSnapshot(t, a, 3, 1)
	_, err := runHeapDiff(&expiringCtx{Context: context.Background()}, spEnv{}, Input{Args: []string{a, a}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("heap diff whose deadline passed as its result arrived: %v; want the deadline error", err)
	}
}
