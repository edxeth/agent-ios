# Command reference

<!-- Generated from the command registry by "make docs" (go run ./internal/commands/refgen docs/commands.md). Do not edit by hand. -->

This page lists every registry command of `iwdp-cli`: the commands that run in the iwdp daemon and share sessions, leases and refs with the MCP server. For each command it gives the usage, the arguments, the flags, the MCP tool name and the MCP profile that registers the tool.

- A command without an MCP tool is a CLI command only.
- The MCP server registers the tools of the selected profile: `core` by default, or any comma-separated set of `core`, `devtools` and `legacy`, or `all` (`IWDP_MCP_TOOLS` or `iwdp-mcp --tools`). See [MCP profiles](guide.md#mcp-profiles).
- The legacy commands of `iwdp-cli` (for example `eval`, `cookies`, `set-breakpoint`) and the legacy MCP tools are not in the registry. `iwdp-cli --help` lists them, and [the migration notes](migration.md) map them to registry commands.
- `iwdp-cli <command> --help` prints the same usage and never connects to a page.

Global flags (any position): `--session`, `--page`, `--timeout`, `--json`, `--transport`, `--max-output`, `--content-boundaries`, `--reservation`, `--wait-lease`, `--takeover`.

## Index

| Command | MCP tool | Profile | Summary |
|---|---|---|---|
| [`addinitscript`](#addinitscript) | `add_init_script` | `devtools` | Run a script in every new document of the page, before page scripts when the page is paused on start (partial otherwise). Prints the script id. |
| [`back`](#back) | `go_back` | `core` | Go back in the page's history (history.back), then wait for the load state. |
| [`blur`](#blur) |  |  | Blur the focused element. |
| [`cdp log`](#cdp-log) |  |  | Show how often CDP clients sent methods that the endpoint does not translate (answered "wasn't found", never forwarded). |
| [`cdp start`](#cdp-start) |  |  | Start the CDP endpoint on 127.0.0.1 (a running endpoint is kept); then run 'get cdp-url' for a client URL. |
| [`cdp status`](#cdp-status) |  |  | Show the CDP endpoint: address, allowed origins and, per endpoint record, its session, page, scope, connections and attachments. |
| [`cdp stop`](#cdp-stop) |  |  | Stop the CDP endpoint: close its client connections (each releases its state) and revoke every CDP URL. |
| [`check`](#check) | `check` | `core` | Check a checkbox or radio (a hidden input through its label) and read the state back. |
| [`click`](#click) | `click` | `core` | Click an element with synthetic pointer and mouse events after the actionability checks. |
| [`close`](#close) | `close_session` | `core` | End this session (--session): release its page leases and run their cleanup. The Safari tab stays open; other sessions are not touched. |
| [`console`](#console) | `get_console_messages` | `devtools` | Print the console messages captured since the lease (and up to 100 that WebKit replays). For a live stream use 'console -d \<duration\>' or 'console --follow'. |
| [`console clear`](#console-clear) | `clear_console` | `devtools` | Empty the console buffer and WebKit's stored messages. |
| [`cookies clear`](#cookies-clear) | `clear_cookies` | `devtools` | Delete the page's cookies (those Page.getCookies lists: the cookies of the page's frames and resources), read back. |
| [`cookies set`](#cookies-set) | `set_cookie` | `core` | Set one cookie (cookies set \<name\> \<value\> [flags]) or import cookies from a file (--curl: a JSON list, a cURL command or a Cookie header), each read back. Values are redacted in the result. |
| [`daemon logs`](#daemon-logs) |  |  | Print the end of the daemon log ($XDG_STATE_HOME/iwdp/daemon.log). |
| [`daemon run`](#daemon-run) |  |  | Run the daemon in the foreground until SIGTERM, 'daemon stop' or the idle timeout (clients start it on demand). |
| [`daemon start`](#daemon-start) |  |  | Start the daemon in the background unless it runs. |
| [`daemon status`](#daemon-status) |  |  | Show the daemon, its pages, leases and device reservations. |
| [`daemon stop`](#daemon-stop) |  |  | Stop the daemon after the cleanup of every page (5 s at most). |
| [`dblclick`](#dblclick) | `dblclick` | `devtools` | Double-click an element: two click sequences (detail 1 and 2), then dblclick. |
| [`device list`](#device-list) |  |  | List the live device reservations. |
| [`device release`](#device-release) |  |  | Release an owner's device reservation. |
| [`device reserve`](#device-reserve) |  |  | Reserve a physical device for an owner, so that only that owner's sessions attach to its pages. |
| [`dialog`](#dialog) | `dialog` | `core` | Arm answers for alert, confirm and prompt, or show the dialog log (status, accept [text], dismiss, default accept\|dismiss). |
| [`diff screenshot`](#diff-screenshot) | `diff_screenshot` | `devtools` | Compare a viewport screenshot of the page with a baseline PNG, pixel by pixel on the host, and report how many pixels differ. |
| [`diff snapshot`](#diff-snapshot) | `diff_snapshot` | `core` | Show what changed between the last two snapshots of this session (a unified line diff). |
| [`drag`](#drag) | `drag` | `devtools` | Drag from one element to another in 10 pointer steps with synthetic drag events (best effort). |
| [`emulate network`](#emulate-network) | `set_emulated_conditions` | `devtools` | Throttle the page's network to a preset (4g, fast-3g, slow-3g) or a byte rate; 'none' removes the throttle. Uses Network.setEmulatedConditions, which only the iOS 16.4 to 17.x and 27.3+ command lists have. |
| [`errors`](#errors) | `get_errors` | `devtools` | Print uncaught errors and unhandled promise rejections with their stacks (the first run installs the error hooks). |
| [`fill`](#fill) | `fill` | `core` | Replace an input's value: beforeinput (insertReplacementText), the native value setter, input and change. |
| [`find`](#find) | `find` | `core` | Find elements by role, text, label, placeholder, alt, title, testid, or first\|last\|nth of a selector; print them with refs. |
| [`focus`](#focus) | `focus` | `devtools` | Focus an element (el.focus()). |
| [`forward`](#forward) | `go_forward` | `core` | Go forward in the page's history (history.forward), then wait for the load state. |
| [`frame`](#frame) | `select_frame` | `core` | Make an iframe (@eN, CSS or locator) the frame that this session's later commands work in, or return to the top document with main. |
| [`get`](#get) | `get` | `core` | Get url, title, or text, html, value, attr, count, box or styles of a target (@ref, CSS selector or locator). |
| [`get cdp-url`](#get-cdp-url) | `get_cdp_url` | `devtools` | Print a CDP URL for this session's page (starts the CDP endpoint if needed). Clients act for this session; use the URL exactly as printed. |
| [`highlight`](#highlight) | `highlight_node` | `core` | Highlight an element on the device screen (DOM.highlightNode) until release. |
| [`hit-test`](#hit-test) | `hit_test` | `core` | Print the element at a viewport point (CSS pixels): its snapshot line with a ref, its tag and its box. |
| [`hover`](#hover) | `hover` | `core` | Move the pointer over an element (synthetic events do not set CSS :hover; --css forces it). |
| [`is`](#is) | `is` | `core` | Print true or false: is a target visible, enabled, checked, editable or focused. |
| [`keyboard`](#keyboard) | `keyboard` | `devtools` | Type into the focused element (keyboard type \<text\>) or insert text without key events (keyboard inserttext \<text\>). |
| [`keydown`](#keydown) |  |  | Send keydown for a key on the focused element (no default action). |
| [`keyup`](#keyup) |  |  | Send keyup for a key on the focused element. |
| [`lease break`](#lease-break) |  |  | End the lease on a page whoever holds it, with its cleanup. |
| [`lease takeover`](#lease-takeover) |  |  | Reattach a lost page and take its lease for this session (needs the device reservation and the device owner's approval). |
| [`mouse`](#mouse) | `mouse` | `devtools` | Low-level mouse: move \<x\> \<y\> [--steps n], down [button], up [button], wheel \<dy\> [dx]. |
| [`network har start`](#network-har-start) | `har_start` | `devtools` | Start recording a HAR of the requests from now on; 'network har stop' writes it. |
| [`network har stop`](#network-har-stop) | `har_stop` | `devtools` | Stop the HAR recording and write it as HAR 1.2 JSON (mode 0600; secret values masked unless --show-secrets). |
| [`network request`](#network-request) | `get_network_request` | `devtools` | Show one request of 'network requests': headers, request body and status; --body adds the response body. |
| [`network requests`](#network-requests) | `list_network_requests` | `devtools` | List the requests captured since the lease: id, method, status, type and URL; network resets show as gaps. |
| [`network route`](#network-route) | `network_route` | `devtools` | Abort, fulfill or delay requests whose URL matches: a glob with \* and \*\* matches the whole URL, other text any URL that contains it. Without --abort, --body or --status the request continues after --delay. |
| [`network unroute`](#network-unroute) | `network_unroute` | `devtools` | Remove the routes with this pattern (all without one); requests that a route holds continue unchanged. |
| [`open`](#open) | `navigate` | `core` | Open a URL in the page (location.href), then wait for the load state; a URL that does not start with http:, https:, about:, data: or file: gets https:// (agent-browser open). The MCP tool navigate takes only http and https URLs. |
| [`press`](#press) | `press` | `core` | Press a key or combination (Enter, Tab, Shift+Tab, Control+a) on the focused element, with its emulated default action. |
| [`profiler start`](#profiler-start) | `profiler_start` | `devtools` | Start sampling the page's JavaScript with the WebKit ScriptProfiler (profiler stop writes the .cpuprofile). |
| [`profiler stop`](#profiler-stop) | `profiler_stop` | `devtools` | Stop sampling and write the CPU profile (.cpuprofile, Chrome DevTools format, mode 0600) to path, or to a new file in the temp directory. |
| [`pushstate`](#pushstate) | `pushstate` | `devtools` | Change the URL without loading: the Next.js router when the page has one, else history.pushState and a popstate event. |
| [`read`](#read) | `read_page` | `core` | Print the visible text of the current page, or of an http(s) URL fetched on this computer; --outline prints only the headings. |
| [`reload`](#reload) | `reload` | `core` | Reload the page (Page.reload after withdrawing unused dialog answers), then wait for the load state. |
| [`removeinitscript`](#removeinitscript) | `remove_init_script` | `devtools` | Remove an init script of this session by the id that addinitscript printed. |
| [`screenshot`](#screenshot) | `take_screenshot` | `core` | Capture the viewport, the full page or one element as PNG; --annotate labels each ref [N] (@eN), --if-changed skips an identical capture. |
| [`scroll`](#scroll) | `scroll` | `core` | Scroll the window or an element up, down, left or right (default 300 px). |
| [`scrollintoview`](#scrollintoview) | `scroll_into_view` | `devtools` | Scroll an element into the center of the viewport. |
| [`select`](#select) | `select_option` | `core` | Select options of a \<select\> by value, then by visible label; dispatches input and change. |
| [`session`](#session) |  |  | Print the current session name (--session or IWDP_SESSION; default "default"). |
| [`session info`](#session-info) | `session_info` | `core` | Show this session's leased pages: page, lease state (active or lost), epoch and last use. |
| [`session list`](#session-list) |  |  | List the sessions that hold page leases in the daemon. |
| [`session release`](#session-release) |  |  | Release this session's page leases and run their cleanup (same as close). |
| [`set credentials`](#set-credentials) | `set_credentials` | `devtools` | Send HTTP Basic credentials to the requests of one origin only; on a command line the password is the first line of stdin. Supported only where a test has shown that other origins and cross-origin redirects never get them (WebKitGTK today). |
| [`set device`](#set-device) |  |  | Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute. |
| [`set geo`](#set-geo) | `set_geolocation` | `devtools` | Answer navigator.geolocation with this position (a page-side shim: WebKit has no geolocation override). |
| [`set headers`](#set-headers) | `set_extra_headers` | `devtools` | Send these extra HTTP headers with every request of the page; '{}' removes this session's headers. |
| [`set media`](#set-media) | `set_media` | `devtools` | Emulate prefers-color-scheme (dark, light or no-preference), and optionally reduced motion and more contrast. |
| [`set offline`](#set-offline) | `set_offline` | `devtools` | Fail every request of the page (on) or stop doing so (off), through request interception; unverified on a device. |
| [`set useragent`](#set-useragent) | `set_user_agent` | `devtools` | Override the user agent string of the page (--reset removes the override). |
| [`set viewport`](#set-viewport) |  |  | Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute. |
| [`skills get`](#skills-get) |  |  | Print agent skills built into this binary (the whole SKILL.md); without a name, or with --all, every skill. |
| [`skills list`](#skills-list) |  |  | List the agent skills built into this binary (name and description); 'skills get \<name\>' prints one. |
| [`skills path`](#skills-path) |  |  | Write the built-in skills to the user cache directory (once per skills version, mode 0600) and print the directory of one skill or of all. |
| [`snapshot`](#snapshot) | `snapshot` | `core` | Print the page's accessibility tree with @eN refs for later commands. |
| [`state load`](#state-load) | `state_load` | `devtools` | Restore a state file written by state save: the page must be at the file's origin; its cookies and storage are replaced by the file's. |
| [`state save`](#state-save) | `state_save` | `devtools` | Save the cookies, localStorage and sessionStorage of the page's current origin to a JSON file (mode 0600; it holds live credentials). |
| [`storage`](#storage) | `storage` | `devtools` | Read, set or clear localStorage or sessionStorage of the page: storage local [key], storage local set \<key\> \<value\>, storage local clear (same for session). |
| [`swipe`](#swipe) | `swipe` | `core` | Swipe with synthetic touch events; scrolls as a fallback when the page cancels none (swipe up scrolls down). |
| [`tab`](#tab) | `tab_select` | `core` | Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs. |
| [`tab close`](#tab-close) | `tab_close` | `devtools` | Close a tab (default: the current one) with window.close(); works only for tabs that a script opened. |
| [`tab list`](#tab-list) | `tab_list` | `core` | List the pages of the device as tabs t1, t2, ... with labels; \* marks this session's current tab. |
| [`tab new`](#tab-new) | `tab_new` | `devtools` | Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab. |
| [`tap`](#tap) | `tap` | `core` | Tap an element, or the element at viewport point \<x\> \<y\>: touchstart and touchend, then the click sequence with pointerType touch. |
| [`trace start`](#trace-start) | `trace_start` | `devtools` | Start a WebKit Timeline recording of the page for a Chrome trace file (trace stop writes it). |
| [`trace stop`](#trace-stop) | `trace_stop` | `devtools` | Stop the Timeline recording and write it as Chrome trace JSON (mode 0600) to path, or to a new file in the temp directory. |
| [`type`](#type) | `type_text` | `core` | Type text key by key (keydown, keypress, beforeinput, insertion, input, keyup) into an element, or with only \<text\> into the focused element. |
| [`uncheck`](#uncheck) |  |  | Uncheck a checkbox (a hidden input through its label) and read the state back. |
| [`upload`](#upload) | `upload_file` | `core` | Set the files of a file input (read on the host, at most 8 MiB in total) and dispatch input and change. |
| [`vitals`](#vitals) | `vitals` | `devtools` | Show the page's Web Vitals: LCP, CLS, TTFB, FCP, INP, DOMContentLoaded and load (unavailable ones say why). |
| [`wait`](#wait) | `wait` | `core` | Wait for milliseconds, an element state, text, a URL glob, a load state or a JavaScript condition (default timeout 25 s; exit 3 on timeout). |

## Commands

### `addinitscript`

```
iwdp-cli addinitscript [<script>] [flags]
```

Run a script in every new document of the page, before page scripts when the page is paused on start (partial otherwise). Prints the script id.

- MCP tool: `add_init_script` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<script>` | no | JavaScript source (or use --file) |

| Flag | Value | Description |
|---|---|---|
| `--file` | file path | read the script from this file |

### `back`

```
iwdp-cli back [flags]
```

Go back in the page's history (history.back), then wait for the load state.

- MCP tool: `go_back` (profile `core`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |

### `blur`

```
iwdp-cli blur
```

Blur the focused element.

- MCP tool: none (CLI only)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

### `cdp log`

```
iwdp-cli cdp log
```

Show how often CDP clients sent methods that the endpoint does not translate (answered "wasn't found", never forwarded).

- MCP tool: none (CLI only)
- Page: none

### `cdp start`

```
iwdp-cli cdp start [flags]
```

Start the CDP endpoint on 127.0.0.1 (a running endpoint is kept); then run 'get cdp-url' for a client URL.

- MCP tool: none (CLI only)
- Page: none

| Flag | Value | Description |
|---|---|---|
| `--port` | integer | port on 127.0.0.1 (default: the daemon's --cdp-port, else 9230; 0 picks a free port) |
| `--allow-origin` | text | comma-separated Origin header values to accept (default: none, every request with an Origin header is refused) |

### `cdp status`

```
iwdp-cli cdp status
```

Show the CDP endpoint: address, allowed origins and, per endpoint record, its session, page, scope, connections and attachments.

- MCP tool: none (CLI only)
- Page: none

### `cdp stop`

```
iwdp-cli cdp stop
```

Stop the CDP endpoint: close its client connections (each releases its state) and revoke every CDP URL.

- MCP tool: none (CLI only)
- Page: none

### `check`

```
iwdp-cli check <target> [flags]
```

Check a checkbox or radio (a hidden input through its label) and read the state back.

- MCP tool: `check` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | checkbox or radio: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--checked` | `true`, `false` | the wanted state; false unchecks (default `true`) |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `click`

```
iwdp-cli click <target> [flags]
```

Click an element with synthetic pointer and mouse events after the actionability checks.

- MCP tool: `click` (profile `core`)
- Legacy MCP parameters: `selector` for `target`
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to click: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--button` | `left`, `right`, `middle` | mouse button; right ends with contextmenu, middle with auxclick (default `left`) |
| `--count` | integer | number of clicks (detail 1..n) |
| `--new-tab` | none | open the link's href with window.open (emulated user gesture) instead of clicking; a gesture call delivered after its window opens nothing, but the page may then hold a transient user activation for a few seconds |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `close`

```
iwdp-cli close
```

End this session (--session): release its page leases and run their cleanup. The Safari tab stays open; other sessions are not touched.

- MCP tool: `close_session` (profile `core`)
- Page: none

### `console`

```
iwdp-cli console [flags]
```

Print the console messages captured since the lease (and up to 100 that WebKit replays). For a live stream use 'console -d \<duration\>' or 'console --follow'.

- MCP tool: `get_console_messages` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--clear` | none | empty the buffer after printing |
| `--level` | `debug`, `log`, `info`, `warning`, `error` | print only messages of this level or above |
| `--since` | integer | print only messages with an id above this one |

### `console clear`

```
iwdp-cli console clear
```

Empty the console buffer and WebKit's stored messages.

- MCP tool: `clear_console` (profile `devtools`)
- Page: leases the session's page

### `cookies clear`

```
iwdp-cli cookies clear [flags]
```

Delete the page's cookies (those Page.getCookies lists: the cookies of the page's frames and resources), read back.

- MCP tool: `clear_cookies` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--domain` | text | delete only the cookies of this domain and its subdomains |

### `cookies set`

```
iwdp-cli cookies set [<name>] [<value>] [flags]
```

Set one cookie (cookies set \<name\> \<value\> [flags]) or import cookies from a file (--curl: a JSON list, a cURL command or a Cookie header), each read back. Values are redacted in the result.

- MCP tool: `set_cookie` (profile `core`)
- Legacy MCP parameters: `http_only` for `http-only`, `same_site` for `same-site`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<name>` | no | the cookie name (not with --curl) |
| `<value>` | no | the cookie value (not with --curl) |

| Flag | Value | Description |
|---|---|---|
| `--curl` | file path | import the cookies of this file: a JSON list of {name, value, ...}, a cURL command (DevTools Copy as cURL) or a Cookie header |
| `--domain` | text | the domain of cookies that have none (default: the cURL URL's host, else the page's host) |
| `--path` | text | the cookie path (default /) |
| `--expires` | text | expiry as Unix seconds (decimals allowed) or RFC 3339 time; 0 or none: a session cookie; a time in the past deletes the cookie of this name, domain and path |
| `--http-only` | none | set HttpOnly |
| `--secure` | none | set Secure |
| `--same-site` | text | Lax, Strict or None (default Lax) |

### `daemon logs`

```
iwdp-cli daemon logs [flags]
```

Print the end of the daemon log ($XDG_STATE_HOME/iwdp/daemon.log).

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `-n`, `--lines` | integer | number of lines from the end (default `50`) |

### `daemon run`

```
iwdp-cli daemon run [flags]
```

Run the daemon in the foreground until SIGTERM, 'daemon stop' or the idle timeout (clients start it on demand).

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `--addr` | text | listing or inspector address of the --transport (webkitgtk: the WEBKIT_INSPECTOR_HTTP_SERVER address) |
| `--idle` | duration | exit after this long without requests and leases (default IWDP_DAEMON_IDLE, else 15m) |
| `--cdp-port` | integer | start the CDP endpoint on 127.0.0.1:\<port\> with the daemon, and use the port for later starts (default: the endpoint starts only on 'cdp start' or 'get cdp-url', on port 9230) |

### `daemon start`

```
iwdp-cli daemon start [flags]
```

Start the daemon in the background unless it runs.

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `--addr` | text | listing or inspector address of the --transport (webkitgtk: the WEBKIT_INSPECTOR_HTTP_SERVER address) |
| `--idle` | duration | exit after this long without requests and leases (default IWDP_DAEMON_IDLE, else 15m) |
| `--cdp-port` | integer | start the CDP endpoint on 127.0.0.1:\<port\> with the daemon, and use the port for later starts (default: the endpoint starts only on 'cdp start' or 'get cdp-url', on port 9230) |

### `daemon status`

```
iwdp-cli daemon status
```

Show the daemon, its pages, leases and device reservations.

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

### `daemon stop`

```
iwdp-cli daemon stop
```

Stop the daemon after the cleanup of every page (5 s at most).

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

### `dblclick`

```
iwdp-cli dblclick <target> [flags]
```

Double-click an element: two click sequences (detail 1 and 2), then dblclick.

- MCP tool: `dblclick` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to double-click: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `device list`

```
iwdp-cli device list
```

List the live device reservations.

- MCP tool: none (CLI only)
- Page: none

### `device release`

```
iwdp-cli device release [flags]
```

Release an owner's device reservation.

- MCP tool: none (CLI only)
- Page: none

| Flag | Value | Description |
|---|---|---|
| `--device` | text | device UDID; \* reserves every device (default `*`) |
| `--owner` | text | reservation owner; default --reservation (IWDP_DEVICE_RESERVATION), else the session |

### `device reserve`

```
iwdp-cli device reserve [flags]
```

Reserve a physical device for an owner, so that only that owner's sessions attach to its pages.

- MCP tool: none (CLI only)
- Page: none

| Flag | Value | Description |
|---|---|---|
| `--device` | text | device UDID; \* reserves every device (default `*`) |
| `--owner` | text | reservation owner; default --reservation (IWDP_DEVICE_RESERVATION), else the session |
| `--ttl` | duration | lifetime, renewed while the owner holds leases (default `2h`) |

### `dialog`

```
iwdp-cli dialog <action> [<value>]
```

Arm answers for alert, confirm and prompt, or show the dialog log (status, accept [text], dismiss, default accept|dismiss).

- MCP tool: `dialog` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<action>` | yes | status, accept, dismiss or default |
| `<value>` | no | accept: the prompt text; default: accept or dismiss |

### `diff screenshot`

```
iwdp-cli diff screenshot [flags]
```

Compare a viewport screenshot of the page with a baseline PNG, pixel by pixel on the host, and report how many pixels differ.

- MCP tool: `diff_screenshot` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--baseline` | file path | the baseline PNG (for example a viewport screenshot taken earlier) |
| `--threshold` | text | a pixel differs when a colour or alpha value changes by more than this fraction of 255 (0 to below 1) (default `0.1`) |

### `diff snapshot`

```
iwdp-cli diff snapshot
```

Show what changed between the last two snapshots of this session (a unified line diff).

- MCP tool: `diff_snapshot` (profile `core`)
- Page: uses the session's page

### `drag`

```
iwdp-cli drag <target> <destination> [flags]
```

Drag from one element to another in 10 pointer steps with synthetic drag events (best effort).

- MCP tool: `drag` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | source element: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<destination>` | yes | drop target: @eN ref, CSS selector or locator |

| Flag | Value | Description |
|---|---|---|
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `emulate network`

```
iwdp-cli emulate network [<preset>] [flags]
```

Throttle the page's network to a preset (4g, fast-3g, slow-3g) or a byte rate; 'none' removes the throttle. Uses Network.setEmulatedConditions, which only the iOS 16.4 to 17.x and 27.3+ command lists have.

- MCP tool: `set_emulated_conditions` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<preset>` | no | 4g, fast-3g, slow-3g or none |

| Flag | Value | Description |
|---|---|---|
| `--bytes_per_second_limit` | integer | a rate in bytes per second instead of a preset; 0 removes the throttle |

### `errors`

```
iwdp-cli errors [flags]
```

Print uncaught errors and unhandled promise rejections with their stacks (the first run installs the error hooks).

- MCP tool: `get_errors` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--clear` | none | empty the error list after printing |

### `fill`

```
iwdp-cli fill <target> [<text>] [flags]
```

Replace an input's value: beforeinput (insertReplacementText), the native value setter, input and change.

- MCP tool: `fill` (profile `core`)
- Legacy MCP parameters: `selector` for `target`, `value` for `text`
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | input, textarea or contenteditable: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<text>` | no | the new value (or --stdin) |

| Flag | Value | Description |
|---|---|---|
| `--stdin` | none | read the value from the standard input (iwdp-cli only), so a secret is not in the command line; one trailing newline is removed |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `find`

```
iwdp-cli find <kind> <value> [<action>...] [flags]
```

Find elements by role, text, label, placeholder, alt, title, testid, or first|last|nth of a selector; print them with refs.

- MCP tool: `find` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<kind>` | yes | role, text, label, placeholder, alt, title, testid, first, last or nth |
| `<value>` | yes | the role, text or attribute value; for first and last the selector; for nth the 0-based index (negative counts from the end) |
| `<action>...` | no | nth: the selector first; then an optional action and its arguments (click, dblclick, tap, hover, focus, check, uncheck, scrollintoview, fill \<text\>, type \<text\>) |

| Flag | Value | Description |
|---|---|---|
| `--name` | text | role only: accessible name (case-insensitive substring unless --exact) |
| `--exact` | none | exact, case-sensitive text match |

### `focus`

```
iwdp-cli focus <target>
```

Focus an element (el.focus()).

- MCP tool: `focus` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to focus: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

### `forward`

```
iwdp-cli forward [flags]
```

Go forward in the page's history (history.forward), then wait for the load state.

- MCP tool: `go_forward` (profile `core`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |

### `frame`

```
iwdp-cli frame <target>
```

Make an iframe (@eN, CSS or locator) the frame that this session's later commands work in, or return to the top document with main.

- MCP tool: `select_frame` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | main, or the iframe: @eN ref, CSS selector or locator (in the current frame) |

### `get`

```
iwdp-cli get <what> [<target>] [<name>]
```

Get url, title, or text, html, value, attr, count, box or styles of a target (@ref, CSS selector or locator).

- MCP tool: `get` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<what>` | yes | url, title, text, html, value, attr, count, box or styles |
| `<target>` | no | @ref, CSS selector or locator such as text=Save (not for url and title) |
| `<name>` | no | attr: the attribute name; styles: comma-separated properties (default: a common set) |

### `get cdp-url`

```
iwdp-cli get cdp-url [flags]
```

Print a CDP URL for this session's page (starts the CDP endpoint if needed). Clients act for this session; use the URL exactly as printed.

- MCP tool: `get_cdp_url` (profile `devtools`)
- Page: none

| Flag | Value | Description |
|---|---|---|
| `--scope` | `page`, `device` | page: only this page; device: also the other pages of the devices reserved by --reservation (default `page`) |
| `--form` | `ws`, `http` | ws: the browser WebSocket URL (every client; agent-browser --cdp, Puppeteer browserWSEndpoint, Playwright connectOverCDP); http: the discovery base for Playwright connectOverCDP and Puppeteer browserURL (default `ws`) |

### `highlight`

```
iwdp-cli highlight [<target>] [flags]
```

Highlight an element on the device screen (DOM.highlightNode) until release.

- MCP tool: `highlight_node` (profile `core`)
- Legacy MCP parameters: `node_id` for `node-id`, `selector` for `target`
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | no | element to highlight: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--node-id` | integer | DOM node id instead of a target (as DOM.getDocument and query-selector return it) |

### `hit-test`

```
iwdp-cli hit-test <x> <y>
```

Print the element at a viewport point (CSS pixels): its snapshot line with a ref, its tag and its box.

- MCP tool: `hit_test` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<x>` | yes | viewport x in CSS pixels |
| `<y>` | yes | viewport y in CSS pixels |

### `hover`

```
iwdp-cli hover <target> [flags]
```

Move the pointer over an element (synthetic events do not set CSS :hover; --css forces it).

- MCP tool: `hover` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to hover: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--css` | none | also force the CSS :hover state with CSS.forcePseudoState until the next action or release; needs the CSS domain, which only WebKitGTK answers (through iwdp CSS.enable hangs, so --css fails with exit 2 before any event) |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `is`

```
iwdp-cli is <state> <target>
```

Print true or false: is a target visible, enabled, checked, editable or focused.

- MCP tool: `is` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<state>` | yes | visible, enabled, checked, editable or focused |
| `<target>` | yes | @ref, CSS selector or locator such as label=Email |

### `keyboard`

```
iwdp-cli keyboard <mode> <text> [flags]
```

Type into the focused element (keyboard type \<text\>) or insert text without key events (keyboard inserttext \<text\>).

- MCP tool: `keyboard` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 30s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<mode>` | yes | type or inserttext |
| `<text>` | yes | the text |

| Flag | Value | Description |
|---|---|---|
| `--delay` | duration | type: pause between characters |

### `keydown`

```
iwdp-cli keydown <key>
```

Send keydown for a key on the focused element (no default action).

- MCP tool: none (CLI only)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character |

### `keyup`

```
iwdp-cli keyup <key>
```

Send keyup for a key on the focused element.

- MCP tool: none (CLI only)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character |

### `lease break`

```
iwdp-cli lease break <page>
```

End the lease on a page whoever holds it, with its cleanup.

- MCP tool: none (CLI only)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<page>` | yes | page key, ws:// URL, or part of its title or URL |

### `lease takeover`

```
iwdp-cli lease takeover [<page>]
```

Reattach a lost page and take its lease for this session (needs the device reservation and the device owner's approval).

- MCP tool: none (CLI only)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<page>` | no | page key, ws:// URL, or part of its title or URL; default: the first page |

### `mouse`

```
iwdp-cli mouse <action> [<a>] [<b>] [flags]
```

Low-level mouse: move \<x\> \<y\> [--steps n], down [button], up [button], wheel \<dy\> [dx].

- MCP tool: `mouse` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<action>` | yes | move, down, up or wheel |
| `<a>` | no | move: x; down and up: left, right or middle; wheel: dy |
| `<b>` | no | move: y; wheel: dx |

| Flag | Value | Description |
|---|---|---|
| `--steps` | integer | move: number of increments, each one step (default 1) |

### `network har start`

```
iwdp-cli network har start [flags]
```

Start recording a HAR of the requests from now on; 'network har stop' writes it.

- MCP tool: `har_start` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--content` | `all`, `none` | all: add the response bodies at stop; none: headers and timings only (default `all`) |

### `network har stop`

```
iwdp-cli network har stop [<path>] [flags]
```

Stop the HAR recording and write it as HAR 1.2 JSON (mode 0600; secret values masked unless --show-secrets).

- MCP tool: `har_stop` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | file to write (default network-\<time\>.har in the current directory) |

| Flag | Value | Description |
|---|---|---|
| `--show-secrets` | none | keep secret header, cookie, query and field values |

### `network request`

```
iwdp-cli network request <id> [flags]
```

Show one request of 'network requests': headers, request body and status; --body adds the response body.

- MCP tool: `get_network_request` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<id>` | yes | request id from 'network requests' |

| Flag | Value | Description |
|---|---|---|
| `--body` | none | add the response body (Network.getResponseBody) |
| `--show-secrets` | none | print secret header, query and body values instead of [masked] |

### `network requests`

```
iwdp-cli network requests [flags]
```

List the requests captured since the lease: id, method, status, type and URL; network resets show as gaps.

- MCP tool: `list_network_requests` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--filter` | text | only URLs that contain this text |
| `--type` | text | only this resource type (Document, Script, Fetch, XHR, Image, ...) |
| `--status` | text | only this status: a code such as 404, a class such as 4xx, or failed |
| `--clear` | none | empty the log after printing |
| `--show-secrets` | none | print secret header, query and body values instead of [masked] |

### `network route`

```
iwdp-cli network route <url> [flags]
```

Abort, fulfill or delay requests whose URL matches: a glob with \* and \*\* matches the whole URL, other text any URL that contains it. Without --abort, --body or --status the request continues after --delay.

- MCP tool: `network_route` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | yes | URL pattern |

| Flag | Value | Description |
|---|---|---|
| `--abort` | none | fail the request |
| `--body` | text | answer with this body |
| `--status` | integer | answer with this HTTP status (default 200 with --body) |
| `--content-type` | text | Content-Type of the answer (default text/plain) |
| `--delay` | duration | hold the request this long first |
| `--mode` | `wip`, `js` | wip: WebKit request interception; js: patch fetch and XMLHttpRequest in the page (default `wip`) |

### `network unroute`

```
iwdp-cli network unroute [<url>]
```

Remove the routes with this pattern (all without one); requests that a route holds continue unchanged.

- MCP tool: `network_unroute` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | URL pattern as given to 'network route' |

### `open`

```
iwdp-cli open <url> [flags]
```

Open a URL in the page (location.href), then wait for the load state; a URL that does not start with http:, https:, about:, data: or file: gets https:// (agent-browser open). The MCP tool navigate takes only http and https URLs.

- MCP tool: `navigate` (profile `core`)
- Aliases: `goto`, `navigate`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | yes | the URL to open |

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |

### `press`

```
iwdp-cli press <key> [flags]
```

Press a key or combination (Enter, Tab, Shift+Tab, Control+a) on the focused element, with its emulated default action.

- MCP tool: `press` (profile `core`)
- Aliases: `key`
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character, with optional Control+, Shift+, Alt+, Meta+ modifiers |

| Flag | Value | Description |
|---|---|---|
| `--phase` | `both`, `down`, `up` | down or up sends only keydown or keyup (default `both`) |

### `profiler start`

```
iwdp-cli profiler start
```

Start sampling the page's JavaScript with the WebKit ScriptProfiler (profiler stop writes the .cpuprofile).

- MCP tool: `profiler_start` (profile `devtools`)
- Page: leases the session's page

### `profiler stop`

```
iwdp-cli profiler stop [<path>]
```

Stop sampling and write the CPU profile (.cpuprofile, Chrome DevTools format, mode 0600) to path, or to a new file in the temp directory.

- MCP tool: `profiler_stop` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | the profile file to write (default: a new iwdp-profile-\*.cpuprofile in the temp directory) |

### `pushstate`

```
iwdp-cli pushstate <url>
```

Change the URL without loading: the Next.js router when the page has one, else history.pushState and a popstate event.

- MCP tool: `pushstate` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | yes | the new URL (absolute or relative to the page) |

### `read`

```
iwdp-cli read [<url>] [flags]
```

Print the visible text of the current page, or of an http(s) URL fetched on this computer; --outline prints only the headings.

- MCP tool: `read_page` (profile `core`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | http or https URL to fetch on this computer instead of reading the current page |

| Flag | Value | Description |
|---|---|---|
| `--outline` | none | print only the headings, one "#" per level |

### `reload`

```
iwdp-cli reload [flags]
```

Reload the page (Page.reload after withdrawing unused dialog answers), then wait for the load state.

- MCP tool: `reload` (profile `core`)
- Legacy MCP parameters: `ignore_cache` for `ignore-cache`
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |
| `--ignore-cache` | none | reload without the cache (Page.reload ignoreCache) |

### `removeinitscript`

```
iwdp-cli removeinitscript <id>
```

Remove an init script of this session by the id that addinitscript printed.

- MCP tool: `remove_init_script` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<id>` | yes | script id, for example init1 |

### `screenshot`

```
iwdp-cli screenshot [<path>] [flags]
```

Capture the viewport, the full page or one element as PNG; --annotate labels each ref [N] (@eN), --if-changed skips an identical capture.

- MCP tool: `take_screenshot` (profile `core`)
- Legacy MCP parameters: `full_page` for `full-page`, `if_changed` for `if-changed`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | PNG file to write (default $TMPDIR/iwdp-screenshot-\<time\>.png) |

| Flag | Value | Description |
|---|---|---|
| `-o`, `--output` | file path | PNG file to write (same as path) |
| `--full-page` | none | capture the whole scrollable document |
| `--full` | none | same as --full-page |
| `--selector` | text | capture one element: CSS selector or @eN ref |
| `--annotate` | none | take a snapshot and draw a box and an [N] label for each @eN ref in the capture; the overlay is removed after the capture |
| `--if-changed` | none | compare with this session's previous capture and write nothing when the PNG is identical |

### `scroll`

```
iwdp-cli scroll <direction> [<px>] [flags]
```

Scroll the window or an element up, down, left or right (default 300 px).

- MCP tool: `scroll` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<direction>` | yes | up, down, left or right |
| `<px>` | no | pixels (default 300) |

| Flag | Value | Description |
|---|---|---|
| `--selector` | text | scroll this element instead of the window |

### `scrollintoview`

```
iwdp-cli scrollintoview <target>
```

Scroll an element into the center of the viewport.

- MCP tool: `scroll_into_view` (profile `devtools`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

### `select`

```
iwdp-cli select <target> <values>... [flags]
```

Select options of a \<select\> by value, then by visible label; dispatches input and change.

- MCP tool: `select_option` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | select element: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<values>...` | yes | option values or labels (several for a multiple select) |

| Flag | Value | Description |
|---|---|---|
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `session`

```
iwdp-cli session
```

Print the current session name (--session or IWDP_SESSION; default "default").

- MCP tool: none (CLI only)
- Page: none

### `session info`

```
iwdp-cli session info
```

Show this session's leased pages: page, lease state (active or lost), epoch and last use.

- MCP tool: `session_info` (profile `core`)
- Page: none

### `session list`

```
iwdp-cli session list
```

List the sessions that hold page leases in the daemon.

- MCP tool: none (CLI only)
- Page: none

### `session release`

```
iwdp-cli session release
```

Release this session's page leases and run their cleanup (same as close).

- MCP tool: none (CLI only)
- Page: none

### `set credentials`

```
iwdp-cli set credentials [<user>] [flags]
```

Send HTTP Basic credentials to the requests of one origin only; on a command line the password is the first line of stdin. Supported only where a test has shown that other origins and cross-origin redirects never get them (WebKitGTK today).

- MCP tool: `set_credentials` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<user>` | no | user name (on a command line, the password is the first line of stdin) |

| Flag | Value | Description |
|---|---|---|
| `--origin` | text | the origin that gets the credentials, for example https://example.com |
| `--password` | text | MCP only (a command line refuses it, because argv is readable by other local users; pipe the password to stdin); never logged or printed |
| `--clear` | none | remove every credential of this session instead |

### `set device`

```
iwdp-cli set device [<args>...]
```

Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute.

- MCP tool: none (CLI only)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<args>...` | no | ignored |

### `set geo`

```
iwdp-cli set geo <latitude> <longitude> [flags]
```

Answer navigator.geolocation with this position (a page-side shim: WebKit has no geolocation override).

- MCP tool: `set_geolocation` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<latitude>` | yes | degrees, -90 to 90 |
| `<longitude>` | yes | degrees, -180 to 180 |

| Flag | Value | Description |
|---|---|---|
| `--accuracy` | integer | accuracy in meters (default `10`) |

### `set headers`

```
iwdp-cli set headers <json>
```

Send these extra HTTP headers with every request of the page; '{}' removes this session's headers.

- MCP tool: `set_extra_headers` (profile `devtools`)
- Legacy MCP parameters: `headers` for `json`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<json>` | yes | a JSON object of header names and string values, for example '{"X-A":"1"}' |

### `set media`

```
iwdp-cli set media <scheme> [flags]
```

Emulate prefers-color-scheme (dark, light or no-preference), and optionally reduced motion and more contrast.

- MCP tool: `set_media` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<scheme>` | yes | dark, light or no-preference |

| Flag | Value | Description |
|---|---|---|
| `--reduced-motion` | none | also emulate prefers-reduced-motion: reduce |
| `--contrast` | `more` | also emulate prefers-contrast |

### `set offline`

```
iwdp-cli set offline <state>
```

Fail every request of the page (on) or stop doing so (off), through request interception; unverified on a device.

- MCP tool: `set_offline` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<state>` | yes | on or off |

### `set useragent`

```
iwdp-cli set useragent [<ua>] [flags]
```

Override the user agent string of the page (--reset removes the override).

- MCP tool: `set_user_agent` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<ua>` | no | user agent string |

| Flag | Value | Description |
|---|---|---|
| `--reset` | none | remove the override |

### `set viewport`

```
iwdp-cli set viewport [<args>...]
```

Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute.

- MCP tool: none (CLI only)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<args>...` | no | ignored |

### `skills get`

```
iwdp-cli skills get [<name>...] [flags]
```

Print agent skills built into this binary (the whole SKILL.md); without a name, or with --all, every skill.

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

| Argument | Required | Description |
|---|---|---|
| `<name>...` | no | skill name from 'skills list'; several names print several skills |

| Flag | Value | Description |
|---|---|---|
| `--all` | none | print every skill |

### `skills list`

```
iwdp-cli skills list
```

List the agent skills built into this binary (name and description); 'skills get \<name\>' prints one.

- MCP tool: none (CLI only)
- Aliases: `skills`
- Page: none; runs in the client process, not in the daemon

### `skills path`

```
iwdp-cli skills path [<name>]
```

Write the built-in skills to the user cache directory (once per skills version, mode 0600) and print the directory of one skill or of all.

- MCP tool: none (CLI only)
- Page: none; runs in the client process, not in the daemon

| Argument | Required | Description |
|---|---|---|
| `<name>` | no | skill name; without it, the directory that holds every skill |

### `snapshot`

```
iwdp-cli snapshot [flags]
```

Print the page's accessibility tree with @eN refs for later commands.

- MCP tool: `snapshot` (profile `core`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `-i`, `--interactive` | none | show only elements with refs |
| `-c`, `--compact` | none | keep lines with refs, values or text and their ancestors |
| `-d`, `--depth` | integer | deepest level shown (0 = top level only) |
| `-s`, `--selector` | text | snapshot only this element: CSS selector, locator or @ref |
| `-u`, `--urls` | none | add url= to links |

### `state load`

```
iwdp-cli state load <path>
```

Restore a state file written by state save: the page must be at the file's origin; its cookies and storage are replaced by the file's.

- MCP tool: `state_load` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | yes | the state file to read |

### `state save`

```
iwdp-cli state save <path>
```

Save the cookies, localStorage and sessionStorage of the page's current origin to a JSON file (mode 0600; it holds live credentials).

- MCP tool: `state_save` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | yes | the state file to write |

### `storage`

```
iwdp-cli storage <area> [<action>] [<key>] [<value>] [flags]
```

Read, set or clear localStorage or sessionStorage of the page: storage local [key], storage local set \<key\> \<value\>, storage local clear (same for session).

- MCP tool: `storage` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<area>` | yes | local or session |
| `<action>` | no | a key to read, or get, set or clear (none: list every item) |
| `<key>` | no | get and set: the key |
| `<value>` | no | set: the value |

| Flag | Value | Description |
|---|---|---|
| `--show-values` | none | show the values of password and token keys instead of [masked] |

### `swipe`

```
iwdp-cli swipe <direction> [<px>] [flags]
```

Swipe with synthetic touch events; scrolls as a fallback when the page cancels none (swipe up scrolls down).

- MCP tool: `swipe` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<direction>` | yes | up, down, left or right (the finger's direction) |
| `<px>` | no | pixels (default 300) |

| Flag | Value | Description |
|---|---|---|
| `--selector` | text | swipe on this element instead of the page |

### `tab`

```
iwdp-cli tab [<id>]
```

Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs.

- MCP tool: `tab_select` (profile `core`)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<id>` | no | tab id (t1, t2, ...) or label from tab list; none: list the tabs |

### `tab close`

```
iwdp-cli tab close [<id>]
```

Close a tab (default: the current one) with window.close(); works only for tabs that a script opened.

- MCP tool: `tab_close` (profile `devtools`)
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<id>` | no | tab id (tN) or label; default: the current tab |

### `tab list`

```
iwdp-cli tab list
```

List the pages of the device as tabs t1, t2, ... with labels; \* marks this session's current tab.

- MCP tool: `tab_list` (profile `core`)
- Page: none

### `tab new`

```
iwdp-cli tab new [<url>] [flags]
```

Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab.

- MCP tool: `tab_new` (profile `devtools`)
- Aliases: `window new`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | URL to open (default about:blank) |

| Flag | Value | Description |
|---|---|---|
| `--label` | text | label for the new tab, usable instead of its tN id |

### `tap`

```
iwdp-cli tap <target> [<y>] [flags]
```

Tap an element, or the element at viewport point \<x\> \<y\>: touchstart and touchend, then the click sequence with pointerType touch.

- MCP tool: `tap` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to tap, or the x coordinate of a point (with y): @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<y>` | no | y coordinate of the point to tap; target is then its x coordinate (CSS pixels of the viewport) |

| Flag | Value | Description |
|---|---|---|
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `trace start`

```
iwdp-cli trace start [flags]
```

Start a WebKit Timeline recording of the page for a Chrome trace file (trace stop writes it).

- MCP tool: `trace_start` (profile `devtools`)
- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--categories` | text | comma-separated: timeline (Timeline records, always on) and script (JavaScript events of the ScriptProfiler) (default `timeline,script`) |

### `trace stop`

```
iwdp-cli trace stop [<path>]
```

Stop the Timeline recording and write it as Chrome trace JSON (mode 0600) to path, or to a new file in the temp directory.

- MCP tool: `trace_stop` (profile `devtools`)
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | the trace file to write (default: a new iwdp-trace-\*.json in the temp directory) |

### `type`

```
iwdp-cli type [<target>] <text> [flags]
```

Type text key by key (keydown, keypress, beforeinput, insertion, input, keyup) into an element, or with only \<text\> into the focused element.

- MCP tool: `type_text` (profile `core`)
- Legacy MCP parameters: `selector` for `target`
- Page: leases the session's page
- Default deadline: 30s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | no | input, textarea or contenteditable: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator; omit it to type into the focused element |
| `<text>` | yes | the text; a newline is the Enter key |

| Flag | Value | Description |
|---|---|---|
| `--delay` | duration | pause between characters; each character is then one step |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `uncheck`

```
iwdp-cli uncheck <target> [flags]
```

Uncheck a checkbox (a hidden input through its label) and read the state back.

- MCP tool: none (CLI only)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | checkbox: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `upload`

```
iwdp-cli upload <target> <files>...
```

Set the files of a file input (read on the host, at most 8 MiB in total) and dispatch input and change.

- MCP tool: `upload_file` (profile `core`)
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | file input: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<files>...` | yes | file paths, relative to the current directory |

### `vitals`

```
iwdp-cli vitals
```

Show the page's Web Vitals: LCP, CLS, TTFB, FCP, INP, DOMContentLoaded and load (unavailable ones say why).

- MCP tool: `vitals` (profile `devtools`)
- Page: leases the session's page

### `wait`

```
iwdp-cli wait [<target>] [flags]
```

Wait for milliseconds, an element state, text, a URL glob, a load state or a JavaScript condition (default timeout 25 s; exit 3 on timeout).

- MCP tool: `wait` (profile `core`)
- Page: leases the session's page
- Default deadline: 25.15s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | no | milliseconds, or an element: @eN ref, CSS selector or locator |

| Flag | Value | Description |
|---|---|---|
| `--state` | `visible`, `hidden`, `attached`, `detached` | element state to wait for (default `visible`) |
| `--text` | text | wait until this text is visible on the page |
| `--url` | text | wait until the URL matches this glob (\* within a path segment, \*\* across segments) |
| `--load` | `domcontentloaded`, `load`, `networkidle` | wait until document.readyState reaches this load state, or (networkidle) until no request has been in flight for 500 ms |
| `--fn` | text | wait until this JavaScript expression is truthy (polled every 100 ms) |
