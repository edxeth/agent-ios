package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 review finding overrides.go:105 (P1) and design 2.4 and 2.7: a state
// change made while a provisional target exists, after that target's
// setup took its snapshot, reaches the target before it commits, so the
// committed target has the new state. Each case sets a first state (its
// setup sends it to page-2, which shows that the setup has run), changes
// the state while page-2 is still provisional, and checks what page-2
// received once it committed. The media case is the reviewer's
// reproduction TestReviewMediaChangedDuringSwap
// (/home/devkit/.pi/artifacts/reviewer/w4-lib-repro/devtools_test.go).
// page-2 commits only after the change returned and its records show the
// change (W4 round 7: a fixed 700 ms commit delay let page-2 commit
// before the change under load).

type duringSwapCase struct {
	name string
	// first sets the state that page-2's setup gets; marker finds that
	// state among page-2's records.
	first  func(ctx context.Context, p *Page, c webkit.Caller) error
	marker func(rec fakewebkit.Record) bool
	// change runs while page-2 is provisional.
	change func(ctx context.Context, p *Page, c webkit.Caller) error
	// check returns "" when page-2's records have the changed state, else
	// what is wrong.
	check func(recs []fakewebkit.Record) string
}

// sentTo returns the records that the client sent to target, in order.
func sentTo(f *fakewebkit.Server, target string) []fakewebkit.Record {
	var out []fakewebkit.Record
	for _, rec := range f.Received() {
		if rec.Target == target {
			out = append(out, rec)
		}
	}
	return out
}

func params(rec fakewebkit.Record) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(rec.Params, &p)
	return p
}

// is reports whether rec is method with every key of want at its value.
func is(rec fakewebkit.Record, method string, want map[string]any) bool {
	if rec.Method != method {
		return false
	}
	p := params(rec)
	for k, v := range want {
		if p[k] != v {
			return false
		}
	}
	return true
}

// last returns the last record of recs that match reports, if any.
func last(recs []fakewebkit.Record, match func(fakewebkit.Record) bool) (fakewebkit.Record, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if match(recs[i]) {
			return recs[i], true
		}
	}
	return fakewebkit.Record{}, false
}

// interceptionRules replays the rule adds and removals of recs and
// returns the URLs of the rules left.
func interceptionRules(recs []fakewebkit.Record) []string {
	var rules []string
	for _, rec := range recs {
		u, _ := params(rec)["url"].(string)
		switch rec.Method {
		case "Network.addInterception":
			rules = append(rules, u)
		case "Network.removeInterception":
			if i := slices.Index(rules, u); i >= 0 {
				rules = slices.Delete(rules, i, i+1)
			}
		}
	}
	return rules
}

// lastBootstrap is the last bootstrap script that recs set.
func lastBootstrap(recs []fakewebkit.Record) string {
	rec, _ := last(recs, func(r fakewebkit.Record) bool { return r.Method == "Page.setBootstrapScript" })
	s, _ := params(rec)["source"].(string)
	return s
}

func bootstrapHas(s string) func(fakewebkit.Record) bool {
	return func(rec fakewebkit.Record) bool {
		src, _ := params(rec)["source"].(string)
		return rec.Method == "Page.setBootstrapScript" && strings.Contains(src, s)
	}
}

func hasRule(regex string) func(fakewebkit.Record) bool {
	return func(rec fakewebkit.Record) bool {
		return is(rec, "Network.addInterception", map[string]any{"url": regex})
	}
}

func mediaDark(ctx context.Context, p *Page, c webkit.Caller) error {
	return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "dark"})
}

var darkMarker = func(rec fakewebkit.Record) bool {
	return is(rec, "Page.overrideUserPreference", map[string]any{"name": "PrefersColorScheme", "value": "Dark"})
}

