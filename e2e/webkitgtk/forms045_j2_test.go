//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/refs"
	iwdpruntime "github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// optionCut is the documented rule of option_not_found's option texts
// (review2 F16): a value or label of more than 100 Unicode code points is
// its first 100 code points and "…"; a shorter one is shown whole.
func optionCut(s string) string {
	n := 0
	for i := range s {
		if n == 100 {
			return s[:i] + "\u2026"
		}
		n++
	}
	return s
}

// listedOptions runs select with args and returns the [value, label]
// pairs of option_not_found's "Available options", each line checked to
// be clean UTF-8 (no U+FFFD).
func listedOptions(t *testing.T, ps *core.PageSession, args ...string) [][2]string {
	t.Helper()
	code, _, errOut := act(context.Background(), ps, append([]string{"--timeout", "10s", "select"}, args...)...)
	lines := strings.Split(strings.TrimRight(errOut, "\n"), "\n")
	if code != 1 || len(lines) < 2 || !strings.Contains(lines[0], "option_not_found") || lines[1] != "Available options:" {
		t.Fatalf("select %q: exit %d, stderr %q; want option_not_found with the options", args, code, errOut)
	}
	var out [][2]string
	for _, l := range lines[2:] {
		if !utf8.ValidString(l) || strings.ContainsRune(l, utf8.RuneError) {
			t.Fatalf("select %q: option line %q is not clean UTF-8", args, l)
		}
		var o struct{ Value, Label string }
		if err := json.Unmarshal([]byte(l), &o); err != nil {
			t.Fatalf("select %q: option line %q: %v", args, l, err)
		}
		out = append(out, [2]string{o.Value, o.Label})
	}
	return out
}

