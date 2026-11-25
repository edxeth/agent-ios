package direct

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
)

func openRaw(t *testing.T, tr *Transport, id string) transport.MessageConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := tr.Open(ctx, transport.Page{DeviceID: testUDID, ID: id})
	if err != nil {
		t.Fatalf("Open %s: %v", id, err)
	}
	if _, err := c.Read(ctx); err != nil { // Target.targetCreated
		t.Fatal(err)
	}
	return c
}

func TestPageConnWriteLimitsAndClose(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	c := openRaw(t, tr, "PID:100/1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Write(ctx, make([]byte, MaxOutgoing+1)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("Write above MaxOutgoing: %v, want a refusal", err)
	}
	short, cancelShort := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelShort()
	if _, err := c.Read(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read with nothing to read: %v, want the ctx deadline", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := len(waitMessages(t, d.fake(0), selForwardDidClose, 1)); n != 1 {
		t.Fatalf("_rpc_forwardDidClose: sent %d times, want once", n)
	}
	if _, err := c.Read(ctx); !errors.Is(err, errPageConnClosed) {
		t.Fatalf("Read after Close: %v", err)
	}
	if err := c.Write(ctx, []byte("{}")); !errors.Is(err, errPageConnClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}

// A page connection whose reader stops is ended once its queue is full; the
// device read loop and the other pages go on.
func TestPageConnQueueOverflowEndsOnlyThatPage(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	slow := openRaw(t, tr, "PID:100/1")
	other := openRaw(t, tr, "PID:100/2")
	for range pageQueueLen + 1 {
		d.fake(0).send(selAppSentData, map[string]any{keyDestination: slow.(*pageConn).sender, keyMessageData: []byte("{}")})
	}
	// The last send returns when the bytes are in the pipe; wait until the
	// read loop has queued it, so the test does not read before the overflow.
	select {
	case <-slow.(*pageConn).ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the page connection was not ended when its queue overflowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for range pageQueueLen + 1 {
		if _, err = slow.Read(ctx); err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "fell 4096 messages behind") {
		t.Fatalf("slow page: %v, want it ended for falling behind", err)
	}
	if n := len(waitMessages(t, d.fake(0), selForwardDidClose, 1)); n != 1 {
		t.Fatalf("_rpc_forwardDidClose: for the ended page sent %d times, want 1", n)
	}
	d.fake(0).send(selAppSentData, map[string]any{keyDestination: other.(*pageConn).sender, keyMessageData: []byte("{}")})
	if _, err := other.Read(ctx); err != nil {
		t.Fatalf("other page: %v", err)
	}
}

func TestOpenRefusesUnknownPages(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	listPages(t, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for id, want := range map[string]string{
		"7":          "want <application id>/<page id>",
		"PID:100/x":  "want <application id>/<page id>",
		"PID:100/99": "not in the listing",
		"PID:999/1":  "not in the listing",
	} {
		if _, err := tr.Open(ctx, transport.Page{DeviceID: testUDID, ID: id}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Open %q: %v, want %q", id, err, want)
		}
	}
	if _, err := tr.Pages(ctx, ""); err == nil {
		t.Fatal("Pages without a device id succeeded")
	}
	if n := len(d.fake(0).messages(selForwardSocketSetup)); n != 0 {
		t.Fatalf("%d socket setups sent for refused pages", n)
	}
}

// Concurrent first calls share one dial; Close ends every page connection
// with ErrPageClosed and later calls with ErrTransportClosed.
func TestTransportSharedDialAndClose(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			listPages(t, tr)
		}()
	}
	wg.Wait()
	d.mu.Lock()
	dials := d.dials
	d.mu.Unlock()
	if dials != 1 {
		t.Fatalf("dials = %d, want 1 shared by concurrent callers", dials)
	}
	c := openRaw(t, tr, "PID:100/1")
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Read(ctx); !errors.Is(err, webkit.ErrPageClosed) || !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Read after transport Close: %v", err)
	}
	if _, err := tr.Pages(ctx, testUDID); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Pages after Close: %v", err)
	}
	if tr.Kind() != transport.KindDirect {
		t.Fatalf("Kind = %q", tr.Kind())
	}
}
