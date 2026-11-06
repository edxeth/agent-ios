# Design: agentic parity, CDP endpoint and direct transport for iwdp-mcp

Status: design for implementation. Date: 2026-11-06. Base branch: `feat/agentic-parity` (b42bb20).
Scope: make iwdp-cli and iwdp-mcp fast, agentic (parity with agent-browser 0.41.2), CDP-compatible, and able to reach the iPhone without ios_webkit_debug_proxy (iwdp).
Targets: Safari 16.6 on iOS 16.7 (iPhone X, permanent test device) and current iOS 26/27.

Evidence used (paths are absolute so that workers can open them):

| Tag | Source |
|---|---|
| INV | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/iwdp-inventory.md (commands, tools, hazards A1-H2 with file:line) |
| MAP | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/cdp-wip-mapping.md (WIP capability per iOS version, protocol facts 1-15) |
| AB | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/agent-browser-surface.md (agent-browser surface, usage counts, ranked parity list) |
| AGG | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/aggregate.md (1084 findings from 211 runs, regenerated 2026-11-06) |
| LAT | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/latency.md (status PARTIAL) and latency/summary.tsv: per-call latency of 12,735 tool calls in 336 past sessions |
| SRC | /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/sources/ (WebKit protocol JSON, Playwright webview backend, pymobiledevice3, mobilecli, agent-browser snapshot.rs) |
| SPIKE-A | Section 11.1 of this document (WebKitGTK MiniBrowser as a local target, run on this host) |
| SPIKE-B | Section 11.2 of this document (go-ios as a library) |

Words used in this document:
- **WIP**: WebKit Inspector Protocol, the protocol that Safari, iwdp and WebKitGTK speak.
- **CDP**: Chrome DevTools Protocol.
- **Page connection**: one inspector connection to one Safari tab. Safari allows one per tab; a new one takes the tab from the old one.
- **Target routing**: WIP wraps every page command in `Target.sendMessageToTarget` and every reply in `Target.dispatchMessageFromTarget` (CLAUDE.md).
- **Runtime**: the JavaScript that iwdp-mcp injects into the page (section 3). Not the WIP `Runtime` domain; that one is written `Runtime.*`.
- **Ref**: an element handle such as `@e7` that a snapshot prints and later commands accept.
- **Lease**: exclusive right of one named session to drive one page (section 2.5).

## 0. Summary

1. A long-lived **daemon** owns every page connection. The CLI, the MCP server and the new CDP endpoint are clients of the daemon. With bounded deadlines and hang detection it removes the multi-minute stalls that caused most wasted time. It also removes the per-command setup (HTTP probe, page listing, WebSocket dial, Target wait, reconnect stall; its size is not measured yet) and makes leases, waits, captures and session state possible.
2. A **transport interface** hides how the daemon reaches a page: through iwdp (today), directly through usbmuxd and webinspectord with the go-ios library, or through WebKitGTK on Linux for local tests. Target routing, provisional-target swaps, deadlines and reconnects live once, above the transport.
3. An injected **agent runtime** (ES2017, runs on Safari 16.6) produces agent-browser-compatible snapshots with `@eN` refs and performs actions, waits, dialog overrides and screenshot annotation. WIP has no Input, Dialog, Emulation or Accessibility domain, so this runtime is the only way to reach parity. All synthetic events have `isTrusted=false`; the output says so.
4. A **command registry** describes each command once. The CLI parser, help text, MCP tool schema and daemon dispatch come from it, so a new command is a new file, and `--help` can never run a command.
5. A **CDP endpoint** in the daemon translates CDP to WIP plus runtime calls, so agent-browser (`--cdp`), chrome-devtools-mcp, Playwright (`connectOverCDP`) and Puppeteer (`connect`) can attach. Unknown methods are answered by the bridge and never forwarded, because unknown methods can wedge the page target (MAP fact 9).
6. Seven workstreams (W1 to W6, with W3 split) own disjoint files. W1 first lands the contracts (interfaces, registry, fake WebKit server) so that the others can start in parallel.
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

LAT reads the long tail as rare stalls, not normal per-command overhead, and states that the benefit of session reuse is a hypothesis until measured. The design therefore treats deadlines and hang recovery as the first fix and session reuse as the second, and section 8.5 measures both. The category "latency-hang" holds 143 findings and 27632 s of wasted time (AGG). Six P0 findings are iwdp-cli or page-socket calls with no deadline that hung 13 to 30 minutes, and three P0 findings are pages that stayed stalled for minutes after a connection closed (AGG P0 [116], [165], [170]).

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
 |                    runtime.Runtime (injected JS), refs.Table, setup replay list     |
 +-------------------------------------------------------------------------------------+
        |  transport.Transport
        +--> iwdp       (HTTP listing on 9221/9224+, WebSocket per page)          default today
        +--> direct     (usbmuxd -> lockdown -> com.apple.webinspector, go-ios)   W2, opt-in first
        +--> webkitgtk  (WEBKIT_INSPECTOR_HTTP_SERVER listing, WebSocket)         local tests (spike A)
