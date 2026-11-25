package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/dialogpolicy"
)

// dialog: the dialog bridge of design 3.7 (agent-browser "dialog"). WIP
// has no dialog events, and a native dialog blocks Runtime.evaluate, so
// the runtime replaces alert, confirm and prompt (80-dialogs.js) in the
// current document and, through the bootstrap part "dialog", in new ones.
// Answers are armed first and issued at most once (dialogBridge).
func init() {
	Register(&Command{
		Name:    "dialog",
		Summary: "Arm answers for alert, confirm and prompt, or show the dialog log (status, accept [text], dismiss, default accept|dismiss).",
		Args: []Arg{
			{Name: "action", Help: "status, accept, dismiss or default", Required: true},
			{Name: "value", Help: "accept: the prompt text; default: accept or dismiss"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runDialog,
	})
}

// Dialog policies (decision D6): with no answer armed, "dismiss" makes
// confirm return false and prompt null; "accept" makes confirm return true
// and prompt its default value. alert is always accepted.
const (
	policyDismiss = dialogpolicy.Dismiss
	policyAccept  = dialogpolicy.Accept
)

// dialogUse is one dialog the page answered (80-dialogs.js handle).
type dialogUse struct {
	ID      string `json:"id"` // the answer used; "" when the policy answered
	Type    string `json:"type"`
	Message string `json:"message"`
	Result  any    `json:"result"`
}

type answerState int

const (
	answerQueued       answerState = iota // in the daemon only
	answerIssued                          // written into one document (or its write was sent)
	answerAcknowledged                    // the page reported its use
	answerExpired                         // its document went away unacknowledged, or a withdrawal reply was lost
)

// dialogAnswer is one armed answer.
type dialogAnswer struct {
	ID     string  `json:"id"`
	Accept bool    `json:"accept"`
	Text   *string `json:"text,omitempty"`
	state  answerState
}

// dialogBridge is the daemon side of design 3.7 for one session on one
// page: at-most-once issuance of armed answers, acknowledgment only by a
// reply from the page, and expiry with the document. Callers hold the
// session state's lock.
type dialogBridge struct {
	epoch   uint64
	started bool
	policy  string
	active  bool // bootstrap part and Console owner set under this epoch
	next    int
	answers []*dialogAnswer
	log     []dialogUse
	notes   []string
}

// newEpoch starts the queue of a new lease epoch: queued answers are
// dropped (release clears the queue) and issued, unacknowledged ones expire.
func (b *dialogBridge) newEpoch(e uint64) {
	if b.started && b.epoch == e {
		return
	}
	if b.started {
		for _, a := range b.answers {
			if a.state == answerIssued {
				b.expire(a)
			}
		}
	}
	b.answers, b.active, b.epoch, b.started = nil, false, e, true
}

func (b *dialogBridge) policyOrDefault() string {
	if b.policy == "" {
		return dialogpolicy.Default
	}
	return b.policy
}

func (b *dialogBridge) arm(accept bool, text *string) *dialogAnswer {
	b.next++
	a := &dialogAnswer{ID: fmt.Sprintf("d%d", b.next), Accept: accept, Text: text}
	b.answers = append(b.answers, a)
	return a
}

// pendingWork reports whether answers wait to be issued or acknowledged.
func (b *dialogBridge) pendingWork() bool {
	for _, a := range b.answers {
		if a.state == answerQueued || a.state == answerIssued {
			return true
		}
	}
	return false
}

// toIssue marks every queued answer issued and returns them; from here on
// they count as issued, even when the reply of the call is lost.
func (b *dialogBridge) toIssue() []*dialogAnswer {
	var out []*dialogAnswer
	for _, a := range b.answers {
		if a.state == answerQueued {
			a.state = answerIssued
			out = append(out, a)
		}
	}
	return out
}

func (b *dialogBridge) find(id string) *dialogAnswer {
	for _, a := range b.answers {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (b *dialogBridge) expire(a *dialogAnswer) {
	a.state = answerExpired
	b.notes = append(b.notes, fmt.Sprintf("dialog answer %s expired with its document; arm it again if it is still needed", a.ID))
}

// acknowledge records uses that a page reply reported.
func (b *dialogBridge) acknowledge(uses []dialogUse) {
	for _, u := range uses {
		if a := b.find(u.ID); a != nil && a.state == answerIssued {
			a.state = answerAcknowledged
		}
		b.log = append(b.log, u)
	}
}

// dialogsReply is the value of the runtime function "dialogs".
type dialogsReply struct {
	Doc       string      `json:"doc"`
	Used      []dialogUse `json:"used"`
	Pending   []string    `json:"pending"`
	Withdrawn []string    `json:"withdrawn"`
	On        bool        `json:"on"`
}

// apply records a reply of the page: withdrawn answers return to the
// queue, reported uses are acknowledged, and an issued answer that the
// page neither holds nor reported used is gone with its document (or a
// reset): it expires.
func (b *dialogBridge) apply(r dialogsReply) {
	for _, id := range r.Withdrawn {
		if a := b.find(id); a != nil && a.state == answerIssued {
			a.state = answerQueued
		}
	}
	b.acknowledge(r.Used)
	held := map[string]bool{}
	for _, id := range r.Pending {
		held[id] = true
	}
	for _, a := range b.answers {
		if a.state == answerIssued && !held[a.ID] {
			b.expire(a)
		}
	}
}

// withdrawLost: the reply of a withdrawing call was lost; the answers may
// still be in the old document, so they expire (design 3.7).
func (b *dialogBridge) withdrawLost() {
	for _, a := range b.answers {
		if a.state == answerIssued {
			b.expire(a)
		}
	}
}

func (b *dialogBridge) takeNotes() []string {
	n := b.notes
	b.notes = nil
	return n
}

// dialogLines describes dialogs the page answered.
func dialogLines(uses []dialogUse) []string {
	var out []string
	for _, u := range uses {
		res, _ := json.Marshal(u.Result)
		if u.Type == "alert" {
			res = []byte("accepted")
		}
		by := "no answer armed: session default"
		if u.ID != "" {
			by = "answer " + u.ID
		}
		out = append(out, fmt.Sprintf("dialog %s %q answered %s (%s)", u.Type, u.Message, res, by))
	}
	return out
}

// activateDialogs makes the session's dialog override part of the desired
// state (dialogpolicy.Install: the bootstrap part "dialog" and the
// bridge's Console ownership).
func (w *w3bPage) activateDialogs(ctx context.Context, policy string) error {
	if w.ps.State != nil {
		if err := dialogpolicy.Install(ctx, w.ps.State, w.caller, w.session, policy); err != nil {
			return err
		}
	}
	w.st.mu.Lock()
	w.st.dialogs.active = true
	w.st.mu.Unlock()
	return nil
}

// dialogsBefore prepares the bridge for a page call that can open a
// dialog: it activates the override and issues queued answers into the
// current document. It returns the policy the call carries.
func (w *w3bPage) dialogsBefore(ctx context.Context) (string, error) {
	w.st.mu.Lock()
	b := &w.st.dialogs
	active, policy, work := b.active, b.policyOrDefault(), b.pendingWork()
	w.st.mu.Unlock()
	if !active {
		if err := w.activateDialogs(ctx, policy); err != nil {
			return "", err
		}
	}
	if work {
		if _, err := w.dialogSync(ctx, map[string]any{}); err != nil {
			return "", err
		}
	}
	return policy, nil
}

// dialogSync runs the runtime function "dialogs" with extra (withdraw,
// nav, hist) and the queued answers to issue, and records the reply.
func (w *w3bPage) dialogSync(ctx context.Context, extra map[string]any) (dialogsReply, error) {
	w.st.mu.Lock()
	b := &w.st.dialogs
	args := map[string]any{}
	for k, v := range extra {
		args[k] = v
	}
	if b.active {
		args["policy"] = b.policyOrDefault()
	}
	if extra["withdraw"] != true {
		if issue := b.toIssue(); len(issue) > 0 {
			args["issue"] = issue
		}
	}
	w.st.mu.Unlock()
	var r dialogsReply
	err := w.rt.Call(ctx, "dialogs", args, &r)
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	if err != nil {
		if extra["withdraw"] == true {
			b.withdrawLost()
		}
		return r, err
	}
	b.apply(r)
	return r, nil
}

func runDialog(ctx context.Context, env Env, in Input) (Result, error) {
	action, value := arg(in, 0), arg(in, 1)
	switch action {
	case "status", "accept", "dismiss", "default":
	default:
		return Result{}, usagef("dialog %q: want status, accept, dismiss or default", action)
	}
	if action == "default" && value != policyAccept && value != policyDismiss {
		return Result{}, usagef("dialog default %q: want accept or dismiss", value)
	}
	if action == "dismiss" && value != "" {
		return Result{}, usagef("dialog dismiss takes no value")
	}
	w, err := openPage(ctx, env, "dialog")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	var lines []string
	w.st.mu.Lock()
	b := &w.st.dialogs
	switch action {
	case "accept", "dismiss":
		var text *string
		if action == "accept" && len(in.Args) > 1 {
			text = &value
		}
		a := b.arm(action == "accept", text)
		desc := action
		if text != nil {
			desc += fmt.Sprintf(" %q", *text)
		}
		lines = append(lines, fmt.Sprintf("armed dialog answer %s: %s", a.ID, desc))
	case "default":
		b.policy = value
		b.active = false // the bootstrap part carries the policy
		lines = append(lines, "dialogs without an armed answer: "+value)
	}
	active, policy := b.active, b.policyOrDefault()
	w.st.mu.Unlock()
	if action != "status" || active {
		// One page call: install with the policy, issue queued answers,
		// collect reports.
		if !active {
			if err := w.activateDialogs(ctx, policy); err != nil {
				return Result{}, err
			}
		}
		if _, err := w.dialogSync(ctx, map[string]any{}); err != nil {
			return Result{}, err
		}
	}
	w.st.mu.Lock()
	b = &w.st.dialogs
	data := map[string]any{"policy": b.policyOrDefault(), "active": b.active, "log": b.log}
	var queued, issued []string
	for _, a := range b.answers {
		switch a.state {
		case answerQueued:
			queued = append(queued, a.ID)
		case answerIssued:
			issued = append(issued, a.ID)
		}
	}
	data["queued"], data["issued"] = queued, issued
	if action == "status" {
		state := "off (no action or dialog command in this session yet)"
		if b.active {
			state = "on"
		}
		lines = append(lines, "dialog override: "+state, "default: "+b.policyOrDefault(),
			fmt.Sprintf("answers waiting in the page: %d; queued in the daemon: %d", len(issued), len(queued)))
		lines = append(lines, dialogLines(b.log)...)
	}
	lines = append(lines, b.takeNotes()...)
	w.st.mu.Unlock()
	return Result{Text: strings.Join(lines, "\n"), Data: data}, nil
}
