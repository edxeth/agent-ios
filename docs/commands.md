# Command reference

<!-- Generated from the command registry by "make docs" (go run ./internal/commands/refgen docs/commands.md). Do not edit by hand. -->

This page lists every registry command of `agent-ios`: the commands that run in the agent-ios daemon and share sessions, leases and refs. For each command it gives the usage, the arguments and the flags.

- `agent-ios` runs only these commands. [The migration notes](migration.md) map each removed command of earlier versions (for example `pages`, `set-breakpoint`, `get-matched-styles`) to its registry command, or say that it was removed.
- `agent-ios <command> --help` prints the same usage and never connects to a page.

Global flags (any position): `--session`, `--page`, `--only-tab`, `--timeout`, `--json`, `--transport`, `--max-output`, `--content-boundaries`, `--reservation`, `--wait-lease`, `--takeover`, `--private`.

## Index

| Command | Summary |
|---|---|
| [`addinitscript`](#addinitscript) | Run a script in every new document of the page, before page scripts when the page is paused on start (partial otherwise). Prints the script id. |
| [`back`](#back) | Go back in the page's history (history.back), then wait for the load state. |
| [`blur`](#blur) | Blur the focused element. |
| [`cdp log`](#cdp-log) | Show how often CDP clients sent methods that the endpoint does not translate (answered "wasn't found", never forwarded). |
| [`cdp start`](#cdp-start) | Start the CDP endpoint on 127.0.0.1 (a running endpoint is kept); then run 'get cdp-url' for a client URL. |
| [`cdp status`](#cdp-status) | Show the CDP endpoint: address, allowed origins and, per endpoint record, its session, page, scope, connections and attachments. |
| [`cdp stop`](#cdp-stop) | Stop the CDP endpoint: close its client connections (each releases its state) and revoke every CDP URL. |
| [`check`](#check) | Check a checkbox or radio (a hidden input through its label) and read the state back. |
| [`click`](#click) | Click an element with synthetic pointer and mouse events after the actionability checks. |
| [`close`](#close) | End this session (--session): release its page leases and run their cleanup. The Safari tab stays open; other sessions are not touched. |
| [`console`](#console) | Print the console messages captured since the lease (and up to 100 that WebKit replays). For new messages only, run it again as console --since "$last_id", with the id of the last message that it printed. |
| [`console clear`](#console-clear) | Empty the console buffer and WebKit's stored messages. |
| [`cookies`](#cookies) | List the page's cookies (those Page.getCookies lists, httpOnly included): name, domain, path, sameSite, expiry or session, httpOnly, secure. Values are hidden unless --show-values. |
| [`cookies clear`](#cookies-clear) | Delete the page's cookies (those Page.getCookies lists: the cookies of the page's frames and resources), read back. |
| [`cookies set`](#cookies-set) | Set one cookie (cookies set \<name\> \<value\> [flags]) or import cookies from a file (--curl: a JSON list, a cURL command or a Cookie header), each read back. Values are redacted in the result. |
| [`daemon logs`](#daemon-logs) | Print the end of the daemon log ($XDG_STATE_HOME/agent-ios/daemon.log). |
| [`daemon run`](#daemon-run) | Run the daemon in the foreground until SIGTERM, 'daemon stop' or the idle timeout (clients start it on demand). |
| [`daemon start`](#daemon-start) | Start the daemon in the background unless it runs. |
| [`daemon status`](#daemon-status) | Show the daemon, its pages, leases and device reservations. |
| [`daemon stop`](#daemon-stop) | Stop the daemon after the cleanup of every page (5 s at most). |
| [`dblclick`](#dblclick) | Double-click an element: two click sequences (detail 1 and 2), then dblclick (left button only, as in Chrome). |
| [`device list`](#device-list) | List the live device reservations. |
| [`device release`](#device-release) | Release an owner's device reservation. |
| [`device reserve`](#device-reserve) | Reserve a physical device for an owner, so that only that owner's sessions attach to its pages. |
| [`devices`](#devices) | List the devices of ios_webkit_debug_proxy's listing (name, UDID, the address of its pages). Starts ios_webkit_debug_proxy when it is not running, and on Linux restarts the one that agent-ios started when it lost its devices, as page commands do. Exits 5 when it lists no device although usbmuxd reports one. |
| [`dialog`](#dialog) | Arm answers for alert, confirm and prompt, or show the dialog log (status, accept [text], dismiss, default accept\|dismiss). |
| [`diff screenshot`](#diff-screenshot) | Compare a viewport screenshot of the page with a baseline PNG, pixel by pixel on the host, and report how many pixels differ. |
| [`diff snapshot`](#diff-snapshot) | Show what changed between the last two snapshots of this session (a unified line diff). |
| [`drag`](#drag) | Drag from one element to another in 10 pointer steps with synthetic drag events (best effort). |
| [`emulate network`](#emulate-network) | Throttle the page's network to a preset (4g, fast-3g, slow-3g) or a byte rate; 'none' removes the throttle. Uses Network.setEmulatedConditions, which only the iOS 16.4 to 17.x and 27.3+ command lists have. |
| [`errors`](#errors) | Print uncaught errors and unhandled promise rejections with their stacks (the first run installs the error hooks). |
| [`eval`](#eval) | Evaluate a JavaScript expression in the page and print the result, awaiting a returned Promise; the page is the session's, or the tab id or ws:// URL of --page. A string prints as is, other values as JSON; a thrown value exits 1 with its message and stack. |
| [`fill`](#fill) | Replace an input's value: beforeinput (insertReplacementText), the native value setter, input and change. |
| [`find`](#find) | Find elements by role, text, label, placeholder, alt, title, testid, or first\|last\|nth of a selector; print them with refs. |
| [`focus`](#focus) | Focus an element (el.focus()). |
| [`forward`](#forward) | Go forward in the page's history (history.forward), then wait for the load state. |
| [`frame`](#frame) | Make an iframe (@eN, CSS or locator) the frame that this session's later commands work in, or return to the top document with main. |
| [`get`](#get) | Get url, title, or text, html, value, attr, count, box or styles of a target (@ref, CSS selector or locator). |
| [`get cdp-url`](#get-cdp-url) | Print a CDP URL for this session's page (starts the CDP endpoint if needed). Clients act for this session; use the URL exactly as printed. |
| [`highlight`](#highlight) | Highlight an element on the device screen (DOM.highlightNode) until release. |
| [`hit-test`](#hit-test) | Print the element at a viewport point (CSS pixels): its snapshot line with a ref, its tag and its box. |
| [`hover`](#hover) | Move the pointer over an element (synthetic events do not set CSS :hover; --css forces it). |
| [`is`](#is) | Print true or false: is a target visible, enabled, checked, editable or focused. |
| [`keyboard`](#keyboard) | Type into the focused element (keyboard type \<text\>) or insert text without key events (keyboard inserttext \<text\>). |
| [`keydown`](#keydown) | Press a key without releasing it: keydown, then unless canceled keypress and the insertion or default action; a modifier key (Shift, Control, Alt, Meta) stays held for later press, keydown, keyup and mouse commands until keyup. |
| [`keyup`](#keyup) | Send keyup for a key on the focused element; a modifier key stops being held. |
| [`lease break`](#lease-break) | End the lease on a page whoever holds it, with its cleanup. |
| [`lease takeover`](#lease-takeover) | Reattach a lost page and take its lease for this session (needs the device reservation and the device owner's approval). |
| [`mouse`](#mouse) | Low-level mouse: move \<x\> \<y\> [--steps n], down [button], up [button], wheel \<dy\> [dx]; the events carry the modifiers that keydown holds. |
| [`network har start`](#network-har-start) | Start recording a HAR of the requests from now on; 'network har stop' writes it. |
| [`network har stop`](#network-har-stop) | Stop the HAR recording and write it as HAR 1.2 JSON (mode 0600; secret values masked unless --show-secrets). |
| [`network request`](#network-request) | Show one request of 'network requests': headers, request body and status; --body adds the response body. |
| [`network requests`](#network-requests) | List the requests captured since the lease: id, method, status, type and URL; network resets show as gaps. |
| [`network route`](#network-route) | Abort, fulfill or delay requests whose URL matches: a glob with \* and \*\* matches the whole URL, other text any URL that contains it. Without --abort, --body or --status the request continues after --delay. |
| [`network unroute`](#network-unroute) | Remove the routes with this pattern (all without one); requests that a route holds continue unchanged. |
| [`open`](#open) | Open a URL in the page (location.href), then wait for the load state; a URL that does not start with http:, https:, about:, data: or file: gets https:// (agent-browser open). |
| [`press`](#press) | Press a key or combination (Enter, Tab, Shift+Tab, Control+a) on the focused element, with its emulated default action; chord modifiers get their own key events, and modifiers held by keydown apply. |
| [`profiler start`](#profiler-start) | Start sampling the page's JavaScript with the WebKit ScriptProfiler (profiler stop writes the .cpuprofile). |
| [`profiler stop`](#profiler-stop) | Stop sampling and write the CPU profile (.cpuprofile, Chrome DevTools format, mode 0600) to path, or to a new file in the temp directory. |
| [`pushstate`](#pushstate) | Change the URL without loading: the Next.js router when the page has one, else history.pushState and a popstate event. |
| [`read`](#read) | Print the visible text of the current page, or of an http(s) URL fetched on this computer; --outline prints only the headings. |
| [`reload`](#reload) | Reload the page (Page.reload after withdrawing unused dialog answers), then wait for the load state. |
| [`removeinitscript`](#removeinitscript) | Remove an init script of this session by the id that addinitscript printed. |
| [`restart-iwdp`](#restart-iwdp) | Restart the ios_webkit_debug_proxy that agent-ios started. Stops only an iwdp that agent-ios started and recorded, then starts a new one. Any other iwdp (for example one that the user or another program started) is never stopped: the command fails and names the owner pid and the manual restart command. When no iwdp runs, it starts one. |
| [`screenshot`](#screenshot) | Capture the viewport, the full page or one element as PNG; --annotate labels each ref [N] (@eN), --if-changed skips an identical capture. |
| [`scroll`](#scroll) | Scroll the window or an element up, down, left or right (default 300 px). |
| [`scrollintoview`](#scrollintoview) | Scroll an element into the center of the viewport. |
| [`select`](#select) | Select options of a \<select\> by value, then by visible label; dispatches input and change. |
| [`session`](#session) | Print the current session name (--session or AGENT_IOS_SESSION; default "default"). |
| [`session info`](#session-info) | Show this session's leased pages: page, lease state (active or lost), epoch and last use. |
| [`session list`](#session-list) | List the sessions that hold page leases in the daemon. |
| [`session release`](#session-release) | Release this session's page leases and run their cleanup (same as close). |
| [`set credentials`](#set-credentials) | Send HTTP Basic credentials to the requests of one origin only; on a command line the password is the first line of stdin. Supported only where a test has shown that other origins and cross-origin redirects never get them (WebKitGTK today). |
| [`set device`](#set-device) | Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute. |
| [`set geo`](#set-geo) | Answer navigator.geolocation with this position (a page-side shim: WebKit has no geolocation override). |
| [`set headers`](#set-headers) | Send these extra HTTP headers with every request of the page; '{}' removes this session's headers. |
| [`set media`](#set-media) | Emulate prefers-color-scheme (dark, light or no-preference), and optionally reduced motion and more contrast. |
| [`set offline`](#set-offline) | Fail every request of the page (on) or stop doing so (off), through request interception; navigator.onLine stays true. |
| [`set useragent`](#set-useragent) | Override the user agent string of the page (--reset removes the override). |
| [`set viewport`](#set-viewport) | Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute. |
| [`sim down`](#sim-down) | Stop the forward that sim up started and shut the simulator down; with --mac also run the configuration's stop command (for example the shutdown of a VM). |
| [`sim keyboard`](#sim-keyboard) | Show or set the simulator's keyboard: onscreen shows iOS's on-screen keyboard when sim tap taps a field, as on an iPhone, for keyboard bugs; sim type then sends only lowercase letters, digits and unshifted symbols, on a US layout. hardware (the Simulator's default) types the text that the simulator's keyboard layout has keys for (AXe refuses the rest) but hides the on-screen keyboard. A change restarts the Simulator app and the simulator, then runs sim up; it refuses while a page has an active lease, unless --restart-iwdp. |
| [`sim release`](#sim-release) | End your turn on the simulator: close this session (release its page leases, as close does) and release the owner's reservation of the simulator. Close your own tab first (tab close; when Safari refuses, release anyway). Releasing no reservation is not an error; another owner's is refused. |
| [`sim reserve`](#sim-reserve) | Reserve the simulator for an owner, waiting up to --wait while another owner holds it; renews the owner's own reservation. While an owner holds it, other owners' page commands on the simulator fail (exit 6), its tabs are hidden from them, and their sim commands that act on it fail. Exit 3 when the wait ends first: the simulator is still another owner's; run the same command again (do not use the simulator meanwhile). The reservation ends with sim release, or --ttl (2m) after the owner's last use (sim commands renew it, and an open page lease keeps renewing it). The owner must be your own name: the default session and qa-login, the name of the guides' examples, are refused. |
| [`sim screenshot`](#sim-screenshot) | Capture the simulator's whole screen as PNG (simctl io screenshot): the status bar, Safari's bars, the on-screen keyboard and other apps, which the page capture of screenshot leaves out. The simulator must be booted (sim up); it starts nothing. |
| [`sim status`](#sim-status) | Check each part that sim up sets up (the Mac over ssh, the desktop login, the simulator, its window, the forward, iwdp's listing) without changing any. Exits 0 also when a part is down; --json has "ready". |
| [`sim swipe`](#sim-swipe) | Swipe on the simulator's screen from \<x1\> \<y1\> to \<x2\> \<y2\> in points (one second; --duration changes it). A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone). |
| [`sim tap`](#sim-tap) | Tap the simulator's screen at \<x\> \<y\>, or the element with --label or --id (sim ui lists them). A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone). |
| [`sim type`](#sim-type) | Type \<text\> into the simulator's focused field as key presses (sim tap the field first). The text is on the command lines of this host and the Mac: never type a secret this way; fill --stdin types into a page without that. A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone). |
| [`sim ui`](#sim-ui) | List the elements of the simulator's screen that have a label, value or identifier, with their frames in points (its accessibility tree, through AXe): the targets of sim tap. It shows page and app text. |
| [`sim up`](#sim-up) | Make the iOS Simulator of the Mac in the simulator configuration ready for agent-ios, doing only what is still needed: start the Mac with the configuration's start command when ssh gets no answer, wait for the desktop login, boot the simulator, open its window, start Safari, forward 127.0.0.1:27753 to its Web Inspector over ssh, and start ios_webkit_debug_proxy, or restart the one that agent-ios started when it does not use that forward. Refuses that restart while a page has an active lease, unless --restart-iwdp, and refuses an iwdp that agent-ios did not start. Exit 7 when Safari shows no tab of --url in time (check your server); it then closes the new tab that its open made, or names it for tab close. Default deadline 15m. |
| [`skills get`](#skills-get) | Print agent skills built into this binary (each guide without its front matter; --json gives the whole SKILL.md); without a name, or with --all, every skill. |
| [`skills list`](#skills-list) | List the agent skills built into this binary (name and description); 'skills get \<name\>' prints one. |
| [`skills path`](#skills-path) | Write the built-in skills to the user cache directory (once per skills version, mode 0600) and print the directory of one skill or of all. |
| [`snapshot`](#snapshot) | Print the page's accessibility tree with @eN refs for later commands. |
| [`state load`](#state-load) | Restore a state file written by state save: the page must be at the file's origin; its cookies and storage are replaced by the file's. |
| [`state save`](#state-save) | Save the cookies, localStorage and sessionStorage of the page's current origin to a JSON file (mode 0600; it holds live credentials). |
| [`status`](#status) | Check whether ios_webkit_debug_proxy answers on its listing address, count the devices that it lists, and name the log of an iwdp that agent-ios starts. It starts nothing. Exits 1 when iwdp does not answer or its device list cannot be read. |
| [`storage`](#storage) | Read, set or clear localStorage or sessionStorage of the page: storage local [key], storage local set \<key\> \<value\>, storage local clear (same for session). |
| [`swipe`](#swipe) | Swipe with synthetic touch events; scrolls as a fallback when the page cancels none (swipe up scrolls down). |
| [`tab`](#tab) | Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs. |
| [`tab close`](#tab-close) | Close a tab (default: the current one) with window.close(); works only for tabs that a script opened. In your turn on the simulator, a session that selected no tab must name the tab (exit 5). |
| [`tab find`](#tab-find) | Print the id of the tab whose page shows exactly this URL (query and fragment included), compared inside the page. It only reads: it navigates and selects nothing. The session keeps the lease of the tab that the command prints, as after any read of that tab, and no lease that only the command used on another tab. Exit 7 when no tab shows the URL; exit 1 when several do or a tab could not be read; exit 6 when the listing hides pages of a device that the caller has not reserved (--device limits the search, and that count, to one device). |
| [`tab list`](#tab-list) | List the pages of the device as tabs t1, t2, ... with labels; \* marks this session's current tab. |
| [`tab new`](#tab-new) | Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab; the session's leases on other pages end. |
| [`tap`](#tap) | Tap an element, or the element at viewport point \<x\> \<y\>: touchstart and touchend, then the click sequence with pointerType touch. |
| [`trace start`](#trace-start) | Start a WebKit Timeline recording of the page for a Chrome trace file (trace stop writes it). |
| [`trace stop`](#trace-stop) | Stop the Timeline recording and write it as Chrome trace JSON (mode 0600) to path, or to a new file in the temp directory. |
| [`type`](#type) | Type text key by key (keydown, keypress, beforeinput, insertion, input, keyup) into an element, or with only \<text\> into the focused element. |
| [`uncheck`](#uncheck) | Uncheck a checkbox (a hidden input through its label) and read the state back. |
| [`upload`](#upload) | Set the files of a file input (read on the host, at most 8 MiB in total) and dispatch input and change. |
| [`vitals`](#vitals) | Show the page's Web Vitals: LCP, CLS, TTFB, FCP, INP, DOMContentLoaded and load (unavailable ones say why). |
| [`wait`](#wait) | Wait for milliseconds, an element state, text, a URL glob, a load state or a JavaScript condition (default timeout 25 s; exit 3 on timeout). |

## Commands

### `addinitscript`

```
agent-ios addinitscript [<script>] [flags]
```

Run a script in every new document of the page, before page scripts when the page is paused on start (partial otherwise). Prints the script id.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<script>` | no | JavaScript source (or use --file) |

| Flag | Value | Description |
|---|---|---|
| `--file` | file path | read the script from this file |

### `back`

```
agent-ios back [flags]
```

Go back in the page's history (history.back), then wait for the load state.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |

### `blur`

```
agent-ios blur
```

Blur the focused element.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

### `cdp log`

```
agent-ios cdp log
```

Show how often CDP clients sent methods that the endpoint does not translate (answered "wasn't found", never forwarded).

- Page: none

### `cdp start`

```
agent-ios cdp start [flags]
```

Start the CDP endpoint on 127.0.0.1 (a running endpoint is kept); then run 'get cdp-url' for a client URL.

- Page: none

| Flag | Value | Description |
|---|---|---|
| `--port` | integer | port on 127.0.0.1 (default: the daemon's --cdp-port, else 9230; 0 picks a free port) |
| `--allow-origin` | text | comma-separated Origin header values to accept (default: none, every request with an Origin header is refused) |

### `cdp status`

```
agent-ios cdp status
```

Show the CDP endpoint: address, allowed origins and, per endpoint record, its session, page, scope, connections and attachments.

- Page: none

### `cdp stop`

```
agent-ios cdp stop
```

Stop the CDP endpoint: close its client connections (each releases its state) and revoke every CDP URL.

- Page: none

### `check`

```
agent-ios check <target> [flags]
```

Check a checkbox or radio (a hidden input through its label) and read the state back.

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
agent-ios click <target> [flags]
```

Click an element with synthetic pointer and mouse events after the actionability checks.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to click: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--button` | text | mouse button: left, right, middle, back or forward (default left); right ends with contextmenu, the others but left with auxclick |
| `--modifiers` | text | modifier keys held during the click: alt, control (ctrl), meta (cmd, command), shift, joined by , or +; every event carries them |
| `--count` | integer | number of clicks (detail 1..n) |
| `--new-tab` | none | open the link's href with window.open (emulated user gesture) instead of clicking; a gesture call delivered after its window opens nothing, but the page may then hold a transient user activation for a few seconds; not with --button or --modifiers |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `close`

```
agent-ios close
```

End this session (--session): release its page leases and run their cleanup. The Safari tab stays open; other sessions are not touched.

- Page: none

### `console`

```
agent-ios console [flags]
```

Print the console messages captured since the lease (and up to 100 that WebKit replays). For new messages only, run it again as console --since "$last_id", with the id of the last message that it printed.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--clear` | none | empty the buffer after printing |
| `--level` | `debug`, `log`, `info`, `warning`, `error` | print only messages of this level or above |
| `--since` | integer | print only messages with an id above this one |

### `console clear`

```
agent-ios console clear
```

Empty the console buffer and WebKit's stored messages.

- Page: leases the session's page

### `cookies`

```
agent-ios cookies [flags]
```

List the page's cookies (those Page.getCookies lists, httpOnly included): name, domain, path, sameSite, expiry or session, httpOnly, secure. Values are hidden unless --show-values.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--show-values` | none | print the cookie values (they can be session tokens) |

### `cookies clear`

```
agent-ios cookies clear [flags]
```

Delete the page's cookies (those Page.getCookies lists: the cookies of the page's frames and resources), read back.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--domain` | text | delete only the cookies of this domain and its subdomains |

### `cookies set`

```
agent-ios cookies set [<name>] [<value>] [flags]
```

Set one cookie (cookies set \<name\> \<value\> [flags]) or import cookies from a file (--curl: a JSON list, a cURL command or a Cookie header), each read back. Values are redacted in the result.

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
agent-ios daemon logs [flags]
```

Print the end of the daemon log ($XDG_STATE_HOME/agent-ios/daemon.log).

- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `-n`, `--lines` | integer | number of lines from the end (default `50`) |

### `daemon run`

```
agent-ios daemon run [flags]
```

Run the daemon in the foreground until SIGTERM, 'daemon stop' or the idle timeout (clients start it on demand).

- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `--addr` | text | listing or inspector address of the --transport (webkitgtk: the WEBKIT_INSPECTOR_HTTP_SERVER address) |
| `--idle` | duration (40s, 1m30s) or seconds (40) | exit after this long without requests and leases (default AGENT_IOS_DAEMON_IDLE, else 15m) |
| `--cdp-port` | integer | start the CDP endpoint on 127.0.0.1:\<port\> with the daemon, and use the port for later starts (default: the endpoint starts only on 'cdp start' or 'get cdp-url', on port 9230) |

### `daemon start`

```
agent-ios daemon start [flags]
```

Start the daemon in the background unless it runs.

- Page: none; runs in the client process, not in the daemon

| Flag | Value | Description |
|---|---|---|
| `--addr` | text | listing or inspector address of the --transport (webkitgtk: the WEBKIT_INSPECTOR_HTTP_SERVER address) |
| `--idle` | duration (40s, 1m30s) or seconds (40) | exit after this long without requests and leases (default AGENT_IOS_DAEMON_IDLE, else 15m) |
| `--cdp-port` | integer | start the CDP endpoint on 127.0.0.1:\<port\> with the daemon, and use the port for later starts (default: the endpoint starts only on 'cdp start' or 'get cdp-url', on port 9230) |

### `daemon status`

```
agent-ios daemon status
```

Show the daemon, its pages, leases and device reservations.

- Page: none; runs in the client process, not in the daemon

### `daemon stop`

```
agent-ios daemon stop
```

Stop the daemon after the cleanup of every page (5 s at most).

- Page: none; runs in the client process, not in the daemon

### `dblclick`

```
agent-ios dblclick <target> [flags]
```

Double-click an element: two click sequences (detail 1 and 2), then dblclick (left button only, as in Chrome).

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to double-click: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

| Flag | Value | Description |
|---|---|---|
| `--button` | text | mouse button: left, right, middle, back or forward (default left); right ends with contextmenu, the others but left with auxclick |
| `--modifiers` | text | modifier keys held during the click: alt, control (ctrl), meta (cmd, command), shift, joined by , or +; every event carries them |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `device list`

```
agent-ios device list
```

List the live device reservations.

- Page: none

### `device release`

```
agent-ios device release [flags]
```

Release an owner's device reservation.

- Page: none

| Flag | Value | Description |
|---|---|---|
| `--device` | text | device UDID; \* reserves every device (default `*`) |
| `--owner` | text | reservation owner; default --reservation (AGENT_IOS_DEVICE_RESERVATION), else the session |

### `device reserve`

```
agent-ios device reserve [flags]
```

Reserve a physical device for an owner, so that only that owner's sessions attach to its pages.

- Page: none

| Flag | Value | Description |
|---|---|---|
| `--device` | text | device UDID; \* reserves every device (default `*`) |
| `--owner` | text | reservation owner; default --reservation (AGENT_IOS_DEVICE_RESERVATION), else the session |
| `--ttl` | duration (40s, 1m30s) or seconds (40) | lifetime, renewed while the owner holds leases (default `2h`) |

### `devices`

```
agent-ios devices
```

List the devices of ios_webkit_debug_proxy's listing (name, UDID, the address of its pages). Starts ios_webkit_debug_proxy when it is not running, and on Linux restarts the one that agent-ios started when it lost its devices, as page commands do. Exits 5 when it lists no device although usbmuxd reports one.

- Page: none

### `dialog`

```
agent-ios dialog <action> [<value>]
```

Arm answers for alert, confirm and prompt, or show the dialog log (status, accept [text], dismiss, default accept|dismiss).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<action>` | yes | status, accept, dismiss or default |
| `<value>` | no | accept: the prompt text; default: accept or dismiss |

### `diff screenshot`

```
agent-ios diff screenshot [flags]
```

Compare a viewport screenshot of the page with a baseline PNG, pixel by pixel on the host, and report how many pixels differ.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--baseline` | file path | the baseline PNG (for example a viewport screenshot taken earlier) |
| `--threshold` | text | a pixel differs when a colour or alpha value changes by more than this fraction of 255 (0 to below 1) (default `0.1`) |

### `diff snapshot`

```
agent-ios diff snapshot
```

Show what changed between the last two snapshots of this session (a unified line diff).

- Page: uses the session's page

### `drag`

```
agent-ios drag <target> <destination> [flags]
```

Drag from one element to another in 10 pointer steps with synthetic drag events (best effort).

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
agent-ios emulate network [<preset>] [flags]
```

Throttle the page's network to a preset (4g, fast-3g, slow-3g) or a byte rate; 'none' removes the throttle. Uses Network.setEmulatedConditions, which only the iOS 16.4 to 17.x and 27.3+ command lists have.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<preset>` | no | 4g, fast-3g, slow-3g or none |

| Flag | Value | Description |
|---|---|---|
| `--bytes_per_second_limit` | integer | a rate in bytes per second instead of a preset; 0 removes the throttle |

### `errors`

```
agent-ios errors [flags]
```

Print uncaught errors and unhandled promise rejections with their stacks (the first run installs the error hooks).

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--clear` | none | empty the error list after printing |

### `eval`

```
agent-ios eval <expression> [flags]
```

Evaluate a JavaScript expression in the page and print the result, awaiting a returned Promise; the page is the session's, or the tab id or ws:// URL of --page. A string prints as is, other values as JSON; a thrown value exits 1 with its message and stack.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<expression>` | yes | the JavaScript expression (quote it for the shell) |

| Flag | Value | Description |
|---|---|---|
| `--await` | none | accepted for the older command line; eval always awaits a returned Promise |

### `fill`

```
agent-ios fill <target> [<text>] [flags]
```

Replace an input's value: beforeinput (insertReplacementText), the native value setter, input and change.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | input, textarea or contenteditable: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<text>` | no | the new value (or --stdin) |

| Flag | Value | Description |
|---|---|---|
| `--stdin` | none | read the value from the standard input (agent-ios only), so a secret is not in the command line; one trailing newline is removed |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `find`

```
agent-ios find <kind> <value> [<action>...] [flags]
```

Find elements by role, text, label, placeholder, alt, title, testid, or first|last|nth of a selector; print them with refs.

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
agent-ios focus <target>
```

Focus an element (el.focus()).

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element to focus: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

### `forward`

```
agent-ios forward [flags]
```

Go forward in the page's history (history.forward), then wait for the load state.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |

### `frame`

```
agent-ios frame <target>
```

Make an iframe (@eN, CSS or locator) the frame that this session's later commands work in, or return to the top document with main.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | main, or the iframe: @eN ref, CSS selector or locator (in the current frame) |

### `get`

```
agent-ios get <what> [<target>] [<name>]
```

Get url, title, or text, html, value, attr, count, box or styles of a target (@ref, CSS selector or locator).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<what>` | yes | url, title, text, html, value, attr, count, box or styles |
| `<target>` | no | @ref, CSS selector or locator such as text=Save (not for url and title) |
| `<name>` | no | attr: the attribute name; styles: comma-separated properties (default: a common set) |

### `get cdp-url`

```
agent-ios get cdp-url [flags]
```

Print a CDP URL for this session's page (starts the CDP endpoint if needed). Clients act for this session; use the URL exactly as printed.

- Page: none

| Flag | Value | Description |
|---|---|---|
| `--scope` | `page`, `device` | page: only this page; device: also the other pages of the devices reserved by --reservation (default `page`) |
| `--form` | `ws`, `http` | ws: the browser WebSocket URL (every client; agent-browser --cdp, Puppeteer browserWSEndpoint, Playwright connectOverCDP); http: the discovery base for Playwright connectOverCDP and Puppeteer browserURL (default `ws`) |

### `highlight`

```
agent-ios highlight [<target>] [flags]
```

Highlight an element on the device screen (DOM.highlightNode) until release.

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
agent-ios hit-test <x> <y>
```

Print the element at a viewport point (CSS pixels): its snapshot line with a ref, its tag and its box.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<x>` | yes | viewport x in CSS pixels |
| `<y>` | yes | viewport y in CSS pixels |

### `hover`

```
agent-ios hover <target> [flags]
```

Move the pointer over an element (synthetic events do not set CSS :hover; --css forces it).

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
agent-ios is <state> <target>
```

Print true or false: is a target visible, enabled, checked, editable or focused.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<state>` | yes | visible, enabled, checked, editable or focused |
| `<target>` | yes | @ref, CSS selector or locator such as label=Email |

### `keyboard`

```
agent-ios keyboard <mode> <text> [flags]
```

Type into the focused element (keyboard type \<text\>) or insert text without key events (keyboard inserttext \<text\>).

- Page: leases the session's page
- Default deadline: 30s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<mode>` | yes | type or inserttext |
| `<text>` | yes | the text |

| Flag | Value | Description |
|---|---|---|
| `--delay` | duration with a unit (50ms, 1s) | type: pause between characters |

### `keydown`

```
agent-ios keydown <key>
```

Press a key without releasing it: keydown, then unless canceled keypress and the insertion or default action; a modifier key (Shift, Control, Alt, Meta) stays held for later press, keydown, keyup and mouse commands until keyup.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character (Shift, ShiftLeft, ShiftRight, Control, Alt, Meta and their Left/Right forms hold a modifier) |

### `keyup`

```
agent-ios keyup <key>
```

Send keyup for a key on the focused element; a modifier key stops being held.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character |

### `lease break`

```
agent-ios lease break <page>
```

End the lease on a page whoever holds it, with its cleanup.

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<page>` | yes | page key, ws:// URL, or part of its title or URL |

### `lease takeover`

```
agent-ios lease takeover [<page>]
```

Reattach a lost page and take its lease for this session (needs the device reservation and the device owner's approval).

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<page>` | no | page key, ws:// URL, or part of its title or URL; default: the first page |

### `mouse`

```
agent-ios mouse <action> [<a>] [<b>] [flags]
```

Low-level mouse: move \<x\> \<y\> [--steps n], down [button], up [button], wheel \<dy\> [dx]; the events carry the modifiers that keydown holds.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<action>` | yes | move, down, up or wheel |
| `<a>` | no | move: x; down and up: left, right, middle, back or forward; wheel: dy |
| `<b>` | no | move: y; wheel: dx |

| Flag | Value | Description |
|---|---|---|
| `--steps` | integer | move: number of increments, each one step (default 1) |

### `network har start`

```
agent-ios network har start [flags]
```

Start recording a HAR of the requests from now on; 'network har stop' writes it.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--content` | `all`, `none` | all: add the response bodies at stop; none: headers and timings only (default `all`) |

### `network har stop`

```
agent-ios network har stop [<path>] [flags]
```

Stop the HAR recording and write it as HAR 1.2 JSON (mode 0600; secret values masked unless --show-secrets).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | file to write (default network-\<time\>.har in the current directory) |

| Flag | Value | Description |
|---|---|---|
| `--show-secrets` | none | keep secret header, cookie, query and field values |

### `network request`

```
agent-ios network request <id> [flags]
```

Show one request of 'network requests': headers, request body and status; --body adds the response body.

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
agent-ios network requests [flags]
```

List the requests captured since the lease: id, method, status, type and URL; network resets show as gaps.

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
agent-ios network route <url> [flags]
```

Abort, fulfill or delay requests whose URL matches: a glob with \* and \*\* matches the whole URL, other text any URL that contains it. Without --abort, --body or --status the request continues after --delay.

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
| `--delay` | duration (40s, 1m30s) or seconds (40) | hold the request this long first |
| `--mode` | `wip`, `js` | wip: WebKit request interception; js: patch fetch and XMLHttpRequest in the page (default `wip`) |

### `network unroute`

```
agent-ios network unroute [<url>]
```

Remove the routes with this pattern (all without one); requests that a route holds continue unchanged.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | URL pattern as given to 'network route' |

### `open`

```
agent-ios open [<url>] [flags]
```

Open a URL in the page (location.href), then wait for the load state; a URL that does not start with http:, https:, about:, data: or file: gets https:// (agent-browser open).

- Aliases: `goto`, `navigate`
- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | the URL to open; or --url-file |

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |
| `--url-file` | file path | read the URL from this file (one line), so the URL is not on the command line |

### `press`

```
agent-ios press <key> [flags]
```

Press a key or combination (Enter, Tab, Shift+Tab, Control+a) on the focused element, with its emulated default action; chord modifiers get their own key events, and modifiers held by keydown apply.

- Aliases: `key`
- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<key>` | yes | key name or character, with optional Control+, Shift+, Alt+, Meta+ modifiers |

| Flag | Value | Description |
|---|---|---|
| `--phase` | `both`, `down`, `up` | down sends a bare keydown (no keypress, insertion or default action; agent-ios only), up only keyup (default `both`) |

### `profiler start`

```
agent-ios profiler start
```

Start sampling the page's JavaScript with the WebKit ScriptProfiler (profiler stop writes the .cpuprofile).

- Page: leases the session's page

### `profiler stop`

```
agent-ios profiler stop [<path>]
```

Stop sampling and write the CPU profile (.cpuprofile, Chrome DevTools format, mode 0600) to path, or to a new file in the temp directory.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | the profile file to write (default: a new agent-ios-profile-\*.cpuprofile in the temp directory) |

### `pushstate`

```
agent-ios pushstate <url>
```

Change the URL without loading: the Next.js router when the page has one, else history.pushState and a popstate event.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | yes | the new URL (absolute or relative to the page) |

### `read`

```
agent-ios read [<url>] [flags]
```

Print the visible text of the current page, or of an http(s) URL fetched on this computer; --outline prints only the headings.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | http or https URL to fetch on this computer instead of reading the current page |

| Flag | Value | Description |
|---|---|---|
| `--outline` | none | print only the headings, one "#" per level |

### `reload`

```
agent-ios reload [flags]
```

Reload the page (Page.reload after withdrawing unused dialog answers), then wait for the load state.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--wait` | `load`, `domcontentloaded`, `none` | after the navigation, wait for this load state of the new document (none: return at once) (default `load`) |
| `--ignore-cache` | none | reload without the cache (Page.reload ignoreCache) |

### `removeinitscript`

```
agent-ios removeinitscript <id>
```

Remove an init script of this session by the id that addinitscript printed.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<id>` | yes | script id, for example init1 |

### `restart-iwdp`

```
agent-ios restart-iwdp
```

Restart the ios_webkit_debug_proxy that agent-ios started. Stops only an iwdp that agent-ios started and recorded, then starts a new one. Any other iwdp (for example one that the user or another program started) is never stopped: the command fails and names the owner pid and the manual restart command. When no iwdp runs, it starts one.

- Page: none; runs in the client process, not in the daemon

### `screenshot`

```
agent-ios screenshot [<path>] [flags]
```

Capture the viewport, the full page or one element as PNG; --annotate labels each ref [N] (@eN), --if-changed skips an identical capture.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | PNG file to write (default $TMPDIR/agent-ios-screenshot-\<time\>.png) |

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
agent-ios scroll <direction> [<px>] [flags]
```

Scroll the window or an element up, down, left or right (default 300 px).

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
agent-ios scrollintoview <target>
```

Scroll an element into the center of the viewport.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | element: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |

### `select`

```
agent-ios select <target> <values>... [flags]
```

Select options of a \<select\> by value, then by visible label; dispatches input and change.

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
agent-ios session
```

Print the current session name (--session or AGENT_IOS_SESSION; default "default").

- Page: none

### `session info`

```
agent-ios session info
```

Show this session's leased pages: page, lease state (active or lost), epoch and last use.

- Page: none

### `session list`

```
agent-ios session list
```

List the sessions that hold page leases in the daemon.

- Page: none

### `session release`

```
agent-ios session release
```

Release this session's page leases and run their cleanup (same as close).

- Page: none

### `set credentials`

```
agent-ios set credentials [<user>] [flags]
```

Send HTTP Basic credentials to the requests of one origin only; on a command line the password is the first line of stdin. Supported only where a test has shown that other origins and cross-origin redirects never get them (WebKitGTK today).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<user>` | no | user name (on a command line, the password is the first line of stdin) |

| Flag | Value | Description |
|---|---|---|
| `--origin` | text | the origin that gets the credentials, for example https://example.com |
| `--password` | text | the password; a command line refuses it, because argv is readable by other local users: pipe the password to stdin; never logged or printed |
| `--clear` | none | remove every credential of this session instead |

### `set device`

```
agent-ios set device [<args>...]
```

Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute.

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<args>...` | no | ignored |

### `set geo`

```
agent-ios set geo <latitude> <longitude> [flags]
```

Answer navigator.geolocation with this position (a page-side shim: WebKit has no geolocation override).

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
agent-ios set headers <json>
```

Send these extra HTTP headers with every request of the page; '{}' removes this session's headers.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<json>` | yes | a JSON object of header names and string values, for example '{"X-A":"1"}' |

### `set media`

```
agent-ios set media <scheme> [flags]
```

Emulate prefers-color-scheme (dark, light or no-preference), and optionally reduced motion and more contrast.

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
agent-ios set offline <state>
```

Fail every request of the page (on) or stop doing so (off), through request interception; navigator.onLine stays true.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<state>` | yes | on or off |

### `set useragent`

```
agent-ios set useragent [<ua>] [flags]
```

Override the user agent string of the page (--reset removes the override).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<ua>` | no | user agent string |

| Flag | Value | Description |
|---|---|---|
| `--reset` | none | remove the override |

### `set viewport`

```
agent-ios set viewport [<args>...]
```

Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute.

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<args>...` | no | ignored |

### `sim down`

```
agent-ios sim down [flags]
```

Stop the forward that sim up started and shut the simulator down; with --mac also run the configuration's stop command (for example the shutdown of a VM).

- Page: none; runs in the client process, not in the daemon
- Default deadline: 3m0s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |
| `--mac` | none | also stop the Mac with the configuration's stop command |

### `sim keyboard`

```
agent-ios sim keyboard [<mode>] [flags]
```

Show or set the simulator's keyboard: onscreen shows iOS's on-screen keyboard when sim tap taps a field, as on an iPhone, for keyboard bugs; sim type then sends only lowercase letters, digits and unshifted symbols, on a US layout. hardware (the Simulator's default) types the text that the simulator's keyboard layout has keys for (AXe refuses the rest) but hides the on-screen keyboard. A change restarts the Simulator app and the simulator, then runs sim up; it refuses while a page has an active lease, unless --restart-iwdp.

- Page: none; runs in the client process, not in the daemon
- Default deadline: 15m0s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<mode>` | no | onscreen or hardware; without it, print the mode |

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |
| `--restart-iwdp` | none | restart even when that ends active page leases |

### `sim release`

```
agent-ios sim release [flags]
```

End your turn on the simulator: close this session (release its page leases, as close does) and release the owner's reservation of the simulator. Close your own tab first (tab close; when Safari refuses, release anyway). Releasing no reservation is not an error; another owner's is refused.

- Page: none
- Default deadline: 30s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--owner` | text | reservation owner; default --reservation (AGENT_IOS_DEVICE_RESERVATION), else the session |

### `sim reserve`

```
agent-ios sim reserve [flags]
```

Reserve the simulator for an owner, waiting up to --wait while another owner holds it; renews the owner's own reservation. While an owner holds it, other owners' page commands on the simulator fail (exit 6), its tabs are hidden from them, and their sim commands that act on it fail. Exit 3 when the wait ends first: the simulator is still another owner's; run the same command again (do not use the simulator meanwhile). The reservation ends with sim release, or --ttl (2m) after the owner's last use (sim commands renew it, and an open page lease keeps renewing it). The owner must be your own name: the default session and qa-login, the name of the guides' examples, are refused.

- Page: none; runs in the client process, not in the daemon
- Default deadline: 11m0s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--owner` | text | reservation owner; default --reservation (AGENT_IOS_DEVICE_RESERVATION), else the session |
| `--ttl` | duration (40s, 1m30s) or seconds (40) | lifetime after the owner's last use, at least 30s (a running sim command renews it, and an open page lease keeps renewing it) (default `2m`) |
| `--wait` | duration (40s, 1m30s) or seconds (40) | how long to wait for another owner's reservation to end, at most 10m (keep it under your tool's timeout) (default `50s`) |

### `sim screenshot`

```
agent-ios sim screenshot [<path>] [flags]
```

Capture the simulator's whole screen as PNG (simctl io screenshot): the status bar, Safari's bars, the on-screen keyboard and other apps, which the page capture of screenshot leaves out. The simulator must be booted (sim up); it starts nothing.

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | PNG file to write (default $TMPDIR/agent-ios-screenshot-\<time\>.png) |

| Flag | Value | Description |
|---|---|---|
| `-o`, `--output` | text | PNG file to write (same as path) |
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim status`

```
agent-ios sim status [flags]
```

Check each part that sim up sets up (the Mac over ssh, the desktop login, the simulator, its window, the forward, iwdp's listing) without changing any. Exits 0 also when a part is down; --json has "ready".

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim swipe`

```
agent-ios sim swipe <x1> <y1> <x2> <y2> [flags]
```

Swipe on the simulator's screen from \<x1\> \<y1\> to \<x2\> \<y2\> in points (one second; --duration changes it). A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone).

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<x1>` | yes | start x in points |
| `<y1>` | yes | start y in points |
| `<x2>` | yes | end x in points |
| `<y2>` | yes | end y in points |

| Flag | Value | Description |
|---|---|---|
| `--duration` | text | seconds the swipe takes |
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim tap`

```
agent-ios sim tap [<x>] [<y>] [flags]
```

Tap the simulator's screen at \<x\> \<y\>, or the element with --label or --id (sim ui lists them). A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone).

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<x>` | no | x in points |
| `<y>` | no | y in points |

| Flag | Value | Description |
|---|---|---|
| `--label` | text | tap the element with this accessibility label |
| `--id` | text | tap the element with this accessibility identifier |
| `--hold` | text | seconds the finger stays down: a long press |
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim type`

```
agent-ios sim type <text> [flags]
```

Type \<text\> into the simulator's focused field as key presses (sim tap the field first). The text is on the command lines of this host and the Mac: never type a secret this way; fill --stdin types into a page without that. A real touch through AXe (the configuration's "axe"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone).

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<text>` | yes | the text to type |

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim ui`

```
agent-ios sim ui [flags]
```

List the elements of the simulator's screen that have a label, value or identifier, with their frames in points (its accessibility tree, through AXe): the targets of sim tap. It shows page and app text.

- Page: none; runs in the client process, not in the daemon
- Default deadline: 1m0s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |

### `sim up`

```
agent-ios sim up [flags]
```

Make the iOS Simulator of the Mac in the simulator configuration ready for agent-ios, doing only what is still needed: start the Mac with the configuration's start command when ssh gets no answer, wait for the desktop login, boot the simulator, open its window, start Safari, forward 127.0.0.1:27753 to its Web Inspector over ssh, and start ios_webkit_debug_proxy, or restart the one that agent-ios started when it does not use that forward. Refuses that restart while a page has an active lease, unless --restart-iwdp, and refuses an iwdp that agent-ios did not start. Exit 7 when Safari shows no tab of --url in time (check your server); it then closes the new tab that its open made, or names it for tab close. Default deadline 15m.

- Page: none; runs in the client process, not in the daemon
- Default deadline: 15m0s (`--timeout` overrides it)

| Flag | Value | Description |
|---|---|---|
| `--device` | text | the simulator: its UDID or exact name (default: "device" of the configuration, else the one booted simulator) |
| `--no-window` | none | do not open the Simulator window |
| `--url` | text | open this http or https URL in the simulator's Safari (a URL of this host: the Mac's address for it, for example http://10.0.2.2:8080/ in a QEMU VM) |
| `--restart-iwdp` | none | restart iwdp even when that ends active page leases |

### `skills get`

```
agent-ios skills get [<name>...] [flags]
```

Print agent skills built into this binary (each guide without its front matter; --json gives the whole SKILL.md); without a name, or with --all, every skill.

- Page: none; runs in the client process, not in the daemon

| Argument | Required | Description |
|---|---|---|
| `<name>...` | no | skill name from 'skills list'; several names print several skills |

| Flag | Value | Description |
|---|---|---|
| `--all` | none | print every skill |

### `skills list`

```
agent-ios skills list
```

List the agent skills built into this binary (name and description); 'skills get \<name\>' prints one.

- Aliases: `skills`
- Page: none; runs in the client process, not in the daemon

### `skills path`

```
agent-ios skills path [<name>]
```

Write the built-in skills to the user cache directory (once per skills version, mode 0600) and print the directory of one skill or of all.

- Page: none; runs in the client process, not in the daemon

| Argument | Required | Description |
|---|---|---|
| `<name>` | no | skill name; without it, the directory that holds every skill |

### `snapshot`

```
agent-ios snapshot [flags]
```

Print the page's accessibility tree with @eN refs for later commands.

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `-i`, `--interactive` | none | show only elements with refs |
| `-c`, `--compact` | none | keep lines with refs, values or text and their ancestors |
| `-d`, `--depth` | integer | deepest level shown (0 = top level only) |
| `-s`, `--selector` | text | scope the snapshot to every element matching a CSS selector or locator, or to a @ref |
| `-u`, `--urls` | none | add url= to links |

### `state load`

```
agent-ios state load <path>
```

Restore a state file written by state save: the page must be at the file's origin; its cookies and storage are replaced by the file's.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | yes | the state file to read |

### `state save`

```
agent-ios state save <path>
```

Save the cookies, localStorage and sessionStorage of the page's current origin to a JSON file (mode 0600; it holds live credentials).

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | yes | the state file to write |

### `status`

```
agent-ios status
```

Check whether ios_webkit_debug_proxy answers on its listing address, count the devices that it lists, and name the log of an iwdp that agent-ios starts. It starts nothing. Exits 1 when iwdp does not answer or its device list cannot be read.

- Page: none; runs in the client process, not in the daemon

### `storage`

```
agent-ios storage <area> [<action>] [<key>] [<value>] [flags]
```

Read, set or clear localStorage or sessionStorage of the page: storage local [key], storage local set \<key\> \<value\>, storage local clear (same for session).

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
agent-ios swipe <direction> [<px>] [flags]
```

Swipe with synthetic touch events; scrolls as a fallback when the page cancels none (swipe up scrolls down).

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
agent-ios tab [<id>]
```

Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs.

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<id>` | no | tab id (t1, t2, ...) or label from tab list; none: list the tabs |

### `tab close`

```
agent-ios tab close [<id>]
```

Close a tab (default: the current one) with window.close(); works only for tabs that a script opened. In your turn on the simulator, a session that selected no tab must name the tab (exit 5).

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<id>` | no | tab id (tN) or label; default: the current tab |

### `tab find`

```
agent-ios tab find [<url>] [flags]
```

Print the id of the tab whose page shows exactly this URL (query and fragment included), compared inside the page. It only reads: it navigates and selects nothing. The session keeps the lease of the tab that the command prints, as after any read of that tab, and no lease that only the command used on another tab. Exit 7 when no tab shows the URL; exit 1 when several do or a tab could not be read; exit 6 when the listing hides pages of a device that the caller has not reserved (--device limits the search, and that count, to one device).

- Page: none

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | the URL to find; or --url-file |

| Flag | Value | Description |
|---|---|---|
| `--url-file` | file path | read the URL from this file (one line), so the URL is not on the command line |
| `--device` | text | only the pages of this device: SIMULATOR (the iOS Simulator) or a UDID; other devices' pages are neither listed nor counted as hidden |

### `tab list`

```
agent-ios tab list [flags]
```

List the pages of the device as tabs t1, t2, ... with labels; \* marks this session's current tab.

- Page: none

| Flag | Value | Description |
|---|---|---|
| `--device` | text | only the pages of this device: SIMULATOR (the iOS Simulator) or a UDID; other devices' pages are neither listed nor counted as hidden |

### `tab new`

```
agent-ios tab new [<url>] [flags]
```

Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab; the session's leases on other pages end.

- Aliases: `window new`
- Page: none

| Argument | Required | Description |
|---|---|---|
| `<url>` | no | URL to open (default about:blank) |

| Flag | Value | Description |
|---|---|---|
| `--label` | text | label for the new tab, usable instead of its tN id |

### `tap`

```
agent-ios tap <target> [<y>] [flags]
```

Tap an element, or the element at viewport point \<x\> \<y\>: touchstart and touchend, then the click sequence with pointerType touch.

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
agent-ios trace start [flags]
```

Start a WebKit Timeline recording of the page for a Chrome trace file (trace stop writes it).

- Page: leases the session's page

| Flag | Value | Description |
|---|---|---|
| `--categories` | text | comma-separated: timeline (Timeline records, always on) and script (JavaScript events of the ScriptProfiler) (default `timeline,script`) |

### `trace stop`

```
agent-ios trace stop [<path>]
```

Stop the Timeline recording and write it as Chrome trace JSON (mode 0600) to path, or to a new file in the temp directory.

- Page: leases the session's page

| Argument | Required | Description |
|---|---|---|
| `<path>` | no | the trace file to write (default: a new agent-ios-trace-\*.json in the temp directory) |

### `type`

```
agent-ios type [<target>] <text> [flags]
```

Type text key by key (keydown, keypress, beforeinput, insertion, input, keyup) into an element, or with only \<text\> into the focused element.

- Page: leases the session's page
- Default deadline: 30s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | no | input, textarea or contenteditable: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator; omit it to type into the focused element |
| `<text>` | yes | the text; a newline is the Enter key |

| Flag | Value | Description |
|---|---|---|
| `--delay` | duration with a unit (50ms, 1s) | pause between characters; each character is then one step |
| `--force` | none | skip the actionability checks (attached, visible, stable, enabled, receives events) |

### `uncheck`

```
agent-ios uncheck <target> [flags]
```

Uncheck a checkbox (a hidden input through its label) and read the state back.

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
agent-ios upload <target> <files>...
```

Set the files of a file input (read on the host, at most 8 MiB in total) and dispatch input and change.

- Page: leases the session's page
- Default deadline: 5s (`--timeout` overrides it)

| Argument | Required | Description |
|---|---|---|
| `<target>` | yes | file input: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator |
| `<files>...` | yes | file paths, relative to the current directory |

### `vitals`

```
agent-ios vitals
```

Show the page's Web Vitals: LCP, CLS, TTFB, FCP, INP, DOMContentLoaded and load (unavailable ones say why).

- Page: leases the session's page

### `wait`

```
agent-ios wait [<target>] [flags]
```

Wait for milliseconds, an element state, text, a URL glob, a load state or a JavaScript condition (default timeout 25 s; exit 3 on timeout).

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
