package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

type sent struct {
	Method string
	Params string
}

type recorder struct {
	mu   sync.Mutex
	sent []sent
}

func (r *recorder) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	p, _ := json.Marshal(params)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sent{method, string(p)})
	return json.RawMessage(`{}`), nil
}

func (r *recorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sent {
		out = append(out, s.Method)
	}
	return out
}

func TestBootstrapPartsJoinInRankOrderAndIsolateThrows(t *testing.T) {
	b := NewBootstrapParts()
	must(t, b.Set("init:1", RankInit, "user()"))
	must(t, b.Set("runtime", RankRuntime, "rt()"))
	must(t, b.Set("dialog", RankDialog, "dlg()"))
	must(t, b.Set("compat", RankCompat, "compat()"))
	must(t, b.Set("init:0", RankInit, "later()"))
	if got, want := b.Names(), []string{"compat", "runtime", "dialog", "init:1", "init:0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	must(t, b.Set("runtime", RankRuntime, "rt2()")) // replacement keeps its place
	script := b.Script()
	for _, frag := range []string{"try {\nrt2()\n} catch (e) {}", "try {\ncompat()\n} catch (e) {}"} {
		if !strings.Contains(script, frag) {
			t.Fatalf("script lacks %q:\n%s", frag, script)
		}
	}
	if strings.Index(script, "compat()") > strings.Index(script, "rt2()") {
		t.Fatal("compat part must run before the runtime")
	}
	if err := b.Set("bad\nname", RankInit, "x"); err == nil {
		t.Fatal("a multi-line part name was accepted")
	}
	b.Remove("runtime")
	if strings.Contains(b.Script(), "rt2()") {
		t.Fatal("removed part is still in the script")
	}
	if NewBootstrapParts().Script() != "" {
		t.Fatal("empty parts must give an empty script")
	}
}

func TestDesiredStateCountsDomainOwners(t *testing.T) {
	d := NewDesiredState()
	if !d.Acquire("capture", "Network") || d.Acquire("legacy", "Network") {
		t.Fatal("only the first owner must report first")
	}
	r := &recorder{}
	if _, err := d.Release(context.Background(), "legacy", r); err != nil {
		t.Fatal(err)
	}
	if len(r.methods()) != 0 {
		t.Fatalf("releasing one of two owners sent %v, want nothing", r.methods())
	}
	if _, err := d.Release(context.Background(), "capture", r); err != nil {
		t.Fatal(err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Network.disable"}) {
		t.Fatalf("last release sent %v, want [Network.disable]", got)
	}
}

func TestDesiredStateHeaderUnionAndReset(t *testing.T) {
	d := NewDesiredState()
	d.Acquire("a", "Network")
	d.Acquire("b", "Network")
	ch := d.SetHeaders("a", map[string]string{"X-A": "1"})
	if !reflect.DeepEqual(ch.Send, map[string]string{"X-A": "1"}) || ch.NeedsNetworkReset {
		t.Fatalf("first header change = %+v", ch)
	}
	ch = d.SetHeaders("b", map[string]string{"X-B": "2"})
	if !reflect.DeepEqual(ch.Send, map[string]string{"X-A": "1", "X-B": "2"}) {
		t.Fatalf("union change = %+v, want the complete effective map", ch)
	}
	if ch = d.SetHeaders("b", map[string]string{"X-B": "2"}); ch.Send != nil || ch.NeedsNetworkReset {
		t.Fatalf("unchanged map = %+v, want no send", ch)
	}
	// An empty map removes X-A; WIP cannot remove a name without a reset.
	ch = d.SetHeaders("a", map[string]string{})
	if ch.Send != nil || !ch.NeedsNetworkReset {
		t.Fatalf("removal = %+v, want a Network reset and no send of an empty value", ch)
	}
	if got := d.EffectiveHeaders(); !reflect.DeepEqual(got, map[string]string{"X-B": "2"}) {
		t.Fatalf("effective = %v", got)
	}
}

func TestDesiredStateReleaseOfHeadersWithoutNetworkOwnerNeedsNoReset(t *testing.T) {
	d := NewDesiredState()
	d.Acquire("a", "Network")
	d.SetHeaders("a", map[string]string{"X-A": "1"})
	r := &recorder{}
	res, err := d.Release(context.Background(), "a", r)
	if err != nil || res.NeedsNetworkReset {
		t.Fatalf("release = %+v, %v; want no reset: Network.disable clears the map", res, err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Network.disable"}) {
		t.Fatalf("sent %v", got)
	}
}

func TestDesiredStateReleaseUndoesOwnerEntries(t *testing.T) {
	d := NewDesiredState()
	d.Acquire("session", "Page")
	d.Acquire("route", "Network")
	d.AddInterception("route", InterceptRule{URL: "*.png", Stage: "request"})
	must(t, d.SetPart("dialogs", "dialog", RankDialog, "d()"))
	must(t, d.SetPart("session", "runtime", RankRuntime, "r()"))
	undone := false
	d.Put(Entry{
		Key: "ua", Owner: "route", Apply: func(context.Context, Sender) error { return nil },
		Release: func(context.Context, Sender) error { undone = true; return nil },
	})
	r := &recorder{}
	if _, err := d.Release(context.Background(), "route", r); err != nil {
		t.Fatal(err)
	}
	if !undone {
		t.Fatal("generic entry was not released")
	}
	if got, want := r.methods(), []string{"Network.removeInterception", "Network.setInterceptionEnabled", "Network.disable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("route release sent %v, want %v", got, want)
	}
	r2 := &recorder{}
	if _, err := d.Release(context.Background(), "dialogs", r2); err != nil {
		t.Fatal(err)
	}
	if len(r2.sent) != 1 || r2.sent[0].Method != "Page.setBootstrapScript" || strings.Contains(r2.sent[0].Params, "d()") || !strings.Contains(r2.sent[0].Params, "r()") {
		t.Fatalf("dialog release sent %+v, want the bootstrap script without the dialog part", r2.sent)
	}
}

// 7.3 test 2: the desired state reaches a provisional target before commit.
func TestDesiredStateReachesProvisionalTargetBeforeCommit(t *testing.T) {
	d := NewDesiredState()
	d.Acquire("session", "Page")
	d.Acquire("capture", "Network")
	d.Acquire("dialogs", "Console")
	d.SetHeaders("capture", map[string]string{"X-Test": "1"})
	d.AddInterception("capture", InterceptRule{URL: "https://example.test/api", Stage: "request"})
	must(t, d.SetPart("session", "runtime", RankRuntime, "window.__rt = 1"))
	must(t, d.SetPart("dialogs", "dialog", RankDialog, "window.__dlg = 1"))

	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{Setup: func(ctx context.Context, tc webkit.TargetCaller) error { return d.Apply(ctx, tc) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.Swap("page-2", 200*time.Millisecond)

	var prov []string
	commit := 0
	for _, r := range f.Log() {
		if r.Method == "Target.didCommitProvisionalTarget" {
			commit = r.Seq
		}
		if r.In && r.Target == "page-2" && commit == 0 {
			prov = append(prov, r.Method)
		}
		if r.Method == "Page.setBootstrapScript" && string(r.Params) != `{"source":""}` && (!strings.Contains(string(r.Params), "__rt") || !strings.Contains(string(r.Params), "__dlg")) {
			t.Fatalf("bootstrap script lacks a part: %s", r.Params)
		}
	}
	// The connection knows none of the new target's agents: the setup
	// first resets the DOM overlay, the needed Console agent and the
	// Debugger, Network and Page agents (the empty bootstrap script is
	// Page's reset), in the same pipelined batch as the enables.
	want := []string{
		"DOM.hideHighlight", "DOM.setInspectModeEnabled", "Console.disable",
		"Debugger.setPauseOnExceptions", "Debugger.setPauseOnDebuggerStatements", "Debugger.disable",
		"Network.disable", "Page.disable", "Page.setBootstrapScript",
		"Page.enable", "Page.setBootstrapScript", "Console.enable", "Network.enable",
		"Network.setExtraHTTPHeaders", "Network.setInterceptionEnabled", "Network.addInterception",
	}
	if !reflect.DeepEqual(prov, want) {
		t.Fatalf("provisional target received before commit %v, want %v", prov, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Design 2.4, extra HTTP headers: a changed effective value is sent with
// the complete effective map. Owners a and b set X-Test to A and B; a sorts
// first, so A is effective. Releasing a makes B effective without removing
// the name, so Release must send the new map (no reset is needed).
func TestDesiredStateReleaseSendsChangedHeaderValue(t *testing.T) {
	d := NewDesiredState()
	d.Acquire("a", "Network")
	d.Acquire("b", "Network")
	d.SetHeaders("a", map[string]string{"X-Test": "A"})
	d.SetHeaders("b", map[string]string{"X-Test": "B"})
	if got := d.EffectiveHeaders(); got["X-Test"] != "A" {
		t.Fatalf("effective = %v, want the value of the owner that sorts first", got)
	}
	r := &recorder{}
	res, err := d.Release(context.Background(), "a", r)
	if err != nil || res.NeedsNetworkReset {
		t.Fatalf("release = %+v, %v; want no reset: the name keeps an owner", res, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) != 1 || r.sent[0].Method != "Network.setExtraHTTPHeaders" || r.sent[0].Params != `{"headers":{"X-Test":"B"}}` {
		t.Fatalf("sent %+v, want one Network.setExtraHTTPHeaders with {X-Test: B}", r.sent)
	}
}
