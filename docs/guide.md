# User guide

This guide explains how iwdp-cli works and how an agent uses it. The [command reference](commands.md) lists every registry command with its flags.

Terms used on every page:

- **WIP**: the WebKit Inspector Protocol. Safari on iOS, ios_webkit_debug_proxy (iwdp) and WebKitGTK speak it.
- **CDP**: the Chrome DevTools Protocol.
- **Page connection**: one inspector connection to one Safari tab. Safari allows one connection per tab; a new connection takes the tab from the old one.
- **Daemon**: the background process that owns every page connection.
- **Session**: a name that a client chooses (`--session`, `IWDP_SESSION`, default `default`). Leases, refs and dialog answers belong to a session.
- **Lease**: the exclusive right of one session to drive one page. See [sessions.md](sessions.md).
- **Ref**: an element handle such as `@e7` that `snapshot` prints and later commands accept.
- **Runtime**: the JavaScript that iwdp-cli injects into the page to take snapshots, check and perform actions, wait, and answer dialogs.

## The daemon

One daemon per user owns every page connection. `iwdp-cli` commands and CDP clients are clients of the daemon, so they share leases, refs and page state, and two of them never take a page from each other.

- A client starts the daemon on demand (`<own executable> daemon run`). The daemon is part of the `iwdp-cli` binary; there is no separate daemon binary.
- Socket: `$XDG_RUNTIME_DIR/iwdp/daemon.sock`, else `$TMPDIR/iwdp-$UID/daemon.sock`. The directory must be a real directory owned by the user with mode 0700; otherwise the daemon and the clients refuse to start.
- Log: `$XDG_STATE_HOME/iwdp/daemon.log` (fallback `~/.local/state/iwdp/daemon.log`). `iwdp-cli daemon logs` prints its end.
- `iwdp-cli daemon start|stop|status|logs|run`. `daemon stop`, SIGTERM and the idle exit clean up every page first (5 s at most).
- `daemon stop` (and the idle exit) leaves running an iwdp that the daemon started (decision D10): the daemon never stops an iwdp, because other clients and agents may use it. One exception, on Linux only: when the iwdp that iwdp-cli started and recorded lists nothing and holds no connection to usbmuxd (no Unix-domain socket), while usbmuxd's socket accepts a connection, the daemon restarts that iwdp before it lists pages; it checks this under the start lock right before the stop. ios_webkit_debug_proxy 1.9.2 subscribes to usbmuxd once, at its start, and keeps that connection while it runs; when usbmuxd exits (on Linux, usbmuxd usually exits when the last device is unplugged and starts again on the next plug-in), the iwdp never subscribes again and never sees a device again. Its device connections went through usbmuxd and ended with it, so the restart ends no connection to a device page. The new iwdp holds its usbmuxd connection, so it is not restarted again. A second case: the recorded iwdp lists nothing although it holds its usbmuxd connection and usbmuxd reports a USB device (`idevice_id -l`). iwdp 1.9.2 ends its whole connection to a device when it fails to pass one inspector message to a WebSocket client that is closing or gone, and connects to a device only when usbmuxd reports it attached, so it never lists that device again (device check of 2026-11-12). The daemon restarts such an iwdp only when it started 30 s or more ago (the age of its record): a device can refuse the new connection for a while after it lost one, and a locked device or one with Web Inspector off never accepts it, so commands restart it at most once per 30 s. A failed start of iwdp, also of such a restart, blocks further starts of the daemon for 30 s, and while that back-off holds the daemon stops no iwdp for a restart. Without `IWDP_NO_DAEMON=1`, the daemon is the only process that starts iwdp by itself: `devices` runs in the daemon too; with it, each command process does. The back-off lives in the daemon's memory; the cases where it does not hold are in [known-limits.md](known-limits.md). When the restarted iwdp lists no device within 3 s (a device that is locked, waits for Trust, or has Web Inspector off, or that still refuses after the loss), the command gets an error with what to do on the device. Another program's iwdp in either state is never restarted: the error names it as `restart-iwdp` does. `iwdp-cli devices` starts and recovers iwdp the same way, and exits 5 with that advice when the listing is empty while usbmuxd reports a device. `iwdp-cli status` shows that iwdp runs and names its log. Its record (pid, process group, listing address) is the file `iwdp-cli-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-cli/` (else `$TMPDIR/iwdp-cli-<uid>/`). `iwdp-cli restart-iwdp` restarts it after it proves that the process is the recorded one. Stop it by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>`, which sends SIGTERM only when the identity check reports `verified`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as [Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand) in device-runbook.md describes.
- Idle exit: after 15 minutes without requests, leases, captures and CDP clients (`IWDP_DAEMON_IDLE` or `--idle` changes it). A page connection without a lease and without a capture closes after 10 minutes, so that other tools (for example Safari Web Inspector on a Mac) can take the page.
- `IWDP_NO_DAEMON=1` runs the same core inside one command process, for debugging. It gives no leases across processes, and a CDP endpoint started this way ends with the process. The command's deadline covers the cleanup of the page state at the end too: the cleanup gets what is left of the deadline, and the process ends at most 200 ms after the deadline. When the page does not answer a reset in time (for example `Console.disable`), a warning on stderr names it, the page connection closes, and the exit status is still the command's own. Closing the connection is expected to end the inspector session; whether iwdp passes the close on to WebKit as a frontend disconnect was not checked. When the last inspector frontend disconnects, current WebKit disables the Console, Network and Page agents and resets the Page overrides; this was not checked in the WebKit of an iOS release. Changes made to the document itself stay until the page reloads.
- A client uses a running daemon only when the daemon has the same protocol version and runs the same binary content. The daemon records its executable file when it starts (device, inode, size, modification time and the SHA-256 of its bytes). A client compares that with its own executable file, and hashes its own bytes only when the file differs. So identical copies of one binary share a daemon (for example the outputs of `make build` and `make install`), and an upgraded or rebuilt `iwdp-cli` never sends a command to the daemon of an older binary.
  - When that daemon is idle, the client asks it to exit and starts its own.
  - When it holds leases, it keeps running: the client fails and tells you to run `iwdp-cli daemon stop` when they finish. `daemon stop` and `daemon status` still reach it.
  - This holds also while another client starts a daemon: a client that waited for the start finds the other binary's daemon and applies the same rule.
  - `daemon status` and `daemon stop` also reach the daemon of the previous protocol (protocol 1, the release before the registry-only `iwdp-cli`), idle or busy. `daemon status` reports it, says that it is older, and leaves it running; `daemon stop` stops it after its cleanup. A daemon of a newer protocol is not touched: the command says so, and the newer `iwdp-cli` that started it can stop it.

