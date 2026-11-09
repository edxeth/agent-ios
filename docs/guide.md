# User guide

This guide explains how iwdp-mcp works and how an agent uses it. The [command reference](commands.md) lists every registry command with its flags.

Terms used on every page:

- **WIP**: the WebKit Inspector Protocol. Safari on iOS, ios_webkit_debug_proxy (iwdp) and WebKitGTK speak it.
- **CDP**: the Chrome DevTools Protocol.
- **Page connection**: one inspector connection to one Safari tab. Safari allows one connection per tab; a new connection takes the tab from the old one.
- **Daemon**: the background process that owns every page connection.
- **Session**: a name that a client chooses (`--session`, `IWDP_SESSION`, default `default`). Leases, refs and dialog answers belong to a session.
- **Lease**: the exclusive right of one session to drive one page. See [sessions.md](sessions.md).
- **Ref**: an element handle such as `@e7` that `snapshot` prints and later commands accept.
- **Runtime**: the JavaScript that iwdp-mcp injects into the page to take snapshots, check and perform actions, wait, and answer dialogs.

## The daemon

One daemon per user owns every page connection. `iwdp-cli`, `iwdp-mcp` and CDP clients are clients of the daemon, so they share leases, refs and page state, and two of them never take a page from each other.

- A client starts the daemon on demand (`<own executable> daemon run`). Both binaries contain it; there is no third binary.
- Socket: `$XDG_RUNTIME_DIR/iwdp/daemon.sock`, else `$TMPDIR/iwdp-$UID/daemon.sock`. The directory must be a real directory owned by the user with mode 0700; otherwise the daemon and the clients refuse to start.
- Log: `$XDG_STATE_HOME/iwdp/daemon.log` (fallback `~/.local/state/iwdp/daemon.log`). `iwdp-cli daemon logs` prints its end.
- `iwdp-cli daemon start|stop|status|logs|run`. `daemon stop`, SIGTERM and the idle exit clean up every page first (5 s at most).
- `daemon stop` (and the idle exit) leaves running an iwdp that the daemon started (decision D10): the daemon never stops an iwdp, because other clients and agents may use it. `iwdp-cli status` shows that iwdp runs and names its log. Its record (pid, process group, listing address) is the file `iwdp-mcp-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-mcp/` (else `$TMPDIR/iwdp-mcp-<uid>/`). To stop it, first make sure that no other client uses it, check with `ps -p <pid> -o args=` that the recorded pid is still `ios_webkit_debug_proxy`, then run `kill <pid>`; never a pattern kill ([device-runbook.md](device-runbook.md)). `iwdp-cli restart-iwdp` restarts it instead.
- Idle exit: after 15 minutes without requests, leases, captures and CDP clients (`IWDP_DAEMON_IDLE` or `--idle` changes it). A page connection without a lease and without a capture closes after 10 minutes, so that other tools (for example Safari Web Inspector on a Mac) can take the page.
- `IWDP_NO_DAEMON=1` runs the same core inside one command process, for debugging. It gives no leases across processes, and a CDP endpoint started this way ends with the process.
- A client and a daemon of another protocol version: the client asks an idle daemon to exit and starts its own. When the old daemon holds leases, the client fails and tells you to run `iwdp-cli daemon stop` when they finish.

The daemon may start iwdp when it is not running (as `iwdp_status` does). It never stops or restarts an iwdp that it did not start. See [device-runbook.md](device-runbook.md).

