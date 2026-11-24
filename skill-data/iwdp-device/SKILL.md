---
name: iwdp-device
description: Device setup and faults of iwdp-cli with a USB-connected iPhone or iPad, and the iOS Simulator (sim up). A device that is not listed, a page list that does not answer, a hidden tab, and the rules for a restart or stop of ios_webkit_debug_proxy on a shared device. Load it from the iwdp guide when a device or its pages do not answer.
---

# iwdp-cli: the device and its faults

The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide. Start every call with the line that the examples start with.

The iPhone needs Web Inspector on (Settings, Safari, Advanced, Web Inspector), at least one open Safari tab, and the device unlocked and trusted. A passcode or Trust prompt needs the user; it does not mean iwdp is down. ios_webkit_debug_proxy (iwdp) serves the device: port 9221 lists devices, and the first device's pages are on port 9224.

## Checks that start nothing

`iwdp-cli status` reads the listing of iwdp and starts nothing: no daemon, no iwdp, no recovery of one. It prints whether iwdp answers and how many devices it lists. Run it before the first command that needs the device when you may not have an iwdp restarted: a command that lists tabs can make the daemon restart an iwdp that iwdp-cli started and that lost its device.

<!-- doc-test: skip device: needs ios_webkit_debug_proxy and a USB-connected iPhone -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli status
```

```text
iwdp answers: true
devices 1
```

The health probe of safety rule 2 asks the daemon for the pages of your session:

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli session info                    # per page of your session: lease, epoch, connection
```

<!-- doc-test: expect -->
```text
pages 0
```

## The shared device

The device and its iwdp serve every agent of the computer.

