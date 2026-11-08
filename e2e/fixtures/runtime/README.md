# Runtime fixtures (W3a)

`breakpoint.html` and `breakpoint.js` are the page of the debugger test
e2e/webkitgtk/debugger_test.go (W1b); they have no expected snapshot.

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
