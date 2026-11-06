# W1a notes for later workstreams

Design: docs/design/agentic-parity.md. W6 merges these notes.

## What W1a provides
- internal/transport: Transport, MessageConn, Limits (WebKitGTK limit 127 KiB, the largest size spike A saw pass).
- internal/transport/webkitgtk: HTML listing parser and page sockets. The iwdp transport is not written yet (W1b, with the deviceOSVersion and busy-hint parsing of internal/proxy).
- internal/webkit: Caller (Client and Session implement it), Session (Dial with two attempts, no direct fallback, active and setup targets, swaps, generations, pause on start, nudge, per-call budget, wire-byte limit, bounded per-subscriber event queues, SubscribeSync for read-loop bookkeeping), DialWebSocket.
- internal/core: Scheduler (classes, epoch fencing, op-fenced Caller, 8 calls in flight, control lane, Release barrier), RunSteps attempt loop with ClockMap bounds, DesiredState, BootstrapParts, PageSession.
- internal/commands: Command spec, Register, CLI parser and help, MCP schema, registration manifest, RegisterMCP.
- internal/runtime: call contract (Expression, envelope, install and retry once, CallAsync) and an embedded bundle that has only js/00-header.js. internal/refs: forward-only ref counter.
- internal/webkit/testutil/fakewebkit: fake endpoint and PageRuntime (page side of the attempt fence).

## Page-side protocol that W3a and W3b must implement
- `__iwdp.call("clock")` returns `{now: performance.now(), doc}`; `doc` is a random id that the runtime picks at install, so every document has its own.
- `__iwdp.call("cancel", {opId})` marks the operation canceled.
- An attempt function receives `{opId, epoch, doc, step, steps, attemptEnd, deadline, args}` (attemptEnd and deadline in performance.now() milliseconds) and returns `{status, reason, now, doc, value}` with status dispatched, retry, expired, interrupted or refused. It refuses an attempt whose doc is not its document or whose epoch is older than the installed owner epoch (a newer epoch becomes the owner), and it runs no event of a step after its call has settled. fakewebkit.PageRuntime is the reference model (KeepOpen models an unsettled attempt, Navigate a new document).

## Contracts added by the W1a repair
- Session.ActiveSetup reports a failed provisional setup of the active target as *webkit.SetupError. Until Session.ReapplySetup succeeds, Scheduler.Begin refuses operations with it at admission (also operations that queued before the swap); a setup in progress is waited for within the operation deadline. The binding check is in Session.Send: it chooses the active target and checks that target's setup under one lock, waits for a running setup within the call budget, and refuses a failed one with ErrNotSent and *SetupError, so a swap cannot commit between the check and the send. Session.SendControl (control lane, Scheduler.Control, the release barrier's cleanup), Session.Probe and the TargetCaller of Setup/ReapplySetup do not pass this gate. W1b must call ReapplySetup (re-apply desired state after a swap), serialized with page operations.
- A send of an attempt after RunSteps stopped waiting for it (lost reply, cancel) fails with core.ErrAttemptClosed as not sent, also while the operation is still active.
- webkit.ErrNotSent marks a command that was never written; the control lane keeps a pending item open after such a failure and treats any other failure as completed (no duplicate completion).
- commands.RunCLI takes newEnv(ctx, inv) and applies --timeout, IWDP_TIMEOUT, Command.Timeout or DefaultTimeout (30 s) before newEnv; RegisterMCP applies Command.Timeout or DefaultTimeout.
- cmd/iwdp-mcp registers the registry tools of the selected profile (registerRegistryTools); their Env.Page fails until W1b provides the daemon page session.
- A document change during an action stops it with core.ErrDocumentChanged (nothing ran in the new document); W3b should make navigating commands wait for the new document before the next action.

## Open items for W1b
- iwdp transport; split of client.go into a Session-backed Client; internal/tools signature change to webkit.Caller.
- Hang detection after the call budget (probe, ErrSessionStalled, background recovery); W1a returns ErrCallTimeout at the call budget.
- Network reinitialization barrier: DesiredState reports NeedsNetworkReset; the barrier itself is not written.
- Env.Manager() is not in commands.Env yet (no core.Manager); no registry command is wired into cmd/iwdp-cli (needs an Env, that is W1b's daemon or no-daemon mode).
- --content-boundaries is parsed but not applied (marker format of agent-browser not checked).
- fakewebkit lacks the fake iwdp listing HTTP and console replay.

## Proof results (WebKitGTK 2.54, this host)
- P1: with pause on start, 10 of 10 cross-site swaps (127.0.0.1 and localhost) ran the bootstrap part before the first inline script. Without pause on start only 4 of 10 did (setup races the new document). The 204 navigation created a provisional target that was destroyed without commit and was dropped. Two ports of one host are the same site and do not swap processes. Consequence for 2.7, 3.7 and W3b/W4: "runs before page scripts" needs pause on start; without it, it is partial.
- P2 (CDP bridge with Playwright, Puppeteer and agent-browser) was not run in W1a.
