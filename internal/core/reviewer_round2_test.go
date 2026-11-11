package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Exercise the real probe state transition, not a copied wrapper formula.
// The resulting stall error flows through errors returned by bound calls.
func TestR2ProbeStallErrorPrivacy(t *testing.T) {
	const title = "Private Untagged Page Title"
	done := make(chan struct{})
	defer close(done)
	c := probeStub{endedCaller{done: done}, &webkit.ErrorData{Code: -32000, Message: title}}
	p := newPageConn(c, nil, func(func(context.Context)) bool { return false }, func(string, ...any) {})
	p.mu.Lock()
	p.expected[c] = true
	p.mu.Unlock()
	if err := p.probe(ctxFor(t, time.Second), c); err == nil {
		t.Fatal("fixture probe must fail")
	}
	if p.State() != ConnStalled {
		t.Fatalf("fixture state %v, want stalled", p.State())
	}
	p.mu.Lock()
	err := p.stallErr
	p.mu.Unlock()
	if got := logsafe.Error(err); strings.Contains(got, title) {
		t.Fatalf("the real probe's stored error still exposes page text when rendered for logging: %s", got)
	}
}
