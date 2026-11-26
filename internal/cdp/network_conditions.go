package cdp

import (
	"context"
	"errors"
	"strconv"

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
//     emulate is refused before any change. Core's condition owners
//     (core DesiredState.SetEmulatedConditions) are the record of what
//     WebKit holds: a refused send leaves the owner's record as it was,
//     so the unthrottled values go to core every time and reach WebKit
//     only when they change the effective conditions, and a client's
//     clear works on every iOS version.
//   - Offline is emulated through interception, as agent-ios set offline
//     does (internal/devtools intercept.go SetOffline): one WIP rule for
//     every URL at the request stage, owned by the attachment's owner.
//     Its lifecycle is offlinePhase, one state machine per attachment,
//     registered on the page (fetchClaims) so that the read loop decides
//     each request-stage interception event once for every attachment
//     (offlineDecision): while offline is on, the request fails (WIP's
//     General error), and no Fetch client sees it; while offline is being
//     set up or torn down, a Fetch client that claims the request reports
//     it, and a request that no Fetch client claims is answered by the
//     offline lifecycle (failed once the setup succeeded, else continued).
//     A request that a CLI interception handler of the session (network
//     route, set offline, set credentials) claims is left to that handler.
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
}

// netConditions are CDP network conditions: latency in milliseconds,
// throughputs in bytes per second (0 or less: no limit).
type netConditions struct {
	offline                   bool
	latency, download, upload float64
}

// normal returns c with every "no limit" value as 0, so that two
// conditions with the same effect are equal.
func (c netConditions) normal() netConditions {
	pos := func(v float64) float64 { return max(v, 0) }
	return netConditions{c.offline, pos(c.latency), pos(c.download), pos(c.upload)}
}

// offlinePhase is the lifecycle of an attachment's offline emulation.
// Transitions (setOfflineLocked, dropConditions, pageState.close):
//
//	off -> settingUp      offline on: interception and the rule are sent
//	settingUp -> on       WebKit confirmed the rule
//	settingUp -> off      WebKit refused a setup call (nothing installed)
//	settingUp -> tearingDown  the rule's outcome is unknown
//	on -> tearingDown     offline off, or the attachment closed
//	tearingDown -> off    the rule is gone (removed, or WebKit lacks it)
//	any -> off            Network.disable dropped the owner's rules, or
//	                      the detach released them (unsubscribe)
//
// A tearingDown that cannot remove its rule stays: the requests that the
// rule may still pause are continued until a retry or the release.
type offlinePhase int

const (
	offlineOff offlinePhase = iota
	offlineSettingUp
	offlineOn
	offlineTearingDown
)

// ruleState is what the attachment knows of its offline rule in WebKit.
type ruleState int

const (
	ruleNone    ruleState = iota // not installed
	ruleOn                       // WebKit confirmed it
	ruleUnknown                  // a call about it had an unknown outcome
)

// condState is the attachment's network conditions; emuState.mu guards it.
type condState struct {
	phase offlinePhase
	rule  ruleState
	setup *offlineSetup // the current or last setup (phase settingUp)
}

// offlineSetup is one offline setup: done is closed when it ends.
type offlineSetup struct{ done chan struct{} }

// offlineRule is the WIP rule of the attachment's offline emulation:
// every URL, request stage, made unique by the attachment's id (globRegex).
func (s *session) offlineRule() core.InterceptRule {
	return core.InterceptRule{URL: globRegex("*", "offline-"+s.id[:8]), Stage: stageRequest, CaseSensitive: true, IsRegex: true}
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
	if !wip.EmulatedConditionsBandwidth(osVersion) {
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
	clearOnly := params == nil
	if clearOnly {
		params = map[string]any{} // the attachment's throttle, if any, ends (core sends only a change)
	}
	if known, ok := wip.Supported(s.t.osVersion, "Network.setEmulatedConditions"); clearOnly && known && !ok {
		params = nil // no throttle can be set on this iOS version: nothing to clear
	}
	return s.do(ctx, method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		st := &s.emu
		st.mu.Lock()
		defer st.mu.Unlock()
		if err := s.setOfflineLocked(ctx, mo, c.offline); err != nil {
			return err
		}
		if params == nil {
			return nil
		}
		return s.owned(ctx, mo, "Network.setEmulatedConditions", params)
	})
}

