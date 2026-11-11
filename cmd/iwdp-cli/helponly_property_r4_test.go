package main

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	reg "github.com/nnemirovsky/iwdp-cli/internal/commands"
)

// helpWords are the help requests of criterion 3a'.
var helpWords = []string{"help", "--help", "-h"}

// otherWords are tokens that are not help requests: unknown words, flags
// (known, unknown, with values, single-dash) and, added in init, the
// words of every registry command name.
var otherWords = []string{"bogus", "x1", "Help", "HELP", "--json", "--bogus", "--timeout=1s", "-x", "-1", "--", "help=1", ""}

func init() {
	for _, c := range reg.All() {
		for _, w := range strings.Fields(c.Name) {
			if !slices.Contains(otherWords, w) {
				otherWords = append(otherWords, w)
			}
		}
	}
}

// helpLine is a generated command line: up to 6 tokens, often only help
// words, else help words mixed with otherWords.
type helpLine []string

func (helpLine) Generate(r *rand.Rand, _ int) reflect.Value {
	n := r.Intn(7)
	onlyHelp := r.Intn(2) == 0
	line := make(helpLine, n)
	for i := range line {
		if onlyHelp || r.Intn(2) == 0 {
			line[i] = helpWords[r.Intn(len(helpWords))]
		} else {
			line[i] = otherWords[r.Intn(len(otherWords))]
		}
	}
	return reflect.ValueOf(line)
}

// classifyBefore is main's classification before criterion 3a' (tree
// 9ed887fc, cmd/iwdp-cli/main.go): only a single help word was the
// overview; "help" followed by words without a flag was a command's help,
// "help" followed by a flag became "--help" for the registry.
func classifyBefore(args []string) (lineKind, []string) {
	flag := false
	if len(args) > 0 {
		for _, a := range args[1:] {
			flag = flag || strings.HasPrefix(a, "-")
		}
	}
	switch {
	case len(args) == 0:
		return lineEmpty, nil
	case len(args) == 1 && slices.Contains(helpWords, args[0]):
		return lineOverview, nil
	case args[0] == "help" && !flag:
		return lineCommandHelp, args[1:]
	case args[0] == "help":
		return lineRegistry, append([]string{"--help"}, args[1:]...)
	}
	return lineRegistry, args
}

// Property (criterion 3a'; round-4 review part 1 F12): a line is the
// overview exactly when it is nonempty and every token is help, --help or
// -h; every other line keeps the classification it had before.
func TestOnlyHelpWordsLinesProperty(t *testing.T) {
	sawOverview, sawMixed := 0, 0
	prop := func(line helpLine) bool {
		args := []string(line)
		kind, rest := classifyLine(slices.Clone(args))
		allHelp := len(args) > 0
		for _, a := range args {
			allHelp = allHelp && slices.Contains(helpWords, a)
		}
		if allHelp {
			sawOverview++
			if kind != lineOverview {
				t.Logf("%q: kind %d; want the overview", args, kind)
				return false
			}
			return true
		}
		sawMixed++
		wantKind, wantRest := classifyBefore(slices.Clone(args))
		if kind != wantKind || !slices.Equal(rest, wantRest) {
			t.Logf("%q: kind %d %q; want the earlier kind %d %q", args, kind, rest, wantKind, wantRest)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
	if sawOverview == 0 || sawMixed == 0 {
		t.Fatalf("generated %d help-only and %d other lines; want both", sawOverview, sawMixed)
	}
}

// The overview kind prints the overview of "--help" with exit 0, for
// generated help-only lines run as iwdp-cli (a sample: each run is a
// process).
func TestGeneratedHelpOnlyLinesPrintTheOverview(t *testing.T) {
	overview := runCLI(t, "", nil, "--help")
	prop := func(line helpLine) bool {
		args := []string(line)
		if len(args) == 0 {
			return true
		}
		for i, a := range args {
			if !slices.Contains(helpWords, a) {
				args[i] = "help"
			}
		}
		r := runCLI(t, "", nil, args...)
		if r.code != 0 || r.stderr != "" || r.stdout != overview.stdout {
			t.Logf("%q: exit %d, stderr %q, stdout %.60q; want the overview of --help", args, r.code, r.stderr, r.stdout)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}
