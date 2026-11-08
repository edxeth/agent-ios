package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/wip"
)

// supported reports whether method is in the page's iOS command list
// (unknown versions, WebKitGTK included, are not gated).
func (p *Page) supported(method string) bool {
	known, ok := wip.Supported(p.ps.Device.OSVersion, method)
	return !known || ok
}

// Media is the "set media" state: prefers-color-scheme, reduced motion,
// contrast and the emulated media type. Zero fields leave a feature alone.
type Media struct {
	ColorScheme   string `json:"colorScheme,omitempty"` // dark, light or no-preference
	ReducedMotion bool   `json:"reducedMotion,omitempty"`
	ContrastMore  bool   `json:"contrastMore,omitempty"`
	Type          string `json:"type,omitempty"` // print or screen
}

var colorSchemeValues = map[string]string{"dark": "Dark", "light": "Light", "no-preference": "NoPreference"}

// SetMedia sets the media overrides of session as the owned entry
// "media:<session>" (design 4.2 "set media"): Page.overrideUserPreference
// on iOS 16.4 and later and WebKitGTK, Page.setForcedAppearance before
// that (only the color scheme), and Page.setEmulatedMedia for the media
// type. The entry is re-applied on a reconnect and on provisional targets,
// and reset on release.
func (p *Page) SetMedia(ctx context.Context, c webkit.Caller, session string, m Media) error {
	if m.ColorScheme != "" {
		if _, ok := colorSchemeValues[m.ColorScheme]; !ok {
			return fmt.Errorf("set media %q: want dark, light or no-preference", m.ColorScheme)
		}
	}
	if m.Type != "" && m.Type != "print" && m.Type != "screen" {
		return fmt.Errorf("set media --type %q: want print or screen", m.Type)
	}
	prefs := p.supported("Page.overrideUserPreference")
	if !prefs && (m.ReducedMotion || m.ContrastMore) {
		return fmt.Errorf("set media: %w: reduced motion and contrast need Page.overrideUserPreference (iOS 16.4 and later)", ErrUnsupported)
	}
	apply := func(ctx context.Context, s core.Sender) error {
		if m.ColorScheme != "" {
			if prefs {
				if _, err := s.Send(ctx, "Page.overrideUserPreference", map[string]string{"name": "PrefersColorScheme", "value": colorSchemeValues[m.ColorScheme]}); err != nil {
					return fmt.Errorf("Page.overrideUserPreference: %w", err)
				}
			} else if m.ColorScheme != "no-preference" {
				if _, err := s.Send(ctx, "Page.setForcedAppearance", map[string]string{"appearance": colorSchemeValues[m.ColorScheme]}); err != nil {
					return fmt.Errorf("Page.setForcedAppearance: %w", err)
				}
			}
		}
		if m.ReducedMotion {
			if _, err := s.Send(ctx, "Page.overrideUserPreference", map[string]string{"name": "PrefersReducedMotion", "value": "Reduce"}); err != nil {
				return fmt.Errorf("Page.overrideUserPreference: %w", err)
			}
		}
		if m.ContrastMore {
			if _, err := s.Send(ctx, "Page.overrideUserPreference", map[string]string{"name": "PrefersContrast", "value": "More"}); err != nil {
				return fmt.Errorf("Page.overrideUserPreference: %w", err)
			}
		}
		if m.Type != "" {
			if _, err := s.Send(ctx, "Page.setEmulatedMedia", map[string]string{"media": m.Type}); err != nil {
				return fmt.Errorf("Page.setEmulatedMedia: %w", err)
			}
		}
		return nil
	}
	release := func(ctx context.Context, s core.Sender) error {
		var firstErr error
		note := func(_ json.RawMessage, err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if prefs {
			for _, n := range []string{"PrefersColorScheme", "PrefersReducedMotion", "PrefersContrast"} {
				note(s.Send(ctx, "Page.overrideUserPreference", map[string]string{"name": n}))
			}
		} else if p.supported("Page.setForcedAppearance") {
			note(s.Send(ctx, "Page.setForcedAppearance", map[string]string{"appearance": ""}))
		}
		if m.Type != "" {
			note(s.Send(ctx, "Page.setEmulatedMedia", map[string]string{"media": ""}))
		}
		return firstErr
	}
	return p.putOverride(ctx, c, core.Entry{Key: "devtools:media:" + session, Owner: owner("media", session), Domain: "Page", Apply: apply, Release: release})
}

// putOverride replaces an override entry as one desired-state change
// (core.DesiredState.Change), so provisional targets get it too: the old
// one is released on the page first (so a removed feature is reset), then
// the new one recorded and applied. Each record comes before the sends
// that carry it (the contract of Change). An entry is the only record of
// what it applied, so it is removed only when its reset succeeded: a
// failed reset of the old entry keeps the old entry, and a failed Apply
// (some of its sends can have succeeded) is undone at once; when that undo
// fails too, a reset-only entry stays, and the release of the owner resets
// the page.
//
// The owner holds e.Domain, the agent whose state the override is (Page
// for every override here): a release that cannot send the reset marks
// that agent unknown, and core resets it later with its disable, which
// resets WebKit's overrides of the agent; core applies the entries of the
// agent again only while the agent is needed (core-events r3).
func (p *Page) putOverride(ctx context.Context, c webkit.Caller, e core.Entry) error {
	return p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if err := acquireDomain(ctx, p.ps, c, e.Owner, e.Domain); err != nil {
			return err
		}
		if old, ok := p.ps.State.Remove(e.Key); ok && old.Release != nil {
			if err := old.Release(ctx, c); err != nil {
				p.ps.State.Put(old)
				return err
			}
		}
		p.ps.State.Put(e)
		if err := e.Apply(ctx, c); err != nil {
			p.ps.State.Remove(e.Key)
			if rerr := e.Release(ctx, c); rerr != nil {
				p.ps.State.Put(core.Entry{Key: e.Key, Owner: e.Owner, Domain: e.Domain, Apply: noop, Release: e.Release})
				return errors.Join(err, fmt.Errorf("undoing the part that was applied: %w", rerr))
			}
			return err
		}
		return nil
	})
}

