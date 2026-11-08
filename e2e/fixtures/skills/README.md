# Skill example fixtures (W6)

Pages for e2e/webkitgtk/skills_doc_test.go, which runs the bash examples of
skills/*/SKILL.md against WebKitGTK (design docs/design/agentic-parity.md,
7.9 acceptance 2). The test serves the examples' origin https://example.com
from this folder: /login is login.html, /second is second.html, /api/me
answers JSON, and / is a start page.

login.html is laid out so that the refs in the skill examples are the refs
that "snapshot -i" hands out: Email e1, Password e2, Sign in e3, and after
the sign-in the heading Account e4, the Delete draft button e5 (it calls
confirm()) and the link Next page e6. When a skill example changes, change
this page with it.
