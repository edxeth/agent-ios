# WebKit Inspector Protocol through ios-webkit-debug-proxy

How agent-ios talks to iOS Safari through ios-webkit-debug-proxy (iwdp), and the protocol traps that the code works around. Read it before you change code that sends WebKit Inspector Protocol commands (`internal/webkit`, `internal/tools`, `internal/core`, `internal/cdp`, `internal/devtools`, `internal/proxy`).

## Ports

- **Port 9221** is the listing port: `http://localhost:9221/json` returns the connected devices. `proxy.CheckListing(ctx, addr)` detects iwdp on it; never use a device port for detection.
- **Device ports**: each device gets its own port. iwdp's default configuration gives them 9222 to 9322 (`iwdpPortFirst` in internal/core/manager.go); an iwdp that agent-ios starts uses 9224 and up (`proxy.DefaultFirstDevicePort`).
  - With agent-ios's iwdp, `http://localhost:9224/json` lists the pages of the first device, `http://localhost:9225/json` those of the second, and so on. `proxy.DevicePort(entry)` takes the port from a device entry's URL.
  - iwdp lists a SIMULATOR pseudo-device before real devices, so an unpinned configuration gives that ghost entry the first device port. Auto-start (`proxy.Start`) pins the first UDID to 9224 to keep the default port on a real device (internal/proxy/config.go).
- Each page has a `webSocketDebuggerUrl` for its WebSocket connection, which speaks the **WebKit Inspector Protocol**. A page accepts only one WebSocket debugger connection.

## Target routing

iwdp uses **Target routing**: a client cannot send domain commands directly on the WebSocket. On connect, iwdp sends a `Target.targetCreated` event with a `targetId`. Every later command is wrapped in `Target.sendMessageToTarget`:

```
Client → Target.sendMessageToTarget { targetId, message: JSON-stringified inner command }
iwdp → WebKit
WebKit → Target.dispatchMessageFromTarget { targetId, message: JSON-stringified response }
iwdp → Client
```

- The outer `sendMessageToTarget` gets an empty ack `{"result":{},"id":N}`, which is ignored.
- The inner command has its own ID, used to match the response (dual-ID routing).
- `webkit.Client` does this transparently: on connect it waits up to `TargetWaitTimeout` (100 ms) for `Target.targetCreated`; if one arrives, every `Send()` call is wrapped in Target routing.
- The mock test server (`internal/webkit/testutil/mock.go`) sends no `Target.targetCreated` by default, so the client falls back to direct mode (no wrapping). `MockServer.EnableTargetRouting(id)` makes the mock behave like iwdp for tests of Target routing.
- iwdp sends error `data` as a JSON array `[{"code":...,"message":...}]`; `ErrorData.Data` is a `json.RawMessage` to handle this.

## Domain enable and disable

Some `<Domain>.enable` methods work through Target routing (for example `Debugger.enable`, `Canvas.enable`, `Worker.enable`, `Animation.enable`). Others, like `CSS.enable`, hang without a response. Most domain methods (`DOM.getDocument`, `CSS.getMatchedStylesForNode`, `CSS.getComputedStyleForNode`, `CSS.setStyleText`, `Runtime.evaluate`) work without an explicit enable.

Checked 2026-11-26 on the iOS 26.5 Simulator through iwdp: `CSS.getMatchedStylesForNode`, `CSS.getInlineStylesForNode` and `CSS.getComputedStyleForNode` answer in about 42 ms without `CSS.enable`, and the connection stays usable (`css matched` and `css computed` use only these). `DOM.requestNode` before `DOM.getDocument` fails with "Document must have been requested" on iOS and with an empty error message on WebKitGTK 2.54.

The pause on `debugger` statements and on exceptions live in JSC::Debugger, not in the Debugger agent: they survive `Debugger.disable` and a new inspector connection, and they act as soon as `Debugger.enable` attaches the debugger. WebKit has no getter for them. A new JSC::Debugger pauses on neither (Debugger.h: the three pause breakpoints start null; Web Inspector's frontend turns the debugger statements on itself). `setPauseOnDebuggerStatements` and `setPauseOnExceptions` are accepted without `Debugger.enable`. agent-ios therefore sends both, turned off, before every `Debugger.enable` that it makes for an owner that must not pause (logpoints, the CDP Debugger subset), and its agent reset uses that baseline (no pause on `debugger` statements; earlier versions reset it to on).

## Known limitations

- `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` hang through iwdp Target routing. The command is sent but no response comes back, and the hang corrupts the connection: every later command on the same connection hangs too. `webkit.Client.Send` refuses these three methods when Target-routed (`ErrHangsThroughTarget`; `TestClient_TargetRoutedCSSGuard` in internal/webkit/client_test.go).
- `Page.snapshotRect` requires explicit pixel dimensions: compute them first with `Runtime.evaluate` (see `CaptureScreenshot` in internal/tools/page.go). `coordinateSystem` is `Viewport` (default screenshot) or `Page` (full page); element screenshots use `Page.snapshotNode`.
- WebKit's `Runtime.evaluate` has no `awaitPromise` parameter (iOS 16.7 and current WebKit). `EvaluateScriptWithOptions` evaluates by reference and calls `Runtime.awaitPromise` on a returned Promise. A thrown value comes back as `result` with `wasThrown: true` (no `exceptionDetails`); the stack is read from the thrown object.
- A message sent after a navigation commits finds no injected script (`Runtime.awaitPromise`: "Missing injected script for given promiseObjectId"), and through iwdp a round trip takes about 40 ms on the iOS Simulator, longer than a fast page needs to commit. So the agent runtime's async calls (`act`, `wait`; internal/runtime `arm`) arm their reply first: one `Runtime.evaluate` makes a promise and returns it by reference, `Runtime.awaitPromise` on it is written, and only then the call's `Runtime.evaluate` (by value), which settles that promise with its result. WebKit runs a target's commands in the order they arrive, so the await is registered before the call runs and is answered when the call's promise settles, before a navigation that the call started (a link's click, a change handler) commits. A result that is not a promise comes by value with the call itself.
- `Page.Cookie.expires` is in milliseconds since the Unix epoch. `sameSite` must be `None`, `Lax` or `Strict`; an empty value is rejected, so `tools.SetCookie` defaults it to `Lax`. A session cookie is sent with `session: true` and no `expires` key: iOS 16.7 WebKit keeps any `expires`, and 0 becomes an expiry at the Unix epoch. `session` is always sent, because iOS 16.7 reads it without a check.

## Client deadlines and writes

- `webkit.Client.Send` applies `DefaultSendTimeout` (30 s) when the context has no deadline, and `NewClient` uses a 5 s WebSocket handshake timeout (`DialTimeout`).
- gorilla/websocket is not safe for concurrent writes: the one-token channel `Client.writeSem` protects `conn.WriteMessage`, and `Send` stops waiting for it when its context ends.