The daemon may start iwdp when it is not running; `iwdp-cli status` only checks and starts nothing. The daemon never stops or restarts an iwdp that it did not start. See [device-runbook.md](device-runbook.md).

- An iwdp that iwdp-cli starts (the daemon or `restart-iwdp`) appends its stdout and stderr to `$XDG_STATE_HOME/iwdp-cli/iwdp.log` (fallback `~/.local/state/iwdp-cli/iwdp.log`, mode 0600). `iwdp-cli status` names the path (or why it is unknown) for every outcome, also when the listing port is owned but does not answer. A start that finds the log at 1 MiB or more keeps it as `iwdp.log.1` (replacing an older one) and begins a new one. iwdp's stdout is block-buffered in a file, so its "Connected ..." lines appear only when it exits; when GNU `stdbuf` is on `PATH` (Linux), iwdp runs under `stdbuf -oL` and they appear at once. stderr is unbuffered either way.
- iwdp lists a device's pages a moment after it starts. After the daemon started iwdp, it waits up to 3 s (within the command's deadline) until a device lists a page; a list that stays empty is reported as before ("No tabs.").
- The daemon log holds no page-supplied text: no page title, no page URL, no message or exception of a page, and no text of an error.
  - **What it keeps:** the operation, in the fixed text of each log line; ids: the page id (the transport's page id, for iwdp its WebSocket debugger URL without query; a CDP target id), target, request and session ids, the session name, the device label; numbers, durations and connection states; the configured addresses (the daemon socket, the CDP listen address); the method names of a CDP client's requests and hashes of the other values it sent (Host, Origin, path segments).
  - **What it keeps of an error:** only what the error's type says. That is the name of a known error (for example `webkit.ErrPageClosed`, `context.DeadlineExceeded`, `core.ErrLost`), the method of the request that failed (`Console.disable`), a WebKit protocol error code (`WebKit protocol error -32000`), a runtime or ref error code that iwdp-cli defines (`runtime error stale_owner`), a WebSocket close code, a system error (`errno 111 (connection refused)`), the operation of a network error (`net dial`), a JSON error's offset or Go type, and `Go runtime error`. Several errors are listed as `[a; b]`, an error's added cause as `(also: ...)`.
  - **What it withholds:** the text of every error. An error that iwdp-cli cannot classify by its type is written as `unclassified` (with its Go type for a typed error); a runtime error code that iwdp-cli does not define is left out; the address of a network error is left out; a recovered panic value is written like an error when it is one, else by its type only, with the stack.

## Deadlines and exit codes

Every command runs under one deadline: 30 s by default, `--timeout <duration>` or `IWDP_TIMEOUT`. The deadline counts from the start of the command, after the program has started. Some commands have their own default (actions 5 s, `wait` 25 s; the reference lists them). The deadline covers the read of the standard input (`fill --stdin`), the start of the daemon, the wait for a lease, the connection, the calls and any retry. The client sends the deadline itself to the daemon: the daemon starts no page work after it, also when the request reaches it late. When the deadline expires, the command returns at once; the client waits at most 200 ms more for the daemon's answer. A result that the daemon finished before the deadline counts as success, so the command can end up to 200 ms after its deadline; the daemon's own deadline error arrives the same way. The deadline error (exit 124) names the command, its page (the `--page` value, else the page that the daemon resolved, also while the command still waits for the lease or the page's queue), the elapsed time, the last wait (what the command or the daemon waited for) and the likely causes, whichever step the deadline ended.

| Exit code | Meaning |
|---|---|
| 0 | ok |
| 1 | command error |
| 2 | usage error of a registry command (found before any connection), also a bad global option such as `--timeout bogus` |
| 3 | a wait ended without its condition (the result names the last observed state) |
| 4 | page busy, stalled or lost |
| 5 | no device or page |
| 6 | device reservation missing; for `tab find`: the listing hides pages |
| 7 | `tab find`: no tab shows the target |
| 124 | the command deadline expired |
| 130 | ended by SIGINT or SIGTERM |

The table holds for every command (the commands in the [command reference](commands.md)). An unknown command or `--flag`, a trailing `ws://` argument and a page command with `IWDP_WS_URL` set and no `--page` are usage errors (exit 2).

`iwdp-cli --help`, `iwdp-cli -h` and `iwdp-cli help` alone print the overview (exit 0); help that cannot be written on stdout exits 1 with "Error: writing the help: ...". `<command> --help` (or `help <command>`) prints the help of a command or group and never connects to a page; it needs none of the command's arguments (`iwdp-cli click --help`), but it checks the values of the options given with it, so `iwdp-cli cookies set --same-site=bogus --help` exits 2. Help never hides a usage error: an unknown `--flag` exits 2 also with `--help` or `-h` (`iwdp-cli click body --bogus --help`, `iwdp-cli network --bogus --help`, `iwdp-cli --bogus --help`), and so does an unknown command (`iwdp-cli not-a-command --help`, `iwdp-cli help not-a-command`) and a line with options but no command (`iwdp-cli --json --help`). A group word, the first words of several commands but no command itself (for example `network`, `cdp`, `daemon`, `network har`), lists the group's commands with their one-line descriptions: `iwdp-cli network --help` prints the list (exit 0), and `iwdp-cli network` alone, or with other flags, prints it as a usage error (exit 2). A word that is a command and a group (`tab`) is the command.

Boolean options, global (`--json`, `--content-boundaries`, `--takeover`, `--private`) and of a command (for example `--force`), are true as `--flag`. `--flag=<value>` takes exactly the values of Go's `strconv.ParseBool`: `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`, `F`, `FALSE`, `false`, `False`. So `--takeover=false` never takes over a page. Any other value (`--takeover=yes`) is a usage error (exit 2) that names the option and the value, before any connection. The same holds for the values that a command checks itself, for example `cookies set --same-site` (Lax, Strict or None, in any case), `cookies set --expires` and `trace start --categories`. `--expires` in Unix seconds must be within ±8640000000000 (the range of a JavaScript Date, ±8.64e15 ms); a larger value is a usage error.

Durations of the global options `--timeout`, `IWDP_TIMEOUT` and `--wait-lease` are a Go duration or a bare whole number of seconds: `5s` and `5` both mean five seconds, `1m30s` and `90` both mean ninety seconds.

Arguments and flags of registry commands: only the exact flags of the command (its reference entry) and the global flags are flags. Another token that starts with `--` is a usage error (exit 2). Every other token is an argument, also one with a single leading dash that is not a short flag of the command: `eval -1`, `eval '-Math.abs(2)'` and `fill '#amount' -5` work (quote what the shell would change). `--` ends the flags: `eval -- --x` evaluates `--x`.

## The agentic loop

An agent works in a loop: look, act, wait, check.

1. **Prepare.** Pick a stable session name. On a physical device, reserve the device for the run (see [sessions.md](sessions.md)).

   ```bash
   export IWDP_SESSION=checkout-run-1
   iwdp-cli device reserve --owner checkout-run-1 --ttl 2h
   ```

2. **Navigate.** `open` sets `location.href` and waits for the load state of the new document. `back`, `forward` and `reload` wait the same way.

   ```bash
   iwdp-cli open https://shop.example/cart
   ```

3. **Look.** `snapshot` prints the accessibility tree with refs, in agent-browser's format. `-i` keeps only elements with refs.

   ```text
   $ iwdp-cli snapshot -i
   - button "Sign In" [ref=e6]
   - textbox "Email" [required, ref=e10]
   - checkbox "Remember me" [checked=false, ref=e12]
   ```

   Read single values with `get` (url, title, text, html, value, attr, count, box, styles), test states with `is` (visible, enabled, checked, editable, focused), find elements with `find`, read the element at a point with `hit-test`, and read the visible text with `read`. `screenshot --annotate` draws the refs on the screenshot.

4. **Act.** Actions take a ref, a CSS selector, or a locator (`text=Save`, `role=button[name="Save"]`, `label=Email`, `placeholder=...`, `testid=...`). A CSS selector that matches more than one element fails with the count.

   ```bash
   iwdp-cli fill @e10 user@example.com
   iwdp-cli click @e6
   ```

   Before an action on an element, the runtime runs the actionability checks of that action. The checks differ per action:

   | Check | Meaning | Actions |
   |---|---|---|
   | attached, visible | the element is in the document and visible | every action on an element |
   | stable | the element has the same box in two animation frames | click (also `--new-tab`), dblclick, tap, hover, check, uncheck, drag |
   | enabled | not disabled (also not through a disabled fieldset), and neither the element nor an ancestor has `aria-disabled="true"` or `inert` | click (also `--new-tab`), dblclick, tap, check, uncheck, select, fill, type, upload |
   | editable | a text input, a textarea or contenteditable, not read-only | fill, type |
   | receives events | the element at its center point is the element or a descendant | click (also `--new-tab`), dblclick, tap, hover, check, uncheck, drag |

   - fill and type check editability, but not stability or receives events. They can therefore change an element that another element covers.
   - hover and drag do not check that the element is enabled. drag checks its source element only, not the drop target.
   - check and uncheck run the checks on the `<label>` of a hidden input.

   The runtime repeats the checks until the action deadline (5 s). Only the actions with the receives-events check fail on a covered element, with `covered by <div class="modal">`. `--force` skips the checks. All input is synthetic; read [synthetic-input.md](synthetic-input.md).

5. **Wait.** `wait` blocks until a condition holds: milliseconds, an element state (`--state visible|hidden|attached|detached`), `--text`, `--url <glob>`, `--load domcontentloaded|load|networkidle`, or `--fn <js>`. It ends with exit 3 and the last state when the condition does not hold in time (default 25 s).

   ```bash
   iwdp-cli wait --url '**/order/*'
   ```

6. **Check and repeat.** Take a new snapshot after every navigation or large page change. `diff snapshot` shows what changed between the last two snapshots of the session.

7. **End.** `iwdp-cli close` ends the session: it releases the session's leases and removes the state it set on the page (overrides, interception, breakpoints, dialog overrides). The Safari tab stays open. Release the device reservation with `iwdp-cli device release --owner checkout-run-1`.

### Refs

- A snapshot gives new elements refs `e1, e2, ...`. Ref numbers are never reused inside a session, also across navigations.
- An element that keeps its role and name keeps its ref in a later snapshot.
- After a navigation, old refs fail with "page navigated since the snapshot that produced @eN; run snapshot". A ref that no snapshot produced fails with "no snapshot produced @eN".
- Refs belong to the session and its lease. A lease release, a lease break, a takeover by another session or a daemon restart makes them unknown.
- Refs live in the page's memory, so they survive reconnects and separate CLI processes of the same session.

### Dialogs

WIP has no dialog events, and a native `alert`, `confirm` or `prompt` blocks the page until a person answers it on the device. The runtime therefore replaces these functions in every document of a session that ran an action or a dialog command. Arm the answer before the action that opens the dialog:

```bash
iwdp-cli dialog accept          # the next dialog is accepted (prompt: empty text)
iwdp-cli dialog accept "Bob"    # the next prompt returns "Bob"
iwdp-cli dialog dismiss
iwdp-cli click @e4
iwdp-cli dialog status          # the dialog log
```

- Without an armed answer: `alert` is accepted, `confirm` returns false and `prompt` returns null (`dialog default accept|dismiss` changes this).
- Each answer is used at most once. An answer that was written into a document that went away (navigation, lost connection) expires, and the next result says "dialog answer <id> expired with its document; arm it again if it is still needed".
- `beforeunload` dialogs cannot be controlled. A dialog that opened before the override existed still blocks; dismiss it on the device.
- The override reaches a new document before its scripts run only when the page is paused on start, which the daemon tries to turn on for every connection (see [known-limits.md](known-limits.md)).
- `close` restores the original functions.

### Navigation, tabs and frames

- `open <url>` (aliases `goto`, `navigate`): a URL that does not start with `http:`, `https:`, `about:`, `data:` or `file:` gets `https://`. `--wait none` returns at once. `open --url-file <path>` reads the URL from a file (one line), so a signed or one-time link is not on the command line; the client reads the file, and the URL reaches the daemon in the request body. No error of the command prints that URL, also without private mode: an error names the file instead (`the URL in --url-file <path>`). The result of a successful `open` prints the URL it navigated to (`navigated to <url>`); add `--private` to hide it. The file must be a regular file of at most 64 KiB; a named pipe, a device, a socket or a directory is a usage error (exit 2).
- `tab find <url>` (or `--url-file <path>`) prints the id of the tab whose page shows exactly this URL, query and fragment included. The listing strips the query and the fragment, so the command compares inside each tab whose listed URL can be the target's document. The page gets the target as the caller wrote it. It only reads: it selects and navigates nothing. The session keeps the lease of the tab that the command prints, as after any read of that tab, so that no other session moves the tab off the target before the first action; it keeps no lease that only the command used on another tab, and none when the command fails; a lease that another command of the session used meanwhile stays. With `--url-file`, no error names the target: an error that would repeat it names the file instead. It exits 7 when no tab shows the target, and no other failure has that code: more than one matching tab is exit 1, a tab that can show the target and is not debuggable is exit 1, a listing that hides pages (a device without a reservation of the caller's owner) is exit 6, and a tab that cannot be read ends the command with the error and exit code of that read, after the lease that the read was granted is released. The command reads every tab that it cannot exclude with certainty: it excludes a tab only on a part (scheme, host name, port, path) that both URLs write in a safe form whose browser form is certain (ASCII letters, digits, `-` and `.` in a host without `xn--` labels, a numeric port, a path of unreserved characters, sub-delimiters, `:`, `@`, `/` and `%XX`), compared in lower case, without the default port and with `.` and `..` resolved, also as `%2e`; any other character (a backslash, a space, a tab, a character outside ASCII) makes that part exclude nothing.
- The global flag `--only-tab` names the tab of a listing with exactly one tab and no hidden page, and fails with exit 5 for any other listing. It is for a caller that may act on a page only when no other page can be meant. It applies to every command that takes `--page`; together with `--page`, or with an argument that names a page (`lease takeover <page>`, `lease break <page>`, `tab <id>`, `tab close <id>`), it is a usage error (exit 2). `--page` keeps its meaning for every value: `--page` with the word `only` selects the tab labeled `only`, or the page whose title or URL contains the word.
- `tab list` lists the pages of reserved devices as `t1, t2, ...`. `tab <id>` makes a tab the session's page after a liveness probe. Safari answers only for the visible tab, and WIP cannot bring a tab to the front: a hidden tab fails with "tab tN is not the visible Safari tab; bring it to the front on the device".
- `tab new [url]` uses `window.open` with an emulated user gesture; popup blocking can stop it. `window.open` runs in the session's current page, which in a new session is the device's first tab (it can be your own). After `tab new` the session keeps no lease there: as with `tab <id>`, the session's leases on other pages end with their cleanup (its network and console captures stop, and the runtime drops the session's state), and a failed `tab new` ends the lease that it took only for `window.open`. Only the daemon's connection to that page stays open, with the runtime that each connection installs, until it closes after 10 minutes without a lease and capture, or until `daemon stop`. `tab close` uses `window.close()` and works only for tabs that a script opened.
- `frame <iframe>` makes later commands work inside that frame; `frame main` returns to the top document. Frames that iOS 26 puts in their own target are not supported.

