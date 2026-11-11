# Device runbook

How to work on a physical iPhone that other people or agents may also use. Read [sessions.md](sessions.md) first.

## Before you start

- On the device: unlocked, trusted with this host, Settings > Safari > Advanced > Web Inspector on, and the page you work on open as the **visible** Safari tab. Safari answers only for the visible tab, and WIP cannot bring a tab to the front.
- Check that iwdp runs: `iwdp-cli status`. When no iwdp runs, the daemon starts one (pinned to the first device on port 9224). When an iwdp runs, the daemon uses it and never starts a second one.
- iwdp 1.9.2 or newer is needed for messages of 30 KB or more (the injected runtime is about 51 KB on the wire).

## Reserve and release

1. Reserve the device before the first page command. Use the same owner for the whole run:

   ```bash
   OWNER=agent-a                                           # your owner name
   iwdp-cli device reserve --owner "$OWNER" --ttl 2h      # all devices; add --device UDID for one
   export IWDP_DEVICE_RESERVATION="$OWNER"
   export IWDP_SESSION="$OWNER-1"
   ```

   The command fails when another owner holds a live reservation. Do not break it; wait or ask the owner. An orchestrator reserves once and passes `IWDP_DEVICE_RESERVATION` to its workers; each worker uses its own `IWDP_SESSION`.
2. Work. Without a reservation, every page command on the device fails with exit 6.
3. End: `iwdp-cli close` for each session, then `iwdp-cli device release --owner "$OWNER"`. `device list` shows the reservations that remain.

## Never stop another agent's iwdp

One iwdp serves every client on the host. Stopping or restarting it ends every other user's and agent's device sessions.

- Never run `pkill`, `killall` or `kill` on `ios_webkit_debug_proxy`, and never run any other script or command that stops or restarts it. Only three tools may stop an iwdp, and each proves the identity of that iwdp before it signals it: `iwdp-cli restart-iwdp`, only for an iwdp that iwdp-cli started and recorded; the Simulator teardown (`make sim-teardown` or `scripts/sim-setup.sh --teardown`), only for the iwdp that setup started, under the run lease; and `scripts/iwdp-owner-check.sh --terminate`, only for a stop by hand of an iwdp that you started yourself and nobody else uses, as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes.
- Do not start a second iwdp on the same device (`direct` transport included): two inspectors on one device take pages from each other.

### iwdp-cli restart-iwdp

`iwdp-cli restart-iwdp` restarts iwdp **only when iwdp-cli (or its daemon) started and recorded it**:

- When iwdp-cli or its daemon starts iwdp, it records the process (pid, process group, listing address, start time and a random token in the process environment) in a private file under `$XDG_RUNTIME_DIR` (else the temporary directory), marked as written by iwdp-cli. A record without that mark (written by the removed iwdp-mcp server, or by an older iwdp-cli) counts as another program's iwdp, so the restart refuses it; whoever started that iwdp, and only when nobody else uses it, stops it once, as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes ([migration.md](migration.md)).
- A restart stops the recorded process group only when the record is valid, the running process still has the recorded start time and token, and that process group owns the listening socket of port 9221. It sends SIGTERM, then SIGKILL after 2 s, checks the identity again before SIGKILL, and starts a new iwdp.
- In every other case it **refuses** and signals nothing: the iwdp belongs to the user, another agent or another program. The refusal names the address, the owner pid when known, the manual start command and this runbook. Do not stop that iwdp yourself unless you started it and nobody else uses it, and then only as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes; otherwise ask whoever started it, or the user.
- When no iwdp runs, it starts one.
- On macOS the identity check reads the process start time (`ps`) and the token from the kernel's process arguments. When either cannot be read, the restart refuses.

`make sim-setup` and `make sim-teardown` follow the same rule for the simulator: teardown stops only the iwdp that setup started, and it proves that process with `scripts/iwdp-owner-check.sh`. Setup starts iwdp with a random start token in its environment and records its pid, start token and start time (`ps -o lstart=`) in a private file. Teardown sends SIGTERM to that pid only when the check reports `verified`, then waits up to 5 s until the check reports `gone`. When the check reports `gone`, teardown removes the record with `scripts/iwdp-owner-check.sh --forget`. That reads the record again and removes it only when the process that this read names is gone and the path still names the same file with the same content: a record that names a live process stays, and so does one that changed after that read. A record that another program wrote over the path between the check and that read is removed when the process that it names is gone, too. In every other case (no match, a check that cannot be made, an iwdp that still runs after SIGTERM, or a record that `--forget` keeps) it says so with the reason and keeps the record in its place for the next teardown, which checks it again; the next setup refuses meanwhile. One case differs: when another file took the record's path while `--forget` compared it, the changed record stays under a private name in the same directory (`.iwdp.pid.forget-<hex>`), which the warning names on one line, escaped: every byte outside printable ASCII, the apostrophe and the backslash as `\0ooo` (a backslash, a zero and three octal digits), so that the name holds no apostrophe and is safe inside single quotes; the command `printf '%b' '<name>'`, with the name pasted between the quotes, gives the name back in bash, dash and zsh. The next teardown does not check that file, and the next setup does not refuse because of it: check it by hand as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes (step 3 says how to name it). Never stop that iwdp with a command of your own; if it stays running, report it. Teardown shuts down the Simulator only when setup's own boot started it (its UDID is recorded); a Simulator that someone else booted stays booted. One Simulator run per user account at a time: setup takes a lease (its run token) under a lock, and refuses while another run holds it; wait until that run has finished. The automatic teardowns (`make test-e2e`, CI) pass their run token and act only on their own lease, under the same lock. `make sim-teardown` is the manual recovery after a crashed run only: it tears down whatever run holds the lease.

