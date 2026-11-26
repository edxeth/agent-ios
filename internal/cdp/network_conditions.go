package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/wip"
)

// Network conditions (parity 0.45; chrome-devtools-mcp 1.14.0 emulate,
// puppeteer-core 25.18.1 NetworkManager #applyNetworkConditions):
// Puppeteer sends Network.overrideNetworkState, then
// Network.emulateNetworkConditionsByRule with one global rule, on every
// emulateNetworkConditions call, also to clear (offline false, latency 0,
// throughput -1); older clients send Network.emulateNetworkConditions. All
// three come down to netConditions, the attachment's state:
//   - Throttling is WIP Network.setEmulatedConditions, owned by the
//     attachment's owner. Its parameters differ by iOS version (command
//     lists, internal/wip): 16.4 to 17.x take bytesPerSecondLimit (a
//     download limit only); 18.0 to 27.2 lack the method; 27.3 takes
//     bandwidth and latency (WebKit commit c4a7ebcc295f). An upload limit
//     cannot be emulated anywhere. A throttle that the device cannot
//     emulate is refused before any change; the unthrottled values change
//     nothing unless this attachment set a throttle, which they clear, so
//     a client's clear works on every iOS version.
//   - Offline is emulated through interception, as agent-ios set offline
//     does (internal/devtools intercept.go SetOffline): one WIP rule for
//     every URL at the request stage, owned by the attachment's owner, and
//     every request that it pauses fails with WIP's General error. A
//     request that a CLI interception handler of the session (network
//     route, set offline, set credentials) claims is left to that handler.
//     A request that a Fetch rule of a CDP attachment also pauses is
//     reported to that client too; its answer then gets "Invalid
//     InterceptionId.".
//   - Network.overrideNetworkState only changes navigator.onLine and
//     navigator.connection in Chrome; the bridge does not emulate them, so
//     it has no effect.
//   - Network.disable drops the owner's conditions and rules (core
//     dropOwner), as Chrome's Network.disable resets them; the detach
//     releases them with the owner.

func init() {
	register(
		Method{Name: "Network.emulateNetworkConditions", Levels: OnPage, Handle: emulateNetworkConditions},
		Method{Name: "Network.emulateNetworkConditionsByRule", Levels: OnPage, Handle: emulateNetworkConditionsByRule},
		Method{Name: "Network.overrideNetworkState", Levels: OnPage, Handle: overrideNetworkState},
	)
	onPageEvent(evRequestIntercepted, offlineIntercepted)
}

// netConditions are CDP network conditions: latency in milliseconds,
// throughputs in bytes per second (0 or less: no limit).
type netConditions struct {
	offline                   bool
	latency, download, upload float64
}

// condState is the attachment's network conditions; emuState.mu guards it
// but offline, which the event translation reads.
type condState struct {
	throttled bool // the attachment set a WIP throttle
	ruleOn    bool // WebKit confirmed the attachment's offline rule
}

// offlineRule is the WIP rule of the attachment's offline emulation:
// every URL, request stage, made unique by the attachment's id (globRegex).
func (s *session) offlineRule() core.InterceptRule {
	return core.InterceptRule{URL: globRegex("*", "offline-"+s.id[:8]), Stage: stageRequest, CaseSensitive: true, IsRegex: true}
}

// bandwidthNames reports whether the WIP Network.setEmulatedConditions of
// iOS osVersion takes bandwidth and latency (27.3 and later; also an
// unknown version, such as WebKitGTK's, whose WebKit is current) rather
// than bytesPerSecondLimit (16.4 to 17.x).
func bandwidthNames(osVersion string) bool {
	parts := strings.Split(osVersion, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return true
	}
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major > 27 || major == 27 && minor >= 3
}

