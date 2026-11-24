# Design: agentic parity, CDP endpoint and direct transport for iwdp-cli

> **Note (2026-11-10):** the MCP server (commit f731071) and the old CLI commands (WS2b) are removed; iwdp-cli runs registry commands only. The parts of this design about the MCP server, its tools and the legacy commands are history.
> The daemon protocol parts of section 2.2 are history too. The endpoints `/v1/op`, `/v1/raw`, `/v1/events` and `/v1/lease` are removed, with the operation scopes and raw calls of the legacy commands and the event streams of MCP clients. `/v1/run` takes an absolute `deadline` instead of `timeoutMs`. `/v1/batch` was never built. The current protocol (version 2) is in internal/daemon/proto.go; internal/daemon/server.go routes its four endpoints: `GET /v1/hello`, `GET /v1/status`, `POST /v1/run` and `POST /v1/shutdown`.

Status: design for implementation, revised after the two design reviews of 2026-11-06 (section 7.10). Date: 2026-11-06. Base branch: `feat/agentic-parity` (b42bb20).
Scope: make iwdp-cli and iwdp-mcp fast, agentic (parity with agent-browser 0.41.2), CDP-compatible, and able to reach the iPhone without ios_webkit_debug_proxy (iwdp).
Targets: Safari 16.6 on iOS 16.7 (iPhone X, permanent test device) and current iOS 26/27.

Evidence used (paths are absolute so that workers can open them):

| Tag | Source |
|---|---|
| INV | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/iwdp-inventory.md (commands, tools, hazards A1-H2 with file:line) |
| MAP | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/cdp-wip-mapping.md (WIP capability per iOS version, protocol facts 1-15) |
| AB | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/agent-browser-surface.md (agent-browser surface, usage counts, ranked parity list) |
| AGG | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/aggregate.md (1403 findings from 300 runs, regenerated 2026-11-06) |
| LAT | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/latency.md (status PARTIAL) and latency/summary.tsv: per-call latency of 12,735 tool calls in 336 past sessions |
| SRC | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/sources/ (WebKit protocol JSON, Playwright webview backend, pymobiledevice3, mobilecli, agent-browser snapshot.rs) |
| SPIKE-A | Section 11.1 of this document (WebKitGTK MiniBrowser as a local target, run on this host) |
| SPIKE-B | Section 11.2 of this document (go-ios as a library) |

Words used in this document:
- **WIP**: WebKit Inspector Protocol, the protocol that Safari, iwdp and WebKitGTK speak.
- **CDP**: Chrome DevTools Protocol.
- **Page connection**: one inspector connection to one Safari tab. Safari allows one per tab; a new one takes the tab from the old one.
- **Target routing**: WIP wraps every page command in `Target.sendMessageToTarget` and every reply in `Target.dispatchMessageFromTarget` (CLAUDE.md).
- **Runtime**: the JavaScript that iwdp-cli injects into the page (section 3). Not the WIP `Runtime` domain; that one is written `Runtime.*`.
- **Ref**: an element handle such as `@e7` that a snapshot prints and later commands accept.
- **Lease**: exclusive right of one named session to drive one page (section 2.5).

## 0. Summary

1. A long-lived **daemon** owns every page connection. The CLI, the MCP server and the new CDP endpoint are clients of the daemon. With bounded deadlines and hang detection it removes the multi-minute stalls that caused most wasted time. It also removes the per-command setup (HTTP probe, page listing, WebSocket dial, Target wait, reconnect stall; its size is not measured yet) and makes leases, waits, captures and session state possible.
2. A **transport interface** hides how the daemon reaches a page: through iwdp (today), directly through usbmuxd and webinspectord with the go-ios library, or through WebKitGTK on Linux for local tests. Target routing, provisional-target swaps, deadlines and reconnects live once, above the transport.
3. An injected **agent runtime** (ES2017, runs on Safari 16.6) produces agent-browser-compatible snapshots with `@eN` refs and performs actions, waits, dialog overrides and screenshot annotation. WIP has no Input, Dialog, Emulation or Accessibility domain, so this runtime is the only way to reach parity. All synthetic events have `isTrusted=false`; the output says so.
4. A **command registry** describes each command once. The CLI parser, help text, MCP tool schema and daemon dispatch come from it, so a new command is a new file, and `--help` can never run a command.
5. A **CDP endpoint** in the daemon translates CDP to WIP plus runtime calls, so agent-browser (`--cdp`), chrome-devtools-mcp, Playwright (`connectOverCDP`) and Puppeteer (`connect`) can attach. Unknown methods are answered by the bridge and never forwarded, because unknown methods can wedge the page target (MAP fact 9).
6. Seven workstreams (W1 to W6, with W3 split) own disjoint files. W1 lands in two merges: W1a first lands the contracts (interfaces, operation scheduler, registry, fake WebKit server) and two contract proofs, so that the others can start in parallel; W1b adds the daemon. Section 7 gives sizes and the merge order, and 7.10 maps every review finding to its tests.
7. Spike A: WebKitGTK MiniBrowser works as a headless local WIP target under `broadwayd`. The existing `webkit.Client` ran `Runtime.evaluate` there unchanged (2 ms per call), and the spike reproduced Target routing, provisional-target swaps, page stealing, blocking `alert()`, and a reconnect failure after close. Spike B: go-ios v1.3.2 is usable as a library for its `ios` transport package only (usbmuxd, lockdown, TLS); its `ios/webinspector` package is not fit for us, and it raises the Go floor to 1.26.0 (section 11.2).

## 1. Current state and the problems to fix

### 1.1 How a command runs today

Every iwdp-cli command runs on `context.Background()` and does this (INV A1-A8):
1. HTTP probe of iwdp port 9221 (up to 5 s).
2. Sequential HTTP listing of devices, then pages per device (up to 5 s each).
3. WebSocket dial with gorilla's default 45 s handshake timeout.
4. Wait up to 100 ms for `Target.targetCreated`, then fall back to unwrapped (direct) mode, which iwdp cannot route (MAP fact 3).
5. One or two protocol calls with no deadline (INV B1-B3).
6. Close. No cleanup is sent (INV A10), so interception, breakpoints and paused requests stay in WebKit.

The MCP server keeps one connection but never reconnects (INV E6) and has no deadlines either (INV A2).

W0 (brief: /home/devkit/.pi/artifacts/retro/browser-checker-20261106/briefs/W0-quick-wins.md) is adding command deadlines, a default per-call timeout and a 5 s handshake timeout, bounded console and network capture, help without execution, cookie redaction and SameSite, eval exception text and `--await`, and viewport, full-page and element screenshots. This design builds on W0 and does not repeat it.

### 1.2 Measured cost (LAT, 336 past sessions)

| Command family | Calls | p50 | p90 | max | Note |
|---|---|---|---|---|---|
| iwdp-cli eval | 1025 | 1.2 s | 7.6 s | 1238 s | agent-browser eval: p50 0.2 s, p90 1.3 s |
| iwdp-cli navigate | 261 | 4.8 s | 11.9 s | 1837 s | returns on ack, not on load (AGG [144]) |
| iwdp-cli screenshot | 139 | 1.8 s | 8.9 s | 126 s | |
| iwdp-cli fill | 44 | 5.5 s | 30.5 s | 53 s | |
| all iwdp-cli | 2397 | 1.4 s | 8.9 s | 1837 s | 16205 s total, 42 timeouts (heuristic count) |

LAT reads the long tail as rare stalls, not normal per-command overhead, and states that the benefit of session reuse is a hypothesis until measured. The design therefore treats deadlines and hang recovery as the first fix and session reuse as the second, and section 8.5 measures both. The category "latency-hang" holds 200 findings and 36232 s of estimated wasted time (AGG; estimates overlap and are not additive). Six P0 findings are iwdp-cli or page-socket calls with no deadline that hung 13 to 30 minutes, and three P0 findings are pages that stayed stalled for minutes after a connection closed (AGG P0 [116], [165], [170]).

### 1.3 What agents asked for (AGG "Missing capabilities", AB section 2)

In order of demand: real taps and key presses (more than 25 findings), click by ref, snapshot with refs, waits, bounded console and errors, screenshots by element and viewport, cookies with SameSite (W0), box and visibility and hit test reads (more than 8 findings of hand-written probe scripts), request bodies, eval with `awaitPromise`, persistent session, history back, viewport and touch emulation.

Two of these cannot be fully delivered on WIP: trusted input (WIP has no Input domain, MAP 1.2) and viewport emulation (`Page.setScreenSizeOverride` is compiled only for non-Cocoa platforms, MAP 1.10). Section 4.3 lists every impossible item with its reason and the closest substitute.


## 2. Architecture

### 2.1 Process model

```
 iwdp-cli (one process per command)   iwdp-mcp (stdio, one per agent)   CDP clients (agent-browser --cdp,
        |                                   |                          Playwright, Puppeteer, chrome-devtools-mcp)
        |  HTTP/1.1 + NDJSON over a Unix socket                        |  HTTP + WebSocket on 127.0.0.1:<cdp-port>
        v                                   v                          v
 +-------------------------------------------------------------------------------------+
 | iwdp daemon (one per user)                                                          |
 |  command registry -> core.Manager (devices, pages, sessions, leases, captures)      |
 |  cdp.Server (CDP <-> WIP + runtime)       devtools (netlog, HAR, routes, traces)    |
 |  core.PageSession: webkit.Session (Target routing, swaps, deadlines, reconnect)     |
 |                    runtime.Runtime (injected JS), refs.Table, desired state         |
 +-------------------------------------------------------------------------------------+
        |  transport.Transport
        +--> iwdp       (HTTP listing on 9221/9224+, WebSocket per page)          default today
        +--> direct     (usbmuxd -> lockdown -> com.apple.webinspector, go-ios)   W2, opt-in first
        +--> webkitgtk  (WEBKIT_INSPECTOR_HTTP_SERVER listing, WebSocket)         local tests (spike A)
```

Decisions:
- **One daemon per user.** It is the only process that opens page connections. Two processes that each open a page connection take the page from each other (MAP fact 1; reproduced in spike A, section 11.1 step C).
- **The daemon is the same binary.** Both `iwdp-cli` and `iwdp-mcp` contain `internal/daemon` and start it as `<own executable> daemon run`. No third binary to install.
- **The MCP server is a daemon client**, not an in-process owner. Reason: the same agent often uses the MCP server and the CLI, and both must share leases and refs. The extra hop is a Unix socket round trip (well under 1 ms), small next to a WIP call. Decision D1 (section 10).
- **No-daemon mode stays.** `IWDP_NO_DAEMON=1` runs the same core in-process for one command, for debugging and for the unit tests. It gives no leases across processes.

### 2.2 Local socket API

Socket: `$XDG_RUNTIME_DIR/iwdp/daemon.sock`, else `$TMPDIR/iwdp-$UID/daemon.sock`. The directory is created with mode 0700. At every start the daemon checks it with `lstat`: it must not be a symlink, must be owned by the current uid and must have mode exactly 0700; otherwise the daemon refuses to start (R16). Protocol: HTTP/1.1 on the Unix socket (Go `net/http`; easy to debug with `curl --unix-socket`). Streams are NDJSON with chunked encoding.

| Method and path | Body | Answer |
|---|---|---|
| GET /v1/hello | none | `{version, protocol, pid, startedAt, transports[]}` |
| POST /v1/run | `{command, args[], flags{}, session, page, timeoutMs, cwd, json}` | NDJSON: `{"type":"line"}` and `{"type":"event"}` items, then exactly one `{"type":"result"}` or `{"type":"error","code","message","hint"}` |
| POST /v1/batch | `{commands:[...], bail}` | NDJSON of results in order (agent-browser `batch`) |
| POST /v1/op | `{op: begin\|end, session, page, class, timeoutMs}` | begin: `{opId, epoch, deadline}`; opens the operation scope of a legacy multi-send command (2.10) |
| POST /v1/raw | `{session, page, opId, method, params}` | `{result}` or `{error}`; one protocol call inside an operation scope; used by legacy commands (2.10) |
| GET /v1/events | query `session, page, methods` | NDJSON event stream until the client closes |
| POST /v1/lease | `{op: acquire, release, break, renew, takeover, session, page, waitMs}` | lease state, including `lost` (2.5) |
| GET /v1/status | none | devices, pages, sessions, leases, captures, CDP listener, transport health |
| POST /v1/shutdown | `{force}` | ok; graceful cleanup first (2.4) |

`protocol` is an integer. A client with a different protocol number asks the daemon to exit when the daemon has no leases, and starts its own version. If leases exist, the client fails with: "daemon version X (protocol N) is running with active sessions; run 'iwdp-cli daemon stop' when they finish". The CLI sends `cwd` so that relative output paths resolve in the caller's directory, never in the daemon's.

### 2.3 Auto-start, lifecycle and idle exit

- Start: a client connects to the socket. On ENOENT or ECONNREFUSED it takes an exclusive `flock` on `daemon.lock` in the same directory, checks again, then runs `<own executable> daemon run` with `Setsid`, stdin closed and stdout and stderr appended to `$XDG_STATE_HOME/iwdp/daemon.log` (fallback `~/.local/state/iwdp/daemon.log`). It polls the socket every 20 ms for up to 3 s. This matches agent-browser's auto-start.
- Commands: `iwdp-cli daemon run|start|stop|status|logs`.
- Idle exit: the daemon exits after 15 minutes with no requests, no leases, no running captures and no CDP clients (`IWDP_DAEMON_IDLE` overrides).
- Page connections that have no lease and no capture close after 10 minutes, so that other tools (for example Safari Web Inspector on a Mac) can take the page.
- SIGTERM, `daemon stop` and idle exit run the cleanup of section 2.4 for every page, with a 5 s total budget.

### 2.4 Desired state, cleanup and release

Today nothing undoes WebKit state (INV A10, D8, D10, F2). The daemon keeps a **desired-state model** per page session (`core.DesiredState`), shared by every entry point (2.10); it is not a history of sends. Each entry has an owner (a session, a capture, a legacy operation, the dialog bridge or a CDP attachment of 5.2) and a reset rule. Reconnect and target swaps re-apply the desired state (2.7); release removes the entries of the releasing owner and applies the result.

| State | Owner examples | On release of the last owner |
|---|---|---|
| Domain enablement (`Network`, `Console`, `Page`, `Runtime`, `Debugger`) | netlog, legacy `NetworkMonitor`, CDP session, dialog bridge (`Console`, 3.7) | reference count per domain; the disable command is sent only when the count reaches zero, except inside the Network reinitialization barrier below |
| Network interception and paused requests | `network route`, CDP `Fetch` | continue every paused request, `Network.setInterceptionEnabled false` |
| Debugger enabled, breakpoints, paused | legacy debugger tools | `Debugger.resume`, remove breakpoints, `Debugger.disable` |
| Forced pseudo state (hover) | `hover --css` | `CSS.forcePseudoState` with an empty list |
| Overlay elements (annotate, highlight) | screenshot, highlight | runtime `overlay.clear`, `DOM.hideHighlight` |
| User-agent, media and preference overrides | `set ...` commands, CDP | explicit reset (WebKit also resets them on disconnect, MAP fact 13) |
| Extra HTTP headers | `set headers`, legacy `Network.setExtraHTTPHeaders`, CDP `Network.setExtraHTTPHeaders` | each owner's call replaces that owner's map, and the effective map is the union of the owners' maps; a new name or a changed value is sent as `Network.setExtraHTTPHeaders` with the complete effective map; a name that loses its last owner is removed by the Network reinitialization barrier below; with no Network owner left, `Network.disable` alone clears the map |
| Bootstrap parts (runtime, dialog override, init scripts, shims) | session, W4 | `Page.setBootstrapScript` with the remaining parts, or empty |
| Page-side runtime state (dialog overrides, pending answers, hooks, timers, ref maps) | session | runtime `__iwdp.release(epoch)` restores the original `alert`, `confirm`, `prompt`, `onerror` hooks and `fetch`/XHR patches, clears pending answers, timers and ref maps (3.3, 3.7) |

**Network reinitialization barrier.** WIP `Network.setExtraHTTPHeaders` merges the given names into the backend's map and never removes a name. Only `Network.disable` clears the map, and it also clears interception rules, stored response bodies, the resource-caching override and emulated conditions, and continues every paused request with its original content (SRC/webkit-src/InspectorNetworkAgent.7615.3.12.11.2.cpp lines 896-964). A smaller map, or `{}`, therefore leaves a removed header in place, and an empty value is a value, not a removal. When a header name loses its last owner while other Network owners remain, the releasing operation runs one exclusive sequence on the page:
1. Close admission of new operations on the page (barrier, 2.10).
2. Continue every paused request unmodified through the control lane (2.10); a later completion for such a request fails with "request was released by a network reset".
3. `Network.disable`, then `Network.enable`.
4. Apply the complete remaining Network state: the effective header map, interception enablement and rules, the resource-caching override and emulated conditions.
5. Reopen admission.

The barrier reports the discontinuity to every remaining Network owner: the netlog records a gap marker with its start and end time, requests in flight across the reset are closed as `capture_reset`, response bodies of earlier requests can no longer be read (`network request --body` says so), and CDP owners receive a `Log.entryAdded` warning. The barrier is used on every WebKit version until the capability probe of 8.3 shows that a version replaces the map instead of merging it.

Release runs only on a connection the daemon still owns. After an unexpected disconnect (2.5) the daemon does not reconnect just to clean up; it records that cleanup is pending and the page may still carry page-side overrides until it navigates. Lease release does not restore authentication state or undo submitted forms or transactions; the docs say so (R17).

### 2.5 Sessions, leases, device ownership and arbitration

Sessions and leases inside the daemon:
- A **session** is a name chosen by the client: `--session <name>` or `IWDP_SESSION`; default `default` (agent-browser rule; decision D8). The shared default is a compatibility default, not isolation: agent and MCP workflows must set distinct stable names. Sharing a session is intentional only when the CLI and the MCP server use the same name, or when a CDP client connects through an endpoint issued for that name (5.2).
- A **lease** binds one page to one session and carries an **epoch** (a counter that increases on each grant). Every operation carries the epoch it was admitted under; an operation with an old epoch is rejected before it sends anything. Release: `iwdp-cli close` (this session only), `session release`, idle TTL of 5 minutes without commands and without captures, or `lease break`.
- A page command from another session on a leased page fails at once with: `page "<title>" is leased by session "<name>" since <time> (last command <age> ago); use --session <name>, wait, or run 'iwdp-cli lease break <page>'`. `--wait-lease <duration>` waits, inside the command deadline (2.6).

Device ownership (outside the daemon). The daemon cannot see every inspector client: another agent's raw script, an older iwdp-cli, or Safari Web Inspector on a Mac can hold a page, and iwdp takes a page from its current owner when a new client attaches (ios_webkit_debug_proxy.c lines 1202-1235). Rules:
1. **No page connection to a physical device without a reservation.** `iwdp-cli device reserve [--device d] --owner <name> [--ttl 2h]` writes a cooperative reservation (flock-protected JSON in `$XDG_RUNTIME_DIR/iwdp/reservations/`, honored by every iwdp-cli and iwdp-mcp of this version, renewed while leases are active). It fails if another live reservation exists. `IWDP_DEVICE_RESERVATION=<owner>` lets an orchestrator create the reservation once and pass it to workers. Local targets (WebKitGTK, simulators) need no reservation.
2. **Preflight before every first attach.** The daemon checks the reservation, then a non-disruptive busy hint where one exists: iwdp lists `devtoolsFrontendUrl` as empty for a page that already has a WebSocket client (ios_webkit_debug_proxy.c lines 1854-1863), but only when iwdp runs with its frontend; with `-F` or `--no-frontend` (how `proxy.Start` starts it) the hint is always empty and ownership is **unknown**. A busy hint fails the attach; an unknown state is accepted only inside a reservation. Hints are not locks and do not protect against clients that ignore reservations; the docs say so.
3. **Unexpected disconnect means ownership unknown.** When a page connection closes without a request from the daemon, the lease moves to `lost`. The daemon never reconnects by itself, and the next ordinary command fails with "connection to page was closed by someone else or by the device; ownership is unknown". Only `--takeover` (or the `lease takeover` command) reattaches, and the docs require the device owner's approval for it (the orchestrator gates device sessions; decisions.md).
4. **Scope.** The CDP endpoint and `tab` discovery list pages only of reserved devices, and CDP discovery and auto-attach are limited to the selected page of the endpoint record unless the record was issued with `--scope device` (5.2).

Scheduling inside one session: every operation (registry command, legacy raw operation, CDP message) enters the per-page scheduler of 2.10. Mutating operations, ref allocation included, run one at a time in FIFO order and exclude reads; read-only operations run in parallel with at most 8 protocol calls in flight per page (MAP fact 8). Control-plane messages that let a running operation finish do not wait in this queue; 2.10 defines them.

### 2.6 Deadlines, cancellation, heartbeats and hang detection

