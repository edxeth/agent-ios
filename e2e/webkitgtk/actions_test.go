//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	iwdpruntime "github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// W3b actions suite (design 7.6 items 1 to 5): every test drives the
// injected runtime through the registry commands and the daemon's attempt
// loop (core.Scheduler, design 2.6) on the real page of WebKitGTK, and
// asserts the final page state of the fixture actions.html. The expected
// values are hand-written from design 3.5 and the fixture's handlers. The
// "daemon test hooks" of 7.6 are wrappers of the scheduler's runtime
// caller: one drops the __iwdp.cancel message, one drops the reply of an
// attempt after the page ran it.

// actionsDaemon is the owner daemon id of this suite's sessions; another
// id than the W3a suite's, so the page resets its maps for it instead of
// refusing an older epoch.
const actionsDaemon = "w3b-actions"

// attemptLog records the attempts the scheduler sent (OnAttempt).
type attemptLog struct {
	mu   sync.Mutex
	sent []time.Time
}

func (l *attemptLog) add() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, time.Now())
}

func (l *attemptLog) after(t time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.sent {
		if s.After(t) {
			n++
		}
	}
	return n
}

// hooks are the test hooks of one action session.
type hooks struct {
	dropCancel bool // never send __iwdp.cancel
	dropReply  int  // drop the reply of the attempt that dispatched this step (after the page ran it); 0: none
}

var actionSeq int

// actionSession is what the daemon gives an action command: the shared
// page connection with a scheduler that holds the lease of a new session,
// and the session's runtime owner and ref table.
func actionSession(t *testing.T, h hooks) (*core.PageSession, *attemptLog) {
	t.Helper()
	actionSeq++
	session := fmt.Sprintf("act-%d", actionSeq)
	rt := iwdpruntime.New(sess, sess.Generation)
	rt.SetOwner(iwdpruntime.Owner{Daemon: actionsDaemon, Session: session, Epoch: 1})
	real := rt.RuntimeCaller()
	var dropped sync.Once
	call := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		if fn == "cancel" && h.dropCancel {
			return json.RawMessage("null"), nil
		}
		v, err := real(ctx, c, fn, args)
		var rep core.AttemptReply
		if a, ok := args.(core.AttemptArgs); ok && h.dropReply > 0 && a.Step == h.dropReply && err == nil &&
			json.Unmarshal(v, &rep) == nil && rep.Status == core.StatusDispatched {
			drop := false
			dropped.Do(func() { drop = true })
			if drop {
				<-ctx.Done() // the reply never reaches the attempt loop
				return nil, ctx.Err()
			}
		}
		return v, err
	}
	log := &attemptLog{}
	s := core.NewScheduler(sess, core.SchedulerOptions{
		Runtime: call, Generation: sess.Generation,
		ReplyGrace: time.Second, OnAttempt: func(core.AttemptEvent) { log.add() },
	})
	t.Cleanup(s.Close)
	s.SetLease(session, 1)
	return &core.PageSession{Caller: sess, Runtime: rt, Refs: refs.NewTable(), Sched: s, DaemonID: actionsDaemon}, log
}

// act runs an action command line under ctx; it returns the exit code,
// stdout and stderr.
func act(ctx context.Context, ps *core.PageSession, args ...string) (int, string, string) {
	var out, errb strings.Builder
	code := commands.RunCLIStarted(ctx, args, &out, &errb, nil,
		func(context.Context, *commands.Invocation) (commands.Env, error) { return env{ps}, nil })

	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

func mustAct(t *testing.T, ps *core.PageSession, args ...string) string {
	t.Helper()
	code, out, errOut := act(context.Background(), ps, append([]string{"--timeout", "10s"}, args...)...)
	if code != 0 {
		t.Fatalf("%s: exit %d: %s", strings.Join(args, " "), code, errOut)
	}
	return out
}

// pageJSON evaluates expr in the page and decodes its value into v.
func pageJSON(t *testing.T, expr string, v any) {
	t.Helper()
	raw := eval(t, expr)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s = %s: %v", expr, raw, err)
	}
}

func pageString(t *testing.T, expr string) string {
	var s string
	pageJSON(t, expr, &s)
	return s
}

func pageInt(t *testing.T, expr string) int {
	var n int
	pageJSON(t, expr, &n)
	return n
}

func loadActions(t *testing.T) {
	t.Helper()
	navigate(t, "actions.html", "!!window.T")
}

// 7.6 item 1: click dispatches the sequence of design 3.5 and counts one
// click; the result says the input was synthetic.
func TestActionClickSequenceAndState(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	out := mustAct(t, ps, "click", "#btn")
	if !strings.Contains(out, "clicked #btn") || !strings.Contains(out, "synthetic event") {
		t.Fatalf("click output %q; want the synthetic-input note", out)
	}
	if n := pageInt(t, "T.clicks"); n != 1 {
		t.Fatalf("click counter %d, want 1", n)
	}
	var events []string
	pageJSON(t, "T.events", &events)
	// The click focuses the button (the default action of mousedown).
	// WebKit fires the focus event only while the page has system focus;
	// headless MiniBrowser under broadway never has it, so there the
	// sequence has no focus event and the focus shows in activeElement.
	want := []string{
		"pointerover", "pointerenter", "mouseover", "mouseenter", "pointermove", "mousemove",
		"pointerdown", "mousedown", "focus", "pointerup", "mouseup", "click",
	}
	if string(eval(t, "document.hasFocus()")) != "true" {
		want = append(want[:8:8], want[9:]...)
	}
	if strings.Join(events, ",") != strings.Join(want, ",") || pageString(t, "document.activeElement.id") != "btn" {
		t.Fatalf("events %v\nwant   %v; focused element %s, want btn", events, want, eval(t, "document.activeElement.id"))
	}
	// --json: trusted false on every result (design 3.5).
	js := mustAct(t, ps, "--json", "click", "#btn")
	var r struct {
		Trusted *bool `json:"trusted"`
	}
	if err := json.Unmarshal([]byte(js), &r); err != nil || r.Trusted == nil || *r.Trusted {
		t.Fatalf("--json result %s: want \"trusted\": false", js)
	}
	if n := pageInt(t, "T.clicks"); n != 2 {
		t.Fatalf("click counter %d after two clicks, want 2", n)
	}
}