// ClearOverride removes the override entry of kind for session and resets
// it on the page, as one desired-state change. It reports whether there
// was one. The entry is the only record of the override, so it stays when
// the reset fails (W4 round 3, review finding overrides.go:140): a failed
// reset on the active target keeps the entry; a reset that a provisional
// target refused keeps a reset-only entry; the release of the owner then
// resets the page.
func (p *Page) ClearOverride(ctx context.Context, c webkit.Caller, kind, session string) (found bool, err error) {
	key := "devtools:" + kind + ":" + session
	var old core.Entry
	reset := false
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var ok bool
		old, ok = p.ps.State.Remove(key)
		found = ok
		if ok && old.Release != nil {
			if err := old.Release(ctx, c); err != nil {
				p.ps.State.Put(old)
				return err
			}
			reset = true
		}
		return nil
	})
	if err != nil && reset {
		p.keepResetEntry(ctx, c, core.Entry{Key: key, Owner: old.Owner, Domain: old.Domain, Apply: noop, Release: old.Release})
	}
	return found, err
}

// resetEntryBudget bounds the change that records a reset-only entry after
// a failed ClearOverride; it runs also when the command's context ended.
const resetEntryBudget = 5 * time.Second

// keepResetEntry records e unless a later change put an entry of its key.
func (p *Page) keepResetEntry(ctx context.Context, c webkit.Caller, e core.Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetEntryBudget)
	defer cancel()
	err := p.ps.State.Change(ctx, c, func(webkit.Caller) error {
		if cur, ok := p.ps.State.Remove(e.Key); ok {
			p.ps.State.Put(cur)
			return nil
		}
		p.ps.State.Put(e)
		return nil
	})
	if err != nil {
		p.logf("keeping the reset of %s for the release: %v", e.Key, err)
	}
}

