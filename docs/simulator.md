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

The Mac needs Xcode with an iOS Simulator runtime, Remote Login (sshd) for the user, and that user logged in to the desktop: a simulator does not run without a desktop session, so a Mac that starts unattended needs automatic login.

## Commands

- `iwdp-cli sim up [--device D] [--url URL] [--no-window] [--restart-iwdp]` does what is still needed, in order: start the Mac with `start` when ssh gets no answer, wait until it answers and a user is logged in to its desktop, boot the simulator, open its window (unless `--no-window`), start Safari, forward `127.0.0.1:27753` to the simulator's `webinspectord_sim` socket (whose path changes at every boot), restart iwdp when it does not list the simulator, open `--url` in Safari, and wait up to 30 s for a tab. Run it again at any time: when everything runs, it changes nothing. Its default deadline is 15 minutes.
- `iwdp-cli sim status [--device D]` checks each of these parts and changes none; it does not start iwdp. `--json` has `ready`.
- `iwdp-cli sim down [--device D] [--mac]` stops the forward and shuts the simulator down; `--mac` also runs `stop`.

A URL of this host, opened in the simulator, uses the Mac's address for this host: `http://10.0.2.2:<port>/` from a QEMU VM with user networking.

Safari's start page is not offered for inspection. When `sim up` reports `tabs: 0`, run it again with `--url`, then `iwdp-cli tab list`.

## Ownership and restarts

- `sim up` stops only the ssh forward that it started and recorded (in the daemon's private directory), after it checked that process's command line. When another program listens on `127.0.0.1:27753`, `sim up` fails and changes nothing.
- A restart of iwdp ends every page connection, those of physical devices too. `sim up` restarts only the iwdp that iwdp-cli started (`restart-iwdp`, device-runbook.md), and refuses while a page of another device has an active lease, unless `--restart-iwdp`.
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