// addSelect adds <select id=id> with the options (value, text) to the page.
func addSelect(t *testing.T, id string, opts [][2]string) {
	t.Helper()
	j, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	eval(t, `(function () { var o = document.getElementById(`+strconvQuote(id)+`); if (o) { o.remove(); }
  var s = document.createElement("select"); s.id = `+strconvQuote(id)+`;
  `+string(j)+`.forEach(function (p) { var e = document.createElement("option"); e.value = p[0]; e.textContent = p[1]; s.appendChild(e); });
  document.body.appendChild(s); return true; })()`)
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Review2 F16: option texts are cut on code points in the page and in Go,
// by one rule: an option of 70 emoji (140 UTF-16 code units) is listed
// whole; one of 150 emoji is cut to 100 and "…", never to half an emoji.
func TestForms045OptionListUnicode(t *testing.T) {
	ps := loadForms(t)
	e70, e150 := strings.Repeat("😀", 70), strings.Repeat("😀", 150)
	addSelect(t, "emoji", [][2]string{{"v", e70}, {e150, "w"}, {"x", "a" + e150}})
	got := listedOptions(t, ps, "#emoji", "absent")
	want := [][2]string{{"v", e70}, {strings.Repeat("😀", 100) + "…", "w"}, {"x", "a" + strings.Repeat("😀", 99) + "…"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed options %q, want %q", got, want)
	}
}

// optionText is a generated option value or label: code points from
// one-byte to four-byte UTF-8 (UTF-16 surrogate pairs), of lengths around
// the 100 code point cut. Its texts have no white space and no character
// that the forms module drops from a label (nrm), so that the page's label
// is the text.
type optionText string

var optionRunes = []rune("az09_é日😀𝄞🇫🇷\u0301\u4e2d€")

func (optionText) Generate(r *rand.Rand, _ int) reflect.Value {
	var n int
	switch r.Intn(3) {
	case 0:
		n = 1 + r.Intn(10)
	case 1:
		n = 95 + r.Intn(11) // 95..105: either side of the cut
	default:
		n = 106 + r.Intn(100)
	}
	var b strings.Builder
	for range n {
		b.WriteRune(optionRunes[r.Intn(len(optionRunes))])
	}
	return reflect.ValueOf(optionText(b.String()))
}

// Review2 F16, generated: for random option values and labels, the page's
// cut and Go's cut together list each text by the documented rule
// (optionCut), never with U+FFFD or a lost "…".
func TestForms045OptionListUnicodeProperty(t *testing.T) {
	ps := loadForms(t)
	prop := func(texts [8]optionText) bool {
		var opts, want [][2]string
		for i := 0; i < len(texts); i += 2 {
			v, l := string(texts[i]), string(texts[i+1])
			opts = append(opts, [2]string{v, l})
			want = append(want, [2]string{optionCut(v), optionCut(l)})
		}
		addSelect(t, "gen", opts)
		got := listedOptions(t, ps, "#gen", "§absent§")
		if !reflect.DeepEqual(got, want) {
			t.Logf("listed options %q\nwant %q", got, want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 40}); err != nil {
		t.Fatal(err)
	}
}

// comboOp is one step of a generated combobox sequence on forms045.html's
// #combo (a Radix-like combobox whose list mounts after a click).
type comboOp int

const (
	comboPick     comboOp = iota // select an enabled option: it is picked and the list closes
	comboAbsent                  // select an option the list lacks: option_not_found
	comboDisabled                // select the disabled option Kiwi: disabled
	comboCancel                  // a select whose client goes away after step 1 opened the list
	comboUserOpen                // the user opens the list (a click)
	comboUserEsc                 // the user closes the list (Escape)
	comboReset                   // the runtime resets its page maps (__iwdp.reset)
	comboOps
)

var comboOpNames = [...]string{"pick", "absent", "disabled", "cancel", "user-open", "user-escape", "reset"}

func (o comboOp) String() string { return comboOpNames[o] }

type comboSeq []comboOp

func (comboSeq) Generate(r *rand.Rand, _ int) reflect.Value {
	s := make(comboSeq, 3+r.Intn(8))
	for i := range s {
		s[i] = comboOp(r.Intn(int(comboOps)))
	}
	return reflect.ValueOf(s)
}

// comboModel is what the page must show: whether the list is open and
// the data-values picked so far.
type comboModel struct {
	open   bool
	picked []string
}

// comboSession is actionSession with one hook: while cancelStep2 holds a
// cancel function, the step 2 attempt of a select calls it and is never
// sent, as when the client goes away between the steps.
func comboSession(t *testing.T, cancelStep2 *atomic.Pointer[context.CancelFunc]) *core.PageSession {
	t.Helper()
	actionSeq++
	session := fmt.Sprintf("act-%d", actionSeq)
	rt := iwdpruntime.New(sess, sess.Generation)
	rt.SetOwner(iwdpruntime.Owner{Daemon: actionsDaemon, Session: session, Epoch: 1})
	real := rt.RuntimeCaller()
	call := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		if a, ok := args.(core.AttemptArgs); ok && a.Step == 2 {
			if cancel := cancelStep2.Load(); cancel != nil {
				(*cancel)()
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		return real(ctx, c, fn, args)
	}
	s := core.NewScheduler(sess, core.SchedulerOptions{Runtime: call, Generation: sess.Generation, ReplyGrace: time.Second})
	t.Cleanup(s.Close)
	s.SetLease(session, 1)
	return &core.PageSession{Caller: sess, Runtime: rt, Refs: refs.NewTable(), Sched: s, DaemonID: actionsDaemon}
}

// comboState is the page's combobox: open (aria-expanded and the list
// mounted, or neither) and the picked data-values.
func comboState(t *testing.T) (string, []string) {
	t.Helper()
	st := pageString(t, `(function () { var c = document.getElementById("combo"), l = document.getElementById("combo-list");
  return c.getAttribute("aria-expanded") + (l ? "+list" : "") + "|" + T.combo.join(","); })()`)
	open, picked, _ := strings.Cut(st, "|")
	if picked == "" {
		return open, nil
	}
	return open, strings.Split(picked, ",")
}

// waitCombo waits until the page shows the model's list state; the list
// mounts in a later task after the click that opens it.
func waitCombo(t *testing.T, m comboModel) (string, []string) {
	t.Helper()
	want := "false"
	if m.open {
		want = "true+list"
	}
	var open string
	var picked []string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if open, picked = comboState(t); open == want {
			break
		}
	}
	return open, picked
}

// Review2 F18 (FIX-F F6, mod-forms OPENED): generated sequences of
// selects, refusals, cancelled selects, user opens and closes and runtime
// resets against a model: a select that opened the list closes it when it
// refuses; a list that the select found open (the user's, or one a
// cancelled select left open) stays open on a refusal; a picked option
// closes the list; a cancelled select picks nothing; a reset changes
// nothing on the page.
func TestForms045ComboboxOpenedModel(t *testing.T) {
	picks := [][2]string{{"Apple", "apple"}, {"Banana", "banana"}, {"cherry", "cherry"}}
	var cancelStep2 atomic.Pointer[context.CancelFunc]
	run := 0
	prop := func(seq comboSeq) bool {
		run++
		loadForms(t)
		ps := comboSession(t, &cancelStep2)
		var m comboModel
		for i, op := range seq {
			var code int
			var errOut string
			want := 0
			switch op {
			case comboPick, comboAbsent, comboDisabled:
				v := [...]string{comboAbsent: "Durian", comboDisabled: "Kiwi"}[op]
				if op == comboPick {
					p := picks[(run+i)%len(picks)]
					v = p[0]
					m.picked = append(m.picked, p[1])
					m.open = false
				} else {
					want = 1 // m.open stays: a list this select opened is closed again
				}
				code, _, errOut = act(context.Background(), ps, "--timeout", "10s", "select", "#combo", v)
			case comboCancel:
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				cancelStep2.Store(&cancel)
				code, _, errOut = act(ctx, ps, "select", "#combo", "Apple")
				cancelStep2.Store(nil)
				cancel()
				want = -1 // any failure
				m.open = true
			case comboUserOpen:
				eval(t, `document.getElementById("combo").click(); true`)
				m.open = true
			case comboUserEsc:
				eval(t, `document.getElementById("combo").dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); true`)
				m.open = false
			case comboReset:
				eval(t, "!window.__iwdp || window.__iwdp.reset()") // none before the first action
			}
			if want >= 0 && code != want || want < 0 && code == 0 {
				t.Logf("sequence %v, step %d (%v): exit %d, stderr %q; want exit %d", seq, i, op, code, errOut, want)
				return false
			}
			if open, picked := waitCombo(t, m); open != map[bool]string{false: "false", true: "true+list"}[m.open] || strings.Join(picked, ",") != strings.Join(m.picked, ",") {
				t.Logf("sequence %v, step %d (%v): the page shows %s, picked %v; the model has open %v, picked %v", seq, i, op, open, picked, m.open, m.picked)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}
