package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Core-events round 9a: a cleanup send forwarded after the commit of the
// target it is forwarded to. A change forwards a successful call only
// after the call to its own target returned, outside the model's lock, so
// a provisional target can commit between the reply and the enqueue
// (changeSender.Send). The forwarded cleanup is then the only record of
// the new active target's obligation, as at the commit (round 8), and it
// must keep the cleanup pending until it succeeds (design 2.4: after a
// release, the released state is gone from the active backend, or a
// cleanup is pending).

// pauseAfterReply pauses a send of method whose reply pause accepts, after
// the reply and before the caller sees it: the caller is descheduled after
// it read its reply, before the change enqueues the forwarded send.
type pauseAfterReply struct {
	webkit.Caller
	method        string
	pause         func(error) bool
	reply, resume chan struct{}
}

func (c *pauseAfterReply) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	res, err := c.Caller.Send(ctx, method, params)
	if method == c.method && c.pause(err) {
		close(c.reply)
		<-c.resume
	}
	return res, err
}

// nextChangeSendsQueuedCleanup runs the background reset of the agents a
// release left unknown (resetUnknown), a change: it first sends the
// queued cleanup to the active target page-2, which removes the released
// header and ends the mark, so the change resets no agent again: page-2
// gets exactly that one Network.disable, and nothing stays unknown.
func nextChangeSendsQueuedCleanup(t *testing.T, r *rig, pg *Page, b *headerBackend) {
	t.Helper()
	disables := disablesOn(r.f, "page-2")
	if err := resetUnknown(ctxFor(t, 10*time.Second), pg); err != nil {
		t.Fatalf("the next change: %v", err)
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the active target page-2 keeps the released header X-Released: %s after the next change", v)
	}
	if n := disablesOn(r.f, "page-2") - disables; n != 1 {
		t.Fatalf("the next change sent %d Network.disable to page-2, want 1 (the queued cleanup, no further reset)", n)
	}
	if got := pg.PS.State.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the queued cleanup succeeded = %v, want none", got)
	}
}

// refuseNetworkDisableOn makes target refuse Network.disable because the
// agent is not enabled; the other targets run it (headerBackend).
func refuseNetworkDisableOn(r *rig, b *headerBackend, target string) {
	r.f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == target {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Network domain must be enabled"}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.hdr, c.Target)
		return nil, nil
	})
}

// Source: design 2.4 and round-8 acceptance 1: a released session's state
// is absent from the active backend or its cleanup remains pending. The
// release's Network.disable gets its reply from page-1 (a success, or a
// refusal because the agent is not enabled, which the release counts as
// done), page-2 commits before the change forwards it, and the release
// deadline ends before the forwarded send goes out. The reviewer's
// reproduction of round 8 (TestReviewR8CleanupEnqueuedAfterCommit) is the
// "success" case. The next change then sends the queued cleanup, which
// ends the mark: one Network.disable, no further reset
// (nextChangeSendsQueuedCleanup).
func TestCleanupForwardedAfterTheCommitStaysPending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		notEnabled bool
	}{{"success", false}, {"not enabled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r, pg, b := provisionalWithReleasedHeader(t)
			d := pg.PS.State
			pause := func(err error) bool { return err == nil }
			if tc.notEnabled {
				refuseNetworkDisableOn(r, b, "page-1")
				pause = notEnabled
			}
			sender := &pauseAfterReply{Caller: controlCaller{pg.conn}, method: "Network.disable", pause: pause, reply: make(chan struct{}), resume: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			type result struct {
				failed []string
				err    error
			}
			released := make(chan result, 1)
			go func() {
				failed, err := releaseOwners(ctx, pg.PS, "s1", sender)
				released <- result{failed, err}
			}()
			select {
			case <-sender.reply:
			case <-time.After(3 * time.Second):
				t.Fatal("active target did not reply")
			}
			// No cleanup has reached the provisional record at commitTarget yet.
			commit(t, r, d, "page-2")
			<-ctx.Done()
			close(sender.resume)
			got := <-released
			if !errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("release = %v, want deadline", got.err)
			}
			pending := d.forgetSession("s1", true, got.failed)
			d.mu.Lock()
			ts := d.syncs["page-2"]
			queued := len(ts.queue)
			d.mu.Unlock()
			oraclePending := cleanupPending(pg, "page-2")
			t.Logf("release=%v failed=%v queued=%d header=%q unknown=%v oraclePending=%v", got.err, got.failed, queued, b.header("page-2", "X-Released"), d.unknownDomains(), oraclePending)
			if b.header("page-2", "X-Released") != "" && !pending {
				t.Fatal("release ended with its header on the active backend and no pending cleanup: the successful old-target send queued after the commit")
			}
			nextChangeSendsQueuedCleanup(t, r, pg, b)
		})
	}
}

