# A simulator on a Mac over SSH

`iwdp-cli sim up`, `sim status` and `sim down` drive Safari in an iOS Simulator that runs on a Mac which this host reaches over SSH: a macOS VM on this Linux host, or a real Mac on the network. The simulator then appears in `iwdp-cli tab list` like a device, and every page command works on its tabs. Simulator pages need no device reservation (sessions.md).

```text
Safari in the Simulator (Mac) -> webinspectord_sim socket -> ssh -L 127.0.0.1:27753 -> ios_webkit_debug_proxy (-s 127.0.0.1:27753) -> iwdp daemon -> iwdp-cli
```

ios_webkit_debug_proxy (iwdp) looks for a simulator on `127.0.0.1:27753`, its `-s` address, and connects to it only when it starts. `sim up` forwards that port over SSH to the simulator's Web Inspector socket on the Mac, and restarts the iwdp that iwdp-cli started when that iwdp does not list the simulator.

## Configuration

`sim` reads `$XDG_CONFIG_HOME/iwdp-cli/simulator.json` (`~/.config/iwdp-cli/simulator.json`):

```json
{
  "ssh": "macos-tahoe",
  "sshConfig": "~/projects/reims-vgpu/.ssh/config",
  "start": ["reims-ctl", "start", "macos-tahoe"],
  "stop": ["reims-ctl", "stop", "macos-tahoe"],
  "device": "iPhone 17 Pro"
}
```

