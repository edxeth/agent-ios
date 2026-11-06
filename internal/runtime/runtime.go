// Package runtime is the Go side of the agent runtime, the JavaScript that
// iwdp-mcp injects into the page (design docs/design/agentic-parity.md,
// section 3.1). W1a provides the call contract (expression, envelope,
// install-and-retry, async calls); W3a adds the JavaScript and the
// owner-epoch handling.
package runtime

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

//go:embed js/*.js
var jsFiles embed.FS

// MissingSentinel is what a call expression returns when the runtime of
// this Version is not installed in the document.
const MissingSentinel = "__IWDP_MISSING__"

var (
	source  string
	Version string // first 12 hex digits of the SHA-256 of the bundle
)

func init() {
	names, err := fs.Glob(jsFiles, "js/*.js")
	if err != nil {
		panic(fmt.Sprintf("runtime: listing js files: %v", err))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		data, err := jsFiles.ReadFile(n)
		if err != nil {
			panic(fmt.Sprintf("runtime: reading %s: %v", n, err))
		}
		b.Write(data)
		b.WriteString("\n")
	}
	source = b.String()
	sum := sha256.Sum256([]byte(source))
	Version = hex.EncodeToString(sum[:])[:12]
}

// Source returns the joined runtime bundle.
func Source() string { return source }

var fnName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// Expression returns the call expression of design 3.1:
//
//	(window.__iwdp && window.__iwdp.v === "<ver>") ? window.__iwdp.call("<fn>", <json args>) : "__IWDP_MISSING__"
func Expression(fn string, args any) (string, error) {
	if !fnName.MatchString(fn) {
		return "", fmt.Errorf("runtime function name %q is not an identifier", fn)
	}
	if args == nil {
		args = map[string]any{}
	}
	a, err := json.Marshal(args) // escapes <, >, &, U+2028 and U+2029: valid JS
	if err != nil {
		return "", fmt.Errorf("runtime call %s: marshaling args: %w", fn, err)
	}
	return fmt.Sprintf(`(window.__iwdp && window.__iwdp.v === %q) ? window.__iwdp.call(%q, %s) : %q`, Version, fn, a, MissingSentinel), nil
}

// Error is a runtime result with ok=false.
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrNotInstalled: the bundle did not define this Version's __iwdp.
var ErrNotInstalled = errors.New("the agent runtime is not installed in the page after an install attempt")

// Envelope is the result of every runtime function.
type Envelope struct {
	OK       bool            `json:"ok"`
	Value    json.RawMessage `json:"value"`
	Warnings []string        `json:"warnings,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// Runtime calls runtime functions in one page.
type Runtime struct {
	c   webkit.Caller
	gen func() uint64
}

// New returns a Runtime on c. gen returns the document generation (it may be nil).
func New(c webkit.Caller, gen func() uint64) *Runtime { return &Runtime{c: c, gen: gen} }

type remoteObject struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype"`
	Value    json.RawMessage `json:"value"`
	ObjectID string          `json:"objectId"`
	Desc     string          `json:"description"`
}

type evalResult struct {
	Result    remoteObject `json:"result"`
	WasThrown bool         `json:"wasThrown"`
}

// Call runs a synchronous runtime function and decodes its value into out
// (out may be nil). It installs the bundle and retries once when the
// runtime is missing.
func (r *Runtime) Call(ctx context.Context, fn string, args, out any) error {
	env, err := r.call(ctx, fn, args, false)
	if err != nil {
		return err
	}
	return decodeEnvelope(fn, env, out)
}

// CallAsync runs a runtime function that returns a promise: Runtime.evaluate
// by reference, then Runtime.awaitPromise (WIP Runtime.evaluate has no
// awaitPromise parameter).
func (r *Runtime) CallAsync(ctx context.Context, fn string, args, out any) error {
	env, err := r.call(ctx, fn, args, true)
	if err != nil {
		return err
	}
	return decodeEnvelope(fn, env, out)
}

func (r *Runtime) call(ctx context.Context, fn string, args any, async bool) (json.RawMessage, error) {
	expr, err := Expression(fn, args)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		v, err := r.eval(ctx, fn, expr, async)
		if err != nil {
			return nil, err
		}
		var s string
		if json.Unmarshal(v, &s) != nil || s != MissingSentinel {
			return v, nil
		}
		if attempt == 0 {
			if err := r.install(ctx); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("runtime call %s: %w", fn, ErrNotInstalled)
}

func (r *Runtime) install(ctx context.Context) error {
	raw, err := r.c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": Source(), "returnByValue": true})
	if err != nil {
		return fmt.Errorf("installing the agent runtime: %w", err)
	}
	var res evalResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("installing the agent runtime: decoding reply: %w", err)
	}
	if res.WasThrown {
		return fmt.Errorf("installing the agent runtime: page threw %s", res.Result.Desc)
	}
	return nil
}

func (r *Runtime) eval(ctx context.Context, fn, expr string, async bool) (json.RawMessage, error) {
	raw, err := r.c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": !async})
	if err != nil {
		return nil, fmt.Errorf("runtime call %s: %w", fn, err)
	}
	var res evalResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("runtime call %s: decoding reply: %w", fn, err)
	}
	if res.WasThrown {
		return nil, fmt.Errorf("runtime call %s: page threw %s", fn, res.Result.Desc)
	}
	if !async || res.Result.ObjectID == "" {
		return res.Result.Value, nil
	}
	raw, err = r.c.Send(ctx, "Runtime.awaitPromise", map[string]any{"promiseObjectId": res.Result.ObjectID, "returnByValue": true})
	if err != nil {
		return nil, fmt.Errorf("runtime call %s: awaiting promise: %w", fn, err)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("runtime call %s: decoding awaited reply: %w", fn, err)
	}
	if res.WasThrown {
		return nil, fmt.Errorf("runtime call %s: promise rejected: %s", fn, res.Result.Desc)
	}
	return res.Result.Value, nil
}

func decodeEnvelope(fn string, raw json.RawMessage, out any) error {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("runtime call %s: result is not an envelope: %w", fn, err)
	}
	if !env.OK {
		if env.Error == nil {
			return fmt.Errorf("runtime call %s: ok=false without an error", fn)
		}
		return env.Error
	}
	if out == nil || len(env.Value) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Value, out); err != nil {
		return fmt.Errorf("runtime call %s: decoding value: %w", fn, err)
	}
	return nil
}