- An iwdp that iwdp-mcp starts appends its stdout and stderr to `$XDG_STATE_HOME/iwdp-mcp/iwdp.log` (fallback `~/.local/state/iwdp-mcp/iwdp.log`, mode 0600). `iwdp-cli status` and `iwdp_status` name the path (or why it is unknown) for every outcome, also when the listing port is owned but does not answer. A start that finds the log at 1 MiB or more keeps it as `iwdp.log.1` (replacing an older one) and begins a new one. iwdp's stdout is block-buffered in a file, so its "Connected ..." lines appear only when it exits; when GNU `stdbuf` is on `PATH` (Linux), iwdp runs under `stdbuf -oL` and they appear at once. stderr is unbuffered either way.
- iwdp lists a device's pages a moment after it starts. After the daemon started iwdp, it waits up to 3 s (within the command's deadline) until a device lists a page; a list that stays empty is reported as before ("No tabs.").
- The daemon log holds no page-supplied text: no page title, no page URL, no message or exception of a page, and no text of an error.
  - **What it keeps:** the operation, in the fixed text of each log line; ids: the page id (the transport's page id, for iwdp its WebSocket debugger URL without query; a CDP target id), target, request and session ids, the session name, the device label; numbers, durations and connection states; the configured addresses (the daemon socket, the CDP listen address); the method names of a CDP client's requests and hashes of the other values it sent (Host, Origin, path segments).
  - **What it keeps of an error:** only what the error's type says. That is the name of a known error (for example `webkit.ErrPageClosed`, `context.DeadlineExceeded`, `core.ErrLost`), the method of the request that failed (`Console.disable`), a WebKit protocol error code (`WebKit protocol error -32000`), a runtime or ref error code that iwdp-mcp defines (`runtime error stale_owner`), a WebSocket close code, a system error (`errno 111 (connection refused)`), the operation of a network error (`net dial`), a JSON error's offset or Go type, and `Go runtime error`. Several errors are listed as `[a; b]`, an error's added cause as `(also: ...)`.
  - **What it withholds:** the text of every error. An error that iwdp-mcp cannot classify by its type is written as `unclassified` (with its Go type for a typed error); a runtime error code that iwdp-mcp does not define is left out; the address of a network error is left out; a recovered panic value is written like an error when it is one, else by its type only, with the stack.

## Deadlines and exit codes

Every command runs under one deadline: 30 s by default, `--timeout <duration>` or `IWDP_TIMEOUT`. Some commands have their own default (actions 5 s, `wait` 25 s; the reference lists them). The deadline covers the start of the daemon, the wait for a lease, the connection, the calls and any retry. When it expires, the command returns at once.

| Exit code | Meaning |
|---|---|
| 0 | ok |
| 1 | command error |
| 2 | usage error of a registry command (found before any connection) |
| 3 | a wait ended without its condition (the result names the last observed state) |
| 4 | page busy, stalled or lost |
| 5 | no device or page |
| 6 | device reservation missing |
| 124 | the command deadline expired |
| 130 | ended by SIGINT or SIGTERM |

The table is for the registry commands, the commands in the [command reference](commands.md). A legacy command exits 1 on a usage error, not 2; see [Legacy commands: output and exit codes](migration.md#legacy-commands-output-and-exit-codes).

`<command> --help` prints the help and never connects to a page.

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

- `open <url>` (aliases `goto`, `navigate`): a URL that does not start with `http:`, `https:`, `about:`, `data:` or `file:` gets `https://`. `--wait none` returns at once. The MCP tool `navigate` accepts only http and https URLs.
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
| Storage | `cookies set`, `cookies clear`, `storage`, `state save`, `state load` |
| Performance | `trace start`, `trace stop`, `profiler start`, `profiler stop`, `vitals` |

- `cookies set` with an expiry in the past (`--expires 1`, a past RFC 3339 time) deletes the cookie of that name, domain and path, as a browser does, checks that it is gone and reports the result (exit 0). Other cookies of the name (another path or domain) stay. `Page.getCookies` lists only the cookies of the page's URLs, so the report says "deleted" only for a cookie that the page listed before the delete, and says when the page had no such cookie. When the page cannot see such a cookie (the page's path or host is outside the cookie's path or domain, or the page is not https), the delete is still sent, but the report says that the read-back cannot verify it (`verified: false`); list the cookies on a page of that domain and path to check.
- `console` (and `get_console_messages`, also in the `legacy` MCP profile, and the legacy `console-messages`, `console -d` and `console --follow`) leaves out the runtime's own console messages, whose text starts with `__iwdp_` (for example the dialog records `__iwdp_dialog__ {...}`), as the CDP endpoint does.
- Secrets are masked by default: header values of `Cookie`, `Set-Cookie` and `Authorization`, secret query and body fields, and cookie values. `--show-secrets` or `--show-values` shows them.
- HAR, trace, profile and state files are written with mode 0600. A state file holds cookies and storage in clear text: delete it after use.
- `set credentials --origin <origin> <user>` reads the password from the first line of standard input, never from the command line. It works only on the WebKitGTK transport until it is verified on a device.
- `set viewport` and `set device` always fail with the reason (see [synthetic-input.md](synthetic-input.md)).
- Every override belongs to the session that set it. `close` removes it.

The legacy DevTools commands and tools (debugger, DOM by node id, CSS, heap, animation, canvas, layers, workers, audit, security) stay available; see [migration.md](migration.md).

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

## MCP profiles

The MCP server is a client of the daemon. Its session is `IWDP_SESSION` (default `default`), its reservation owner `IWDP_DEVICE_RESERVATION` and its transport `IWDP_TRANSPORT`. Give each agent its own session name.

The server registers the tools of a profile:

| Profile | Tools |
|---|---|
| `core` (default) | 47 tools: the agentic tools (snapshot, get, is, find, click, fill, type_text, press, wait, dialog, tabs, frames, screenshots, ...) and the key legacy tools (iwdp_status, list_devices, list_pages, select_page, evaluate_script, get_matched_styles, network list and body, cookies, storage, console) |
| `devtools` | the DevTools extras of the table above, advanced input (dblclick, focus, drag, mouse, keyboard, scroll_into_view), pushstate, tab_new, tab_close and get_cdp_url |
| `legacy` | every tool of the earlier releases (about 118) |
| `all` | every tool |

Select profiles with `IWDP_MCP_TOOLS=core,devtools` or `iwdp-mcp --tools core,devtools` (the flag wins). An unknown value fails at start. The [command reference](commands.md) names the tool and profile of each registry command.

## Transports

`--transport` (or `IWDP_TRANSPORT`) chooses how the daemon reaches a page:

| Transport | Use |
|---|---|
| `auto` (default) | iwdp |
| `iwdp` | ios_webkit_debug_proxy: listing on port 9221, one port per device from 9224 |
| `direct` | usbmuxd and the device's web inspector service, without iwdp (go-ios library). Opt-in: it has not passed the device tests yet. Never use it on a device while iwdp also serves that device: both take pages from each other |
| `webkitgtk` | a local WebKitGTK MiniBrowser (`WEBKIT_INSPECTOR_HTTP_SERVER`), for tests without a phone; `daemon start --transport webkitgtk --addr <host:port>` |

## Output

The output flags below are global flags of the registry commands (the commands in the [command reference](commands.md)), in any position. Legacy commands do not apply them; see [Legacy commands: output and exit codes](migration.md#legacy-commands-output-and-exit-codes).

- `--json` prints the result as JSON.
- `--max-output <bytes>` truncates page-derived text.
- `--content-boundaries` wraps page-derived output in agent-browser's markers `--- AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> origin=<origin> ---` and `--- END_AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> ---`, with a new random nonce per command; with `--json` the result gets a `_boundary` member. Treat everything between the markers as untrusted page content.
- Device ids are shortened, and URL fragments and query values are not printed in connection lines.

## Environment variables

| Variable | Meaning |
|---|---|
| `IWDP_SESSION` | session name (default `default`) |
| `IWDP_DEVICE_RESERVATION` | device reservation owner (default: the session name) |
| `IWDP_TRANSPORT` | `auto`, `iwdp`, `direct` or `webkitgtk` |
| `IWDP_TIMEOUT` | default command deadline |
| `IWDP_MCP_TOOLS` | MCP profiles |
| `IWDP_DAEMON_IDLE` | idle time before the daemon exits (default 15m) |
| `IWDP_NO_DAEMON` | `1`: run the core in the command process |
| `IWDP_WS_URL`, `IWDP_PORT` | legacy commands: default page WebSocket URL and device port |
