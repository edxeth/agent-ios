package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// shotFake answers what a capture needs: the viewport size, a PNG from
// Page.snapshotRect, and the runtime's snapshot and overlay calls; seq
// records the order of overlay, capture and overlay removal.
type shotFake struct {
	h    *rtHarness
	mu   sync.Mutex
	seq  []string
	png  []byte
	fail bool
}

func pngOf(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, c)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newShotFake(t *testing.T) *shotFake {
	t.Helper()
	h := newActionHarness(t)
	s := &shotFake{h: h, png: pngOf(t, color.Black)}
	note := func(e string) { s.mu.Lock(); s.seq = append(s.seq, e); s.mu.Unlock() }
	h.page.set("snapshot", okEnv(map[string]any{
		"tree": []any{}, "nextRef": 3, "doc": "doc-1",
		"refs": []any{map[string]any{"ref": "e1", "role": "button", "name": "Save"}, map[string]any{"ref": "e2", "role": "link", "name": "Below the fold"}},
	}))
	h.page.set("overlay", okEnv(map[string]any{"refs": []string{"e1"}, "area": map[string]any{"x": 0, "y": 0, "width": 3, "height": 2}}))
	h.page.set("overlayClear", okEnv(true))
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			if m[1] == "overlay" || m[1] == "overlayClear" {
				note(m[1])
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		if strings.Contains(p.Expression, "innerWidth") {
			return map[string]any{"result": map[string]any{"type": "string", "value": `{"width":3,"height":2}`}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0}}, nil
	})
	h.f.Handle("Page.snapshotRect", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		note("capture")
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fail {
			return nil, &fakewebkit.Error{Code: -32000, Message: "capture failed"}
		}
		return map[string]any{"dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(s.png)}, nil
	})
	return s
}

func (s *shotFake) order() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := strings.Join(s.seq, ",")
	s.seq = nil
	return o
}