```

Decisions:
- **One daemon per user.** It is the only process that opens page connections. Two processes that each open a page connection take the page from each other (MAP fact 1; reproduced in spike A, section 11.1 step C).
- **The daemon is the same binary.** Both `iwdp-cli` and `iwdp-mcp` contain `internal/daemon` and start it as `<own executable> daemon run`. No third binary to install.
- **The MCP server is a daemon client**, not an in-process owner. Reason: the same agent often uses the MCP server and the CLI, and both must share leases and refs. The extra hop is a Unix socket round trip (well under 1 ms), small next to a WIP call. Open decision D1 in section 10.
- **No-daemon mode stays.** `IWDP_NO_DAEMON=1` runs the same core in-process for one command, for debugging and for the unit tests. It gives no leases across processes.

### 2.2 Local socket API

Socket: `$XDG_RUNTIME_DIR/iwdp/daemon.sock`, else `$TMPDIR/iwdp-$UID/daemon.sock`. The directory is created with mode 0700 and the daemon refuses to start if it is owned by another user. Protocol: HTTP/1.1 on the Unix socket (Go `net/http`; easy to debug with `curl --unix-socket`). Streams are NDJSON with chunked encoding.

| Method and path | Body | Answer |
|---|---|---|
| GET /v1/hello | none | `{version, protocol, pid, startedAt, transports[]}` |
| POST /v1/run | `{command, args[], flags{}, session, page, timeoutMs, cwd, json}` | NDJSON: `{"type":"line"}` and `{"type":"event"}` items, then exactly one `{"type":"result"}` or `{"type":"error","code","message","hint"}` |
| POST /v1/batch | `{commands:[...], bail}` | NDJSON of results in order (agent-browser `batch`) |
| POST /v1/raw | `{session, page, method, params, timeoutMs}` | `{result}` or `{error}`; used by legacy commands (2.10) |
| GET /v1/events | query `session, page, methods` | NDJSON event stream until the client closes |
| POST /v1/lease | `{op: acquire, release, break, renew, session, page, waitMs}` | lease state |
| GET /v1/status | none | devices, pages, sessions, leases, captures, CDP listener, transport health |
| POST /v1/shutdown | `{force}` | ok; graceful cleanup first (2.4) |

`protocol` is an integer. A client with a different protocol number asks the daemon to exit when the daemon has no leases, and starts its own version. If leases exist, the client fails with: "daemon version X (protocol N) is running with active sessions; run 'iwdp-cli daemon stop' when they finish". The CLI sends `cwd` so that relative output paths resolve in the caller's directory, never in the daemon's.

### 2.3 Auto-start, lifecycle and idle exit

- Start: a client connects to the socket. On ENOENT or ECONNREFUSED it takes an exclusive `flock` on `daemon.lock` in the same directory, checks again, then runs `<own executable> daemon run` with `Setsid`, stdin closed and stdout and stderr appended to `$XDG_STATE_HOME/iwdp/daemon.log` (fallback `~/.local/state/iwdp/daemon.log`). It polls the socket every 20 ms for up to 3 s. This matches agent-browser's auto-start.
- Commands: `iwdp-cli daemon run|start|stop|status|logs`.
- Idle exit: the daemon exits after 15 minutes with no requests, no leases, no running captures and no CDP clients (`IWDP_DAEMON_IDLE` overrides).
- Page connections that have no lease and no capture close after 10 minutes, so that other tools (for example Safari Web Inspector on a Mac) can take the page.
- SIGTERM, `daemon stop` and idle exit run the cleanup of section 2.4 for every page, with a 5 s total budget.

### 2.4 Cleanup duties

Today nothing undoes WebKit state (INV A10, D8, D10, F2). The daemon records every state change it makes in the page session's **setup list** and undoes it when a lease ends:

| State set by iwdp-mcp | Undo on release |
|---|---|
| Network interception and paused requests | continue every paused request, `Network.setInterceptionEnabled false` |
| Debugger enabled, breakpoints, paused | `Debugger.resume`, remove breakpoints, `Debugger.disable` |
| Forced pseudo state (hover) | `CSS.forcePseudoState` with an empty list |
| Overlay elements (annotate, highlight) | runtime `overlay.clear`, `DOM.hideHighlight` |
| User-agent, media and preference overrides | WebKit resets them on disconnect (MAP fact 13); the daemon also sends the reset explicitly |
| Dialog overrides and runtime | left in the page (harmless; removed on the next navigation) |

### 2.5 Sessions, leases and arbitration

- A **session** is a name chosen by the client: `--session <name>` or `IWDP_SESSION`; default `default` (same rule as agent-browser: "The default unnamed session is one browser shared with every other agent on the machine"). It holds the current page, refs, dialog policy, captures and init scripts.
- A **lease** binds one page to one session. Any page command acquires it implicitly. Release: `iwdp-cli close`, `session release`, idle TTL of 5 minutes without commands and without captures, or `lease break`.
- A page command from another session on a leased page fails at once (no queue) with: `page "<title>" is leased by session "<name>" since <time> (last command <age> ago); use --session <name>, wait, or run 'iwdp-cli lease break <page>'`. `--wait-lease <duration>` waits instead.
- Inside one session, commands that change the page (Command.Mutates) run one at a time per page in FIFO order. Read-only commands run in parallel, at most 8 protocol calls in flight per page (MAP fact 8: more than about 60 in flight breaks the session).
- Clients outside the daemon (another agent's own iwdp client, Safari Web Inspector) cannot be arbitrated. When the page connection closes without a request from the daemon, the lease moves to state `lost` and the next command fails with "page was taken by another inspector client". The daemon does not reconnect by itself, because two owners that both reconnect take the page from each other in a loop. The next explicit command from the lease owner reconnects.

### 2.6 Deadlines, heartbeats and hang detection

Default budgets (all overridable; W0 provides `--timeout` and `IWDP_TIMEOUT`):

| Budget | Default | Reason |
|---|---|---|
| Command | 30 s (W0); waits 25 s like agent-browser | no unbounded call |
| One protocol call | min(remaining command time, 10 s) | |
| WebSocket handshake | 5 s (W0) | gorilla default is 45 s (INV A7) |
| Wait for `Target.targetCreated` | 3 s; 12 s right after a close | reconnect stall about 10 s (MAP fact 2) |
| Liveness probe | 2 s, `Runtime.evaluate "1"` | mobilecli uses 2 s |
| Reply nudge | 250 ms | iOS 26 holds replies until the next message (MAP fact 7) |
| Lease idle TTL | 5 min | |

No direct-mode fallback: the transport states whether the endpoint uses Target routing (iwdp, WebKitGTK and webinspectord all do). If `Target.targetCreated` does not arrive in time, the connect fails with "page did not announce its target in 3 s; it may be held by another inspector or not visible". The 100 ms fallback of today stays only for the legacy mock server (MAP fact 3).

Hang detection for one call:
1. At +250 ms with no reply, send one nudge (`Runtime.evaluate "0"`, reply ignored). Only when the device is iOS 26 or later or the version is unknown.
2. At the call deadline, run the liveness probe. If the probe answers, return `ErrCallTimeout` ("WebKit accepted the command and did not answer; the page is alive"). The session stays.
3. If the probe fails too, mark the session `stalled`, close it, reconnect once (dial 5 s, target wait 12 s), replay the setup list, and return `ErrSessionReset`. The command layer retries idempotent commands once; other commands report the reset.
4. A hidden tab accepts commands and never answers (MAP fact 12). Before the first command on a page, and before the first command after 20 s idle, the daemon runs the probe. A failed probe on a fresh connection gives: "tab is not answering; it is probably not the visible Safari tab. Bring it to the front on the device."
5. Every write has a write deadline (fixes INV B2).

Heartbeat: there is no periodic traffic to the phone. The probe in step 4 is lazy. MCP clients keep one `/v1/events` stream open; the daemon renews their leases while the stream lives and ends the stream with a final event on shutdown.

### 2.7 Provisional-target swaps and document generations

WebKit swaps processes on cross-origin navigation. The page connection receives `Target.targetCreated{isProvisional:true}`, then `Target.didCommitProvisionalTarget{oldTargetId,newTargetId}`, then `Target.targetDestroyed{old}`. Spike A reproduced this exact sequence on WebKitGTK (section 11.1 step A). Today the client keeps the old target id (MAP fact 5).

`webkit.Session` handles it:
1. On a provisional `targetCreated`, record the pending target. Do not route to it.
2. On `didCommitProvisionalTarget`, switch the routing target, fail calls still waiting on the old target with `ErrTargetSwapped`, increment the **document generation**, and run the setup list on the new target (`Page.enable` and bootstrap script, `Console.enable` and `Network.enable` when a capture runs, interception rules, overrides).
3. On `targetDestroyed` of the current target with no swap, the page is gone: fail with `ErrPageClosed`.
4. A main-frame `Page.frameNavigated` also increments the generation (same-process navigation).

The generation is the basis for ref staleness (3.3), runtime re-install (3.1) and wait logic (3.6). `Target.setPauseOnStart` is never used (MAP fact 6).

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
	MaxOutgoing int  // largest message the far side accepts: WebKitGTK about 128 KiB (spike A), iwdp 64 MiB
	MaxIncoming int  // read limit; 512 MiB today (client.go)
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
	Setup   *SetupList       // replayed after reconnect and swap; undone on release
	Boot    *BootstrapParts  // named parts joined into one Page.setBootstrapScript
}
```

`webkit.Client` and `webkit.NewClient(ctx, wsURL)` stay as a thin wrapper (a WebSocket `MessageConn` plus a `Session`) so that existing tests and tools keep compiling. `internal/tools` functions change their parameter from `*webkit.Client` to `webkit.Caller` (mechanical change, W1).

Bootstrap script: WIP has one `Page.setBootstrapScript` per session, and it runs only while the Page agent is enabled (MAP fact 13). `BootstrapParts` joins named parts (runtime from W3a, dialog override from W3b, user init scripts and the geolocation shim from W4) in a fixed order, each wrapped in `try { } catch (e) { }` so that one failing part does not stop the others.

### 2.10 How the CLI, the MCP server and the CDP endpoint share the core

- **CLI**: parses arguments with the registry spec (help and unknown flags are handled before any connection), sends `/v1/run`, prints the result (`--json` prints the raw result). Exit code: 0 ok, 1 command error, 2 usage error, 3 timeout, 4 page busy or lost, 5 no device or page.
- **MCP server**: registers one MCP tool per registry command that has an `MCPName` (schema generated from the spec) and forwards to `/v1/run`. The existing hand-written tools stay and use a daemon-backed `webkit.Caller` (`/v1/raw` and `/v1/events`) instead of their own connection.
- **Legacy CLI commands**: `connectToPage` returns the same daemon-backed `Caller`, so every one of the 105 existing commands gets a warm connection, deadlines and swap handling without being rewritten. Their client-side collectors (for example `console -d`) subscribe through `/v1/events`.
- **CDP endpoint**: `internal/cdp` in the daemon. Each CDP connection is a session named `cdp-<n>` unless the URL has `?session=<name>`. It uses the same `PageSession`, runtime, refs and leases.

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
	Mutates  bool     // serialized per page when true
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
- Size budget: the joined bundle must stay **at or below 64 KiB**, enforced by a unit test. Reason: the whole bundle goes in one `Runtime.evaluate`, and WebKitGTK closes the connection for any message above about 128 KiB (spike A step D: 127 KiB passed, 130 KiB closed the socket); Target routing escapes the payload a second time. Chunked install through page `eval` is not an option because page CSP can forbid `eval`, while `Runtime.evaluate` is not subject to CSP. iwdp 1.9.2 or newer is required for payloads at or above 30 KB (MAP fact 10); `doctor` checks the version.
- Version: `runtime.Version` is the first 12 hex digits of the SHA-256 of the bundle. The bundle defines `window.__iwdp` (non-enumerable) only if it is absent or has another version.
- Install paths: (1) on demand for the current document by `Runtime.evaluate`; (2) as the `runtime` part of the bootstrap script (2.9), so that new documents have it before page scripts run. Path 2 needs `Page.enable` in the session, which the daemon keeps.
- Every call is a small expression: `(window.__iwdp && window.__iwdp.v === "<ver>") ? window.__iwdp.call("<fn>", <json args>) : "__IWDP_MISSING__"` with `returnByValue: true`. On `__IWDP_MISSING__` the Go side installs and retries once. After `ErrTargetSwapped` it waits for the new target, installs and retries once if the command is idempotent.
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