**One absolute deadline per operation.** The command deadline (W0: default 30 s, `--timeout`, `IWDP_TIMEOUT`; waits default 25 s like agent-browser) is fixed when the request arrives and covers queueing, lease wait, discovery, connect, probes, re-applying desired state, the calls themselves and any retry. Every step gets `min(step budget, remaining time)`. When it expires the command returns at once with exit code 124 (like `timeout(1)`; decision D9); nothing after that point is charged to the caller.

| Step budget | Default | Reason |
|---|---|---|
| One protocol call | 10 s | |
| WebSocket handshake | 5 s (W0) | gorilla default is 45 s (INV A7) |
| Wait for `Target.targetCreated` | 3 s; 12 s right after a close | reconnect stall about 10 s (MAP fact 2) |
| Liveness probe | 2 s, `Runtime.evaluate "1"` | mobilecli uses 2 s |
| Reply nudge | at 250 ms | iOS 26 holds replies until the next message (MAP fact 7) |
| Runtime attempt (cancellation, below) | 250 ms, at most the remaining time | bounds how long a canceled action can still dispatch |
| Background recovery (see below) | 15 s, once | |
| Lease idle TTL | 5 min | |

No direct-mode fallback: the transport states whether the endpoint uses Target routing (iwdp, WebKitGTK and webinspectord all do). A connection that closes or stays silent without `Target.targetCreated` is a failed attempt (spike A step B saw exactly this on alternate reconnects); one more attempt is made if time remains, then the connect fails with "page did not announce its target; it may be held by another inspector or not visible". The 100 ms fallback stays only for the legacy mock server (MAP fact 3).

Hang detection for one call:
1. At +250 ms with no reply, send one nudge (`Runtime.evaluate "0"`, reply ignored) when the device is iOS 26 or later or its version is unknown.
2. At the call budget, run the probe within the remaining time. Probe answers: `ErrCallTimeout` ("WebKit accepted the command and did not answer; the page is alive"); the session stays.
3. Probe fails: mark the session `stalled` and return `ErrSessionStalled` to the caller within its deadline. A **background recovery** owned by the page session (not by the command) then reconnects once, bounded at 15 s, only if the lease is still valid, not `lost`, and the page passes the preflight of 2.5; it re-applies desired state. It never runs for a lost page.
4. A hidden tab accepts commands and never answers (MAP fact 12). Before the first command on a page and after 20 s idle, the daemon runs the probe. A failed probe on a fresh connection gives: "tab is not answering; it is probably not the visible Safari tab. Bring it to the front on the device."
5. Every write has a write deadline (fixes INV B2).

**Retries.** Registry commands have `RetrySafe bool` (default false). Only retry-safe commands (pure reads such as `get url`, `snapshot` without new refs, `tab list`) are retried after `ErrSessionStalled` or `ErrTargetSwapped`, and only inside the deadline. `eval`, actions, navigation and every legacy raw operation are never retried automatically; their error says whether the command may have run ("the page may have executed the command before the connection failed").

**Cancellation.** A client that disconnects from `/v1/run` (Ctrl+C, killed process, MCP cancel) cancels the operation in the daemon at once. Go-side cancellation does not stop page-side work, and a cancel message to the page can be lost, so the guarantee rests on what the daemon sends, not on what the page learns:
1. **Steps.** A side-effecting action is an ordered list of **steps**, each with an ordinal: one click or tap; one character of `type --delay` (without `--delay`, one step may type several characters, and its reply says how many); one increment of `drag` or `mouse move --steps`. Single-step actions have one step.
2. **Daemon-owned retry loops.** Every wait for actionability (3.5) is a loop in the daemon. Each **attempt** is one runtime call with `{opId, epoch, step, attemptEnd, deadline}` (3.1) for the current step. It checks readiness once and, only when every check passes, dispatches that step's side effects (events, a value, a submit) inside the same call; otherwise it returns `retry` with the reason and has changed nothing. The events of one step may run in separate tasks inside the attempt (3.5), but page code never dispatches after the attempt end, after the operation deadline, or after the call has settled, and it does nothing for a message that arrives after its attempt end. The page records the last step ordinal it ran for the operation and refuses any other ordinal than the next one, so a step can never run twice.
3. **Attempts only while active.** The daemon sends an attempt only while the operation is active: not canceled, lease epoch current, deadline not passed. A canceled operation gets no new attempt, whether or not the page ever hears of the cancel.
4. **At most one dispatch per step; advance only on acknowledgment.** Readiness attempts for a step repeat until the step dispatches. The operation advances to the next step only after the reply that acknowledges the dispatch of the current one. When an attempt's reply is missing, or the operation is canceled or expires, the whole sequence stops: the ambiguous step is not retried and no later step is sent. The error names the step ("the page may have run step 2 of 3 of type before the reply was lost"), and the result lists the acknowledged steps. A step that its attempt end interrupts after some of its events is reported as interrupted and also stops the sequence.
5. **Ambiguous after dispatch.** A cancel while an attempt is in flight does not prove that the attempt did nothing. The canceled client is answered at once with "canceled; outcome unknown" when an attempt was in flight. The operation keeps the page's mutation slot (2.10) for its whole sequence and then until the in-flight attempt's reply arrives (the daemon log records what happened) or its slot-release boundary passes (clock bounds below), so a conflicting successor waits at most the attempt budget.
6. **Best-effort page cancel.** The daemon also sends `__iwdp.cancel(opId)` (1 s budget) on the control lane (2.10), so that an in-flight attempt or wait slice (3.6) ends early. Losing this message changes only how soon a slice ends, never whether a side effect happens.

**Clock bounds.** Fencing uses monotonic clocks only: the daemon's Go monotonic clock and the document's `performance.now()`, captured at install with the other built-ins (3.1). `Date.now()` is never used, so a change of the device or host wall clock has no effect. Assumption: the page clock runs at a rate within `rho = 100 ppm` (0.0001) of the daemon clock. This is a stated operating assumption, not a measured device fact; the device suite records the observed rate (8.3).
- **Mapping.** The mapping between the clocks is measured per document: the daemon reads its clock at send (`h0`) and at reply (`h1`) of a runtime call that returns the page's `performance.now()` value `p`. The page read `p` at some daemon instant between `h0` and `h1`. Every attempt reply carries a fresh `p`. A new document has a new `performance` time origin, so its mapping is measured at its runtime install, and an attempt addressed to another document does nothing.
- **Projection.** For a future daemon instant `T` at or after `h1`, the earliest page-clock value that the page can show at `T` is `pageMin(T) = p + (1 - rho) * (T - h1)`, which equals `T - (h1 - p) - rho * (T - h1)`: `T` minus the worst-case offset `h1 - p` and minus the drift allowance through `T`, not only through the mapping's current age. Every measured mapping of the document gives a valid lower bound; the daemon uses the largest one.
- **Slot-release boundary.** For an attempt sent at daemon time `s`, the boundary is `H = min(s + 250 ms, D)`, where `D` is the operation deadline on the daemon clock. The daemon admits a successor only after the attempt's reply or, without a reply, at `H` at the earliest.
- **Page expiry.** The attempt carries `attemptEnd = pageMin(H)` and `deadline = pageMin(D)`, each projected through its own instant. The page dispatches only while `performance.now() < attemptEnd` and `performance.now() < deadline`, checked before each event; at equality it refuses. Because the page clock shows at least `pageMin(H)` at daemon time `H`, the page refuses from `H` on, whatever the true offset and rate within `rho`. Uncertainty and drift only shorten the page window; positive slack is never added.
- **Refresh or fail closed.** The uncertainty of a mapping at `T` is the width of the possible page values, `(h1 - h0) + rho * (2T - h0 - h1)`. When it exceeds 100 ms at `H`, the daemon first measures again with one runtime call. When it still exceeds 100 ms, the attempt is not sent, and the action fails with "page clock uncertainty is N ms; the page is too slow to fence actions safely". This limit is an availability policy; safety comes from the projection above.

The resulting guarantee: after the daemon has canceled an operation, or its deadline has passed, no new attempt starts, no later step is sent, and the page can dispatch nothing from the slot-release boundary of the attempt that was in flight on; the successor is admitted no earlier than that boundary.

Heartbeat: there is no periodic traffic to the phone. MCP clients keep one `/v1/events` stream open; the daemon renews their leases while it lives and ends it with a final event on shutdown.

### 2.7 Provisional-target swaps and document generations

WebKit swaps processes on cross-origin navigation: `Target.targetCreated{isProvisional:true}`, then `Target.didCommitProvisionalTarget{oldTargetId,newTargetId}`, then `Target.targetDestroyed{old}` (reproduced in spike A step A). The bootstrap script runs when a new global object is created, not afterwards (Page.json 16.6 lines 227-230; InspectorPageAgent.cpp lines 1031-1039), so setup at commit is too late. `webkit.Session` therefore keeps two kinds of target:
1. **Active target**: receives command traffic.
2. **Setup target**: on a provisional `targetCreated` the session sends the desired state (2.4) to the provisional target id at once (`Page.enable`, `Page.setBootstrapScript` with all parts, `Runtime.enable` when CDP needs it, `Console.enable` and `Network.enable` while an owner holds them, for example a capture or the dialog bridge of 3.7, interception rules, overrides), as Playwright's webview driver does (wvPage.ts lines 217-250, 306-312). At commit the initialized target becomes active; calls still waiting on the old target fail with `ErrTargetSwapped`. An abandoned provisional target (destroyed without commit) is dropped.
3. **Pause-on-start policy**: when the session has early-install parts (dialog override, init scripts, interception), the session sets `Target.setPauseOnStart(true)` and sends `Target.resume` for each paused target after its setup or after 2 s, on every path (error, cancel, release, close). Without such parts it is off. **"Runs before page scripts" requires pause-on-start and is partial without it.** Proof P1 (WebKitGTK 2.54, 2026-11-06) ran the bootstrap part before the first inline script in 10 of 10 cross-site swaps with pause-on-start and in only 4 of 10 without it, because the setup races the new document. Playwright also reports that iOS 26 sometimes delivers an unpaused provisional target. The command help and the CDP docs therefore label "runs before page scripts" as partial when pause-on-start is off, and as best effort with it on.
4. On `targetDestroyed` of the active target with no swap: `ErrPageClosed`.
5. **Generation**: a document is identified by (target id, main-frame loader id). Commit and main-frame `Page.frameNavigated` for the same document increase the generation once. Same-document navigations do not.

### 2.8 Transport interface

Package `internal/transport` (W1 defines it; W1 implements `iwdp` and `webkitgtk`; W2 implements `direct`):

```go
type Kind string // "iwdp", "direct", "webkitgtk"

type Device struct {
	ID        string // UDID or synthetic id; never printed in full by default (AGG [287], [333])
	Label     string // short label for output, for example "iPhone X …a1b2"
	Name      string
	OSVersion string // "16.7.10"; "" when unknown. iwdp lists it as deviceOSVersion.
	Transport Kind
}

type Page struct {
	DeviceID   string
	ID         string // transport-local id, stable while the tab lives
	Title, URL string
	AppID      string // for example "com.apple.mobilesafari"
	Debuggable bool
}

type Transport interface {
	Kind() Kind
	Devices(ctx context.Context) ([]Device, error)
	Pages(ctx context.Context, deviceID string) ([]Page, error)
	Open(ctx context.Context, p Page) (MessageConn, error)
	Close() error
}

// MessageConn carries WIP JSON text messages for one page connection.
// It knows nothing about Target routing; webkit.Session does that.
type MessageConn interface {
	Write(ctx context.Context, msg []byte) error // honours the ctx deadline
	Read(ctx context.Context) ([]byte, error)    // one message; error after close
	Close() error
	Limits() Limits
}

type Limits struct {
	MaxOutgoing int  // largest message the far side accepts, in serialized wire bytes: WebKitGTK about 128 KiB (spike A), iwdp 64 MiB, direct 16 MiB
	MaxIncoming int  // read limit: 512 MiB today (client.go); direct: frame cap, default 256 MiB (section 6)
	Routed      bool // endpoint uses Target routing
}
```

