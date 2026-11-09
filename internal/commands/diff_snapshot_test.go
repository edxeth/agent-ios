package commands

import (
	"fmt"
	"strings"
	"testing"
	"testing/quick"
)

// apply rebuilds both sides from a diff: kept and removed lines give a,
// kept and added lines give b.
func apply(ops []lineOp) (a, b []string) {
	for _, o := range ops {
		if o.kind != '+' {
			a = append(a, o.text)
		}
		if o.kind != '-' {
			b = append(b, o.text)
		}
	}
	return a, b
}

// lcs is the reference oracle: the length of a longest common subsequence
// by dynamic programming.
func lcs(a, b []string) int {
	t := make([][]int, len(a)+1)
	for i := range t {
		t[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	return t[0][0]
}

// toLines maps random bytes to lines from a small alphabet, so equal lines
// are common.
func toLines(bs []byte) []string {
	out := make([]string, len(bs)%40)
	for i := range out {
		out[i] = string(rune('a' + bs[i]%4))
	}
	return out
}

// Property: diffLines is an edit script from a to b (it rebuilds both
// sides) and a shortest one (its edits are len(a)+len(b)-2*LCS, by the
// dynamic-programming oracle).
func TestDiffLinesIsShortestEditScript(t *testing.T) {
	prop := func(x, y []byte) bool {
		a, b := toLines(x), toLines(y)
		ops := diffLines(a, b)
		ga, gb := apply(ops)
		edits := 0
		for _, o := range ops {
			if o.kind != ' ' {
				edits++
			}
		}
		return strings.Join(ga, ",") == strings.Join(a, ",") && strings.Join(gb, ",") == strings.Join(b, ",") &&
			edits == len(a)+len(b)-2*lcs(a, b)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// Past maxDiffEdits the middle is shown as removed and added: still a
// correct script.
func TestDiffLinesFallbackStaysCorrect(t *testing.T) {
	var a, b []string
	for i := 0; i < maxDiffEdits; i++ {
		a = append(a, fmt.Sprint("a", i))
		b = append(b, fmt.Sprint("b", i))
	}
	a = append([]string{"same"}, append(a, "end")...)
	b = append([]string{"same"}, append(b, "end")...)
	ops := diffLines(a, b)
	ga, gb := apply(ops)
	if strings.Join(ga, ",") != strings.Join(a, ",") || strings.Join(gb, ",") != strings.Join(b, ",") {
		t.Fatal("the fallback diff does not rebuild both sides")
	}
	if ops[0] != (lineOp{' ', "same"}) || ops[len(ops)-1] != (lineOp{' ', "end"}) {
		t.Fatal("the fallback lost the common prefix or suffix")
	}
}

// The hunk text equals GNU diffutils 3.12 "diff -U3 --label before --label
// after" on the same inputs (cases with one shortest script).
func TestUnifiedMatchesGNUDiff(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{
			"l1 l2 l3 l4 l5 l6 l7 l8 l9 l10 l11 l12", "l1 X l3 l4 l5 l6 l7 l8 l9 l10 Y l12",
			"--- before\n+++ after\n@@ -1,5 +1,5 @@\n l1\n-l2\n+X\n l3\n l4\n l5\n@@ -8,5 +8,5 @@\n l8\n l9\n l10\n-l11\n+Y\n l12\n",
		},
		{"x", "x y", "--- before\n+++ after\n@@ -1 +1,2 @@\n x\n+y\n"},
		{"x y", "y", "--- before\n+++ after\n@@ -1,2 +1 @@\n-x\n y\n"},
		{"a", "b", "--- before\n+++ after\n@@ -1 +1 @@\n-a\n+b\n"},
		{"x", "", "--- before\n+++ after\n@@ -1 +0,0 @@\n-x\n"},
	} {
		if got := unified(diffLines(strings.Fields(c.a), strings.Fields(c.b)), diffContext); got != c.want {
			t.Errorf("diff %q -> %q:\n%s\nwant:\n%s", c.a, c.b, got, c.want)
		}
	}
}
