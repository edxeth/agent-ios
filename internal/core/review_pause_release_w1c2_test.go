package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Hold the first resume before it writes. This is a possible transport
// ErrNotSent path (for example cancellation while waiting for writeSem).
type reviewUnwrittenResume struct {
	webkit.Caller
	entered chan struct{}
	finish  chan struct{}
	first   bool
}

func (c *reviewUnwrittenResume) SendControl(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "Debugger.resume" && !c.first {
		c.first = true
		close(c.entered)
		<-c.finish
		return nil, fmt.Errorf("%w: resume canceled before obtaining the write slot", webkit.ErrNotSent)
	}
	if next, ok := c.Caller.(interface {
		SendControl(context.Context, string, any) (json.RawMessage, error)
	}); ok {
		return next.SendControl(ctx, method, params)
	}
	return c.Send(ctx, method, params)
}

// Source: W1c-core-review-2 C02: release must answer every pause on its own
// target. A client resume that was never sent leaves that pause open.
func TestReviewReleaseDoesNotAbandonAnInflightUnsentPause(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	c := &reviewUnwrittenResume{Caller: h.s.c, entered: make(chan struct{}), finish: make(chan struct{})}
	h.s.c = c
	client := make(chan error, 1)
	go func() { _, err := h.s.Control(ctx, resumeOn("page-2")); client <- err }()
	<-c.entered
	released := make(chan error, 1)
	go func() { released <- h.s.Release(ctx, nil) }()
	var early bool
	var releaseErr error
	select {
	case releaseErr = <-released:
		early = true
	case <-time.After(50 * time.Millisecond):
	}
	close(c.finish)
	if err := <-client; !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("client resume = %v, want ErrNotSent", err)
	}
	if !early {
		releaseErr = <-released
	}
	if releaseErr != nil {
		t.Fatalf("Release: %v", releaseErr)
	}
	paused, _, _, calls := m.state()
	if len(paused) != 0 {
		t.Fatalf("Release returned nil (before client completed: %v), but backend targets %v remain paused; resume calls written = %d", early, paused, calls)
	}
}

// resumePause explicitly supports a newer Debugger.paused event arriving
// before the older resume's reply (its p.seq == seq guard). The release
// barrier must drain that newer pending pause, rather than end the lease.
func TestReviewReleaseAnswersANewerPauseBeforeResumeReply(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	h.f.Handle("Debugger.resume", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		m.calls++
		delete(m.paused, c.Target)
		m.resumes = append(m.resumes, c.Target)
		first := m.calls == 1
		m.mu.Unlock()
		if first {
			m.pause(c.Target)
		}
		return nil, nil
	})
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	paused, resumes, _, _ := m.state()
	if len(paused) != 0 {
		t.Fatalf("Release returned nil but newer pause on %v remains; resume writes %v", paused, resumes)
	}
}