// Design 3.9: --annotate snapshots, draws labels for the refs in the
// capture area, captures, removes the overlay, and lists "[N] @eN role
// name" for each label; the overlay also goes when the capture fails.
func TestScreenshotAnnotate(t *testing.T) {
	s := newShotFake(t)
	path := filepath.Join(t.TempDir(), "a.png")
	out := s.h.mustRun(t, "screenshot", "--annotate", path)
	want := "Screenshot saved to " + path + " (3x2 px, "
	if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "\n[1] @e1 button \"Save\"") || strings.Contains(out, "@e2") {
		t.Fatalf("output %q, want the file and one label [1] @e1 button \"Save\"", out)
	}
	if o := s.order(); o != "overlay,capture,overlayClear" {
		t.Fatalf("order %q, want overlay,capture,overlayClear", o)
	}
	if a := s.h.page.last("overlay"); strings.Join(toStrings(a["refs"]), ",") != "e1,e2" || a["full"] != false {
		t.Fatalf("overlay args %v, want both refs of the snapshot and full false", a)
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, s.png) {
		t.Fatalf("written file: %v", err)
	}
	s.mu.Lock()
	s.fail = true
	s.mu.Unlock()
	if code, _, _ := s.h.run("screenshot", "--annotate", "-o", path); code == 0 {
		t.Fatal("a failed capture succeeded")
	}
	if o := s.order(); o != "overlay,capture,overlayClear" {
		t.Fatalf("order after a failed capture %q, want the overlay removed", o)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Design 3.9: --if-changed compares the SHA-256 of the PNG with the
// session's previous capture and writes nothing when it is the same.
func TestScreenshotIfChanged(t *testing.T) {
	s := newShotFake(t)
	path := filepath.Join(t.TempDir(), "s.png")
	if out := s.h.mustRun(t, "screenshot", "--if-changed", "-o", path); !strings.HasPrefix(out, "Screenshot saved to "+path) {
		t.Fatalf("first capture: %q", out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if out := s.h.mustRun(t, "screenshot", "--if-changed", "-o", path); out != "unchanged" {
		t.Fatalf("identical capture: %q, want unchanged", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unchanged capture wrote the file (%v)", err)
	}
	s.mu.Lock()
	s.png = pngOf(t, color.White)
	s.mu.Unlock()
	if out := s.h.mustRun(t, "screenshot", "--if-changed", "-o", path); !strings.HasPrefix(out, "Screenshot saved to "+path) {
		t.Fatalf("changed capture: %q", out)
	}
}

func TestScreenshotUsage(t *testing.T) {
	s := newShotFake(t)
	for _, bad := range [][]string{{"screenshot", "--full-page", "--selector", "#a"}, {"screenshot", "a.png", "-o", "b.png"}, {"screenshot", "--frobnicate"}} {
		if code, _, errOut := s.h.run(bad...); code != ExitUsage {
			t.Fatalf("%v: exit %d (%q), want the usage exit", bad, code, errOut)
		}
	}
}

// Design 3.9 and 2.6 (review finding 5): when the capture never answers
// and the command's deadline passes, the command exits with the timeout
// code at once, and a cleanup operation of the same lease removes the
// overlay after the command's operation ended.
func TestScreenshotAnnotateRemovesOverlayAfterDeadline(t *testing.T) {
	s := newShotFake(t)
	s.h.f.Stall("Page.snapshotRect")
	start := time.Now()
	code, _, errOut := s.h.run("--timeout", "1s", "screenshot", "--annotate", filepath.Join(t.TempDir(), "a.png"))
	if code != ExitTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %v (%q), want %d right after the 1 s deadline", code, time.Since(start), errOut, ExitTimeout)
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		s.mu.Lock()
		o := strings.Join(s.seq, ",")
		s.mu.Unlock()
		if o == "overlay,overlayClear" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("calls %q, want the overlay removed after the deadline", o)
		}
	}
	// A later lease epoch refuses the cleanup: nothing is sent.
	s.order()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.h.ps.Sched.Begin(ctx, core.OpSpec{Name: "screenshot", Class: core.ClassMutate, Session: "agent", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	w := &w3bPage{ps: s.h.ps, op: op, session: "agent", epoch: 1}
	op.Cancel(core.ErrDeadline)
	if err := w.removeOverlay(op.Context(), nil); err != nil {
		t.Fatal(err)
	}
	s.h.ps.Sched.SetLease("agent", 2)
	op.End()
	// The slot is released after the cleanup ran, so the next operation's
	// admission shows that it is over.
	next, err := s.h.ps.Sched.Begin(ctx, core.OpSpec{Name: "next", Class: core.ClassMutate, Session: "agent", Epoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	next.End()
	if o := s.order(); o != "" {
		t.Fatalf("cleanup under an old epoch sent %q", o)
	}
}

// W3b-R2 finding 3 (design 2.10, 3.9): the overlay of a capture whose
// deadline passed is removed before any queued successor runs. The
// successor queues while the capture is in flight; it must see the
// overlay already cleared. Before the fix the cleanup was a new operation
// queued behind it.
func TestScreenshotAnnotateCleanupRunsBeforeQueuedSuccessor(t *testing.T) {
	s := newShotFake(t)
	s.h.f.Stall("Page.snapshotRect")
	done := make(chan int, 1)
	go func() {
		code, _, _ := s.h.run("--timeout", "1s", "screenshot", "--annotate", filepath.Join(t.TempDir(), "a.png"))
		done <- code
	}()
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		drawn := slices.Contains(s.seq, "overlay")
		s.mu.Unlock()
		if drawn {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the capture never drew its overlay")
		}
	}
	session, epoch := s.h.ps.Sched.Lease()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.h.ps.Sched.Begin(ctx, core.OpSpec{Name: "successor", Class: core.ClassRead, Session: session, Epoch: epoch})
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	s.mu.Lock()
	s.seq = append(s.seq, "successor")
	s.mu.Unlock()
	op.End()
	if code := <-done; code != ExitTimeout {
		t.Fatalf("capture exit %d, want %d", code, ExitTimeout)
	}
	if o := s.order(); o != "overlay,overlayClear,successor" {
		t.Fatalf("calls %q, want the overlay removed before the queued successor ran", o)
	}
}