// SetUserAgent overrides the user agent (Page.overrideUserAgent) as the
// owned entry "useragent:<session>".
func (p *Page) SetUserAgent(ctx context.Context, c webkit.Caller, session, ua string) error {
	if ua == "" {
		_, err := p.ClearOverride(ctx, c, "useragent", session)
		return err
	}
	return p.putOverride(ctx, c, core.Entry{
		Key: "devtools:useragent:" + session, Owner: owner("useragent", session), Domain: "Page",
		Apply: func(ctx context.Context, s core.Sender) error {
			_, err := s.Send(ctx, "Page.overrideUserAgent", map[string]string{"value": ua})
			return err
		},
		Release: func(ctx context.Context, s core.Sender) error {
			_, err := s.Send(ctx, "Page.overrideUserAgent", map[string]any{})
			return err
		},
	})
}

// SetHeaders replaces the extra HTTP headers of session (owner
// "headers:<session>"; design 2.4): Network is enabled for the owner, a new
// name or value is sent as the complete effective map, and an empty map
// gives up the owner's names: with other Network owners left, the Network
// reinitialization barrier removes them (WIP merges header maps and never
// removes a name); with none left, Network.disable alone clears the map.
// It reports whether the barrier ran.
func (p *Page) SetHeaders(ctx context.Context, c webkit.Caller, session string, h map[string]string) (reset bool, err error) {
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var cerr error
		reset, cerr = p.setHeaders(ctx, c, session, h)
		return cerr
	})
	return reset, err
}

// setHeaders is SetHeaders inside its change.
func (p *Page) setHeaders(ctx context.Context, c webkit.Caller, session string, h map[string]string) (reset bool, err error) {
	o := owner("headers", session)
	if len(h) == 0 {
		last := p.ps.State.ReleaseDomain(o, "Network")
		ch := p.ps.State.SetHeaders(o, nil)
		switch {
		case last:
			_, err := c.Send(ctx, "Network.disable", nil)
			return false, err
		case ch.NeedsNetworkReset:
			return true, core.NetworkReset(ctx, p.ps.Sched, c, p.ps.State)
		}
		return false, nil
	}
	if err := acquireDomain(ctx, p.ps, c, o, "Network"); err != nil {
		return false, err
	}
	ch := p.ps.State.SetHeaders(o, h)
	if ch.NeedsNetworkReset {
		return true, core.NetworkReset(ctx, p.ps.Sched, c, p.ps.State)
	}
	if ch.Send != nil {
		if _, err := c.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": ch.Send}); err != nil {
			return false, fmt.Errorf("Network.setExtraHTTPHeaders: %w", err)
		}
	}
	return false, nil
}

// GeoShim returns the bootstrap source that answers navigator.geolocation
// with a fixed position (design 4.3: WIP has no geolocation override). The
// first shim of a document saves the original methods and whether they
// were own properties in the hidden property __iwdpGeo; a later shim
// keeps that record, so GeoRestore always gets the page's own methods
// back. The callbacks that the shim has scheduled are recorded there too.
func GeoShim(lat, lng, accuracy float64) string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	return `(function () {
  var g = navigator.geolocation;
  if (!g) return;
  var names = ["getCurrentPosition", "watchPosition", "clearWatch"];
  if (!Object.prototype.hasOwnProperty.call(g, "__iwdpGeo")) {
    var orig = {};
    for (var i = 0; i < names.length; i++) {
      orig[names[i]] = {own: Object.prototype.hasOwnProperty.call(g, names[i]), value: g[names[i]]};
    }
    Object.defineProperty(g, "__iwdpGeo", {configurable: true, value: {orig: orig, timers: []}});
  }
  var S = g.__iwdpGeo;
  var pos = function () {
    return {coords: {latitude: ` + f(lat) + `, longitude: ` + f(lng) + `, accuracy: ` + f(accuracy) + `, altitude: null, altitudeAccuracy: null, heading: null, speed: null}, timestamp: Date.now()};
  };
  var later = function (ok) {
    var t = setTimeout(function () { S.timers.splice(S.timers.indexOf(t), 1); ok(pos()); }, 0);
    S.timers.push(t);
  };
  var id = 0;
  g.getCurrentPosition = function (ok) { later(ok); };
  g.watchPosition = function (ok) { id++; later(ok); return id; };
  g.clearWatch = function () {};
})();`
}