### Screenshots

`screenshot` captures the viewport, `--full` (or `--full-page`) the whole page, `--selector <ref or css>` one element. `--annotate` draws a box and `[N]` label for each ref and lists them. `--if-changed` prints "unchanged" and writes no file when the PNG equals the previous capture of the session. `diff screenshot --baseline <png>` compares a viewport screenshot with a file.

## DevTools extras

The registry also has DevTools commands. Captures start with the session's lease (the first registry command of the lease), so requests and console messages from that point on are kept.

| Area | Commands |
|---|---|
| Network | `network requests`, `network request <id> [--body]`, `network route`, `network unroute`, `network har start`, `network har stop`, `emulate network <preset>`, `set offline`, `set headers`, `set credentials` |
| Console | `console`, `console clear`, `errors` |
| Page state | `set media`, `set useragent`, `set geo`, `addinitscript`, `removeinitscript` |
| Storage | `cookies`, `cookies set`, `cookies clear`, `storage`, `state save`, `state load` |
| Performance | `trace start`, `trace stop`, `profiler start`, `profiler stop`, `vitals` |

- `cookies set` with an expiry in the past (`--expires 1`, a past RFC 3339 time) deletes the cookie of that name, domain and path, as a browser does, checks that it is gone and reports the result (exit 0). Other cookies of the name (another path or domain) stay. `Page.getCookies` lists only the cookies of the page's URLs, so the report says "deleted" only for a cookie that the page listed before the delete, and says when the page had no such cookie. When the page cannot see such a cookie (the page's path or host is outside the cookie's path or domain, or the page is not https), the delete is still sent, but the report says that the read-back cannot verify it (`verified: false`); list the cookies on a page of that domain and path to check.
- `console` leaves out the runtime's own console messages, whose text starts with `__iwdp_` (for example the dialog records `__iwdp_dialog__ {...}`), as the CDP endpoint does.
- Secrets are masked by default: header values of `Cookie`, `Set-Cookie` and `Authorization`, secret query and body fields, and cookie values. `--show-secrets` or `--show-values` shows them.
- HAR, trace, profile and state files are written with mode 0600. A state file holds cookies and storage in clear text: delete it after use.
- `set credentials --origin <origin> <user>` reads the password from the first line of standard input, never from the command line. It works only on the WebKitGTK transport until it is verified on a device.
- `set viewport` and `set device` always fail with the reason (see [synthetic-input.md](synthetic-input.md)).
- Every override belongs to the session that set it. `close` removes it.