- Never stop, restart or kill an iwdp that another agent may use (`pkill`, or a script that stops or restarts it). The daemon never stops a running iwdp.
- `iwdp-cli restart-iwdp` stops only an iwdp that iwdp-cli started and recorded, and refuses any other. Run it only when your task or the device owner allows a restart; it ends the page connections of every session.
- Stop an iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>` from a checkout of the iwdp-cli repository, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as docs/device-runbook.md (Stopping an iwdp by hand) in that repository describes.
- Never run `iwdp-cli daemon stop` on a shared computer: it ends every session's page connections, also other agents'.
- `devices`, `device list`, `session list`, `cdp status` and `daemon status` print one status line in private mode. Without it they print device identifiers and the sessions of other agents: keep that output out of a shared transcript.

## The simulator

An iOS Simulator on a Mac or VM over SSH (docs/simulator.md) is shared: several agents, each on its own repository, use it one at a time, because Safari answers only for its visible tab and the sim commands act on one screen.

1. **Serve your page first.** Before you reserve, serve your repository's page on a random free port of this host (not a common one such as 8000, 8080 or 8765: other agents and programs use them) in a process that outlives your tool call (a background job; a server started inside a tool call can end with it), and check that `http://127.0.0.1:<port>/` answers with your page. If you cannot serve it, report that without reserving.
2. **Take your turn.** `iwdp-cli sim reserve` reserves the simulator for your owner (`--reservation`, `IWDP_DEVICE_RESERVATION`, else your session) and waits up to `--wait` (50 s; at most 10m, keep it under your tool's timeout) while another agent holds it. Exit 3, or your tool's timeout, means it is still theirs: run the same command again until it succeeds, and meanwhile run no other `iwdp-cli` command on the simulator. Waiting for another agent's turn is the normal case, not a failure: do not stop, skip the check or ask the user because the simulator is reserved or its Mac is starting. While another owner holds it, its tabs are hidden from you and your page and sim commands fail with exit 6. The agent that drives the simulator reserves it; never hold it while you wait for a delegate that drives it (or pass your owner to the delegate in `IWDP_DEVICE_RESERVATION`).
3. **Open your own tab.** `iwdp-cli sim up --url http://10.0.2.2:<port>/<path>`: in a QEMU VM, 10.0.2.2 is this host as the simulator sees it. It starts the configured Mac or VM if needed (up to 15 minutes), boots the simulator, connects it, and opens your URL in a new tab, which becomes the visible one. It returns once Safari lists the new tab of that URL (exit 7 when none comes: check your server) and prints that tab's id (`new tab t3`, `new tab:    t3` without private mode): select it with `tab <id>`. That id is the tab sim up opened, also when an earlier tab shows the same URL. When sim up says the new tab's id is not known and another tab shows the same URL, `tab find` cannot tell yours: serve your page on a URL that no other tab shows (a port or path of your own) and run `sim up --url` with it. When sim up says only that the new tab's id is not known, find it with `tab find --device SIMULATOR --url-file <file>` instead; `--device SIMULATOR` searches the simulator alone, so a plugged-in iPhone that you have not reserved does not stop it. When `tab find` fails, stop: never run a page command without your tab selected, since it may land on another agent's tab. Tabs that you did not open are earlier agents' work: never select, navigate, close or `lease break` them.
4. **End your turn.** `tab close` your own tab, then `iwdp-cli sim release`, which also closes your session as `close` does, also after a failure. `tab close` can fail (Safari may refuse to close a tab, for example one that was navigated many times): then leave that tab and still run `sim release`. A reservation that you stop using ends 2 minutes after your last command (`--ttl`, at least 30 s); your open tab keeps it while you work on it, and a running sim command keeps it for as long as it runs. A command that loses the turn (released, expired, or another agent's on a free simulator) stops with exit 6. `sim reserve` needs `IWDP_SESSION` (or `--owner`) set to a name of your own; `device reserve` and `device release` refuse the simulator.

<!-- doc-test: skip simulator: needs a Mac over SSH and its configuration -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli sim reserve || exit "$?"                             # exit 3: another agent's turn; run it again
iwdp-cli sim up --url "http://10.0.2.2:41873/" || exit "$?"  # your server's port; the Mac's address of this host; prints "new tab t<N>"
iwdp-cli tab "$TAB" || exit "$?"   # TAB: the t<N> that sim up printed; when it printed none, tab find --device SIMULATOR --url-file "$TARGET_FILE" prints it
```

`sim up` refuses to restart iwdp while a page has an active lease (`--restart-iwdp` overrides, only for your own leases) and refuses an iwdp that iwdp-cli did not start: report either refusal. Safari's start page is not inspectable: on `tabs 0`, run `sim up --url <url>`.

When the page is not enough, act on the simulator's whole screen: `sim screenshot` captures it with the keyboard and Safari's bars; `sim tap <x> <y>` (screen points; `sim ui` lists elements and their centers), `sim swipe` and `sim type` are real touches, so a tapped field opens the keyboard, unlike `click`. For keyboard bugs run `sim keyboard onscreen` first (it restarts the simulator; `sim type` then refuses Shift characters), and `sim keyboard hardware` after. Never `sim type` a secret. Run `sim down` only when the task says so: the user may watch the simulator.

## Faults

- **`status` prints `devices 0` although the device is plugged in.** Run `iwdp-cli tab list` once, if your task allows a restart of iwdp. On Linux, iwdp-cli restarts the iwdp that it started itself when that iwdp lost its usbmuxd connection (after an unplug) or its connection to a device that usbmuxd still reports; a second restart comes at most once per 30 s. When the device stays unlisted, ask the user to unlock it and keep it unlocked, tap Trust and check Web Inspector, then list again; when it is unlocked and still unlisted, ask the user to unplug it and plug it in again. A refused restart means that the iwdp belongs to another program: report it.
- **`iwdp-cli status` exits 1.** iwdp is down or stuck. Report it with the failed check; do not restart it yourself.
- **Exit 5 while the daemon holds a page ("iwdp did not answer the page list").** iwdp 1.9.2 can get one broken file-descriptor slot; then it closes every page-list request without an answer while a page connection is open. Commands on a page that the daemon already holds still work: keep working in your session without `--page`. `tab list`, `tab new`, `tab close`, `tab find`, a `--page` for a page that is not attached, another session's first command and CDP clients fail. Only a restart of iwdp clears the fault; that belongs to the device owner. Report it, and mark the checks that need another tab as not run.
- **The device is listed, the tab listing shows no tab and `hidden 0`.** Ask the user to open the page in Safari. With `hidden` above 0 the pages exist, but your owner has no reservation of their device: reserve it, or ask the device owner.
- **Exit 4 on the first command of a tab that was fine before.** The tab is probably not the visible Safari tab: hidden tabs accept commands and never answer. Ask the user to bring it to the front. Or another inspector (another agent, Safari Web Inspector on a Mac) took the page: stop and ask. Only the device owner can approve `--takeover`.
- **Exit 4 on the first attach.** A first attach can fail once: run the command again. When it fails the same way again, treat it as the item above.
- **Exit 6 from `device reserve`.** Another owner holds the reservation. In private mode the line names no device; ask the device owner.
