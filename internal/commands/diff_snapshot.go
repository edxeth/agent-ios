package commands

import (
	"context"
	"fmt"
	"strings"
)

// diff snapshot: a line diff of the last two snapshots of the session
// (design docs/design/agentic-parity.md, section 4.2). The text follows
// agent-browser's: a unified diff with three lines of context and the
// headers "before" and "after", then the counts, or "No changes detected".
func init() {
	Register(&Command{
		Name:      "diff snapshot",
		MCPName:   "diff_snapshot",
		Profile:   ProfileCore,
		Summary:   "Show what changed between the last two snapshots of this session (a unified line diff).",
		Page:      AnyPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runDiffSnapshot,
	})
}

func runDiffSnapshot(ctx context.Context, env Env, _ Input) (Result, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return Result{}, err
	}
	if ps == nil || ps.Refs == nil {
		return Result{}, errNoRuntime
	}
	before, after, n := ps.Refs.Snapshots()
	if n < 2 {
		return Result{}, fmt.Errorf("diff snapshot needs two snapshots of this session, it has %d; run snapshot, change the page, run snapshot again", n)
	}
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	ops := diffLines(a, b)
	var add, del, same int
	for _, o := range ops {
		switch o.kind {
		case '+':
			add++
		case '-':
			del++
		default:
			same++
		}
	}
	data := map[string]any{"changed": add+del > 0, "additions": add, "removals": del, "unchanged": same}
	if add+del == 0 {
		data["diff"] = ""
		return Result{Text: "No changes detected", Data: data}, nil
	}
	diff := unified(ops, diffContext)
	data["diff"] = diff
	return Result{Text: fmt.Sprintf("%s\n%d additions, %d removals, %d unchanged", diff, add, del, same), Data: data}, nil
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
