# Device runbook

How to work on a physical iPhone that other people or agents may also use. Read [sessions.md](sessions.md) first.

## Before you start

- On the device: unlocked, trusted with this host, Settings > Safari > Advanced > Web Inspector on, and the page you work on open as the **visible** Safari tab. Safari answers only for the visible tab, and WIP cannot bring a tab to the front.
- Check that iwdp runs: `iwdp-cli status` (MCP: `iwdp_status`). When no iwdp runs, iwdp-mcp starts one (pinned to the first device on port 9224). When an iwdp runs, iwdp-mcp uses it and never starts a second one.
- iwdp 1.9.2 or newer is needed for messages of 30 KB or more (the injected runtime is about 51 KB on the wire).

## Reserve and release

1. Reserve the device before the first page command. Use the same owner for the whole run:

   ```bash
   iwdp-cli device reserve --owner <owner> --ttl 2h      # all devices; --device <udid> for one
   export IWDP_DEVICE_RESERVATION=<owner>
   export IWDP_SESSION=<owner>-1
   ```

   The command fails when another owner holds a live reservation. Do not break it; wait or ask the owner. An orchestrator reserves once and passes `IWDP_DEVICE_RESERVATION` to its workers; each worker uses its own `IWDP_SESSION`.
2. Work. Without a reservation, every page command on the device fails with exit 6.
3. End: `iwdp-cli close` for each session, then `iwdp-cli device release --owner <owner>`. `device list` shows the reservations that remain.

## Never stop another agent's iwdp

One iwdp serves every client on the host. Stopping or restarting it ends every other user's and agent's device sessions.

- Never run `pkill`, `killall` or `kill` on `ios_webkit_debug_proxy`, and never run `iwdpctl`, unless you started that process yourself and nobody else uses it.
- Do not start a second iwdp on the same device (`direct` transport included): two inspectors on one device take pages from each other.

### restart_iwdp and iwdp-cli restart-iwdp

`restart_iwdp` (MCP) and `iwdp-cli restart-iwdp` restart iwdp **only when iwdp-mcp started it**:

- When iwdp-mcp starts iwdp, it records the process (pid, process group, listing address, start time and a random token in the process environment) in a private file under `$XDG_RUNTIME_DIR` (else the temporary directory).
- A restart stops the recorded process group only when the record is valid, the running process still has the recorded start time and token, and that process group owns the listening socket of port 9221. It sends SIGTERM, then SIGKILL after 2 s, checks the identity again before SIGKILL, and starts a new iwdp.
- In every other case it **refuses** and signals nothing: the iwdp belongs to the user, another agent or another program. The refusal names the address, the owner pid when known, and the manual restart command. Do not run that command yourself; ask the owner.
- When no iwdp runs, it starts one.
- A refused restart keeps the MCP server's page connection; a successful one closes it.
- On macOS the identity check reads the process start time (`ps`) and the token from the kernel's process arguments. When either cannot be read, the restart refuses.

`make sim-setup` and `make sim-teardown` follow the same rule for the simulator: teardown stops only the iwdp that setup started (pid, start time and token checked). On macOS teardown cannot read the environment exactly, so it always refuses and prints `kill <pid>` for you to run; until then the next setup refuses because port 9221 is in use.

## When a page is "lost"

A command fails with "connection to page was closed by someone else or by the device; ownership is unknown" (exit 4). Someone else (another agent, Safari Web Inspector, a script) took the page, or the device went away.

1. Stop. Do not retry in a loop: the daemon never reconnects a lost page by itself.
2. Find out who uses the device (the reservation list, the people or orchestrator who share it).
3. Only with the device owner's approval: `iwdp-cli lease takeover [page]` or the same command again with `--takeover`. The takeover needs your reservation, reattaches the page, re-applies your session's page state and gives a new lease epoch. Your old refs and dialog answers are gone: take a new snapshot and arm answers again.

Other errors that look similar:

- "stalled" or "a background reconnect is running": the page stopped answering; the daemon tries one reconnect (15 s at most). A command that arrives meanwhile waits within its deadline.
- "tab is not answering; it is probably not the visible Safari tab. Bring it to the front on the device.": bring the tab to the front.
- "page ... is leased by session ...": another session holds the page; see [sessions.md](sessions.md).

## Pause-on-start

When Safari moves a page to a new process (a cross-site navigation), the daemon gets the new page target before its first script runs. To install the agent runtime, the dialog override, init scripts and interception rules before the page's own scripts, the daemon asks WebKit to pause each new target (`Target.setPauseOnStart`), sends the page state, and resumes the target (after its setup, or after 2 s at most).

- The daemon turns pause-on-start on for every page connection, also after a reconnect.
- It is best effort. When WebKit refuses it or does not answer within 1 s, the connection stays usable, the daemon log gets one line, and pause-on-start is off for that connection. Then "runs before page scripts" is partial: a dialog that the first inline script of the new document opens can still block the page, and an init script can run after it.
- Not yet verified on a device: whether iwdp relays the reply of `Target.setPauseOnStart` on iOS 16.7, and whether iOS 26 holds that reply (iOS 26 can hold replies until the next message). Record both in the device check below.

## Device checks that have not run yet

These checks need the device owner's grant and a time when nobody else inspects the device. They are recorded here so that the next run follows one list. Record outputs and timings in the run report.

1. Daemon: `device reserve`, `IWDP_DEVICE_RESERVATION=<o> iwdp-cli eval 1+1`; the busy hint with iwdp's frontend on; the reconnect gap after a close; that a second inspector moves the lease to lost; the log line of pause-on-start (on or off).
2. Navigation: `open` across two sites, `wait --url` across the swap, `dialog accept` before an inline `confirm` of a new document, `reload`, `back` and `forward` across sites, `tab list` and a hidden tab, `tab new` (popup blocked or not), `screenshot --annotate` and `--if-changed`, `frame` on a cross-origin iframe, `hover --css` (expect exit 2 through iwdp), `tap` and `swipe` touch events, `click --new-tab`.
3. Actions: `click` on a covered element (the error names the cover, exit 124 after about 4.75 s); a mousedown handler with `setTimeout 0` state; `fill --stdin` with a password from a pipe (`ps` shows no value).
4. DevTools: a route and a netlog (`Network.requestIntercepted` arrives; a fulfilled fetch), credentials scoped to one origin (then enable them for the device), storage, cookies, `state save` and `state load`, `vitals`, `trace` and `profiler` files, `diff screenshot` with 0 differing pixels, `set media dark`, `emulate network slow-3g` (accepted on iOS 16.4 to 17.x and on 27.3 or later; on iOS 16.0 to 16.3 and 18.0 to 27.2 it fails with "Network.setEmulatedConditions is not in the iOS <version> command list").
5. Direct transport (`--transport direct`, only while iwdp does not serve the device): `devices`, `pages`, `eval 1+1`, `snapshot -i`, `click`; repeat on one iOS 17 or later device before `auto` may choose it.
6. CDP: the client scenarios through `get cdp-url`, before any is advertised as working on iOS.
