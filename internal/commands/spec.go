// Package commands is the command registry (design
// docs/design/agentic-parity.md, section 2.11). Each command is described
// once; the CLI parser, help text and dispatch come from that description, so a new command is a new file whose init() calls
// Register, and --help can never run a command.
package commands

import (
	"context"
	"io"
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
	Summary   string
	Args      []Arg
	Flags     []Flag
	Page      PageNeed
	Mutates   bool // serialized per page when true; ref allocation counts as a mutation
	RetrySafe bool // only pure reads may be retried (design 2.6)
	Timeout   time.Duration
	Output    OutputKind
	// Local commands run in the client process (the daemon lifecycle);
	// every other command runs in the daemon (design 2.10).
	Local bool
	// ClientStep, when set, runs in the iwdp-cli process after parsing and
	// before the command runs or goes to the daemon (RunCLI only), under
	// the command deadline (ctx). It may change the input: a secret read
	// from the standard input here (fill --stdin, the password of set
	// credentials) reaches the daemon in the /v1/run request body, never
	// in argv. A read that may block must end when ctx ends
	// (readWithin).
	ClientStep func(ctx context.Context, in *Input, stdin io.Reader) error
	Run        func(ctx context.Context, env Env, in Input) (Result, error)
}

// Env is what a running command can use.
type Env interface {
	Page(ctx context.Context) (*core.PageSession, error) // resolves, leases, probes
	Emit(line string)                                    // streaming output (console --follow)
	ResolvePath(p string) string                         // relative to the caller's cwd
}

// RemoteEnv is an Env that runs commands in another process (the daemon,
// design 2.10). RunCLI calls RunRemote instead of Run for
// every command that is not Local.
type RemoteEnv interface {
	Env
	RunRemote(ctx context.Context, c *Command, in Input) (Result, error)
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
	Reservation       string        // --reservation, IWDP_DEVICE_RESERVATION: device reservation owner (design 2.5)
	WaitLease         time.Duration // --wait-lease: wait this long for another session's lease
	Takeover          bool          // --takeover: reattach a lost page (needs the reservation)
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
