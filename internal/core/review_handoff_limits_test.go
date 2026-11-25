package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Source: Session.Subscribe's bounded per-subscriber queue contract and
// W1c-core-review-3's explicit requirement to judge handoff memory bounds.
// Use the existing default queue budget as the regression's limit.
func TestReviewPreparedEventsKeepABoundedQueue(t *testing.T) {
	h := &handoff{}
	sub := &connSub{h: func(string, string, json.RawMessage) {}}
	for i := 0; i < 4*webkit.DefaultEventQueue; i++ {
		h.deliver(sub, EventPosition{}, "page-2", "Console.messageAdded", json.RawMessage(`{"message":"replay"}`))
	}
	if len(h.asyncHeld) > webkit.DefaultEventQueue {
		t.Fatalf("prepared handoff retained %d events for one subscriber, exceeding the existing %d-event queue budget; there is no bound/drop path", len(h.asyncHeld), webkit.DefaultEventQueue)
	}
}

// Source: queued subscribers must not delay other subscribers, and
// Manager.Close must stop its background work within shutdown's deadline.
func TestReviewSlowReplaySubscriberDoesNotBlockOthersOrClose(t *testing.T) {
	m := NewManager(ManagerOptions{})
	first := newEmitCallerFor(t)
	next := newEmitCallerFor(t)
	p := newPageConn(first, nil, m.goBackground, t.Logf)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.Subscribe("Console.messageAdded", func(string, json.RawMessage) {
		once.Do(func() { close(entered) })
		<-release
	})
	p.prepare(next)
	next.emit("before-fast-subscriber")
	fast := make(chan struct{}, 1)
	p.Subscribe("Console.messageAdded", func(string, json.RawMessage) {
		select {
		case fast <- struct{}{}:
		default:
		}
	})
	next.emit("fast-event")
	if err := p.swap(next); err != nil {
		t.Fatalf("swap: %v", err)
	}
	<-entered
	fastBlocked := false
	select {
	case <-fast:
	case <-time.After(100 * time.Millisecond):
		fastBlocked = true
	}
	closed := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { closed <- m.Close(ctx) }()
	closeBlocked := false
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-time.After(300 * time.Millisecond):
		closeBlocked = true
	}
	close(release) // always release the test collaborator before failing.
	if closeBlocked {
		closeErr = <-closed
	}
	if closeErr != nil {
		t.Errorf("Manager.Close = %v", closeErr)
	}
	p.close()
	if fastBlocked || closeBlocked {
		t.Fatalf("one slow replay subscriber blocked independent subscriber=%v, Manager.Close beyond its deadline=%v", fastBlocked, closeBlocked)
	}
}

// Source: C11: Close tracks watches; no new connection/watch may outlive
// an owner that already returned from Close.
func TestReviewAttachRacingCloseDoesNotInstallAfterShutdown(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.Handle("Page.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		once.Do(func() { close(entered) })
		<-release
		return nil, nil
	})
	attached := make(chan error, 1)
	go func() { attached <- evalOnce(ctxFor(t, 5*time.Second), r, agent) }()
	<-entered
	if err := r.m.Close(ctxFor(t, time.Second)); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	err := <-attached
	pages := r.m.allPages()
	// The fake server sees a closed connection asynchronously: give it up
	// to a second to settle at 0. An escaped connection stays open.
	conns := r.f.OpenConnections()
	for end := time.Now().Add(time.Second); conns != 0 && time.Now().Before(end); conns = r.f.OpenConnections() {
		time.Sleep(10 * time.Millisecond)
	}
	// Manager.Close is idempotent; explicitly clean any escaped connection.
	for _, pg := range pages {
		pg.PS.Sched.Close()
		pg.conn.close()
	}
	if err == nil || conns != 0 {
		t.Fatalf("attach finished after Close: err=%v, installed pages=%d, live connections=%d; post-Close watch falls back to untracked goroutine", err, len(pages), conns)
	}
}

// Source: design 2.5: a connection ended by a peer is lost and needs
// --takeover, never an automatic reopen. Exercise the touched fresh-probe
// path against an endpoint that steals an existing inspector on each dial.
func TestReviewFreshProbeLossRequiresTakeoverOnNextCommand(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatal(err)
	}
	pg := r.m.FindAttached(r.f.URL)
	pg.conn.mu.Lock()
	pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
	pg.conn.mu.Unlock()
	peerReady := make(chan func(), 1)
	r.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if c.Conn == 2 && p.Expression == "1" {
			peer, err := webkit.DialWebSocket(context.Background(), r.f.URL, transport.Limits{Routed: true})
			if err != nil {
				t.Errorf("peer inspector: %v", err)
				peerReady <- func() {}
			} else {
				peerReady <- func() { _ = peer.Close() }
			}
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
	if !errors.Is(err, ErrLost) {
		t.Fatalf("first reopen = %v, want ErrLost from the new connection's probe", err)
	}
	var closePeer func()
	select {
	case closePeer = <-peerReady:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not connect")
	}
	defer closePeer()
	before := r.f.ConnectionCount()
	err = evalOnce(ctxFor(t, 5*time.Second), r, agent) // no Takeover
	after := r.f.ConnectionCount()
	if !errors.Is(err, ErrLost) || after != before {
		t.Fatalf("after reported peer loss, command without --takeover: err=%v, state=%s, connections %d -> %d; it silently reopened and stole the peer inspector", err, pg.ConnState(), before, after)
	}
}