// GeoRestore is the current-document cleanup of the geolocation shim: it
// cancels the shim's scheduled callbacks and gives navigator.geolocation
// its original methods back (an inherited method by deleting the shim's
// own property). A document without the shim is left alone.
const GeoRestore = `(function () {
  var g = navigator.geolocation;
  if (!g || !Object.prototype.hasOwnProperty.call(g, "__iwdpGeo")) return false;
  var S = g.__iwdpGeo;
  S.timers.forEach(function (t) { clearTimeout(t); });
  for (var n in S.orig) {
    if (S.orig[n].own) g[n] = S.orig[n].value; else delete g[n];
  }
  delete g.__iwdpGeo;
  return true;
})()`

// GeoPartPrefix starts the name of every geolocation shim's bootstrap
// part: "geolocation:<session>" of a session (SetGeolocation) and
// "geolocation:<owner>" of a CDP attachment (internal/cdp).
const GeoPartPrefix = "geolocation:"

// GeoRelease returns the Release of a geolocation owner's entry. The
// owner's bootstrap part is gone when it runs (core.DesiredState.Release
// drops an owner's parts before its entries' releases), so the parts left
// are the surviving owners' overrides (design 2.4: a release removes only
// the owner's state). It restores the current document's geolocation
// (GeoRestore); when another owner's geolocation part remains, it installs
// the effective one, the last in script order, in the current document
// again, so the surviving override stays active there as in new documents.
func GeoRelease(boot *core.BootstrapParts) func(context.Context, core.Sender) error {
	return func(ctx context.Context, s core.Sender) error {
		if _, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": GeoRestore, "returnByValue": true}); err != nil {
			return fmt.Errorf("restoring navigator.geolocation in the current document: %w", err)
		}
		survivor := ""
		for _, n := range boot.Names() {
			if src, ok := boot.Source(n); ok && strings.HasPrefix(n, GeoPartPrefix) {
				survivor = src
			}
		}
		if survivor == "" {
			return nil
		}
		if _, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": survivor, "returnByValue": true}); err != nil {
			return fmt.Errorf("installing the remaining geolocation override in the current document: %w", err)
		}
		return nil
	}
}

// SetGeolocation installs the geolocation shim of session as the bootstrap
// part "geolocation:<session>" and in the current document. The entry
// "devtools:geo:<session>" restores the current document's geolocation on
// release (owner "geo:<session>"; GeoRelease, which keeps another owner's
// override); the release removes the bootstrap part.
// The entry is put before the shim is sent, so a failed send still has
// its cleanup. It runs as one desired-state change.
func (p *Page) SetGeolocation(ctx context.Context, c webkit.Caller, session string, lat, lng, accuracy float64) error {
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return fmt.Errorf("set geo %v %v: latitude must be in [-90, 90] and longitude in [-180, 180]", lat, lng)
	}
	return p.ps.State.Change(ctx, c, func(c webkit.Caller) error { return p.setGeolocation(ctx, c, session, lat, lng, accuracy) })
}

// setGeolocation is SetGeolocation inside its change.
func (p *Page) setGeolocation(ctx context.Context, c webkit.Caller, session string, lat, lng, accuracy float64) error {
	src := GeoShim(lat, lng, accuracy)
	o := owner("geo", session)
	if err := p.ps.State.SetPart(o, GeoPartPrefix+session, core.RankShim, src); err != nil {
		return err
	}
	p.ps.State.Put(core.Entry{Key: "devtools:geo:" + session, Owner: o, Apply: noop, Release: GeoRelease(p.ps.Boot)})
	if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": p.ps.Boot.Script()}); err != nil {
		return fmt.Errorf("Page.setBootstrapScript: %w", err)
	}
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": src, "returnByValue": true}); err != nil {
		return fmt.Errorf("installing the geolocation shim in the current document: %w", err)
	}
	return nil
}

