// Package skills holds the agent skills of this repository as files in
// the binary. Each skill is one file, skills/<name>/SKILL.md, with a front
// matter that gives its name and description; internal/skills reads them
// and "iwdp-cli skills" prints them (design docs/design/agentic-parity.md,
// sections 4.2 and 7.9), so an installed binary always prints the skills
// that match its commands.
package skills

import "embed"

// FS holds every skills/<name>/SKILL.md of the repository.
//
//go:embed */SKILL.md
var FS embed.FS