// Source: design 2.4, and resetAgent's contract that it removes only the
// mark it read. The reset of an unknown Network agent (a release whose
// cleanup page-1 refused) sends Network.disable to page-1, and page-2,
// whose setup snapshot still has the released header, commits after that
// reply: before the change enqueues the forwarded disable for page-2, or
// after. The reset then removes its mark, but the forwarded disable is
// still unsent when the deadline ends the change, so it must keep the
// cleanup pending until it succeeds. The next change sends it, which ends
// the mark: one Network.disable, no further reset.
func TestResetMarkEndsWithCleanupStillForwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		// afterEnqueue pauses after the change enqueued the forwarded
		// disable (page-2 commits with it queued) instead of before.
		afterEnqueue bool
	}{{"commit before the enqueue", false}, {"commit after the enqueue", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r, pg, b := provisionalWithReleasedHeader(t)
			d := pg.PS.State
			b.refuseOn("page-1")
			failed, err := releaseOwners(ctxFor(t, 10*time.Second), pg.PS, "s1", controlCaller{pg.conn})
			if err == nil || len(failed) != 1 || failed[0] != "Network" {
				t.Fatalf("fixture: release = %v, failed %v; want page-1's refusal of the Network cleanup", err, failed)
			}
			if !d.forgetSession("s1", true, failed) || b.header("page-2", "X-Released") != "old" {
				t.Fatalf("fixture: want Network pending and the released header on page-2 (unknown %v)", d.unknownDomains())
			}
			b.refuseOn("")
			pause := &pauseAfterReply{method: "Network.disable", pause: func(err error) bool { return err == nil }, reply: make(chan struct{}), resume: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			var c webkit.Caller = controlCaller{pg.conn}
			if !tc.afterEnqueue {
				pause.Caller, c = c, pause
			}
			cs, end, err := d.beginChange(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			var s Sender = cs
			if tc.afterEnqueue {
				pause.Caller, s = cs, pause
			}
			reset := make(chan error, 1)
			go func() { reset <- end(resetAgent(ctx, pg.PS, s, "Network")) }()
			select {
			case <-pause.reply:
			case <-time.After(3 * time.Second):
				t.Fatal("page-1 did not reply to the reset's Network.disable")
			}
			commit(t, r, d, "page-2")
			<-ctx.Done()
			close(pause.resume)
			if err := <-reset; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("reset = %v, want the deadline while its forwarded disable waits", err)
			}
			if v := b.header("page-2", "X-Released"); v != "" && !d.isUnknown("Network") {
				t.Fatalf("page-2 is active with the released header X-Released: %s and its cleanup still queued, but no cleanup is pending: the reset removed its mark", v)
			}
			nextChangeSendsQueuedCleanup(t, r, pg, b)
		})
	}
}

// committedRecord makes page-2, whose setup ran with nopCaller, the active
// target after page-1, with nothing queued for it, and returns a change
// that began on page-1 and its record of page-2.
func committedRecord(t *testing.T) (*DesiredState, *changeSender, *targetSync) {
	t.Helper()
	d := NewDesiredState()
	d.mu.Lock()
	d.active = "page-1"
	ts := d.registerLocked("page-2", nopCaller{})
	d.mu.Unlock()
	d.commitTarget("page-2")
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("fixture: unknown domains after a commit with nothing queued = %v", got)
	}
	return d, &changeSender{d: d, target: "page-1"}, ts
}

// enqueueCleanup queues a cleanup send of Network for c's registered
// targets, as changeSender.Send does after its own target answered.
func enqueueCleanup(c *changeSender, method string) *queued {
	d := c.d
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ver++
	c.enqueueLocked(d.ver, "Network", forwardCall(method, nil))
	return c.pending[len(c.pending)-1].q
}

// Source: markUnsentCleanupLocked's rule, which markActiveCleanupLocked
// applies at the enqueue: for each domain the last cleanup send on its way
// to the active target carries the mark, so the success of an earlier one
// does not end it while a later one is still queued.
func TestLastCleanupQueuedForActiveTargetCarriesTheMark(t *testing.T) {
	d, c, ts := committedRecord(t)
	first := enqueueCleanup(c, "Network.disable")
	second := enqueueCleanup(c, "Network.enable")
	if first.mark == 0 || second.mark == 0 || !d.isUnknown("Network") {
		t.Fatalf("marks %d and %d, unknown %v: want each cleanup queued for the active target to mark Network", first.mark, second.mark, d.unknownDomains())
	}
	if err := d.pump(context.Background(), "page-2", ts, func() bool { return first.completed }); err != nil {
		t.Fatal(err)
	}
	if !first.completed || !d.isUnknown("Network") {
		t.Fatalf("after the first cleanup was sent (completed %v): unknown %v, want Network pending while the second is queued", first.completed, d.unknownDomains())
	}
	if err := d.drain(context.Background(), "page-2"); err != nil {
		t.Fatal(err)
	}
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after both cleanup sends succeeded = %v, want none", got)
	}
}

// Source: markUnsentCleanupLocked's rule: a domain that is unknown already
// keeps its mark, because that obligation (here a release whose cleanup
// was never sent, forgetSession) may be older than the queued send, so
// the send's success does not end it; the next reset does.
func TestCleanupQueuedForActiveTargetKeepsAnOlderMark(t *testing.T) {
	d, c, ts := committedRecord(t)
	d.mu.Lock()
	d.markUnknownLocked("Network")
	older := d.unknown["Network"]
	d.mu.Unlock()
	q := enqueueCleanup(c, "Network.disable")
	if q.mark != 0 {
		t.Fatalf("the queued cleanup carries mark %d, want none: the older mark %d stays", q.mark, older)
	}
	if err := d.pump(context.Background(), "page-2", ts, nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	got := d.unknown["Network"]
	d.mu.Unlock()
	if got != older {
		t.Fatalf("Network's mark after the queued cleanup succeeded = %d, want the older mark %d", got, older)
	}
}