- Allocation: the daemon keeps a per page-session counter (`refs.Table`). A snapshot call passes `nextRef`; the runtime gives new elements `e<nextRef++>` and returns the new counter. Ref ids are never reused inside a session, also across navigations (agent-browser rule: "Invalidated IDs are never recycled").
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
A test on WebKitGTK deletes `checkVisibility` before install and compares results with the native function on a fixture page.

### 3.5 Actionability and event sequences (`50-actionability.js`, `55-actions.js`, `60-keyboard.js`)

Actionability for click, dblclick, tap, hover, check, select, fill and type, retried every animation frame until the command timeout (default 5 s for actions; `--timeout` overrides): attached, visible (3.4), stable (same rect in two consecutive animation frames), enabled (no `disabled`, no `aria-disabled=true`, not inside a disabled `fieldset` or an `inert` subtree), and receives events: `scrollIntoView({block:'center', inline:'center'})` if the center is outside the viewport, then `elementFromPoint` at the center (through open shadow roots) must be the element or a descendant. Otherwise `covered` with the covering element described as `<tag id class>` (agent-browser form `covered by <...>`). `--force` skips the checks.

All events below are synthetic (`isTrusted=false`). Every action result has `"trusted": false`, and the CLI prints "synthetic event" once per session. Coordinates are client coordinates of the element center.

| Command | Sequence |
|---|---|
| click | pointerover, pointerenter, mouseover, mouseenter, pointermove, mousemove, pointerdown, mousedown, focus (if focusable and not already focused), pointerup, mouseup, click. `--button right` ends with contextmenu. `--new-tab`: `window.open(href)` for links, through `Runtime.evaluate{emulateUserGesture:true}` |
| dblclick | click sequence twice with `detail` 1 and 2, then dblclick |
| tap | touchstart and touchend with one touch point (constructor support on iOS 16.7 is unverified; fallback `PointerEvent{pointerType:'touch'}`), then the click sequence with `pointerType: 'touch'` |
| hover | pointerover, pointerenter, mouseover, mouseenter, pointermove, mousemove. CSS `:hover` is not applied by synthetic events (MAP 1.2). `--css` also sets `CSS.forcePseudoState{hover}` on the node (works through Target routing per iwdp issue 434) and keeps it until the next action or release (2.4) |
| focus / blur | `el.focus()` or `DOM.focus`; blur on `document.activeElement` |
| fill | focus; check editable (input types that take text, textarea, contenteditable; not `readonly`); select all; set the value through the prototype's native value setter (React-safe, MAP 1.2); dispatch beforeinput (`insertReplacementText`), input, change. contenteditable: `execCommand('selectAll')` then `execCommand('insertText')` |
| type | per character: keydown, keypress, beforeinput (`insertText`), value update through the native setter at the caret, input, keyup. `--delay ms` between characters |
| keyboard inserttext | beforeinput and input with the whole text at the caret; no key events |
| press / keydown / keyup | key events on `document.activeElement` with `key`, `code`, `keyCode`, modifiers and the legacy `keyIdentifier` (Playwright webViewInput.ts). Default actions are emulated because synthetic keys have none: Enter in a single-line input runs `form.requestSubmit()`, Enter on a link or button runs click, Space on a button or checkbox runs click, Tab and Shift+Tab move focus in tab order, Backspace and Delete edit the value, arrow keys move the caret or change a select. Each emulated default action is named in the result |
| check / uncheck | if the state differs, click the element, or its `<label>` when the input is hidden; read the state back; fail if it did not change |
| select | by value, then by visible label; multi-select takes several values; set `option.selected`, dispatch input and change |
| scroll | `scroll up\|down\|left\|right [px]` (default 300 px) on the window or `--selector` element; `scrollintoview <target>` |
| drag | pointer and mouse sequence from source center to target center in 10 steps, plus synthetic `dragstart`, `dragenter`, `dragover`, `drop`, `dragend` with a shared `DataTransfer`. Native HTML5 drag is not driven by synthetic events (MAP 1.2); the result says "best effort" |
| upload | the CLI reads the files and sends them base64; the runtime builds `File` objects, assigns `input.files` through a `DataTransfer`, dispatches input and change. Total size limit: 8 MiB on iwdp and direct, 64 KiB on WebKitGTK (message limit) |
| mouse move / down / up / wheel | the same events at coordinates; the runtime keeps the pointer position and button state for the session |

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

`open <url>`: `Runtime.evaluate("location.href = <json url>")` with a 1 s deadline and a lost reply accepted, because a committed navigation can destroy the target before it answers (MAP 1.6). `Page.navigate` is never used: it is removed in iOS 26.4 (MAP section 0) and in WebKitGTK 2.54 (spike A). Then `wait --load load` unless `--wait none`. `back`, `forward`: `history.back()` and `history.forward()` with the same wait. `reload`: `Page.reload` with the same wait.

### 3.7 Dialogs (`80-dialogs.js`)

WIP has no dialog events or commands, and a native dialog blocks `Runtime.evaluate` until a person dismisses it (MAP 1.4; spike A step F reproduced the block on WebKitGTK). The runtime therefore replaces `window.alert`, `window.confirm` and `window.prompt` through the bootstrap script and in the current document.

- Answers must be synchronous, so the agent arms them before the action: `dialog accept [text]` and `dialog dismiss` push an answer onto the session queue, which the daemon writes into the page (`__iwdp.dialog.queue`) and re-writes after each navigation.
- Default when the queue is empty: alert returns (accepted), confirm returns false, prompt returns null. `--dialog-default accept|dismiss` per session changes confirm and prompt (open decision D6).
- Every dialog is logged with type, message, answer and time. `dialog status` prints the log since the last command, and action results include the dialogs they caused, so an agent sees "confirm('Delete?') answered false" in the click result.
- `beforeunload`: not controllable; the runtime sets no handler and reports `unsupported`.
- Dialogs opened before the override exists (during early page load if the bootstrap is not set, or in cross-origin frames) still block. Detection: a probe timeout after an action, then the error "page is blocked, probably by a native dialog; dismiss it on the device". The direct transport may later answer them through a Safari automation session (section 9, R10).

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
- MCP profiles (open decision D2): `core` (agentic commands, about 40 tools), `devtools` (W4 extras), `legacy` (the 116 existing tools). `IWDP_MCP_TOOLS=core,devtools` selects; `all` registers everything.
- "WS" names the workstream that owns the command file.

### 4.2 Mapping table

| agent-browser | iwdp-cli command and flags | MCP tool | WIP mechanism | WS |
|---|---|---|---|---|
| **Navigation** | | | | |
| `open [url]` (aliases `goto`, `navigate`) | `open <url> [--wait load\|domcontentloaded\|networkidle\|none]`; aliases `goto`, `navigate` | `navigate` (+`wait_until`) | `location.href` by evaluate, then wait (3.6) | W3b |
| `back`, `forward` | `back`, `forward` `[--wait ...]` | `go_back`, `go_forward` | `history.back/forward` + wait | W3b |
| `reload` | `reload [--wait ...] [--ignore-cache]` | `reload` | `Page.reload` + wait | W3b |
| `pushstate <url>` | `pushstate <url>` | `pushstate` | runtime: Next.js router if present, else `history.pushState` + popstate | W3b |
| `read [url]` | `read [--outline] [--max-output n]` (rendered current page); `read <url>` fetches over HTTP on the host and needs no device | `read_page` | runtime text walker (visible text, headings outline) | W3a |
| `close [--all]` | `close [--all]`: ends the session, releases leases, runs cleanup; does not close the Safari tab | `close_session` | daemon | W1 |
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
| `set credentials` | `set credentials <user> <pass>` | `set_credentials` | `Authorization` header through `setExtraHTTPHeaders` (sent to every origin; the help says so) | W4 |
| `set geo` | `set geo <lat> <lng>` | `set_geolocation` | bootstrap shim of `navigator.geolocation` | W4 |
| `set offline` | `set offline on\|off` | `set_offline` | interception with error for all URLs; unverified on device | W4 |
| `set viewport`, `set device` | `set viewport` and `set device` exist and fail with the reason (4.3) | none | none | W4 |
| **Cookies and storage** | | | | |
| `cookies`, `cookies get` | `cookies [--show-values] [--name n]` (redacted by default, W0) | `get_cookies` | `Page.getCookies`; `Storage.getCookies` on iOS 27.3+ | W0 base, W4 |
| `cookies set`, `cookies set --curl` | `cookies set <name> <value> [--same-site] [--expires] ...`, `cookies set --curl <file>`, `--json <file>` | `set_cookie` | `Page.setCookie` with read-back (W0) | W0 base, W4 import |
| `cookies clear` | `cookies clear [--domain d]` | `clear_cookies` | `Page.deleteCookie` per cookie; scope is the frame resource URLs (MAP 1.8) | W4 |
| `storage local\|session` | `storage local [key]`, `storage local set <k> <v>`, `storage local clear` (same for session) | existing storage tools + `storage` | JS `localStorage` (no storage id needed) | W4 |
| `state save/load`, `--state` | `state save <path>`, `state load <path>` | `state_save`, `state_load` | cookies + storage of the current origin, JSON | W4 |
| **Sessions and diagnostics** | | | | |
| `session`, `session id/list/info`, `--session` | `session [id\|list\|info]`, `session release`, `lease break <page>` | `session_info` | daemon | W1 |
| `doctor` | `doctor [--json]`: daemon, transport, iwdp version (needs 1.9.2+), usbmuxd, devices, Web Inspector hint, visible tab probe, leases | `doctor` | daemon | W1 |
| `batch [--bail]` | `batch [--bail] "cmd" ...` | none (MCP calls are already batched by clients) | one `/v1/batch` request | W1 |
| `skills get/list/path` | `skills get\|list\|path` (skills embedded in the binary) | none | none | W6 |
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