func duringSwapCases() []duringSwapCase {
	wipRegex := func(pattern string) string { re, _ := PatternRegex(pattern); return re }
	return []duringSwapCase{
		{
			name: "media", first: mediaDark, marker: darkMarker,
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "light"})
			},
			check: func(recs []fakewebkit.Record) string {
				rec, _ := last(recs, func(r fakewebkit.Record) bool {
					return is(r, "Page.overrideUserPreference", map[string]any{"name": "PrefersColorScheme"})
				})
				if params(rec)["value"] != "Light" {
					return "the last color scheme of page-2 is " + string(rec.Params) + ", want Light"
				}
				return ""
			},
		},
		{
			name: "user agent",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetUserAgent(ctx, c, "s1", "UA-first")
			},
			marker: func(rec fakewebkit.Record) bool {
				return is(rec, "Page.overrideUserAgent", map[string]any{"value": "UA-first"})
			},
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetUserAgent(ctx, c, "s1", "UA-second")
			},
			check: func(recs []fakewebkit.Record) string {
				rec, _ := last(recs, func(r fakewebkit.Record) bool { return r.Method == "Page.overrideUserAgent" })
				if params(rec)["value"] != "UA-second" {
					return "the last user agent of page-2 is " + string(rec.Params) + ", want UA-second"
				}
				return ""
			},
		},
		{
			name: "user agent cleared",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetUserAgent(ctx, c, "s1", "UA-first")
			},
			marker: func(rec fakewebkit.Record) bool {
				return is(rec, "Page.overrideUserAgent", map[string]any{"value": "UA-first"})
			},
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetUserAgent(ctx, c, "s1", "")
			},
			check: func(recs []fakewebkit.Record) string {
				rec, _ := last(recs, func(r fakewebkit.Record) bool { return r.Method == "Page.overrideUserAgent" })
				if _, set := params(rec)["value"]; set {
					return "the last user agent of page-2 is " + string(rec.Params) + ", want the override removed"
				}
				return ""
			},
		},
		{
			name: "headers",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", map[string]string{"X-Test": "first"})
				return err
			},
			marker: func(rec fakewebkit.Record) bool { return rec.Method == "Network.setExtraHTTPHeaders" },
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", map[string]string{"X-Test": "second"})
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				rec, _ := last(recs, func(r fakewebkit.Record) bool { return r.Method == "Network.setExtraHTTPHeaders" })
				if h, _ := params(rec)["headers"].(map[string]any); h["X-Test"] != "second" {
					return "the last extra headers of page-2 are " + string(rec.Params) + ", want X-Test: second"
				}
				return ""
			},
		},
		{
			name: "headers cleared",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", map[string]string{"X-Test": "first"})
				return err
			},
			marker: func(rec fakewebkit.Record) bool { return rec.Method == "Network.setExtraHTTPHeaders" },
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", nil)
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				// The headers owner was Network's only owner, so
				// Network.disable clears the map (design 2.4).
				i := slices.IndexFunc(recs, func(r fakewebkit.Record) bool { return r.Method == "Network.setExtraHTTPHeaders" })
				if !slices.ContainsFunc(recs[i+1:], func(r fakewebkit.Record) bool { return r.Method == "Network.disable" }) {
					return "page-2 kept X-Test: no Network.disable after its headers"
				}
				return ""
			},
		},
		{
			name: "geolocation",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetGeolocation(ctx, c, "s1", 11, 22, 5)
			},
			marker: bootstrapHas("latitude: 11,"),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetGeolocation(ctx, c, "s1", 33, 44, 5)
			},
			check: func(recs []fakewebkit.Record) string {
				if s := lastBootstrap(recs); !strings.Contains(s, "latitude: 33,") || strings.Contains(s, "latitude: 11,") {
					return "the last bootstrap script of page-2 lacks the new position 33, 44"
				}
				return ""
			},
		},
		{
			name: "offline on", first: mediaDark, marker: darkMarker,
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetOffline(ctx, c, "s1", true)
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.Contains(interceptionRules(recs), ".*") {
					return "page-2 has no offline rule .*"
				}
				if _, ok := last(recs, func(r fakewebkit.Record) bool {
					return is(r, "Network.setInterceptionEnabled", map[string]any{"enabled": true})
				}); !ok {
					return "page-2 never got interception enabled"
				}
				return ""
			},
		},
		{
			name: "offline off",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetOffline(ctx, c, "s1", true)
			},
			marker: hasRule(".*"),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetOffline(ctx, c, "s1", false)
			},
			check: func(recs []fakewebkit.Record) string {
				if slices.Contains(interceptionRules(recs), ".*") {
					return "page-2 keeps the offline rule .*"
				}
				return ""
			},
		},
		{
			name: "credentials",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "user", "pw")
				return err
			},
			marker: hasRule(`^https://a\.example/`),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetCredentials(ctx, c, "s1", "https://b.example", "user", "pw")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.Contains(interceptionRules(recs), `^https://b\.example/`) {
					return "page-2 has no credentials rule for https://b.example"
				}
				return ""
			},
		},
		{
			name: "credentials cleared",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "user", "pw")
				return err
			},
			marker: hasRule(`^https://a\.example/`),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.ClearCredentials(ctx, c, "s1")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if slices.Contains(interceptionRules(recs), `^https://a\.example/`) {
					return "page-2 keeps the credentials rule for https://a.example"
				}
				return ""
			},
		},
		{
			name: "emulated network",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.EmulateNetworkRate(ctx, c, "s1", 1000)
			},
			marker: func(rec fakewebkit.Record) bool {
				return is(rec, "Network.setEmulatedConditions", map[string]any{"bytesPerSecondLimit": float64(1000)})
			},
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.EmulateNetworkRate(ctx, c, "s1", 2000)
			},
			check: func(recs []fakewebkit.Record) string {
				rec, _ := last(recs, func(r fakewebkit.Record) bool { return r.Method == "Network.setEmulatedConditions" })
				if params(rec)["bytesPerSecondLimit"] != float64(2000) {
					return "the last emulated conditions of page-2 are " + string(rec.Params) + ", want 2000 bytes/s"
				}
				return ""
			},
		},
		{
			name: "init script added",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddInitScript(ctx, c, "s1", "/*init-first*/")
				return err
			},
			marker: bootstrapHas("/*init-first*/"),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddInitScript(ctx, c, "s1", "/*init-second*/")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !strings.Contains(lastBootstrap(recs), "/*init-second*/") {
					return "the last bootstrap script of page-2 lacks the second init script"
				}
				return ""
			},
		},
		{
			name: "init script removed",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddInitScript(ctx, c, "s1", "/*init-first*/")
				return err
			},
			marker: bootstrapHas("/*init-first*/"),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.RemoveInitScript(ctx, c, "s1", "init1")
			},
			check: func(recs []fakewebkit.Record) string {
				if strings.Contains(lastBootstrap(recs), "/*init-first*/") {
					return "the last bootstrap script of page-2 keeps the removed init script"
				}
				return ""
			},
		},
		{
			name: "wip route added",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/route-first/*", Abort: true})
				return err
			},
			marker: hasRule(wipRegex("**/route-first/*")),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/route-second/*", Abort: true})
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.Contains(interceptionRules(recs), wipRegex("**/route-second/*")) {
					return "page-2 has no rule for the second route"
				}
				return ""
			},
		},
		{
			name: "wip route removed",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/route-first/*", Abort: true})
				return err
			},
			marker: hasRule(wipRegex("**/route-first/*")),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.RemoveRoutes(ctx, c, "s1", "")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if slices.Contains(interceptionRules(recs), wipRegex("**/route-first/*")) {
					return "page-2 keeps the rule of the removed route"
				}
				return ""
			},
		},
		{
			name: "js route added",
			first: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/js-first/*", Abort: true, Mode: ModeJS})
				return err
			},
			marker: bootstrapHas("js-first"),
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/js-second/*", Abort: true, Mode: ModeJS})
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !strings.Contains(lastBootstrap(recs), "js-second") {
					return "the last bootstrap script of page-2 lacks the second JS route"
				}
				return ""
			},
		},
		{
			name: "network log started", first: mediaDark, marker: darkMarker,
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, _, err := p.StartNetlog(ctx, c, "s1")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.ContainsFunc(recs, func(r fakewebkit.Record) bool { return r.Method == "Network.enable" }) {
					return "page-2 never got Network.enable for the network log"
				}
				return ""
			},
		},
		{
			// The owned Timeline domain is state; the recording is not
			// (the trace entry's Apply is a no-op), so page-2 gets
			// Timeline.enable and no Timeline.start.
			name: "trace started", first: mediaDark, marker: darkMarker,
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.StartTrace(ctx, c, "s1", false)
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.ContainsFunc(recs, func(r fakewebkit.Record) bool { return r.Method == "Timeline.enable" }) {
					return "page-2 never got Timeline.enable for the trace"
				}
				if slices.ContainsFunc(recs, func(r fakewebkit.Record) bool { return r.Method == "Timeline.start" }) {
					return "page-2 got Timeline.start; recordings stay on the active target"
				}
				return ""
			},
		},
		{
			name: "errors started", first: mediaDark, marker: darkMarker,
			change: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.StartErrors(ctx, c, "s1")
				return err
			},
			check: func(recs []fakewebkit.Record) string {
				if !slices.ContainsFunc(recs, func(r fakewebkit.Record) bool { return r.Method == "Console.enable" }) {
					return "page-2 never got Console.enable for the console capture"
				}
				if !strings.Contains(lastBootstrap(recs), ErrorHooks) {
					return "the last bootstrap script of page-2 lacks the error hooks"
				}
				return ""
			},
		},
	}
}

func TestStateChangedDuringASwapReachesTheCommittedTarget(t *testing.T) {
	credentialsVerified[transport.KindIWDP] = true
	t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
	for _, tc := range duringSwapCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.must(t, tc.first)
			<-r.f.Provisional("page-2")
			eventually(t, "page-2's setup sends the first state", func() bool {
				return slices.ContainsFunc(sentTo(r.f, "page-2"), tc.marker)
			})
			// Change returns after its repeated sends to provisional
			// targets ended (core.DesiredState.Change), so page-2's records
			// show the change before page-2 commits.
			r.must(t, tc.change)
			if msg := tc.check(sentTo(r.f, "page-2")); msg != "" {
				t.Fatal("before the commit: " + msg)
			}
			r.f.Commit("page-2")
			eventually(t, "the page connection commits page-2", func() bool { return activeTargetOf(r.page(t).ps.Caller) == "page-2" })
			if msg := tc.check(sentTo(r.f, "page-2")); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// A recording stop whose desired-state change cannot begin (another change
// holds the page past the command's deadline) changes nothing: the
// recording keeps running and a later stop ends it.
func TestRecordingStopIsRetriedAfterAFailedChange(t *testing.T) {
	r := newRig(t)
	later := func(method string, params any) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			r.f.Emit(method, params)
		}()
	}
	r.f.Handle("Timeline.stop", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		later("Timeline.recordingStopped", map[string]any{"endTime": 1})
		return map[string]any{}, nil
	})
	r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		later("ScriptProfiler.trackingComplete", map[string]any{"timestamp": 2, "samples": map[string]any{"stackTraces": []any{}}})
		return map[string]any{}, nil
	})
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		if err := p.StartTrace(ctx, c, "s1", false); err != nil {
			return err
		}
		return p.StartProfiler(ctx, c, "s1")
	})
	p := r.page(t)
	inside, release, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		held <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, _, err := p.StopTrace(short, c, "s1"); err == nil {
			t.Error("trace stop succeeded while another change held the page past its deadline")
		}
		if _, err := p.StopProfiler(short, c, "s1"); err == nil {
			t.Error("profiler stop succeeded while another change held the page past its deadline")
		}
		return nil
	})
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		if _, _, err := p.StopTrace(ctx, c, "s1"); err != nil {
			return fmt.Errorf("retried trace stop: %w", err)
		}
		if _, err := p.StopProfiler(ctx, c, "s1"); err != nil {
			return fmt.Errorf("retried profiler stop: %w", err)
		}
		return nil
	})
}

// An override is recorded before its sends (the contract of
// core.DesiredState.Change): a provisional target whose setup runs while
// the override's send to the active target is in flight takes the
// override from its snapshot. Recorded after the send, the override would
// be in neither the snapshot nor the target's queue.
func TestOverrideRecordedBeforeItsSend(t *testing.T) {
	r := newRig(t)
	r.f.DelayReplies("Page.overrideUserAgent", 400*time.Millisecond)
	active := r.f.ActiveTarget()
	changed := make(chan error, 1)
	go func() {
		changed <- r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetUserAgent(ctx, c, "s1", "UA-second")
		})
	}()
	eventually(t, "the active target receives the override", func() bool {
		return slices.ContainsFunc(sentTo(r.f, active), func(rec fakewebkit.Record) bool {
			return is(rec, "Page.overrideUserAgent", map[string]any{"value": "UA-second"})
		})
	})
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", time.Second); close(swapped) }()
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	<-swapped
	rec, _ := last(sentTo(r.f, "page-2"), func(r fakewebkit.Record) bool { return r.Method == "Page.overrideUserAgent" })
	if params(rec)["value"] != "UA-second" {
		t.Fatalf("page-2, set up during the override's send, has user agent %s, want UA-second", rec.Params)
	}
}
