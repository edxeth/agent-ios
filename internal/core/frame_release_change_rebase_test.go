package core

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// W3b rebase onto the committed core (docs/notes/w3b.md, "W3b rebase onto
// the committed core"; docs/notes/w1b.md "core-events r2"): the temporary
// Runtime owner of ReleaseFrameContexts acquires and gives back Runtime as
// changes of the desired state, like every other owner, so the context
// replay's check and its Runtime.disable/enable batch never interleave
// with the temporary owner's Runtime.enable or Runtime.disable.
func TestFrameReleaseTemporaryOwnerWaitsForRunningChanges(t *testing.T) {
	const wait = 150 * time.Millisecond
	fr := newFrameReleaseRig(t, 17, "")
	ctx := ctxFor(t, 5*time.Second)
	tmp := SessionOwner(frameReleaseKind, agent.Session)

	// A change runs when the release starts: the temporary owner's
	// acquisition waits for it.
	_, end, err := fr.ps.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatalf("beginChange: %v", err)
	}
	heldCh := make(chan func(error) error, 1) // the change that the frame's release starts
	done := make(chan error, 1)
	go func() {
		began := false
		done <- fr.ps.ReleaseFrameContexts(ctx, fr.epoch, agent.Session, fr.ps.Caller, func(ctx context.Context, c webkit.Caller) error {
			if cc, ok := c.(contextCaller); ok && cc.ec.ID != 0 && !began {
				// A change begins while the temporary owner holds
				// Runtime: its release waits for this change.
				began = true
				_, held, err := fr.ps.State.beginChange(ctx, nil)
				if err != nil {
					return err
				}
				heldCh <- held
			}
			return nil
		})
	}()
	time.Sleep(wait)
	if n := fr.onSecond("Runtime.enable"); n != 0 || slices.Contains(fr.ps.State.AllOwners(), tmp) {
		_ = end(nil)
		t.Fatalf("while another change ran: Runtime.enable %d, owners %q; want no acquisition", n, fr.ps.State.AllOwners())
	}
	if err := end(nil); err != nil {
		t.Fatalf("end: %v", err)
	}
	var held func(error) error
	select {
	case held = <-heldCh:
	case err := <-done:
		t.Fatalf("ReleaseFrameContexts returned (%v) without a release in the frame's new context", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no release in the frame's new context")
	}
	time.Sleep(wait)
	if n := fr.onSecond("Runtime.disable"); n != 0 || !slices.Contains(fr.ps.State.AllOwners(), tmp) {
		_ = held(nil)
		t.Fatalf("while the frame's change ran: Runtime.disable %d, owners %q; want the temporary owner kept", n, fr.ps.State.AllOwners())
	}
	if err := held(nil); err != nil {
		t.Fatalf("end of the frame's change: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ReleaseFrameContexts: %v", err)
	}
	if en, dis := fr.onSecond("Runtime.enable"), fr.onSecond("Runtime.disable"); en != 1 || dis != 1 || slices.Contains(fr.ps.State.AllOwners(), tmp) {
		t.Fatalf("after the release: Runtime.enable %d, Runtime.disable %d, owners %q; want 1, 1 and no temporary owner", en, dis, fr.ps.State.AllOwners())
	}
}

// When the change that gives the temporary ownership back cannot begin
// before ctx ends, the owner still leaves the model (as a lease release
// removes its owners without the change lock), and Runtime becomes
// unknown, so the next change that uses it resets it.
func TestFrameReleaseTemporaryOwnerLeavesWithoutTheChangeLock(t *testing.T) {
	fr := newFrameReleaseRig(t, 17, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tmp := SessionOwner(frameReleaseKind, agent.Session)
	var end func(error) error
	err := fr.ps.ReleaseFrameContexts(ctx, fr.epoch, agent.Session, fr.ps.Caller, func(ctx context.Context, c webkit.Caller) error {
		if cc, ok := c.(contextCaller); ok && cc.ec.ID != 0 && end == nil {
			var err error
			if _, end, err = fr.ps.State.beginChange(ctx, nil); err != nil {
				t.Errorf("beginChange: %v", err)
			}
			cancel() // the release's budget ends while the change runs
		}
		return nil
	})
	if end == nil {
		t.Fatal("the frame's release began no change")
	}
	defer func() { _ = end(nil) }()
	if err == nil {
		t.Fatal("ReleaseFrameContexts returned nil although its ownership change could not begin")
	}
	if slices.Contains(fr.ps.State.AllOwners(), tmp) || fr.ps.State.Needs("Runtime") {
		t.Fatalf("owners %q, Runtime needed %v; want the temporary owner gone", fr.ps.State.AllOwners(), fr.ps.State.Needs("Runtime"))
	}
	fr.ps.State.mu.Lock()
	_, unknown := fr.ps.State.unknown["Runtime"]
	fr.ps.State.mu.Unlock()
	if !unknown {
		t.Fatal("Runtime is not unknown after a removal whose disable was not sent")
	}
}
