package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/refs"
	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// w3bPage is what the W3b commands (actions, waits, navigation, dialogs,
// annotated screenshots) use of their page: the W3a runtime and ref table,
// the operation the command runs in, a caller fenced by it, and the
// logical session's W3b state (design docs/design/agentic-parity.md,
// sections 2.6 and 3.5 to 3.9; notes in docs/notes/w3b.md).
type w3bPage struct {
	*pageRuntime
	ps      *core.PageSession
	caller  webkit.Caller // the operation's fenced caller, or the page's
	op      *core.Op
	end     func()
	session string
	epoch   uint64
	st      *sessionW3b
}

// errNoScheduler: a page session made outside the daemon without a
// scheduler cannot run the attempt loop of design 2.6.
var errNoScheduler = errors.New("actions need the page scheduler of the daemon (core.Scheduler); this page session has none")

// openPage opens the command's page. In the daemon the command runs inside
// its operation (Env.Op); a page session with a scheduler but no
// operation (tests, embedders) gets its own mutating operation, which
// close ends.
func openPage(ctx context.Context, env Env, name string) (*w3bPage, error) {
	p, err := openRuntime(ctx, env)
	if err != nil {
		return nil, err
	}
	ps, err := env.Page(ctx)
	if err != nil {
		return nil, err
	}
	w := &w3bPage{pageRuntime: p, ps: ps, caller: ps.Caller, end: func() {}}
	if oe, ok := env.(interface{ Op() *core.Op }); ok {
		w.op = oe.Op()
	}
	if ps.Sched != nil {
		w.session, w.epoch = ps.Sched.Lease()
		if w.op == nil {
			op, err := ps.Sched.Begin(ctx, core.OpSpec{Name: name, Class: core.ClassMutate, Session: w.session, Epoch: w.epoch, PageWork: true})
			if err != nil {
				return nil, err
			}
			w.op, w.end = op, op.End
			w.rt = ps.Runtime.Via(op.Caller())
		}
	}
	if w.op != nil {
		w.caller = w.op.Caller()
	}
	w.st = stateFor(ps, w.session, w.epoch)
	return w, nil
}

func (w *w3bPage) close() { w.end() }

// dropStates forgets the W3b states of a page record whose ref table was
// collected.
func dropStates(k weak.Pointer[refs.Table]) {
	statesMu.Lock()
	defer statesMu.Unlock()
	delete(states, k)
}

// actionReplyReserve: the attempts of an action end this long before the
// command deadline (core.Action.Reserve), so the reply of the last attempt
// arrives inside it and an element that never became ready fails with its
// reason instead of an unknown outcome (design 3.5: the action timeout
// lies inside the command deadline). A short deadline gets a quarter.
const actionReplyReserve = 250 * time.Millisecond

func reserveFor(ctx context.Context) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return actionReplyReserve
	}
	return min(actionReplyReserve, time.Until(d)/4)
}

// sessionW3b is the W3b state of one logical session on one page record,
// kept by the daemon across CLI processes: the dialog bridge, the forced
// hover, the last screenshot hash and whether the synthetic-input note
// was shown.
type sessionW3b struct {
	mu       sync.Mutex
	dialogs  dialogBridge
	noted    bool
	hoverCSS bool // hover --css holds the CSS domain and maybe a forced :hover
	lastShot string
}

// states holds the W3b state per page record and logical session. The
// page record's ref table (one per record, docs/notes/w3a-wiring.md) is
// the key, held weakly: when the record and its table are gone, a cleanup
// drops the record's states, so a daemon that sees many pages does not
// keep them. The state outlives leases on purpose: answers issued under an
// earlier lease are reported as expired by the session's next command.
var (
	statesMu sync.Mutex
	states   = map[weak.Pointer[refs.Table]]map[string]*sessionW3b{}
)

// stateFor returns the session's W3b state on ps. A new lease epoch starts
// a new dialog queue (design 3.7: release clears it); answers issued
// under the old lease and never acknowledged are reported as expired.
func stateFor(ps *core.PageSession, session string, epoch uint64) *sessionW3b {
	statesMu.Lock()
	defer statesMu.Unlock()
	k := weak.Make(ps.Refs)
	byS := states[k]
	if byS == nil {
		byS = map[string]*sessionW3b{}
		states[k] = byS
		if ps.Refs != nil { // a page session without a ref table has one shared entry
			goruntime.AddCleanup(ps.Refs, dropStates, k)
		}
	}
	s := byS[session]
	if s == nil {
		s = &sessionW3b{}
		byS[session] = s
	}
	s.mu.Lock()
	s.dialogs.newEpoch(epoch)
	s.mu.Unlock()
	return s
}