Transport selection: `--transport auto|iwdp|direct|webkitgtk` or `IWDP_TRANSPORT`. `auto` uses iwdp when it is running (this keeps today's behavior and never competes with another agent that uses iwdp), otherwise `direct` when the device and its iOS version are supported (section 11.2), otherwise it starts iwdp as today (`proxy.Start`). Device ids are the same UDIDs in iwdp and direct, so a lease survives a transport change.

### 2.9 Layers inside the daemon

```go
// internal/webkit (W1): Target routing, ids, swaps, deadlines on top of one MessageConn.
type Caller interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
	OnEvent(method string, h EventHandler)        // kept for the existing collectors
	Subscribe(method string, h EventHandler) func() // returns unsubscribe (fixes INV E4)
	IsTargetRouted() bool
	Done() <-chan struct{}
}

func NewSession(ctx context.Context, c transport.MessageConn, o SessionOptions) (*Session, error)
func (s *Session) Generation() uint64
func (s *Session) Probe(ctx context.Context) error

// internal/core (W1): devices, pages, sessions, leases, page sessions.
type PageSession struct {
	Caller  webkit.Caller
	Device  transport.Device
	Page    transport.Page
	Runtime *runtime.Runtime // W3a
	Refs    *refs.Table      // W3a
	Sched   *Scheduler       // operations of every entry point (2.10)
	State   *DesiredState    // owned entries (2.4); sent to new and provisional targets, reduced on release
	Boot    *BootstrapParts  // named parts joined into one Page.setBootstrapScript
}
```

`webkit.Client` and `webkit.NewClient(ctx, wsURL)` stay as a thin wrapper (a WebSocket `MessageConn` plus a `Session`) so that existing tests and tools keep compiling. `internal/tools` functions change their parameter from `*webkit.Client` to `webkit.Caller` (mechanical change, W1). The signature change alone does not make legacy calls safe on a shared connection; the operation scope and desired-state mapping of 2.10 do.

Bootstrap script: WIP has one `Page.setBootstrapScript` per session, and it runs only while the Page agent is enabled (MAP fact 13). `BootstrapParts` joins named parts (runtime from W3a, dialog override from W3b, user init scripts and the geolocation shim from W4) in a fixed order, each wrapped in `try { } catch (e) { }` so that one failing part does not stop the others.

### 2.10 One operation boundary for the CLI, the MCP server, legacy calls and CDP

All entry points become **operations** in one manager-owned scheduler per page (`core.Scheduler`, W1). An operation has a class (read, mutate, exclusive), the lease epoch, the absolute deadline (2.6) and an owner for any state it sets (2.4).
- **Registry commands** declare their class (`Mutates`, `RetrySafe`).
- **Legacy commands** (the 105 CLI commands and 116 MCP tools that keep their code) get a daemon-backed `webkit.Caller`. Each legacy command opens an operation scope (`POST /v1/op` begin and end; raw calls carry the op id), so a multi-send sequence runs as one exclusive operation. Raw calls go through the same gates as everything else: methods not in the device's iOS command list are refused (5.4), the CSS hang methods are refused, and state-changing methods (`X.enable`, `X.disable`, `Network.setExtraHTTPHeaders`, interception, overrides, `Page.setBootstrapScript`) are mapped onto the desired-state model with the legacy command as owner, instead of being forwarded blindly. Example: legacy `NetworkMonitor.Stop` sends `Network.disable` (internal/tools/network.go lines 152-157); the daemon turns it into "legacy owner releases Network", so a netlog capture or a CDP client that still needs Network keeps it, and the backend's clearing of interception and headers (InspectorNetworkAgent.cpp lines 896-909) does not happen under them.
- **CDP messages** are operations owned by the CDP attachment and its logical session (5.2). The bridge reads every CDP connection without waiting for earlier replies and hands each message to the scheduler at once, so a client's `Fetch.continueRequest` is seen while its own `Runtime.evaluate` waits.
- **Control lane.** An operation that waits for page progress (an awaited evaluation, a wait slice, a runtime attempt, a call made while JavaScript is paused, a request held by interception) keeps its slot until it ends or its deadline passes, and ordinary operations stay queued behind it. The messages that let it finish must not queue behind it: an awaited `fetch` held by CDP `Fetch` or a `network route` finishes only after the request is continued, fulfilled or failed, and an evaluation stopped at a breakpoint finishes only after `Debugger.resume`. These messages use a separate control lane that never waits behind ordinary operations. It admits only:
  1. cancellation: removal of a queued operation and `__iwdp.cancel(opId)` (2.6);
  2. completion of a held request: CDP `Fetch.continueRequest`, `fulfillRequest` and `failRequest`, route answers of W4 and of legacy interception commands, sent as WIP `Network.interceptContinue`, `interceptWithRequest`, `interceptWithResponse`, `interceptRequestWithResponse` or `interceptRequestWithError`;
  3. `Debugger.resume` while the page is paused, and `Target.resume` for a target paused on start (2.7);
  4. the daemon's own probe and nudge (2.6) and the resume and continue steps of barriers.

  Every control-lane message is validated before it is sent: the lease epoch is current; the sender acts for the logical session that holds the lease (a CDP sender through its endpoint record, 5.2); and the message names an open pending item that the daemon tracks: a request id from `Network.requestIntercepted` or `responseIntercepted` that is not yet completed, the current `Debugger.paused` state, a target paused on start, or a running operation id. Each pending item is completed at most once. A message that fails validation is refused with an error and never sent; a message for any other method is an ordinary operation and queues. The control lane therefore cannot start new work.
- **Barriers.** Release, `lease break`, `--takeover`, background recovery (2.6) and the Network reinitialization barrier (2.4) first close admission: new ordinary operations wait, and fail at their deadline or with the lease error when the lease ends. Release and `lease break` cancel the running operation (2.6); recovery waits for it; the Network barrier runs inside the releasing operation, which already holds the slot. The barrier then sends its own resume and continue steps through the control lane, runs, and reopens admission or ends the lease.
- The CLI parses with the registry spec (help and unknown flags handled before any connection), sends `/v1/run`, prints the result; exit codes: 0 ok, 1 command error, 2 usage, 124 timeout (decision D9), 4 page busy or lost, 5 no device or page, 6 reservation missing.
- The MCP server registers one tool per registry command with an `MCPName` in the selected profile (decision D2) from a deduplicated registration manifest: when a registry command reuses an existing tool name, the legacy handler of that name is not registered.

### 2.11 Command registry

Package `internal/commands` (W1 owns `registry.go`, `spec.go`, `cli.go`, `mcp.go`; every command is its own file owned by one workstream):

```go
type Command struct {
	Name     string   // CLI path, for example "click" or "network requests"
	Aliases  []string // for example "goto", "navigate" for "open"
	MCPName  string   // "" = not an MCP tool
	Profile  string   // MCP profile: "core", "devtools", "legacy"
	Summary  string
	Args     []Arg    // positional
	Flags    []Flag   // typed: string, int, bool, duration, enum, file path
	Page     PageNeed // NoPage, AnyPage, LeasedPage
	Mutates  bool     // serialized per page when true; ref allocation counts as a mutation
	RetrySafe bool    // default false; only pure reads may be retried (2.6)
	Timeout  time.Duration
	Output   OutputKind // text, json, file, stream
	Run      func(ctx context.Context, env Env, in Input) (Result, error)
}

type Env interface {
	Page(ctx context.Context) (*core.PageSession, error) // resolves, leases, probes
	Manager() *core.Manager
	Emit(line string)        // streaming output (console --follow)
	ResolvePath(p string) string // relative to the caller's cwd
}

func Register(c *Command) // called from init() in each command file
```

Rules: a new command is a new file with an `init()` that calls `Register`. Tests in `internal/commands` check that names and aliases are unique, that every flag has help text, that `--help` never calls `Run`, and that MCP schemas pass `codexCompatibleInputSchema`. Global flags (`--session`, `--page`, `--timeout`, `--json`, `--transport`, `--max-output`, `--content-boundaries`) are parsed once in `cli.go`.

`--content-boundaries` wraps page-derived text in markers, and `--max-output` truncates it, as agent-browser does (AB 1.15). URL fragments and query values are not printed in connection lines (AGG [266]).


## 3. Agent runtime (injected JavaScript)

### 3.1 Packaging, size budget and install

- Source: `internal/runtime/js/NN-name.js`, hand-written **ES2017** (no optional chaining, no `??`, no class fields, no `Array.prototype.at`, no `structuredClone`; Safari 16.6 supports more, but ES2017 is the agreed floor). `go:embed js/*.js` joins the files in name order, so W3b adds files without editing a manifest: `10-core.js`, `20-visibility.js`, `30-aria.js`, `40-snapshot.js`, `45-locate.js` (W3a); `50-actionability.js`, `55-actions.js`, `60-keyboard.js`, `70-waits.js`, `80-dialogs.js`, `90-overlay.js` (W3b).
- Size budget: the joined bundle must stay **at or below 64 KiB** of source, and, separately, every serialized outer message (install, bootstrap with all parts, init scripts, upload envelopes) is checked against `Limits.MaxOutgoing` on its real wire bytes before it is written, with the error "bootstrap is N bytes on the wire; the limit for this transport is M". Tests use escape-heavy content (quotes, backslashes, control characters), because JSON-in-JSON escaping can more than double the size. Reason: the whole bundle goes in one `Runtime.evaluate`, and WebKitGTK closes the connection for any message above about 128 KiB (spike A step D: 127 KiB passed, 130 KiB closed the socket); Target routing escapes the payload a second time. Chunked install through page `eval` is not an option because page CSP can forbid `eval`, while `Runtime.evaluate` is not subject to CSP. iwdp 1.9.2 or newer is required for payloads at or above 30 KB (MAP fact 10); `doctor` checks the version.
- Owner epoch: the install records `{v, daemonId, session, epoch}`. A call whose owner differs (another session, a new lease epoch, a restarted daemon) first runs `__iwdp.reset`, which clears ref maps and pending dialog answers; refs of other owners are then unknown.
- Operation fencing: every runtime call carries `{opId, epoch, deadline}`, and an action attempt also carries `step` and `attemptEnd` (2.6). Both times are on the document's monotonic clock (`performance.now()`, captured at install), each projected through its own daemon instant with the conservative clock bounds of 2.6 (`pageMin`), which only shorten the page window; wall clocks are not used. The page dispatches only while `performance.now()` is strictly below both. Page code does no side effect when the op is canceled, the epoch changed, the step is not the next one, or the attempt end or the deadline passed, including a call that arrives after its attempt end, and it never dispatches after the call has settled.
- Version: `runtime.Version` is the first 12 hex digits of the SHA-256 of the bundle. The bundle defines `window.__iwdp` (non-enumerable) only if it is absent or has another version.
- Install paths: (1) on demand for the current document by `Runtime.evaluate`; (2) as the `runtime` part of the bootstrap script (2.9), so that new documents have it before page scripts run. Path 2 needs `Page.enable` in the session, which the daemon keeps.
- Every call is a small expression: `(window.__iwdp && window.__iwdp.v === "<ver>") ? window.__iwdp.call("<fn>", <json args>) : "__IWDP_MISSING__"` with `returnByValue: true`. On `__IWDP_MISSING__` the Go side installs and retries once. After `ErrTargetSwapped` it waits for the new target, installs and retries once only if the command is `RetrySafe` (2.6) and time remains before the deadline; otherwise it returns the error.
- Async calls (waits, stability checks): `Runtime.evaluate` without `returnByValue` gives a promise object id; then `Runtime.awaitPromise{promiseObjectId, returnByValue:true}`. WIP `Runtime.evaluate` has **no `awaitPromise` parameter** in 16.6 or main (protocol JSON); spike A confirmed that `Runtime.awaitPromise` works and that `evaluate{awaitPromise:true}` is silently ignored. W0's `eval --await` must use the same two-step path.
- Built-ins are captured at install time (`Element.prototype.getBoundingClientRect`, `document.querySelectorAll`, `Event` constructors, `JSON.stringify`), so later page code that overrides them does not change runtime behavior. WIP has no isolated world, so a hostile page can still read or replace `window.__iwdp`; section 9 lists this risk.

Go API (W3a, `internal/runtime`):

```go
const Version string
func Source() string

type Runtime struct{ /* Caller, generation source, boot parts */ }
func New(c webkit.Caller, gen func() uint64) *Runtime
func (r *Runtime) Call(ctx context.Context, fn string, args, out any) error      // sync functions
func (r *Runtime) CallAsync(ctx context.Context, fn string, args, out any) error // promise + Runtime.awaitPromise
```

Result envelope from every runtime function: `{"ok":true,"value":...,"warnings":[...]}` or `{"ok":false,"error":{"code":...,"message":...,"details":{...}}}`. Error codes: `stale_ref`, `unknown_ref`, `not_found`, `ambiguous`, `not_visible`, `not_enabled`, `not_editable`, `covered`, `unstable`, `timeout`, `bad_selector`, `unsupported`. The Go side maps each code to an actionable message (agent-browser wording where it exists, for example `covered by <div class="modal">`).

### 3.2 Snapshot (agent-browser compatible)

The runtime walks the flat tree (light DOM, open shadow roots, same-origin iframes one level deep) and returns JSON nodes: `{ref, role, name, level, checked, expanded, selected, disabled, required, value, url, cursor:{kind, hints, text}, children[]}`. Go renders the text, so the format has golden-file unit tests without a browser.

Text format follows agent-browser's renderer (SRC/agent-browser/snapshot.rs `render_tree` lines 1222-1352), not its older documentation:

```
- banner
  - navigation
    - link "Home" [ref=e3, url=/]
  - button "Sign In" [ref=e6]
- main
  - heading "Welcome" [level=1, ref=e8]
  - textbox "Email" [required, ref=e10]: user@example.com
  - checkbox "Remember me" [checked=false, ref=e12]
  - generic "Open menu" [ref=e14] clickable [cursor:pointer, onclick]
```

Rules copied as behavior from snapshot.rs (no code copied; agent-browser is Apache-2.0): two spaces per level; name as a JSON string with NBSP mapped to space and zero-width characters removed; attribute order `level, checked, expanded, selected, disabled, required, ref, url`; `: value` only when the value differs from the name; skip `generic` nodes with no ref and at most one child; `-i` shows only nodes with refs; `-c` keeps lines with refs, values or text and their ancestors; `(no interactive elements)` when `-i` finds nothing; `-d N` limits depth; `-s <selector or @ref>` scopes; `-u` adds `url=` to links. Elements with `cursor:pointer`, `onclick`, `tabindex` or `contenteditable` and no interactive role get a ref and the `clickable|focusable|editable [hints]` suffix, as agent-browser does. `--json` returns the node tree plus `{url, title, generation, refs}`.

Role and name: a compact implementation of the HTML-AAM implicit role table and the accessible-name steps that matter for agents (`aria-labelledby`, `aria-label`, `<label for>` and wrapping `<label>`, `alt`, `title`, `placeholder`, `value` of buttons, text content for name-from-content roles, `aria-hidden` and `hidden` exclusion). Playwright's `roleUtils.ts` (Apache-2.0) is the reference for the rules. If a worker adapts code from it, the file keeps the Apache-2.0 notice and `NOTICE` gets an entry (section 12). `DOM.getAccessibilityPropertiesForNode` is used only for `is` checks with `--native-ax`, after a device test (MAP section 2, option B; spike A showed it answers on WebKitGTK).

### 3.3 Refs and their lifetime

- Allocation: one serialized allocator per session and page in the daemon (`refs.Table`), used only inside a mutate operation, so two snapshots never get the same range. Refs live for the logical session: kept across reconnects of the same lease, reset on a session change or lease epoch change (owner epoch, 3.1), and on daemon restart (old refs fail with "daemon restarted; run snapshot"). A BFCache restore or frame document change counts as a new generation. A snapshot call passes `nextRef`; the runtime gives new elements `e<nextRef++>` and returns the new counter. The counter belongs to the session in the daemon and is never reset while the session lives, so a reset of the page maps makes old refs unknown without recycling their ids. Ref ids are never reused inside a session, also across navigations (agent-browser rule: "Invalidated IDs are never recycled").
- Storage in the page: an expando on the element (`el.__iwdpRef`) and `__iwdp.refs`, a `Map` from ref to element. Elements that keep their role and name keep their ref across snapshots (Playwright ariaSnapshot.ts lines 231-236 uses the same idea). JS state outlives the page connection, so refs survive reconnects and one-shot CLI processes.
- Daemon table: ref to `{generation, role, name, uniqueInSnapshot, frameContextId}`.
- Resolve order for `@eN`: (1) ref unknown to the table: `unknown_ref` "no snapshot produced @eN"; (2) table generation older than the current one: `stale_ref` "page navigated since the snapshot that produced @eN; run snapshot"; (3) element in the page map and connected: use it; (4) element gone, ref was unique by role and name in its snapshot, and exactly one element now has that role and name: use it and add a warning; (5) else `stale_ref` (agent-browser fallback rule).
- Targets accepted by every element command: `@eN`, a CSS selector, or locator forms `text=...`, `role=button[name="Save"]`, `label=...`, `placeholder=...`, `testid=...` (the `find` command uses the same resolver). A CSS selector that matches more than one element fails with `ambiguous` and the count, except where the command defines "first".

### 3.4 Visibility polyfill (`20-visibility.js`)

`Element.prototype.checkVisibility` needs Safari 17.4 (MAP section 2), so iOS 16.7 does not have it. The runtime uses its own function on every platform, so that results do not depend on the WebKit version:
1. Not connected: hidden.
2. Any flat-tree ancestor or self with computed `display: none`: hidden. An ancestor (not self) with `content-visibility: hidden`: hidden.
3. `display: contents`: visible if any child is visible.
4. No layout box (`getClientRects().length === 0`) or an empty bounding box: hidden.
5. Computed `visibility` is `hidden` or `collapse`: hidden. `opacity: 0` counts as visible (Playwright rule).
6. `<option>` inside a list-box `<select>`: use the `<select>` (WebKit quirk, Playwright domUtils.ts lines 122-134).
The test oracle is a fixture with hand-written expected visibility for each case (Playwright rules, including the separate `display: contents` rule, domUtils.ts lines 135-156), not native `checkVisibility`, which tests only the element's own box.

### 3.5 Actionability and event sequences (`50-actionability.js`, `55-actions.js`, `60-keyboard.js`)

Actionability is checked by attempts that the daemon repeats until the action timeout (default 5 s; `--timeout` overrides; always inside the command deadline of 2.6). Each attempt checks once, waits at most two animation frames for the stable check, and dispatches only when every check passes (2.6). `--force` skips the checks.

| Check | Meaning | Used by |
|---|---|---|
| attached | the element is connected | every action |
| visible | 3.4 | every action |
| stable | same rect in two consecutive animation frames | click, dblclick, tap, hover, check, uncheck, drag |
| enabled | no `disabled`, no `aria-disabled=true`, not inside a disabled `fieldset` or an `inert` subtree | click, dblclick, tap, check, uncheck, select, fill, type |
| editable | an input type that takes text, a textarea or contenteditable; not `readonly` | fill, type |
| receives events | `scrollIntoView({block:'center', inline:'center'})` if the center is outside the viewport, then `elementFromPoint` at the center (through open shadow roots) must be the element or a descendant; otherwise `covered`, with the covering element described as `<tag id class>` (agent-browser form `covered by <...>`) | click, dblclick, tap, hover, check, uncheck, drag |

`check` and `uncheck` resolve a hidden input to its `<label>` first and run the checks on the label. `fill` and `type` check editability, not hit testing. Every attempt is fenced by the operation id, lease epoch, attempt end and page-side deadline (3.1). The daemon sends no attempt after a cancel or the deadline, even when the page never receives the cancel. The only side effect that can still happen is the step of an attempt already in flight, and only before that attempt's slot-release boundary (2.6); its outcome stays unknown until its reply arrives, and no later step is sent. A covered element that is uncovered after the cancel therefore receives no click.

Input contract. All events are synthetic (`isTrusted=false`). Every action result has `"trusted": false`, and the CLI prints "synthetic event" once per session. Coordinates are client coordinates of the element center: the center of its bounding box, or for an element in several boxes (an inline element that wraps across lines) the center of its first box with an area, preferring one whose center is in the viewport.
- **Cancellation is honored.** Cancellable events are dispatched before any mutation, and their result decides the next step: a canceled `keydown` skips `keypress`, text insertion and the default action; a canceled `keypress` or `beforeinput` skips the insertion. Each event runs in its own task (Playwright webViewInput.ts lines 214-238).
- **Text edits respect the selection**: an insertion replaces `selectionStart..selectionEnd` and moves the caret to its end.
- **Default actions are emulated once, per control.** Synthetic keys have no default action, so the runtime applies it, only if no event of that key was canceled: Enter in a form follows HTML implicit submission (click the form's default button if it has one, else `form.requestSubmit()` only when the form has a single text field); Enter on a link or button clicks it; Space on a button or checkbox clicks it on keyup; Tab and Shift+Tab move focus in tab order; Backspace and Delete edit the value at the selection; arrow keys move the caret or change a select. The result names each emulated default action. No default action is applied twice.

| Command | Sequence |
|---|---|
| click | pointerover, pointerenter, mouseover, mouseenter, pointermove, mousemove, pointerdown, mousedown, focus (if focusable and not already focused), pointerup, mouseup, click. `--button right` ends with contextmenu. `--new-tab`: `window.open(href)` for links, through `Runtime.evaluate{emulateUserGesture:true}` |
| dblclick | click sequence twice with `detail` 1 and 2, then dblclick |
| tap | touchstart and touchend with one touch point (constructor support on iOS 16.7 is unverified; fallback `PointerEvent{pointerType:'touch'}`), then the click sequence with `pointerType: 'touch'` |
| hover | pointerover, pointerenter, mouseover, mouseenter, pointermove, mousemove. CSS `:hover` is not applied by synthetic events (MAP 1.2). `--css` also sets `CSS.forcePseudoState{hover}` on the node (works through Target routing per iwdp issue 434) and keeps it until the next action or release (2.4) |
| focus / blur | `el.focus()` or `DOM.focus`; blur on `document.activeElement` |
| fill | focus; dispatch beforeinput (`insertReplacementText`); if it is canceled, stop with "page canceled the input" and leave the value unchanged; else select all, set the value through the prototype's native value setter (React-safe, MAP 1.2), dispatch input and change. contenteditable: the same beforeinput first, then `execCommand('selectAll')` and `execCommand('insertText')` |
| type | per character: keydown; unless canceled, keypress; unless canceled, beforeinput (`insertText`); unless canceled, replace the selection through the native setter, move the caret, dispatch input; then keyup. `--delay ms` between characters; each character is one step (2.6) |
| keyboard inserttext | beforeinput with the whole text; unless canceled, insertion at the selection and input; no key events |
| press / keydown / keyup | key events on `document.activeElement` with `key`, `code`, `keyCode`, modifiers and the legacy `keyIdentifier` (Playwright webViewInput.ts), then the emulated default action of the key (above) |
| check / uncheck | if the state differs, click the element, or its `<label>` when the input is hidden (the checks ran on the label); read the state back; fail if it did not change |
| select | by value, then by visible label; multi-select takes several values; set `option.selected`, dispatch input and change |
| scroll | `scroll up\|down\|left\|right [px]` (default 300 px) on the window or `--selector` element; `scrollintoview <target>` |
| drag | pointer and mouse sequence from source center to target center in 10 steps (each one step of 2.6), plus synthetic `dragstart`, `dragenter`, `dragover`, `drop`, `dragend` with a shared `DataTransfer`. Native HTML5 drag is not driven by synthetic events (MAP 1.2); the result says "best effort" |
| upload | the CLI reads the files and sends them base64; the runtime builds `File` objects, assigns `input.files` through a `DataTransfer`, dispatches input and change. Total size limit: 8 MiB on iwdp and direct; on WebKitGTK the wire-byte limit of the envelope applies (3.1) |
| mouse move / down / up / wheel | the same events at coordinates; each increment of `mouse move --steps` is one step (2.6); the runtime keeps the pointer position and button state for the session |

Test oracles: tests assert the final page state (value, caret, checked state, whether the form submitted, click counters) on fixtures with page-side cancel handlers, not only the recorded event sequence (7.6).

### 3.6 Waits (`70-waits.js` plus Go)

| Form | Mechanism |
|---|---|
| `wait <ms>` | Go sleep, bounded by the command deadline |
| `wait <target> [--state visible\|hidden\|attached\|detached]` | in-page promise using `MutationObserver` plus a 100 ms poll, with a 1 s slice per call; Go repeats slices until the deadline, so a navigation or target swap only ends one slice |
| `wait --text <t>` | same slices; matches visible text (3.4) |
| `wait --url <glob>` | `location.href` per slice plus `Page.frameNavigated`; glob with `*` and `**` |
| `wait --load domcontentloaded\|load` | `document.readyState` per slice, generation-aware: after `open` the wait first requires a new generation or a changed URL, then the ready state |
| `wait --load networkidle` | daemon network tracker (W4): no request in flight for 500 ms. If Network capture is off, the daemon enables it for the page; the first wait can miss requests that started earlier, and the result says so. Fallback without a session: `performance.getEntriesByType('resource')` count stable for 500 ms |
| `wait --fn <js>` | expression evaluated per poll (100 ms), truthy ends the wait |

Default wait timeout 25 s (agent-browser value). On expiry: exit code 3 and the last observed state (for example "URL is still https://a/b").

`open <url>`: `Runtime.evaluate("location.href = <json url>")` (the same call first withdraws unused dialog answers, 3.7) with a 1 s deadline and a lost reply accepted, because a committed navigation can destroy the target before it answers (MAP 1.6). `Page.navigate` is never used: it is removed in iOS 26.4 (MAP section 0) and in WebKitGTK 2.54 (spike A). Then `wait --load load` unless `--wait none`. `back`, `forward`: `history.back()` and `history.forward()` (the same call first withdraws unused dialog answers) with the same wait. `reload [--ignore-cache]` is one exclusive operation in two phases, because `Page.reload` is a WIP command and not a runtime call: (1) a runtime call withdraws the unused dialog answers of the current document (3.7); (2) only after its reply, `Page.reload{ignoreCache}` with `ignoreCache` set by `--ignore-cache`, then the same wait. When the reply of phase 1 is lost, the withdrawn answers expire, and phase 2 still runs if time remains. A dialog that the old document opens between the phases gets the default answer.

### 3.7 Dialogs (`80-dialogs.js`)

WIP has no dialog events or commands, and a native dialog blocks `Runtime.evaluate` until a person dismisses it (MAP 1.4; spike A step F). The runtime replaces `window.alert`, `confirm` and `prompt` through the bootstrap script (installed on provisional targets, 2.7) and in the current document.
- Answers are synchronous, so the agent arms them first: `dialog accept [text]` and `dialog dismiss` add an answer with an id to the session queue.
- **At-most-once issuance.** Each answer is in one state: `queued` (in the daemon, never written into a page), `issued` (written into one document, identified by its generation, 2.7), `acknowledged` (its use was reported) or `expired`. The daemon issues a queued answer with a runtime call into the current document; the answer counts as issued from the moment that call is sent, even if its reply is lost. An issued answer is never written again after a reconnect, and it is written into another document only after an acknowledged withdrawal (below) has returned it to `queued`.
- **Acknowledgment.** A use is acknowledged only by a reply that the daemon receives from the page: the reply of the action that caused the dialog lists the answers it used, and every later runtime call in the same document reports the answers used since the previous call. The page also writes a marked `console.debug` message (hidden from console output) for each use. That message is telemetry for the dialog log, not proof: Console delivery can be lost, and the backend forwards messages only while Console is enabled and keeps a bounded history (SRC/webkit-src/InspectorConsoleAgent.7615.3.12.11.2.cpp lines 39-40, 67-84, 250-265).
- **Navigation and reconnect.** When the document of an issued, unacknowledged answer goes away (navigation, target swap, lost connection, takeover), the answer becomes `expired`: it may or may not have been used, and the next result says "dialog answer <id> expired with its document; arm it again if it is still needed". Only `queued` answers are issued into a new document, after the runtime is installed there. Commands that navigate on purpose withdraw the unused answers of the current document first (3.6): `open`, `back` and `forward` in the same runtime call that starts the navigation; `reload` in a separate first phase whose reply must arrive before `Page.reload` is sent. Withdrawn answers return to `queued` when the withdrawing reply arrives and expire when it is lost; they are issued again only into the new document.
- **Console ownership.** The dialog bridge is a Console owner in the desired state (2.4) from the moment the session has a dialog override, independent of any capture.
- Default when the document holds no unused answer (decision D6): alert accepted, confirm false, prompt null. This also covers a dialog that a new document opens before its queued answers are issued, so the bridge fails closed. Every dialog is logged and reported in the result of the action that caused it.
- Release restores the original functions and clears the queue (2.4). `beforeunload` is not controllable. Dialogs opened before the override exists still block; the error says to dismiss them on the device.
- The override reaches a new document before its scripts run only with pause-on-start (2.7; proof P1: 10 of 10 with it, 4 of 10 without it). The dialog override is an early-install part, so the session keeps pause-on-start on while it exists. Where pause-on-start is off or the device delivers an unpaused provisional target, the guarantee is partial: a dialog that the new document opens before the override is installed blocks as above, and the dialog help says so.
- CDP clients: see 5.3; dynamic dialog handling is not offered.

### 3.8 Tabs and frames

- `tab` lists pages of the selected device with stable ids `t1, t2, ...` (kept by the daemon per page id) and optional labels (`tab new --label x`).
- `tab <id|label>` moves the session's current page and lease. WIP cannot raise a tab, and Safari answers only for the visible tab (MAP fact 12). The switch therefore probes the tab and fails with "tab t2 is not the visible Safari tab; bring it to the front on the device" when it does not answer.
- `tab new [url]`: `window.open(url)` through `Runtime.evaluate{emulateUserGesture:true}` in the current page, then poll the page list (250 ms, up to 10 s). Popup blocking can stop it (unverified on device).
- `tab close [id]`: `window.close()`; works only for tabs that a script opened.
- `frame <target|main>`: for same-origin frames the runtime reaches the frame document directly. For cross-origin frames on iOS 16.7, `Runtime.enable` gives `executionContextCreated{id, frameId}` and the runtime is installed in that context (`contextId` parameter). Refs inside frames carry the context id in the ref table. iOS 26 and later can put cross-origin frames in their own targets; those are not supported in v1 (section 9, R7).

### 3.9 Annotated screenshots (`90-overlay.js`)

`screenshot --annotate`: take a snapshot if the session has none for the current generation; the runtime creates one host element with a closed shadow root, `position: fixed`, maximum `z-index`, `pointer-events: none`, and draws a box and a `[N]` label for each ref whose box intersects the capture area (N is the number of `@eN`, as agent-browser does). Then `Page.snapshotRect` captures, and the overlay is removed in a `finally` step (and by the cleanup list if the capture fails). The result lists `[N] @eN role "name"` for each label. `--if-changed` compares a SHA-256 of the PNG with the previous capture of the session and returns "unchanged" without writing a file.


## 4. Command surface

### 4.1 Rules

- agent-browser names, flags and output forms are kept wherever WIP can support them, so that skills and prompts move with few edits. Existing iwdp-cli names stay as aliases (for example `navigate` for `open`, `type-text` for `type`).
- Every command is a registry entry (2.11) with one MCP tool name. MCP names follow the repository's snake_case style; existing MCP names are kept when the meaning is the same and get new optional parameters (for example `click` gets `target`, which accepts `@eN`).
- Global flags: `--session`, `--page <tN|id|url-substring>`, `--device`, `--timeout`, `--json`, `--transport`, `--max-output`, `--content-boundaries`.
- MCP profiles (decision D2): `core` is the default (about 40 agentic and key debugging tools); `devtools` (W4 extras) and `legacy` (the 116 existing tools) are added with `IWDP_MCP_TOOLS=core,devtools,legacy` or `--tools`; `all` registers everything.
- "WS" names the workstream that owns the command file.

### 4.2 Mapping table

| agent-browser | iwdp-cli command and flags | MCP tool | WIP mechanism | WS |
|---|---|---|---|---|
| **Navigation** | | | | |
| `open [url]` (aliases `goto`, `navigate`) | `open <url> [--wait load\|domcontentloaded\|networkidle\|none]`; aliases `goto`, `navigate` | `navigate` (+`wait_until`) | `location.href` by evaluate, then wait (3.6) | W3b |
| `back`, `forward` | `back`, `forward` `[--wait ...]` | `go_back`, `go_forward` | `history.back/forward` + wait | W3b |
| `reload` | `reload [--wait ...] [--ignore-cache]` | `reload` | dialog answer withdrawal, then `Page.reload` + wait (3.6) | W3b |
| `pushstate <url>` | `pushstate <url>` | `pushstate` | runtime: Next.js router if present, else `history.pushState` + popstate | W3b |
| `read [url]` | `read [--outline] [--max-output n]` (rendered current page); `read <url>` fetches over HTTP on the host and needs no device | `read_page` | runtime text walker (visible text, headings outline) | W3a |
| `close` (upstream also has `--all`) | `close`: ends only the caller's session (`--session`), releases its leases, runs its cleanup and revokes its CDP endpoint records (5.2); does not close the Safari tab. There is no `--all`: it is rejected as an unknown flag (exit 2) | `close_session` | daemon | W1 |
| `connect <port\|url>` | `connect <ws-url\|tN\|page-id>`: pins the session to a page | `select_page` (existing) | daemon | W1 |
| `get cdp-url` | `get cdp-url` (starts the CDP listener if needed) | `get_cdp_url` | daemon | W5 |
| **Snapshot** | | | | |
| `snapshot -i -c -d -s -u --json` | same flags; `-s` also takes `@eN` | `snapshot` {interactive, compact, depth, selector, urls} | runtime (3.2) | W3a |
| `diff snapshot` | `diff snapshot` | `diff_snapshot` | Go line diff of the last two snapshots of the session | W3a |
| **Actions** | | | | |
| `click [--new-tab]` | `click <target> [--new-tab] [--button left\|right\|middle] [--count n] [--force]` | `click` (existing, +`target`) | runtime 3.5 | W3b |
| (iOS) | `tap <target\|x y>` | `tap` | runtime touch sequence | W3b |
| `dblclick` | `dblclick <target>` | `dblclick` | runtime | W3b |
| `fill` | `fill <target> <text>` (`--stdin` for secrets) | `fill` (existing, +`target`) | runtime native setter | W3b |
| `type` | `type <target> <text> [--delay ms]`; alias `type-text` | `type_text` (existing, +`target`) | runtime key events | W3b |
| `keyboard type`, `keyboard inserttext` | `keyboard type <text>`, `keyboard inserttext <text>` | `keyboard` {mode} | runtime | W3b |
| `press` (alias `key`), `keydown`, `keyup` | `press <key>` (alias `key`), `keydown <key>`, `keyup <key>` | `press` {key, phase} | runtime + emulated default actions | W3b |
| `hover` | `hover <target> [--css]` | `hover` | runtime + optional `CSS.forcePseudoState` | W3b |
| `focus` | `focus <target>`, `blur` | `focus` | `el.focus()` | W3b |
| `check`, `uncheck` | `check <target>`, `uncheck <target>` | `check` {checked} | runtime | W3b |
| `select` | `select <target> <value...>` | `select_option` | runtime | W3b |
| `drag` | `drag <src> <dst>` | `drag` | runtime, best effort | W3b |
| `upload` | `upload <target> <file...>` | `upload_file` | runtime `DataTransfer` | W3b |
| `scroll`, `scrollintoview` | `scroll <dir> [px] [--selector t]`, `scrollintoview <target>` | `scroll`, `scroll_into_view` | runtime | W3b |
| `mouse move/down/up/wheel` | `mouse move <x> <y> [--steps n]`, `mouse down\|up [btn]`, `mouse wheel <dy> [dx]` | `mouse` {action} | runtime | W3b |
| (iOS) | `swipe <up\|down\|left\|right> [px] [--selector t]` | `swipe` | touch events + scroll fallback | W3b |
| **Waits** | | | | |
| `wait <sel\|@ref\|ms>`, `--text`, `--url`, `--load`, `--fn`, `--state` | same flags, `--timeout` (default 25 s) | `wait` | 3.6 | W3b |
| **Reading state** | | | | |
| `eval <js>`, `-b`, `--stdin`, `--json` | `eval <js> [-b base64] [--stdin] [--await] [--json]` | `evaluate_script` (existing) | `Runtime.evaluate` (+`Runtime.awaitPromise`) | W0 base, W1 moves to registry |
| `get url`, `title` | `get url`, `get title` | `get` {what} | runtime or page list | W3a |
| `get text`, `html`, `value`, `attr`, `count`, `box`, `styles` | `get text\|html\|value\|box\|styles <target>`, `get attr <target> <name>`, `get count <selector>` | `get` {what, target, name} | runtime; styles by `getComputedStyle`, never the CSS domain | W3a |
| (asked in AGG) | `hit-test <x> <y>`: element at a point, its box and ref | `hit_test` | runtime `elementFromPoint` | W3a |
| `is visible`, `enabled`, `checked` | `is visible\|enabled\|checked\|editable\|focused <target>` | `is` {state, target} | runtime | W3a |
| `find role/text/label/placeholder/alt/title/testid/first/nth` | `find <kind> <value> [action [args]] [--name n] [--exact]` | `find` {kind, value, name, exact, action} | runtime locator; action runs through the action commands | W3a (locate), W3b (action) |
| **Tabs and frames** | | | | |
| `tab`, `tab list` | `tab [list] [--json]` | `tab_list` | transport page list | W3b |
| `tab new [--label] [url]` | `tab new [--label x] [url]`; alias `window new` | `tab_new` | `window.open` with user gesture | W3b |
| `tab <id\|label>` | `tab <tN\|label>` | `tab_select` | lease move + probe | W3b |
| `tab close [id]` | `tab close [tN]` | `tab_close` | `window.close()` | W3b |
| `frame <sel\|@ref\|main>` | `frame <target\|main>` | `select_frame` | context id (3.8) | W3b |
| **Dialogs** | | | | |
| `dialog status`, `accept [text]`, `dismiss` | same; plus `--dialog-default` session flag | `dialog` {action, text} | runtime override (3.7) | W3b |
| **Console and errors** | | | | |
| `console [--clear]` | `console [--clear] [--level l] [--since id] [--follow] [--duration d]` | `get_console_messages` (existing, now daemon buffer), `clear_console` | daemon ring buffer from `Console.messageAdded` (replay of the last 100 on enable, MAP 1.7) | W4 |
| `errors [--clear]` | `errors [--clear]` | `get_errors` | console errors with source javascript + bootstrap `window.onerror` and `unhandledrejection` hooks | W4 |
| `highlight <@ref>` | `highlight <target>` | `highlight_node` (existing, +`target`) | `DOM.highlightNode` (draws on the device) | W3b |
| **Network** | | | | |
| `network requests [--filter] [--clear]` | `network requests [--filter p] [--type t] [--status s] [--clear]` | `list_network_requests` (existing, daemon buffer) | daemon netlog from `Network.*` events (no replay: capture starts with the lease, MAP 1.8) | W4 |
| `network request <id>` | `network request <id> [--body]` | `get_network_request` | stored request, `postData`, response headers, `Network.getResponseBody`; fallback `Page.getResourceContent` | W4 |
| `network route <url> [--abort] [--body] [--status] [--delay]` | same flags | `network_route` | `Network.setInterceptionEnabled` + `addInterception` + `interceptRequestWithResponse\|WithError\|Continue`; fallback: `fetch`/`XMLHttpRequest` patch in the bootstrap (`--mode js`) | W4 |
| `network unroute [url]` | same | `network_unroute` | remove interception, continue paused requests | W4 |
| `network har start/stop` | `network har start [--content all\|none]`, `network har stop [path]` | `har_start`, `har_stop` | HAR 1.2 from the netlog | W4 |
| `addinitscript`, `removeinitscript` | same | `add_init_script`, `remove_init_script` | bootstrap part (2.9) | W4 |
| **Screenshots** | | | | |
| `screenshot [path] [--full] [--annotate] [--if-changed] [--screenshot-format/quality]` | `screenshot [path] [--full\|--full-page] [--selector t\|@eN] [--annotate] [--if-changed] [--format png\|jpeg] [--quality n]` | `take_screenshot` (existing, extended) | `Page.snapshotRect` (W0), overlay (3.9), JPEG on the host | W0 base, W3b annotate |
| `diff screenshot --baseline` | `diff screenshot --baseline <png> [--threshold n]` | `diff_screenshot` | host pixel diff | W4 |
| **Emulation and settings** | | | | |
| `set media dark\|light [reduced-motion]` | `set media <dark\|light\|no-preference> [--reduced-motion] [--contrast more]` | `set_media` | `Page.overrideUserPreference` + `Page.setEmulatedMedia` (session scoped) | W4 |
| (--user-agent) | `set useragent <ua>` | `set_user_agent` | `Page.overrideUserAgent` | W4 |
| `set headers` | `set headers <json>` | `set_extra_headers` (existing) | `Network.setExtraHTTPHeaders` | W4 |
| `set credentials` | `set credentials --origin <origin> <user>` (password from stdin) | `set_credentials` | only origin-scoped: request interception adds `Authorization` to requests of that origin; enabled only after the device test shows that subresources and cross-origin redirects of other origins never receive it; otherwise the command returns unsupported. Never global extra headers; never logged | W4 |
| `set geo` | `set geo <lat> <lng>` | `set_geolocation` | bootstrap shim of `navigator.geolocation` | W4 |
| `set offline` | `set offline on\|off` | `set_offline` | interception with error for all URLs; unverified on device | W4 |
| `set viewport`, `set device` | `set viewport` and `set device` exist and fail with the reason (4.3) | none | none | W4 |
| **Cookies and storage** | | | | |
| `cookies`, `cookies get` | `cookies [--show-values] [--name n]` (redacted by default, W0) | `get_cookies` | `Page.getCookies`; `Storage.getCookies` on iOS 27.3+ | W0 base, W4 |
| `cookies set`, `cookies set --curl` | `cookies set <name> <value> [--same-site] [--expires] ...`, `cookies set --curl <file>`, `--json <file>` | `set_cookie` | `Page.setCookie` with read-back (W0) | W0 base, W4 import |
| `cookies clear` | `cookies clear [--domain d]` | `clear_cookies` | `Page.setCookie` of each cookie with an expiry in the past (exact identity, W5c-1b), read back; scope is the frame resource URLs (MAP 1.8) | W4 |
| `storage local\|session` | `storage local [key]`, `storage local set <k> <v>`, `storage local clear` (same for session) | existing storage tools + `storage` | JS `localStorage` (no storage id needed) | W4 |
| `state save/load`, `--state` | `state save <path>`, `state load <path>` | `state_save`, `state_load` | cookies + storage of the current origin, JSON | W4 |
| **Sessions and diagnostics** | | | | |
| `session`, `session id/list/info`, `--session` | `session [id\|list\|info]`, `session release`, `lease break <page>` | `session_info` | daemon | W1 |
| `doctor` | `doctor [--json]`: daemon, transport, iwdp version (needs 1.9.2+), usbmuxd, devices, Web Inspector hint, visible tab probe, leases | `doctor` | daemon | W1 |
| `batch [--bail]` | `batch [--bail] "cmd" ...` | none (MCP calls are already batched by clients) | one `/v1/batch` request | W1 |
| `skills get/list/path` | `skills get\|list\|path` (the skill guides `skill-data/<name>/SKILL.md`, embedded in the binary; the discovery stub `skills/<name>/SKILL.md` only points at `skills get <name>`) | none | none | W6 |
| `trace start/stop` | `trace start [--categories]`, `trace stop [path]` | `trace_start`, `trace_stop` | `Timeline.start/stop` converted to Chrome trace JSON | W4 |
| `profiler start/stop` | `profiler start`, `profiler stop [path]` | `profiler_start`, `profiler_stop` | `ScriptProfiler` samples converted to `.cpuprofile` | W4 |
| `vitals [url]` | `vitals [--json]` | `vitals` | JS: navigation and paint timing on 16.7; LCP and event timing on Safari 26.2+ (MAP 1.12) | W4 |
| (DevTools extra) | `emulate network <preset>` | `set_emulated_conditions` (existing) | `Network.setEmulatedConditions` only where the iOS command list has it (16.4, 27.3+) | W4 |

### 4.3 Impossible or partial on iOS WebKit

| agent-browser feature | Status | Reason | Closest substitute |
|---|---|---|---|
| Trusted input (real tap, key, mouse) | impossible on WIP | no Input domain; every event is `isTrusted=false` | synthetic sequences (3.5); research item R10 (Safari automation session) |
| CSS `:hover` by pointer move | partial | synthetic moves do not set hover state | `hover --css` (`CSS.forcePseudoState`) |
| set viewport, set device | impossible | `Page.setScreenSizeOverride` is compiled only for non-Cocoa (`#if !PLATFORM(COCOA)`, MAP 1.10) | screenshots at native size; user agent override only |
| pdf | impossible | no print API in WIP; `window.print()` opens native UI | screenshot `--full-page` |
| download | impossible | no download events in WIP; Safari saves to Files | read the URL with `get attr @eN href` and fetch on the host |
| record, stream, screencast | impossible as video | no screencast frames on iOS (MAP 1.9) | `screenshot --if-changed` loop |
| inspect (open DevTools) | not applicable | Chrome DevTools UI | `get cdp-url` and a CDP client |
| browser contexts, incognito, `--state` isolation per context | impossible | one Safari profile; no `Target.createBrowserContext` | separate sessions share one cookie jar; documented |
| native pickers (date, color, select popup) | impossible to dismiss | native UI outside WebKit (MAP 1.2) | `fill` or `select` by value |
| beforeunload dialog | impossible | no protocol hook | none |
| clipboard read and write | partial | needs a user gesture and permission | `eval` with `emulateUserGesture`; unverified |
| set geo, set offline | partial | no WIP method | JS shim; interception |
| a11y (axe audit) | not shipped | axe-core is MPL-2.0 and large | existing `run_audit` (WebKit Audit domain); user-supplied script through `eval` |
| subframe navigation, iOS 26 cross-origin frame targets | not in v1 | separate targets per frame (MAP 1.5) | top-level page only |
| react, webmcp, flow, auth, plugin, chat, dashboard, install, upgrade, confirm/deny | out of scope | agent-browser product features, not browser control | none |


## 5. CDP endpoint

### 5.1 Listener, authorization and HTTP surface

- Started by `iwdp-cli cdp start [--port 9230]`, `get cdp-url`, or the daemon flag `--cdp-port`. Not started by default (decision D3). Default port 9230: 9221 to 9229 belong to iwdp, and 9222 is often used by a local Chrome (it is on this host).
- Binds `127.0.0.1` only. Rejects requests whose `Host` header is not `127.0.0.1:<port>` or `localhost:<port>` (DNS rebinding guard, as Chrome does).
- Loopback plus a Host check does not stop a web page or another local user, so the endpoint also has these rules:
  - **Token.** The daemon generates a random 256-bit token at each start and requires it on every HTTP path and WebSocket upgrade as a path prefix (`/t/<token>/`). A request without a valid token gets 404 and no discovery data; there is no unauthenticated discovery at the root. The token reaches clients only through the private Unix API (`iwdp-cli get cdp-url`, 2.2); it is never logged, and URLs in logs and status output are redacted.
  - **Endpoint forms.** `get cdp-url` issues an endpoint record (5.2) and prints its `ws` form; `--json` returns both typed forms. Every path of the record is below `/t/<token>/e/<endpoint>`.
    - `ws`: the browser WebSocket URL `ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>`. It is the default form for every client, because clients use a `ws://` URL verbatim. agent-browser 0.41.2 needs it: for an `http://` URL its resolver keeps only host, port and query and then requests the unprefixed `/json/version`, so the token path is lost (agent-browser commit d52a5e90, `cli/src/native/browser.rs` lines 2920-2948 and `cli/src/native/cdp/discovery.rs` lines 130-150).
    - `http`: the discovery base `http://127.0.0.1:9230/t/<token>/e/<endpoint>`, only for clients whose pinned version appends `/json/version` to the base path: playwright-core 1.65.0 (`server/chromium/chromium.ts` lines 457-464) and puppeteer-core 25.16.0 (`common/BrowserConnector.ts` lines 203-209). Any other client uses the `ws` form until the endpoint-resolution test (7.8 test 10) shows the exact request it makes.
  - **Origin.** Requests with any `Origin` header (browser pages, including `null`) are rejected unless the origin is in `--cdp-allow-origin` (empty by default).
  - **No side effects on GET.** `/json/new`, `/json/close` and `/json/activate` accept only `PUT`.
  - The socket directory check of 2.2 protects the Unix API that hands out the token.
- HTTP, all below `/t/<token>/e/<endpoint>`: `GET /json/version` (also with a trailing slash, which Playwright sends; `Browser: "Safari/<iOS version> (iwdp-cli <version>)"`, `Protocol-Version: "1.3"`, `webSocketDebuggerUrl` of the record's browser endpoint), `GET /json` and `/json/list` (one entry per page in the record's scope, `type: "page"`, `webSocketDebuggerUrl: ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/page/<targetId>`), `PUT /json/new?url` (`window.open`, see 3.8), `PUT /json/close/<id>` (`window.close()`), `PUT /json/activate/<id>` (probe only; error text if the tab is not visible), `GET /json/protocol` (the supported subset in CDP JSON form, generated from the handler table).
- WebSocket, below `/t/<token>/e/<endpoint>`: `/devtools/browser/<endpoint>` (flat sessions through `Target.attachToTarget{flatten:true}`; Puppeteer, Playwright, agent-browser) and `/devtools/page/<targetId>` (one page; chrome-devtools-mcp through Puppeteer uses the browser endpoint, older tools use this).

### 5.2 Target model

The rules below come from the initialization code of the pinned clients (Puppeteer 25.16.0, playwright-core 1.65.0), not only from the CDP specification.
- **Tab and page targets.** Each Safari page in scope is exposed as a `tab` target with one child `page` target, as current Chrome does, both with `browserContextId: "default"`. The page `targetId` is stable for the life of the tab: an opaque string derived from the device id and the transport page id. WebKit provisional swaps (2.7) are hidden: the CDP target id does not change, and the bridge emits `Runtime.executionContextsCleared`, `Page.frameNavigated`, `Runtime.executionContextCreated` and lifecycle events instead.
- **Filters and auto-attach.** `Target.setAutoAttach` and `Target.setDiscoverTargets` apply the client's filter exactly, with Chrome's default filter (exclude `browser` and `tab`) when none is given. Puppeteer excludes `page` at browser level and attaches tabs, then the pages below them (TargetManager.ts lines 159-177); Playwright creates its pages from auto-attach events (crBrowser.ts lines 85-99, 171-208). Existing targets are announced in Chrome's order (target created, then attached). `setAutoAttach` on a tab session attaches its page. `Target.getTargetInfo` without a target id describes the browser.
- **Endpoint records, logical sessions and leases.** A CDP connection has no `--session` flag, so it takes its logical session from its URL. `get cdp-url`, run with the caller's `--session`, creates an endpoint record `{endpoint, logicalSession, selectedPage, reservationOwner, scope}` and puts the record's opaque random id into both URL forms (5.1).
  - `selectedPage` is the session's current page when the URL is issued, resolved like the page of any command (exit 5 when there is none). Issuing a URL does not take the lease, so it also works for a page that another session holds; the attach then fails as described below. A later `tab` switch in the session does not move the record; issue a new URL for the new page. Issuing again for the same session, page and scope returns the same record.
  - `scope` is `page` by default. `get cdp-url --scope device` widens discovery and auto-attach to the pages of the devices reserved by `reservationOwner` (2.5).
  - Every connection through the record, and every tab and page attachment session created on it, acts for `logicalSession`: the lease, refs, dialog policy and desired state belong to that session, exactly as for a CLI command with the same `--session`. A second connection through the same URL intentionally joins the same logical session. Attaching a page takes the lease for `logicalSession`, or reuses it with its epoch when the session already holds it, so refs of the session's CLI commands stay valid.
  - Each CDP attachment (`sessionId`) has its own attachment identity, which owns the state it sets (2.4). Detaching or disconnecting releases only that attachment's entries; the lease stays with the logical session under the rules of 2.5.
  - A page leased by another session is listed but never attached; `Target.attachToTarget` on it fails with the lease message (2.5) and leaves that lease unchanged.
  - The record lives until its logical session ends (`close`, `session release`), `cdp stop` or daemon exit; after that its URLs get 404.
- **Frames.** The main frame id is the WIP frame id from `Page.getResourceTree`. Same-origin child frames are reported by `Page.getFrameTree`; cross-origin frame targets (iOS 26) are not exposed in v1.
- **Execution contexts and utility worlds.** One default context per frame, with the id from WIP `Runtime.executionContextCreated` and `auxData{frameId, isDefault:true, type:"default"}`. `Page.createIsolatedWorld{worldName}` and `Page.addScriptToEvaluateOnNewDocument{worldName}` register a world name per frame. For every registered name and every new document the bridge allocates a distinct CDP context id (an alias) and emits `Runtime.executionContextCreated` with `name: worldName` and `auxData{frameId, isDefault:false, type:"isolated"}`; calls on an alias run in the WIP main world. A context id is never emitted twice under different names, because Playwright keys contexts by id. All aliases are cleared with `executionContextsCleared` on navigation. Playwright and Puppeteer ignore the result of `createIsolatedWorld` and find their utility world only through this named event (crPage.ts lines 511-518, 732-746; FrameManager.ts lines 612-632). These worlds are **not** security-isolated; the docs say so.
- **Node ids.** CDP `nodeId` is the WIP `nodeId` of the page session's connection. CDP `backendNodeId` is a runtime handle number (the number of a ref or an internal handle), resolved in the page; the same element keeps its number for its document. This avoids one `DOM.requestNode` round trip per AX node.
- **Navigation identity.** The bridge owns loader ids: one per document generation (2.7). `Page.navigate` returns it after the document request or the generation change is seen; `frameNavigated` carries the same id; the main-frame document request in `Network.requestWillBeSent` uses `requestId == loaderId` (Playwright frames.ts lines 719-745, crNetworkManager.ts lines 369-370). Same-document navigations return no loader id and emit `navigatedWithinDocument`. Lifecycle events carry the session id of the page session.
- **Required on attach.** `Performance.enable` is fatal in Puppeteer if it fails (Page.ts lines 433-447), so it is tier 1 and returns success with the runtime metrics of 5.3.
- **Dialogs.** `Page.handleJavaScriptDialog` returns the error "dynamic dialog handling is not supported on iOS WebKit; pre-arm answers with iwdp-cli dialog accept|dismiss". The bridge never emits `javascriptDialogOpening` or `javascriptDialogClosed`, and no answer from a CDP client is kept for a later dialog. The session's dialog policy (3.7) still answers dialogs (otherwise a native dialog would block the page), and each automatic answer is reported as a `Log.entryAdded` warning, not as a dialog event.
- **Pins.** chrome-devtools-mcp 1.12.0 with the Puppeteer it pins (25.16.0), playwright-core 1.65.0, agent-browser 0.41.2 (binary checksum recorded). The minimum method set is derived from each client's initialization code and frozen by the compatibility tests (8.4), which assert behavior (locator found the element, navigation resolved, value changed), not only method names.
- **Early proofs.** Before W5 starts, W1a runs proof P1 (provisional setup across a document swap) and proof P2 (pinned Playwright and Puppeteer attach plus one utility-world locator against a minimal bridge); see 7.1.

### 5.3 Domains and methods

Tier 1 is required for the four named clients (agent-browser method list taken from the strings of its 0.41.2 binary; Puppeteer and Playwright from their connect sequences; to be confirmed by the compatibility tests in 8.4, which log every method each client sends). Tier 2 adds DevTools extras. Tier 3 is optional.

| Domain | Methods and events | Implementation | Tier |
|---|---|---|---|
| Browser | getVersion, close (detach only), setDownloadBehavior (accepted, no effect), getWindowForTarget (synthetic), grantPermissions (error) | bridge | 1 |
| Target | tab and page targets with the client's filter (5.2); setDiscoverTargets, getTargets, getTargetInfo (browser when no id), setAutoAttach{flatten, filter}, attachToTarget, detachFromTarget, createTarget (`window.open`), closeTarget (`window.close`), activateTarget (probe), getBrowserContexts (default only); events targetCreated, targetDestroyed, targetInfoChanged, attachedToTarget, detachedFromTarget | bridge + transport page list (poll every 2 s while a client listens) | 1 |
| Page | enable, disable, getFrameTree, getResourceTree, navigate (`location.href` + wait; returns frameId and loaderId), reload, getNavigationHistory (current entry only), navigateToHistoryEntry (`history.go`), captureScreenshot (`snapshotRect`; clip; `captureBeyondViewport` uses Page coordinates; jpeg on the host), getLayoutMetrics (runtime), addScriptToEvaluateOnNewDocument and removeScriptToEvaluateOnNewDocument (bootstrap parts), createIsolatedWorld (named alias context, 5.2), setLifecycleEventsEnabled, handleJavaScriptDialog (error: unsupported), bringToFront (probe), stopLoading (`window.stop()`), setBypassCSP (error); events frameNavigated, navigatedWithinDocument (runtime `popstate`/`hashchange` hook), domContentEventFired, loadEventFired, lifecycleEvent (init, DOMContentLoaded, load, networkIdle synthesized) | WIP + runtime | 1 |
| Runtime | enable, disable, evaluate (`awaitPromise` through `Runtime.awaitPromise`; `exceptionDetails` built from `wasThrown`), callFunctionOn (`objectId` goes to WIP; `executionContextId` is turned into a function object by evaluate first; `awaitPromise` through `Runtime.awaitPromise` on 16.6, where WIP callFunctionOn lacks it), getProperties, releaseObject, releaseObjectGroup, runIfWaitingForDebugger (no effect), addBinding and removeBinding (page function that writes a marked `console.debug` message; the bridge turns it into `Runtime.bindingCalled` and hides it from console output); events executionContextCreated (with `auxData{frameId, isDefault, type}`), executionContextsCleared, executionContextDestroyed, consoleAPICalled and exceptionThrown (from `Console.messageAdded`) | WIP + runtime | 1 |
| DOM | enable, disable, getDocument, querySelector, querySelectorAll, describeNode, resolveNode (`nodeId` to WIP; `backendNodeId` to runtime), requestNode, getBoxModel and getContentQuads (runtime rects; WIP has no box model method), scrollIntoViewIfNeeded, focus (WIP `DOM.focus`), getOuterHTML, setAttributeValue, removeNode, getFrameOwner, getNodeForLocation (runtime `elementFromPoint`), setFileInputFiles (runtime upload with host files) | WIP + runtime | 1 |
| Accessibility | enable, disable (no effect), getFullAXTree (from the runtime snapshot in full mode: `nodeId`, `role{type:"role"}`, `name{type:"computedString"}`, properties checked, expanded, selected, disabled, required, level, focusable, focused, `childIds`, `backendDOMNodeId` = runtime handle), getPartialAXTree, queryAXTree (runtime locator) | runtime | 1 |
| Input | dispatchMouseEvent (moved, pressed, released, wheel at x and y; click count; button state per session), dispatchKeyEvent (keyDown, keyUp, rawKeyDown, char; emulated default actions), insertText, dispatchTouchEvent, emulateTouchFromMouseEvent, synthesizeTapGesture | runtime (3.5); untrusted | 1 |
| Network | enable, disable, getResponseBody, getRequestPostData (stored from `requestWillBeSent`), setExtraHTTPHeaders, setCacheDisabled (`setResourceCachingDisabled`), setUserAgentOverride (`Page.overrideUserAgent`), getCookies, setCookie, deleteCookies, clearBrowserCookies (`Page.*` cookies; `Storage.*` on 27.3+), emulateNetworkConditions (`setEmulatedConditions` where the iOS command list has it, else error); events requestWillBeSent, responseReceived, dataReceived, loadingFinished, loadingFailed, requestServedFromCache (WIP `timing` converted) | WIP | 1 |
| Emulation | setEmulatedMedia (`setEmulatedMedia` + `overrideUserPreference`), setUserAgentOverride, setFocusEmulationEnabled (no effect, success), setGeolocationOverride (JS shim), setTouchEmulationEnabled (no effect: the device is touch), setDeviceMetricsOverride and clearDeviceMetricsOverride (error "viewport is fixed by the device", except an override equal to the real viewport, which succeeds) | WIP + runtime | 1 |
| Log | enable, disable; entryAdded from console messages with source network, security or other | WIP | 1 |
| Fetch | enable{patterns}, disable, requestPaused, continueRequest, fulfillRequest, failRequest (WIP interception through the control lane of 2.10; unverified on device, MAP 1.8); continueWithAuth (error) | WIP | 2 |
| Storage | getCookies, setCookies, clearCookies, clearDataForOrigin (JS storage only) | WIP + runtime | 2 |
| Performance | enable (required on attach, 5.2), getMetrics (navigation timing, DOM node count, JS heap from `Memory` where present) | runtime | 1 |
| Audits | enable (no effect, success: Puppeteer 25.16.0 sends it on every attach and fails the attach when it is unknown, P2 item 3); no `issueAdded` events | bridge | 1 |
| Tracing | start, end; dataCollected, tracingComplete (Timeline events converted, W4) | WIP | 2 |
| Profiler | enable, start, stop (ScriptProfiler converted, W4) | WIP | 3 |
| Debugger, HeapProfiler, Overlay, CSS, Security | not in v1, except `Security.setIgnoreCertificateErrors` (error) and `Overlay.highlightNode` (`DOM.highlightNode`, tier 3) | | 3 |

### 5.4 Unsupported methods

- The bridge never forwards a CDP method that it does not translate. Unknown or unsupported methods get `{"code": -32601, "message": "'<Domain.method>' wasn't found"}`, the same text Chrome uses for a missing method, so clients that probe for optional features continue.
- Methods with no effect on iOS but that clients call on every attach (listed in the table as "no effect") return success, so that attach does not fail.
- Each unknown method is logged once per CDP session; `iwdp-cli cdp log` prints the counts. The compatibility tests fail on an unknown method that is not on an expected-missing list per client; each entry on such a list names the optional client feature it belongs to and is reviewed, so a required client operation cannot fail while the tests stay green.
- Passthrough to WIP exists only behind `IWDP_CDP_PASSTHROUGH=<Domain.method,...>` and only for methods in the iOS command list of the device's version (MAP section 0). `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` are always refused (CLAUDE.md known limitation).
- The command lists per iOS version (16.4, 17.0, 18.0, 26.0, 26.2, 26.4, 27.0, 27.3) are generated into `internal/wip/methods_gen.go` from SRC/webkit-legacy-ios (method names are facts; the file header names the source). The bridge and the runtime layer use them to choose, for example, `Storage.getCookies` on 27.3 and later.

### 5.5 Client notes

- Every client uses the exact URL that `iwdp-cli get cdp-url` prints, the `ws` form of 5.1, written `<cdp-ws-url>` below; the `http` form is written `<cdp-http-url>`. Never assemble a URL by hand.
- agent-browser: `agent-browser --cdp <cdp-ws-url> open <url>`, `snapshot -i`, `click @e3`. Its snapshot calls `Accessibility.getFullAXTree` and `DOM.resolveNode{backendNodeId}`; both are served from the runtime, so its refs and ours are separate but consistent.
- Puppeteer and chrome-devtools-mcp: connect with `defaultViewport: null` (otherwise Puppeteer sends `Emulation.setDeviceMetricsOverride`). Puppeteer: `connect({browserWSEndpoint: '<cdp-ws-url>'})`, or `browserURL: '<cdp-http-url>'`. chrome-devtools-mcp: `--wsEndpoint <cdp-ws-url>`.
- Playwright: `chromium.connectOverCDP('<cdp-ws-url>')` (or `<cdp-http-url>`), use `browser.contexts()[0]`; `newContext()` and `newPage()` in a new context fail (no browser contexts). Locators work through Runtime and DOM; `page.mouse`, `page.keyboard` and `page.touchscreen` go through Input (untrusted).


## 6. Direct transport (no ios_webkit_debug_proxy)

Based on spike B (section 11.2). Package `internal/transport/direct` (W2).

- **Device layer: go-ios `ios` package only, through an adapter.** `ios.ListDevices` filtered to `Properties.ConnectionType == "USB"` and `ios.GetDevice(udid)` choose the device. The steps of `ios.ConnectToService(device, "com.apple.webinspector")` (usbmuxd connect, lockdown session with TLS, service start, service connect) run in the W2 adapter described under "Socket ownership" below. No tunnel and no root on this path. `USBMUXD_SOCKET_ADDRESS` overrides the socket; pair records come from usbmuxd (`ReadPairRecord`), not from `/var/lib/lockdown` directly.
- **Not go-ios `ios/webinspector`.** In v1.3.2 it has one shared event queue for all pages (open PR #878), no `_rpc_forwardDidClose:`, no target-swap handling, page listing by fixed sleep, a client that dies on its first read error, and a CDP translator that is the wrong shape for us. Two other Go tools (mobilecli, maestro-runner) also use only go-ios transport and wrote their own WIR layer.
- **Own WIR session layer (MIT, about 400 lines plus tests).** One long-lived webinspectord connection per device (the service stalls new connections for seconds after a close: mobilecli comments, MAP fact 2). Messages: `_rpc_reportIdentifier:`, `_rpc_getConnectedApplications:`, `_rpc_forwardGetListing:` with a listing revision counter instead of a sleep, `_rpc_forwardSocketSetup:` (sender key per page connection, `WIRMessageDataTypeChunkSupportedKey: 0`, no auto-pause), `_rpc_forwardSocketData:` (`WIRSocketDataKey` = raw WIP JSON), `_rpc_forwardDidClose:` on close; receive `_rpc_applicationSentData:` routed by `WIRDestinationKey` to the right `MessageConn`, plus the application and listing updates. Target routing stays in `webkit.Session`; the direct `MessageConn` carries the same JSON that iwdp relays.
- **Socket ownership.** go-ios `ConnectToService` and `ConnectLockdownWithSession` leave usbmux sockets open on some error paths and take no context (connect.go lines 90-109, 227-247; usbmuxconnection.go lines 80-90), and go-ios issue #880 reports that failed lockdown sessions leak sockets and can leave a device unreachable until reboot. W2 therefore writes a narrow adapter that dials the usbmux socket itself with a context deadline, owns and closes every intermediate connection on every path, and uses go-ios only for steps that accept a provided connection (lockdown messages, pair record request, TLS setup). If a needed step cannot take a provided connection, W2 copies that function into the adapter with the MIT notice (or uses an upstream fix once merged) instead of calling the leaking path. Lockdown failures get at most 2 retries with backoff, inside the operation deadline.
- **Frames and limits.** The go-ios plist reader rejects an oversized frame after reading only the length header and leaves the body in the stream (plistcodec.go lines 102-116). All pages of a device share that stream, so W2 owns the frame reader:
  - `MaxIncoming` is a configurable frame cap, default 256 MiB. A frame above the cap is drained to `io.Discard` under a deadline so the stream stays aligned, and every page session of that device gets the event "device dropped an N-byte message; a pending reply may be lost". The call whose reply was dropped fails at its own deadline. If the drain does not finish in time, the adapter closes the device connection and every page session of the device fails with `ErrPageClosed`. An oversized frame is therefore a device-wide event, not a per-page error.
  - `MaxOutgoing` stays 16 MiB (the go-ios `maxMessageSize`; the device-side limit is not measured).
  - Operations with a known large result (full-page screenshots, heap snapshots) are refused before sending when their estimate exceeds the cap. Results above the cap need chunk reassembly (`WIRMessageDataTypeChunkSupportedKey: 1` with Partial and Final messages), which is not in v1; the heap snapshot command then prints "use --transport iwdp".
- **Selection.** `--transport direct` first; `auto` chooses direct only when iwdp is not running, and only after the device test in 8.3 passes on iOS 16.7 and on one iOS 17 or later device. Never use direct and iwdp on the same device at the same time: both connect to webinspectord, and page connections take pages from each other.
- **Cost.** go-ios v1.3.2 requires `go 1.26.0`; iwdp-cli is at `go 1.25.7`, and a 1.25.7 toolchain fails to build with it (spike B). Linked modules grow from 8 to 20, the stripped binary from 9.70 MB to 10.68 MB, no cgo, all licenses MIT, BSD or Apache. Decision D5 accepts go-ios and the Go 1.26 floor. If the adapter cannot avoid every leaking path, the fallback is a minimal own usbmuxd and lockdown client behind the same interface (mobium, Apache-2.0, shows that this is possible without dependencies).
- **Wrapped surface** (W2 defines it inside its package; nothing outside `direct` imports go-ios):

```go
type deviceDialer interface {
	Devices(ctx context.Context) ([]transport.Device, error)           // USB only, with OSVersion from lockdown
	Dial(ctx context.Context, udid string) (io.ReadWriteCloser, error) // adapter: usbmuxd, lockdown, TLS, "com.apple.webinspector"; owns every socket
}
```

- **iOS 17 and later.** go-ios selects the tunnel shim (`com.apple.webinspector.shim.remote`) only when the device entry has RSD. go-ios's own e2e harness, mobilecli and mobium use the lockdown path without a tunnel on iOS 17 to 26, but nobody in this project has run it. If it fails on a current iOS device, the tunnel path needs `ios/tunnel` (gvisor, quic-go) and root or a userspace agent; then the direct transport stays limited to iOS 16 and iwdp remains the path for newer devices (R4).


## 7. Workstream plan

### 7.1 Order, early proofs, sizes and merge order

```
W0 (running) ──► merge into feat/agentic-parity
                  │
                  ▼
W1a contracts and proofs ──────────────┬──────────────┬──────────────┐
  interfaces, scheduler, registry,     │              │              │
  fakewebkit, proofs P1 and P2         ▼              ▼              ▼
                  │                  W2 direct    W3a runtime,    W6 skeleton
                  ▼                  transport    snapshot, refs  and drafts
W1b daemon, leases, reservations,                     │
    legacy bridge │                                   │
                  ├───────────────┬───────────────────┤
                  ▼               ▼                   ▼
                 W4 DevTools ──► W3b actions, waits, dialogs, tabs, annotate
                  │               │
                  └──────┬────────┘
                         ▼
                        W5 CDP endpoint (needs W1b, W3a, W3b, W4 and proof P2)
                         ▼
                        W6 final docs, skills, prompt drafts
```

- W1a is a small first merge: `internal/transport` interfaces; `webkit.Caller` and `webkit.Session` (routing, active and setup targets, deadlines, wire-byte limits); the `core.Scheduler`, `core.DesiredState` and `core.BootstrapParts` types with operation classes and epoch fencing; the `internal/commands` registry with CLI and MCP binding; `internal/webkit/testutil/fakewebkit`; `internal/runtime` API stubs (`Call`, `CallAsync` signatures); the `internal/refs` interface; the esbuild test dependency (D4). After it merges, W2, W3a and W6 start. W3a can run against WebKitGTK and `IWDP_NO_DAEMON=1` before W1b lands.
- W1a also runs two contract proofs. Their code lives in `e2e/proofs/` (Go test package plus a Node directory with its own lockfile); W5 deletes that directory when its own suite covers the same scenarios.
  - **P1, provisional setup.** On WebKitGTK, a cross-origin navigation between two local ports. The session sends `Page.enable` and a bootstrap part to the provisional target before commit. Pass: the first inline `<script>` of the new document sees the marker set by the bootstrap part in 10 of 10 runs, with pause-on-start on and off; the active target changes at commit; an abandoned provisional target is dropped.
  - **P2, CDP attach and utility world.** A minimal bridge (tab and page targets, filtered auto-attach, one utility-world alias, `Runtime.evaluate` and `callFunctionOn`, `DOM.resolveNode`, `Performance.enable`) in front of WebKitGTK. Pass: pinned playwright-core 1.65.0 `connectOverCDP` reads `locator('#target').textContent()`, and pinned Puppeteer 25.16.0 `connect` completes `page.waitForSelector('#target')`; both through the utility-world alias. P2 also gives agent-browser 0.41.2 `--cdp` the exact tokenized `ws` URL that the minimal bridge issues and checks that agent-browser connects through it with the token path intact (5.1).
  - A failed proof does not block the W1a merge. It amends the design before the dependent work starts: a P1 failure labels "runs before page scripts" as partial in 2.7, 3.7 and the help texts before W3b and W4 start; a P2 failure amends 5.2 before W5 starts.

Sizes. These are planning assumptions, not acceptance criteria. The "now" column adds the contracts of this revision (ownership, deadlines, fencing, provisional setup, CDP compatibility, authorization, the direct-transport adapter) and the two proofs.

| WS | Before the review | Now | Main reasons |
|---|---|---|---|
| W1a | about 1200 lines | about 2200 lines, plus about 700 lines of proof code that W5 later deletes | scheduler and desired-state types, setup target in `webkit.Session`, wire-byte limits, larger fake, proofs P1 and P2 |
| W1b | about 2300 lines (W1 L, 3500 in total) | about 3800 lines (W1 XL, 6000 in total) | reservations and preflight, `lost` and takeover, background recovery, operation scopes for legacy calls, reference-counted domain ownership, raw-call gates, lstat socket checks |
| W2 | M, about 1300 lines | M to L, about 2000 lines | own usbmux dialing and socket ownership, frame reader with drain, fake usbmuxd with failure injection |
| W3a | L, about 2100 lines plus fixtures | L, about 2500 lines plus fixtures | owner epoch and reset, serialized ref allocator, hand-written visibility oracle, wire-size tests |
| W3b | L, about 2500 lines | L, about 3000 lines | cancellation-aware input, per-control default actions, operation fencing in page loops, dialog acknowledgments |
| W4 | M to L, about 2500 lines | L, about 2800 lines | origin-scoped credentials, desired-state owners for every override and capture, early interception on provisional targets |
| W5 | XL, about 4000 lines | XL, about 5500 lines | tab and page targets with filters, world aliases, loader-id ownership, token and Origin checks, four pinned clients with semantic assertions |
| W6 | S to M | M | prompt and skill drafts for approval, finding-to-owner matrix |

When P1 and P2 report, the orchestrator re-estimates W1b and W5 against the proof code. If the P2 bridge needed clearly more than the planned minimal shape, W5 splits into W5a (target model, Target, Page, Runtime, DOM, Accessibility, Input) and W5b (Network, Fetch, Storage, Performance, Tracing, device runs of the compatibility suite).

Merge order. Every workstream works on its own branch from `feat/agentic-parity` and rebases before merge.
1. W0.
2. W1a (with the proof reports).
3. W1b.
4. W3a.
5. W2. It may swap places with W3a, because it touches only `internal/transport/direct` and `go.mod`; no other pair may change order.
6. W4 (before W3b because `wait --load networkidle` uses the W4 network tracker, 3.6).
7. W3b.
8. W5.
9. W6.

### 7.2 Shared-file rules

| Shared file | Only owner | Rule for others |
|---|---|---|
| `go.mod`, `go.sum` | W1a (esbuild test dependency, D4); W2 (go-ios and the Go 1.26 floor, D5) | others ask in their report |
| `cmd/iwdp-cli/main.go` | W1 | none; new commands are registry files |
| `cmd/iwdp-mcp/main.go` | W1 (one call that registers registry tools; daemon-backed `getClient`) | none |
| `internal/webkit/client.go`, `types.go` | W1 | others use `webkit.Caller` |
| `internal/core/{scheduler.go, desired_state.go, bootstrap.go}` | W1 | others register owners, desired-state entries and bootstrap parts through the API; they do not edit these files |
| `internal/tools/*.go` | W1 (mechanical `*webkit.Client` to `webkit.Caller`) | W4 adds new code in `internal/devtools` instead of editing `tools/network.go` |
| `internal/webkit/testutil/mock.go` | W0 then W1 | new fakes go in `fakewebkit` |
| `e2e/proofs/**` | W1a | W5 deletes it (7.1) |
| `README.md`, `CLAUDE.md`, `skills/**`, `skill-data/**` | W6 | other workstreams write their notes in `docs/notes/<ws>.md`; W6 merges them |
| `e2e/*.go` (existing) | W1 (e2e/mcp_test.go asserts the tool list) | new e2e suites are new directories |

Acceptance lists below are numbered; "7.3 test 7" means item 7 of the W1 list. Section 7.10 maps every review finding to its tests.

### 7.3 W1: daemon, transport interface, core and registry

- Owns: `internal/transport/{transport.go, iwdp/, webkitgtk/}`, `internal/webkit/{session.go, caller.go, wsconn.go}` and the split of `client.go`, `internal/webkit/testutil/fakewebkit/`, `internal/core/` (Manager, PageSession, Scheduler, DesiredState, BootstrapParts, leases, reservations), `internal/daemon/` (server, client, auto-start, lifecycle, socket directory checks), `internal/commands/{registry.go, spec.go, cli.go, mcp.go, output.go}` and the command files `daemon.go`, `session.go`, `lease.go`, `device.go` (`device reserve`), `close.go`, `connect.go`, `doctor.go`, `batch.go`, `devices.go`, `pages.go`, `eval.go` (moved from W0's code), `internal/wip/methods_gen.go` and its generator, `cmd/iwdp-cli/main.go`, `cmd/iwdp-mcp/main.go`, `internal/tools/*` signature change, `internal/proxy` (parse `deviceOSVersion` and the busy hint), `e2e/proofs/**` (W1a).
- Provides: `transport.Transport`, `transport.MessageConn`, `webkit.Caller`, `webkit.Session`, `core.Manager`, `core.PageSession`, `core.Scheduler` (with the control lane and barriers of 2.10 and the daemon-owned attempt loop of 2.6), `core.DesiredState`, `commands.Register` and `commands.Env`, daemon API (2.2), `fakewebkit`.
- Consumes: W0's deadline and help code.
- Acceptance (tests run with `go test ./... -count=1`, with `fakewebkit` unless marked; the part that owns each test is named in brackets):
  1. [W1a] Target routing with no direct fallback: a fake that sends `targetCreated` after 2 s connects; one that never sends it fails after 3 s with the documented message; a fake that closes alternate reconnects before announcing a target gives one failed attempt and a successful second attempt inside the deadline.
  2. [W1a] Provisional setup and swap: on a provisional `targetCreated` the fake receives the desired state (`Page.enable`, all bootstrap parts, enabled domains, interception rules) addressed to the provisional target id before `didCommitProvisionalTarget`; at commit, pending calls on the old target fail with `ErrTargetSwapped` and the next call goes to the new target; commit plus the main-frame `frameNavigated` of the same document increase the generation once; an abandoned provisional target is dropped and receives nothing after its destroy; with pause-on-start on, `Target.resume` is sent for every paused target on the success, error, cancel and release paths.
  3. [W1b] Short deadline with a dead probe: the fake stalls a call and the probe; `eval --timeout 1s` exits 124 (decision D9) within 1.2 s, and the session is `stalled`; the fake received exactly one `Runtime.evaluate` for that eval (no retry). Background recovery reconnects once within 15 s while the lease is valid, and never for a `lost` page. With a live probe the call returns `ErrCallTimeout` at its deadline. A `RetrySafe` read is retried once after the reconnect, inside its own deadline.
  4. [W1a] Cancellation (2.6): (a) while queued: a mutating operation holds the page; a second one waits in the scheduler queue and its client disconnects; the fake receives nothing from the second operation, and the queue continues with the next one. (b) Lost cancel during a retry loop: the fake answers every attempt with `retry` (`covered`); the client cancels at 0.5 s of a 3 s deadline and a test hook drops the `__iwdp.cancel` message; the fake receives no attempt sent after the cancel, and the cancel message was sent within 1 s on the control lane. (c) Attempt in flight at the cancel: the result is "canceled; outcome unknown", and a queued successor mutation reaches the fake only after the attempt's reply or its slot-release boundary (2.6). (d) Lost attempt reply: no second dispatching attempt is sent, and the error says the page may have run the action. A fake runtime in `fakewebkit` enforces the page side of 2.6 (step ordinals, attempt end) on a fake page clock for the following cases. (e) Multi-step sequence: `type --delay 300ms "abc"` spans several attempt windows; the fake receives steps 1, 2 and 3 in order, each dispatched once, and each step only after the reply of the previous one. (f) Cancel between steps: the client cancels after the reply of step 1 and before step 2; no attempt for step 2 or 3 is sent. (g) Lost step reply: the fake runs step 2 and its reply is dropped; step 2 is not sent again, step 3 is never sent, and the error names step 2 of 3. The same holds for a `drag` with its reply of step 4 dropped. (h) Fake clocks: the page clock has a 5 s offset from the daemon clock, the measured round trip is 80 ms, attempt messages are delivered 200 ms late, and the attempt's reply and the cancel are both lost while a successor mutation waits. The `attemptEnd` that was sent maps to no later than the slot-release boundary under the worst-case offset; the fake page dispatches nothing from the predecessor after the successor is admitted; the successor is admitted no earlier than that boundary. A mapping uncertainty above 100 ms causes one new measurement, and when it stays above 100 ms the attempt is not sent and the clock error is returned. Moving the fake page's `Date.now()` by one hour changes nothing. Worst-rate slower clock: a newly measured mapping with `h0 = 999 ms`, `h1 = s = 1000 ms` and `p = 1000 ms`, a deadline beyond the attempt, and a page clock that runs at exactly `1 - rho` of the daemon rate; the attempt carries `attemptEnd = 1249.975 ms` (`pageMin(1250 ms)`), not 1250 ms. The reply and the cancel are lost, and a successor is admitted at `H = 1250 ms`. The fake page tries a predecessor event at daemon times `H` and `H + 0.001 ms`; both are refused as expired, and the predecessor dispatches nothing after the successor's admission. A second run with the deadline `D = 1100 ms` before `s + 250 ms` checks that `H = D`, that `deadline = pageMin(1100 ms) = 1099.99 ms`, and that the page refuses at `D`.
  5. [W1a] Nudge: the fake withholds a reply until the next message; the call completes within 300 ms because of the nudge.
  6. [W1a] Wire-byte limit: `Limits.MaxOutgoing` is checked on the serialized outer message. A property test over generated strings rich in quotes, backslashes and control characters checks that a message is written exactly when its Target-routed wire size is at or below the limit, and that a refused message gives "N bytes on the wire; the limit for this transport is M" before any write. A 20 MiB reply passes.
  7. [W1b] Ownership and arbitration (2.5): (a) attach refused with exit 4 when the fake iwdp listing shows the busy hint (empty `devtoolsFrontendUrl` with the frontend on), with no WebSocket dial; (b) attach refused with exit 6 when another owner holds the device reservation; (c) unknown ownership (listing in `-F` mode) is refused without a reservation and accepted with one; (d) steal: the fake closes the connection when a second client connects; the lease goes to `lost`; the fake connection count shows no automatic reconnect; the next command exits 4 with "ownership is unknown"; `--takeover` with the reservation reattaches and re-applies the desired state; `--takeover` without it exits 6.
  8. [W1b] Leases: two sessions on one page: the second gets the lease error at once; `--wait-lease 2s` succeeds after release; a `--wait-lease` longer than the command deadline ends at the deadline with exit 124 (decision D9); idle TTL expiry releases and runs cleanup (the fake receives `Network.setInterceptionEnabled false`, `Debugger.resume` and `__iwdp.release(epoch)` when they apply). `close --all` exits 2 as an unknown flag before any connection, and the other session's lease, refs and captures are unchanged; `close` in one session leaves the other session's lease and state unchanged.
  9. [W1b] Mixed entry points: a capture and a legacy `NetworkMonitor` both own Network; the legacy `Network.disable` becomes an owner release, and the fake receives `Network.disable` only after the last owner releases. A legacy raw call of a method that is not in the device's iOS command list, and the three CSS hang methods, are refused before sending. A legacy multi-send operation runs as one exclusive operation (a concurrent read waits). Release and reacquire while an old operation is still running: the old operation's later sends are rejected by epoch, and the fake receives none of them.
  10. [W1a] Event routing and bad replies: while a subscriber blocks on an event flood, replies still arrive within 50 ms (bounded per-subscriber queue, never the read loop); malformed and unsupported-method replies give errors and keep the session.
  11. [W1b] Daemon: auto-start under concurrent starts creates exactly one daemon (10 parallel clients); protocol mismatch path; idle exit; `SIGTERM` cleanup within 5 s; a socket directory that is a symlink, owned by another uid or not mode 0700 makes the daemon refuse to start.
  12. [W1a] Registry: help for every command prints without calling `Run` (table test over all registered commands); an unknown flag exits 2 before any connection; MCP schemas pass `codexCompatibleInputSchema`; names and aliases are unique; the MCP registration manifest has no duplicate tool name.
  13. [W1b] Legacy bridge: all existing `cmd/iwdp-cli` and `cmd/iwdp-mcp` tests pass unchanged against the daemon-backed `Caller`.
  14. [W1b] WebKitGTK (build tag `webkitgtk`): `eval 1+1` through CLI and daemon; p50 of 50 warm CLI evals at or below 50 ms on this host.
  15. [W1a] Proofs P1 and P2 (7.1) report pass or fail with their logs.
  16. [W1a] Control lane (2.10): (a) an exclusive operation sends a `Runtime.evaluate` whose reply the fake holds until it receives `Network.interceptContinue` for request `r1`, after it has emitted `Network.requestIntercepted{r1}`; the owner's continue for `r1`, sent while the evaluation waits, reaches the fake at once, and the evaluation completes within 1 s of its 10 s deadline. (b) A second continue for `r1`, a continue for an unknown request id, one with an old epoch and one from another logical session are refused, and the fake receives none of them. (c) An ordinary mutation submitted during the wait reaches the fake only after the evaluation ends. (d) Paused evaluation: the fake emits `Debugger.paused` and holds the evaluation until `Debugger.resume`; a resume sent during the wait passes, and the evaluation completes. (e) Cancel behind a wait: canceling the waiting operation sends `__iwdp.cancel` within 1 s while the operation still holds its slot. (f) A release during the wait closes admission, continues `r1` and resumes the debugger through the control lane, and only then sends its cleanup.
  17. [W1b] Network reinitialization barrier (2.4): owner A (a legacy `Network.setExtraHTTPHeaders{X-A}`) and owner B (a netlog capture with one interception rule) both own Network, and one request is paused; A sets an empty map. The fake receives, as one exclusive sequence with no other operation's message between them, `Network.interceptContinue` for the paused request, `Network.disable`, `Network.enable`, `Network.setExtraHTTPHeaders` with B's effective map (no `X-A`), interception enablement and B's rule. The fake never receives `X-A` with an empty value. B receives the gap marker, and a later completion for the continued request fails with "request was released by a network reset". A new header name or a changed value is sent without the barrier.
- Size: see 7.1.

### 7.4 W2: direct transport

- Owns: `internal/transport/direct/**` (adapter, WIR session layer, frame reader, `fakeusbmuxd` and `fakewebinspectord` test servers), `go.mod` and `go.sum` changes for go-ios, `docs/notes/w2.md`.
- Consumes: `transport.Transport`, `transport.MessageConn`, `fakewebkit` (for the WIP level). Provides: `direct.New(opts) transport.Transport`.
- Acceptance:
  1. A fake webinspectord (plist over a `net.Pipe`) drives: identifier report, application list, listing with revision counter, socket setup, data in both directions routed by `WIRDestinationKey` to two concurrent pages, `_rpc_forwardDidClose:` on close, application disconnect ends the page connections with `ErrPageClosed`.
  2. Oversized frame (cap set to 1 MiB in the test): the reader drains the frame under its deadline; the next frame, for page B, decodes correctly; every page session of the device receives the "device dropped an N-byte message" event; the call whose reply was dropped fails at its deadline instead of hanging. A drain that misses its deadline closes the device connection, and every page session of the device fails with `ErrPageClosed`. A full-page screenshot whose estimate exceeds the cap is refused before sending.
  3. Error paths at the lower boundary: a fake usbmuxd Unix socket injects a failure, and in a second run a stall, at every step (usbmuxd connect, `ReadPairRecord`, lockdown session start, TLS handshake, `StartService`, service connect, first read). After each run the fake's count of accepted connections equals its count of closed connections, the call returns within its context deadline, and lockdown retries stop after 2.
  4. Device test (gate `IWDP_DEVICE_TEST=1`, runbook 8.3) on iOS 16.7: devices, pages, `eval`, `snapshot -i`, `click` through `--transport direct`. One iOS 17 or later device run before `auto` may pick direct.
- Size: see 7.1.

### 7.5 W3a: runtime core, snapshot, refs, getters

- Owns: `internal/runtime/**` (Go API, `js/10-core.js` to `js/45-locate.js`, size and ES2017 tests), `internal/refs/**`, command files `snapshot.go`, `diff_snapshot.go`, `get.go`, `is.go`, `find.go` (locate part), `hit_test.go`, `read.go`; e2e fixtures `e2e/fixtures/runtime/**`; suite `e2e/webkitgtk/runtime_test.go`.
- Consumes: `webkit.Caller`, `core.PageSession`, `BootstrapParts`, registry. Provides: `runtime.Runtime`, the snapshot JSON model, the target resolver (`__iwdp.resolve`), `refs.Table`, `__iwdp.def(name, fn)` for W3b, `__iwdp.reset`, `__iwdp.release` and the operation fence (`opId`, epoch, deadline) for W3b.
- Acceptance:
  1. Renderer golden tests (no browser) for `-i`, `-c`, `-d`, `-u`, names with NBSP and zero-width characters, duplicate role and name, cursor-interactive suffix; expected text derived from agent-browser's `render_tree` rules (cite lines).
  2. WebKitGTK fixtures: forms, ARIA widgets, shadow DOM, same-origin iframe, hidden variants; each fixture has a hand-written expected snapshot.
  3. Visibility: on the hidden-variants fixture the runtime's result equals a hand-written expected table built from the Playwright rules, including the separate `display: contents` rule (3.4), both with `checkVisibility` deleted and with it present. Native `checkVisibility` is not the oracle.
  4. Refs: the same element keeps its ref across snapshots; a replaced unique element resolves with a warning; a replaced duplicate fails with `stale_ref`; after navigation the old ref fails with the "page navigated" message; ids never repeat in a session.
  5. Ownership handoff: session A takes a snapshot and releases; session B acquires the page. `__iwdp.reset` runs before B's first call; A's refs are `unknown_ref` for B; the page map holds only B's refs. Daemon restart against an unchanged document: the install owner differs, the reset runs, and an old ref fails with "daemon restarted; run snapshot".
  6. Concurrent snapshots: two snapshot commands of one session in parallel get disjoint ref ranges, and every returned ref resolves to its element.
  7. Size: the bundle is at or below 64 KiB of source; the serialized install message and the bootstrap message with all parts plus an escape-heavy init script (quotes, backslashes, control characters) are measured against `MaxOutgoing` of each transport, and a message over the limit fails before writing with the size error of 3.1. ES2017 check (D4) passes; install is idempotent; a page that overrides `getBoundingClientRect` after install does not change results.
  8. `get box`, `get styles`, `is visible` and `hit-test` replace the hand-written probes named in AGG [318], [319], [324], [331], [335] on a fixture that reproduces them.
- Size: see 7.1.

### 7.6 W3b: actions, waits, dialogs, tabs, annotated screenshots

- Owns: `internal/runtime/js/50-actionability.js` to `90-overlay.js`, command files `open.go` (open, back, forward, reload, pushstate), `click.go`, `tap.go`, `dblclick.go`, `fill.go`, `type.go`, `keyboard.go`, `press.go`, `hover.go`, `focus.go`, `check.go`, `select.go`, `scroll.go`, `swipe.go`, `drag.go`, `upload.go`, `mouse.go`, `wait.go`, `dialog.go`, `tab.go`, `frame.go`, `highlight.go`, `screenshot_annotate.go`, `find.go` (action part, in `find_action.go`); suite `e2e/webkitgtk/actions_test.go`.
- Consumes: W3a runtime, resolver and operation fence; W1 core (generation, lease moves, bootstrap parts); W4 network tracker for `networkidle`; W0 screenshot code.
- Acceptance (WebKitGTK unless marked; every test asserts the final page state, and the recorded event sequence is only an extra check against 3.5):
  1. Each action on fixtures: the recorded event sequence matches 3.5, and the final state (value, caret, checked state, form submitted or not, click counters) matches a hand-written expectation. Multi-step actions: `type --delay 300ms "abc"` gives "abc" exactly once across several attempt windows; `drag` dispatches its 10 steps in order, each once; `mouse move 100 100 --steps 5` gives 5 moves in order.
  2. Cancellation: a page that cancels `beforeinput` makes `fill` fail with "page canceled the input" and keeps the old value; a canceled `keydown` for Enter leaves the form unsubmitted and names no emulated default; a canceled `keypress` inserts nothing; `type` into a selection replaces the selection and leaves the caret after the inserted text.
  3. Covered element: `click` fails before dispatch and names the covering element; `--force` clicks. Canceled covered click, three runs of `click` with `--timeout 3s` on a covered element: (a) the client disconnects at 0.5 s and the cover is removed at 1 s; (b) the same timing with the `__iwdp.cancel` message suppressed (daemon test hook), so the page never learns of the cancel before the deadline; (c) no disconnect, the deadline expires at 3 s and the cover is removed at 3.5 s. In every run the page's click counter stays 0 until 5 s after the cover removal, and the daemon log shows no attempt sent after the cancel or the deadline (2.6). Multi-step cancellation: (d) `type --delay 500ms "abcdef"`, the client disconnects at 1.2 s; the value holds the characters of the acknowledged steps plus at most the step in flight, and no character is added in the following 5 s. (e) A daemon test hook drops the reply of step 2 of `type --delay 300ms "abc"` after the page ran it; the value is "ab", never "abb" or "abc", and the error names step 2 of 3. (f) The same hook on step 4 of `drag`: the page's pointer log ends at step 4, and no drop event fires.
  4. React-style controlled input (fixture with a value setter tracker) accepts `fill` and `type`.
  5. Default actions: `press Enter` in a form with a default button clicks that button; in a form with two text fields and no button it does not submit; `press Tab` moves focus; each result names its emulated default action. `check` on a hidden checkbox with a visible `<label>` checks it and reads the state back.
  6. Waits: each form succeeds and times out with exit code 3 and the last state; a cross-origin navigation (two local ports give a real process swap, spike A) during `wait --url` succeeds; `wait --load networkidle` uses the W4 tracker.
  7. Dialogs (3.7): (a) `dialog accept "x"` then a click on a prompt button gives "x" in the page; an unarmed confirm returns false and the click result reports it; no evaluate blocks. (b) Lost acknowledgment: arm accept and click a confirm button; a daemon test hook drops the click's reply and the `console.debug` marker, then closes the page connection after the page used the answer; reattach with `--takeover` (2.5); a confirm in the same document returns false; then navigate cross-origin, and a confirm in the new document also returns false; the answer is reported as expired, and the fixture's record of written answers shows the accept in exactly one document. (c) Withdrawal on purpose: arm accept without using it, then `open` a cross-origin URL; the answer is withdrawn and issued into the new document, and a confirm there returns true. With the reply of the withdrawing call dropped, the answer expires and the confirm returns false. The same with `reload` and with `reload --ignore-cache`: a daemon test hook pauses between the two phases, and a confirm that the old document opens then returns false (the answer is no longer there); the protocol log shows `Page.reload` only after the withdrawal reply, with `ignoreCache` true for `--ignore-cache`; in the new document the first confirm returns true and a second one returns false (the answer was written once). With the phase-1 reply dropped, the answer expires, the reload still runs, and a confirm in the new document returns false. (d) Console ownership: with no console capture running, Console stays enabled while the dialog override exists, and the dialog log lists each use. (e) After release, `window.confirm` is the native function again and the queue is empty.
  8. Early install: after a cross-process navigation, the first inline script of the new document sees the dialog override (its confirm is answered by the policy and logged) and a user init script.
  9. `open` works on WebKitGTK, where `Page.navigate` does not exist (spike A), and the command returns after load.
  10. Annotated screenshot: labels match refs, overlay is gone after capture (DOM check), `--if-changed` skips an identical capture.
  11. Tabs: `tab new` and `tab close` on WebKitGTK if MiniBrowser lists script-opened windows as targets (not checked in spike A; otherwise with the fake); hidden-tab probe tested with the fake.
- Size: see 7.1.

### 7.7 W4: DevTools extras

- Owns: `internal/devtools/**` (netlog ring buffer with bodies on demand, HAR 1.2 writer, route rules over WIP interception and the JS fallback, console and error buffer, Timeline to Chrome trace converter, ScriptProfiler to `.cpuprofile` converter, media and user-agent overrides, origin-scoped credentials, geolocation shim, state save and load), command files `console.go`, `errors.go`, `network_cmds.go`, `har.go`, `route.go`, `initscript.go`, `set.go`, `cookies_ext.go`, `storage_cmd.go`, `state.go`, `trace.go`, `profiler_cmd.go`, `vitals.go`, `diff_screenshot.go`; suite `e2e/webkitgtk/devtools_test.go`.
- Consumes: W1 core (DesiredState owners for re-apply after swaps and for cleanup, BootstrapParts), W3a runtime for JS fallbacks. Provides to W5: netlog events in CDP shape, interception API, trace converter. Provides to W3b: the network tracker for `networkidle`.
- Acceptance:
  1. Netlog: bounded by count and bytes (defaults 2000 entries, 64 MiB of bodies), survives a target swap (Network re-enabled on the provisional target), `network request <id> --body` returns the body for a request seen while capture ran and the WebKit error text otherwise.
  2. HAR validates against the HAR 1.2 schema (test fixture) and opens in a HAR viewer library test.
  3. Route: abort, fulfill with body and status, delay; JS fallback mode passes the same tests; `unroute` continues paused requests; release cleans up (the fake sees `setInterceptionEnabled false`). Early interception: a route set before a cross-process navigation intercepts a `fetch` made by the first inline script of the new document.
  4. Console: `console` after the fact returns at most the 100 replayed messages plus everything since the lease, and says when messages were dropped; after a reconnect or re-enable the replayed messages are not stored twice (fake replay and WebKitGTK); `errors` includes uncaught errors and unhandled rejections with stack.
  5. Trace: Timeline fixture converts to trace events that load in a trace parser test (no Chrome needed).
  6. `set media dark` changes `matchMedia('(prefers-color-scheme: dark)')` on WebKitGTK and is re-applied after reconnect.
  7. Credentials: after `set credentials --origin A`, a page on origin A loads a subresource from origin B, and a request to A redirects to B; the test HTTP servers show that no request to B carries `Authorization`. When the transport cannot enforce the origin scope, the command returns unsupported. The password never appears in the daemon log, status output or error text.
  8. Header removal with remaining Network owners (2.4; local HTTP server): with a netlog capture and one `network route` active, `set headers '{"X-A":"1"}'`, then `set headers '{}'`. The next request to the test server has no `X-A` field (absent, not empty); the route still applies; the netlog shows the gap marker and closes requests in flight across the reset as `capture_reset`; `network request <earlier id> --body` says that the body is no longer available. The same holds when the header owner is the legacy `set_extra_headers` tool.
- Size: see 7.1.

### 7.8 W5: CDP endpoint

- Owns: `internal/cdp/**` (server, authorization, target model, one file per domain), command files `cdp.go` (`cdp start|stop|status|log`) and `get_cdp_url.go`, `e2e/cdpcompat/**` (Go test driver plus a small Node project with a lockfile pinned to playwright-core 1.65.0, puppeteer-core 25.16.0 and chrome-devtools-mcp 1.12.0; agent-browser 0.41.2 from PATH with its checksum checked); deletes `e2e/proofs/` (7.1).
- Consumes: everything above, and the proof P2 report. Provides: CDP listener.
- Acceptance:
  1. Unit tests per domain with `fakewebkit` and a CDP test client: method mapping, error for unknown methods (never forwarded: the fake records zero unknown methods), target id stable across a swap.
  2. Authorization (5.1): a missing or wrong token is rejected on every HTTP endpoint and on the WebSocket upgrade; a foreign `Origin` and `Origin: null` are rejected; GET on `/json/new`, `/json/close` and `/json/activate` is rejected; a wrong `Host` header is rejected; the listener binds `127.0.0.1` only; the token does not appear in the daemon log or in `cdp status` output.
  3. Targets and auto-attach (sequence tests against the fake): a Puppeteer-style `setAutoAttach` whose filter excludes `page` at browser level attaches the tab, then its page; with no filter, Chrome's default filter applies; existing targets are announced as created, then attached; `getTargetInfo` without an id describes the browser; by default only the record's selected page is announced; an endpoint issued with `--scope device` adds the other pages of reserved devices; a page leased by another session is listed and never attached.
  4. Utility worlds: `createIsolatedWorld{worldName}` produces a named `executionContextCreated` with `isDefault:false` and an id distinct from the main context, per frame and document; calls on the alias run in the main world; a navigation clears the aliases with `executionContextsCleared` and new ids appear for the new document; no id is emitted under two names.
  5. Navigation identity: the `loaderId` returned by `Page.navigate` equals the one in `frameNavigated` and the `requestId` of the main-frame document request; a same-document navigation returns no loader id and emits `navigatedWithinDocument`; lifecycle events carry the page session id.
  6. Dialogs: `Page.handleJavaScriptDialog` returns the unsupported error; no `javascriptDialogOpening` is emitted; an automatic policy answer appears as a `Log.entryAdded` warning; a later dialog is answered by the session policy, never by an earlier CDP call.
  7. `Performance.enable` and `Performance.getMetrics` succeed on attach.
  8. Compatibility on WebKitGTK (gate `IWDP_CDP_COMPAT=1`; needs Node). Every client connects with the exact URL printed by `get cdp-url` (5.5), never a hand-assembled one. agent-browser `--cdp`: open, snapshot -i, click @ref, fill, get title, screenshot; Puppeteer: connect, goto, `$eval`, `waitForSelector`, click, type, screenshot, `accessibility.snapshot`; Playwright `connectOverCDP`: goto, evaluate, `getByRole().click()`, fill, a locator read through the utility world, screenshot, `ariaSnapshot`; chrome-devtools-mcp: take_snapshot, click by uid, navigate_page, list_network_requests, take_screenshot. Each scenario asserts behavior (locator found the element, navigation resolved, value changed). Each script records the methods it sent; an unknown method fails the test unless it is on the client's expected-missing list, where each entry names the optional feature it belongs to and was reviewed.
  9. Device: the scenarios of test 8 run on the device behind the device gate (8.3) before any of them is advertised as supported on iOS.
  10. Endpoint resolution (no browser; a recording HTTP and WebSocket server): each pinned client receives the exact output of `get cdp-url`. agent-browser 0.41.2 with the `ws` form opens exactly that WebSocket URL, token and endpoint path included, and makes no discovery request; with the `http` form it requests the unprefixed `/json/version` and gets 404, which is why `ws` is its form. playwright-core 1.65.0 and puppeteer-core 25.16.0 with the `http` form request `/t/<token>/e/<endpoint>/json/version` (Playwright with a trailing slash) and then open the advertised WebSocket URL. No request without the token gets discovery data.
  11. Logical session binding (5.2): (a) `iwdp-cli --session s1 snapshot -i`, then `iwdp-cli --session s1 get cdp-url`; Puppeteer connects with that URL, attaches the tab and then its page, and evaluates in the page; s1's lease epoch is unchanged, and `iwdp-cli --session s1 click @eN` with a ref from the snapshot still works. (b) The tab and page attachments have distinct CDP session ids; detaching the page attachment removes only the state it set (an extra header it set is gone) and leaves the lease with s1. (c) A second connection through the same URL joins s1 without a lease change. (d) `iwdp-cli --session s2 get title` on the same page exits 4 with the lease message, and a Puppeteer connection through an endpoint issued for s2 lists the page but fails `Target.attachToTarget` on it with the lease message; in both cases s1's lease epoch and its CDP connections are unchanged. (e) After `iwdp-cli --session s1 close`, the s1 URLs get 404.
  12. Awaited intercepted fetch (2.10): Puppeteer with `page.setRequestInterception(true)` and a handler that answers `/resource` with `respond({body: 'ok'})`; `page.evaluate(() => fetch('/resource').then(r => r.text()))` returns "ok" within 2 s of a 10 s deadline. The same with Playwright `page.route('/resource', r => r.fulfill({body: 'ok'}))`.
- Size: see 7.1.

### 7.9 W6: docs, skills, agent prompts (drafts for approval)

- Owns: `README.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `skills/**` and `skill-data/**` of this repository, `docs/**` except `docs/design/`, embedded skills (`internal/skills/`, `skills get`).
- Skill layout (agent-browser's): `skills/<name>/SKILL.md` is a short discovery stub with the name and description of its guide and no bash block; it is the only file of its directory, because Pi finds nested SKILL.md files too. The guide is `skill-data/<name>/SKILL.md`, which the binary embeds and `iwdp-cli skills get <name>` prints, so an agent always reads the guide of the installed binary. Agents install the stub by a symlink of `skills/<name>` into an Agent Skills directory (README, Agent Skill).
- Agent prompts and active skills outside the repository (browser-checker prompt, safari-agent skill) are produced as **draft copies plus diffs staged for the user's approval**; W6 never edits the active agent configuration (decisions.md).
- Content: the agentic loop; session, reservation and lease rules; synthetic-input statement; impossible features; CDP how-to with tokens; device runbook; migration notes. Draft safety rules from AGG: no infrastructure writes and stop on a guard refusal (P0 [047]; P1 [015], [063], [108], [262]); build, origin and auth preflight and one final verdict contract (P0 [026], [108], [161], [292]); one bounded diagnostic budget then BLOCKED (P1 [097], [116], [165], [170]); default masking of password and token fields, 0600 permissions and deletion rules for HAR and state files, no dumps of account or security pages (P1 [191], [202], [304], [108], [212], [197]; agent-browser-skill [044]); cookie owner preservation and restore, with server-session revocation assigned to the auth helper (P1 [176], [177], [301]); bounded shell calls and known evidence directories, no filesystem-wide searches (P0 [305], [317]; P1 [179], [180], [268], [310], [312], [314], [330], [334]). Shell-level timeouts stay for external helpers that do not go through the daemon.
- A finding-to-owner matrix (`docs/notes/retro-coverage.md`) lists each P0 and P1 finding as solved by a workstream, constrained by a rule, or owned elsewhere (harness and provider gaps, auth helpers, container naming).
- Acceptance:
  1. Every registry command appears in the generated command reference.
  2. The examples of the skill guides (`skill-data/*/SKILL.md`) run against WebKitGTK in a doc test.
  3. The matrix covers every P0 and P1 finding of AGG.
  4. The prompt and skill drafts exist as copies plus diffs at the paths named in the W6 report, each safety rule above appears in them, and no file outside the repository was changed.
- Size: see 7.1.

### 7.10 Review finding coverage

The first design review of 2026-11-06 (/home/devkit/.pi/artifacts/reviewer/agentic-parity-design-20261106-161713.md) raised findings F1 to F15; the second (/home/devkit/.pi/artifacts/reviewer/agentic-parity-design-round2-20261106-164424.md) raised R2-1 to R2-6; the third (/home/devkit/.pi/artifacts/reviewer/agentic-parity-design-round3-20261106-170049.md) raised R3-1 to R3-3; the fourth (/home/devkit/.pi/artifacts/reviewer/agentic-parity-design-round4-20261106-170824.md) raised R4-1. Each one has a contract in this document and named tests in the workstream that owns the code.

| Finding | Contract | Tests |
|---|---|---|
| F1 ownership before attach, takeover after loss | 2.5 | 7.3 test 7 (busy, reserved, unknown, lost then `--takeover`); 7.8 test 3 (CDP scope) |
| F2 one deadline, retries, cancellation | 2.6, 3.1, 3.5 | 7.3 tests 3 and 4 (dead probe, queued cancel); 7.6 test 3 (canceled covered click) |
| F3 setup on provisional targets | 2.7 | 7.3 tests 2 and 15 (P1); 7.6 test 8 (first-script marker, confirm); 7.7 test 3 (fetch) |
| F4 one operation boundary for legacy, registry and CDP | 2.4, 2.10 | 7.3 test 9 |
| F5 owner epoch, refs, dialog state | 3.1, 3.3, 3.7, 2.4 | 7.5 tests 5 and 6 (A releases and B acquires, restart, concurrent snapshots); 7.6 test 7 (consumed answers across a navigation, release); 7.3 test 8 |
| F6 utility worlds | 5.2 | 7.8 tests 4 and 8; 7.3 test 15 (P2) |
| F7 tab targets, auto-attach, loader ids, `Performance.enable` | 5.2 | 7.8 tests 3, 5 and 7 |
| F8 CDP dialogs | 5.2, 3.7 | 7.8 test 6 |
| F9 endpoint authorization | 5.1, 2.2 | 7.8 test 2 (token, Origin, PUT, redaction); 7.3 test 11 (socket directory) |
| F10 credentials | 4.2 (`set credentials`) | 7.7 test 7 |
| F11 go-ios error paths and oversized frames | 6 | 7.4 tests 2 and 3 |
| F12 input cancellation and default actions | 3.5 | 7.6 tests 1, 2 and 5 |
| F13 device gate and fake failure shapes | 3.4, 8.1, 8.3 | 7.5 test 3; 7.3 tests 1 and 10; 7.7 test 4; 7.8 test 9 |
| F14 wire bytes | 3.1, 2.8 | 7.3 test 6; 7.5 test 7 |
| F15 prompt and skill drafts, retro coverage | 7.9 | 7.9 tests 3 and 4 |
| R2-1 cancellation when the cancel message is lost | 2.6, 3.1, 3.5 | 7.3 test 4 (lost cancel, attempt in flight, lost attempt reply); 7.6 test 3 (cancel at 0.5 s with the cancel message lost, cover removed at 1 s, 3 s deadline: no click) |
| R2-2 dialog answers without a durable acknowledgment | 3.7, 2.4, 2.7 | 7.6 test 7 (lost acknowledgment with reconnect and navigation, withdrawal, Console ownership) |
| R2-3 control messages blocked behind the operation they complete | 2.10, 2.5, 2.6 | 7.3 test 16 (intercepted request, paused evaluation, cancel behind a wait, release barrier); 7.8 test 12 (awaited intercepted fetch with Puppeteer and Playwright) |
| R2-4 agent-browser drops the path of an HTTP base URL | 5.1, 5.5 | 7.8 test 10 (exact issued URL per pinned client); 7.8 test 8; 7.3 test 15 (P2 with agent-browser) |
| R2-5 header removal while Network stays enabled | 2.4, 2.10 | 7.3 test 17 (barrier sequence); 7.7 test 8 (removed header absent on the wire, capture discontinuity) |
| R2-6 logical session of a CDP connection | 5.2, 5.1, 2.5 | 7.8 test 11 (CLI to CDP sharing, tab and page attachments, competing session) |
| R3-1 one dispatch per operation blocks multi-step actions | 2.6, 3.1, 3.5 | 7.3 test 4 (e to g: delayed typing over several attempts, cancel between steps, lost step reply, drag); 7.6 test 1 (multi-step final state); 7.6 test 3 (d to f) |
| R3-2 clock tolerance outlives the slot-release boundary | 2.6 (clock bounds), 3.1 | 7.3 test 4 (h: fake clocks with offset, late delivery, lost reply and cancel, queued successor, refresh and fail closed, wall clock change) |
| R4-1 drift allowance taken at the mapping's current age, not through `H` | 2.6 (clock bounds: projection and page expiry), 3.1 | 7.3 test 4 (h: worst-rate slower page clock, new mapping, lost reply and cancel, predecessor refused at and just after successor admission, deadline projected separately) |
| R3-3 reload cannot withdraw answers in the same runtime call | 3.6, 3.7 | 7.6 test 7 (c: `reload` and `reload --ignore-cache`, answer absent between phases, written once, lost phase-1 reply) |
| Decision: `close` ends only the caller's session (no `close --all`) | 4.2, 2.5 | 7.3 test 8 |

## 8. Test strategy

### 8.1 Unit tests on a scriptable fake WebKit server

`internal/webkit/testutil/fakewebkit` (W1a) replaces the need for a device in unit tests. The existing `testutil.MockServer` stays for legacy tests.

| Fake feature | Purpose |
|---|---|
| Target routing with configurable `targetCreated` delay or none | connect rules (2.6) |
| Handler per method; record of every received method, params and target id | assert desired-state setup, replay and cleanup |
| `Stall(method)`: accept and never answer | deadlines, probe, background recovery |
| `Withhold`: hold replies until the next message arrives | iOS 26 nudge |
| `HoldUntil(method)`: hold a reply until a named message arrives, after emitting `Network.requestIntercepted` or `Debugger.paused` | control lane (2.10) |
| `Swap()`: provisional create, commit, destroy; setup traffic to the provisional target is recorded before commit | 2.7 |
| `AbortProvisional()`: provisional create, then destroy without commit | abandoned provisional targets (2.7) |
| `Steal()`: close the first connection when a second one opens | leases, `lost` state (spike A step C) |
| `ReconnectGap(d)`: after a close, new connections get no `targetCreated` for `d` | reconnect stall (MAP fact 2) |
| `CloseBeforeTarget`: alternate reconnects close before announcing a target | failed connect attempts (spike A step B) |
| `MaxIncoming(n)`: close on larger client messages | wire-byte limits (spike A step D) |
| Large replies (tens of MiB) | read limits |
| Console replay: `Console.enable` replays the last 100 messages | console buffer without duplicate records (7.7 test 4) |
| Malformed replies and unsupported-method errors | session survives bad input |
| Event flood while a reply is pending | a slow subscriber never blocks reply routing; events go through a bounded per-subscriber queue, never the read loop |
| Fake iwdp listing HTTP (9221 and device ports, with `deviceOSVersion`; busy hint with the frontend on, always empty in `-F` mode) | discovery and the ownership preflight (2.5) |
| Fake hidden tab: accepts, never answers anything | tab probe |

Scenarios built on these features include mixed legacy, registry and CDP operations with release and reacquire while an old operation runs (epoch fencing, 7.3 test 9).

Plus: `fakeusbmuxd` and `fakewebinspectord` (W2) at the socket and plist level, golden-file renderer tests (W3a), CDP client harness (W5). All unit tests run with `go test ./... -count=1` in under 60 s and need no network, device or browser.

### 8.2 Local end-to-end tests on WebKitGTK (no phone)

Spike A shows that WebKitGTK 2.54 MiniBrowser runs headless under `broadwayd` and exposes the same WIP with Target routing. Suite layout: `e2e/webkitgtk/` with build tag `webkitgtk`; `make test-webkitgtk` runs `scripts/webkitgtk-setup.sh` (start `broadwayd :N`, start MiniBrowser with `WEBKIT_INSPECTOR_HTTP_SERVER=127.0.0.1:<free port>` on a fixture page served by the test, teardown on exit) and `go test -tags=webkitgtk ./e2e/webkitgtk/...`.

- Transport `webkitgtk` (W1): lists targets by parsing the HTML listing at `/` (there is no `/json`), WebSocket path `/socket/<connection>/<target>/WebPage`.
- Safari 16.6 approximation: a bootstrap part (test only) deletes APIs newer than Safari 16.6 that the runtime might touch (`Element.prototype.checkVisibility`, `Array.prototype.at`, `structuredClone`) before the runtime installs.
- Process swaps: two local HTTP ports give real cross-origin navigations and the provisional-target sequence.
- Limits of this oracle: WebKitGTK is newer WebKit than Safari 16.6, has no iOS touch behavior and a 128 KiB message cap that iOS does not have. Device tests stay required for release.
- CI: the repository has no CI config today; the suite is opt-in locally. If CI is added, it needs `webkit2gtk-4.1` and `gtk3` (`broadwayd`).

### 8.3 Real-device tests (gated) and runbook

Build tag `device`; the suite refuses to run unless `IWDP_DEVICE_TEST=1` and a device reservation (2.5) exist. The orchestrator grants each device session; the suite never restarts or stops iwdp, never runs `iwdp-cli restart-iwdp`, and never navigates the visible tab without that grant (decisions.md).
- **Contracts fail the gate.** Shared semantic fixtures have hand-written expected results (snapshots, visibility, event outcomes, waits, dialogs). On iOS 16.7 any mismatch fails, except entries in `e2e/device/known-differences.yaml`, each with a reason and a reviewer. WebKitGTK output is never the expected answer.
- **Capability probes are separate and report-only**: interception, whether `Network.setExtraHTTPHeaders` merges or replaces the header map (2.4), the rate of the page's `performance.now()` against the host monotonic clock (2.6), `getResponseBody`, `Touch` construction, `DOM.getAccessibilityPropertiesForNode` cost, reconnect gap, large evaluate.
- The CDP scenarios of 8.4 run on the device under the same grant before any scenario is advertised as supported on iOS.
- Cleanup is session-scoped (`iwdp-cli close --session <test session>`); the suite checks that its own lease, interception and breakpoints are gone and does not touch other sessions.
- Target devices: iPhone X on iOS 16.7 (permanent contract) and a named iOS 26 or 27 device for current-version claims.

### 8.4 CDP compatibility tests

`e2e/cdpcompat` (W5), gate `IWDP_CDP_COMPAT=1`, needs Node and `npm ci` in that directory (decision D7). Each client script runs a fixed scenario against the CDP endpoint in front of WebKitGTK, records every CDP method sent, and writes a JSON result that a Go test checks. Versions are pinned: agent-browser 0.41.2, playwright-core 1.65.0, puppeteer-core 25.16.0 (the version chrome-devtools-mcp 1.12.0 pins), chrome-devtools-mcp 1.12.0. The same scripts run against the device behind the device gate.

### 8.5 Latency targets (measured, not assumed)

| Measure | Today (LAT) | Target, warm daemon | Where measured |
|---|---|---|---|
| CLI eval p50 | 1.2 s (device) | at or below 50 ms on WebKitGTK; at or below 300 ms on the iPhone | 7.3 test 14; device suite |
| navigate (open) p50, to load | 4.8 s (returns before load) | page load time plus at most 300 ms | device suite |
| snapshot -i on a 500-element page | n/a | at or below 500 ms on the iPhone | device suite |
| hang longer than the command deadline | 42 timeouts (heuristic count), max 1837 s | none | fake stall tests (7.3 test 3) |


## 9. Risks and open questions

| # | Risk or question | Impact | How to resolve |
|---|---|---|---|
| R1 | Another agent or tool outside the daemon takes a page (one connection per page) | commands fail with `lost`; captures stop | device reservations, non-disruptive preflight, `lost` without automatic reconnect, `--takeover` only with the device owner's approval (2.5); clients that ignore reservations are not stopped, and the docs say so; device test step for steal behavior behind iwdp |
| R2 | Reconnect stall after a close (about 10 s on iPhone; spike A saw every second quick reconnect fail on WebKitGTK) | slow recovery | daemon keeps connections open; 12 s target wait after a close; fake `ReconnectGap` tests; device suite measures the real gap |
| R3 | Synthetic input does not trigger behavior that checks `isTrusted` or needs a user gesture (popups, clipboard, media, fullscreen, some payment and auth widgets) | false negatives in checks | every result says `trusted: false`; skills require reports to state it; `emulateUserGesture` where WIP allows; R10 for trusted input |
| R4 | Direct transport on iOS 17 and later may need the tunnel | direct path limited to iOS 16 | one device run (8.3) on an iOS 17+ device before `auto` uses direct; iwdp stays the fallback |
| R5 | go-ios raises the Go floor to 1.26 and moves fast; issue #880 socket leak | build break for users with pinned toolchains; device unreachable | decision D5; pin v1.3.2; the W2 adapter owns every socket and the frame reader (section 6); fake usbmuxd failure tests (7.4 test 3) |
| R6 | Runtime size and WebKitGTK 128 KiB message cap; iwdp before 1.9.2 lags on payloads of 30 KB or more | install fails or lags | 64 KiB source budget plus a wire-byte check of every outgoing message (3.1); `doctor` checks iwdp version |
| R7 | iOS 26 site isolation puts cross-origin frames in separate targets | snapshot and actions miss cross-origin iframe content | v1 documents the limit; later: attach frame targets through Target routing (Playwright chose not to) |
| R8 | Playwright `connectOverCDP` expects isolated worlds, frame lifecycle and `Runtime.callFunctionOn` with execution context ids | some Playwright features fail | tier 1 emulation (5.2, 5.3); compatibility suite lists what works; document `contexts()[0]` only |
| R9 | Pages can tamper with `window.__iwdp` or built-ins (no isolated world) | wrong results on hostile pages | capture built-ins at install; version check; docs mark page output as untrusted (agent-browser trust boundary) |
| R10 | Trusted input through Safari Remote Automation (`Automation.performInteractionSequence` through a webinspectord automation session, go-ios `AutomationSession`, pymobiledevice3 automation_session.py) | could deliver real taps, but automation sessions open their own window and need Settings > Safari > Advanced > Remote Automation | research spike after W2, on the device; not in v1 |
| R11 | Network interception on device is unverified on 16.7 and reported broken on iOS 26.5 WKWebView (MAP 1.8) | route and mock may not work | device suite records it; JS `fetch`/XHR fallback mode (`--mode js`) |
| R12 | `getResponseBody` works only for requests seen while Network was enabled; inspect-webkit says Safari exposes no bodies | `network request --body` empty | capture starts with the lease; `Page.getResourceContent` fallback; device suite records it |
| R13 | Native dialogs before the override blocks the page | session stuck | bootstrap installs overrides before page scripts; detection message; R10 may allow answering |
| R14 | Unknown methods wedge the page target (MAP fact 9) | whole session hangs | CDP never forwards unknown methods; passthrough only for methods in the iOS version's command list |
| R15 | MCP tool count: 116 today plus about 40 new tools; some MCP clients limit tool counts | tools hidden or rejected | profiles (D2) |
| R16 | Daemon security: local socket and CDP port give full control of the phone's Safari | other local users or web pages could drive it | lstat-checked 0700 socket directory, per-daemon token on every CDP path, Origin rejection, PUT-only side effects (5.1) |
| R17 | Privacy: snapshots and network logs contain page text, tokens and signed URLs (AGG [128], [266], [316]) | secrets in transcripts | W0 cookie redaction carries into `cookies` and `network request` (header values of `Cookie`, `Set-Cookie`, `Authorization` redacted unless `--show-values`); URL fragments hidden in connection lines; device UDIDs shortened (AGG [287], [333]) |
| R18 | WebKitGTK as oracle is newer than Safari 16.6 | tests pass locally and fail on device | API-deletion mode (8.2), ES2017 check, device suite before release |
| R19 | Version skew between daemon and clients after an upgrade | confusing errors | protocol number in `/v1/hello` (2.2) |
| R20 | Hidden Safari tabs never answer, and WIP cannot raise a tab | commands time out | probe before first use; clear message; `tab` lists which tab answered last |
| Q1 | Does iwdp also stall a page for about 10 s after a client closes (MAP fact 2 is from direct webinspectord)? | sizing of reconnect budgets | device suite measures it with iwdp |
| Q2 | Does `Touch` construction work on iOS 16.7 Safari for synthetic `TouchEvent`? | tap fidelity | device suite; fallback already designed |
| Q3 | Cost of `DOM.getAccessibilityPropertiesForNode` per node on the iPhone | whether `--native-ax` is usable | device suite |

## 10. Decisions taken

Recorded in /home/devkit/.pi/artifacts/retro/browser-checker-20261106/decisions.md (2026-11-06).
- **D1** The MCP server is a daemon client and shares leases and refs with the CLI. No in-process MCP owner.
- **D2** The MCP default profile is `core` (about 40 agentic and key debugging tools). The full set is available with `IWDP_MCP_TOOLS=all` or `--tools all`. Registration uses a deduplicated manifest (2.10).
- **D3** The CDP endpoint is opt-in, default port 9230, with the token rules of 5.1.
- **D4** esbuild's Go API (`github.com/evanw/esbuild/pkg/api`) is a pinned test-only dependency that checks the runtime stays ES2017 syntax. It is a syntax guard only; API and behavior checks stay in fixtures and the device gate.
- **D5** The direct transport uses go-ios (its `ios` package only, through the W2 adapter of section 6) and the module's Go floor rises to 1.26.0 when W2 merges. Direct stays opt-in until the device gates of 7.4 pass per iOS version.
- **D6** An unanswered `confirm` returns false and `prompt` returns null while the session's override is active; the policy can change it; release restores normal behavior.
- **D7** Node-based CDP compatibility tests live in `e2e/cdpcompat` behind `make test-cdpcompat`, with a lockfile; they are not part of `go test ./...`.
- **D8** The session flag is `--session` (`IWDP_SESSION`), default `default`.
- Module files: W1a adds the esbuild test dependency; W2 adds go-ios and the Go floor; nobody else edits `go.mod`.

## 11. Spikes

Throwaway code for both spikes is not part of the repository. Copies kept for reference: /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/spikes/ (spike A Go programs and fixture page; spike B report). Working copies were under `/tmp/spike-a` and `/tmp/spike-b-goios`.

### 11.1 Spike A: WebKitGTK MiniBrowser as a local WIP target (run on this host, 2026-11-06)

Environment: `webkit2gtk-4.1 2.54.2-1`, `/usr/lib/webkit2gtk-4.1/MiniBrowser` (GTK 3), `broadwayd` from GTK 3, Go 1.27.1. No display was needed.

Commands:

```sh
cd /tmp/spike-a
timeout 900 broadwayd :7 &              # headless GTK backend (HTML5 display on port 8087, unused)
GDK_BACKEND=broadway BROADWAY_DISPLAY=:7 \
WEBKIT_INSPECTOR_HTTP_SERVER=127.0.0.1:9555 \
  timeout 900 /usr/lib/webkit2gtk-4.1/MiniBrowser file:///tmp/spike-a/page.html &
curl -s http://127.0.0.1:9555/          # HTML "Inspectable targets" page
curl -s -i http://127.0.0.1:9555/json   # 404 Not Found (also /json/list)
# Go programs in a copy of the repo (git archive into /tmp/spike-a/repo, cmd/spikea*), using internal/webkit:
go run ./cmd/spikea  ws://127.0.0.1:9555/socket/1/1/WebPage
go run ./cmd/spikea2 ws://127.0.0.1:9555/socket/1/1/WebPage page-8
go run ./cmd/spikea3 ws://127.0.0.1:9555/socket/1/1/WebPage
```

Results:
- Listing: `WebKitInspectorHTTPServer libsoup/3.8.1` serves only HTML. Each target row has title, URL and `window.open('Main.html?ws=' + window.location.host + '/socket/1/1/WebPage')`. No `/json`. The WebSocket path is `/socket/<connectionID>/<targetID>/WebPage`. The MiniBrowser warnings about GL and cursor themes are harmless.
- Protocol: on connect the server sends `{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"page-8","type":"page"}}}`. An unwrapped command gets no answer. This is the same Target routing as iwdp.
- Existing `webkit.Client` unchanged: connected in 2.5 ms with Target routing on. `Runtime.evaluate` 2 to 20 ms; 50 sequential evaluates in 103 ms (2.1 ms each). Click by `el.click()` worked. `Console.enable`, `Page.enable`, `Network.enable`, `DOM.getDocument`, `Page.snapshotRect` (PNG data URL) and `DOM.getAccessibilityPropertiesForNode` answered. `CSS.enable` answered (so the iwdp CSS hang is not a WebKit-wide behavior).
- Missing methods: `Page.navigate` gives `'Page.navigate' was not found` (matches its removal in iOS 26.4); `Input.dispatchMouseEvent` gives `'Input' domain was not found`.
- `Runtime.evaluate{awaitPromise:true}` returned the promise object (`{}`), so the parameter is ignored; `Runtime.evaluate` then `Runtime.awaitPromise{promiseObjectId}` returned 42.
- Step A, cross-origin navigation by `location.href` (file URL to http://127.0.0.1:9556): the evaluate answered `ok`, then `Target.targetCreated{targetId:"page-17", isProvisional:true}` (+5 ms), `Target.didCommitProvisionalTarget{oldTargetId:"page-8", newTargetId:"page-17"}` (+398 ms), `Target.targetDestroyed{page-8}`. The listing URL stayed `/socket/1/1/WebPage`.
- Step B, reconnect right after a close: three connect-plus-eval cycles 100 ms apart gave ok (13 ms), **failure** (no `targetCreated` in 3 s, then connection closed), ok (6 ms). The same pattern repeated in a second run. Quick reconnects are unreliable here too.
- Step C, second connection: the first connection was closed by the server ("connection closed") when the second connected. In one run the second connection also got no `targetCreated` and fell back to direct mode (`'Runtime' domain was not found`). Page stealing is WebKit behavior, not only iwdp behavior.
- Step D, sizes: a 20 MiB `returnByValue` string arrived in 1.4 s. Outgoing evaluate expressions of 60, 100, 120 and 127 KiB passed; 130 and 200 KiB closed the connection (libsoup incoming message limit, about 128 KiB).
- Step F, dialogs: after `setTimeout(() => alert('spike'), 0)` the next `Runtime.evaluate "2"` got no reply in 3 s. A native dialog blocks evaluation, as on iOS.
- `Element.prototype.checkVisibility` exists in WebKitGTK 2.54 (`typeof` is `"function"`), so polyfill tests must delete it first.

Conclusion: WebKitGTK is a usable local target for the transport, session, runtime, action, wait, dialog, swap and CDP tests. It needs a `webkitgtk` transport that parses the HTML listing. It does not prove Safari 16.6 behavior.

### 11.2 Spike B: go-ios as a Go library (research, no device)

Full report: /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/spikes/spike-b-goios-report.md (read go-ios v1.3.2, commit 2fb34682, MIT; builds measured in /tmp). Key results:
- Usable as a library: yes, for the `ios` transport package. `webinspector.New` uses lockdown `ConnectToService` unless the device entry has RSD, in which case it uses the tunnel shim `com.apple.webinspector.shim.remote`.
- Device choice: `ios.ListDevices` includes network devices (filter `ConnectionType == "USB"`); `ios.GetDevice(udid)` uses the env var `udid` or the first device when empty; socket from `USBMUXD_SOCKET_ADDRESS` or `/var/run/usbmuxd`; pair records through usbmuxd `ReadPairRecord`.
- `ios/webinspector` v1.3.2 limits: one shared event queue (cap 256, drops events; open PR #878), no `_rpc_forwardDidClose:`, no swap handling, listing by fixed sleep, chunking off (`WIRMessageDataTypeChunkSupportedKey: 0`), 16 MiB frame cap that ends the client, no reconnect, Target routing left to the caller (`SendCommand` returns only the outer ack). Page keys can collide across apps (open PR #879).
- Issue #876 (`--port` ignored by `ios webinspector cdp`) is CLI-only: docopt makes `--port` a list; `NewCDPServer(client, host, port)` takes the port directly. Fix PR #877 is open.
- Issue #880 (open): failed lockdown sessions leak usbmux sockets; devices can become unreachable until reboot.
- Cost: `go 1.26.0` required (a 1.25.7 toolchain fails: "requires go >= 1.26.0"); linked modules 8 to 20; stripped `iwdp-mcp` 9,695,392 to 10,682,528 bytes; no cgo; gvisor and quic-go are not linked unless `ios/tunnel` is imported; all linked licenses MIT, BSD or Apache.
- iOS 17 and later through lockdown: indirect evidence only (go-ios e2e harness on iOS 18 and 26.5 devices without RSD, mobilecli, mobium on iOS 26.6.2). Not run here.
- Other Go options: mobilecli (FSL-1.1-ALv2, do not copy), maestro-runner (Apache-2.0, own WIR layer), mobium (Apache-2.0, no dependencies), gidevice (MIT, no webinspector, stale), sonic-ios-bridge (AGPL).

## 12. Licensing and attribution

- iwdp-cli stays MIT. New dependencies allowed by this design: go-ios (MIT, D5), esbuild Go API as a test-only dependency (MIT, D4). gorilla/websocket and the MCP SDK are already used.
- Do not copy code from pymobiledevice3 (GPL-3.0), mobilecli (FSL-1.1-ALv2) or sonic-ios-bridge (AGPL-3.0). Behavior facts from them (message names, timings) are cited, not copied.
- Playwright (Apache-2.0) and agent-browser (Apache-2.0): prefer own implementations. If a worker adapts code (for example role tables from `roleUtils.ts` or key tables from `webViewInput.ts`), that file keeps the original copyright and license header, a `NOTICE` file at the repository root names the source and license, and the README license section mentions it.
- WebKit protocol method lists (`internal/wip/methods_gen.go`) contain names only; the generator header cites the WebKit source files (BSD-style license).
- axe-core (MPL-2.0) is not bundled.
