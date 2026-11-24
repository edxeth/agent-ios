---
name: iwdp-device
description: A physical iPhone or iPad and local WebKitGTK pages with iwdp-cli (the start and end of a run, the tab of the task), the details of a simulator turn, real touches and the keyboard, and device faults. A device that is not listed, a page list that does not answer, a hidden tab, and the rules for a restart or stop of ios_webkit_debug_proxy on a shared device. Load it from the iwdp guide.
---

# iwdp-cli: the device and its faults

The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide.

A physical iPhone needs Web Inspector on (Settings, Safari, Advanced, Web Inspector), at least one open Safari tab, and the device unlocked and trusted. A passcode or Trust prompt needs the user; it does not mean iwdp is down.

## Checks that start nothing

`iwdp-cli status` reads the listing of iwdp and starts nothing: no daemon, no iwdp, no recovery of one. Run it before the first command that needs the device when you may not have an iwdp restarted: a command that lists tabs can make the daemon restart an iwdp that iwdp-cli started and that lost its device.

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

## A physical device or WebKitGTK

Use a physical device only when the user names one, and reserve it: page commands on it fail with exit 6 until your owner has a reservation. Local WebKitGTK pages need no reservation.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli device reserve --owner "$IWDP_SESSION" --ttl 1h || exit "$?"
iwdp-cli tab list
```

<!-- doc-test: expect -->
```text
device reserve exit 0
hidden 0
```

`tab list` prints one line per tab, with `*` before the current tab of your session, then `hidden <n>`: the pages of devices that your owner has not reserved.

**Find the tab of the task before the first action.** `tab find` prints the id of the one tab whose URL equals the target exactly, query and fragment included. It only reads.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
tab="$(iwdp-cli tab find --url-file "$TARGET_FILE")" || exit "$?"   # TARGET_FILE: the file that holds the task's URL
iwdp-cli tab "$tab"                                                  # select it only after it was found
```

<!-- doc-test: expect -->
```text
tab exit 0
```

When `tab find` exits 7, no tab shows the target: act on no tab and report it. A tab that shows another page can hold the state of another task or of the user. Any other exit code is not a missing target: see the exit codes of the iwdp guide. Only when the task hands you the device's one tab to work in, open the target there: `iwdp-cli --only-tab open --url-file "$TARGET_FILE"`, which fails (exit 5) unless the listing has exactly one tab and no hidden page.

End the run, also after a failure:

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
closed=0
iwdp-cli close || closed=$?                # release the device also after a failed close
iwdp-cli device release --owner "$IWDP_SESSION" || exit "$?"
[ "$closed" = 0 ]
```

`close` ends your session: it releases your pages, removes your routes, headers, overrides and dialog answers, and revokes your CDP URLs. It does not close the Safari tab, does not touch other sessions, and does not undo forms you submitted or sign-ins you made.

## The shared device

- `iwdp-cli restart-iwdp` stops only an iwdp that iwdp-cli started and recorded, and refuses any other. Run it only when your task or the device owner allows a restart; it ends the page connections of every session.
- Stop an iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>` from a checkout of the iwdp-cli repository, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as docs/device-runbook.md (Stopping an iwdp by hand) in that repository describes.
- Never run `iwdp-cli daemon stop` on a shared computer: it ends every session's page connections, also other agents'.
- Without private mode, `devices`, `device list`, `session list`, `cdp status` and `daemon status` print device identifiers and the sessions of other agents: keep that output out of a shared transcript.

## The simulator

The iwdp guide has the steps of a turn. The details:

- `sim reserve` waits up to `--wait` (50 s; at most 10m, keep it under your tool's timeout). Never hold the turn while you wait for a delegate that drives the simulator; pass the delegate your owner in `IWDP_DEVICE_RESERVATION` instead.
- A reservation that you stop using ends 2 minutes after your last command; your open tab keeps it while you work on it. A command that lost the turn stops with exit 6: run `sim reserve` again. `device reserve` and `device release` are not for the simulator.
- When `sim up` prints `new tab unknown: run iwdp-cli tab find ...`, run it as printed: its `--device SIMULATOR` searches the simulator alone, so a plugged-in iPhone that you have not reserved does not stop it.
- `sim up` refuses to restart iwdp while a page has an active lease (`--restart-iwdp` overrides, only for your own leases) and refuses an iwdp that iwdp-cli did not start: report either refusal. Safari's start page is not inspectable: on `tabs 0`, run `sim up --url <url>`.

When the page is not enough, act on the simulator's whole screen: `sim screenshot` captures it with the keyboard and Safari's bars; `sim tap <x> <y>` (screen points; `sim ui` lists elements and their centers), `sim swipe` and `sim type` are real touches, so a tapped field opens the keyboard, unlike `click`. For keyboard bugs run `sim keyboard onscreen` first (it restarts the simulator; `sim type` then refuses Shift characters), and `sim keyboard hardware` after. Never `sim type` a secret. Run `sim down` only when the task says so: the user may watch the simulator.

## Faults

- **`status` prints `devices 0` although the device is plugged in.** Run `iwdp-cli tab list` once, if your task allows a restart of iwdp. On Linux, iwdp-cli then restarts the iwdp that it started itself when that iwdp lost the device. When the device stays unlisted, ask the user to unlock it and keep it unlocked, tap Trust and check Web Inspector, then list again; when it is unlocked and still unlisted, ask the user to unplug it and plug it in again. A refused restart means that the iwdp belongs to another program: report it.
- **`iwdp-cli status` exits 1.** iwdp is down or stuck. Report it with the failed check; do not restart it yourself.
- **Exit 5 while the daemon holds a page ("iwdp did not answer the page list").** Commands on a page that the daemon already holds still work: keep working in your session without `--page`. `tab list`, `tab new`, `tab close`, `tab find`, a `--page` for a page that is not attached, another session's first command and CDP clients fail. Only a restart of iwdp clears the fault; that belongs to the device owner. Report it, and mark the checks that need another tab as not run.
- **The device is listed, the tab listing shows no tab and `hidden 0`.** Ask the user to open the page in Safari. With `hidden` above 0 the pages exist, but your owner has no reservation of their device: reserve it, or ask the device owner.
- **Exit 4 on the first command of a tab that was fine before.** The tab is probably not the visible Safari tab: hidden tabs accept commands and never answer. Ask the user to bring it to the front. Or another inspector (another agent, Safari Web Inspector on a Mac) took the page: stop and ask. Only the device owner can approve `--takeover`.
- **Exit 4 on the first attach.** A first attach can fail once: run the command again. When it fails the same way again, treat it as the item above.
- **Exit 6 from `device reserve`.** Another owner holds the reservation. In private mode the line names no device; ask the device owner.
