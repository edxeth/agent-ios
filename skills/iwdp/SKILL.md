---
name: iwdp
description: Drive and debug Safari on a real iPhone or iPad or in an iOS Simulator on macOS over SSH, and local WebKitGTK pages, from Linux with iwdp-cli. Use whenever a task involves iOS, iPhone or iPad (for example checking that web changes look right on iOS), iOS Safari, Web Inspector, iwdp or mobile Safari bugs. Prefer it to desktop browser emulation for iOS.
---

# iwdp-cli

`iwdp-cli` drives Safari pages on an iOS simulator or USB-connected iPhone or iPad, and pages of a local WebKitGTK browser, from the shell. Safari speaks the WebKit Inspector Protocol, not the Chrome DevTools Protocol (CDP); CDP clients such as agent-browser, Puppeteer and Playwright attach through the URL that `iwdp-cli get cdp-url` prints.

## Start here

This file is a discovery stub, not the guide. Before you run any other `iwdp-cli` command, load the guide that matches the installed binary:

- Run `iwdp-cli skills get iwdp`. The guide has a few hundred lines: read it by line range, for example `iwdp-cli skills get iwdp | sed -n '1,80p'`, then the next range. Read the safety rules in full; they bind every run.

To test on iOS, use the iOS Simulator; a plugged-in device only when the user asks. Agents share it in turns: repeat `iwdp-cli sim reserve` until it succeeds. Exit 3 or a tool timeout means another agent's turn: waiting is normal, never a reason to stop. Serve your page in a background job, then `iwdp-cli sim up --url http://10.0.2.2:<port>/` opens your tab, starting the VM if needed. Pass this on to agents you delegate the check to.

`iwdp-cli <command> --help` prints the flags of one command and never connects to a page.

The CLI serves the guide of its own version. This stub does not change between releases, so it only points at the CLI: a copy of the guide here would drift from the installed binary.