### 5.1 Listener and HTTP surface

- Started by `iwdp-cli cdp start [--port 9230]`, `get cdp-url`, or the daemon flag `--cdp-port`. Not started by default (open decision D3). Default port 9230: 9221 to 9229 belong to iwdp, and 9222 is often used by a local Chrome (it is on this host).
- Binds `127.0.0.1` only. Rejects requests whose `Host` header is not `127.0.0.1:<port>` or `localhost:<port>` (DNS rebinding guard, as Chrome does). Optional `--cdp-token` adds a required `?token=` query.
- HTTP: `GET /json/version` (`Browser: "Safari/<iOS version> (iwdp-mcp <version>)"`, `Protocol-Version: "1.3"`, `webSocketDebuggerUrl` of the browser endpoint), `GET /json` and `/json/list` (one entry per page, `type: "page"`, `webSocketDebuggerUrl: ws://127.0.0.1:9230/devtools/page/<targetId>`), `PUT|GET /json/new?url` (`window.open`, see 3.8), `/json/close/<id>` (`window.close()`), `/json/activate/<id>` (probe only; error text if the tab is not visible), `/json/protocol` (the supported subset in CDP JSON form, generated from the handler table).
- WebSocket: `/devtools/browser/<id>` (flat sessions through `Target.attachToTarget{flatten:true}`; Puppeteer, Playwright, agent-browser) and `/devtools/page/<targetId>` (one page; chrome-devtools-mcp through Puppeteer uses the browser endpoint, older tools use this).

### 5.2 Target model

- One CDP target per Safari page of every connected device, `type: "page"`, `browserContextId: "default"`. `targetId` is stable for the life of the tab: an opaque string derived from the device id and the transport page id.
- WebKit provisional swaps (2.7) are hidden: the CDP target id does not change. The bridge emits `Runtime.executionContextsCleared`, `Page.frameNavigated`, `Runtime.executionContextCreated` and lifecycle events instead.
- Frames: the main frame id is the WIP frame id from `Page.getResourceTree`. Same-origin child frames are reported by `Page.getFrameTree`; cross-origin frame targets (iOS 26) are not exposed in v1.
- Execution contexts: one default context per frame, ids from WIP `Runtime.executionContextCreated`. `Page.createIsolatedWorld` returns the main-world context id, because WIP has no isolated worlds; Playwright's utility world then shares the main world (section 9, R8).
- Node ids: CDP `nodeId` is the WIP `nodeId` of the bridge's own page connection. CDP `backendNodeId` is a runtime handle number (the number of a ref or an internal handle), resolved in the page; the same element keeps its number for its document. This avoids one `DOM.requestNode` round trip per AX node.
- Leases: a CDP session that attaches to a page takes the lease for its session name. `Target.attachToTarget` on a page leased by another session fails with the lease message.

### 5.3 Domains and methods

Tier 1 is required for the four named clients (agent-browser method list taken from the strings of its 0.41.2 binary; Puppeteer and Playwright from their connect sequences; to be confirmed by the compatibility tests in 8.4, which log every method each client sends). Tier 2 adds DevTools extras. Tier 3 is optional.

| Domain | Methods and events | Implementation | Tier |
|---|---|---|---|
| Browser | getVersion, close (detach only), setDownloadBehavior (accepted, no effect), getWindowForTarget (synthetic), grantPermissions (error) | bridge | 1 |
| Target | setDiscoverTargets, getTargets, getTargetInfo, setAutoAttach{flatten}, attachToTarget, detachFromTarget, createTarget (`window.open`), closeTarget (`window.close`), activateTarget (probe), getBrowserContexts (default only); events targetCreated, targetDestroyed, targetInfoChanged, attachedToTarget, detachedFromTarget | bridge + transport page list (poll every 2 s while a client listens) | 1 |
| Page | enable, disable, getFrameTree, getResourceTree, navigate (`location.href` + wait; returns frameId and loaderId), reload, getNavigationHistory (current entry only), navigateToHistoryEntry (`history.go`), captureScreenshot (`snapshotRect`; clip; `captureBeyondViewport` uses Page coordinates; jpeg on the host), getLayoutMetrics (runtime), addScriptToEvaluateOnNewDocument and removeScriptToEvaluateOnNewDocument (bootstrap parts), createIsolatedWorld (main world), setLifecycleEventsEnabled, handleJavaScriptDialog (sets the answer for the next dialog), bringToFront (probe), stopLoading (`window.stop()`), setBypassCSP (error); events frameNavigated, navigatedWithinDocument (runtime `popstate`/`hashchange` hook), domContentEventFired, loadEventFired, lifecycleEvent (init, DOMContentLoaded, load, networkIdle synthesized), javascriptDialogOpening and javascriptDialogClosed (emitted from the dialog log, after the fact) | WIP + runtime | 1 |
| Runtime | enable, disable, evaluate (`awaitPromise` through `Runtime.awaitPromise`; `exceptionDetails` built from `wasThrown`), callFunctionOn (`objectId` goes to WIP; `executionContextId` is turned into a function object by evaluate first; `awaitPromise` through `Runtime.awaitPromise` on 16.6, where WIP callFunctionOn lacks it), getProperties, releaseObject, releaseObjectGroup, runIfWaitingForDebugger (no effect), addBinding and removeBinding (page function that writes a marked `console.debug` message; the bridge turns it into `Runtime.bindingCalled` and hides it from console output); events executionContextCreated (with `auxData{frameId, isDefault, type}`), executionContextsCleared, executionContextDestroyed, consoleAPICalled and exceptionThrown (from `Console.messageAdded`) | WIP + runtime | 1 |
| DOM | enable, disable, getDocument, querySelector, querySelectorAll, describeNode, resolveNode (`nodeId` to WIP; `backendNodeId` to runtime), requestNode, getBoxModel and getContentQuads (runtime rects; WIP has no box model method), scrollIntoViewIfNeeded, focus (WIP `DOM.focus`), getOuterHTML, setAttributeValue, removeNode, getFrameOwner, getNodeForLocation (runtime `elementFromPoint`), setFileInputFiles (runtime upload with host files) | WIP + runtime | 1 |
| Accessibility | enable, disable (no effect), getFullAXTree (from the runtime snapshot in full mode: `nodeId`, `role{type:"role"}`, `name{type:"computedString"}`, properties checked, expanded, selected, disabled, required, level, focusable, focused, `childIds`, `backendDOMNodeId` = runtime handle), getPartialAXTree, queryAXTree (runtime locator) | runtime | 1 |
| Input | dispatchMouseEvent (moved, pressed, released, wheel at x and y; click count; button state per session), dispatchKeyEvent (keyDown, keyUp, rawKeyDown, char; emulated default actions), insertText, dispatchTouchEvent, emulateTouchFromMouseEvent, synthesizeTapGesture | runtime (3.5); untrusted | 1 |
| Network | enable, disable, getResponseBody, getRequestPostData (stored from `requestWillBeSent`), setExtraHTTPHeaders, setCacheDisabled (`setResourceCachingDisabled`), setUserAgentOverride (`Page.overrideUserAgent`), getCookies, setCookie, deleteCookies, clearBrowserCookies (`Page.*` cookies; `Storage.*` on 27.3+), emulateNetworkConditions (`setEmulatedConditions` where the iOS command list has it, else error); events requestWillBeSent, responseReceived, dataReceived, loadingFinished, loadingFailed, requestServedFromCache (WIP `timing` converted) | WIP | 1 |
| Emulation | setEmulatedMedia (`setEmulatedMedia` + `overrideUserPreference`), setUserAgentOverride, setFocusEmulationEnabled (no effect, success), setGeolocationOverride (JS shim), setTouchEmulationEnabled (no effect: the device is touch), setDeviceMetricsOverride and clearDeviceMetricsOverride (error "viewport is fixed by the device", except an override equal to the real viewport, which succeeds) | WIP + runtime | 1 |
| Log | enable, disable; entryAdded from console messages with source network, security or other | WIP | 1 |
| Fetch | enable{patterns}, disable, requestPaused, continueRequest, fulfillRequest, failRequest (WIP interception; unverified on device, MAP 1.8); continueWithAuth (error) | WIP | 2 |
| Storage | getCookies, setCookies, clearCookies, clearDataForOrigin (JS storage only) | WIP + runtime | 2 |
| Performance | enable, getMetrics (navigation timing, DOM node count, JS heap from `Memory` where present) | runtime | 2 |
| Tracing | start, end; dataCollected, tracingComplete (Timeline events converted, W4) | WIP | 2 |
| Profiler | enable, start, stop (ScriptProfiler converted, W4) | WIP | 3 |
| Debugger, HeapProfiler, Overlay, CSS, Security | not in v1, except `Security.setIgnoreCertificateErrors` (error) and `Overlay.highlightNode` (`DOM.highlightNode`, tier 3) | | 3 |