// NetworkPresets are the "emulate network" presets in bytes per second
// (Network.setEmulatedConditions has only a throughput limit).
var NetworkPresets = map[string]int{
	"slow-3g": 50_000,  // 400 kbit/s, Chrome DevTools "Slow 3G"
	"fast-3g": 200_000, // 1.6 Mbit/s, Chrome DevTools "Fast 3G"
	"4g":      1_125_000,
}

// EmulateNetwork throttles the page to preset (owner "emulate:<session>"),
// or removes the throttle for "none". Network.setEmulatedConditions is in
// the iOS 16.4 to 17.x and 27.3+ command lists only (internal/wip: the
// 16.4 and 17.0 lists have it; the 18.0 to 27.0 lists do not).
func (p *Page) EmulateNetwork(ctx context.Context, c webkit.Caller, session, preset string) error {
	bps := 0
	if preset != "none" {
		var ok bool
		if bps, ok = NetworkPresets[preset]; !ok {
			return fmt.Errorf("emulate network %q: want %s or none", preset, strings.Join(presetNames(), ", "))
		}
	}
	return p.EmulateNetworkRate(ctx, c, session, bps)
}

// EmulateNetworkRate throttles the page to bytesPerSecond, or removes the
// throttle for 0 (WebKit: no positive bytesPerSecondLimit, no limit).
func (p *Page) EmulateNetworkRate(ctx context.Context, c webkit.Caller, session string, bytesPerSecond int) error {
	if !p.supported("Network.setEmulatedConditions") {
		return fmt.Errorf("emulate network: %w: Network.setEmulatedConditions is not in the iOS %s command list", ErrUnsupported, p.ps.Device.OSVersion)
	}
	if bytesPerSecond < 0 {
		return fmt.Errorf("emulate network: %d bytes per second: want 0 (no limit) or more", bytesPerSecond)
	}
	params := json.RawMessage("{}")
	if bytesPerSecond > 0 {
		params = json.RawMessage(fmt.Sprintf(`{"bytesPerSecondLimit":%d}`, bytesPerSecond))
	}
	return p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if send := p.ps.State.SetEmulatedConditions(owner("emulate", session), params); send != nil {
			if _, err := c.Send(ctx, "Network.setEmulatedConditions", send); err != nil {
				return fmt.Errorf("Network.setEmulatedConditions: %w", err)
			}
		}
		return nil
	})
}

func presetNames() []string { return []string{"slow-3g", "fast-3g", "4g"} }

// AddInitScript adds a user init script of session as a bootstrap part
// (design 4.2 "addinitscript"): it runs in every new document before page
// scripts when pause-on-start is on, and is partial without it (2.7). It
// returns the script id. It runs as one desired-state change.
func (p *Page) AddInitScript(ctx context.Context, c webkit.Caller, session, source string) (string, error) {
	p.mu.Lock()
	p.inits[session]++
	id := fmt.Sprintf("init%d", p.inits[session])
	p.mu.Unlock()
	err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if err := p.ps.State.SetPart(owner("initscript", session), "initscript:"+session+":"+id, core.RankInit, source); err != nil {
			return err
		}
		if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": p.ps.Boot.Script()}); err != nil {
			p.ps.Boot.Remove("initscript:" + session + ":" + id)
			return fmt.Errorf("Page.setBootstrapScript: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// RemoveInitScript removes the init script id of session, as one
// desired-state change.
func (p *Page) RemoveInitScript(ctx context.Context, c webkit.Caller, session, id string) error {
	return p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if !p.ps.Boot.Remove("initscript:" + session + ":" + id) {
			return fmt.Errorf("removeinitscript: session %q has no init script %q", session, id)
		}
		if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": p.ps.Boot.Script()}); err != nil {
			return fmt.Errorf("Page.setBootstrapScript: %w", err)
		}
		return nil
	})
}