### Stopping an iwdp by hand

`iwdp-cli restart-iwdp` and the Simulator teardown prove the identity of an iwdp before they signal it; use them. Stop an iwdp by hand only when neither applies (for example an iwdp that a version before the rename to iwdp-cli started, [migration.md](migration.md#rename-to-iwdp-cli)), and only in this order. Identity is what makes a stop safe; who listens on the port is not checked here (that decides only whether `restart-iwdp` may restart an iwdp).

1. **You started this iwdp yourself, and nobody else uses it:** no other agent, session or tool works through it. If another agent or the user started it, or you do not know, send no signal: ask whoever started it, or the user.
2. **Check and stop in one step:** run `scripts/iwdp-owner-check.sh --terminate <record>` from a checkout of this repository. The record of an iwdp that iwdp-cli started is `iwdp-cli-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-cli/` (else `$TMPDIR/iwdp-cli-<uid>/`); its token variable is `IWDP_CLI_START_TOKEN`. The record of one that a version before the rename started is `iwdp-mcp-started-<uid>-<address>.json` in `$XDG_RUNTIME_DIR/iwdp-mcp/` (else `$TMPDIR/iwdp-mcp-<uid>/`); its token variable is `IWDP_MCP_START_TOKEN`. Keep the file name: it decides the kind, the format and the variable, and a record with another name, or whose content does not match its name, cannot be checked. The check reads the record once and the token from it: never read, copy or type the token, and never send a signal yourself. It reads the environment of the process with exact boundaries between arguments and environment (Linux `/proc/<pid>/environ`, macOS the `KERN_PROCARGS2` buffer); when it cannot, the result is `cannot-check`. It checks the record before the process: a regular file of yours that group and others cannot write, with a valid pid, a start time and a start token. Any other record is `cannot-check`, also when no process has its pid, so it is never `gone` and never removed. It prints one result with a fixed reason, never a token or the command line of a process:
   - `verified` (exit 0): the process with the recorded pid has the recorded start time, carries the recorded start token in its environment and runs ios_webkit_debug_proxy, and the check sent SIGTERM to that process (Linux: through a pidfd that it held during the checks).
   - `mismatch` (exit 1) or `cannot-check` (exit 2): nothing was signaled. Leave the process running, and ask whoever started it, or the user.
   - `gone` (exit 3): the record is valid, and no process has the recorded pid; there is nothing to stop. Remove the record as step 3 says.
3. **Run `scripts/iwdp-owner-check.sh <record>` (without `--terminate`; it signals nothing) until it reports `gone`** (iwdp exits within a few seconds). Then remove the record with `scripts/iwdp-owner-check.sh --forget <record>`, never by hand: it reads the record again, signals nothing, and removes it only when its process is gone and the path still names exactly the record that it read (`gone`, exit 3). A record that changed after that read stays (`cannot-check`; when another file took its path, the reason names the private file where the changed record stays, escaped as above, and nothing checks that file again by itself); for that or any other result, ask. To check such a private file, move it under its original name (without the leading `.` and the `.forget-<hex>` suffix) into a new directory that only you can write, and follow these steps for it. The next start records the new iwdp. When it still reports `verified` after 10 s, that iwdp ignores SIGTERM: keep the record, send nothing more, and report it. Any other result: ask.

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

1. Daemon: `iwdp-cli device reserve --owner "$OWNER"`, `IWDP_DEVICE_RESERVATION="$OWNER" iwdp-cli eval 1+1`; the busy hint with iwdp's frontend on; the reconnect gap after a close; that a second inspector moves the lease to lost; the log line of pause-on-start (on or off).
2. Navigation: `open` across two sites, `wait --url` across the swap, `dialog accept` before an inline `confirm` of a new document, `reload`, `back` and `forward` across sites, `tab list` and a hidden tab, `tab new` (popup blocked or not), `screenshot --annotate` and `--if-changed`, `frame` on a cross-origin iframe, `hover --css` (expect exit 2 through iwdp), `tap` and `swipe` touch events, `click --new-tab`.
3. Actions: `click` on a covered element (the error names the cover, exit 124 after about 4.75 s); a mousedown handler with `setTimeout 0` state; `fill --stdin` with a password from a pipe (`ps` shows no value).
4. DevTools: a route and a netlog (`Network.requestIntercepted` arrives; a fulfilled fetch), credentials scoped to one origin (then enable them for the device), storage, cookies, `state save` and `state load`, `vitals`, `trace` and `profiler` files, `diff screenshot` with 0 differing pixels, `set media dark`, `emulate network slow-3g` (accepted on iOS 16.4 to 17.x and on 27.3 or later; on iOS 16.0 to 16.3 and 18.0 to 27.2 it fails with "Network.setEmulatedConditions is not in the iOS <version> command list").
5. Direct transport (`--transport direct`, only while iwdp does not serve the device): `tab list`, `eval 1+1`, `snapshot -i`, `click`; repeat on one iOS 17 or later device before `auto` may choose it.
6. CDP: the client scenarios through `get cdp-url`, before any is advertised as working on iOS.