// setOfflinePhaseLocked moves the attachment's offline lifecycle to ph and
// publishes it on the page (fetchClaims); s.emu.mu is held.
func (s *session) setOfflinePhaseLocked(ph offlinePhase) {
	st := &s.emu.net
	if st.phase == offlineSettingUp && ph != offlineSettingUp {
		close(st.setup.done)
	}
	if ph == offlineSettingUp && st.phase != offlineSettingUp {
		st.setup = &offlineSetup{done: make(chan struct{})}
	}
	st.phase = ph
	s.c.s.claims.setOffline(s, ph, st.setup)
}

// removeOfflineRuleLocked removes the attachment's offline rule when it
// may be installed; s.emu.mu is held. A target that does not hold the
// rule refuses its removal; the rule is gone, as wanted.
func (s *session) removeOfflineRuleLocked(ctx context.Context, mo *core.ManagedOp) error {
	st := &s.emu.net
	if st.rule == ruleNone {
		return nil
	}
	err := s.owned(ctx, mo, "Network.removeInterception", s.offlineRule())
	switch {
	case err == nil || core.MissingInterceptRule(err):
		st.rule = ruleNone
		return nil
	case !wipNoEffect(err):
		st.rule = ruleUnknown
	}
	return err
}

// setOfflineLocked turns the attachment's offline emulation on or off
// (offlinePhase); s.emu.mu is held.
func (s *session) setOfflineLocked(ctx context.Context, mo *core.ManagedOp, on bool) error {
	st := &s.emu.net
	if on {
		if st.phase == offlineOn {
			return nil
		}
		// A rule left by a teardown that failed goes first: WebKit
		// refuses a second rule with the same text.
		if err := s.removeOfflineRuleLocked(ctx, mo); err != nil {
			return err
		}
		s.setOfflinePhaseLocked(offlineSettingUp)
		err := s.owned(ctx, mo, "Network.setInterceptionEnabled", map[string]bool{"enabled": true})
		if err == nil {
			err = s.owned(ctx, mo, "Network.addInterception", s.offlineRule())
			switch {
			case err == nil:
				st.rule = ruleOn
				s.setOfflinePhaseLocked(offlineOn)
				return nil
			case !wipNoEffect(err):
				st.rule = ruleUnknown
				s.setOfflinePhaseLocked(offlineTearingDown) // a later call or the release removes it
				return err
			}
		}
		// Nothing was installed: the owner gives back the interception
		// it enabled (core sends the disable only when no other owner
		// needs interception).
		if off := s.owned(ctx, mo, "Network.setInterceptionEnabled", map[string]bool{"enabled": false}); off != nil {
			err = errors.Join(err, off)
		}
		s.setOfflinePhaseLocked(offlineOff)
		return err
	}
	if st.phase == offlineOff {
		return nil
	}
	s.setOfflinePhaseLocked(offlineTearingDown)
	if err := s.removeOfflineRuleLocked(ctx, mo); err != nil {
		return err
	}
	s.setOfflinePhaseLocked(offlineOff) // the rule is gone; core's record of interception ends with the call below or the release
	return s.owned(ctx, mo, "Network.setInterceptionEnabled", map[string]bool{"enabled": false})
}

