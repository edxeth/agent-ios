package devtools

import (
	"context"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
)

// registryOp runs one operation of session s1 as a registry command does
// (core.WithLeaseHooks) and returns the page's DevTools state.
func (r *rig) registryOp(t *testing.T) *Page {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p *Page
	err := r.m.Do(core.WithLeaseHooks(ctx), r.req, r.ao, "test", core.ClassMutate, false, func(_ context.Context, mo *core.ManagedOp) error {
		var err error
		p, err = For(mo.Page.PS)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Design 4.2 "network requests" (no replay: capture starts with the
// lease, MAP 1.8) and "console" (7.7 test 4: the replayed messages plus
// everything since the lease): requests and messages that come after the
// first command of a lease and before any network or console command are
// captured; a released lease stops both captures and gives up Network and
// Console.
func TestCapturesStartWithTheLease(t *testing.T) {
	r := newRig(t)
	p := r.registryOp(t) // an unrelated first command
	for _, m := range []string{"Network.enable", "Console.enable"} {
		if got := len(received(r.f, m)); got != 1 {
			t.Fatalf("%s sent %d times at the lease start, want 1", m, got)
		}
	}
	r.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"source": "console-api", "level": "log", "text": "early", "timestamp": 1.0}})
	k := p.Console("s1")
	if k == nil {
		t.Fatal("no console capture runs for the lease holder s1")
	}
	eventually(t, "the early message in the buffer", func() bool { m, _ := k.Messages(ConsoleFilter{}); return len(m) == 1 && m[0].Text == "early" })
	r.f.Emit("Network.requestWillBeSent", reqEvent("early", "https://fixture.example/early", 1))
	finish(r.f, "early", 1.5)
	n := p.Netlog("s1")
	if n == nil {
		t.Fatal("no network log runs for the lease holder s1")
	}
	eventually(t, "the early request in the log", func() bool { _, ok := n.Get("page-1:early"); return ok })

	mark := lastSeq(r.f)
	r.release(t)
	if p.Netlog("s1") != nil || p.Console("s1") != nil {
		t.Fatal("a capture of s1 still runs after the release")
	}
	for _, m := range []string{"Network.disable", "Console.disable"} {
		if got := len(receivedAfter(r.f, m, mark)); got != 1 {
			t.Fatalf("%s sent %d times at the release, want 1", m, got)
		}
	}
}
