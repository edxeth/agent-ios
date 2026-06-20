---
name: iwdp
description: Debug and automate real iPhone/iPad Safari pages from Linux through ios-webkit-debug-proxy and iwdp-cli. Use when the user mentions iOS Safari, iPhone Safari, Web Inspector, iwdp, WebKit Inspector Protocol, mobile Safari bugs, real-device Safari screenshots, iOS cookies/storage/network/console debugging, or asks to drive a connected iPhone browser.
---

# iOS Safari Debugging with iwdp

Use this skill for **real iOS Safari** on a USB-connected iPhone/iPad. This is not Chrome CDP, Playwright, or agent-browser. The stack is:

```text
system usbmuxd → ios_webkit_debug_proxy → WebKit Inspector Protocol → iwdp-cli
```

`agent-browser` cannot connect to this directly because it expects Chrome DevTools Protocol. Use `iwdp-cli` for real iOS Safari.

## Local contract on this machine

Commands are globally available:

```bash
iwdpctl                 # doctor/start/fix the iwdp service; default command
iwdp-cli                # CLI for Safari page actions
ios_webkit_debug_proxy  # low-level proxy, usually started by iwdpctl
```

`iwdpctl` is a local wrapper script. Under the hood it runs:

```bash
ios_webkit_debug_proxy --no-frontend -c 'null:9221,:9224-9229'
```

If `iwdpctl` is missing, run that command directly, then use `iwdp-cli`.

Ports for this local setup:

```text
9221 = iwdp device-list endpoint: connected iPhones/iPads
9224 = first iOS device Safari page endpoint
9225 = second iOS device, then 9226, etc. up to 9229
9222 = local Chromium on this machine, so this setup avoids it
```

These ports come from `iwdpctl`'s config, `IWDP_CONFIG=null:9221,:9224-9229`. Stock iwdp usually puts the first device on `9222`; this host moves iOS pages to `9224` to avoid local port conflicts.

The range `9224-9229` is only an assignment range. iwdp does not bind every port in the range. It binds one device port per connected iOS device.

## Device-side prerequisites

The iPhone must have:

- Web Inspector enabled: Settings → Safari → Advanced → Web Inspector = ON
- at least one Safari tab open
- the device unlocked and trusted if pairing or Web Inspector access asks for it

With no open Safari tab, `9224` can be up while `iwdp-cli pages` returns `[]`.

## First move: health gate

Before using `iwdp-cli`, run:

```bash
iwdpctl
```

Bare `iwdpctl` means `doctor`: it starts iwdp if missing and fixes the stale state where `usbmuxd` sees the iPhone but iwdp's `9221` device list is empty.

Healthy output has all of these:

```text
USB devices seen by libimobiledevice: <UDID>
Pairing: SUCCESS
Ports: 9221 and 9224 both owned by ios_webkit_debug_proxy
iwdp devices on 9221: [{ ..., "url": "localhost:9224" }]
```

After a reboot, plug in and unlock the phone, then run:

```bash
iwdpctl
```

If `9221` is alive but `Devices: []`, iwdp is stale. Run:

```bash
iwdpctl
# or, more forcefully:
iwdpctl restart
```

If pairing fails, tell the user to unlock the iPhone and tap **Trust This Computer**, then run:

```bash
idevicepair validate
iwdpctl
```

A passcode or Trust error means the phone needs user action. It does not mean iwdp is down.

## Default workflow

Use `iwdp-cli` for deterministic debugging and reproducible transcripts.

```bash
iwdpctl

iwdp-cli pages
iwdp-cli eval 'document.title'
iwdp-cli eval 'location.href'
iwdp-cli screenshot -o iphone-safari.png
```

`iwdp-cli` auto-connects to the first available Safari page when no WebSocket URL is passed. That is the right default for one tab.

`iwdpctl ws` is also a first-page shortcut. Do not use it to choose among several tabs. When multiple tabs or devices are open and you need a specific page, list pages and copy that page's `ws://` URL:

```bash
iwdp-cli pages
iwdp-cli eval 'document.title' 'ws://localhost:9224/devtools/page/3'
```

Navigation and interaction:

```bash
iwdp-cli navigate 'https://example.com'
iwdp-cli click 'button[type=submit]'
iwdp-cli fill 'input[name=email]' 'user@example.com'
iwdp-cli type-text 'hello'
```

DOM discovery:

```bash
iwdp-cli eval 'document.body.innerText.slice(0,2000)'
iwdp-cli eval 'JSON.stringify([...document.querySelectorAll("a,button,[role=button]")].map((el,i)=>({i,tag:el.tagName,text:(el.innerText||el.value||el.getAttribute("aria-label")||"").trim(),href:el.href||null})), null, 2)'
```

Logs and storage:

```bash
iwdp-cli console-messages -d 5
iwdp-cli cookies
iwdp-cli get-local-storage 'https://example.com'
iwdp-cli get-session-storage 'https://example.com'
```

Network note: `iwdp-cli network` attaches to the WebKit Network domain, but it may be quiet on some pages. For a quick fallback inventory, use page resource timing:

```bash
iwdp-cli eval 'JSON.stringify(performance.getEntriesByType("resource").map(r=>({name:r.name,initiatorType:r.initiatorType,transferSize:r.transferSize,duration:Math.round(r.duration)})).slice(-30), null, 2)'
```

## Common failure states

### `iwdp-cli pages` says ios_webkit_debug_proxy is not running

Run:

```bash
iwdpctl
```

### `9221` is listening but device list is `[]`

This means iwdp is stale, not healthy. Run:

```bash
iwdpctl
# or
iwdpctl restart
```

### `9224` connection refused

`9224` only exists after iwdp sees a device on `9221`. Run:

```bash
iwdpctl
curl -fsS http://localhost:9221/json
iwdp-cli pages
```

### Device visible on `9221` but `iwdp-cli pages` returns `[]`

The device is attached, but Safari has no inspectable tab. Ask the user to open a page in Safari and confirm Web Inspector is on, then run:

```bash
iwdp-cli pages
```

### `idevicepair validate` mentions passcode or trust

The phone is visible but locked or not trusted. Ask the user to unlock it and accept Trust, then run:

```bash
idevicepair validate
iwdpctl
```

### User asks whether to use agent-browser

For real iOS Safari, use this skill. `agent-browser` is CDP-first and cannot talk to WebKit Inspector Protocol without a separate CDP→WIP adapter.

## What to report back

When showing results to the user, include:

- device name/port from `iwdpctl status`
- selected page URL/title
- screenshot path when captured
- commands used, if the user is debugging setup
- any auth blocker without exposing secrets or tokens