// spec resolves a command target (ref, CSS selector or locator); "" is none.
func (w *w3bPage) spec(ctx context.Context, t string) (*targetSpec, error) {
	if t == "" {
		return nil, nil
	}
	s, err := w.target(ctx, t)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// action is one side-effecting command as runtime attempts (design 2.6).
type action struct {
	Name    string // command name for messages
	Kind    string // runtime dispatcher (55-actions.js, 60-keyboard.js)
	Target  string // "" for page-level actions (press, keyboard, mouse)
	Target2 string // drag destination
	Force   bool
	Gesture bool // each attempt is the probe actProbe, then actGesture with emulateUserGesture (core.Action.Probe)
	Steps   int
	Delay   time.Duration
	Args    func(step int) map[string]any // kind-specific fields per step
	Form    *formCtx                      // fill, type, check, select and upload: the words of their refusals (forms045.go)
	// Needs are the runtime modules that hold Kind's dispatcher or its
	// checks (internal/runtime With, parity-045 decision D5): each attempt
	// first makes sure they are installed in the document it runs in.
	Needs []runtime.Module
}

// actValue is the value of one dispatched step (50-actionability.js act).
type actValue struct {
	D   []string        `json:"d"`
	Err string          `json:"err"`
	Wrn string          `json:"warn"`
	V   json.RawMessage `json:"v"`
	Dlg []dialogUse     `json:"dlg"`
	FW  string          `json:"fw"` // facts of a form read-back warning (forms045.go)
}

// actResult is what an action reports (design 3.5: "trusted": false and
// every emulated default action by name).
type actResult struct {
	Trusted  bool            `json:"trusted"`
	Steps    int             `json:"steps"`
	Acked    int             `json:"acknowledged"`
	Emulated []string        `json:"emulated"`
	Value    json.RawMessage `json:"value,omitempty"`
	Dialogs  []dialogUse     `json:"dialogs,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// failedPrefix is how core.RunSteps reports a page reply with a status it
// does not know; the act function answers "failed" for a target it
// refuses before the fence (nothing ran).
const failedPrefix = "page answered \"failed\" "

// run runs a in the daemon-owned attempt loop. The dialog bridge is
// active for every action, so a dialog that the action opens is answered
// instead of blocking the page (design 3.7).
func (w *w3bPage) run(ctx context.Context, a action) (*actResult, error) {
	if w.op == nil {
		return nil, errNoScheduler
	}
	t1, err := w.spec(ctx, a.Target)
	if err != nil {
		return nil, err
	}
	t2, err := w.spec(ctx, a.Target2)
	if err != nil {
		return nil, err
	}
	if err := w.clearHover(ctx); err != nil {
		return nil, err
	}
	policy, err := w.dialogsBefore(ctx)
	if err != nil {
		return nil, err
	}
	if a.Steps < 1 {
		a.Steps = 1
	}
	fn, probe := runtime.With("act", a.Needs...), ""
	if a.Gesture {
		fn, probe = runtime.With("actGesture", a.Needs...), runtime.With("actProbe", a.Needs...)
	}
	res, err := w.op.RunSteps(core.Action{Name: a.Name, Fn: fn, Probe: probe, Steps: a.Steps, Delay: a.Delay, Reserve: reserveFor(ctx), Args: func(step int) any {
		m := map[string]any{"k": a.Kind, "dp": policy}
		if t1 != nil {
			m["t"] = t1
		}
		if t2 != nil {
			m["t2"] = t2
		}
		if a.Force {
			m["f"] = true
		}
		if a.Args != nil {
			for k, v := range a.Args(step) {
				m[k] = v
			}
		}
		return m
	}})
	out := &actResult{Trusted: false, Steps: a.Steps, Acked: res.Acked, Emulated: []string{}}
	var failure string
	for _, raw := range res.Values {
		var v actValue
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("%s: decoding the page's step result: %w", a.Name, err)
		}
		out.Emulated = append(out.Emulated, v.D...)
		out.Dialogs = append(out.Dialogs, v.Dlg...)
		if v.Wrn != "" {
			out.Warnings = append(out.Warnings, v.Wrn)
		}
		if v.FW != "" {
			out.Warnings = append(out.Warnings, a.formWarning(v.FW))
		}
		if len(v.V) > 0 && string(v.V) != "null" {
			out.Value = v.V
		}
		if v.Err != "" {
			failure = v.Err
		}
	}
	w.st.mu.Lock()
	w.st.dialogs.acknowledge(out.Dialogs)
	w.st.mu.Unlock()
	if err != nil {
		err = actionError(err)
		var re *runtime.Error
		if errors.As(err, &re) {
			if fe := a.formError(re.Code, re.Message); fe != nil {
				err = fe
			}
		}
		return out, err
	}
	if failure != "" {
		// A form refusal after the fence: fill's focus guard (only focus
		// moved) or the option match of a combobox, which step 1 opened.
		if code, facts, ok := strings.Cut(failure, ": "); ok {
			if fe := a.formError(code, facts); fe != nil {
				return out, fe
			}
		}
		return out, fmt.Errorf("%s: %s", a.Name, failure)
	}
	return out, nil
}

// actionError turns the page's refusal of a target (ambiguous, stale or
// unknown ref, bad selector), which the page answers before the fence as
// status "failed", into the runtime error it names. Other stop reasons
// stay core.StepError (cancel, deadline, lost reply).
func actionError(err error) error {
	var se *core.StepError
	if errors.As(err, &se) && strings.HasPrefix(se.Reason, failedPrefix) && !se.MayHaveRun {
		code, msg, _ := strings.Cut(strings.TrimPrefix(se.Reason, failedPrefix), ": ")
		return &runtime.Error{Code: code, Message: msg}
	}
	return err
}

// text renders an action result: what happened, the emulated default
// actions, dialogs and the synthetic-input note once per session.
func (w *w3bPage) text(done string, r *actResult) string {
	lines := []string{done}
	for _, e := range r.Emulated {
		lines = append(lines, "emulated default action: "+e)
	}
	lines = append(lines, dialogLines(r.Dialogs)...)
	for _, wn := range r.Warnings {
		lines = append(lines, "warning: "+wn)
	}
	lines = append(lines, w.notes()...)
	w.st.mu.Lock()
	if !w.st.noted {
		w.st.noted = true
		lines = append(lines, "note: synthetic event (isTrusted=false); behavior that checks isTrusted or needs a user gesture may differ")
	}
	w.st.mu.Unlock()
	return strings.Join(lines, "\n")
}

// result is the command result of an action.
func (w *w3bPage) result(done string, r *actResult) Result {
	return Result{Text: w.text(done, r), Data: r}
}

// notes returns the dialog bridge's pending reports (expired answers).
func (w *w3bPage) notes() []string {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	return w.st.dialogs.takeNotes()
}

// clearHover ends a hover --css forced state at the next action (design
// 3.5 hover; 2.4 forced pseudo state). The release is one change of the
// desired state (core.DesiredState.Change) with core's pending cleanup
// (core.PageSession.ReleaseOwnerChange): a reset or disable that fails
// leaves CSS unknown after the owner left, and the same change then resets
// a CSS cleanup that is pending (core.ResetUnknownAgent). Until that all
// succeeds, the flag stays set and the next action tries again: when the
// change could not begin before ctx ended, the hover owner still holds its
// state; when a send failed, the CSS reset is still due.
func (w *w3bPage) clearHover(ctx context.Context) error {
	w.st.mu.Lock()
	held := w.st.hoverCSS
	w.st.hoverCSS = false
	w.st.mu.Unlock()
	if !held || w.ps.State == nil {
		return nil
	}
	err := w.ps.State.Change(ctx, w.caller, func(c webkit.Caller) error {
		if _, err := w.ps.ReleaseOwnerChange(ctx, c, hoverOwner(w.session)); err != nil {
			return err
		}
		return core.ResetUnknownAgent(ctx, w.ps, c, "CSS")
	})
	if err != nil {
		w.st.mu.Lock()
		w.st.hoverCSS = true
		w.st.mu.Unlock()
	}
	return err
}

func hoverOwner(session string) core.Owner { return core.SessionOwner("w3b-hover", session) }

// nodeOf returns the protocol node id of a target: the runtime resolves it
// (pick) and DOM.requestNode maps the object to a node.
func (w *w3bPage) nodeOf(ctx context.Context, spec *targetSpec) (int, error) {
	if err := w.rt.Call(ctx, "ping", nil, nil); err != nil {
		return 0, err
	}
	args, err := json.Marshal(map[string]any{"t": spec})
	if err != nil {
		return 0, err
	}
	expr := fmt.Sprintf(`(function(){var r=window.__iwdp.call("pick",%s);if(!r.ok){throw new Error(r.error.code+": "+r.error.message)}return r.value})()`, args)
	raw, err := w.caller.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr})
	if err != nil {
		return 0, err
	}
	var ev struct {
		Result struct {
			ObjectID    string `json:"objectId"`
			Description string `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return 0, fmt.Errorf("resolving the target: %w", err)
	}
	if ev.WasThrown || ev.Result.ObjectID == "" {
		return 0, fmt.Errorf("resolving the target: %s", strings.TrimPrefix(ev.Result.Description, "Error: "))
	}
	if _, err := w.caller.Send(ctx, "DOM.getDocument", map[string]any{"depth": 0}); err != nil {
		return 0, err
	}
	raw, err = w.caller.Send(ctx, "DOM.requestNode", map[string]any{"objectId": ev.Result.ObjectID})
	if err != nil {
		return 0, err
	}
	var n struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(raw, &n); err != nil || n.NodeID == 0 {
		return 0, fmt.Errorf("DOM.requestNode gave no node id: %s", raw)
	}
	return n.NodeID, nil
}