// W3b decision 1 (one task per event): a mousedown handler queues a
// microtask and a task, and the click handler depends on the task. Real
// input runs them between mouse down and click, so the page sees
// mousedown, micro, task, then a click that finds the task done.
func TestActionClickRunsPageTasksBetweenEvents(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "click", "#seq")
	var seq []string
	pageJSON(t, "T.seq", &seq)
	if want := "mousedown,micro,task,click:ready"; strings.Join(seq, ",") != want {
		t.Fatalf("page order %v, want %s (real input order)", seq, want)
	}
}

// 7.6 item 1, multi-step actions: type --delay gives the text exactly once
// over several attempt windows; drag dispatches its 10 steps in order;
// mouse move --steps 5 gives 5 moves in order.
func TestActionMultiStep(t *testing.T) {
	loadActions(t)
	ps, log := actionSession(t, hooks{})
	js := mustAct(t, ps, "--json", "type", "#slow", "abc", "--delay", "300ms")
	if v := pageString(t, "document.getElementById('slow').value"); v != "abc" {
		t.Fatalf("type --delay 300ms abc: value %q, want abc", v)
	}
	var r struct {
		Steps int `json:"steps"`
		Acked int `json:"acknowledged"`
	}
	if err := json.Unmarshal([]byte(js), &r); err != nil || r.Steps != 3 || r.Acked != 3 || log.after(time.Time{}) < 3 {
		t.Fatalf("type result %s (%d attempts): want 3 steps, 3 acknowledged", js, log.after(time.Time{}))
	}

	out := mustAct(t, ps, "drag", "#src", "#dst")
	if !strings.Contains(out, "best effort") {
		t.Fatalf("drag output %q; want it to say best effort", out)
	}
	var drag []string
	pageJSON(t, "T.drag", &drag)
	if strings.Join(drag, ",") != "dragstart,dragenter,drop,dragend" || pageInt(t, "T.dropped") != 1 {
		t.Fatalf("drag events %v, dropped %d; want dragstart, dragenter, drop, dragend and one drop", drag, pageInt(t, "T.dropped"))
	}
	var moves [][3]int
	pageJSON(t, "T.moves.filter(function (m) { return m[0] === 1; })", &moves)
	if len(moves) != 9 {
		t.Fatalf("drag moved the pressed pointer %d times, want 9 (steps 2 to 10): %v", len(moves), moves)
	}
	for i := 1; i < len(moves); i++ {
		if moves[i][1] <= moves[i-1][1] {
			t.Fatalf("drag moves are not in order from source to target: %v", moves)
		}
	}

	ps2, _ := actionSession(t, hooks{}) // a new owner: the pointer starts at (0, 0)
	eval(t, "T.moves = []")
	mustAct(t, ps2, "mouse", "move", "100", "100", "--steps", "5")
	pageJSON(t, "T.moves", &moves)
	want := [][3]int{{0, 20, 20}, {0, 40, 40}, {0, 60, 60}, {0, 80, 80}, {0, 100, 100}}
	if fmt.Sprint(moves) != fmt.Sprint(want) {
		t.Fatalf("mouse move --steps 5: moves %v, want %v", moves, want)
	}
}

// 7.6 item 2: the page's cancel handlers decide.
func TestActionPageCancellation(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	code, _, errOut := act(context.Background(), ps, "fill", "#cancelbi", "new")
	if code == 0 || !strings.Contains(errOut, "page canceled the input") {
		t.Fatalf("fill on a canceled beforeinput: exit %d, stderr %q", code, errOut)
	}
	if v := pageString(t, "document.getElementById('cancelbi').value"); v != "keep" {
		t.Fatalf("value after the canceled fill %q, want keep", v)
	}

	mustAct(t, ps, "focus", "#q3")
	out := mustAct(t, ps, "press", "Enter")
	if strings.Contains(out, "emulated default action") || string(eval(t, "T.submitted.f3 === undefined")) != "true" {
		t.Fatalf("Enter with a canceled keydown: output %q, submitted %s; want no default action", out, eval(t, "JSON.stringify(T.submitted)"))
	}

	mustAct(t, ps, "type", "#kp", "x")
	if v := pageString(t, "document.getElementById('kp').value"); v != "" {
		t.Fatalf("type into an input that cancels keypress: value %q, want empty", v)
	}

	eval(t, "var s = document.getElementById('sel'); s.focus(); s.setSelectionRange(0, 5); true")
	mustAct(t, ps, "type", "#sel", "HEY")
	var st struct {
		V    string `json:"v"`
		S, E int
	}
	pageJSON(t, "(function (s) { return {v: s.value, S: s.selectionStart, E: s.selectionEnd}; })(document.getElementById('sel'))", &st)
	if st.V != "HEY world" || st.S != 3 || st.E != 3 {
		t.Fatalf("type into the selection 0..5 of \"hello world\": %+v, want \"HEY world\" with the caret at 3", st)
	}
}

// 7.6 item 4: a React-style controlled input (value tracker) sees fill and
// type, because the runtime sets the value through the native setter.
func TestActionReactControlledInput(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "fill", "#react", "hello")
	if v := pageString(t, "T.react"); v != "hello" {
		t.Fatalf("controlled input state after fill: %q, want hello", v)
	}
	mustAct(t, ps, "type", "#react", "!")
	if v := pageString(t, "T.react"); v != "hello!" {
		t.Fatalf("controlled input state after type: %q, want hello!", v)
	}
}

