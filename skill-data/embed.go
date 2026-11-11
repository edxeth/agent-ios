// Package skilldata holds the agent skill guides of this repository as
// files in the binary. Each guide is one file, skill-data/<name>/SKILL.md,
// with a front matter that gives its name and description; internal/skills
// reads them and "iwdp-cli skills" prints them (design
// docs/design/agentic-parity.md, sections 4.2 and 7.9), so an installed
// binary always prints the guides that match its commands. The discovery
// stubs in skills/<name>/SKILL.md, which agents find, are not embedded:
// they only point at "iwdp-cli skills get <name>".
package skilldata

import "embed"

// FS holds every skill-data/<name>/SKILL.md of the repository.
//
//go:embed */SKILL.md
var FS embed.FS
