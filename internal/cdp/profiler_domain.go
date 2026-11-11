package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Profiler (design 5.3, tier 3): enable, disable, start and stop, with the
// {profile} of stop. The profile is a WIP ScriptProfiler run with samples,
// converted by W4's devtools.ConvertSamples. The run is state of the
// attachment's owner, as a trace is (tracing_domain.go): its entry
// (core.ScriptProfilerEntry) belongs to that owner, so a detach or the end
// of the CDP connection stops it. The page has one ScriptProfiler run, so
// a start while another recording holds it (a trace with JavaScript, W4's
// "profiler start" or another attachment's profile) fails and names it.
// The V8 behavior is kept (v8-profiler-agent-impl.cc): start needs enable,
// a second start succeeds and changes nothing, disable discards a running
// profile. Profiler methods for coverage (startPreciseCoverage and the
// like) are not translated.

func init() {
	register(
		Method{Name: "Profiler.enable", Levels: OnPage, Handle: profilerEnable},
		Method{Name: "Profiler.disable", Levels: OnPage, Handle: profilerDisable},
		Method{Name: "Profiler.start", Levels: OnPage, Handle: profilerStart},
		Method{Name: "Profiler.stop", Levels: OnPage, Handle: profilerStop},
	)
}

// V8's texts.
const (
	errProfilerDisabled = "Profiler is not enabled"
	errNoProfile        = "No recording profiles found"
)

// profilerKey is the desired-state key of an attachment's profile.
func profilerKey(s *session) string { return "cdp:profiler:" + s.id }

// cdpProfile is a running profile. It takes trackingStart only after its
// startTracking was sent (armed) and trackingComplete only after that
// trackingStart, as W4's profiler does.
type cdpProfile struct {
	mu       sync.Mutex
	armed    bool
	started  bool
	complete bool
	start    float64
	end      float64
	samples  []devtools.StackSample
	done     chan struct{}
	unsub    []func()
}

func newProfile() *cdpProfile { return &cdpProfile{done: make(chan struct{})} }

func (r *cdpProfile) arm() {
	r.mu.Lock()
	r.armed, r.started = true, false
	r.mu.Unlock()
}

func (r *cdpProfile) subscribe(c webkit.Caller) {
	sub := subscribeSync(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unsub = append(r.unsub,
		sub("ScriptProfiler.trackingStart", func(_ string, params json.RawMessage) {
			var v struct {
				Timestamp float64 `json:"timestamp"`
			}
			if json.Unmarshal(params, &v) == nil {
				r.mu.Lock()
				if r.armed && !r.started {
					r.started, r.start = true, v.Timestamp
				}
				r.mu.Unlock()
			}
		}),
		sub("ScriptProfiler.trackingComplete", func(_ string, params json.RawMessage) {
			var v struct {
				Timestamp float64 `json:"timestamp"`
				Samples   *struct {
					StackTraces []devtools.StackSample `json:"stackTraces"`
				} `json:"samples"`
			}
			if json.Unmarshal(params, &v) != nil {
				return
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.started || r.complete {
				return
			}
			r.end, r.complete = v.Timestamp, true
			if v.Samples != nil {
				r.samples = v.Samples.StackTraces
			}
			close(r.done)
		}))
}

func (r *cdpProfile) stop() {
	r.mu.Lock()
	unsub := r.unsub
	r.unsub = nil
	r.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

func profilerEnable(_ context.Context, call *Call) (any, Later, error) {
	x := extrasOf(call.session)
	x.mu.Lock()
	x.profOn = true
	x.mu.Unlock()
	return map[string]any{}, nil, nil
}

// profilerDisable discards a running profile (V8 stops it without a
// result) and disables the domain. A stop that fails keeps the profile,
// so a retry or the detach stops it.
func profilerDisable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	x := extrasOf(s)
	x.mu.Lock()
	r := x.prof
	x.mu.Unlock()
	if r != nil {
		if _, err := stopProfile(ctx, call, r); err != nil {
			return nil, nil, err
		}
	}
	x.mu.Lock()
	x.profOn = false
	x.mu.Unlock()
	return map[string]any{}, nil, nil
}

func profilerStart(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	x := extrasOf(s)
	x.mu.Lock()
	switch {
	case x.prof != nil:
		x.mu.Unlock()
		return map[string]any{}, nil, nil
	case !x.profOn:
		x.mu.Unlock()
		return nil, nil, serverError(errProfilerDisabled)
	}
	r := newProfile()
	x.prof = r
	x.mu.Unlock()
	started := false
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		ps := s.pg.PS
		r.subscribe(ps.Caller)
		return ps.State.Change(ctx, mo.Op.Caller(), func(cc webkit.Caller) error {
			e := core.ScriptProfilerEntry(profilerKey(s), s.owner, r.stop)
			var err error
			_, started, err = core.StartScriptProfiler(ctx, ps, cc, e, map[string]any{"includeSamples": true}, r.arm)
			return scriptProfilerBusy(call.Method, err)
		})
	})
	switch {
	case err != nil && started:
		return nil, nil, fmt.Errorf("%w; the profiler may be running: Profiler.stop or the detach stops it", err)
	case err != nil:
		x.mu.Lock()
		if x.prof == r {
			x.prof = nil
		}
		x.mu.Unlock()
		r.stop()
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// stopProfileAdmitted, when set (tests only), runs in stopProfile's
// operation before it looks for the run's entry.
var stopProfileAdmitted func()

// stopProfile stops the run of r; the entry goes only with a stop that
// succeeded (core.StopScriptProfiler), and then r leaves the attachment.
// released reports a run that the lease's release had stopped already: the
// release ends the running operations (core.Manager.Release), so it can
// remove the entry after this operation was admitted under the lease.
func stopProfile(ctx context.Context, call *Call, r *cdpProfile) (released bool, err error) {
	s := call.session
	err = s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if stopProfileAdmitted != nil {
			stopProfileAdmitted()
		}
		ps := s.pg.PS
		if !ps.State.HasEntry(profilerKey(s)) {
			released = true // the release of the lease stopped it already
			return nil
		}
		return ps.State.Change(ctx, mo.Op.Caller(), func(cc webkit.Caller) error {
			_, err := core.StopScriptProfiler(ctx, ps, cc, profilerKey(s))
			return err
		})
	})
	if err != nil {
		return false, fmt.Errorf("%s: %w; the profiler may still be running: send it again, or the detach stops it", call.Method, err)
	}
	x := extrasOf(s)
	x.mu.Lock()
	if x.prof == r {
		x.prof = nil
	}
	x.mu.Unlock()
	r.stop()
	return released, nil
}

func profilerStop(ctx context.Context, call *Call) (any, Later, error) {
	x := extrasOf(call.session)
	x.mu.Lock()
	r := x.prof
	x.mu.Unlock()
	if r == nil {
		return nil, nil, serverError(errNoProfile)
	}
	released, err := stopProfile(ctx, call, r)
	if err != nil {
		return nil, nil, err
	}
	if released {
		return nil, nil, serverError("Profiler.stop: the profile ended with the lease of the session, so it has no result")
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("stopping the profile: WebKit sent no ScriptProfiler.trackingComplete: %w", ctx.Err())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"profile": devtools.ConvertSamples(r.samples, r.start, r.end)}, nil, nil
}