// 7.6 item 5: emulated default actions, each named in the result.
func TestActionDefaultActions(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#q1")
	out := mustAct(t, ps, "press", "Enter")
	if !strings.Contains(out, `emulated default action: implicit submission: clicked <button id="go">`) || pageString(t, "T.submitted.f1") != "go" {
		t.Fatalf("Enter in a form with a default button: output %q, submitted %s", out, eval(t, "JSON.stringify(T.submitted)"))
	}
	mustAct(t, ps, "focus", "#a2")
	out = mustAct(t, ps, "press", "Enter")
	if strings.Contains(out, "implicit submission") || string(eval(t, "T.submitted.f2 === undefined")) != "true" {
		t.Fatalf("Enter in a form with two fields and no button: output %q, submitted %s; want no submission", out, eval(t, "JSON.stringify(T.submitted)"))
	}
	out = mustAct(t, ps, "press", "Tab")
	if pageString(t, "document.activeElement.id") != "b2" || !strings.Contains(out, `emulated default action: focus moved to <input id="b2">`) {
		t.Fatalf("press Tab: output %q, focus on %s; want #b2", out, eval(t, "document.activeElement.id"))
	}
	out = mustAct(t, ps, "check", "#hc")
	if string(eval(t, "document.getElementById('hc').checked")) != "true" || !strings.Contains(out, "checked #hc") {
		t.Fatalf("check on a hidden checkbox with a label: output %q, checked %s", out, eval(t, "document.getElementById('hc').checked"))
	}
}

var ackedPattern = regexp.MustCompile(`\((\d+) acknowledged\)`)

// 7.6 item 3: a covered element fails before dispatch and names the cover;
// --force clicks.
func TestActionCoveredElement(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	code, _, errOut := act(context.Background(), ps, "--timeout", "1500ms", "click", "#covered")
	if code == 0 || !strings.Contains(errOut, `covered by <div id="cover" class="modal">`) || strings.Contains(errOut, "may have run") {
		t.Fatalf("click on a covered element: exit %d, stderr %q; want a failure before dispatch that names the cover", code, errOut)
	}
	if n := pageInt(t, "T.coveredClicks"); n != 0 {
		t.Fatalf("covered element got %d clicks", n)
	}
	mustAct(t, ps, "click", "#covered", "--force")
	if n := pageInt(t, "T.coveredClicks"); n != 1 {
		t.Fatalf("click --force: %d clicks, want 1", n)
	}
}

// 7.6 item 3 (a) to (c): a canceled or expired click on a covered element
// never clicks, also when the cover goes away later, and no attempt is
// sent after the cancel or the deadline.
//
// The new-tab cases run click --new-tab on a covered link through the same
// fenced attempts (review finding 1): window.open is never called.
func TestActionCanceledCoveredClick(t *testing.T) {
	click := []string{"click", "#covered"}
	newTab := []string{"click", "#coveredlink", "--new-tab"}
	const clicks, opened = "T.coveredClicks", "T.opened.length"
	for _, c := range []struct {
		name               string
		h                  hooks
		disconnect, remove time.Duration // 0: no disconnect
		cmd                []string
		counter            string // page expression that counts the side effect
	}{
		{"a-disconnect", hooks{}, 500 * time.Millisecond, time.Second, click, clicks},
		{"b-cancel-message-lost", hooks{dropCancel: true}, 500 * time.Millisecond, time.Second, click, clicks},
		{"c-deadline", hooks{}, 0, 3500 * time.Millisecond, click, clicks},
		{"a-disconnect-new-tab", hooks{}, 500 * time.Millisecond, time.Second, newTab, opened},
		{"b-cancel-message-lost-new-tab", hooks{dropCancel: true}, 500 * time.Millisecond, time.Second, newTab, opened},
		{"c-deadline-new-tab", hooks{}, 0, 3500 * time.Millisecond, newTab, opened},
	} {
		t.Run(c.name, func(t *testing.T) {
			loadActions(t)
			ps, log := actionSession(t, c.h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			type result struct {
				code   int
				errOut string
				at     time.Time
			}
			done := make(chan result, 1)
			go func() {
				code, _, errOut := act(ctx, ps, append([]string{"--timeout", "3s"}, c.cmd...)...)
				done <- result{code, errOut, time.Now()}
			}()
			stopAt := start.Add(3 * time.Second)
			if c.disconnect > 0 {
				time.Sleep(c.disconnect)
				stopAt = time.Now()
				cancel()
			}
			time.Sleep(time.Until(start.Add(c.remove)))
			eval(t, "document.getElementById('cover').remove(); true")
			removed := time.Now()
			var r result
			select {
			case r = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the click did not return within 5 s after the cover was removed")
			}
			if r.code == 0 {
				t.Fatalf("the canceled click succeeded: %q", r.errOut)
			}
			if r.at.Before(stopAt) {
				stopAt = r.at // the deadline run ends before the deadline (action reserve)
			}
			time.Sleep(time.Until(removed.Add(5 * time.Second)))
			if n := pageInt(t, c.counter); n != 0 {
				t.Fatalf("the page counted %d (%s) within 5 s after the cover was removed; stderr %q", n, c.counter, r.errOut)
			}
			if n := log.after(stopAt); n != 0 {
				t.Fatalf("%d attempts were sent after the cancel or the deadline", n)
			}
			t.Logf("result after %s: exit %d, %s", r.at.Sub(start).Round(time.Millisecond), r.code, strings.TrimSpace(r.errOut))
		})
	}
}

// 7.6 item 3 (d) to (f): multi-step cancellation.
func TestActionMultiStepCancellation(t *testing.T) {
	t.Run("d-disconnect-during-type", func(t *testing.T) {
		loadActions(t)
		ps, _ := actionSession(t, hooks{})
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(1200*time.Millisecond, cancel)
		code, _, errOut := act(ctx, ps, "--timeout", "10s", "type", "#slow", "abcdef", "--delay", "500ms")
		m := ackedPattern.FindStringSubmatch(errOut)
		if code == 0 || m == nil {
			t.Fatalf("canceled type: exit %d, stderr %q; want the acknowledged steps", code, errOut)
		}
		acked, _ := strconv.Atoi(m[1])
		v := pageString(t, "document.getElementById('slow').value")
		if !strings.HasPrefix("abcdef", v) || len(v) < acked || len(v) > acked+1 {
			t.Fatalf("value %q after %d acknowledged steps; want those characters plus at most the step in flight", v, acked)
		}
		time.Sleep(5 * time.Second)
		if w := pageString(t, "document.getElementById('slow').value"); w != v {
			t.Fatalf("value changed from %q to %q after the cancel", v, w)
		}
	})
	t.Run("e-lost-reply-of-type-step-2", func(t *testing.T) {
		loadActions(t)
		ps, _ := actionSession(t, hooks{dropReply: 2})
		code, _, errOut := act(context.Background(), ps, "--timeout", "10s", "type", "#slow", "abc", "--delay", "300ms")
		if code == 0 || !strings.Contains(errOut, "step 2 of 3") {
			t.Fatalf("type with the reply of step 2 lost: exit %d, stderr %q; want the error to name step 2 of 3", code, errOut)
		}
		time.Sleep(time.Second)
		if v := pageString(t, "document.getElementById('slow').value"); v != "ab" {
			t.Fatalf("value %q, want ab (step 2 ran once, step 3 never)", v)
		}
	})
	t.Run("f-lost-reply-of-drag-step-4", func(t *testing.T) {
		loadActions(t)
		ps, _ := actionSession(t, hooks{dropReply: 4})
		code, _, errOut := act(context.Background(), ps, "--timeout", "10s", "drag", "#src", "#dst")
		if code == 0 || !strings.Contains(errOut, "step 4 of 10") {
			t.Fatalf("drag with the reply of step 4 lost: exit %d, stderr %q", code, errOut)
		}
		time.Sleep(time.Second)
		var moves [][3]int
		pageJSON(t, "T.moves.filter(function (m) { return m[0] === 1; })", &moves)
		var drag []string
		pageJSON(t, "T.drag", &drag)
		if len(moves) != 3 || strings.Contains(strings.Join(drag, ","), "drop") {
			t.Fatalf("pointer log %v and drag events %v; want the moves of steps 2 to 4 only and no drop", moves, drag)
		}
	})
}

// 7.6 item 1: more actions and their final states: fill, dblclick, tap on
// a target and at a point, keyboard inserttext.
func TestActionMoreFinalStates(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "fill", "#name", "new value")
	if v := pageString(t, "document.getElementById('name').value"); v != "new value" {
		t.Fatalf("fill: value %q", v)
	}
	mustAct(t, ps, "dblclick", "#btn")
	if n := pageInt(t, "T.clicks"); n != 2 {
		t.Fatalf("dblclick: %d clicks, want 2", n)
	}
	mustAct(t, ps, "tap", "#btn")
	var c struct{ X, Y float64 }
	pageJSON(t, "(function (r) { return {X: r.left + r.width / 2, Y: r.top + r.height / 2}; })(document.getElementById('btn').getBoundingClientRect())", &c)
	out := mustAct(t, ps, "tap", fmt.Sprint(int(c.X)), fmt.Sprint(int(c.Y)))
	if n := pageInt(t, "T.clicks"); n != 4 || !strings.Contains(out, `tapped <button id="btn">`) {
		t.Fatalf("tap and tap x y: %d clicks, output %q; want 4 clicks and the tapped element named", n, out)
	}
	mustAct(t, ps, "focus", "#ta")
	mustAct(t, ps, "keyboard", "inserttext", "one two")
	if v := pageString(t, "document.getElementById('ta').value"); v != "one two" {
		t.Fatalf("keyboard inserttext: value %q", v)
	}
}

