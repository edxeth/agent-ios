package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/refs"
	"github.com/edxeth/agent-ios/internal/runtime"
)

// diff snapshot (design docs/design/agentic-parity.md, section 4.2;
// agent-browser 0.45 "diff snapshot"): a fresh snapshot compared with a
// baseline. Without --baseline the baseline is the last snapshot that this
// session took of the page (snapshot and diff snapshot both replace it, so
// each diff becomes the next baseline), and the fresh snapshot uses that
// snapshot's options; with --baseline it is a file of snapshot text
// (agent-ios snapshot > before.txt). The [ref=eN] annotations are removed
// from both sides before the comparison. The text follows agent-browser's:
// a unified diff with three lines of context and the headers "before" and
// "after", then the counts, or "No changes detected".
func init() {
	Register(&Command{
		Name: "diff snapshot",
		Summary: "Compare a fresh snapshot with this session's last snapshot of the page (taken with the same options) or with --baseline <file> " +
			"of saved snapshot text, as a unified line diff without the [ref=eN] annotations.",
		Flags: []Flag{
			{Name: "baseline", Short: "b", Type: FlagPath, Help: "compare with this file of snapshot text (agent-ios snapshot > before.txt) instead of the last snapshot"},
			{Name: "selector", Short: "s", Type: FlagString, Help: "snapshot only this element: CSS selector, locator or @ref"},
			{Name: "interactive", Short: "i", Type: FlagBool, Help: "show only elements with refs"},
			{Name: "compact", Short: "c", Type: FlagBool, Help: "keep lines with refs, values or text and their ancestors"},
			{Name: "depth", Short: "d", Type: FlagInt, Help: "deepest level shown (0 = top level only)"},
			{Name: "urls", Short: "u", Type: FlagBool, Help: "add url= to links"},
		},
		Page:    LeasedPage,
		Mutates: true, // the fresh snapshot allocates refs (design 2.11)
		Output:  OutputText,
		Run:     runDiffSnapshot,
	})
}

// Error codes of diff snapshot (agent-browser 0.45 precondition codes).
const (
	codeFileNotFound     = "file_not_found"
	codeNoBaseline       = "no_baseline"
	codeBaselineMismatch = "baseline_mismatch"
)

func codedError(code, format string, a ...any) error {
	return &runtime.Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

func runDiffSnapshot(ctx context.Context, env Env, in Input) (Result, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return Result{}, err
	}
	if ps == nil || ps.Refs == nil || ps.Runtime == nil {
		return Result{}, errNoRuntime
	}
	var before string
	eff := in
	if b := flagString(in, "baseline"); b != "" {
		if before, err = readBaseline(env.ResolvePath(b)); err != nil {
			return Result{}, err
		}
	} else {
		last, ok := ps.Refs.LastSnapshot(historySession(ps))
		if !ok {
			return Result{}, codedError(codeNoBaseline, "no earlier snapshot on this tab; run 'snapshot' first or pass --baseline <file>")
		}
		if last.FrameID != frameScopeID(ps, ps.Runtime.Generation()) {
			return Result{}, codedError(codeBaselineMismatch, "the last snapshot was taken in a different frame; select that frame or run 'snapshot' again, or pass --baseline <file>")
		}
		opts, err := implicitBaselineOptions(in, last.Options)
		if err != nil {
			return Result{}, err
		}
		eff = Input{Args: in.Args, Flags: snapshotFlags(opts), Global: in.Global}
		before = last.Text
	}
	after, warnings, err := takeSnapshot(ctx, env, eff)
	if err != nil {
		return Result{}, err
	}
	d := diffSnapshots(snapshotComparisonText(before), snapshotComparisonText(after))
	data := map[string]any{"changed": d.changed(), "additions": d.add, "removals": d.del, "unchanged": d.same, "diff": d.diff}
	return Result{Text: withWarnings(diffSnapshotText(d.diff, d.add, d.del, d.same), warnings), Data: data}, nil
}

