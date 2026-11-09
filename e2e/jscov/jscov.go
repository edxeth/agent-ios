// Package jscov converts the basic blocks of JavaScriptCore's control-flow
// profiler (WebKit inspector protocol: Runtime.enableControlFlowProfiler,
// then Runtime.getBasicBlocks for a script) into LCOV line records, so the
// page scripts that the endpoint injects get line coverage from the engine
// that runs them. e2e/webkitgtk's checkVisibility oracle uses it
// (docs/notes/w5.md, "JavaScript coverage of the page shims").
//
// The block format is JavaScriptCore's (Source/JavaScriptCore: CodeBlock,
// BasicBlockLocation, InspectorRuntimeAgent::getBasicBlocks): offsets are
// UTF-16 code units from the start of the script text, both ends are
// inclusive, and the blocks overlap: a function has one range over its
// whole text, and the blocks inside it have their own smaller ranges. A
// function that never ran is one unexecuted range.
package jscov

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Block is one basic block of Runtime.getBasicBlocks.
type Block struct {
	StartOffset    int  `json:"startOffset"`
	EndOffset      int  `json:"endOffset"`
	HasExecuted    bool `json:"hasExecuted"`
	ExecutionCount int  `json:"executionCount"`
}

// sourceURLScheme starts the sourceURL that maps an evaluated script to a
// file of the repository.
const sourceURLScheme = "iwdp-jscov:///"

// SourceURL returns the sourceURL for file (a path relative to the
// repository root); a script that ends with "//# sourceURL=" and this
// value is reported under it in Debugger.scriptParsed.
func SourceURL(file string) string { return sourceURLScheme + file }

// File returns the repository file of a sourceURL made by SourceURL.
func File(sourceURL string) (string, bool) {
	file, ok := strings.CutPrefix(sourceURL, sourceURLScheme)
	return file, ok && file != ""
}

// closingBrace matches the text of a block that holds only a closing
// brace: the unreachable tail after a return or throw. WebKit's Web
// Inspector does not mark such an unexecuted block either
// (BasicBlockAnnotator._isTextRangeOnlyClosingBrace).
var closingBrace = regexp.MustCompile(`^\s*\}$`)

// Lines returns the hit count of each line of source (1-based) that the
// blocks cover. A non-space character belongs to the smallest block that
// contains it (on equal ranges an executed block wins, then the larger
// count); a line counts when
// one of its characters belongs to a block, and its count is the largest
// count of those blocks (at least 1 for an executed block, 0 for an
// unexecuted one). Lines without such a character have no record.
func Lines(source string, blocks []Block) (map[int]int, error) {
	units := utf16.Encode([]rune(source))
	var kept []Block
	for _, b := range blocks {
		if b.StartOffset < 0 || b.EndOffset < b.StartOffset || b.EndOffset >= len(units) {
			return nil, fmt.Errorf("basic block [%d, %d] is outside the source of %d UTF-16 code units", b.StartOffset, b.EndOffset, len(units))
		}
		if !b.HasExecuted && closingBrace.MatchString(string(utf16.Decode(units[b.StartOffset:b.EndOffset+1]))) {
			continue
		}
		kept = append(kept, b)
	}
	lines := map[int]int{}
	line := 1
	for i, u := range units {
		if u == '\n' {
			line++
			continue
		}
		if u < 0x80 && unicode.IsSpace(rune(u)) {
			continue
		}
		in, ok := innermost(kept, i)
		if !ok {
			continue
		}
		count := 0
		if in.HasExecuted {
			count = max(in.ExecutionCount, 1)
		}
		if c, seen := lines[line]; !seen || count > c {
			lines[line] = count
		}
	}
	return lines, nil
}

// innermost returns the smallest block that contains offset; on equal
// ranges an executed block wins, then the larger count, so the order of
// the blocks does not matter.
func innermost(blocks []Block, offset int) (Block, bool) {
	var best Block
	found := false
	for _, b := range blocks {
		if offset < b.StartOffset || offset > b.EndOffset {
			continue
		}
		size, bestSize := b.EndOffset-b.StartOffset, best.EndOffset-best.StartOffset
		if !found || size < bestSize || size == bestSize && better(b, best) {
			best, found = b, true
		}
	}
	return best, found
}

// better orders two blocks of the same range: executed first, then the
// larger count.
func better(a, b Block) bool {
	if a.HasExecuted != b.HasExecuted {
		return a.HasExecuted
	}
	return a.ExecutionCount > b.ExecutionCount
}

// Add adds the counts of from to into, line by line (the LCOV merge of
// two runs of the same file).
func Add(into, from map[int]int) {
	for line, n := range from {
		into[line] += n
	}
}

// WriteLCOV writes one LCOV record for file with the line counts.
func WriteLCOV(w io.Writer, file string, lines map[int]int) error {
	nums := make([]int, 0, len(lines))
	for n := range lines {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var b strings.Builder
	fmt.Fprintf(&b, "TN:\nSF:%s\n", file)
	hit := 0
	for _, n := range nums {
		fmt.Fprintf(&b, "DA:%d,%d\n", n, lines[n])
		if lines[n] > 0 {
			hit++
		}
	}
	fmt.Fprintf(&b, "LF:%d\nLH:%d\nend_of_record\n", len(nums), hit)
	_, err := io.WriteString(w, b.String())
	return err
}