### 5.4 Unsupported methods

- The bridge never forwards a CDP method that it does not translate. Unknown or unsupported methods get `{"code": -32601, "message": "'<Domain.method>' wasn't found"}`, the same text Chrome uses for a missing method, so clients that probe for optional features continue.
- Methods with no effect on iOS but that clients call on every attach (listed in the table as "no effect") return success, so that attach does not fail.
- Each unknown method is logged once per CDP session; `iwdp-cli cdp log` prints the counts. The compatibility tests fail on an unknown method that is not on an expected-missing list per client.
- Passthrough to WIP exists only behind `IWDP_CDP_PASSTHROUGH=<Domain.method,...>` and only for methods in the iOS command list of the device's version (MAP section 0). `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` are always refused (CLAUDE.md known limitation).
- The command lists per iOS version (16.4, 17.0, 18.0, 26.0, 26.2, 26.4, 27.0, 27.3) are generated into `internal/wip/methods_gen.go` from SRC/webkit-legacy-ios (method names are facts; the file header names the source). The bridge and the runtime layer use them to choose, for example, `Storage.getCookies` on 27.3 and later.

### 5.5 Client notes

- agent-browser: `agent-browser --cdp 9230 open <url>`, `snapshot -i`, `click @e3`. Its snapshot calls `Accessibility.getFullAXTree` and `DOM.resolveNode{backendNodeId}`; both are served from the runtime, so its refs and ours are separate but consistent.
- Puppeteer and chrome-devtools-mcp: connect with `defaultViewport: null` (otherwise Puppeteer sends `Emulation.setDeviceMetricsOverride`). chrome-devtools-mcp: `--browser-url http://127.0.0.1:9230`.
- Playwright: `chromium.connectOverCDP('http://127.0.0.1:9230')`, use `browser.contexts()[0]`; `newContext()` and `newPage()` in a new context fail (no browser contexts). Locators work through Runtime and DOM; `page.mouse`, `page.keyboard` and `page.touchscreen` go through Input (untrusted).


## 6. Direct transport (no ios_webkit_debug_proxy)

Based on spike B (section 11.2). Package `internal/transport/direct` (W2).

