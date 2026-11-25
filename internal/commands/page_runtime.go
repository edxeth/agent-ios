package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/refs"
	"github.com/edxeth/agent-ios/internal/runtime"
)

// pageRuntime is what the W3a commands (snapshot, get, is, find, hit-test,
// read) use of a page session: the agent runtime and the session's ref
// table (design docs/design/agentic-parity.md, sections 3.1 and 3.3).
type pageRuntime struct {
	rt      *runtime.Runtime
	refs    *refs.Table
	session string // whose snapshot history this command adds to (historySession)
}

// errNoRuntime: the daemon handed out a page session without the W3a parts.
var errNoRuntime = errors.New("this page session has no agent runtime; the daemon must create it with runtime.New and refs.NewTable")

// openRuntime returns the runtime of the command's page. With a scheduler,
// the runtime's owner is the lease holder and epoch (design 3.1 owner
// epoch), so the page resets its ref maps for a new holder; the page's
// reset reports reach the session's ref table. In the daemon the command
// runs inside an operation, and its runtime calls go through the
// operation's fenced caller (design 2.5, 2.10): they count against the
// per-page limit of protocol calls in flight, and none is sent after the
// operation ended or its lease epoch changed.
func openRuntime(ctx context.Context, env Env) (*pageRuntime, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return nil, err
	}
	if ps == nil || ps.Runtime == nil || ps.Refs == nil {
		return nil, errNoRuntime
	}
	setOwnerFromLease(ps)
	t := ps.Refs
	ps.Runtime.OnReset(func(i runtime.ResetInfo) { t.NoteReset(i.Reason, i.MaxRef) })
	rt := ps.Runtime
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		rt = rt.Via(oe.Op().Caller())
		releaseEarlierFrames(ctx, env, ps, oe.Op())
		// The session's frame scope (frame.go, design 3.8).
		if _, err := applyFrameScope(ps, oe.Op(), rt.Generation()); err != nil {
			return nil, err
		}
	}
	return &pageRuntime{rt: rt, refs: t, session: historySession(ps)}, nil
}

// historySession is the logical session whose snapshot history a command
// of ps reads and adds to: the lease holder, which runs the command. The
// ref table belongs to the page and serves every lease owner in turn, but
// "diff snapshot" compares the last two snapshots of this session, so the
// table keeps the history per session. Without a scheduler (a page
// session made outside the daemon) it is "".
func historySession(ps *core.PageSession) string {
	if ps.Sched == nil {
		return ""
	}
	holder, _ := ps.Sched.Lease()
	return holder
}

func setOwnerFromLease(ps *core.PageSession) {
	if ps.Sched == nil {
		return
	}
	holder, epoch := ps.Sched.Lease()
	if holder == "" {
		return
	}
	daemon := ps.DaemonID
	if daemon == "" {
		daemon = runtime.DaemonID
	}
	ps.Runtime.SetOwner(runtime.Owner{Daemon: daemon, Session: holder, Epoch: epoch})
}

