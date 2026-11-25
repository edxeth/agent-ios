package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The Emulation domain (design docs/design/agentic-parity.md 5.3, tier 1).
// The media, user-agent and geolocation overrides are state of the
// attachment's desired-state owner (design 2.4, 5.2), so a detach removes
// them. A clear (an empty value) sends nothing unless this attachment set
// the override: Playwright sends empty values on every page setup, and
// they must not remove an override that the session's CLI commands set.
// Focus and touch emulation have no effect (the device is a touch device);
// the viewport cannot change.

func init() {
	register(
		Method{Name: "Emulation.setEmulatedMedia", Levels: OnPage, Handle: emuMedia},
		Method{Name: "Emulation.setUserAgentOverride", Levels: OnPage, Handle: emuUserAgent},
		Method{Name: "Emulation.setGeolocationOverride", Levels: OnPage, Handle: emuGeolocation},
		Method{Name: "Emulation.setFocusEmulationEnabled", Levels: OnPage},
		Method{Name: "Emulation.setTouchEmulationEnabled", Levels: OnPage},
		Method{Name: "Emulation.setDeviceMetricsOverride", Levels: OnPage, Handle: emuDeviceMetrics},
		Method{Name: "Emulation.clearDeviceMetricsOverride", Levels: OnPage, Handle: emuClearDeviceMetrics},
	)
}

// emuState is what the attachment's overrides set: the media type, the
// user preferences by WIP name, the user agent.
type emuState struct {
	mu    sync.Mutex
	media bool
	prefs map[string]bool
	ua    bool
}

// mediaFeatures maps CDP media features to WIP user preferences
// (Page.overrideUserPreference) and their values.
var mediaFeatures = map[string]struct {
	pref   string
	values map[string]string
}{
	"prefers-color-scheme":   {"PrefersColorScheme", map[string]string{"light": "Light", "dark": "Dark"}},
	"prefers-reduced-motion": {"PrefersReducedMotion", map[string]string{"reduce": "Reduce", "no-preference": "NoPreference"}},
	"prefers-contrast":       {"PrefersContrast", map[string]string{"more": "More", "no-preference": "NoPreference"}},
}

// baselineValues are the values of a feature that iOS WebKit cannot
// emulate that equal the device's state; they are accepted and change
// nothing (Playwright sends forced-colors none on every setup).
var baselineValues = map[string]bool{"": true, "none": true, "no-preference": true}

