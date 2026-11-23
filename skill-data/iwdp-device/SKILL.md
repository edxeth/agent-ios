---
name: iwdp-device
description: Device setup and faults of iwdp-cli with a USB-connected iPhone or iPad, and the iOS Simulator on a Mac over SSH (sim up). A device that is not listed, a page list that does not answer, a hidden tab, and the rules for a restart or stop of ios_webkit_debug_proxy on a shared device. Load it from the iwdp guide when a device or its pages do not answer.
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

An iOS Simulator on a Mac that this host reaches over SSH (a macOS VM or a real Mac) works like a device once `iwdp-cli sim up` connected it; docs/simulator.md has its configuration (`~/.config/iwdp-cli/simulator.json`). `sim up` does only what is still needed: it starts the Mac with the configured command, waits for the desktop login, boots the simulator, opens its window, starts Safari, forwards its Web Inspector to 127.0.0.1:27753 and restarts the iwdp that iwdp-cli started when that iwdp does not list the simulator. It refuses that restart while another device's page has an active lease. It takes up to 15 minutes when it starts a VM. `iwdp-cli sim status` changes nothing.

<!-- doc-test: skip simulator: needs a Mac over SSH and its configuration -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli sim up --url "http://10.0.2.2:8080/" || exit "$?"   # --url: optional; the Mac's address of this host
iwdp-cli tab list
```

Safari's start page is not inspectable: when `sim up` reports `tabs 0`, run it with `--url`. Run `sim down` only when the task says so: the user may watch the simulator.

## Faults

- **`status` prints `devices 0` although the device is plugged in.** Run `iwdp-cli tab list` once, if your task allows a restart of iwdp. On Linux, iwdp-cli restarts the iwdp that it started itself when that iwdp lost its usbmuxd connection (after an unplug) or its connection to a device that usbmuxd still reports; a second restart comes at most once per 30 s. When the device stays unlisted, ask the user to unlock it and keep it unlocked, tap Trust and check Web Inspector, then list again; when it is unlocked and still unlisted, ask the user to unplug it and plug it in again. A refused restart means that the iwdp belongs to another program: report it.
- **`iwdp-cli status` exits 1.** iwdp is down or stuck. Report it with the failed check; do not restart it yourself.
- **Exit 5 while the daemon holds a page ("iwdp did not answer the page list").** iwdp 1.9.2 can get one broken file-descriptor slot; then it closes every page-list request without an answer while a page connection is open. Commands on a page that the daemon already holds still work: keep working in your session without `--page`. `tab list`, `tab new`, `tab close`, `tab find`, a `--page` for a page that is not attached, another session's first command and CDP clients fail. Only a restart of iwdp clears the fault; that belongs to the device owner. Report it, and mark the checks that need another tab as not run.
- **The device is listed, the tab listing shows no tab and `hidden 0`.** Ask the user to open the page in Safari. With `hidden` above 0 the pages exist, but your owner has no reservation of their device: reserve it, or ask the device owner.
- **Exit 4 on the first command of a tab that was fine before.** The tab is probably not the visible Safari tab: hidden tabs accept commands and never answer. Ask the user to bring it to the front. Or another inspector (another agent, Safari Web Inspector on a Mac) took the page: stop and ask. Only the device owner can approve `--takeover`.
- **Exit 4 on the first attach.** A first attach can fail once: run the command again. When it fails the same way again, treat it as the item above.
- **Exit 6 from `device reserve`.** Another owner holds the reservation. In private mode the line names no device; ask the device owner.