// readBaseline reads a baseline file of diff snapshot and strips one
// trailing newline ("\n" or "\r\n"), which a redirected snapshot ends with.
func readBaseline(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", codedError(codeFileNotFound, "baseline %s does not exist", path)
	}
	if err != nil {
		return "", fmt.Errorf("reading the baseline: %w", err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if len(s) < len(data) {
		s = strings.TrimSuffix(s, "\r")
	}
	return s, nil
}

// takeSnapshot runs snapshot with the flags of in (which also makes it the
// session's last snapshot) and returns its text and warnings.
func takeSnapshot(ctx context.Context, env Env, in Input) (string, []string, error) {
	res, err := runSnapshot(ctx, env, Input{Args: nil, Flags: snapshotFlags(snapshotOptionsOf(in)), Global: in.Global})
	if err != nil {
		return "", nil, err
	}
	m, _ := res.Data.(map[string]any)
	text, ok := m["snapshot"].(string)
	if !ok {
		return "", nil, fmt.Errorf("snapshot returned no text")
	}
	warnings, _ := m["warnings"].([]string)
	return text, warnings, nil
}

// snapshotOptionsOf returns the snapshot options that the flags of in name.
func snapshotOptionsOf(in Input) refs.SnapshotOptions {
	o := refs.SnapshotOptions{
		Selector: flagString(in, "selector"), Interactive: flagBool(in, "interactive"),
		Compact: flagBool(in, "compact"), URLs: flagBool(in, "urls"),
	}
	if d, ok := in.Flags["depth"].(int); ok {
		o.Depth, o.DepthSet = d, true
	}
	return o
}

// snapshotFlags is the flag map of snapshot for the options o.
func snapshotFlags(o refs.SnapshotOptions) map[string]any {
	f := map[string]any{"interactive": o.Interactive, "compact": o.Compact, "urls": o.URLs}
	if o.Selector != "" {
		f["selector"] = o.Selector
	}
	if o.DepthSet {
		f["depth"] = o.Depth
	}
	return f
}

// implicitBaselineOptions returns the options of a diff snapshot without
// --baseline: those the command names, which must agree with the last
// snapshot's, else the last snapshot's (agent-browser
// implicit_baseline_scope). A boolean flag that is not set inherits.
func implicitBaselineOptions(in Input, last refs.SnapshotOptions) (refs.SnapshotOptions, error) {
	o := last
	var differ []string
	if sel, ok := in.Flags["selector"].(string); ok && sel != "" {
		if sel != last.Selector {
			was := "no selector"
			if last.Selector != "" {
				was = fmt.Sprintf("%q", last.Selector)
			}
			differ = append(differ, fmt.Sprintf("--selector %q (last snapshot: %s)", sel, was))
		}
		o.Selector = sel
	}
	if d, ok := in.Flags["depth"].(int); ok {
		if !last.DepthSet || d != last.Depth {
			was := "no depth"
			if last.DepthSet {
				was = fmt.Sprint(last.Depth)
			}
			differ = append(differ, fmt.Sprintf("--depth %d (last snapshot: %s)", d, was))
		}
		o.Depth, o.DepthSet = d, true
	}
	for _, b := range []struct {
		name string
		was  bool
		set  *bool
	}{{"interactive", last.Interactive, &o.Interactive}, {"compact", last.Compact, &o.Compact}, {"urls", last.URLs, &o.URLs}} {
		if flagBool(in, b.name) {
			if !b.was {
				differ = append(differ, "--"+b.name+" (last snapshot: without)")
			}
			*b.set = true
		}
	}
	if len(differ) > 0 {
		return o, codedError(codeBaselineMismatch, "the last snapshot used different options: %s; run 'snapshot' with the same options or pass --baseline <file>", strings.Join(differ, ", "))
	}
	return o, nil
}

// recordSnapshot makes text, which snapshot rendered with the flags of in
// through p, the last snapshot of p's session on this page: the implicit
// baseline of the next diff snapshot.
func recordSnapshot(ctx context.Context, env Env, p *pageRuntime, in Input, text string) {
	frame := ""
	if ps, err := env.Page(ctx); err == nil && ps != nil {
		frame = frameScopeID(ps, p.rt.Generation())
	}
	p.refs.RememberSnapshot(p.session, refs.LastSnapshot{Text: text, Options: snapshotOptionsOf(in), FrameID: frame})
}

// frameScopeID is the frame id of the lease holder's frame scope on ps
// that applyFrameScope applies for the top document generation gen; ""
// for the top document.
func frameScopeID(ps *core.PageSession, gen uint64) string {
	f := framesOf(ps, false)
	if f == nil || ps.Sched == nil {
		return ""
	}
	session, epoch := ps.Sched.Lease()
	f.mu.Lock()
	defer f.mu.Unlock()
	sc, ok := f.scopes[session]
	if !ok || sc.epoch != epoch || sc.gen != gen {
		return ""
	}
	return sc.frameID
}

// snapshotDiff is a line diff of two snapshot texts.
type snapshotDiff struct {
	diff           string // unified diff; "" when nothing changed
	add, del, same int
}

func (d snapshotDiff) changed() bool { return d.add+d.del > 0 }

// diffSnapshots compares two snapshot texts line by line (agent-browser
// diff_snapshots).
func diffSnapshots(before, after string) snapshotDiff {
	ops := diffLines(splitLines(before), splitLines(after))
	var d snapshotDiff
	for _, o := range ops {
		switch o.kind {
		case '+':
			d.add++
		case '-':
			d.del++
		default:
			d.same++
		}
	}
	if d.changed() {
		d.diff = unified(ops, diffContext)
	}
	return d
}

// splitLines splits s into lines; "" has none.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// snapshotComparisonText removes the generated [ref=eN] annotations from a
// snapshot text, for a comparison across documents (agent-browser
// snapshot_comparison_text): in a bracket after a space, outside quoted
// names, the fields "ref=e<digits>" are dropped, and the bracket with its
// space when no field is left. Quoted names keep text that looks like a
// ref.
func snapshotComparisonText(tree string) string {
	out := make([]byte, 0, len(tree))
	quoted := false
	for i := 0; i < len(tree); i++ {
		ch := tree[i]
		if quoted && ch == '\\' {
			out = append(out, ch)
			if i+1 < len(tree) {
				i++
				out = append(out, tree[i])
			}
			continue
		}
		if ch == '"' {
			quoted = !quoted
		}
		if !quoted && ch == '[' && len(out) > 0 && out[len(out)-1] == ' ' {
			end := i + 1
			for end < len(tree) && tree[end] != ']' && tree[end] != '\n' {
				end++
			}
			if end < len(tree) && tree[end] == ']' {
				fields := strings.Split(tree[i+1:end], ", ")
				var kept []string
				for _, f := range fields {
					if !isRefField(f) {
						kept = append(kept, f)
					}
				}
				if len(kept) != len(fields) {
					if len(kept) == 0 {
						out = out[:len(out)-1]
					} else {
						out = append(out, "["+strings.Join(kept, ", ")+"]"...)
					}
					i = end
					continue
				}
			}
		}
		out = append(out, ch)
	}
	return string(out)
}

// isRefField reports whether f is "ref=e" and one or more ASCII digits.
func isRefField(f string) bool {
	id, ok := strings.CutPrefix(f, "ref=e")
	if !ok || id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// diffSnapshotText is the text of diff snapshot; private mode makes it
// from the members that it prints (privateReads).
func diffSnapshotText(diff string, add, del, same int) string {
	if add+del == 0 {
		return "No changes detected"
	}
	return fmt.Sprintf("%s\n%d additions, %d removals, %d unchanged", diff, add, del, same)
}

// diffContext is the number of unchanged lines shown around a change
// (agent-browser: context_radius(3)).
const diffContext = 3

// lineOp is one line of a diff: ' ' kept, '-' only in a, '+' only in b.
type lineOp struct {
	kind byte
	text string
}

// maxDiffEdits bounds the Myers search. Past it the middle part (after the
// common prefix and suffix) is shown as removed and added in full: still a
// correct diff, just not the shortest one. It bounds the time and the
// memory (about maxDiffEdits squared ints) for unrelated snapshots.
const maxDiffEdits = 1000

// diffLines returns a shortest edit script from a to b (Myers' O(ND)
// algorithm) as a list of operations, in order.
func diffLines(a, b []string) []lineOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]lineOp, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		ops = append(ops, lineOp{' ', l})
	}
	ops = append(ops, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, lineOp{' ', l})
	}
	return ops
}

