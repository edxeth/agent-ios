---
name: agent-ios
description: Drive and debug Safari on a real iPhone or iPad or in an iOS Simulator on macOS over SSH, and local WebKitGTK pages, from Linux with agent-ios. Use whenever a task involves iOS, iPhone or iPad (for example checking that web changes look right on iOS), iOS Safari, Web Inspector, iwdp or mobile Safari bugs. Prefer it to desktop browser emulation for iOS.
---

# agent-ios

`agent-ios` drives Safari pages on an iOS simulator or USB-connected iPhone or iPad, and pages of a local WebKitGTK browser, from the shell. Safari speaks the WebKit Inspector Protocol, not the Chrome DevTools Protocol (CDP); CDP clients such as agent-browser, Puppeteer and Playwright attach through the URL that `agent-ios get cdp-url` prints.

## Start here

This file is a discovery stub, not the guide. Before you run any other `agent-ios` command, load the guide that matches the installed binary:

- Run `agent-ios skills get agent-ios`. Read the safety rules in full; they bind every run.

To test on iOS, use the iOS Simulator; a plugged-in device only when the user asks. Agents share it in turns. First serve your page in a background job on a random uncommon port, and check that it answers. Then repeat `agent-ios sim reserve` until it succeeds: exit 3 or a tool timeout means another agent's turn, and waiting is normal, never a reason to stop. `agent-ios sim up --url http://10.0.2.2:<port>/` opens your tab, starting the VM if needed: select the tab id it prints. At the end, also after a failure, `tab close` your tab and run `agent-ios sim release`. Pass this on to agents you delegate the check to.

`agent-ios <command> --help` prints the flags of one command and never connects to a page.

The CLI serves the guide of its own version. This stub does not change between releases, so it only points at the CLI.