// throttleParams returns the WIP Network.setEmulatedConditions parameters
// of c on iOS osVersion (nil: no throttle), or the reason the device
// cannot emulate them.
func throttleParams(method, osVersion string, c netConditions) (map[string]any, error) {
	if c.upload > 0 {
		return nil, serverError("%s: upload throttling cannot be emulated on iOS WebKit (uploadThroughput %v)", method, c.upload)
	}
	if c.download <= 0 && c.latency <= 0 {
		return nil, nil
	}
	if known, ok := wip.Supported(osVersion, "Network.setEmulatedConditions"); known && !ok {
		return nil, serverError("%s: network conditions cannot be emulated on iOS %s: Network.setEmulatedConditions is not in its command list (it is in 16.4 to 17.x and from 27.3)", method, osVersion)
	}
	if !bandwidthNames(osVersion) {
		if c.latency > 0 {
			return nil, serverError("%s: latency cannot be emulated on iOS %s (latency %v ms); only downloadThroughput is supported before iOS 27.3", method, osVersion, c.latency)
		}
		return map[string]any{"bytesPerSecondLimit": int64(c.download)}, nil
	}
	p := map[string]any{}
	if c.download > 0 {
		p["bandwidth"] = int64(c.download)
	}
	if c.latency > 0 {
		p["latency"] = int64(c.latency)
	}
	return p, nil
}

// applyConditions makes c the attachment's network conditions. What the
// device cannot emulate is refused before any change.
func (s *session) applyConditions(ctx context.Context, method string, c netConditions) error {
	params, err := throttleParams(method, s.t.osVersion, c)
	if err != nil {
		return err
	}
	return s.do(ctx, method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		st := &s.emu
		st.mu.Lock()
		defer st.mu.Unlock()
		if err := s.setOfflineLocked(ctx, mo, c.offline); err != nil {
			return err
		}
		switch {
		case params != nil:
			if err := s.owned(ctx, mo, "Network.setEmulatedConditions", params); err != nil {
				return err
			}
			st.net.throttled = true
		case st.net.throttled:
			if err := s.owned(ctx, mo, "Network.setEmulatedConditions", map[string]any{}); err != nil {
				return err
			}
			st.net.throttled = false
		}
		return nil
	})
}

// setOfflineLocked turns the attachment's offline emulation on or off;
// s.emu.mu is held. The flag is on before the rule is installed, so that
// the first request it pauses fails, and it stays on while the rule may
// be in place (a failed removal), so that no paused request is left
// without an answer.
func (s *session) setOfflineLocked(ctx context.Context, mo *core.ManagedOp, on bool) error {
	st := &s.emu
	if on {
		st.offline.Store(true)
		if st.net.ruleOn {
			return nil
		}
		if err := s.owned(ctx, mo, "Network.setInterceptionEnabled", map[string]bool{"enabled": true}); err != nil {
			return err
		}
		if err := s.owned(ctx, mo, "Network.addInterception", s.offlineRule()); err != nil {
			return err
		}
		st.net.ruleOn = true
		return nil
	}
	if st.net.ruleOn {
		// A target that does not hold the rule refuses its removal; the
		// rule is gone, as wanted.
		if err := s.owned(ctx, mo, "Network.removeInterception", s.offlineRule()); err != nil && !core.MissingInterceptRule(err) {
			return err
		}
		st.net.ruleOn = false
		if err := s.owned(ctx, mo, "Network.setInterceptionEnabled", map[string]bool{"enabled": false}); err != nil {
			st.offline.Store(false)
			return err
		}
	}
	st.offline.Store(false)
	return nil
}

// dropConditions forgets the attachment's conditions after Network.disable
// dropped them (core dropOwner).
func (s *session) dropConditions() {
	st := &s.emu
	st.mu.Lock()
	defer st.mu.Unlock()
	st.net = condState{}
	st.offline.Store(false)
}

