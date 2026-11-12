# Migration notes

Earlier releases had one connection per command and one tool per WebKit method. The current release runs every command through the daemon and adds the registry commands of the agentic loop. The commands of earlier releases that are not registry commands are removed; this page says what changed and which registry command replaces each removed one.

## What changed for every user

- **The daemon.** The first command starts a background daemon that keeps the page connection. Commands are faster and share state. `iwdp-cli daemon stop` stops it; see [guide.md](guide.md#the-daemon).
- **Daemon protocol 2.** The daemon's socket API is now protocol 2: the endpoints `/v1/op`, `/v1/raw`, `/v1/events` and `/v1/lease` are gone, and `/v1/run` carries the command's absolute deadline. A client of protocol 1 refuses a daemon of protocol 2 (and replaces it when idle).
- **Sessions and leases.** A page belongs to one session at a time. Give each agent its own `--session` or `IWDP_SESSION`, and end it with `iwdp-cli close`. See [sessions.md](sessions.md).
- **Device reservations.** Pages of a physical device need a reservation: `iwdp-cli device reserve --owner <owner>` first. Without one, page commands exit 6.
- **Lost pages.** When someone else takes the page, commands fail with exit 4 and "ownership is unknown"; nothing reconnects by itself. `--takeover` reattaches, with the device owner's approval.
- **The "Connecting to: <title>" line is gone.** Errors still name the page.
- **restart-iwdp** restarts only an iwdp that iwdp-cli started and recorded, and refuses otherwise ([device-runbook.md](device-runbook.md)). It also refuses every iwdp that an earlier version started. The removed iwdp-mcp server and an iwdp-cli older than the writer mark wrote records without the iwdp-cli mark. Every version before the rename to iwdp-cli, also one that wrote the mark, kept its record and start token under the old names, which this release does not read ([Rename to iwdp-cli](#rename-to-iwdp-cli)). Whoever started such an iwdp, and only when nobody else uses it, stops it once, as [Stopping an iwdp of an older version](#stopping-an-iwdp-of-an-older-version) describes; the next start records the new iwdp under the new names, with the mark.
- **Exit codes of registry commands**: 0 ok, 1 error, 2 usage, 3 wait timeout, 4 page busy or lost, 5 no device or page, 6 reservation missing, 124 deadline, 130 signal.

## Rename to iwdp-cli

The project and its Go module are renamed from iwdp-mcp to iwdp-cli: the module path is `github.com/nnemirovsky/iwdp-cli`, and `go install github.com/nnemirovsky/iwdp-cli/cmd/iwdp-cli@latest` installs the binary. The files and the environment variable that iwdp-cli keeps for an iwdp that it starts are renamed too, without a migration; nothing is carried over from the old names:

- The record of a started iwdp is `iwdp-cli-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-cli/` (else `$TMPDIR/iwdp-cli-<uid>/`). It was `iwdp-mcp-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-mcp/` (else `$TMPDIR/iwdp-mcp-<uid>/`).
- The start lock is `iwdp-cli-start-<uid>-<address>.lock` (was `iwdp-mcp-start-<uid>-<address>.lock`).
- The start token in the environment of a started iwdp is `IWDP_CLI_START_TOKEN` (was `IWDP_MCP_START_TOKEN`).
- The iwdp log is now `$XDG_STATE_HOME/iwdp-cli/iwdp.log` (fallback `~/.local/state/iwdp-cli/iwdp.log`), and `iwdp-cli status` names that path. It was `$XDG_STATE_HOME/iwdp-mcp/iwdp.log`; the old file is not moved, and an iwdp that an older version started keeps writing to it.

An iwdp that an older version started is therefore another program's iwdp for this version: it reads no record for it, so `restart-iwdp` refuses it. Whoever started that iwdp, and only when nobody else uses it, stops it once; the next start records the new iwdp under the new names. The daemon's own files keep the name `iwdp` (`$XDG_RUNTIME_DIR/iwdp/`, `$XDG_STATE_HOME/iwdp/daemon.log`) and are not affected.

### Stopping an iwdp of an older version

A pid in an old record proves nothing by itself: the recorded iwdp may have exited, and its pid may now belong to another process, also to another owner's iwdp. Stop such an iwdp only if you started it yourself and nobody else uses it, as [Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand) in device-runbook.md describes, with the old record (`$XDG_RUNTIME_DIR/iwdp-mcp/iwdp-mcp-started-<uid>-<address>.json`, else `$TMPDIR/iwdp-mcp-<uid>/`): `scripts/iwdp-owner-check.sh --terminate <record>` checks the process against that record and its start token `IWDP_MCP_START_TOKEN` and sends SIGTERM only when it reports `verified`; then run `scripts/iwdp-owner-check.sh <record>` until it reports `gone`. If you did not start it, another client uses it or you do not know, or the check does not report `verified`, send no signal: leave it running and ask whoever started it, or the user.

## MCP server

The MCP server (`iwdp-mcp`), its tool profiles (`IWDP_MCP_TOOLS`, `--tools`), the Claude Code plugin and the MCP skill `ios-safari-debug` are removed. Agents run `iwdp-cli` commands instead and read the built-in skill with `iwdp-cli skills get iwdp`. Each former tool of a registry command is the registry command of the [command reference](commands.md) (for example `navigate` is `open`, `tab_select` is `tab`, `select_frame` is `frame`, `close_session` is `close`, `read_page` is `read`); the other former tools map like the removed commands below. Parameters are now flags: `full_page` is `--full-page`, `http_only` is `--http-only`, and a CSS selector is the target argument. A client that needs a protocol connection uses the CDP endpoint (`iwdp-cli get cdp-url`, [cdp.md](cdp.md)).

## CLI: removed commands and their registry commands

`iwdp-cli` runs only the registry commands of the [command reference](commands.md). Every command of earlier versions that is not in the registry is removed; a removed name is an unknown command (exit 2). A removed name that starts registry commands, such as `network`, lists them instead: `iwdp-cli network --help` prints the list (exit 0), `iwdp-cli network` prints it as a usage error (exit 2). The global options take the values of earlier versions: `--timeout`, `IWDP_TIMEOUT` and `--wait-lease` accept a duration (`5s`) or whole seconds (`5`). Every usage error exits 2, also an unknown `--flag` (for example `click body --bogus` or `console --follow`) and a bad global option. No command takes its page from a trailing `ws://` argument or from `IWDP_WS_URL` any more: a trailing page URL, a page URL as the screenshot file, and a page command while `IWDP_WS_URL` is set without `--page` are usage errors that name `--page`. Name the page with `--page <tab id or ws:// URL>`. `IWDP_PORT` is no longer read.

| Removed command | Registry command | Notes |
|---|---|---|
| `eval [--await] <expr> [ws-url]` | `eval <expr>` | always awaits a returned Promise and prints its value; earlier versions awaited it only with `--await` and otherwise printed the Promise object itself. `--await` is still accepted and changes nothing; only known flags are flags, so `-1` or `'-Math.abs(2)'` is the expression; `--` ends the flags; output changes: [below](#the-kept-commands-eval-cookies-status-devices-restart-iwdp) |
| `cookies [--show-values] [ws-url]` | `cookies [--show-values]` | output changes: [below](#the-kept-commands-eval-cookies-status-devices-restart-iwdp) |
| `devices`, `status`, `restart-iwdp` | the same names | same standard output and exit codes; the error lines change: [below](#the-kept-commands-eval-cookies-status-devices-restart-iwdp) |
| `pages [port]` | `tab list` | tabs of the reserved devices as `t1, t2, ...` |
| `navigate <url> [ws-url]` | `open <url>` (aliases `goto`, `navigate`) | waits for the load state |
| `reload [--ignore-cache]` | `reload [--ignore-cache]` | waits for the load state |
| `screenshot [--full-page] [--selector css] [-o file]` | `screenshot [path] [--full-page] [--selector] [-o file]` | same flags; also `--annotate`, `--if-changed` |
| `snapshot-node <nodeId>` | `screenshot --selector <@eN or css>` | refs instead of node ids |
| `dom [selector]`, `query-selector`, `query-selector-all`, `get-outer-html`, `get-attributes` | `snapshot`, `find`, `get html|attr|count <target>` | refs instead of node ids |
| `get-computed-style` | `get styles <target>` | |
| `click <selector>` | `click <target>` | actionability checks |
| `fill <selector> <value>` | `fill <target> <text>`, `fill <target> --stdin` | |
| `type-text <text>` | `type <target> <text>`, `keyboard type <text>` | no `type-text` alias |
| `highlight-node <nodeId>` | `highlight <target>` | |
| `force-pseudo-state <nodeId> hover` | `hover <target> --css` | hover only |
| `console [-d] [--follow]`, `console-messages` | `console [--since <id>]`, `errors` | no live stream: run `console --since "$last_id"` again, with the id of the last message that it printed |
| `clear-console` | `console clear` | |
| `network [-d] [--follow]`, `get-response-body <id>` | `network requests`, `network request <id> --body` | no live stream |
| `set-request-interception`, `intercept-continue`, `intercept-respond` | `network route <url> [--abort|--body|--status|--delay]`, `network unroute` | patterns, not request ids |
| `set-extra-headers <json>` | `set headers <json>` | |
| `set-network-conditions <bytes/sec>` | `emulate network <preset>` | |
| `set-cookie ...` | `cookies set <name> <value> [flags]` | |
| `delete-cookie` | `cookies clear [--domain d]` | clears every cookie of the domain |
| `get-local-storage`, `set-local-storage-item`, `clear-local-storage`, `get-session-storage`, `set-session-storage-item`, `clear-session-storage` | `storage local|session [get|set|clear]` | page origin only |
| `remove-local-storage-item`, `remove-session-storage-item` | removed | `storage local|session clear`, or `eval "localStorage.removeItem('k')"` |
| `timeline-record` | `trace start`, `trace stop [path]` | Chrome trace JSON |
| `cpu-profile`, `script-profile` | `profiler start`, `profiler stop [path]` | ScriptProfiler |
| `call-function`, `get-properties` | removed | use `eval` |
| `get-event-listeners`, `hide-highlight` | removed | |
| `get-matched-styles`, `get-inline-styles`, `set-style-text`, `get-all-stylesheets`, `get-stylesheet-text` | removed | CSS rule reading and editing |
| `disable-cache`, `set-log-level` | removed | |
| `list-indexed-databases`, `get-indexed-db-data`, `clear-indexed-db-store` | removed | IndexedDB |
| `debugger-enable`, `debugger-disable`, `set-breakpoint`, `remove-breakpoint`, `pause`, `resume`, `step-over`, `step-into`, `step-out`, `get-script-source`, `search-in-content`, `eval-on-frame`, `set-pause-on-exceptions` | removed | the JavaScript debugger |
| `set-dom-breakpoint`, `remove-dom-breakpoint`, `set-event-breakpoint`, `remove-event-breakpoint`, `set-url-breakpoint`, `remove-url-breakpoint` | removed | DOM, event and URL breakpoints |
| `memory-track`, `heap-snapshot`, `heap-track`, `heap-gc` | removed | heap and memory |
| `animation-enable`, `animation-disable`, `animation-track`, `get-animation-effect`, `resolve-animation` | removed | animations |
| `canvas-enable`, `canvas-disable`, `get-canvas-content`, `start-canvas-recording`, `stop-canvas-recording`, `get-shader-source` | removed | canvas |
| `get-layer-tree`, `get-compositing-reasons` | removed | layer tree |
| `worker-enable`, `worker-disable`, `send-to-worker`, `get-service-worker-info` | removed | workers |
| `run-audit`, `get-certificate-info` | removed | audits and certificates |
| `browser-extensions-enable`, `browser-extensions-disable` | removed | browser extensions |
| none | `wait`, `dialog`, `back`, `forward`, `tab`, `frame`, `diff snapshot`, `diff screenshot`, `vitals`, `state save|load`, `network har start|stop`, `set media|useragent|geo|offline|credentials`, `addinitscript`, `get cdp-url` | new |

## The kept commands: eval, cookies, status, devices, restart-iwdp

These five commands keep their names. They are registry commands now, so their output differs from earlier versions in these ways.

- **Output flags.** `--json`, `--max-output <bytes>` and `--content-boundaries` now apply to all five, as to every registry command. Earlier versions ignored these flags or read them as arguments; for example `eval document.title --content-boundaries` printed the title without markers, and now it prints the title between the content markers. `--max-output` cuts the text output and appends `[output truncated: N of M bytes shown]`; it does not cut `--json` output.
- **Errors name the command.** An error of the command prints `Error: <command>: <message>` on stderr, for example `Error: eval: <thrown message>` or `Error: restart-iwdp: <message>`. Earlier versions printed `Error: <message>`. An error while `eval` or `cookies` connects to or starts the daemon prints `Error: <message>`, without the name; a deadline prints the deadline message (exit 124), as before. The exit codes are unchanged, except that usage errors exit 2 (above).
- **Usage errors.** A missing expression (`eval` alone) and an unknown option (`cookies --bogus`) exit 2 with the registry's usage error. Earlier versions printed the usage line and exited 1.

Output of each command:

| Command | Standard output (text) | `--json` | Other changes |
|---|---|---|---|
| `eval <expr>` | a string as is, another value as indented JSON, else the value's description or `(<type>)`, as before; a returned Promise prints its resolved value (earlier versions printed the Promise object unless `--await` was given) | `{"type":...,"subtype":...,"value":...}`; `subtype` only when WebKit gives one; a result without a value has `description` (when WebKit gives one) instead of `value` | a thrown value prints `Error: eval: <message>` and its stack (exit 1) |
| `cookies` | one line per cookie, `name=<value>  domain=<d> path=<p>`, then `sameSite=`, `expires=` (RFC 3339, UTC), `httpOnly`, `secure`, `session` when they apply; `No cookies.` for none | `{"cookies":[{"name","domain","path","expires","session","httpOnly","secure","sameSite"}],"valuesShown":false}`; with `--show-values` each cookie also has `value` and `valuesShown` is true | a hidden value prints as `(hidden)`, also an empty value; earlier versions printed a shortened value with its length, such as `s3cr...(redacted, 31 chars)`. The stderr line `Cookie values are redacted; pass --show-values to print them.` is gone; the command summary in `cookies --help` says that values are hidden unless `--show-values` |
| `status` | unchanged: `ios_webkit_debug_proxy is running` or `ios_webkit_debug_proxy is NOT running`, then the iwdp log line | `{"running":true,"log":"<path>"}`, with `"running":false` on stdout when the check fails (then the error follows on stderr); `logError` instead of `log` when the path is unknown | `--content-boundaries` and `--max-output` apply to the standard output also when the check fails (exit 1), as `--json` does. Not running: stderr is `Error: status: ios_webkit_debug_proxy is not running.` and then `Start it with: <command>` (exit 1); earlier only the `Start it with:` line. A listing owner that does not answer: `Error: status: <how it failed>` (exit 1) |
| `devices` | unchanged: one line per device (name, UDID, `(pages on <host:port>)`); `No devices connected.` for none | the listing's device entries as a JSON array (`[]` for none) | not running: `devices` starts iwdp as page commands do (decision D10); a failed start: `Error: devices: ios_webkit_debug_proxy is not running and starting it failed: ...` (exit 1); earlier it printed `Start it with: <command>` and started nothing. A listing owner that does not answer: `Error: devices: <how it failed>` (exit 5). On Linux an iwdp that iwdp-cli started and that lost its devices is restarted first (guide.md, the daemon); an empty listing while usbmuxd reports a USB device: `Error: devices: ios_webkit_debug_proxy lists no device, although usbmuxd reports <n> USB device(s). ...` (exit 5) |
| `restart-iwdp` | unchanged: `ios_webkit_debug_proxy restarted` | `{"restarted":true}` | a refusal or failure prints `Error: restart-iwdp: <message>`; it refuses records without the iwdp-cli mark (above) |

## For skills and prompts written for agent-browser

The registry commands keep agent-browser's names, flags and output wherever WIP allows (`open`, `snapshot -i`, `click @e3`, `fill`, `get text`, `is visible`, `find role button --name Save click`, `wait --load networkidle`, `--session`, `--json`, `--content-boundaries`). The differences: all input is synthetic ([synthetic-input.md](synthetic-input.md)); `set viewport` and `set device` fail; there are no browser contexts, PDF or downloads; physical devices need `device reserve`.

## Command lines, daemon status and daemon stop after the upgrade

- Help no longer hides a usage error. A line with an unknown `--flag` or an unknown command exits 2 also with `--help` or `-h`, and so does `help <unknown command>` (it exited 1 before). Only `iwdp-cli --help`, `-h` or `help` alone print the overview; options without a command (`iwdp-cli --json --help`) exit 2. An empty command line prints the overview on stderr and exits 2 (it exited 1).
- `--json=false`, `--content-boundaries=false` and `--takeover=false` now mean false. Before, a global boolean option ignored its value and was always true. A value that Go's `strconv.ParseBool` does not accept (`--json=yes`) exits 2.
- After the upgrade, `iwdp-cli daemon status` and `iwdp-cli daemon stop` reach the daemon that the previous release started (protocol 1), also while it holds leases. Other commands replace that daemon when it is idle; when it is busy they tell you to run `iwdp-cli daemon stop` when its sessions finish.