// myers is the greedy forward Myers search. Before each edit count d it
// saves the frontier (the furthest x per diagonal k after d-1 edits), then
// walks back through the saved frontiers to recover the edits.
func myers(a, b []string) []lineOp {
	n, m := len(a), len(b)
	total := n + m
	if total == 0 {
		return nil
	}
	off := total + 1
	v := make([]int, 2*total+3)
	var trace [][]int // trace[d][k+d+1] is v[k] after d-1 edits, k from -d-1 to d+1
	found := -1
	for d := 0; d <= total && d <= maxDiffEdits && found < 0; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: a line of b is inserted
			} else {
				x = v[off+k-1] + 1 // right: a line of a is deleted
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		ops := make([]lineOp, 0, n+m)
		for _, l := range a {
			ops = append(ops, lineOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, lineOp{'+', l})
		}
		return ops
	}
	var rev []lineOp
	x, y := n, m
	for d := found; d > 0; d-- {
		at := func(k int) int { return trace[d][k+d+1] }
		k := x - y
		pk := k - 1 // the edit was a deletion
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1 // the edit was an insertion
		}
		px := at(pk)
		py := px - pk
		for x > px && y > py {
			x, y = x-1, y-1
			rev = append(rev, lineOp{' ', a[x]})
		}
		if pk == k+1 {
			rev = append(rev, lineOp{'+', b[py]})
		} else {
			rev = append(rev, lineOp{'-', a[px]})
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		x, y = x-1, y-1
		rev = append(rev, lineOp{' ', a[x]})
	}
	ops := make([]lineOp, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops
}

// unified formats ops as a unified diff with ctx lines of context.
func unified(ops []lineOp, ctx int) string {
	var b strings.Builder
	b.WriteString("--- before\n+++ after\n")
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		// Extend the hunk while the next change is within 2*ctx kept lines.
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			j := end
			for j < len(ops) && ops[j].kind == ' ' {
				j++
			}
			if j == len(ops) || j-end > 2*ctx {
				end = min(end+ctx, j)
				break
			}
			end = j
		}
		aStart, bStart := 1, 1
		for _, o := range ops[:start] {
			if o.kind != '+' {
				aStart++
			}
			if o.kind != '-' {
				bStart++
			}
		}
		var aLen, bLen int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aLen++
			}
			if o.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, o := range ops[start:end] {
			b.WriteByte(o.kind)
			b.WriteString(o.text)
			b.WriteByte('\n')
		}
		i = end
	}
	return b.String()
}

// hunkRange is a hunk header range: "start,len", where an empty range
// starts one line before (GNU diff convention).
func hunkRange(start, n int) string {
	if n == 0 {
		start--
	}
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}
