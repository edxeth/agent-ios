# Runtime fixtures (W3a)

`checkvisibility.html` holds the cases of the differential test of the CDP
endpoint's checkVisibility shim, e2e/webkitgtk/page_shims_test.go; its
expected values are WebKitGTK's native `Element.prototype.checkVisibility`.

`cdp.html` is the page of the CDP domain test e2e/webkitgtk/cdp_w5b2_test.go
(W5b-2); its expected values are hand-written in that test.

Pages for the WebKitGTK suite e2e/webkitgtk/runtime_test.go (design
docs/design/agentic-parity.md, section 7.5). Each `<name>.html` that has a
`<name>.snapshot.txt` or `<name>.interactive.txt` comes with a
hand-written expected snapshot: it was written from the rules of design 3.2
and the role and name rules of internal/runtime/js/30-aria.js, never captured
from a browser. `hidden.html` lists its expected visibility in
`hidden.expected.txt` (Playwright rules, design 3.4). `{{ORIGIN}}` in an
expected file is replaced by the fixture server's origin.

`hidden.snapshot.txt` leaves out what is not exposed: display none (also
inherited, the hidden attribute and a display-none shadow host), text and
elements with computed visibility hidden or collapse (also an element with
cursor:pointer and onclick, which would otherwise get a cursor-interactive
ref), the content of an element with content-visibility hidden, and the
content of a closed `<details>` other than its first `<summary>`. It keeps a
span that sets visibility back to visible inside a hidden parent, opacity 0,
a zero-size box, an off-screen box, display contents, and both selects with
their options. Generic containers without a ref and with at most one child
are not rendered (render_tree rule), so their text is at the top level.

`diff045-a.html` and `diff045-b.html` are the pages of the diff snapshot and
diff url tests, e2e/webkitgtk/diffstate045_test.go (agent-browser 0.45
contract); their expected diffs are hand-written in that test from the
snapshot rules above. diff045-b.html has a black background, so its
screenshot differs from diff045-a.html's.

`logpoint.html` is the page of the logpoint test e2e/webkitgtk/logpoint045_test.go:
`logpoint.js` is a plain script; `logpoint-mapped.js` has an inline source map
(data: URL) back to `src/cart.ts`, and `logpoint-mapped2.js` a map file
(`maps/logpoint-mapped2.js.map`) back to `src/basket.ts`. The mappings were
written by hand (original line 4 to generated line 2, 5 to 3, 6 to 4, 7 to 5,
9 to 7, 1-based); the test's expected locations follow from them.
