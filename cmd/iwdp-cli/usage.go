package main

import (
	"fmt"
	"io"
	"strings"

	reg "github.com/nnemirovsky/iwdp-cli/internal/commands"
)

// synopsisWidth aligns command summaries in the overview.
const synopsisWidth = 24

const globalOptionsHelp = `Global options (any position before "--"):
  --timeout <d>   Deadline for the whole command: a Go duration (10s, 2m) or whole
                  seconds (10). Default 30s, or IWDP_TIMEOUT. On expiry the command
                  exits 124 and names the page, the elapsed time and the likely causes.
  -h, --help      Show help and exit 0. Never connects to a page or writes files.
                  An unknown option or command still exits 2.
  --page <p>      The page: a tab id from 'tab list' or a ws:// URL. Default: the
                  session's tab, else the first debuggable page.
  --session <s>   Session that holds the page lease (IWDP_SESSION; default "default").
  --reservation <owner>  Device reservation owner (IWDP_DEVICE_RESERVATION; default
                  the session). Pages of a physical device need one: 'device reserve'.
  --wait-lease <d>  Wait this long for another session's lease (inside the deadline);
                  a duration or whole seconds, as --timeout.
  --takeover      Reattach a page whose connection someone else took (needs the
                  reservation and the device owner's approval).
  --transport <t> auto (default), iwdp, direct or webkitgtk (IWDP_TRANSPORT).
  --json          Print the result as JSON.
  --max-output <n>  Cut text output after n bytes.
  --content-boundaries  Mark page content in the output.
Boolean options are true as --flag; --flag=<v> takes the values of Go's
strconv.ParseBool (true, false, 1, 0, t, f, ...), so --takeover=false is false.
Commands run in the iwdp daemon, started on demand ('iwdp-cli daemon --help').
Exit codes: 0 ok, 1 error, 2 usage, 4 page busy or lost, 5 no device or page,
6 device reservation missing, 124 deadline.`

const footerHelp = `Port assignment:
  ios_webkit_debug_proxy listens on port 9221 for device listing.
  Each connected device gets an incremented port starting at 9224:
    Device 1 -> localhost:9224
    Extra devices and the SIMULATOR ghost entry -> localhost:9225+

Environment:
  IWDP_TIMEOUT   Default command deadline: a duration or whole seconds (default: 30s)
  IWDP_SESSION, IWDP_DEVICE_RESERVATION, IWDP_TRANSPORT  Defaults of the options above
  IWDP_WS_URL    No longer selects a page: a page command with it set and no
                 --page fails with exit 2. Name the page with --page.`

// printUsage writes the overview of all commands.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "iwdp-cli — iOS Safari debugging CLI")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usage: iwdp-cli <command> [options]")
	_, _ = fmt.Fprintln(w, "       iwdp-cli <command> --help")
	_, _ = fmt.Fprintln(w, "       iwdp-cli <group> --help   (lists a group's commands, for example 'network')")
	_, _ = fmt.Fprintln(w, "\nCommands:")
	for _, c := range reg.All() {
		_, _ = fmt.Fprintf(w, "  %-*s %s\n", synopsisWidth, c.Name, c.Summary)
	}
	_, _ = fmt.Fprintf(w, "\n%s\n\n%s\n", globalOptionsHelp, footerHelp)
}

// printCommandHelp writes the help of the registry command name (its
// command words, for example "network requests"), or the command list of
// the group that name names (for example "network"). It reports false for
// an unknown command.
func printCommandHelp(w io.Writer, name string) bool {
	words := strings.Fields(name)
	c, n := reg.Lookup(words)
	if c == nil || n != len(words) {
		if list, ok := reg.GroupHelp(words); ok && c == nil {
			_, _ = fmt.Fprint(w, list)
			return true
		}
		return false
	}
	_, _ = fmt.Fprint(w, reg.Help(c))
	return true
}