// ---- W3b repair round 1 (review of the actions part) ----

// noEmulated fails when an action result names an emulated default action.
func noEmulated(t *testing.T, what, out string) {
	t.Helper()
	if strings.Contains(out, "emulated default action") {
		t.Fatalf("%s: output %q names an emulated default action; want none", what, out)
	}
}

// Review finding 1 (design 3.5 click --new-tab, 2.6 fence): --new-tab runs
// the click checks in fenced attempts. A covered link fails and names the
// cover without calling window.open, and its waiting attempts leave the
// page without a user activation (each attempt probes without the
// gesture first); --force skips the checks; a ready link calls
// window.open once with its href while the page has a user gesture
// (Runtime.evaluate emulateUserGesture); an element without href fails
// without a click.
func TestActionNewTabChecksAndGesture(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	code, out, errOut := act(context.Background(), ps, "--timeout", "1s", "click", "#coveredlink", "--new-tab")
	if n := pageInt(t, "T.opened.length"); code == 0 || n != 0 || !strings.Contains(errOut, `covered by <div id="cover" class="modal">`) {
		t.Fatalf("click --new-tab on a covered link: exit %d, opened %d, out %q, stderr %q; want a failure that names the cover and no window.open", code, n, out, errOut)
	}
	if g := string(eval(t, "navigator.userActivation.isActive")); g != "false" {
		t.Fatalf("the page has a user activation (%s) after the waiting attempts on the covered link; want none (and the gesture check below needs none)", g)
	}
	out = mustAct(t, ps, "click", "#link", "--new-tab")
	var opened []struct {
		URL     string `json:"url"`
		Gesture *bool  `json:"gesture"`
	}
	pageJSON(t, "T.opened", &opened)
	want := origin + "/child.html"
	if len(opened) != 1 || opened[0].URL != want || opened[0].Gesture == nil || !*opened[0].Gesture || !strings.Contains(out, "opened "+want) {
		t.Fatalf("click #link --new-tab: window.open calls %+v, output %q; want one call with %s under a user gesture", opened, out, want)
	}
	mustAct(t, ps, "click", "#coveredlink", "--new-tab", "--force")
	if n := pageInt(t, "T.opened.length"); n != 2 {
		t.Fatalf("click --new-tab --force on the covered link: %d window.open calls, want 2", n)
	}
	code, _, errOut = act(context.Background(), ps, "click", "#btn", "--new-tab")
	if code == 0 || !strings.Contains(errOut, "no href") || pageInt(t, "T.clicks") != 0 || pageInt(t, "T.opened.length") != 2 {
		t.Fatalf("click --new-tab on a button: exit %d, stderr %q, clicks %d; want a failure without a click or window.open", code, errOut, pageInt(t, "T.clicks"))
	}
}

// Review finding 1, lost reply: the hook drops the reply of the attempt
// that ran window.open. The error says the step may have run, the step is
// not sent again, and the page called window.open exactly once.
func TestActionNewTabLostReply(t *testing.T) {
	loadActions(t)
	ps, log := actionSession(t, hooks{dropReply: 1})
	code, _, errOut := act(context.Background(), ps, "--timeout", "10s", "click", "#link", "--new-tab")
	if code == 0 || !strings.Contains(errOut, "may have run step 1 of 1") {
		t.Fatalf("click --new-tab with the reply lost: exit %d, stderr %q; want the may-have-run error of step 1", code, errOut)
	}
	sent := log.after(time.Time{})
	time.Sleep(time.Second)
	if n := pageInt(t, "T.opened.length"); n != 1 || log.after(time.Time{}) != sent {
		t.Fatalf("window.open ran %d times, %d attempts after the error; want once and none", n, log.after(time.Time{})-sent)
	}
}

