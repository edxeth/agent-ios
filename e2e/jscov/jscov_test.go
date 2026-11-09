package jscov

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf16"
)

// recordedSource and recordedBlocks are a script and the answer of
// Runtime.getBasicBlocks for it, recorded from WebKitGTK 2.54 with the
// control-flow profiler on: f(true) runs f's first branch, g never runs.
// The "é" puts a character before the later blocks that is one UTF-16
// code unit and two UTF-8 bytes.
const recordedSource = "(function () {\n  var f = function (a) {\n    if (a) {\n      return 1;\n    }\n    return 2;\n  };\n  f(true);\n  var g = function () {\n    return \"é\";\n  };\n  return 7;\n})()\n//# sourceURL=iwdp-probe.js"

const recordedBlocks = `[{"startOffset":74,"endOffset":87,"hasExecuted":false,"executionCount":0},{"startOffset":161,"endOffset":162,"hasExecuted":false,"executionCount":0},{"startOffset":34,"endOffset":50,"hasExecuted":true,"executionCount":1},{"startOffset":51,"endOffset":67,"hasExecuted":true,"executionCount":1},{"startOffset":88,"endOffset":91,"hasExecuted":false,"executionCount":0},{"startOffset":10,"endOffset":24,"hasExecuted":true,"executionCount":1},{"startOffset":92,"endOffset":114,"hasExecuted":true,"executionCount":1},{"startOffset":148,"endOffset":160,"hasExecuted":true,"executionCount":1},{"startOffset":68,"endOffset":73,"hasExecuted":false,"executionCount":0},{"startOffset":115,"endOffset":147,"hasExecuted":false,"executionCount":0},{"startOffset":1,"endOffset":162,"hasExecuted":true,"executionCount":1},{"startOffset":25,"endOffset":91,"hasExecuted":true,"executionCount":1}]`

// The expected lines follow from what the script ran: "return 2;" (line
// 6) and g's body (line 10) did not run; every other line with code ran.
// Lines 5, 7 and 13 hold a closing brace after a return, which JSC
// reports as an unexecuted block of its own and the converter leaves to
// the enclosing executed function. Line 9 and 11 have code of the outer
// function next to g's unexecuted range. Line 14, a comment at the top
// level, is in no block.
func TestLinesFromRecordedBlocks(t *testing.T) {
	var blocks []Block
	if err := json.Unmarshal([]byte(recordedBlocks), &blocks); err != nil {
		t.Fatal(err)
	}
	got, err := Lines(recordedSource, blocks)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int{1: 1, 2: 1, 3: 1, 4: 1, 5: 1, 6: 0, 7: 1, 8: 1, 9: 1, 10: 0, 11: 1, 12: 1, 13: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines = %v, want %v", got, want)
	}
}

// A block that holds more than a closing brace stays unexecuted, and an
// executed block keeps its count; equal ranges prefer the executed block.
func TestLinesCountsAndTies(t *testing.T) {
	src := "a\n}\nb"
	got, err := Lines(src, []Block{
		{StartOffset: 0, EndOffset: 4, HasExecuted: true, ExecutionCount: 3},
		{StartOffset: 0, EndOffset: 0, HasExecuted: false},
		{StartOffset: 0, EndOffset: 0, HasExecuted: true, ExecutionCount: 0},
		{StartOffset: 1, EndOffset: 2, HasExecuted: false},
		{StartOffset: 4, EndOffset: 4, HasExecuted: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Line 1: the tie of the two one-character blocks goes to the executed
	// one, whose count 0 becomes 1. Line 2: "\n}" is only a closing brace,
	// so the outer block (count 3) counts. Line 3: "b" is its own
	// unexecuted block.
	if want := map[int]int{1: 1, 2: 3, 3: 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines = %v, want %v", got, want)
	}
}

func TestLinesRejectsABlockOutsideTheSource(t *testing.T) {
	for _, b := range []Block{{StartOffset: -1, EndOffset: 0}, {StartOffset: 2, EndOffset: 1}, {StartOffset: 0, EndOffset: 3}} {
		if _, err := Lines("abc", []Block{b}); err == nil || !strings.Contains(err.Error(), "outside the source of 3 UTF-16 code units") {
			t.Errorf("Lines with block %+v: error %v, want one that names the source length", b, err)
		}
	}
}

func TestSourceURLMapsToTheFile(t *testing.T) {
	u := SourceURL("internal/cdp/page_shims.js")
	if f, ok := File(u); !ok || f != "internal/cdp/page_shims.js" {
		t.Fatalf("File(%q) = %q, %v", u, f, ok)
	}
	for _, other := range []string{"", "iwdp-jscov:///", "http://127.0.0.1/page_shims.js", "internal/cdp/page_shims.js"} {
		if f, ok := File(other); ok {
			t.Errorf("File(%q) = %q, true; want no file", other, f)
		}
	}
}

func TestAddAndWriteLCOV(t *testing.T) {
	lines := map[int]int{3: 1, 1: 0}
	Add(lines, map[int]int{1: 2, 7: 0})
	var b strings.Builder
	if err := WriteLCOV(&b, "internal/cdp/page_shims.js", lines); err != nil {
		t.Fatal(err)
	}
	want := "TN:\nSF:internal/cdp/page_shims.js\nDA:1,2\nDA:3,1\nDA:7,0\nLF:3\nLH:2\nend_of_record\n"
	if b.String() != want {
		t.Fatalf("WriteLCOV wrote\n%s\nwant\n%s", b.String(), want)
	}
}

// scriptCase is a random script (characters of one, two and four UTF-8
// bytes, line breaks, closing braces) with random blocks inside it.
type scriptCase struct {
	Source string
	Blocks []Block
}

func (scriptCase) Generate(r *rand.Rand, size int) reflect.Value {
	alphabet := []string{"x", " ", "\n", "}", "é", "😀", ";"}
	var sb strings.Builder
	for n := 1 + r.Intn(size+1); n > 0; n-- {
		sb.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	src := sb.String()
	units := len(utf16.Encode([]rune(src)))
	var blocks []Block
	for n := r.Intn(8); n > 0; n-- {
		a, b := r.Intn(units), r.Intn(units)
		if a > b {
			a, b = b, a
		}
		blocks = append(blocks, Block{StartOffset: a, EndOffset: b, HasExecuted: r.Intn(2) == 0, ExecutionCount: r.Intn(3)})
	}
	return reflect.ValueOf(scriptCase{src, blocks})
}

// Two properties of the mapping: the order of the blocks does not matter,
// and a line "é😀" in front of the script (four UTF-16 code units with its
// line break) moves every record one line down when the offsets move by
// four.
func TestLinesProperties(t *testing.T) {
	prop := func(c scriptCase) bool {
		base, err := Lines(c.Source, c.Blocks)
		if err != nil {
			return false
		}
		rev := make([]Block, len(c.Blocks))
		for i, b := range c.Blocks {
			rev[len(c.Blocks)-1-i] = b
		}
		if again, err := Lines(c.Source, rev); err != nil || !reflect.DeepEqual(again, base) {
			return false
		}
		shifted := make([]Block, len(c.Blocks))
		for i, b := range c.Blocks {
			b.StartOffset += 4
			b.EndOffset += 4
			shifted[i] = b
		}
		moved, err := Lines("é😀\n"+c.Source, shifted)
		if err != nil {
			return false
		}
		want := map[int]int{}
		for line, n := range base {
			want[line+1] = n
		}
		return reflect.DeepEqual(moved, want)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
