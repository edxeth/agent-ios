# Migration notes

Earlier releases had one connection per command and one tool per WebKit method. The current release runs every command through the daemon and adds the registry commands of the agentic loop. The commands of earlier releases that are not registry commands are removed; this page says what changed and which registry command replaces each removed one.

## What changed for every user

- **The daemon.** The first command starts a background daemon that keeps the page connection. Commands are faster and share state. `agent-ios daemon stop` stops it; see [guide.md](guide.md#the-daemon).
- **Daemon protocol 2.** The daemon's socket API is now protocol 2: the endpoints `/v1/op`, `/v1/raw`, `/v1/events` and `/v1/lease` are gone, and `/v1/run` carries the command's absolute deadline. A client of protocol 1 refuses a daemon of protocol 2 (and replaces it when idle).
- **Sessions and leases.** A page belongs to one session at a time. Give each agent its own `--session` or `AGENT_IOS_SESSION`, and end it with `agent-ios close`. See [sessions.md](sessions.md).
- **Device reservations.** Pages of a physical device need a reservation: `agent-ios device reserve --owner <owner>` first. Without one, page commands exit 6.
- **Lost pages.** When someone else takes the page, commands fail with exit 4 and "ownership is unknown"; nothing reconnects by itself. `--takeover` reattaches, with the device owner's approval.
- **The "Connecting to: <title>" line is gone.** Errors still name the page.
- **restart-iwdp** restarts only an iwdp that agent-ios started and recorded, and refuses otherwise ([device-runbook.md](device-runbook.md)). It also refuses every iwdp that an earlier version started. The removed iwdp-mcp server and an iwdp-cli older than the writer mark wrote records without the iwdp-cli mark. Every version before the rename to iwdp-cli, also one that wrote the mark, kept its record and start token under the old names, which this release does not read ([Rename to iwdp-cli](#rename-to-iwdp-cli)). Whoever started such an iwdp, and only when nobody else uses it, stops it once, as [Stopping an iwdp of an older version](#stopping-an-iwdp-of-an-older-version) describes; the next start records the new iwdp under the new names, with the mark. Since the [rename to agent-ios](#rename-to-agent-ios) the mark is `agent-ios`, and an iwdp that iwdp-cli started is refused too.
- **Exit codes of registry commands**: 0 ok, 1 error, 2 usage, 3 wait timeout, 4 page busy or lost, 5 no device or page, 6 reservation missing, 124 deadline, 130 signal.

## Rename to agent-ios

The program, its skills and its Go module are renamed from iwdp-cli to agent-ios: the binary is `agent-ios`, the module path is `github.com/edxeth/agent-ios`, and `go install github.com/edxeth/agent-ios/cmd/agent-ios@latest` installs it. `iwdp` still names ios_webkit_debug_proxy, so `restart-iwdp`, `sim up --restart-iwdp`, `--transport iwdp` and the iwdp log keep their names. Nothing reads the old names; move what you need by hand:

- The built-in skills are `agent-ios`, `agent-ios-cdp`, `agent-ios-device` and `agent-ios-devtools` (were `iwdp`, `iwdp-cdp`, `iwdp-device` and `iwdp-devtools`); `agent-ios skills get agent-ios` prints the guide, and the discovery stub is `skills/agent-ios`. `skills path` installs them under `<user cache directory>/agent-ios/skills` (was `.../iwdp/skills`).
- The environment variables start with `AGENT_IOS_` instead of `IWDP_` (`AGENT_IOS_WS_URL`, `AGENT_IOS_SESSION`, `AGENT_IOS_TIMEOUT`, `AGENT_IOS_TRANSPORT`, `AGENT_IOS_PRIVATE`, `AGENT_IOS_NO_DAEMON`, `AGENT_IOS_DAEMON_IDLE`, `AGENT_IOS_DAEMON_DIR`, `AGENT_IOS_DEVICE_RESERVATION`, and the `AGENT_IOS_SIM_*` variables of scripts/sim-setup.sh). The start token in the environment of a started iwdp is `AGENT_IOS_START_TOKEN` (was `IWDP_CLI_START_TOKEN`).
- The daemon's directory is `$XDG_RUNTIME_DIR/agent-ios/` (else `$TMPDIR/agent-ios-<uid>/`; was `iwdp` and `iwdp-<uid>`), with its socket, locks, reservations, simulator forward and ssh connections. The record of a started iwdp, `agent-ios-started-<uid>-<address>.json` (was `iwdp-cli-started-...`), is in that directory too (was `$XDG_RUNTIME_DIR/iwdp-cli/`, else `$TMPDIR/iwdp-cli-<uid>/`), and its writer mark is `agent-ios`. The start lock is `agent-ios-start-<uid>-<address>.lock` (was `iwdp-cli-start-...`).
- The daemon log is `$XDG_STATE_HOME/agent-ios/daemon.log` (was `.../iwdp/daemon.log`), and the iwdp log is `$XDG_STATE_HOME/agent-ios/iwdp.log` (was `.../iwdp-cli/iwdp.log`); the fallback is `~/.local/state/agent-ios/`.
- The simulator settings are `$XDG_CONFIG_HOME/agent-ios/simulator.json` (was `.../iwdp-cli/simulator.json`), and the fallback directory of the shared ssh connection is `/tmp/agent-ios-ssh-<uid>` (was `/tmp/iwdp-ssh-<uid>`).
- Default output files are named `agent-ios-screenshot-<time>.png`, `agent-ios-trace-*.json` and `agent-ios-profile-*.cpuprofile` (were `iwdp-...`).
- The daemon socket path is now 5 bytes longer. A Unix socket path holds at most 107 bytes on Linux and 103 on macOS, so `XDG_RUNTIME_DIR` may be at most 85 bytes long on Linux and 81 on macOS (was 90 and 86); without it, `TMPDIR` (without a trailing `/`) may be at most 84 bytes on Linux and 80 on macOS less the digits of your uid (80 for uid 1000 on Linux, 77 for uid 501 on macOS; was 5 more), and `AGENT_IOS_DAEMON_DIR` at most 95 and 91. Over the limit, the daemon and the commands that need it exit with `daemon socket path too long: <path> is <n> bytes, over the limit of <limit> bytes ...`; set a shorter directory.

### What iwdp-cli and agent-ios do not share

agent-ios reads none of iwdp-cli's names, so the two programs share nothing that coordinates them: not the daemon, its page connections, sessions and leases; not the device and simulator reservations; not the host-wide start lock of the iwdp listing address; not the records of a started iwdp; and not the simulator's forward. An agent of each program can therefore reserve the same simulator or device and attach to the same page, each believing it holds it. agent-ios cannot verify or stop an iwdp that iwdp-cli started (`restart-iwdp` refuses it as another program's iwdp), and the current `scripts/iwdp-owner-check.sh` cannot check a record that iwdp-cli or the iwdp-cli version of `scripts/sim-setup.sh` wrote: it reports an `iwdp-cli-started-*.json` record as `cannot-check` and an `iwdp.pid` record of the old setup as `mismatch` (its token is in `IWDP_SIM_START_TOKEN`), and the new teardown keeps such a record and its process. Never run the two programs on one machine at the same time.

### Switching one machine

Run these steps in order. Steps 2 to 5 use the old program and the old scripts: `OLD` below is a checkout of the last iwdp-cli commit (the commit before the rename; for example `git worktree add <dir> <that commit>`), never a checkout of agent-ios.

1. **Let every turn end.** Start no new work with iwdp-cli, and wait until no agent is in a turn: `iwdp-cli device list` prints `No device is reserved.` (it lists the simulator's reservation too), and `iwdp-cli daemon status` lists no page with an active lease. Stopping the daemon earlier ends the sessions of the agents still at work.
2. **Stop the simulator's forward** when `iwdp-cli sim up` set the simulator up: `iwdp-cli sim down` stops the forward that `sim up` started and shuts the simulator down (`iwdp-cli sim down --mac` also runs the configuration's stop command for the Mac).
3. **Stop the iwdp that iwdp-cli started**, only when nobody else uses it. Its record is `$XDG_RUNTIME_DIR/iwdp-cli/iwdp-cli-started-<uid>-<address>.json` (else `$TMPDIR/iwdp-cli-<uid>/`; `:` and `/` of the address are `_`). Run `OLD/scripts/iwdp-owner-check.sh --terminate <record>`, then `OLD/scripts/iwdp-owner-check.sh <record>` until it reports `gone`, then `OLD/scripts/iwdp-owner-check.sh --forget <record>`. For any result other than `verified` and then `gone`, send no signal and ask whoever started it ([Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand)).
4. **Stop the daemon:** `iwdp-cli daemon stop`.
5. **End a setup of the simulator tests** (macOS, `make sim-setup` or `make test-e2e` of iwdp-cli) whose state is still there, `$XDG_RUNTIME_DIR/iwdp-sim-<uid>/` (else `$TMPDIR/iwdp-sim-<uid>/`) with a `lease` or an `iwdp.pid`: `OLD/scripts/sim-setup.sh --teardown`, only when no run of the simulator tests is active. The current setup uses the same directory, and its teardown cannot check that record (above).
6. **Move and delete the old files.** agent-ios creates its runtime directory itself.
   - Move the settings: `mv ~/.config/iwdp-cli ~/.config/agent-ios` (or `$XDG_CONFIG_HOME/...`).
   - The old logs are no longer written: `~/.local/state/iwdp/daemon.log` and `~/.local/state/iwdp-cli/iwdp.log` (`iwdp.log.1`). Keep a copy if you need them, then delete both directories.
   - Delete the old runtime files: `$XDG_RUNTIME_DIR/iwdp/` (else `$TMPDIR/iwdp-<uid>/`: the old socket, locks, reservations, forward and ssh sockets), `$XDG_RUNTIME_DIR/iwdp-cli-start-<uid>-*.lock` (else in `$TMPDIR`), `/tmp/iwdp-ssh-<uid>` and the old skills cache `<user cache directory>/iwdp/skills` (`~/.cache/iwdp/skills` on Linux).
   - Delete the records of both earlier generations only through the checker, after it reports `gone`: `iwdp-cli-started-*.json` in `$XDG_RUNTIME_DIR/iwdp-cli/` with `OLD/scripts/iwdp-owner-check.sh --forget <record>` (step 3), and `iwdp-mcp-started-*.json` in `$XDG_RUNTIME_DIR/iwdp-mcp/` (else `$TMPDIR/iwdp-mcp-<uid>/`) with `scripts/iwdp-owner-check.sh --forget <record>`, which still checks them ([Stopping an iwdp of an older version](#stopping-an-iwdp-of-an-older-version)). Then remove the empty directories `iwdp-cli` and `iwdp-mcp`. A record whose process still runs is the only proof of that process's identity: never delete it by hand.
   - Rename the `IWDP_` variables in shell profiles, agent configurations and scripts (above), and replace a link to the old skill stub (`~/.agents/skills/iwdp`) with one to `skills/agent-ios`.
7. **Recover an iwdp of the old setup.** When `OLD/scripts/sim-setup.sh --teardown` leaves the ios_webkit_debug_proxy that the old setup started running, or its record `iwdp-sim-<uid>/iwdp.pid` stays, check it with the old checker only: `OLD/scripts/iwdp-owner-check.sh --terminate <dir>/iwdp-sim-<uid>/iwdp.pid`, then `OLD/scripts/iwdp-owner-check.sh <record>` until `gone`, then `OLD/scripts/iwdp-owner-check.sh --forget <record>`, and run `OLD/scripts/sim-setup.sh --teardown` again. For any other result, send no signal and ask.
8. **Remove the old program** so that nothing calls it: delete the `iwdp-cli` binary (`rm "$(command -v iwdp-cli)"`; `go install` puts it in `$(go env GOPATH)/bin`), and remove the `OLD` checkout when steps 3, 5 and 7 are done. Then install agent-ios.

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

The MCP server (`iwdp-mcp`), its tool profiles (`IWDP_MCP_TOOLS`, `--tools`), the Claude Code plugin and the MCP skill `ios-safari-debug` are removed. Agents run `agent-ios` commands instead and read the built-in skill with `agent-ios skills get agent-ios`. Each former tool of a registry command is the registry command of the [command reference](commands.md) (for example `navigate` is `open`, `tab_select` is `tab`, `select_frame` is `frame`, `close_session` is `close`, `read_page` is `read`); the other former tools map like the removed commands below. Parameters are now flags: `full_page` is `--full-page`, `http_only` is `--http-only`, and a CSS selector is the target argument. A client that needs a protocol connection uses the CDP endpoint (`agent-ios get cdp-url`, [cdp.md](cdp.md)).

## CLI: removed commands and their registry commands

`agent-ios` runs only the registry commands of the [command reference](commands.md). Every command of earlier versions that is not in the registry is removed; a removed name is an unknown command (exit 2). A removed name that starts registry commands, such as `network`, lists them instead: `agent-ios network --help` prints the list (exit 0), `agent-ios network` prints it as a usage error (exit 2). The global options take the values of earlier versions: `--timeout`, `AGENT_IOS_TIMEOUT` and `--wait-lease` accept a duration (`5s`) or whole seconds (`5`). Every usage error exits 2, also an unknown `--flag` (for example `click body --bogus` or `console --follow`) and a bad global option. No command takes its page from a trailing `ws://` argument or from `AGENT_IOS_WS_URL` any more: a trailing page URL, a page URL as the screenshot file, and a page command while `AGENT_IOS_WS_URL` is set without `--page` are usage errors that name `--page`. Name the page with `--page <tab id or ws:// URL>`. `IWDP_PORT` is no longer read.

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
| `get-matched-styles`, `get-inline-styles` | `css matched <target>`, `css computed <target> [props...]` | read only: the matched rules with the cascade, inline and inherited styles, and the computed style ([guide.md](guide.md#devtools-extras)) |
| `set-style-text`, `get-all-stylesheets`, `get-stylesheet-text` | removed | CSS editing and style sheet text |
| `disable-cache`, `set-log-level` | removed | |
| `list-indexed-databases`, `get-indexed-db-data`, `clear-indexed-db-store` | removed | IndexedDB |
| `debugger-enable`, `debugger-disable`, `set-breakpoint`, `remove-breakpoint`, `pause`, `resume`, `step-over`, `step-into`, `step-out`, `get-script-source`, `search-in-content`, `eval-on-frame`, `set-pause-on-exceptions` | removed | the JavaScript debugger; to log a value at a script location without pausing, use `logpoint add <url>:<line> <expression>` |
| `set-dom-breakpoint`, `remove-dom-breakpoint`, `set-event-breakpoint`, `remove-event-breakpoint`, `set-url-breakpoint`, `remove-url-breakpoint` | removed | DOM, event and URL breakpoints |
| `heap-snapshot` | `heap snapshot [path]` | JavaScriptCore heap snapshot JSON plus a summary by class; `heap diff <a> <b>` compares two files |
| `heap-gc` | `heap gc` | |
| `memory-track`, `heap-track` | removed | memory and heap tracking |
| `animation-enable`, `animation-disable`, `animation-track`, `get-animation-effect`, `resolve-animation` | removed | animations |
| `canvas-enable`, `canvas-disable`, `get-canvas-content`, `start-canvas-recording`, `stop-canvas-recording`, `get-shader-source` | removed | canvas |
| `get-layer-tree`, `get-compositing-reasons` | removed | layer tree |
| `worker-enable`, `worker-disable`, `send-to-worker`, `get-service-worker-info` | removed | workers |
| `run-audit`, `get-certificate-info` | removed | audits and certificates |
| `browser-extensions-enable`, `browser-extensions-disable` | removed | browser extensions |
| none | `wait`, `dialog`, `back`, `forward`, `tab`, `frame`, `diff snapshot`, `diff screenshot`, `vitals`, `state save|load`, `network har start|stop`, `set media|useragent|geo|offline|credentials`, `addinitscript`, `get cdp-url` | new |

## Changes after 0.1.0

- **BREAKING: state names.** `state save`, `state load` and `state show` take a state name or a path. A name is 1 to 128 letters, digits, `_` and `-`, stored as `<name>.json` under `$XDG_STATE_HOME/agent-ios/states`; anything else is a path. So `state save login` now writes the named state `login`, where 0.1.0 wrote the file `./login`. Write `./login` or `login.json` for a file in the current directory.
- **keydown** follows agent-browser 0.45: keydown, then keypress and the insertion or default action unless the page cancels the keydown. 0.1.0 sent a bare keydown; `press <key> --phase down` still does.
- **diff snapshot** compares with the session's last snapshot of the page (with its options) or with `--baseline <file>`; 0.1.0 compared the last two snapshots.
- **Stale refs** fail with agent-browser 0.45's message "Ref @eN is from a document that is no longer loaded ..." (code `stale_ref` unchanged).
- **Form errors** start with a stable code (`read_only`, `disabled`, `option_not_found`, ...) and refuse at once instead of waiting for the action deadline ([guide.md](guide.md#the-agentic-loop)).

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
| `devices` | unchanged: one line per device (name, UDID, `(pages on <host:port>)`); `No devices connected.` for none | the listing's device entries as a JSON array (`[]` for none) | not running: `devices` starts iwdp as page commands do (decision D10); a failed start: `Error: devices: ios_webkit_debug_proxy is not running and starting it failed: ...` (exit 1); earlier it printed `Start it with: <command>` and started nothing. A listing owner that does not answer: `Error: devices: <how it failed>` (exit 5). On Linux an iwdp that agent-ios started and that lost its devices is restarted first (guide.md, the daemon); an empty listing while usbmuxd reports a USB device: `Error: devices: ios_webkit_debug_proxy lists no device, although usbmuxd reports <n> USB device(s). ...` (exit 5) |
| `restart-iwdp` | unchanged: `ios_webkit_debug_proxy restarted` | `{"restarted":true}` | a refusal or failure prints `Error: restart-iwdp: <message>`; it refuses records without the agent-ios mark (above) |

## For skills and prompts written for agent-browser

The registry commands keep agent-browser's names, flags and output wherever WIP allows (`open`, `snapshot -i`, `click @e3`, `fill`, `get text`, `is visible`, `find role button --name Save click`, `wait --load networkidle`, `--session`, `--json`, `--content-boundaries`). The differences: all input is synthetic ([synthetic-input.md](synthetic-input.md)); `set viewport` and `set device` fail; there are no browser contexts, PDF or downloads; physical devices need `device reserve`.

## Command lines, daemon status and daemon stop after the upgrade

- Help no longer hides a usage error. A line with an unknown `--flag` or an unknown command exits 2 also with `--help` or `-h`, and so does `help <unknown command>` (it exited 1 before). Only `agent-ios --help`, `-h` or `help` alone print the overview; options without a command (`agent-ios --json --help`) exit 2. An empty command line prints the overview on stderr and exits 2 (it exited 1).
- `--json=false`, `--content-boundaries=false` and `--takeover=false` now mean false. Before, a global boolean option ignored its value and was always true. A value that Go's `strconv.ParseBool` does not accept (`--json=yes`) exits 2.
- After that upgrade of iwdp-cli, `iwdp-cli daemon status` and `iwdp-cli daemon stop` reached the daemon that the previous release had started (protocol 1), also while it held leases, and other commands replaced that daemon when it was idle; when it was busy they told you to run `iwdp-cli daemon stop` when its sessions finished. agent-ios does not reach a daemon of iwdp-cli at all ([Rename to agent-ios](#rename-to-agent-ios)).