// dropConditions forgets the attachment's conditions after Network.disable
// dropped them (core dropOwner).
func (s *session) dropConditions() {
	st := &s.emu
	st.mu.Lock()
	defer st.mu.Unlock()
	st.net.rule = ruleNone
	s.setOfflinePhaseLocked(offlineOff)
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

// emulateNetworkConditionsByRule applies the rules with CDP's first-match
// semantics where WIP can express them: WIP's conditions are global, so
// every URL must get the same conditions. That holds when each rule before
// the first global one (empty urlPattern) has that global rule's
// conditions (none without a global rule); the rules after it are never
// reached. Other rules are refused before any change. The deprecated
// top-level offline adds offline. The rule ids are "1", "2" and so on.
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
	ids := []string{}
	first := len(p.MatchedNetworkConditions) // the first global rule
	for i, r := range p.MatchedNetworkConditions {
		ids = append(ids, strconv.Itoa(i+1))
		if r.URLPattern == "" && first == len(p.MatchedNetworkConditions) {
			first = i
			c = netConditions{r.Offline, r.Latency, r.DownloadThroughput, r.UploadThroughput}
		}
	}
	for _, r := range p.MatchedNetworkConditions[:first] {
		if (netConditions{r.Offline, r.Latency, r.DownloadThroughput, r.UploadThroughput}).normal() != c.normal() {
			return nil, nil, serverError("%s: conditions for some URLs only (urlPattern %q) cannot be emulated on iOS WebKit; use an empty urlPattern for all requests", call.Method, r.URLPattern)
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

// offlineAct is what the offline lifecycle does with a request whose
// interception event it decided (claimDecision.offline).
type offlineAct int

const (
	offlineFail     offlineAct = iota + 1 // offline is on: fail it
	offlineContinue                       // offline is ending: continue it
	offlineAwait                          // offline is being set up: fail it once the setup succeeded, else continue it
)

// offlineDecisionLocked decides a request-stage interception event for the
// offline lifecycles of the page's attachments, after its Fetch claimants
// were found; fetchClaims.mu is held. An attachment whose offline is on
// takes the request from every Fetch claimant; one that is being set up
// or torn down takes it only when no Fetch client claims it. The choice
// among attachments does not depend on map order.
func (p *pageClaims) offlineDecisionLocked(d *claimDecision) {
	var best *session
	var bestPh offlinePhase
	rank := map[offlinePhase]int{offlineOn: 3, offlineSettingUp: 2, offlineTearingDown: 1}
	for s, o := range p.offline {
		ph := o.phaseNow()
		if rank[ph] == 0 {
			continue
		}
		if best == nil || rank[ph] > rank[bestPh] || rank[ph] == rank[bestPh] && s.id < best.id {
			best, bestPh = s, ph
		}
	}
	switch {
	case best == nil:
		return
	case bestPh == offlineOn:
		clear(d.claimants)
		d.offline, d.act = best, offlineFail
	case len(d.claimants) > 0:
		return
	case bestPh == offlineSettingUp:
		d.offline, d.act, d.setup = best, offlineAwait, p.offline[best].setup
	default:
		d.offline, d.act = best, offlineContinue
	}
}

// resolveOffline answers the request id of target (rawURL) that the
// decision d gave to s's offline lifecycle, on the control lane, unless
// a CLI handler of the session claims it. A failure that the detached
// attachment can no longer send becomes a continue, so the request is
// never left paused.
func (s *session) resolveOffline(d *claimDecision, target, id, rawURL string) {
	act := d.act
	if act == offlineAwait {
		wait, stop := context.WithTimeout(s.ctx, core.DefaultControlBudget) // the setup's calls have their own deadlines
		select {
		case <-d.setup.done:
		case <-wait.Done():
		}
		stop()
		act = offlineContinue
		if s.c.s.claims.offlinePhase(s) == offlineOn {
			act = offlineFail
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), core.DefaultControlBudget)
	defer cancel()
	ps := s.page
	handler, others := ps.otherRules(stageRequest, rawURL)
	if handler {
		return
	}
	if act == offlineFail {
		err := s.control(func() error {
			_, err := s.pg.PS.Sched.Control(ctx, core.ControlMsg{
				Kind: core.ControlCompleteRequest, Session: s.c.rec.session, Epoch: s.epoch,
				RequestID: id, TargetID: target, Method: "Network.interceptRequestWithError", Params: map[string]any{"errorType": "General"},
			})
			return err
		})
		switch {
		case err == nil || errors.Is(err, core.ErrControlRefused):
			return
		case !errors.Is(err, errDetached):
			ps.logf("cdp: page %s: failing request %s of target %q offline: %v", logsafe.ID(ps.target), logsafe.ID(id), logsafe.ID(target), logsafe.Error(err))
			return
		}
		// detached: continue it instead
	} else if others {
		return // another owner's rule paused it too: that owner answers it
	}
	ps.continueNow(target, id, stageRequest)
}