// slowNewTabCaller delays the attempt calls of click --new-tab: the reply
// of each probe (actProbe) by probeReply, so the measured round trip
// includes it, and the delivery of each user-gesture call by gestureSend,
// as a slow link would (W3b-R2 finding 6, the reviewer's 350 ms delay).
type slowNewTabCaller struct {
	webkit.Caller
	probeReply, gestureSend time.Duration
}

func (c slowNewTabCaller) Send(ctx context.Context, m string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	expr, _ := p["expression"].(string)
	sleep := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m == "Runtime.evaluate" && p["emulateUserGesture"] == true {
		if err := sleep(c.gestureSend); err != nil {
			return nil, err
		}
	}
	raw, err := c.Caller.Send(ctx, m, params)
	if err == nil && m == "Runtime.evaluate" && strings.Contains(expr, `"actProbe"`) {
		err = sleep(c.probeReply)
	}
	return raw, err
}

// newTabSession is actionSession on caller c, without hooks.
func newTabSession(t *testing.T, c webkit.Caller) *core.PageSession {
	t.Helper()
	actionSeq++
	session := fmt.Sprintf("act-%d", actionSeq)
	rt := iwdpruntime.New(c, sess.Generation)
	rt.SetOwner(iwdpruntime.Owner{Daemon: actionsDaemon, Session: session, Epoch: 1})
	s := core.NewScheduler(c, core.SchedulerOptions{Runtime: rt.RuntimeCaller(), Generation: sess.Generation, ReplyGrace: time.Second})
	t.Cleanup(s.Close)
	s.SetLease(session, 1)
	return &core.PageSession{Caller: c, Runtime: rt, Refs: refs.NewTable(), Sched: s, DaemonID: actionsDaemon}
}

// W3b-R2 finding 6 (a), (d): the user-gesture call of click --new-tab is
// delivered 350 ms late, and the measured round trip (the probe's reply
// also takes 350 ms) shows that the link is that slow. Its attempt window
// is sized from that round trip, so the call arrives inside it and the
// window opens once; with the plain 250 ms window it expired.
func TestActionNewTabWindowCoversTheMeasuredRoundTrip(t *testing.T) {
	loadActions(t)
	ps := newTabSession(t, slowNewTabCaller{Caller: sess, probeReply: 350 * time.Millisecond, gestureSend: 350 * time.Millisecond})
	code, out, errOut := act(context.Background(), ps, "--timeout", "10s", "click", "#link", "--new-tab")
	if n := pageInt(t, "T.opened.length"); code != 0 || n != 1 {
		t.Fatalf("click --new-tab over a slow link: exit %d, opened %d, out %q, stderr %q; want one window.open", code, n, out, errOut)
	}
}

// W3b-R2 finding 6 (d), forced expiry: the probe answers at once, so the
// gesture call's window is the plain 250 ms, and every gesture call is
// delivered 350 ms late. Each one expires on the page and opens nothing;
// the command fails. (The page can still hold a transient user activation
// from the expired calls: docs/notes/w3b.md, "Actions".)
func TestActionNewTabExpiredGestureOpensNothing(t *testing.T) {
	loadActions(t)
	ps := newTabSession(t, slowNewTabCaller{Caller: sess, gestureSend: 350 * time.Millisecond})
	code, out, errOut := act(context.Background(), ps, "--timeout", "2s", "click", "#link", "--new-tab")
	if n := pageInt(t, "T.opened.length"); code == 0 || n != 0 {
		t.Fatalf("click --new-tab with late gesture calls: exit %d, opened %d, out %q, stderr %q; want a failure and no window.open", code, n, out, errOut)
	}
}

// Review finding 2 (design 4.2 tap <x> <y>): the touch, pointer and mouse
// events of a tap at a point carry that point, not the center of the
// element there; tap <target> keeps the center. WebKitGTK has no Touch
// constructor, so the expected events start with pointerdown.
func TestActionTapAtPointKeepsThePoint(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	var r struct{ L, T, W, H float64 }
	pageJSON(t, "(function (r) { return {L: r.left, T: r.top, W: r.width, H: r.height}; })(document.getElementById('btn').getBoundingClientRect())", &r)
	x, y := int(r.L)+2, int(r.T)+2
	out := mustAct(t, ps, "tap", fmt.Sprint(x), fmt.Sprint(y))
	var at [][3]any
	pageJSON(t, "T.at", &at)
	want := fmt.Sprint([][3]any{{"pointerdown", x, y}, {"mousedown", x, y}, {"pointerup", x, y}, {"mouseup", x, y}, {"click", x, y}})
	if got := fmt.Sprint(intsOf(at)); got != want || pageInt(t, "T.clicks") != 1 {
		t.Fatalf("tap %d %d: the button received %s (clicks %d), want %s; output %q", x, y, got, pageInt(t, "T.clicks"), want, out)
	}
	eval(t, "T.at = []; true")
	mustAct(t, ps, "tap", "#btn")
	pageJSON(t, "T.at", &at)
	cx, cy := r.L+r.W/2, r.T+r.H/2
	if len(at) != 5 || at[4][0] != "click" || abs(at[4][1].(float64)-cx) > 1 || abs(at[4][2].(float64)-cy) > 1 {
		t.Fatalf("tap #btn: the button received %v, want the click at the center (%.1f, %.1f)", at, cx, cy)
	}
}

