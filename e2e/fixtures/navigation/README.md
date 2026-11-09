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
