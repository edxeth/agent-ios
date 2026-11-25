package main

import (
	"fmt"
	"io"
	"strings"

	reg "github.com/edxeth/agent-ios/internal/commands"
)

// synopsisWidth aligns command summaries in the overview.
const synopsisWidth = 24

const globalOptionsHelp = `Global options (any position before "--"):
  --timeout <d>   Deadline for the whole command: a Go duration (10s, 2m) or whole
                  seconds (10). Default 30s, or AGENT_IOS_TIMEOUT. On expiry the command
                  exits 124 and names the page, the elapsed time and the likely causes.
  -h, --help      Show help and exit 0. Never connects to a page or writes files.
                  An unknown option or command still exits 2.
  --page <p>      The page: a tab id from 'tab list' or a ws:// URL. Default: the
                  session's tab, else the first debuggable page.
  --only-tab      The page is the only tab of the listing; exit 5 unless the listing
                  shows exactly one tab and no hidden page. Not with --page or a page argument.
  --session <s>   Session that holds the page lease (AGENT_IOS_SESSION; default "default").
  --reservation <owner>  Device reservation owner (AGENT_IOS_DEVICE_RESERVATION; default
                  the session). Pages of a physical device need one: 'device reserve'.
  --wait-lease <d>  Wait this long for another session's lease (inside the deadline);
                  a duration or whole seconds, as --timeout.
  --takeover      Reattach a page whose connection someone else took (needs the
                  reservation and the device owner's approval).
  --transport <t> auto (default), iwdp, direct or webkitgtk (AGENT_IOS_TRANSPORT).
  --json          Print the result as JSON.
  --max-output <n>  Cut text output after n bytes.
  --content-boundaries  Mark page content in the output.
Boolean options are true as --flag; --flag=<v> takes the values of Go's
strconv.ParseBool (true, false, 1, 0, t, f, ...), so --takeover=false is false.
Commands run in the agent-ios daemon, started on demand ('agent-ios daemon --help').
Exit codes: 0 ok, 1 error, 2 usage, 4 page busy or lost, 5 no device or page,
6 device reservation missing, 124 deadline.`

const footerHelp = `Port assignment:
  ios_webkit_debug_proxy listens on port 9221 for device listing.
  Each connected device gets an incremented port starting at 9224:
    Device 1 -> localhost:9224
    Extra devices and the SIMULATOR ghost entry -> localhost:9225+

Environment:
  AGENT_IOS_TIMEOUT   Default command deadline: a duration or whole seconds (default: 30s)
  AGENT_IOS_SESSION, AGENT_IOS_DEVICE_RESERVATION, AGENT_IOS_TRANSPORT  Defaults of the options above
  AGENT_IOS_WS_URL    No longer selects a page: a page command with it set and no
                 --page fails with exit 2. Name the page with --page.`

// printUsage writes the overview of all commands on w and returns the
// error of the write.
func printUsage(w io.Writer) error {
	var b strings.Builder
	b.WriteString("agent-ios — iOS Safari debugging CLI\n\n")
	b.WriteString("Usage: agent-ios <command> [options]\n")
	b.WriteString("       agent-ios <command> --help\n")
	b.WriteString("       agent-ios <group> --help   (lists a group's commands, for example 'network')\n")
	b.WriteString("\nCommands:\n")
	for _, c := range reg.All() {
		fmt.Fprintf(&b, "  %-*s %s\n", synopsisWidth, c.Name, c.Summary)
	}
	fmt.Fprintf(&b, "\n%s\n\n%s\n", globalOptionsHelp, footerHelp)
	_, err := io.WriteString(w, b.String())
	return err
}

// printCommandHelp writes the help of the registry command name (its
// command words, for example "network requests"), or the command list of
// the group that name names (for example "network"), and returns the
// error of the write. It reports known false, and writes nothing, for an
// unknown command.
func printCommandHelp(w io.Writer, name string) (known bool, err error) {
	words := strings.Fields(name)
	c, n := reg.Lookup(words)
	if c == nil || n != len(words) {
		if list, ok := reg.GroupHelp(words); ok && c == nil {
			_, err := io.WriteString(w, list)
			return true, err
		}
		return false, nil
	}
	_, err = io.WriteString(w, reg.Help(c))
	return true, err
}
