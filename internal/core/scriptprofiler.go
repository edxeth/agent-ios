package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// The ScriptProfiler lifecycle of a page (W4 rebase review findings F2 and
// F3). WebKit runs at most one ScriptProfiler run per page
// (InspectorScriptProfilerAgent): startTracking while the agent tracks
// starts nothing (no trackingStart; its includeSamples is ignored), and a
// run survives a same-target navigation. Every recording that uses the
// run, whether a W4 profiler, a W4 trace with script tracking or a legacy
// ScriptProfiler.startTracking (Manager.Raw), is one desired-state entry of
// the ScriptProfiler domain. That entry is the record of the run:
//
//   - Its owner holds the run. A start of another recording is refused
//     while it exists (ScriptProfilerBusyError); only a run that no entry
//     holds is abandoned, and a start stops it and starts again.
//   - Its Release sends ScriptProfiler.stopTracking. A release that cannot
//     run it, or whose stop fails, marks the agent unknown
//     (release_lifecycle.go), and core's reset (agentResetCalls) stops the
//     run later; every start runs that pending reset first.
//   - A stop removes it only after stopTracking succeeded. A stop that
//     failed keeps it, so a retry or the release stops the run.

// scriptProfilerDomain is the Entry.Domain of the ScriptProfiler records.
const scriptProfilerDomain = "ScriptProfiler"

// ScriptProfilerBusyError refuses a ScriptProfiler start while another
// recording holds the page's run: the entry Key of Owner.
type ScriptProfilerBusyError struct {
	Key   string
	Owner Owner
}

func (e *ScriptProfilerBusyError) Error() string {
	return fmt.Sprintf("the page's ScriptProfiler runs one recording at a time, and %s holds it (%s)", e.Owner, e.Key)
}

// ScriptProfilerEntry returns the entry that records a recording's run:
// key and owner name it, and onRelease (optional) drops the recording's
// local state when its owner releases it. The Release always sends
// ScriptProfiler.stopTracking (a no-op while the agent does not track).
func ScriptProfilerEntry(key string, owner Owner, onRelease func()) Entry {
	return Entry{
		Key: key, Owner: owner, Domain: scriptProfilerDomain,
		Apply: func(context.Context, Sender) error { return nil }, // a new target gets no run
		Release: func(ctx context.Context, s Sender) error {
			if onRelease != nil {
				onRelease()
			}
			_, err := s.Send(ctx, "ScriptProfiler.stopTracking", nil)
			return err
		},
	}
}

