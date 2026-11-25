package webkit

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

func clientCtxIDs(c *Client) (string, []int) {
	target, list := c.ContextInventory()
	var ids []int
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	return target, ids
}

// W3b final repair round 1, finding 2: a Client (core's connection for an
// explicit ws:// URL) keeps the execution contexts that its target
// reports, from the connection's start and before any handler runs, with
// the rules of Session.ExecutionContexts: a frame's detach or navigation
// drops its contexts, a main-frame navigation drops all of them, and the
// CDP destroy and clear events apply. When the client follows a new
// target, the old target's contexts are gone, and a call addressed to the
// old target (WithTarget) is not sent.
func TestClientContextInventory(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	created := func(id int, frame, typ string) {
		f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "frameId": frame, "type": typ}})
	}
	f.Handle("Runtime.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		created(1, "main", "normal")
		created(7, "kid", "normal")
		created(8, "kid", "user")
		created(9, "ad", "normal")
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Send(ctx, "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	// The reports precede the reply, and the read loop records them first.
	if target, ids := clientCtxIDs(c); target != "page-1" || !reflect.DeepEqual(ids, []int{1, 7, 8, 9}) {
		t.Fatalf("inventory after Runtime.enable = %s %v, want page-1 [1 7 8 9]", target, ids)
	}
	_, list := c.ContextInventory()
	if want := (ExecutionContext{ID: 8, FrameID: "kid", Type: "user"}); list[2] != want {
		t.Fatalf("context 8 = %+v, want %+v", list[2], want)
	}
	// A barrier call: the events emitted before it are applied when it returns.
	barrier := func() {
		t.Helper()
		if _, err := c.Send(ctx, "Runtime.releaseObjectGroup", map[string]string{"objectGroup": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	f.Emit("Page.frameDetached", map[string]any{"frameId": "ad"})
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "kid", "parentId": "main"}})
	barrier()
	if _, ids := clientCtxIDs(c); !reflect.DeepEqual(ids, []int{1}) {
		t.Fatalf("inventory after the frames detached and navigated = %v, want [1]", ids)
	}
	created(12, "kid", "normal")
	created(13, "kid", "normal")
	f.Emit("Runtime.executionContextDestroyed", map[string]any{"executionContextId": 12})
	barrier()
	if _, ids := clientCtxIDs(c); !reflect.DeepEqual(ids, []int{1, 13}) {
		t.Fatalf("inventory after a destroyed context = %v, want [1 13]", ids)
	}
	f.Emit("Runtime.executionContextsCleared", map[string]any{})
	barrier()
	if _, ids := clientCtxIDs(c); len(ids) != 0 {
		t.Fatalf("inventory after executionContextsCleared = %v, want none", ids)
	}
	created(20, "main", "normal")
	created(21, "kid", "normal")
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main"}})
	barrier()
	if _, ids := clientCtxIDs(c); len(ids) != 0 {
		t.Fatalf("inventory after a main-frame navigation = %v, want none", ids)
	}
	created(30, "main", "normal")
	barrier()
	f.Swap("page-2", 0)
	deadline := time.Now().Add(time.Second)
	for target, _ := c.ContextInventory(); target != "page-2"; target, _ = c.ContextInventory() {
		if time.Now().After(deadline) {
			t.Fatal("the client did not follow page-2")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ids := clientCtxIDs(c); len(ids) != 0 {
		t.Fatalf("inventory of page-2 = %v, want none (page-1's are gone)", ids)
	}
	n := len(f.ReceivedMethod("Runtime.evaluate"))
	_, err = c.Send(WithTarget(ctx, "page-1"), "Runtime.evaluate", map[string]any{"expression": "1", "contextId": 30})
	if !errors.Is(err, ErrTargetSwapped) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("call addressed to page-1: %v, want not sent with ErrTargetSwapped", err)
	}
	if _, err := c.Send(WithTarget(ctx, "page-2"), "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatalf("call addressed to page-2: %v", err)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != n+1 || got[n].Target != "page-2" {
		t.Fatalf("Runtime.evaluate records %+v after %d: want one more, on page-2", got, n)
	}
}

// A Client without Target routing keeps its contexts under the target ""
// and ignores calls without an addressed target.
func TestClientContextInventoryWithoutTargetRouting(t *testing.T) {
	m := testutil.NewMockServer()
	defer m.Close()
	m.HandleFunc("Runtime.enable", map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, m.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := m.SendEvent("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 4, "frameId": "kid", "type": "normal"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		target, ids := clientCtxIDs(c)
		if target == "" && reflect.DeepEqual(ids, []int{4}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("direct-mode inventory = %q %v, want \"\" [4]", target, ids)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
