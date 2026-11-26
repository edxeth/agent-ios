# Navigation fixtures (W3b-2a)

Pages of e2e/webkitgtk/navigation_test.go (design docs/design/agentic-parity.md,
sections 3.6 to 3.9 and 7.6 items 6 to 11). The test server serves this
directory on two sites (127.0.0.1 and localhost), so a navigation between
them is a real cross-process swap, and answers GET /record (one dialog
result of one document), GET /records (all of them, as JSON) and GET
/nocontent (204 No Content, a navigation that WebKit abandons).

frames.html holds three iframes: #same (second.html, same origin), #other
(child.html of the site given by ?other=, cross-origin) and #kid (child.html,
same origin). child.html counts the clicks on its button #press.

frames045.html (e2e/webkitgtk/frames045_test.go) holds an unnamed
same-origin iframe (title "Outer", outer045.html, which nests the other
site's child045.html), an unnamed cross-origin iframe (.xo, child045.html of
the ?other= site), and two frames of scroll045.html at known offsets:
#scrolled (left 37, top 253, border 5, padding 7) and #scaled (left 400, top
253, border 3, padding 2, transform scale(2) from its top left corner).
scroll045.html has the button #inside at left 20, top 400, 100x30.
child045.html has a checkbox, a button that adds "Late text" after 300 ms,
and a button that logs to the console and throws an uncaught error.