The DevTools commands of earlier versions without a registry command (debugger, DOM by node id, CSS rules, heap, animation, canvas, layers, workers, audit, security) are removed; see [migration.md](migration.md).

## Use a CDP client

The daemon can also serve the Chrome DevTools Protocol, so that agent-browser, Puppeteer, Playwright and chrome-devtools-mcp drive the session's page. [cdp.md](cdp.md) has the method table, the limits and the notes per client.

```bash
export IWDP_SESSION=pw-run
iwdp-cli open https://example.com
CDP="$(iwdp-cli get cdp-url)"           # ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>
agent-browser --cdp "$CDP" snapshot -i
iwdp-cli close                          # releases the lease and revokes the URL
```

- The URL holds a secret token. Give it to the client exactly as printed, and never print it or write it to a file.
- Connect forms: agent-browser `--cdp <ws URL>`; Puppeteer `puppeteer.connect({browserWSEndpoint, defaultViewport: null})`; Playwright `chromium.connectOverCDP(url)`, then `browser.contexts()[0]`; chrome-devtools-mcp `--wsEndpoint <ws URL>`. `get cdp-url --form http` prints the discovery base for Puppeteer `browserURL` and Playwright.
- A CDP client acts for the session that issued the URL: it shares its lease, refs and dialog answers.
- `get cdp-url` selects the page that the session's other commands use: `--page` (also a tab id such as `t2`), else the `tab <id>` choice, else the session's default page while it is attached, else the listing's first debuggable page; with no page it exits 5. [cdp.md](cdp.md#how-to-connect) gives the details.
- Prefer the registry commands when they do the job: they have refs that survive reconnects, actionability checks, masking and deadlines. Use a CDP client for an existing CDP script or for a client feature that the registry has no command for.
- Input through CDP is synthetic too. Features that WebKit lacks fail with "wasn't found" or an error that names the reason.