// DomainEntry returns the entry of domain whose key sorts first. A
// recording that a page runs once, such as the ScriptProfiler run or a
// Timeline recording (W4's "trace start", CDP's Tracing.start), is one
// entry of its agent's domain, so this entry names the recording that
// holds the agent.
func (d *DesiredState) DomainEntry(domain string) (Entry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for k, e := range d.entries {
		if e.Domain == domain {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return Entry{}, false
	}
	sort.Strings(keys)
	return d.entries[keys[0]], true
}

// scriptProfilerHolder returns the entry that holds the page's run.
func (d *DesiredState) scriptProfilerHolder() (Entry, bool) {
	return d.DomainEntry(scriptProfilerDomain)
}

// activeCaller is the caller that a change's caller c sends through: the
// recording's start and stop go to the active target only (a new target
// gets no run), so they are not repeated on provisional targets.
func activeCaller(c webkit.Caller) webkit.Caller {
	if cs, ok := c.(*changeSender); ok {
		return cs.Caller
	}
	return c
}

// StartScriptProfiler starts the run of the recording e (made by
// ScriptProfilerEntry) on the page's active target, inside a change whose
// caller is c. arm (optional) runs before each startTracking, so that the
// recording takes the events of its own run only. In order:
//
//  1. Another recording's entry refuses the start (ScriptProfilerBusyError,
//     not sent). A second start of e itself is sent as it is: it joins e's
//     own run, as WebKit answers it.
//  2. A stop that a release left pending runs first (ResetUnknownAgent).
//  3. e is recorded before the start is sent.
//  4. A start without trackingStart found a run that no recording holds:
//     that run is stopped and the start sent again; without trackingStart
//     then, the start fails.
//
// sent reports whether the page may track for e after an error; when it
// may not, e is removed again.
func StartScriptProfiler(ctx context.Context, ps *PageSession, c webkit.Caller, e Entry, params map[string]any, arm func()) (res json.RawMessage, sent bool, err error) {
	d := ps.State
	h, held := d.scriptProfilerHolder()
	if held && h.Key != e.Key {
		return nil, false, notSent{&ScriptProfilerBusyError{Key: h.Key, Owner: h.Owner}}
	}
	ac := activeCaller(c)
	if held {
		res, err = ac.Send(ctx, "ScriptProfiler.startTracking", params)
		return res, true, err
	}
	if err := ResetUnknownAgent(ctx, ps, c, scriptProfilerDomain); err != nil {
		return nil, false, fmt.Errorf("stopping the ScriptProfiler run whose stop a release left pending: %w", err)
	}
	d.Put(e)
	if arm == nil {
		arm = func() {}
	}
	fresh, unsub := trackingStarts(ps.Caller)
	defer unsub()
	start := func() (json.RawMessage, error) {
		arm()
		fresh.reset()
		return ac.Send(ctx, "ScriptProfiler.startTracking", params)
	}
	defer func() {
		if err != nil && !sent {
			d.Remove(e.Key)
		}
	}()
	if res, err = start(); err != nil {
		return nil, !noEffect(err), fmt.Errorf("ScriptProfiler.startTracking: %w", err)
	}
	if fresh.started() {
		return res, true, nil
	}
	if _, err := ac.Send(ctx, "ScriptProfiler.stopTracking", nil); err != nil {
		return nil, true, fmt.Errorf("ScriptProfiler.startTracking started no new run, because a run that no recording holds was tracking, and stopping that run failed (ScriptProfiler.stopTracking): %w", err)
	}
	if res, err = start(); err != nil {
		return nil, !noEffect(err), fmt.Errorf("ScriptProfiler.startTracking after stopping a run that no recording held: %w", err)
	}
	if !fresh.started() {
		return nil, true, errors.New("ScriptProfiler.startTracking started no new run (no ScriptProfiler.trackingStart), also after the run that was tracking was stopped")
	}
	return res, true, nil
}

// StopScriptProfiler stops the run of the recording key on the page's
// active target, inside a change whose caller is c. The entry is removed
// only after stopTracking succeeded; a stop that failed or whose outcome
// is unknown keeps it, so that a retry or the release of its owner stops
// the run. A stop while another recording holds the run is refused (not
// sent); without any holder it is sent as it is (it ends an abandoned run).
func StopScriptProfiler(ctx context.Context, ps *PageSession, c webkit.Caller, key string) (json.RawMessage, error) {
	d := ps.State
	if h, held := d.scriptProfilerHolder(); held && h.Key != key {
		return nil, notSent{&ScriptProfilerBusyError{Key: h.Key, Owner: h.Owner}}
	}
	res, err := activeCaller(c).Send(ctx, "ScriptProfiler.stopTracking", nil)
	if err != nil {
		return nil, fmt.Errorf("ScriptProfiler.stopTracking: %w", err)
	}
	d.Remove(key)
	return res, nil
}

// legacyScriptProfilerKey is the entry of a legacy owner's run.
func legacyScriptProfilerKey(owner Owner) string { return "legacy:scriptprofiler:" + string(owner) }

// legacyScriptProfiler runs a legacy ScriptProfiler.startTracking or
// stopTracking (Manager.Raw) through the page's ScriptProfiler lifecycle,
// inside the change of legacyCall.
func legacyScriptProfiler(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (json.RawMessage, error) {
	key := legacyScriptProfilerKey(owner)
	if method == "ScriptProfiler.stopTracking" {
		return StopScriptProfiler(ctx, ps, c, key)
	}
	var p map[string]any
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%s: bad params: %w", method, err)
		}
	}
	res, _, err := StartScriptProfiler(ctx, ps, c, ScriptProfilerEntry(key, owner, nil), p, nil)
	var busy *ScriptProfilerBusyError
	if errors.As(err, &busy) {
		return nil, notSent{fmt.Errorf("%s: %w; stop that recording first ('profiler stop', 'trace stop', or ScriptProfiler.stopTracking of its session)", method, err)}
	}
	return res, err
}

// trackingStartSeen notes whether a trackingStart arrived since reset.
type trackingStartSeen struct {
	seen  atomic.Bool
	fresh bool // no read-loop subscription: every start counts as fresh
}

func (s *trackingStartSeen) reset() { s.seen.Store(false) }

func (s *trackingStartSeen) started() bool { return s.fresh || s.seen.Load() }

// trackingStarts subscribes to ScriptProfiler.trackingStart in the read
// loop of the page connection pc, so that the event, which WebKit sends
// before the reply to startTracking, is seen when the reply arrives. A
// connection without read-loop subscriptions counts every start as fresh.
func trackingStarts(pc webkit.Caller) (*trackingStartSeen, func()) {
	s := &trackingStartSeen{}
	ss, ok := pc.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	})
	if !ok {
		s.fresh = true
		return s, func() {}
	}
	return s, ss.SubscribeSync("ScriptProfiler.trackingStart", func(string, json.RawMessage) { s.seen.Store(true) })
}
