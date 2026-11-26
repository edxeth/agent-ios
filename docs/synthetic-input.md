# Synthetic input and impossible features

## The synthetic-input statement

agent-ios cannot produce trusted input on iOS. The WebKit Inspector Protocol has no Input domain, so every click, tap, key press, scroll and drag is a **synthetic DOM event** that the injected runtime dispatches in the page. Every such event has `isTrusted === false`.

What this means for results:

- Every action result says that the input was synthetic (`"trusted": false` in JSON), and names each default action that the runtime emulated (for example a form submission after Enter).
- A page that checks `event.isTrusted`, or a browser feature that needs a real user gesture, can behave differently than for a person: popups, clipboard access, media playback, fullscreen, file pickers, some payment and sign-in widgets. A check that fails for such a reason is not proof of a bug. Reports must state that the input was synthetic.
- The runtime follows real input closely: pointer, mouse, touch and key events in browser order; cancelled events stop what follows (a cancelled `keydown` inserts no text; a cancelled `beforeinput` changes no value); text replaces the selection; Enter, Space, Tab, Backspace and the arrow keys get their default action emulated once; values are set through the native setter, so React and similar frameworks see them.
- `click --new-tab`, `tab new` and CDP `Target.createTarget` use `window.open` with an emulated user gesture (`Runtime.evaluate` with `emulateUserGesture`). This is the only user activation that WIP can give, and Safari can still block the popup.

A trusted path exists only through Safari's remote automation, which opens its own window and needs a device setting. It is not part of this release.

## Impossible or partial features

| Feature | Status | Reason | Closest substitute |
|---|---|---|---|
| Trusted input (real tap, key, mouse) | impossible | WIP has no Input domain | synthetic events (above) |
| CSS `:hover` from a pointer move | partial | synthetic moves do not set the hover state | `hover --css` forces `:hover` through `CSS.forcePseudoState`, but only where `CSS.enable` answers (WebKitGTK). Through iwdp `CSS.enable` hangs, so `hover --css` fails with exit 2 |
| Fixed viewport: `set viewport`, `set device`, CDP `Emulation.setDeviceMetricsOverride` | impossible | `Page.setScreenSizeOverride` is compiled only for non-Apple platforms; the device fixes the viewport | screenshots at the device size; `set useragent` for the user agent only. The CDP method succeeds only when it asks for the real viewport |
| PDF | impossible | WIP has no print API; `window.print()` opens native UI | `screenshot --full` |
| Downloads | impossible | WIP has no download events; Safari saves to Files | `get attr @eN href` and fetch the URL on the host |
| Video recording, screencast | impossible | iOS sends no screencast frames | `screenshot --if-changed` in a loop |
| Browser contexts, incognito, per-context state | impossible | one Safari profile; no `Target.createBrowserContext` | separate sessions; they share one cookie jar |
| Native pickers (date, color, select popup) | cannot be dismissed | native UI outside WebKit | `fill` or `select` by value |
| `beforeunload` dialogs | impossible | no protocol hook | none |
| Dynamic dialog handling (CDP `Page.handleJavaScriptDialog`) | impossible | WIP has no dialog events; a native dialog blocks evaluation | pre-armed answers: `dialog accept` or `dialog dismiss` |
| Clipboard read and write | partial, not verified | needs a user gesture and permission | `eval` with an emulated gesture |
| Geolocation, offline | partial | no WIP method | `set geo` (a page-side shim), `set offline` (interception) |
| Accessibility audit (axe) | not shipped | axe-core is MPL-2.0 and large | your own script through `eval` |
| Frames that WebKit puts in their own target (site isolation) | not supported | separate targets per frame; the iOS 26.5 Simulator kept cross-site iframes in the page's target, where `frame` works | the top-level page and frames in the page's target |
| Native HTML5 drag and drop | best effort | synthetic events do not start a native drag | `drag` dispatches the drag events with a shared `DataTransfer` and says "best effort" |
| Touch events on WebKitGTK | not available | WebKitGTK has no `Touch` constructor | `tap` and `swipe` send pointer and mouse events there; touch is checked on devices only |

Commands for impossible features exist so that agents get the reason instead of a silent failure: `set viewport` and `set device` always fail with the explanation, and unknown CDP methods are answered "wasn't found" and never forwarded.
