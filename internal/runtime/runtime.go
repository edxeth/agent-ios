// Package runtime is the Go side of the agent runtime, the JavaScript that
// iwdp-mcp injects into the page (design docs/design/agentic-parity.md,
// sections 3.1 to 3.4): the bundle in js/, the call contract (expression,
// envelope, install-and-retry, async calls), the owner epoch and the
// snapshot renderer.
package runtime

import (
	"context"
	"crypto/rand"
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
	"sync"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

//go:embed js/*.js
var jsFiles embed.FS

// MissingSentinel is what a call expression returns when the runtime of
// this Version is not installed in the document.
const MissingSentinel = "__IWDP_MISSING__"

// versionPlaceholder is the string in js/10-core.js that the install
// replaces with Version: the bundle cannot contain its own hash.
const versionPlaceholder = "__IWDP_VERSION__"

// MaxSourceBytes is the size budget of the joined bundle (design 3.1).
const MaxSourceBytes = 64 << 10

var (
	source  string
	Version string // first 12 hex digits of the SHA-256 of the bundle
	// DaemonID identifies this process as a page owner (design 3.1). A
	// daemon that keeps its id across restarts may set it before the first
	// call; the default is random per process.
	DaemonID = randomID()
)

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails (crypto/rand documentation)
	return hex.EncodeToString(b)
}

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
	bundle := b.String()
	sum := sha256.Sum256([]byte(bundle))
	Version = hex.EncodeToString(sum[:])[:12]
	// The bundle's files run in name order; 10-core.js exposes the runtime
	// under construction to the later files as window.__iwdpBuilding, and
	// this last statement removes it.
	source = strings.ReplaceAll(bundle, versionPlaceholder, Version) + "try { delete window.__iwdpBuilding; } catch (e) {}\n"
}

// Source returns the runtime bundle as it is installed: the joined files
// with this Version. It is the expression of the on-demand install and the
// runtime part of the bootstrap script (design 3.1 install paths).
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

// CodeStaleOwner is the error code of a call that the page refused because
// its owner is an older lease epoch of the daemon that owns the page now
// (design 3.1: lease epochs only grow within one daemon).
const CodeStaleOwner = "stale_owner"

// ErrNotInstalled: the bundle did not define this Version's __iwdp.
var ErrNotInstalled = errors.New("the agent runtime is not installed in the page after an install attempt")

// Envelope is the result of every runtime function.
type Envelope struct {
	OK       bool            `json:"ok"`
	Value    json.RawMessage `json:"value"`
	Warnings []string        `json:"warnings,omitempty"`
	Error    *Error          `json:"error,omitempty"`
	// Reset is set on the first call after the page reset its maps for a
	// new owner (design 3.1).
	Reset *ResetInfo `json:"reset,omitempty"`
}

// Owner is the owner of the page runtime: the daemon, the logical session
// and its lease epoch (design 3.1). A call with another owner first makes
// the page run __iwdp.reset, which clears the ref maps and pending dialog
// answers.
type Owner struct {
	Daemon  string `json:"daemon"`
	Session string `json:"session"`
	Epoch   uint64 `json:"epoch"`
}

// ResetInfo describes a reset of the page maps: why (refs.ResetDaemon,
// ResetSession or ResetEpoch) and the highest ref the page had handed out.
type ResetInfo struct {
	Reason string `json:"reason"`
	MaxRef int    `json:"maxRef"`
}

// Runtime calls runtime functions in one page.
type Runtime struct {
	c           webkit.Caller
	gen         func() uint64
	*ownerState // shared with the runtimes that Via returns
}

// ownerState is what the page knows of its owner, as seen from the calls.
type ownerState struct {
	mu    sync.Mutex
	owner *Owner
	// confirmed is the owner of the last call that the page answered
	// without refusing its owner.
	confirmed *Owner
	onReset   func(ResetInfo)
}

// New returns a Runtime on c. gen returns the document generation (it may be nil).
func New(c webkit.Caller, gen func() uint64) *Runtime {
	return &Runtime{c: c, gen: gen, ownerState: &ownerState{}}
}

// Via returns a Runtime that sends its calls through c and shares the
// owner, the owner confirmation and the reset reports of r. The daemon
// gives each command the page runtime through its operation's fenced
// caller (core.Op.Caller), so its calls count against the per-page limit
// and are not sent after the operation or its lease epoch ended (design
// 2.5, 2.10).
func (r *Runtime) Via(c webkit.Caller) *Runtime {
	return &Runtime{c: c, gen: r.gen, ownerState: r.ownerState}
}

// Release runs the page-side release of a lease epoch through c (runtime
// function release, design 2.4): the page drops its ref maps and restores
// its hooks only when its current owner has this epoch, so the state of a
// later owner stays. The call carries no owner, so it never makes the page
// reset for a new owner. A document without this runtime version has
// nothing to release, so Release does not install the runtime.
func (r *Runtime) Release(ctx context.Context, c webkit.Caller, epoch uint64) error {
	expr, err := Expression("release", map[string]uint64{"epoch": epoch})
	if err != nil {
		return err
	}
	v, err := r.eval(ctx, c, "release", expr, false)
	if err != nil {
		return err
	}
	var s string
	if json.Unmarshal(v, &s) == nil && s == MissingSentinel {
		return nil
	}
	_, err = r.decodeEnvelope("release", v, nil)
	return err
}