// emulateNetworkConditions is Chrome's deprecated method.
func emulateNetworkConditions(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Offline            bool    `json:"offline"`
		Latency            float64 `json:"latency"`
		DownloadThroughput float64 `json:"downloadThroughput"`
		UploadThroughput   float64 `json:"uploadThroughput"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	err := call.session.applyConditions(ctx, call.Method, netConditions{p.Offline, p.Latency, p.DownloadThroughput, p.UploadThroughput})
	return nil, nil, err
}

// cdpNetworkConditions is CDP's Network.NetworkConditions.
type cdpNetworkConditions struct {
	URLPattern         string  `json:"urlPattern"`
	Offline            bool    `json:"offline"`
	Latency            float64 `json:"latency"`
	DownloadThroughput float64 `json:"downloadThroughput"`
	UploadThroughput   float64 `json:"uploadThroughput"`
}

// emulateNetworkConditionsByRule applies the first global rule (empty
// urlPattern; none: no conditions) and the deprecated top-level offline.
// A rule for some URLs only cannot be emulated unless it changes nothing
// (no offline, no limit). The rule ids are "1", "2" and so on.
func emulateNetworkConditionsByRule(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Offline                     *bool                  `json:"offline"`
		EmulateOfflineServiceWorker bool                   `json:"emulateOfflineServiceWorker"`
		MatchedNetworkConditions    []cdpNetworkConditions `json:"matchedNetworkConditions"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.EmulateOfflineServiceWorker {
		return nil, nil, serverError("%s: offline service workers cannot be emulated on iOS WebKit", call.Method)
	}
	var c netConditions
	global := false
	ids := []string{}
	for i, r := range p.MatchedNetworkConditions {
		ids = append(ids, strconv.Itoa(i+1))
		limited := r.Offline || r.Latency > 0 || r.DownloadThroughput > 0 || r.UploadThroughput > 0
		switch {
		case r.URLPattern != "" && limited:
			return nil, nil, serverError("%s: conditions for some URLs only (urlPattern %q) cannot be emulated on iOS WebKit; use an empty urlPattern for all requests", call.Method, r.URLPattern)
		case r.URLPattern == "" && !global:
			global = true
			c = netConditions{r.Offline, r.Latency, r.DownloadThroughput, r.UploadThroughput}
		}
	}
	if p.Offline != nil && *p.Offline {
		c.offline = true
	}
	if err := call.session.applyConditions(ctx, call.Method, c); err != nil {
		return nil, nil, err
	}
	return map[string]any{"ruleIds": ids}, nil, nil
}

// overrideNetworkState has no effect: navigator.onLine and
// navigator.connection are not emulated (requests follow the conditions
// of the other two methods).
func overrideNetworkState(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		Offline            bool    `json:"offline"`
		Latency            float64 `json:"latency"`
		DownloadThroughput float64 `json:"downloadThroughput"`
		UploadThroughput   float64 `json:"uploadThroughput"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// offlineIntercepted fails a request that the attachment's offline rule
// paused, unless a CLI interception handler of the session answers it.
// It runs after fetchIntercepted (file order), under ps.mu.
func offlineIntercepted(ps *pageState, target string, params json.RawMessage) {
	if ps.s == nil || !ps.s.emu.offline.Load() {
		return
	}
	var e interceptedReq
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" || e.Request == nil {
		return // a response-stage event: the offline rule pauses requests only
	}
	k := netKey{target, e.RequestID}
	if ps.net.gapAt != 0 && !ps.holdOpen(k, stageRequest, ps.fetch.hold) {
		return // a reset that comes after this event continued its hold
	}
	ps.failOfflineSoon(target, e.RequestID, e.Request.URL)
}

// failOfflineSoon fails the paused request id of target as offline mode
// does, through the control lane, unless a CLI handler claims its URL.
func (ps *pageState) failOfflineSoon(target, id, rawURL string) {
	if handler, _ := ps.otherRules(stageRequest, rawURL); handler {
		return
	}
	s := ps.s
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), core.DefaultControlBudget)
		defer cancel()
		err := s.control(func() error {
			_, err := s.pg.PS.Sched.Control(ctx, core.ControlMsg{
				Kind: core.ControlCompleteRequest, Session: s.c.rec.session, Epoch: s.epoch,
				RequestID: id, TargetID: target, Method: "Network.interceptRequestWithError", Params: map[string]any{"errorType": "General"},
			})
			return err
		})
		if err != nil && !errors.Is(err, core.ErrControlRefused) && !errors.Is(err, errDetached) {
			ps.logf("cdp: page %s: failing request %s of target %q offline: %v", logsafe.ID(ps.target), logsafe.ID(id), logsafe.ID(target), logsafe.Error(err))
		}
	}()
}
