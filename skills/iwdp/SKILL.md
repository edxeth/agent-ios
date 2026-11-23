---
name: iwdp
description: Drive and debug real iPhone or iPad Safari pages, an iOS Simulator's Safari on a Mac or macOS VM over SSH, and local WebKitGTK pages from Linux with iwdp-cli. Snapshot with @eN refs, synthetic actions, waits, dialogs, tabs, console, network, cookies, storage, screenshots, traces, and a CDP endpoint for agent-browser, Puppeteer and Playwright. Use when the user mentions iOS Safari, iPhone Safari, Web Inspector, iwdp, ios-webkit-debug-proxy, WebKit Inspector Protocol, mobile Safari bugs, real-device Safari screenshots, or asks to drive a connected iPhone browser.
---

# iwdp-cli

`iwdp-cli` drives Safari pages on a USB-connected iPhone or iPad, and pages of a local WebKitGTK browser, from the shell. Safari speaks the WebKit Inspector Protocol, not the Chrome DevTools Protocol (CDP); CDP clients such as agent-browser, Puppeteer and Playwright attach through the URL that `iwdp-cli get cdp-url` prints.

Install: `go install github.com/nnemirovsky/iwdp-cli/cmd/iwdp-cli@latest`, or a binary from https://github.com/nnemirovsky/iwdp-cli/releases. A device also needs ios-webkit-debug-proxy (`brew install ios-webkit-debug-proxy` on macOS).

## Start here

This file is a discovery stub, not the guide. Before you run any other `iwdp-cli` command, load the guide that matches the installed binary:

- Run `iwdp-cli skills get iwdp`. The guide has a few hundred lines: read it by line range, for example `iwdp-cli skills get iwdp | sed -n '1,80p'`, then the next range. Read the safety rules in full; they bind every run.
- Or run `iwdp-cli skills path iwdp`: it writes the guide to the user cache and prints its directory, and you read `SKILL.md` there with line offsets.

`iwdp-cli <command> --help` prints the flags of one command and never connects to a page.

The CLI serves the guide of its own version. This stub does not change between releases, so it only points at the CLI: a copy of the guide here would drift from the installed binary.