| Field | Meaning |
|---|---|
| `ssh` | The ssh destination of the Mac: a `Host` of `sshConfig` or of your ssh configuration, or `user@host`. Required. ssh must log in without a prompt (`BatchMode`), for example with a key. |
| `sshConfig` | The ssh configuration file (`ssh -F`). Optional; `~/` is the home directory. |
| `start` | The command that this host runs when the Mac does not answer over ssh, for example the start of a VM. Optional; without it `sim up` fails when the Mac is down. |
| `stop` | The command that `sim down --mac` runs on this host. Optional. |
| `device` | The simulator: its UDID or its exact name (the newest runtime's device of that name). Optional; without it the one booted iOS simulator. `--device` overrides it. |
| `axe` | The path of AXe's `axe` program on the Mac, for the touch commands. Optional; without it `sim tap`, `sim swipe`, `sim type` and `sim ui` fail. |

The Mac needs Xcode with an iOS Simulator runtime, Remote Login (sshd) for the user, and that user logged in to the desktop: a simulator does not run without a desktop session, so a Mac that starts unattended needs automatic login.

## Commands

- `iwdp-cli sim up [--device D] [--url URL] [--no-window] [--restart-iwdp]` does what is still needed, in order: start the Mac with `start` when ssh gets no answer, wait until it answers and a user is logged in to its desktop, boot the simulator, open its window (unless `--no-window`), start Safari when it does not run, forward `127.0.0.1:27753` to the simulator's `webinspectord_sim` socket (whose path changes at every boot), start iwdp, or restart the one that iwdp-cli started when it does not use that forward, open `--url` in Safari, and wait up to 30 s for a tab. Run it again at any time: when everything runs, it changes nothing. A `--url` that is not an absolute http or https URL is a usage error (exit 2) before any step. Its default deadline is 15 minutes.
- `iwdp-cli sim status [--device D]` checks each of these parts and changes none; it does not start iwdp. `--json` has `ready`.
- `iwdp-cli sim down [--device D] [--mac]` stops the forward and shuts the simulator down; `--mac` also runs `stop`.

A URL of this host, opened in the simulator, uses the Mac's address for this host: `http://10.0.2.2:<port>/` from a QEMU VM with user networking.

Safari's start page is not offered for inspection. When `sim up` reports `tabs: 0`, run it again with `--url`, then `iwdp-cli tab list`. A page list that iwdp does not answer is an error, not `tabs: 0`.

## The whole screen and real touches

The page commands see only Safari's page. For what lies outside it, or what only a real finger triggers, the sim commands act on the simulator's screen:

- `iwdp-cli sim screenshot [path]` captures the whole screen (`simctl io screenshot`): the status bar, Safari's bars, the on-screen keyboard and other apps. `screenshot` captures the page alone.
- `iwdp-cli sim tap <x> <y>`, `sim tap --label <label>` or `--id <id>` (`--hold <seconds>` for a long press), `sim swipe <x1> <y1> <x2> <y2>` and `sim type <text>` send real touches and key presses through [AXe](https://github.com/cameroncooke/AXe), which iOS handles as the user's: a tapped text field opens the on-screen keyboard and can zoom the page, which a synthetic `click` does not do. Coordinates are screen points: `sim screenshot`'s pixel size divided by the scale (3 on an iPhone 17 Pro, 402×874 points). Safari's page starts below the status bar, at 62 points on that iPhone.
- `iwdp-cli sim ui` lists the screen's elements that have a label, value or identifier (Safari's bars, the keyboard's keys, alerts), with the center that `sim tap` would touch. It shows page and app text.
- `iwdp-cli sim keyboard [onscreen|hardware]` shows or sets the keyboard. `hardware` (the Simulator's default) types the text that the simulator's keyboard layout has keys for (AXe refuses other characters, such as 日 on a US layout) but hides the on-screen keyboard, as an iPad with a keyboard does. `onscreen` shows it for a tapped field, as on an iPhone, for keyboard bugs; AXe's key presses then lose Shift, so `sim type` types only lowercase letters, digits and unshifted symbols, on a US layout, and refuses the rest (tap the keys with `sim tap --label` instead). A change quits the Simulator app and restarts the simulator (about a minute) and runs `sim up`. It checks first and refuses, changing nothing, when the simulator does not exist, another simulator runs (quitting the app would shut it down), the iwdp is not iwdp-cli's, or a page has an active lease (unless `--restart-iwdp`).

`sim type` returns once the key presses are sent; the field can take a moment to show them, so wait (`wait --fn`) before reading its value. It puts its text on the command lines of this host and the Mac: never type a secret with it. `fill --stdin` types into a page without that.

AXe runs on the Mac. Install its release (a universal binary for Intel and Apple silicon; GPL-3.0, run as a separate program) and name it in the configuration as `"axe"`, for example:

```sh
curl -fL -o AXe.tgz https://github.com/cameroncooke/AXe/releases/download/v2.1.1/AXe-macOS-v2.1.1-universal.tar.gz
scp AXe.tgz my-mac:/tmp/ && ssh my-mac 'mkdir -p ~/axe && tar xzf /tmp/AXe.tgz -C ~/axe'
# simulator.json: "axe": "/Users/<user>/axe/axe"
```

## Several agents

Several agents, each on its own repository, share one simulator in turns: Safari answers only for its visible tab, and the sim commands act on one screen.

- `iwdp-cli sim reserve [--owner O] [--ttl 2m] [--wait 50s]` reserves the simulator for an owner (`--owner`, else `--reservation` / `IWDP_DEVICE_RESERVATION`, else the session). The owner must be a name of the agent's own: the default session (`default`, every agent's that sets none) is refused with exit 2. While it waits it prints at once that this is another agent's turn and to run it again if cut off. While another owner holds it, it waits up to `--wait` (at most 10m), checking every 2 s, and then fails with exit 3 and a message that says to run it again: an agent whose tool call ended first also runs it again. A free simulator, or the owner's own reservation, returns at once (the latter renewed). With `--json` the waiting line goes to stderr, so that stdout holds the JSON result alone, and in private mode, whose stderr is one status line, it is not printed. `--ttl` is at least 30 s: the renewals below renew only a reservation that is still live, and the daemon's sweep runs every 5 s, so a shorter TTL could end a turn in use between two of them.
- While an owner holds it: other owners' page commands on the simulator fail with exit 6, also on a page that the daemon is already attached to and under a lease that they took before; `tab list`, `tab find` and the CDP endpoint (`get cdp-url` and the page list of an endpoint issued before) hide its pages from them; their sim commands except `sim status` fail with exit 6; and the daemon's sweep (every 5 s) ends their leases on its pages, so that no event stream or CDP attachment of theirs goes on in that turn. A reservation that cannot be read admits nothing on the simulator. The owner's sim commands renew it for as long as each runs (every second; a `sim up` can run 15 minutes), and so does the daemon while one of the owner's page leases on the simulator is open (a lease ends after 5 minutes idle; a lease on a physical device renews only that device's reservation). A sim command stops with exit 6 when its turn ends while it runs: its reservation was released or expired, or, run on a free simulator, another owner reserved it. It holds the turn it started in: when its owner releases the simulator and reserves it again, that is a new turn, and the command stops; it also fails with exit 6 when it returned without an error after its turn ended. An operation admitted just before another owner reserves the simulator can still send its commands until the sweep fences its lease, within 5 s: from then on the lease admits no operation and its running operations are cancelled, also while another page's cleanup stalls; the lease itself ends once its page-side cleanup ends, at most 5 s later: the reservation is cooperative turn-taking, as sessions.md says of reservations, not a lock that cuts off work in flight. The reservation ends with `iwdp-cli sim release`, or `--ttl` (2 minutes) after the owner's last use, so a run that ends without its release, and without an open tab, holds the others up at most that long. `sim status` names the owner.
- Without a reservation the simulator is free for all, as for a single agent.
- The simulator's reservation stands apart from the physical devices': `device reserve` of every device (`*`) neither covers nor blocks it, and `device reserve` and `device release` refuse `--device SIMULATOR` (exit 2): the sim commands take and give back the turn.
- `tab find --device SIMULATOR` and `tab list --device SIMULATOR` look at the simulator's pages alone: a plugged-in iPhone that the agent has not reserved hides its pages, which stop a `tab find` over every device with exit 6, but not one limited to the simulator. Only that device's page list is read: another device's failing page list does not matter, and the simulator's own failing page list is an error (exit 5), not a simulator without tabs (`tab find` exit 7). An agent on a phone uses `--device <UDID>` the same way while another agent holds the simulator.
- `sim up --url` opens a new tab, which becomes the visible one: each agent works in its own tab and leaves the tabs of earlier turns alone. At the end it closes its own tab (`tab close`) and runs `sim release`, which also closes its session (its page leases) and releases the simulator. `tab close` runs `window.close()`, which Safari may refuse: in live runs it closed the tabs that `sim up --url` opened, but refused one that had been navigated 40 times. When it fails, the agent leaves the tab and still runs `sim release`. `sim up --url` returns once Safari lists a tab of that URL that was not listed before the call (a tab of an earlier turn with the same URL does not count), and whose URL equals `--url` in the form a browser writes it (WHATWG URL; a URL whose form is not certain, such as one with user info, never matches). It prints that tab's id (`new tab: tN`) for `iwdp-cli tab tN`; when the id cannot be found it still succeeds and the agent uses `tab find --device SIMULATOR --url-file`. Under `IWDP_NO_DAEMON=1` it prints no id: the core and its tab ids end with the command, and the next command numbers the tabs anew. When it has no id and another tab shows the same URL (an earlier tab, or a second new one; `"sharedURL": true` in `--json`), `tab find` would find several tabs, so it says so instead and tells the agent to serve its page on a URL that no other tab shows (a port or path of its own) and run `sim up --url` with it, leaving the other tabs alone. It fails with exit 7, saying how to check the server, when no such tab shows within 30 s; a page list of iwdp that fails is that error instead (exit 1), not exit 7.
- While the simulator is reserved, a page command of the owner's session that has chosen no tab (no `tab <id>`, no `--page`) fails with exit 5 when the simulator shows several tabs, debuggable or not, instead of acting on the first one, which may be an earlier agent's; also when the session used its one tab before the others appeared, and when the tabs cannot be counted. Another owner gets exit 6 first. Without a reservation the first debuggable page is the session's, as for a single agent.
- Not isolated: Safari's cookies and storage of one origin are shared by all tabs, so two agents on the same origin (host and port) see each other's state.

## Ownership and restarts

- `sim up` signals only the ssh forward that it started and recorded (in the daemon's private directory, under a lock): the process must have the recorded pid, start time and start token, as for `restart-iwdp`, checked again right before each signal. A record whose pid no process has, or another process has now, is removed without a signal; a process that cannot be identified (EPERM included), or that does not end after SIGTERM, is an error and keeps its record. A new ssh that cannot be shown to be the one just started is not signaled.
- The forward counts only while its own process receives the connections to `127.0.0.1:27753` (the most specific listening socket for that address, as for iwdp's start). When another program listens there, `sim up` fails and starts nothing.
- `sim up` uses only an iwdp that iwdp-cli started, which looks for a simulator on 127.0.0.1:27753, also after its own start: another program's iwdp is refused, since `sim up` cannot tell where it looks nor restart it.
- iwdp's SIMULATOR device counts as the forward's only when iwdp started or restarted after that forward listened (the record's `synced`). Otherwise `sim up` restarts iwdp.
- A restart of iwdp ends every page connection, the simulator's and those of physical devices. `sim up` refuses it while the daemon holds an active page lease on any device, unless `--restart-iwdp`. It counts before it changes the forward and again right before the restart; a lease taken between that count and the restart still ends (known-limits.md).
- Quitting the Simulator app on the Mac shuts its simulators down; `sim up` boots the simulator again.

## Linux: usbmuxd

ios_webkit_debug_proxy 1.9.2 exits at its start (status 255) when usbmuxd does not answer on `/var/run/usbmuxd`, also when it should list only a simulator. Linux distributions start usbmuxd when an Apple device is plugged in and stop it after the last one is unplugged, so with no iPhone plugged in iwdp does not start. Keep usbmuxd running, for example with this systemd drop-in (`/etc/systemd/system/usbmuxd.service.d/always-on.conf`), then `systemctl daemon-reload` and `systemctl enable --now usbmuxd`:

```ini
[Service]
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

`Restart=always` brings it back after the udev rule's `usbmuxd -x` stops it at an unplug. `sim up` names this cause when an iwdp start fails while usbmuxd does not answer.