// intsOf turns the JSON numbers of [type, x, y] rows into ints.
func intsOf(rows [][3]any) [][3]any {
	out := make([][3]any, len(rows))
	for i, r := range rows {
		out[i] = [3]any{r[0], int(r[1].(float64)), int(r[2].(float64))}
	}
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// Review finding 5: keydown and press --phase down send only keydown (no
// keypress, no insertion); keyup sends only keyup; a full press sends
// keydown, keypress, the insertion and keyup.
func TestActionKeydownPhaseSendsOnlyKeydown(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#keys")
	mustAct(t, ps, "keydown", "a")
	if v := pageString(t, "T.keys.join(',')"); v != "keydown:a" {
		t.Fatalf("keydown a sent %s, want keydown:a", v)
	}
	mustAct(t, ps, "press", "b", "--phase", "down")
	mustAct(t, ps, "keyup", "a")
	if v := pageString(t, "T.keys.join(',')"); v != "keydown:a,keydown:b,keyup:a" {
		t.Fatalf("keydown a, press b --phase down, keyup a sent %s, want keydown:a,keydown:b,keyup:a", v)
	}
	if v := pageString(t, "document.getElementById('keys').value"); v != "" {
		t.Fatalf("the split phases inserted %q, want nothing", v)
	}
	eval(t, "T.keys = []; true")
	mustAct(t, ps, "press", "c")
	if v, val := pageString(t, "T.keys.join(',')"), pageString(t, "document.getElementById('keys').value"); v != "keydown:c,keypress:c,input:c,keyup:c" || val != "c" {
		t.Fatalf("press c sent %s, value %q; want keydown:c,keypress:c,input:c,keyup:c and c", v, val)
	}
}

// Review finding 3 (design 3.5: a canceled keydown skips the default
// action): split key phases keep the keydown's cancellation. Space on a
// button clicks on keyup only after a matching uncanceled keydown; a keyup
// without a keydown, after a canceled keydown, after an owner reset or
// after a release does not click.
func TestActionSplitSpaceKeepsCancellation(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#btn")
	mustAct(t, ps, "keydown", "Space")
	out := mustAct(t, ps, "keyup", "Space")
	if n := pageInt(t, "T.clicks"); n != 1 || !strings.Contains(out, `emulated default action: click <button id="btn">`) {
		t.Fatalf("keydown Space, keyup Space: %d clicks, output %q; want one named click", n, out)
	}
	noEmulated(t, "keyup Space without a keydown", mustAct(t, ps, "keyup", "Space"))

	eval(t, "T.cancelKey = true; true")
	mustAct(t, ps, "keydown", "Space")
	noEmulated(t, "keyup Space after a canceled keydown", mustAct(t, ps, "keyup", "Space"))
	eval(t, "T.cancelKey = false; true")

	mustAct(t, ps, "keydown", "Space")
	ps2, _ := actionSession(t, hooks{}) // a new owner: the page resets its maps
	mustAct(t, ps2, "focus", "#btn")
	noEmulated(t, "keyup Space of a new owner", mustAct(t, ps2, "keyup", "Space"))

	mustAct(t, ps2, "keydown", "Space")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ps2.Runtime.Release(ctx, sess, 1); err != nil {
		t.Fatalf("runtime release: %v", err)
	}
	noEmulated(t, "keyup Space after a release", mustAct(t, ps2, "keyup", "Space"))
	if n := pageInt(t, "T.clicks"); n != 1 {
		t.Fatalf("the button has %d clicks, want 1 (only the matching uncanceled keydown clicks)", n)
	}
}

// Review finding 4 (design 3.5: Enter activates links and buttons, Space
// activates buttons, checkboxes and radios on keyup): Enter leaves a
// checkbox and a radio outside a form unchanged; Space checks them and
// names the click; Enter still clicks a button.
func TestActionEnterAndSpaceOnCheckboxAndRadio(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	for _, id := range []string{"cb", "r1"} {
		state := "document.getElementById('" + id + "').checked"
		mustAct(t, ps, "focus", "#"+id)
		out := mustAct(t, ps, "press", "Enter")
		noEmulated(t, "Enter on #"+id, out)
		if v := string(eval(t, state)); v != "false" {
			t.Fatalf("Enter on #%s: checked %s, want false", id, v)
		}
		out = mustAct(t, ps, "press", "Space")
		if v := string(eval(t, state)); v != "true" || !strings.Contains(out, `emulated default action: click <input id="`+id+`">`) {
			t.Fatalf("Space on #%s: checked %s, output %q; want checked and the click named", id, v, out)
		}
	}
	mustAct(t, ps, "focus", "#btn")
	out := mustAct(t, ps, "press", "Enter")
	if n := pageInt(t, "T.clicks"); n != 1 || !strings.Contains(out, `emulated default action: click <button id="btn">`) {
		t.Fatalf("Enter on a button: %d clicks, output %q; want one named click", n, out)
	}
}

// Review finding 6 (design 7.6 item 1): select by label and by value,
// multiple values, and the refusals that change nothing.
func TestActionSelect(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	out := mustAct(t, ps, "select", "#pick", "Banana")
	if v := pageString(t, "document.getElementById('pick').value"); v != "b" || pageString(t, "T.form.join(',')") != "pick:input,pick:change" || !strings.Contains(out, `selected ["b"] in #pick`) {
		t.Fatalf("select #pick Banana: value %q, events %s, output %q; want b, input then change", v, eval(t, "T.form"), out)
	}
	mustAct(t, ps, "select", "#multi", "x", "Zulu")
	if v := pageString(t, "Array.prototype.filter.call(document.getElementById('multi').options, function (o) { return o.selected; }).map(function (o) { return o.value; }).join(',')"); v != "x,z" {
		t.Fatalf("select #multi x Zulu: selected %q, want x,z", v)
	}
	eval(t, "T.form = []; true")
	for _, c := range []struct{ args, err string }{
		{"Durian", `no option with the value or label "Durian"`},
		{"a c", "the select takes one value"},
	} {
		code, _, errOut := act(context.Background(), ps, append([]string{"select", "#pick"}, strings.Fields(c.args)...)...)
		if code == 0 || !strings.Contains(errOut, c.err) {
			t.Fatalf("select #pick %s: exit %d, stderr %q; want %q", c.args, code, errOut, c.err)
		}
	}
	if v := pageString(t, "document.getElementById('pick').value"); v != "b" || pageInt(t, "T.form.length") != 0 {
		t.Fatalf("after the refused selects: value %q, events %s; want b and no events", v, eval(t, "T.form"))
	}
}

// Review finding 6: scroll moves the window or a --selector element by the
// given pixels (default direction words of design 3.5); scrollintoview
// centers an element.
func TestActionScrollAndScrollIntoView(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	if out := mustAct(t, ps, "scroll", "down", "120"); pageInt(t, "window.scrollY") != 120 || !strings.Contains(out, "scrolled down") {
		t.Fatalf("scroll down 120: scrollY %d, output %q", pageInt(t, "window.scrollY"), out)
	}
	mustAct(t, ps, "scroll", "up", "20")
	mustAct(t, ps, "scroll", "down", "40", "--selector", "#box")
	if y, b := pageInt(t, "window.scrollY"), pageInt(t, "document.getElementById('box').scrollTop"); y != 100 || b != 40 {
		t.Fatalf("scroll up 20, then down 40 in #box: scrollY %d, box scrollTop %d; want 100 and 40", y, b)
	}
	mustAct(t, ps, "scrollintoview", "#far")
	var c struct{ Y, H float64 }
	pageJSON(t, "(function (r) { return {Y: r.top + r.height / 2, H: window.innerHeight}; })(document.getElementById('far').getBoundingClientRect())", &c)
	if abs(c.Y-c.H/2) > 1 {
		t.Fatalf("scrollintoview #far: its center is at y %.1f of a %.0f px viewport, want the middle", c.Y, c.H)
	}
}

// Review finding 6: swipe sends touch events where the page has a Touch
// constructor and then scrolls, the finger's opposite way, as its named
// emulated default. WebKitGTK has no Touch constructor (new Touch throws
// "Illegal constructor"), so here the result warns and only the scroll
// fallback runs; touch cancellation is covered on devices only.
func TestActionSwipe(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	out := mustAct(t, ps, "swipe", "up", "200")
	if y := pageInt(t, "window.scrollY"); y != 200 || !strings.Contains(out, "emulated default action: scroll") || !strings.Contains(out, "no Touch constructor") {
		t.Fatalf("swipe up 200: scrollY %d, output %q; want 200, the named scroll and the Touch warning", y, out)
	}
	mustAct(t, ps, "swipe", "up", "60", "--selector", "#box")
	mustAct(t, ps, "swipe", "down", "50")
	if y, b := pageInt(t, "window.scrollY"), pageInt(t, "document.getElementById('box').scrollTop"); y != 150 || b != 60 {
		t.Fatalf("swipe up 60 in #box, then down 50: scrollY %d, box scrollTop %d; want 150 and 60", y, b)
	}
}

// Review finding 6: upload sets the file's name, type and bytes on the
// input and dispatches input and change; a second file for a single input
// and a target that is not a file input change nothing.
func TestActionUpload(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("hello upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustAct(t, ps, "upload", "#file", a)
	if !strings.Contains(out, "set 1 file(s), 12 bytes, on #file") || pageString(t, "T.form.join(',')") != "file:input,file:change" {
		t.Fatalf("upload: output %q, events %s; want input then change", out, eval(t, "T.form"))
	}
	eval(t, "T.content = null; document.getElementById('file').files[0].text().then(function (s) { T.content = s; }); true")
	var f struct {
		Name, Type, Content string
		Size                int
	}
	for deadline := time.Now().Add(5 * time.Second); f.Content == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		pageJSON(t, "(function (f) { return {Name: f.name, Type: f.type, Size: f.size, Content: T.content || ''}; })(document.getElementById('file').files[0])", &f)
	}
	if f.Name != "a.txt" || f.Type != "text/plain" || f.Size != 12 || f.Content != "hello upload" {
		t.Fatalf("the input's file is %+v, want a.txt, text/plain, 12 bytes %q", f, "hello upload")
	}
	eval(t, "T.form = []; true")
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"#file", a, b}, "the file input takes one file"},
		{[]string{"#btn", a}, "the element is not a file input"},
	} {
		code, _, errOut := act(context.Background(), ps, append([]string{"upload"}, c.args...)...)
		if code == 0 || !strings.Contains(errOut, c.err) {
			t.Fatalf("upload %v: exit %d, stderr %q; want %q", c.args, code, errOut, c.err)
		}
	}
	if n, name := pageInt(t, "document.getElementById('file').files.length"), pageString(t, "document.getElementById('file').files[0].name"); n != 1 || name != "a.txt" || pageInt(t, "T.form.length") != 0 {
		t.Fatalf("after the refused uploads: %d files (%s), events %s; want a.txt only and no events", n, name, eval(t, "T.form"))
	}
}