// SetOwner sets the owner that every later call hands to the page. The
// daemon sets it when it grants the lease.
func (r *Runtime) SetOwner(o Owner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owner = &o
}

// Owner returns the owner set by SetOwner.
func (r *Runtime) Owner() (Owner, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner == nil {
		return Owner{}, false
	}
	return *r.owner, true
}

// OwnerConfirmed reports whether the page has accepted the current owner:
// no owner is set, or a call that carried it got an envelope that does not
// refuse it. Until then the page can still hold the refs of an earlier
// owner, and the reset report that drops them arrives with the next call.
func (r *Runtime) OwnerConfirmed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner == nil || (r.confirmed != nil && *r.confirmed == *r.owner)
}

// OnReset sets the function that receives the page's reset reports; the
// session's ref table uses it (refs.Table.NoteReset). A later call
// replaces it.
func (r *Runtime) OnReset(f func(ResetInfo)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onReset = f
}

// Generation returns the document generation of the page, 1 without a source.
func (r *Runtime) Generation() uint64 {
	if r.gen == nil {
		return 1
	}
	return r.gen()
}

// RuntimeCaller returns a function with the signature of
// core.SchedulerOptions.Runtime: it runs fn through the given caller (an
// operation's fenced caller) with this runtime's owner and returns the
// envelope's value.
func (r *Runtime) RuntimeCaller() func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
	return func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		var v json.RawMessage
		if _, err := r.callVia(ctx, c, fn, args, false, &v); err != nil {
			return nil, err
		}
		return v, nil
	}
}

// ownedExpression is Expression with the owner check of design 3.1 in the
// condition: __iwdp.own(owner) resets the page maps for a new owner and
// returns true.
func ownedExpression(fn string, args any, o *Owner) (string, error) {
	expr, err := Expression(fn, args)
	if err != nil || o == nil {
		return expr, err
	}
	ob, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("runtime call %s: marshaling owner: %w", fn, err)
	}
	cond := fmt.Sprintf("(window.__iwdp && window.__iwdp.v === %q)", Version)
	return strings.Replace(expr, cond, fmt.Sprintf("(window.__iwdp && window.__iwdp.v === %q && window.__iwdp.own(%s))", Version, ob), 1), nil
}

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
	_, err := r.callVia(ctx, r.c, fn, args, false, out)
	return err
}

// CallWarn is Call that also returns the envelope's warnings.
func (r *Runtime) CallWarn(ctx context.Context, fn string, args, out any) ([]string, error) {
	return r.callVia(ctx, r.c, fn, args, false, out)
}

// CallAsync runs a runtime function that returns a promise: Runtime.evaluate
// by reference, then Runtime.awaitPromise (WIP Runtime.evaluate has no
// awaitPromise parameter).
func (r *Runtime) CallAsync(ctx context.Context, fn string, args, out any) error {
	_, err := r.callVia(ctx, r.c, fn, args, true, out)
	return err
}

func (r *Runtime) callVia(ctx context.Context, c webkit.Caller, fn string, args any, async bool, out any) ([]string, error) {
	r.mu.Lock()
	owner := r.owner
	r.mu.Unlock()
	env, err := r.call(ctx, c, fn, args, async, owner)
	if err != nil {
		return nil, err
	}
	warnings, err := r.decodeEnvelope(fn, env, out)
	var re *Error
	if owner != nil && (err == nil || (errors.As(err, &re) && re.Code != CodeStaleOwner)) {
		r.mu.Lock()
		r.confirmed = owner
		r.mu.Unlock()
	}
	return warnings, err
}

func (r *Runtime) call(ctx context.Context, c webkit.Caller, fn string, args any, async bool, owner *Owner) (json.RawMessage, error) {
	expr, err := ownedExpression(fn, args, owner)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		v, err := r.eval(ctx, c, fn, expr, async)
		if err != nil {
			return nil, err
		}
		var s string
		if json.Unmarshal(v, &s) != nil || s != MissingSentinel {
			return v, nil
		}
		if attempt == 0 {
			if err := install(ctx, c); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("runtime call %s: %w", fn, ErrNotInstalled)
}

func install(ctx context.Context, c webkit.Caller) error {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": Source(), "returnByValue": true})
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

func (r *Runtime) eval(ctx context.Context, c webkit.Caller, fn, expr string, async bool) (json.RawMessage, error) {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": !async})
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
	raw, err = c.Send(ctx, "Runtime.awaitPromise", map[string]any{"promiseObjectId": res.Result.ObjectID, "returnByValue": true})
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

func (r *Runtime) decodeEnvelope(fn string, raw json.RawMessage, out any) ([]string, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("runtime call %s: result is not an envelope: %w", fn, err)
	}
	if env.Reset != nil {
		r.mu.Lock()
		f := r.onReset
		r.mu.Unlock()
		if f != nil {
			f(*env.Reset)
		}
	}
	if !env.OK {
		if env.Error == nil {
			return env.Warnings, fmt.Errorf("runtime call %s: ok=false without an error", fn)
		}
		return env.Warnings, env.Error
	}
	if out == nil || len(env.Value) == 0 {
		return env.Warnings, nil
	}
	if err := json.Unmarshal(env.Value, out); err != nil {
		return env.Warnings, fmt.Errorf("runtime call %s: decoding value: %w", fn, err)
	}
	return env.Warnings, nil
}
