package cdp

// The Audits domain (design docs/design/agentic-parity.md, sections 5.3
// and 5.4). Puppeteer 25.16.0 sends Audits.enable on every attach (issues
// are on by default) and fails the attach when it is unknown (proof P2,
// docs/notes/p2.md item 3). WebKit has no issues to report, so enable is a
// no-effect success and no Audits.issueAdded event is ever sent.

func init() {
	register(Method{Name: "Audits.enable", Levels: OnPage})
	registerEvents("Audits")
}