// targetSpec is a target as the page resolver takes it (45-locate.js
// resolve): a ref with what the table knows of it, or a selector (CSS or a
// locator form such as text=... or role=button[name="Save"]).
type targetSpec struct {
	Ref      string `json:"ref,omitempty"`
	Doc      string `json:"doc,omitempty"`
	Role     string `json:"role,omitempty"`
	Name     string `json:"name,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
	Selector string `json:"selector,omitempty"`
}

// target turns a command's target argument into a targetSpec. A ref runs
// steps 1 and 2 of the resolve order of design 3.3 in the table; the page
// runs the rest. A ref the table does not know, and every ref while the
// page has not accepted the runtime's current owner, gets one ping first:
// the page reports a reset of its maps for another owner with the next
// call (design 3.1). The report reaches the table before the lookup, so it
// decides between "no snapshot produced", "daemon restarted" and "the
// lease changed", and a ref of this runtime's earlier owner does not reach
// the page. The page itself refuses refs that its last reset dropped
// (45-locate.js resolveRef), for owner changes made through other runtimes.
func (p *pageRuntime) target(ctx context.Context, s string) (targetSpec, error) {
	n, ok := refs.ParseRef(s)
	if !ok {
		return targetSpec{Selector: s}, nil
	}
	e, err := p.refs.Lookup(n, p.rt.Generation())
	var re *refs.Error
	if (errors.As(err, &re) && re.Code == "unknown_ref") || !p.rt.OwnerConfirmed() {
		if perr := p.rt.Call(ctx, "ping", nil, nil); perr != nil {
			return targetSpec{}, perr
		}
		e, err = p.refs.Lookup(n, p.rt.Generation())
	}
	if err != nil {
		return targetSpec{}, err
	}
	return targetSpec{Ref: refs.Format(n), Doc: e.Doc, Role: e.Role, Name: e.Name, Unique: e.Unique}, nil
}

// allocated is the part of a page result that hands out refs (snapshot,
// find, hitTest in the runtime).
type allocated struct {
	Refs []struct {
		Ref    string `json:"ref"`
		Role   string `json:"role"`
		Name   string `json:"name"`
		Unique bool   `json:"unique"`
	} `json:"refs"`
	NextRef int    `json:"nextRef"`
	Doc     string `json:"doc"`
}

// allocate runs the page function fn, which hands out refs from
// args["nextRef"] on, inside the table's serialized allocation (design
// 3.3), records the refs it returns and decodes its value into out. It
// returns the page's warnings and the document generation the refs belong
// to.
func (p *pageRuntime) allocate(ctx context.Context, fn string, args map[string]any, out any) ([]string, uint64, error) {
	var warnings []string
	var gen uint64
	err := p.refs.Allocate(ctx, func(next int) (refs.Allocation, error) {
		// The generation is read before the call: a navigation during the
		// call then makes these refs stale instead of current.
		gen = p.rt.Generation()
		args["nextRef"] = next
		var raw json.RawMessage
		w, err := p.rt.CallWarn(ctx, fn, args, &raw)
		warnings = w
		if err != nil {
			return refs.Allocation{}, err
		}
		var a allocated
		if err := json.Unmarshal(raw, &a); err != nil {
			return refs.Allocation{}, fmt.Errorf("runtime call %s: decoding refs: %w", fn, err)
		}
		alloc := refs.Allocation{Next: a.NextRef, Generation: gen, Doc: a.Doc}
		for _, r := range a.Refs {
			n, ok := refs.ParseRef(r.Ref)
			if !ok {
				return alloc, fmt.Errorf("runtime call %s: the page returned ref %q", fn, r.Ref)
			}
			alloc.Refs = append(alloc.Refs, refs.Entry{Ref: n, Role: r.Role, Name: r.Name, Unique: r.Unique})
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return alloc, fmt.Errorf("runtime call %s: decoding value: %w", fn, err)
		}
		return alloc, nil
	})
	return warnings, gen, err
}

// withWarnings appends the page's warnings to text, one "warning:" line each.
func withWarnings(text string, warnings []string) string {
	if len(warnings) == 0 {
		return text
	}
	lines := make([]string, 0, len(warnings)+1)
	lines = append(lines, text)
	for _, w := range warnings {
		lines = append(lines, "warning: "+w)
	}
	return strings.Join(lines, "\n")
}

// flagBool and flagString read parsed flags; an absent flag is false or "".
func flagBool(in Input, name string) bool {
	b, _ := in.Flags[name].(bool)
	return b
}

func flagString(in Input, name string) string {
	s, _ := in.Flags[name].(string)
	return s
}

// arg returns positional argument i, or "" when it was not given.
func arg(in Input, i int) string {
	if i < len(in.Args) {
		return in.Args[i]
	}
	return ""
}

func usagef(format string, a ...any) error { return &UsageError{fmt.Sprintf(format, a...)} }