// mediaFeature is CDP's Emulation.MediaFeature.
type mediaFeature struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// emuMedia keeps the presence of each field (pinned Puppeteer sends the
// media type and the features in separate calls, puppeteer-core 25.16.0
// EmulationManager.ts): an omitted media changes no media type, and an
// omitted features list changes no feature. A supplied features list
// replaces the attachment's whole feature set, so a feature that this
// attachment overrode and the list omits is cleared, and [] clears them
// all; other owners' overrides stay.
func emuMedia(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Media    *string         `json:"media"`
		Features *[]mediaFeature `json:"features"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Media != nil && *p.Media != "" && *p.Media != "print" && *p.Media != "screen" {
		return nil, nil, invalidParam("media %q: want print, screen or empty", *p.Media)
	}
	type pref struct{ name, value string }
	var prefs []pref
	var features []mediaFeature
	if p.Features != nil {
		features = *p.Features
	}
	for _, f := range features {
		mf, ok := mediaFeatures[f.Name]
		if !ok {
			if baselineValues[f.Value] {
				continue
			}
			return nil, nil, serverError("Emulation.setEmulatedMedia: media feature %q with value %q cannot be emulated on iOS WebKit", f.Name, f.Value)
		}
		v, ok := mf.values[f.Value]
		if !ok && f.Value != "" {
			return nil, nil, serverError("Emulation.setEmulatedMedia: %s %q cannot be emulated on iOS WebKit", f.Name, f.Value)
		}
		prefs = append(prefs, pref{mf.pref, v})
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		st := &s.emu
		st.mu.Lock()
		defer st.mu.Unlock()
		if p.Media != nil && (*p.Media != "" || st.media) {
			if err := s.owned(ctx, mo, "Page.setEmulatedMedia", map[string]string{"media": *p.Media}); err != nil {
				return err
			}
			st.media = *p.Media != ""
		}
		if p.Features == nil {
			return nil
		}
		// The replacement first clears the attachment's overrides that
		// the list omits (in name order), then sends the list.
		listed := map[string]bool{}
		for _, pr := range prefs {
			listed[pr.name] = true
		}
		var dropped []pref
		for name, on := range st.prefs {
			if on && !listed[name] {
				dropped = append(dropped, pref{name, ""})
			}
		}
		slices.SortFunc(dropped, func(a, b pref) int { return strings.Compare(a.name, b.name) })
		for _, pr := range append(dropped, prefs...) {
			if pr.value == "" && !st.prefs[pr.name] {
				continue
			}
			params := map[string]string{"name": pr.name}
			if pr.value != "" {
				params["value"] = pr.value
			}
			if err := s.owned(ctx, mo, "Page.overrideUserPreference", params); err != nil {
				return err
			}
			if st.prefs == nil {
				st.prefs = map[string]bool{}
			}
			st.prefs[pr.name] = pr.value != ""
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// emuUserAgent overrides the user agent (Page.overrideUserAgent).
// acceptLanguage, platform and userAgentMetadata have no WIP equivalent
// and are ignored.
func emuUserAgent(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		UserAgent string `json:"userAgent"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		st := &s.emu
		st.mu.Lock()
		defer st.mu.Unlock()
		if p.UserAgent == "" && !st.ua {
			return nil
		}
		params := map[string]string{}
		if p.UserAgent != "" {
			params["value"] = p.UserAgent
		}
		if err := s.owned(ctx, mo, "Page.overrideUserAgent", params); err != nil {
			return err
		}
		st.ua = p.UserAgent != ""
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// emuGeolocation installs W4's geolocation shim (devtools.GeoShim) as a
// bootstrap part of the attachment's owner and in the current document;
// the owner's entry restores the current document's geolocation on
// release and installs another owner's surviving override again (the
// session's CLI "set geo"; devtools.GeoRelease), as
// devtools.Page.SetGeolocation does for a session. A position without
// coordinates (CDP: position unavailable) cannot be emulated by the shim.
func emuGeolocation(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		Accuracy  *float64 `json:"accuracy"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Latitude == nil || p.Longitude == nil {
		return nil, nil, serverError("Emulation.setGeolocationOverride: an unavailable position (no latitude or longitude) cannot be emulated on iOS WebKit")
	}
	lat, lng, acc := *p.Latitude, *p.Longitude, 1.0
	if p.Accuracy != nil {
		acc = *p.Accuracy
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 || acc < 0 {
		return nil, nil, invalidParam("latitude must be in [-90, 90], longitude in [-180, 180] and accuracy 0 or more")
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		ps := s.pg.PS
		src := devtools.GeoShim(lat, lng, acc)
		return ps.State.Change(ctx, mo.Op.Caller(), func(c webkit.Caller) error {
			first := !ps.State.Needs("Page")
			if err := ps.State.SetPart(s.owner, devtools.GeoPartPrefix+string(s.owner), core.RankShim, src); err != nil {
				return err
			}
			ps.State.Put(core.Entry{
				Key: "cdp:geo:" + string(s.owner), Owner: s.owner, Apply: func(context.Context, core.Sender) error { return nil },
				Release: devtools.GeoRelease(ps.State.Boot),
			})
			if first {
				if _, err := c.Send(ctx, "Page.enable", nil); err != nil {
					return fmt.Errorf("sending Page.enable: %w", err)
				}
			}
			if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": ps.State.Boot.Script()}); err != nil {
				return fmt.Errorf("Page.setBootstrapScript: %w", err)
			}
			if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": src, "returnByValue": true}); err != nil {
				return fmt.Errorf("installing the geolocation shim in the current document: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// errFixedViewport is the reply to a viewport change (design 5.3).
const errFixedViewport = "viewport is fixed by the device"

// emuDeviceMetrics succeeds only for an override equal to the page's
// viewport (width and height in CSS pixels, deviceScaleFactor 0 or the
// page's), or for 0, 0 and 0, which means no override; any other override
// fails with the fixed-viewport error and the real values.
func emuDeviceMetrics(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Width             float64 `json:"width"`
		Height            float64 `json:"height"`
		DeviceScaleFactor float64 `json:"deviceScaleFactor"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Width == 0 && p.Height == 0 && p.DeviceScaleFactor == 0 {
		return map[string]any{}, nil, nil
	}
	var vp [3]float64
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		r, err := wipCall(pc.ctx, pc.mo, "Runtime.evaluate", map[string]any{"expression": "[innerWidth, innerHeight, devicePixelRatio]", "returnByValue": true})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(r.Result["value"])
		if err := json.Unmarshal(raw, &vp); err != nil {
			return fmt.Errorf("reading the viewport: %s: %w", raw, err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if p.Width == vp[0] && p.Height == vp[1] && (p.DeviceScaleFactor == 0 || p.DeviceScaleFactor == vp[2]) {
		return map[string]any{}, nil, nil
	}
	return nil, nil, serverError("%s: the page is %vx%v CSS pixels at scale %v; connect with defaultViewport: null", errFixedViewport, vp[0], vp[1], vp[2])
}

func emuClearDeviceMetrics(context.Context, *Call) (any, Later, error) {
	return nil, nil, serverError("%s", errFixedViewport)
}