- **Device layer: go-ios `ios` package only.** `ios.ListDevices` filtered to `Properties.ConnectionType == "USB"`, `ios.GetDevice(udid)`, `ios.ConnectToService(device, "com.apple.webinspector")` over usbmuxd and lockdown with TLS, `ios.NewPlistCodecReadWriter` for framing. No tunnel and no root on this path. `USBMUXD_SOCKET_ADDRESS` overrides the socket; pair records come from usbmuxd (`ReadPairRecord`), not from `/var/lib/lockdown` directly.
- **Not go-ios `ios/webinspector`.** In v1.3.2 it has one shared event queue for all pages (open PR #878), no `_rpc_forwardDidClose:`, no target-swap handling, page listing by fixed sleep, a client that dies on its first read error, and a CDP translator that is the wrong shape for us. Two other Go tools (mobilecli, maestro-runner) also use only go-ios transport and wrote their own WIR layer.
- **Own WIR session layer (MIT, about 400 lines plus tests).** One long-lived webinspectord connection per device (the service stalls new connections for seconds after a close: mobilecli comments, MAP fact 2). Messages: `_rpc_reportIdentifier:`, `_rpc_getConnectedApplications:`, `_rpc_forwardGetListing:` with a listing revision counter instead of a sleep, `_rpc_forwardSocketSetup:` (sender key per page connection, `WIRMessageDataTypeChunkSupportedKey: 0`, no auto-pause), `_rpc_forwardSocketData:` (`WIRSocketDataKey` = raw WIP JSON), `_rpc_forwardDidClose:` on close; receive `_rpc_applicationSentData:` routed by `WIRDestinationKey` to the right `MessageConn`, plus the application and listing updates. Target routing stays in `webkit.Session`; the direct `MessageConn` carries the same JSON that iwdp relays.
- **Limits.** `MaxOutgoing` and `MaxIncoming` 16 MiB (go-ios `maxMessageSize`; a larger frame kills the plist codec). Heap snapshots above that need chunk reassembly (`WIRMessageDataTypeChunkSupportedKey: 1` and Partial and Final messages); not in v1. The heap snapshot command prints "use --transport iwdp" when the direct transport refuses a large result.
- **Robustness.** Bounded retries (at most 2) with backoff for lockdown failures, and explicit close of usbmux sockets on every error path, because go-ios issue #880 reports that failed lockdown sessions leak sockets and can leave a device unreachable until reboot.
- **Selection.** `--transport direct` first; `auto` chooses direct only when iwdp is not running, and only after the device test in 8.3 passes on iOS 16.7 and on one iOS 17 or later device. Never use direct and iwdp on the same device at the same time: both connect to webinspectord, and page connections take pages from each other.
- **Cost.** go-ios v1.3.2 requires `go 1.26.0`; iwdp-mcp is at `go 1.25.7`, and a 1.25.7 toolchain fails to build with it (spike B). Linked modules grow from 8 to 20, the stripped binary from 9.70 MB to 10.68 MB, no cgo, all licenses MIT, BSD or Apache. Open decision D5: accept go-ios and the Go 1.26 floor, or write a minimal usbmuxd and lockdown client (mobium, Apache-2.0, shows that this is possible without dependencies) behind the same interface.
- **Wrapped surface** (W2 defines it inside its package; nothing outside `direct` imports go-ios):

```go
type deviceDialer interface {
	Devices(ctx context.Context) ([]transport.Device, error)      // USB only, with OSVersion from lockdown
	Dial(ctx context.Context, udid string) (io.ReadWriteCloser, error) // ConnectToService("com.apple.webinspector")
}
```

- **iOS 17 and later.** go-ios selects the tunnel shim (`com.apple.webinspector.shim.remote`) only when the device entry has RSD. go-ios's own e2e harness, mobilecli and mobium use the lockdown path without a tunnel on iOS 17 to 26, but nobody in this project has run it. If it fails on a current iOS device, the tunnel path needs `ios/tunnel` (gvisor, quic-go) and root or a userspace agent; then the direct transport stays limited to iOS 16 and iwdp remains the path for newer devices (R4).


## 7. Workstream plan

### 7.1 Order and dependencies

```
W0 (running) ──► merge into feat/agentic-parity
                  │
                  ▼
W1a contracts (2-3 days) ──────────────┬──────────────┬──────────────┐
  interfaces, registry, fakewebkit     │              │              │
                  │                    ▼              ▼              ▼
W1b daemon, leases, legacy bridge    W2 direct    W3a runtime,    W6 skill and
                  │                  transport    snapshot, refs  doc skeleton
                  │                                   │
                  ├───────────────┬───────────────────┤
                  ▼               ▼                   ▼
                 W4 DevTools     W3b actions, waits, dialogs, tabs, annotate
                  │               │
                  └──────┬────────┘
                         ▼
                        W5 CDP endpoint (needs W1b, W3a, W3b; W4 for Network/Fetch/Tracing tiers)
                         ▼
                        W6 final docs, skills, agent prompts
```

- W1a is a small first merge: `internal/transport` interfaces, `webkit.Caller` and `webkit.Session` (routing, swaps, deadlines), `internal/commands` registry with CLI and MCP binding, `internal/webkit/testutil/fakewebkit`, `internal/runtime` API stubs (`Call`, `CallAsync` signatures) and `internal/refs` interface. After it merges, W2, W3a and W6 start. W3a can run against WebKitGTK and `IWDP_NO_DAEMON=1` before W1b lands.
- Every workstream works on its own branch from `feat/agentic-parity` and rebases before merge. Merge order: W0, W1a, then W1b, W2, W3a in any order, then W3b, W4, then W5, then W6.

### 7.2 Shared-file rules

| Shared file | Only owner | Rule for others |
|---|---|---|
| `go.mod`, `go.sum` | W2 (go-ios, Go 1.26 floor if D5 accepts) | others ask in their report; a test-only dependency (D4) goes in with W3a |
| `cmd/iwdp-cli/main.go` | W1 | none; new commands are registry files |
| `cmd/iwdp-mcp/main.go` | W1 (one call that registers registry tools; daemon-backed `getClient`) | none |
| `internal/webkit/client.go`, `types.go` | W1 | others use `webkit.Caller` |
| `internal/tools/*.go` | W1 (mechanical `*webkit.Client` to `webkit.Caller`) | W4 adds new code in `internal/devtools` instead of editing `tools/network.go` |
| `internal/webkit/testutil/mock.go` | W0 then W1 | new fakes go in `fakewebkit` |
| `README.md`, `CLAUDE.md`, `skills/**` | W6 | other workstreams write their notes in `docs/notes/<ws>.md`; W6 merges them |
| `e2e/*.go` (existing) | W1 (e2e/mcp_test.go asserts the tool list) | new e2e suites are new directories |

### 7.3 W1: daemon, transport interface, core and registry

- Owns: `internal/transport/{transport.go, iwdp/, webkitgtk/}`, `internal/webkit/{session.go, caller.go, wsconn.go}` and the split of `client.go`, `internal/webkit/testutil/fakewebkit/`, `internal/core/` (Manager, PageSession, SetupList, BootstrapParts, leases), `internal/daemon/` (server, client, auto-start, lifecycle), `internal/commands/{registry.go, spec.go, cli.go, mcp.go, output.go}` and the command files `daemon.go`, `session.go`, `close.go`, `connect.go`, `doctor.go`, `batch.go`, `devices.go`, `pages.go`, `eval.go` (moved from W0's code), `internal/wip/methods_gen.go` and its generator, `cmd/iwdp-cli/main.go`, `cmd/iwdp-mcp/main.go`, `internal/tools/*` signature change, `internal/proxy` (parse `deviceOSVersion`).
- Provides: `transport.Transport`, `transport.MessageConn`, `webkit.Caller`, `webkit.Session`, `core.Manager`, `core.PageSession`, `commands.Register` and `commands.Env`, daemon API (2.2), `fakewebkit`.
- Consumes: W0's deadline and help code.
- Acceptance (tests run with `go test ./... -count=1`; all with `fakewebkit` unless marked):
  1. Target routing with no direct fallback: a fake that sends `targetCreated` after 2 s connects; one that never sends it fails after 3 s with the documented message.
  2. Swap: fake emits provisional `targetCreated`, `didCommitProvisionalTarget`, `targetDestroyed`; pending calls fail with `ErrTargetSwapped`, the next call goes to the new target, the setup list replays (asserted on the fake's received methods), generation increments.
  3. Stall: fake never answers one call; with a live probe the call returns `ErrCallTimeout` at the deadline; with a dead probe the session reconnects once and returns `ErrSessionReset`.
  4. Nudge: fake withholds a reply until the next message; the call completes within 300 ms because of the nudge.
  5. Steal: fake closes the connection after a second client connects; the lease goes to `lost` and no automatic reconnect happens.
  6. Large messages: 20 MiB reply passes; an outgoing message above `Limits.MaxOutgoing` fails before writing with a clear error.
  7. Leases: two sessions, one page: the second gets the lease error at once; `--wait-lease 2s` succeeds after release; idle TTL expiry releases and runs cleanup (fake receives `Network.setInterceptionEnabled false` and `Debugger.resume` when they were set).
  8. Daemon: auto-start under concurrent starts creates exactly one daemon (10 parallel clients); protocol mismatch path; idle exit; `SIGTERM` cleanup within 5 s.
  9. Registry: help for every command prints without calling `Run` (table test over all registered commands); unknown flag exits 2 before any connection; MCP schemas pass `codexCompatibleInputSchema`; names and aliases are unique.
  10. Legacy bridge: all existing `cmd/iwdp-cli` and `cmd/iwdp-mcp` tests pass unchanged against the daemon-backed `Caller`.
  11. WebKitGTK (build tag `webkitgtk`): `eval 1+1` through CLI and daemon; p50 of 50 warm CLI evals at or below 50 ms on this host.
- Size: L (about 3500 lines with tests). W1a: about 1200 lines.

### 7.4 W2: direct transport

- Owns: `internal/transport/direct/**`, `go.mod` and `go.sum` changes, `docs/notes/w2.md`.
- Consumes: `transport.Transport`, `transport.MessageConn`, `fakewebkit` (for the WIP level). Provides: `direct.New(opts) transport.Transport`.
- Acceptance:
  1. A fake webinspectord (plist over a `net.Pipe`) drives: identifier report, application list, listing with revision counter, socket setup, data in both directions routed by `WIRDestinationKey` to two concurrent pages, `_rpc_forwardDidClose:` on close, application disconnect ends the page connections with `ErrPageClosed`.
  2. A 16 MiB plus 1 byte frame gives a clear error and does not kill other page connections of the device.
  3. Lockdown failure injection: at most 2 retries, all sockets closed (counted by the fake dialer).
  4. Device test (gate `IWDP_DEVICE_TEST=1`, runbook 8.3) on iOS 16.7: devices, pages, `eval`, `snapshot -i`, `click` through `--transport direct`. One iOS 17 or later device run before `auto` may pick direct.
- Size: M (about 1300 lines with tests).

### 7.5 W3a: runtime core, snapshot, refs, getters

- Owns: `internal/runtime/**` (Go API, `js/10-core.js` to `js/45-locate.js`, size and ES2017 tests), `internal/refs/**`, command files `snapshot.go`, `diff_snapshot.go`, `get.go`, `is.go`, `find.go` (locate part), `hit_test.go`, `read.go`; e2e fixtures `e2e/fixtures/runtime/**`; suite `e2e/webkitgtk/runtime_test.go`.
- Consumes: `webkit.Caller`, `core.PageSession`, `BootstrapParts`, registry. Provides: `runtime.Runtime`, the snapshot JSON model, the target resolver (`__iwdp.resolve`), `refs.Table`, `__iwdp.def(name, fn)` for W3b.
- Acceptance:
  1. Renderer golden tests (no browser) for `-i`, `-c`, `-d`, `-u`, names with NBSP and zero-width characters, duplicate role and name, cursor-interactive suffix; expected text derived from agent-browser's `render_tree` rules (cite lines).
  2. WebKitGTK fixtures: forms, ARIA widgets, shadow DOM, same-origin iframe, hidden variants; each fixture has a hand-written expected snapshot.
  3. Polyfill parity: with `checkVisibility` deleted, the runtime's visibility matches native `checkVisibility` on the hidden-variants fixture.
  4. Refs: same element keeps its ref across snapshots; replaced unique element resolves with a warning; replaced duplicate fails with `stale_ref`; after navigation the old ref fails with the "page navigated" message; ids never repeat in a session.
  5. Bundle at or below 64 KiB; ES2017 check (D4) passes; install is idempotent; a page that overrides `getBoundingClientRect` after install does not change results.
  6. `get box`, `get styles`, `is visible` and `hit-test` replace the hand-written probes named in AGG [318], [319], [324], [331], [335] on a fixture that reproduces them.
- Size: L (about 1200 lines JS, 900 lines Go, fixtures).

### 7.6 W3b: actions, waits, dialogs, tabs, annotated screenshots

- Owns: `internal/runtime/js/50-actionability.js` to `90-overlay.js`, command files `open.go` (open, back, forward, reload, pushstate), `click.go`, `tap.go`, `dblclick.go`, `fill.go`, `type.go`, `keyboard.go`, `press.go`, `hover.go`, `focus.go`, `check.go`, `select.go`, `scroll.go`, `swipe.go`, `drag.go`, `upload.go`, `mouse.go`, `wait.go`, `dialog.go`, `tab.go`, `frame.go`, `highlight.go`, `screenshot_annotate.go`, `find.go` (action part, in `find_action.go`); suite `e2e/webkitgtk/actions_test.go`.
- Consumes: W3a runtime and resolver, W1 core (generation, lease moves, bootstrap parts), W0 screenshot code.
- Acceptance (WebKitGTK unless marked):
  1. Each action on fixtures records the exact event sequence with a page-side listener and compares it with section 3.5.
  2. Covered element: click fails before dispatch and names the covering element; `--force` clicks.
  3. React-style controlled input (fixture with a value setter tracker) accepts `fill` and `type`.
  4. `press Enter` submits a form; `press Tab` moves focus; each result names the emulated default action.
  5. Waits: each form succeeds and times out with exit code 3 and the last state; a cross-origin navigation (two local ports give a real process swap, spike A) during `wait --url` succeeds.
  6. Dialogs: `dialog accept "x"` then click on a prompt button gives "x" in the page; unarmed confirm returns false and the click result reports it; no evaluate blocks.
  7. `open` works on WebKitGTK, where `Page.navigate` does not exist (spike A), and the command returns after load.
  8. Annotated screenshot: labels match refs, overlay is gone after capture (DOM check), `--if-changed` skips an identical capture.
  9. Tabs: `tab new` and `tab close` on WebKitGTK if MiniBrowser lists script-opened windows as targets (not checked in spike A; otherwise with the fake); hidden-tab probe tested with the fake.
- Size: L (about 1300 lines JS, 1200 lines Go).

### 7.7 W4: DevTools extras

- Owns: `internal/devtools/**` (netlog ring buffer with bodies on demand, HAR 1.2 writer, route rules over WIP interception and the JS fallback, console and error buffer, Timeline to Chrome trace converter, ScriptProfiler to `.cpuprofile` converter, media and user-agent overrides, geolocation shim, state save and load), command files `console.go`, `errors.go`, `network_cmds.go`, `har.go`, `route.go`, `initscript.go`, `set.go`, `cookies_ext.go`, `storage_cmd.go`, `state.go`, `trace.go`, `profiler_cmd.go`, `vitals.go`, `diff_screenshot.go`; suite `e2e/webkitgtk/devtools_test.go`.
- Consumes: W1 core (SetupList for re-enable after swaps, BootstrapParts, cleanup), W3a runtime for JS fallbacks. Provides to W5: netlog events in CDP shape, interception API, trace converter.
- Acceptance:
  1. Netlog: bounded by count and bytes (defaults 2000 entries, 64 MiB of bodies), survives a target swap (Network re-enabled), `network request <id> --body` returns the body for a request seen while capture ran and the WebKit error text otherwise.
  2. HAR validates against the HAR 1.2 schema (test fixture) and opens in a HAR viewer library test.
  3. Route: abort, fulfill with body and status, delay; JS fallback mode passes the same tests; `unroute` continues paused requests; release cleans up (fake sees `setInterceptionEnabled false`).
  4. Console: `console` after the fact returns at most the 100 replayed messages plus everything since the lease, and says when messages were dropped; `errors` includes uncaught errors and unhandled rejections with stack.
  5. Trace: Timeline fixture converts to trace events that load in a trace parser test (no Chrome needed).
  6. `set media dark` changes `matchMedia('(prefers-color-scheme: dark)')` on WebKitGTK and is re-applied after reconnect.
- Size: M to L (about 2500 lines).

### 7.8 W5: CDP endpoint

- Owns: `internal/cdp/**` (server, target model, one file per domain), command files `cdp.go` (`cdp start|stop|status|log`) and `get_cdp_url.go`, `e2e/cdpcompat/**` (Go test driver plus a small Node project pinned to playwright-core 1.65.0, puppeteer-core, chrome-devtools-mcp 1.12.0; agent-browser 0.41.2 from PATH).
- Consumes: everything above. Provides: CDP listener.
- Acceptance:
  1. Unit tests per domain with `fakewebkit` and a CDP test client: method mapping, error for unknown methods (never forwarded: the fake records zero unknown methods), target id stable across a swap.
  2. Compatibility on WebKitGTK (gate `IWDP_CDP_COMPAT=1`; needs Node): agent-browser `--cdp`: open, snapshot -i, click @ref, fill, get title, screenshot; Puppeteer: connect, goto, `$eval`, click, type, screenshot, `accessibility.snapshot`; Playwright `connectOverCDP`: goto, evaluate, `getByRole().click()`, fill, screenshot, `ariaSnapshot`; chrome-devtools-mcp: take_snapshot, click by uid, navigate_page, list_network_requests, take_screenshot. Each script records methods sent; unknown methods must be on the client's expected-missing list in the test.
  3. Security: Host header check, localhost bind, token mode.
- Size: XL (about 4000 lines Go plus test scripts).

### 7.9 W6: docs, skills, agent prompts

- Owns: `README.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `skills/**`, `docs/**` except `docs/design/`, embedded skills for `skills get` (`internal/commands/skills.go` and `internal/skills/`).
- Content: the agentic loop (snapshot, act on refs, wait, re-snapshot), session and lease rules, synthetic-input statement for reports, impossible-features table, CDP endpoint how-to per client, device runbook, migration notes from old command names, the browser-checker prompt changes (wrap nothing in `timeout` any more; use `wait` instead of sleeps; never print cookie values).
- Acceptance: every command in the registry appears in the generated command reference (test compares registry and docs); skill examples run against WebKitGTK in a doc test (each fenced `iwdp-cli` line in the skill is executed by a test with the `webkitgtk` tag).
- Size: S to M.


## 8. Test strategy

### 8.1 Unit tests on a scriptable fake WebKit server

`internal/webkit/testutil/fakewebkit` (W1a) replaces the need for a device in unit tests. The existing `testutil.MockServer` stays for legacy tests.

| Fake feature | Purpose |
|---|---|
| Target routing with configurable `targetCreated` delay or none | connect rules (2.6) |
| Handler per method; record of every received method and params | assert setup replay and cleanup |
| `Stall(method)`: accept and never answer | deadlines, probe, reset |
| `Withhold`: hold replies until the next message arrives | iOS 26 nudge |
| `Swap()`: provisional create, commit, destroy | 2.7 |
| `Steal()`: close the first connection when a second one opens | leases, `lost` state (spike A step C) |
| `ReconnectGap(d)`: after a close, new connections get no `targetCreated` for `d` | reconnect stall (MAP fact 2; spike A step B) |
| `MaxIncoming(n)`: close on larger client messages | WebKitGTK limit (spike A step D) |
| Large replies (tens of MiB) | read limits |
| Fake iwdp listing HTTP (9221 and device ports, with `deviceOSVersion`) | discovery |
| Fake hidden tab: accepts, never answers anything | tab probe |

Plus: `fakewebinspectord` (W2) at the plist level, golden-file renderer tests (W3a), CDP client harness (W5). All unit tests run with `go test ./... -count=1` in under 60 s and need no network, device or browser.

### 8.2 Local end-to-end tests on WebKitGTK (no phone)

Spike A shows that WebKitGTK 2.54 MiniBrowser runs headless under `broadwayd` and exposes the same WIP with Target routing. Suite layout: `e2e/webkitgtk/` with build tag `webkitgtk`; `make test-webkitgtk` runs `scripts/webkitgtk-setup.sh` (start `broadwayd :N`, start MiniBrowser with `WEBKIT_INSPECTOR_HTTP_SERVER=127.0.0.1:<free port>` on a fixture page served by the test, teardown on exit) and `go test -tags=webkitgtk ./e2e/webkitgtk/...`.

- Transport `webkitgtk` (W1): lists targets by parsing the HTML listing at `/` (there is no `/json`), WebSocket path `/socket/<connection>/<target>/WebPage`.
- Safari 16.6 approximation: a bootstrap part (test only) deletes APIs newer than Safari 16.6 that the runtime might touch (`Element.prototype.checkVisibility`, `Array.prototype.at`, `structuredClone`) before the runtime installs.
- Process swaps: two local HTTP ports give real cross-origin navigations and the provisional-target sequence.
- Limits of this oracle: WebKitGTK is newer WebKit than Safari 16.6, has no iOS touch behavior and a 128 KiB message cap that iOS does not have. Device tests stay required for release.
- CI: the repository has no CI config today; the suite is opt-in locally. If CI is added, it needs `webkit2gtk-4.1` and `gtk3` (`broadwayd`).

### 8.3 Real-device tests (gated) and runbook

Build tag `device`; the suite refuses to run unless `IWDP_DEVICE_TEST=1` is set. It never restarts iwdp and never runs `iwdpctl`. Runbook (`docs/device-testing.md`, W6):
1. Preconditions: no other agent uses the iPhone (ask the owner); iPhone unlocked, Safari > Advanced > Web Inspector on; Safari shows the fixture page served from the host (`make fixtures-serve` prints a LAN URL); for the direct transport, iwdp is not running.
2. Run `go test -tags=device ./e2e/device -run TestDevice -v -count=1 -timeout 15m` with `IWDP_TRANSPORT=iwdp`, then (when allowed) `direct`.
3. The suite covers: connect and probe, `eval` latency (records p50 and p90), snapshot on fixtures (compare with WebKitGTK output; differences are reported, not failed), every action with the event recorder, waits including a cross-origin navigation, dialogs, annotated screenshot, console replay limit, network capture with body, interception (records whether `requestIntercepted` fires; MAP open question), `DOM.getAccessibilityPropertiesForNode` cost per node, large evaluate (200 KB), reconnect stall time after close.
4. Output: `e2e/device/report-<date>.json` with iOS version and measurements; the PR that changes defaults cites it.
5. Cleanup: `iwdp-cli close --all`; the suite checks that no lease, interception or breakpoint is left.
Target devices: iPhone X on iOS 16.7 (always), and one iOS 26 or 27 device when available.

### 8.4 CDP compatibility tests

`e2e/cdpcompat` (W5), gate `IWDP_CDP_COMPAT=1`, needs Node and `npm ci` in that directory (open decision D7). Each client script runs a fixed scenario against the CDP endpoint in front of WebKitGTK, records every CDP method sent, and writes a JSON result that a Go test checks. Versions are pinned: agent-browser 0.41.2, playwright-core 1.65.0, puppeteer-core (version at implementation time, recorded), chrome-devtools-mcp 1.12.0. The same scripts run against the device behind the device gate.

### 8.5 Latency targets (measured, not assumed)

| Measure | Today (LAT) | Target, warm daemon | Where measured |
|---|---|---|---|
| CLI eval p50 | 1.2 s (device) | at or below 50 ms on WebKitGTK; at or below 300 ms on the iPhone | 7.3 test 11; device suite |
| navigate (open) p50, to load | 4.7 s (returns before load) | page load time plus at most 300 ms | device suite |
| snapshot -i on a 500-element page | n/a | at or below 500 ms on the iPhone | device suite |
| hang longer than the command deadline | 40 timeouts, max 1837 s | none | fake stall tests |


## 9. Risks and open questions

| # | Risk or question | Impact | How to resolve |
|---|---|---|---|
| R1 | Another agent or tool outside the daemon takes a page (one connection per page) | commands fail with `lost`; captures stop | detection and clear message (2.5); docs tell agents to use only iwdp-cli or the MCP server; device test step for steal behavior behind iwdp |
| R2 | Reconnect stall after a close (about 10 s on iPhone; spike A saw every second quick reconnect fail on WebKitGTK) | slow recovery | daemon keeps connections open; 12 s target wait after a close; fake `ReconnectGap` tests; device suite measures the real gap |
| R3 | Synthetic input does not trigger behavior that checks `isTrusted` or needs a user gesture (popups, clipboard, media, fullscreen, some payment and auth widgets) | false negatives in checks | every result says `trusted: false`; skills require reports to state it; `emulateUserGesture` where WIP allows; R10 for trusted input |
| R4 | Direct transport on iOS 17 and later may need the tunnel | direct path limited to iOS 16 | one device run (8.3) on an iOS 17+ device before `auto` uses direct; iwdp stays the fallback |
| R5 | go-ios raises the Go floor to 1.26 and moves fast; issue #880 socket leak | build break for users with pinned toolchains; device unreachable | decision D5; pin v1.3.2; wrap behind `deviceDialer`; bounded retries |
| R6 | Runtime size and WebKitGTK 128 KiB message cap; iwdp before 1.9.2 lags on payloads of 30 KB or more | install fails or lags | 64 KiB budget test; `doctor` checks iwdp version |
| R7 | iOS 26 site isolation puts cross-origin frames in separate targets | snapshot and actions miss cross-origin iframe content | v1 documents the limit; later: attach frame targets through Target routing (Playwright chose not to) |
| R8 | Playwright `connectOverCDP` expects isolated worlds, frame lifecycle and `Runtime.callFunctionOn` with execution context ids | some Playwright features fail | tier 1 emulation (5.2, 5.3); compatibility suite lists what works; document `contexts()[0]` only |
| R9 | Pages can tamper with `window.__iwdp` or built-ins (no isolated world) | wrong results on hostile pages | capture built-ins at install; version check; docs mark page output as untrusted (agent-browser trust boundary) |
| R10 | Trusted input through Safari Remote Automation (`Automation.performInteractionSequence` through a webinspectord automation session, go-ios `AutomationSession`, pymobiledevice3 automation_session.py) | could deliver real taps, but automation sessions open their own window and need Settings > Safari > Advanced > Remote Automation | research spike after W2, on the device; not in v1 |
| R11 | Network interception on device is unverified on 16.7 and reported broken on iOS 26.5 WKWebView (MAP 1.8) | route and mock may not work | device suite records it; JS `fetch`/XHR fallback mode (`--mode js`) |
| R12 | `getResponseBody` works only for requests seen while Network was enabled; inspect-webkit says Safari exposes no bodies | `network request --body` empty | capture starts with the lease; `Page.getResourceContent` fallback; device suite records it |
| R13 | Native dialogs before the override blocks the page | session stuck | bootstrap installs overrides before page scripts; detection message; R10 may allow answering |
| R14 | Unknown methods wedge the page target (MAP fact 9) | whole session hangs | CDP never forwards unknown methods; passthrough only for methods in the iOS version's command list |
| R15 | MCP tool count: 116 today plus about 40 new tools; some MCP clients limit tool counts | tools hidden or rejected | profiles (D2) |
| R16 | Daemon security: local socket and CDP port give full control of the phone's Safari | other local users or web pages could drive it | 0700 socket directory, uid check, 127.0.0.1 bind, Host check, optional token |
| R17 | Privacy: snapshots and network logs contain page text, tokens and signed URLs (AGG [128], [266], [316]) | secrets in transcripts | W0 cookie redaction carries into `cookies` and `network request` (header values of `Cookie`, `Set-Cookie`, `Authorization` redacted unless `--show-values`); URL fragments hidden in connection lines; device UDIDs shortened (AGG [287], [333]) |
| R18 | WebKitGTK as oracle is newer than Safari 16.6 | tests pass locally and fail on device | API-deletion mode (8.2), ES2017 check, device suite before release |
| R19 | Version skew between daemon and clients after an upgrade | confusing errors | protocol number in `/v1/hello` (2.2) |
| R20 | Hidden Safari tabs never answer, and WIP cannot raise a tab | commands time out | probe before first use; clear message; `tab` lists which tab answered last |
| Q1 | Does iwdp also stall a page for about 10 s after a client closes (MAP fact 2 is from direct webinspectord)? | sizing of reconnect budgets | device suite measures it with iwdp |
| Q2 | Does `Touch` construction work on iOS 16.7 Safari for synthetic `TouchEvent`? | tap fidelity | device suite; fallback already designed |
| Q3 | Cost of `DOM.getAccessibilityPropertiesForNode` per node on the iPhone | whether `--native-ax` is usable | device suite |

## 10. Decisions that need input

| # | Decision | Recommendation | Alternatives |
|---|---|---|---|
| D1 | MCP server as a daemon client (shared leases with the CLI) | yes | MCP server owns sessions in-process; simpler, but the CLI and MCP then take pages from each other |
| D2 | Default MCP tool profile | `core` plus `legacy` by default (keeps today's tools; about 155 tools), `IWDP_MCP_TOOLS` to reduce | `core` only by default (breaking for current users) |
| D3 | CDP endpoint start | opt-in (`cdp start` or `get cdp-url`), port 9230 | always on with the daemon; port 9222 (conflicts with a local Chrome on this host) |
| D4 | How to enforce ES2017 in the runtime | test-only dependency on the esbuild Go API (MIT): the bundle must transform with target ES2017 to the same output as with target esnext, which proves it uses no newer syntax | no dependency: code review plus the WebKitGTK API-deletion mode only |
| D5 | go-ios dependency and Go 1.26 floor for the direct transport | accept go-ios v1.3.2 (`ios` package only) and raise `go` to 1.26.0 when W2 merges | own minimal usbmuxd and lockdown client (no new dependency, more code to own); or keep direct out of the default build (`//go:build direct`) |
| D6 | Default answer for unarmed `confirm` and `prompt` | dismiss (false, null), alert accepted | accept by default (riskier: confirms destructive actions) |
| D7 | Node toolchain for CDP compatibility tests inside a Go repo | yes, isolated in `e2e/cdpcompat` with its own lockfile, opt-in | keep the scripts outside the repository |
| D8 | Session flag name and default | `--session` and `IWDP_SESSION`, default `default`, as agent-browser | per-process default (no sharing; loses refs between CLI calls) |

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

- iwdp-mcp stays MIT. New dependencies allowed by this design: go-ios (MIT, D5), esbuild Go API as a test-only dependency (MIT, D4). gorilla/websocket and the MCP SDK are already used.
- Do not copy code from pymobiledevice3 (GPL-3.0), mobilecli (FSL-1.1-ALv2) or sonic-ios-bridge (AGPL-3.0). Behavior facts from them (message names, timings) are cited, not copied.
- Playwright (Apache-2.0) and agent-browser (Apache-2.0): prefer own implementations. If a worker adapts code (for example role tables from `roleUtils.ts` or key tables from `webViewInput.ts`), that file keeps the original copyright and license header, a `NOTICE` file at the repository root names the source and license, and the README license section mentions it.
- WebKit protocol method lists (`internal/wip/methods_gen.go`) contain names only; the generator header cites the WebKit source files (BSD-style license).
- axe-core (MPL-2.0) is not bundled.