// Review finding 6: hover sends the pointer and mouse move events of
// design 3.5 and no press; synthetic events do not set CSS :hover.
//
// hover --css is not asserted here: WebKit applies CSS.forcePseudoState
// only while the CSS agent is enabled, and webkit.Session refuses
// CSS.enable (it hangs through iwdp), so the forced state has no effect on
// WebKitGTK (observed: the :hover color stays unset). Reported to the
// orchestrator (repair round 1 report) instead of pinning the gap.
func TestActionHover(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	const color = "getComputedStyle(document.getElementById('hov')).color"
	out := mustAct(t, ps, "hover", "#hov")
	want := "pointerover,pointerenter,mouseover,mouseenter,pointermove,mousemove"
	if v := pageString(t, "T.hover.join(',')"); v != want || pageString(t, color) != "rgb(0, 0, 0)" || !strings.Contains(out, "hovered #hov") {
		t.Fatalf("hover: events %s, color %s, output %q; want %s and no :hover", v, eval(t, color), out, want)
	}
	if n := pageInt(t, "T.clicks"); n != 0 {
		t.Fatalf("hover clicked %d times", n)
	}
}

// W3b-R1 merge brief item 2: WebKitGTK answers CSS.enable, so hover --css
// forces :hover for real (the fixture's #hov:hover color), and the next
// action resets it. Before the repair the command printed "CSS :hover
// forced" while the color stayed unset: WebKit applies a forced pseudo
// state only while the CSS domain is enabled.
func TestActionHoverCSS(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	ps.State = core.NewDesiredState()
	const color = "getComputedStyle(document.getElementById('hov')).color"
	out := mustAct(t, ps, "hover", "#hov", "--css")
	if c := pageString(t, color); c != "rgb(255, 0, 0)" || !strings.Contains(out, "CSS :hover forced") {
		t.Fatalf("hover --css: color %s, output %q; want the :hover color rgb(255, 0, 0)", c, out)
	}
	if v := pageString(t, "T.hover.join(',')"); v != "pointerover,pointerenter,mouseover,mouseenter,pointermove,mousemove" {
		t.Fatalf("hover --css events %s", v)
	}
	mustAct(t, ps, "blur")
	if c := pageString(t, color); c != "rgb(0, 0, 0)" {
		t.Fatalf("color after the next action %s; want the forced :hover reset to rgb(0, 0, 0)", c)
	}
}