## Transports

`--transport` (or `IWDP_TRANSPORT`) chooses how the daemon reaches a page:

| Transport | Use |
|---|---|
| `auto` (default) | iwdp |
| `iwdp` | ios_webkit_debug_proxy: listing on port 9221, one port per device from 9224 |
| `direct` | usbmuxd and the device's web inspector service, without iwdp (go-ios library). Opt-in: it has not passed the device tests yet. Never use it on a device while iwdp also serves that device: both take pages from each other |
| `webkitgtk` | a local WebKitGTK MiniBrowser (`WEBKIT_INSPECTOR_HTTP_SERVER`), for tests without a phone; `daemon start --transport webkitgtk --addr <host:port>` |

## Output

The output flags below are global flags of the registry commands (the commands in the [command reference](commands.md)), in any position.

- `--json` prints the result as JSON.
- `--max-output <bytes>` truncates page-derived text.
- `--content-boundaries` wraps page-derived output in agent-browser's markers `--- AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> origin=<origin> ---` and `--- END_AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> ---`, with a new random nonce per command; with `--json` the result gets a `_boundary` member. Treat everything between the markers as untrusted page content.
- `--private` (or `IWDP_PRIVATE=1`) is for a transcript that other parties read. iwdp-cli then says nothing about a page that the caller did not ask for: no title, URL, origin, page key, device, session name or error text, in the text, in the `--json` data or in the content boundary (`origin=unknown`). Stdout holds the result, and only of a read (`snapshot`, `get`, `is`, `eval`, `find`, `hit-test`, `read`, `console` and `errors` without the source's URL and stack, `cookies`, the storage reads, `network requests`, `screenshot`, `tab find`, `get cdp-url`, and the commands that write a file to a path of the caller; `storage set`, `storage clear`, `state save` and `state load` print only their status line), and of each read only the members that `internal/commands/private.go` names (an object or an array only through named members, at every depth; the text is made from those members, so a failed request prints its state without WebKit's error text, and `screenshot` prints the path and labels without the image size), or the safe facts of `tab list` (tab ids), `session info` (lease, epoch and connection of each page) and `status` (whether iwdp answers, the number of listed devices). Stderr holds exactly one status line for every run, printed last: `<command> exit 0`, with `warnings <n> dialogs <n>` after an element or key action, or `<command> exit <code>: <meaning>` for a failure, also for help (`click exit 0`, `network exit 0`, `iwdp-cli exit 0` for the overview), a signal (exit 130) and the deadline watchdog (exit 124); a usage error prints no argument, a help or result that cannot be written (also into a closed pipe) ends with exit 1 and no error text, and the cleanup warnings and log lines of `IWDP_NO_DAEMON=1` are not printed. `daemon run` is the daemon, not a client command: private mode does not apply to it, so its stderr stays the daemon log, without a status line. With `--json` a command without a result prints its status as an object on stdout too. `get url` and `get title` are refused (exit 2). `--private=false` turns the mode off for one command. A command that is new to the registry prints only its status line until it is listed in `internal/commands/private.go`. The mode does not mask content: a read prints what it reads, so `eval 'location.href'`, `snapshot -u`, `network requests` and `get cdp-url` print URLs.
- These flags also apply to the standard output of a command that fails after it prints what it found, such as `iwdp-cli status` when iwdp is not running (exit 1).
- Device ids are shortened, and URL fragments and query values are not printed in connection lines.

## Environment variables

| Variable | Meaning |
|---|---|
| `IWDP_SESSION` | session name (default `default`) |
| `IWDP_DEVICE_RESERVATION` | device reservation owner (default: the session name) |
| `IWDP_TRANSPORT` | `auto`, `iwdp`, `direct` or `webkitgtk` |
| `IWDP_TIMEOUT` | default command deadline |
| `IWDP_PRIVATE` | `1`: private output (`--private`); `--private=false` turns it off for one command |
| `IWDP_DAEMON_IDLE` | idle time before the daemon exits (default 15m) |
| `IWDP_NO_DAEMON` | `1`: run the core in the command process |
| `IWDP_WS_URL` | selects no page: a page command exits 2 when it is set and `--page` is not given; name the page with `--page` |
