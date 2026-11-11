package main

import (
	"fmt"
	"os"
	"strings"

	reg "github.com/nnemirovsky/iwdp-cli/internal/commands"
)

// main runs one registry command (internal/commands): the command
// registry parses the line, prints its help and runs it, a Local command
// here and every other one in the daemon (runRegistry). Only these lines
// are handled here: a line of only "help", "--help" and "-h", in any
// number and order, prints the overview with the global options (exit 0);
// "help <command or group words>" prints that
// help; an empty line prints the overview as a usage error (exit 2). The
// registry parses every other line, also "help" followed by a flag, so an
// unknown option or command exits 2 also with help.
func main() {
	kind, args := classifyLine(os.Args[1:])
	switch kind {
	case lineEmpty:
		printUsage(os.Stderr)
		os.Exit(reg.ExitUsage)
	case lineOverview:
		printUsage(os.Stdout)
		return
	case lineCommandHelp:
		name := strings.Join(args, " ")
		if !printCommandHelp(os.Stdout, name) {
			fmt.Fprintf(os.Stderr, "Error: unknown command %q (run 'iwdp-cli --help')\n", name)
			os.Exit(reg.ExitUsage)
		}
		return
	}
	code := runRegistry(args)
	exits.finish(func() int { return code })
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