// The same through the daemon and separate CLI processes: the daemon's
// WebKitGTK page connection answers CSS.enable, the forced :hover is
// applied, and the session's next action resets it.
func TestActionHoverCSSThroughTheDaemon(t *testing.T) {
	addr := runtimeBrowser(t, "actions.html")
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	const color = "getComputedStyle(document.getElementById('hov')).color"
	if r := cli(t, "--session", "hov", "hover", "#hov", "--css"); r.code != 0 || !strings.Contains(r.stdout, "CSS :hover forced") {
		t.Fatalf("hover --css: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", "hov", "eval", color); r.code != 0 || !strings.Contains(r.stdout, "rgb(255, 0, 0)") {
		t.Fatalf("color after hover --css: exit %d, stdout %q, stderr %q; want rgb(255, 0, 0)", r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", "hov", "hover", "#hov"); r.code != 0 {
		t.Fatalf("hover: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := cli(t, "--session", "hov", "eval", color); r.code != 0 || !strings.Contains(r.stdout, "rgb(0, 0, 0)") {
		t.Fatalf("color after the next action: exit %d, stdout %q, stderr %q; want rgb(0, 0, 0)", r.code, r.stdout, r.stderr)
	}
}

// Review finding 6: blur blurs the focused element.
func TestActionBlur(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#name")
	if id := pageString(t, "document.activeElement.id"); id != "name" {
		t.Fatalf("focus #name: active element %q", id)
	}
	out := mustAct(t, ps, "blur")
	if tag := pageString(t, "document.activeElement.localName"); tag != "body" || !strings.Contains(out, "blurred the focused element") {
		t.Fatalf("blur: active element %s, output %q; want body", tag, out)
	}
}

// Review finding 6: mouse down and up at the pointer position with the
// button state of the session; wheel sends a wheel event and scrolls as
// its named default unless the page cancels it.
func TestActionMouseButtonsAndWheel(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	var c struct{ X, Y int }
	pageJSON(t, "(function (r) { return {X: Math.round(r.left + r.width / 2), Y: Math.round(r.top + r.height / 2)}; })(document.getElementById('btn').getBoundingClientRect())", &c)
	mustAct(t, ps, "mouse", "move", fmt.Sprint(c.X), fmt.Sprint(c.Y))
	mustAct(t, ps, "mouse", "down")
	mustAct(t, ps, "mouse", "down", "right")
	mustAct(t, ps, "mouse", "up", "right")
	mustAct(t, ps, "mouse", "up")
	var got [][6]any
	pageJSON(t, "T.mouse", &got)
	row := func(typ string, button, buttons int) string {
		return fmt.Sprint([6]any{typ, float64(button), float64(buttons), float64(c.X), float64(c.Y), "btn"})
	}
	want := []string{
		row("pointerdown", 0, 1), row("mousedown", 0, 1), row("pointerdown", 2, 3), row("mousedown", 2, 3),
		row("pointerup", 2, 1), row("mouseup", 2, 1), row("pointerup", 0, 0), row("mouseup", 0, 0),
	}
	if len(got) != len(want) {
		t.Fatalf("mouse down, down right, up right, up: events %v, want %v", got, want)
	}
	for i := range want {
		if fmt.Sprint(got[i]) != want[i] {
			t.Fatalf("mouse event %d is %v, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
	eval(t, "T.mouse = []; true")
	out := mustAct(t, ps, "mouse", "wheel", "150")
	if y := pageInt(t, "window.scrollY"); y != 150 || pageString(t, "T.mouse[0][0] + ':' + T.mouse.length") != "wheel:1" || !strings.Contains(out, "emulated default action: scroll") {
		t.Fatalf("mouse wheel 150: scrollY %d, events %s, output %q; want 150, one wheel and the named scroll", y, eval(t, "T.mouse"), out)
	}
	eval(t, "T.cancelWheel = true; true")
	out = mustAct(t, ps, "mouse", "wheel", "100")
	noEmulated(t, "a canceled wheel", out)
	if y := pageInt(t, "window.scrollY"); y != 150 || pageInt(t, "T.mouse.length") != 2 {
		t.Fatalf("canceled mouse wheel 100: scrollY %d, events %s; want 150 and the wheel event", y, eval(t, "T.mouse"))
	}
}

// Review finding 6: highlight by target resolves the target to its DOM
// node and highlights that node; the reported node id is the element's.
func TestActionHighlightByTarget(t *testing.T) {
	loadActions(t)
	ps, _ := actionSession(t, hooks{})
	js := mustAct(t, ps, "--json", "highlight", "#btn")
	var r struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal([]byte(js), &r); err != nil || r.NodeID == 0 {
		t.Fatalf("highlight #btn --json: %s (%v); want a node id", js, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := sess.Send(ctx, "DOM.resolveNode", map[string]any{"nodeId": r.NodeID})
	if err != nil {
		t.Fatalf("DOM.resolveNode %d: %v", r.NodeID, err)
	}
	var obj struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	_ = json.Unmarshal(raw, &obj)
	raw, err = sess.Send(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": obj.Object.ObjectID, "functionDeclaration": "function () { return this.id; }", "returnByValue": true})
	if err != nil || !strings.Contains(string(raw), `"value":"btn"`) {
		t.Fatalf("the highlighted node %d is %s (%v), want #btn", r.NodeID, raw, err)
	}
	if _, err := sess.Send(ctx, "DOM.hideHighlight", nil); err != nil {
		t.Fatalf("DOM.hideHighlight: %v", err)
	}
}
