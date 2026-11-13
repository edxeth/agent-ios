package main

import (
	"fmt"
	"os"
	"strings"

	reg "github.com/nnemirovsky/iwdp-cli/internal/commands"
)

// main runs one registry command (internal/commands): the command
// registry parses the line, prints its help and runs it, a Local command
// here and every other one in the daemon (runRegistry). Every line ends
// through exits.finish, which prints the end of the run on stderr and
// exits (in private mode exactly one status line).
//
// Private mode is decided first (commands.PrivateRequested), and what it
// needs is installed before any output or work (enterPrivate), so that
// every path of the run, also a help text that blocks on stdout, ends
// with its status line.
func main() {
	args := os.Args[1:]
	private := reg.PrivateRequested(args)
	if private {
		enterPrivate()
	}
	exits.finish(runLine(args, private))
}

// runLine runs the command line args and returns how it ends. Only these
// lines are handled here: a line of only "help", "--help" and "-h", in
// any number and order, prints the overview with the global options (exit
// 0); "help <command or group words>" prints that help; an empty line
// prints the overview as a usage error (exit 2). The registry parses every
// other line, also "help" followed by a flag, so an unknown option or
// command exits 2 also with help. private is the mode of the run
// (commands.PrivateRequested). A help text that cannot be written on
// stdout ends the run with exit 1 (commands.Outcome.WriteFailed).
func runLine(args []string, private bool) reg.Outcome {
	kind, words := classifyLine(args)
	switch kind {
	case lineEmpty:
		var usage strings.Builder
		_ = printUsage(&usage) // a strings.Builder never fails
		return reg.Outcome{Code: reg.ExitUsage, Private: private, Message: strings.TrimSuffix(usage.String(), "\n")}
	case lineOverview:
		o := reg.Outcome{Private: private}
		if err := printUsage(os.Stdout); err != nil {
			return o.WriteFailed("help", err)
		}
		return o
	case lineCommandHelp:
		name := strings.Join(words, " ")
		known, err := printCommandHelp(os.Stdout, name)
		if !known {
			return reg.Outcome{Code: reg.ExitUsage, Private: private, Message: fmt.Sprintf("Error: unknown command %q (run 'iwdp-cli --help')", name)}
		}
		// The words name a command or a group of the registry; the
		// status line names them as one line.
		o := reg.Outcome{Command: strings.Join(strings.Fields(name), " "), Private: private}
		if err != nil {
			return o.WriteFailed("help", err)
		}
		return o
	}
	return runRegistry(words)
}

// lineKind is how main handles a command line (classifyLine).
type lineKind int

const (
	lineEmpty       lineKind = iota // no arguments: the overview on stderr, exit 2
	lineOverview                    // only help words: the overview, exit 0
	lineCommandHelp                 // "help <words>" without a flag: the help of those words
	lineRegistry                    // the registry parses the arguments
)

// classifyLine returns how main handles args, and the arguments of that
// handling: the words of lineCommandHelp, the registry's arguments of
// lineRegistry ("help" followed by a flag becomes "--help" and the rest).
func classifyLine(args []string) (lineKind, []string) {
	switch {
	case len(args) == 0:
		return lineEmpty, nil
	case onlyHelpWords(args):
		return lineOverview, nil
	case args[0] == "help" && !anyFlag(args[1:]):
		return lineCommandHelp, args[1:]
	case args[0] == "help":
		return lineRegistry, append([]string{"--help"}, args[1:]...)
	}
	return lineRegistry, args
}

// onlyHelpWords reports whether every argument is "help", "--help" or "-h".
func onlyHelpWords(args []string) bool {
	for _, a := range args {
		if a != "help" && a != "--help" && a != "-h" {
			return false
		}
	}
	return true
}

// anyFlag reports whether an argument starts with "-".
func anyFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}
