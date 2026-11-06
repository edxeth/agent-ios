// Package commands is the command registry (design
// docs/design/agentic-parity.md, section 2.11). Each command is described
// once; the CLI parser, help text, MCP tool schema and dispatch come from
// that description, so a new command is a new file whose init() calls
// Register, and --help can never run a command.
package commands

import (
	"context"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// FlagType is the value type of a flag.
type FlagType int

const (
	FlagString FlagType = iota
	FlagInt
	FlagBool
	FlagDuration
	FlagEnum
	FlagPath // a file path, resolved relative to the caller's cwd
)

// Arg is a positional argument.
type Arg struct {
	Name     string
	Help     string
	Required bool
	Variadic bool // takes the rest of the arguments; only the last Arg
}

// Flag is a typed command flag.
type Flag struct {
	Name    string // without dashes, for example "delay"
	Short   string // one letter, or ""
	Type    FlagType
	Help    string
	Enum    []string // FlagEnum values
	Default string   // shown in help; parsed like a given value
}

// PageNeed says whether a command needs a page.
type PageNeed int

const (
	NoPage PageNeed = iota
	AnyPage
	LeasedPage
)

// OutputKind is how a command's result is shown.
type OutputKind int

const (
	OutputText OutputKind = iota
	OutputJSON
	OutputFile
	OutputStream
)

// Command describes one command.
type Command struct {
	Name      string   // CLI path, for example "click" or "network requests"
	Aliases   []string // for example "goto", "navigate" for "open"
	MCPName   string   // "" = not an MCP tool
	Profile   string   // MCP profile: "core", "devtools", "legacy"
	Summary   string
	Args      []Arg
	Flags     []Flag
	Page      PageNeed
	Mutates   bool // serialized per page when true; ref allocation counts as a mutation
	RetrySafe bool // only pure reads may be retried (design 2.6)
	Timeout   time.Duration
	Output    OutputKind
	Run       func(ctx context.Context, env Env, in Input) (Result, error)
}

// Env is what a running command can use.
type Env interface {
	Page(ctx context.Context) (*core.PageSession, error) // resolves, leases, probes
	Emit(line string)                                    // streaming output (console --follow)
	ResolvePath(p string) string                         // relative to the caller's cwd
}

// Globals are the global flags, parsed once for every command.
type Globals struct {
	Session           string        // --session, IWDP_SESSION; default "default" (D8)
	Page              string        // --page
	Timeout           time.Duration // --timeout; 0: the command's default
	JSON              bool          // --json
	Transport         string        // --transport auto|iwdp|direct|webkitgtk
	MaxOutput         int           // --max-output, in bytes; 0: no limit
	ContentBoundaries bool          // --content-boundaries
}

// Input is a parsed invocation of a command.
type Input struct {
	Args   []string
	Flags  map[string]any // by flag name: string, int, bool or time.Duration
	Global Globals
}

// Result is what a command returns. Text is printed; Data is printed as
// JSON with --json (Text when Data is nil).
type Result struct {
	Text string
	Data any
}
