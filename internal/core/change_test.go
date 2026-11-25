package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Tests of DesiredState.Change, the public change path (W4 review finding
// overrides.go:105). page-0 is the active target; scriptedTarget is the
// provisional target page-1, whose setup (Apply) ran before the change.

func setUA(ctx context.Context, d *DesiredState, c webkit.Caller, value string) error {
	return d.Change(ctx, c, func(c webkit.Caller) error {
		d.Put(Entry{Key: "test:ua", Owner: "test:s1", Apply: func(ctx context.Context, s Sender) error {
			_, err := s.Send(ctx, "Page.overrideUserAgent", map[string]string{"value": value})
			return err
		}})
		_, err := c.Send(ctx, "Page.overrideUserAgent", map[string]string{"value": value})
		return err
	})
}

// A change after the provisional setup reaches the provisional target
// before Change returns, and the active target gets it too.
func TestChangeReachesAProvisionalTarget(t *testing.T) {
	p, c, ps := ownersPage(t)
	q := &scriptedTarget{}
	ctx := ctxFor(t, 3*time.Second)
	if err := ps.State.Apply(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := setUA(ctx, ps.State, c, "A"); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	got := q.value
	q.mu.Unlock()
	if got != "A" {
		t.Fatalf("provisional page-1 has user agent %q after the change, want A", got)
	}
	if sent := sentSince(p, 0, "Page.overrideUserAgent"); len(sent) != 1 {
		t.Fatalf("active page-0 got %q, want one Page.overrideUserAgent", sent)
	}
}

// Changes reach a provisional target in the order they began (FIFO), and
// each returns the result of its own send there: A is held at page-1, B
// queues behind it and fails there.
func TestChangeKeepsOrderAndOwnResults(t *testing.T) {
	_, c, ps := ownersPage(t)
	bad := errors.New("page-1 rejected B")
	q := &scriptedTarget{blockValue: "A", failValue: "B", failure: bad, blocked: make(chan struct{}), release: make(chan struct{})}
	ctx := ctxFor(t, 5*time.Second)
	if err := ps.State.Apply(ctx, q); err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- setUA(ctx, ps.State, c, "A") }()
	<-q.blocked
	go func() { second <- setUA(ctx, ps.State, c, "C") }()
	eventually(t, 2*time.Second, "C queued behind A", func() bool { return queuedFor(ps.State, "page-1") > 0 })
	close(q.release)
	if err := <-first; err != nil {
		t.Fatalf("change A: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("change C: %v", err)
	}
	q.mu.Lock()
	got := q.value
	q.mu.Unlock()
	if got != "C" {
		t.Fatalf("page-1 ends with %q, want C of the later change", got)
	}
	if err := setUA(ctx, ps.State, c, "B"); !errors.Is(err, bad) {
		t.Fatalf("change B returned %v, want page-1's failure %v", err, bad)
	}
}

// fn's error is Change's error, and the change ends: the next one begins.
func TestChangeReturnsTheFunctionsError(t *testing.T) {
	_, c, ps := ownersPage(t)
	ctx := ctxFor(t, 2*time.Second)
	bad := errors.New("fn failed")
	if err := ps.State.Change(ctx, c, func(webkit.Caller) error { return bad }); !errors.Is(err, bad) {
		t.Fatalf("Change returned %v, want %v", err, bad)
	}
	if err := ps.State.Change(ctx, c, func(webkit.Caller) error { return nil }); err != nil {
		t.Fatalf("the next change: %v", err)
	}
}

// A Change on the caller that a Change gave runs inside that change
// (Entry.Release functions get such a caller during a lease release); a
// nested change would wait for the outer one forever.
func TestChangeOnAChangeCallerJoinsIt(t *testing.T) {
	_, c, ps := ownersPage(t)
	q := &scriptedTarget{}
	ctx := ctxFor(t, 2*time.Second)
	if err := ps.State.Apply(ctx, q); err != nil {
		t.Fatal(err)
	}
	err := ps.State.Change(ctx, c, func(outer webkit.Caller) error {
		return ps.State.Change(ctx, outer, func(inner webkit.Caller) error {
			if inner != outer {
				t.Errorf("the nested change got another caller")
			}
			ps.State.Put(Entry{Key: "test:ua", Owner: "test:s1", Apply: func(context.Context, Sender) error { return nil }})
			_, err := inner.Send(ctx, "Page.overrideUserAgent", map[string]string{"value": "N"})
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.value != "N" {
		t.Fatalf("page-1 has %q, want N from the nested change", q.value)
	}
}

// namedTarget is a provisional target with an id that answers every call.
type namedTarget struct{ id string }

func (n namedTarget) TargetID() string { return n.id }

func (namedTarget) Send(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage("{}"), nil
}

// A provisional target's setup never waits for a running change (the
// change may wait for the setup: the gate of webkit.Session).
func TestSetupDoesNotWaitForAChange(t *testing.T) {
	_, c, ps := ownersPage(t)
	ctx := ctxFor(t, 3*time.Second)
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ps.State.Change(ctx, c, func(webkit.Caller) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	setup := make(chan error, 1)
	go func() { setup <- ps.State.Apply(ctx, namedTarget{"page-2"}) }()
	select {
	case err := <-setup:
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the setup of page-2 waited for the running change")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
