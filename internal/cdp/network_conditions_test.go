package cdp

import (
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Parity 0.45 network conditions (network_conditions.go): what
// puppeteer-core 25.18.1 sends for page.emulateNetworkConditions
// (NetworkManager #applyNetworkConditions) and what WIP receives, per iOS
// version class of the command lists (internal/wip; the WIP parameters
// from the iOS 16.4, 17.0 and 27.3 InspectorBackendCommands.js).

// versionPage attaches a client to a page of a new device with iOS
// version v, as Puppeteer does.
func versionPage(t *testing.T, v string) (*cdptest.Client, string, *fakewebkit.Server) {
	t.Helper()
	r := newRig(t)
	dev := r.l.AddDevice("00008020-0000000000000V"+strings.ReplaceAll(v, ".", ""), "iPhone V", v)
	if _, err := r.res.Reserve(dev.ID, "o1", time.Hour); err != nil {
		t.Fatal(err)
	}
	f := r.addPage(t, dev, "V")
	c := dial(t, r.issue(t, "s1", "V", ScopePage).WS)
	_, sid := puppeteerAttach(t, c)
	return c, sid, f
}

// pptrConditions sends what Puppeteer sends for one
// emulateNetworkConditions call and returns the error of the rule call.
func pptrConditions(t *testing.T, c *cdptest.Client, sid string, offline bool, latency, download, upload float64) error {
	t.Helper()
	call(t, c, sid, "Network.overrideNetworkState", map[string]any{"offline": offline, "latency": latency, "downloadThroughput": download, "uploadThroughput": upload})
	_, err := c.Call(testCtx(t), sid, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
		map[string]any{"urlPattern": "", "offline": offline, "latency": latency, "downloadThroughput": download, "uploadThroughput": upload},
	}})
	return err
}

func conditionsSent(f *fakewebkit.Server) []string {
	var out []string
	for _, m := range f.ReceivedMethod("Network.setEmulatedConditions") {
		out = append(out, string(m.Params))
	}
	return out
}

// iOS 16.4 to 17.x: a download limit is WIP bytesPerSecondLimit; latency
// and upload limits are refused before any change; the clear that
// Puppeteer sends (latency 0, throughput -1) clears only a throttle this
// attachment set; overrideNetworkState changes nothing.
func TestNetworkConditionsByRuleOnIOS16(t *testing.T) {
	c, sid, f := versionPage(t, "16.7.10")
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatalf("clear before any throttle: %v", err)
	}
	if got := conditionsSent(f); len(got) != 0 {
		t.Fatalf("a clear of nothing sent %v", got)
	}
	if err := pptrConditions(t, c, sid, false, 0, 50000, -1); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]float64{{100, 50000, -1}, {0, 50000, 1000}} {
		err := pptrConditions(t, c, sid, false, bad[0], bad[1], bad[2])
		if err == nil || !strings.Contains(err.Error(), "cannot be emulated") {
			t.Fatalf("latency %v upload %v: %v, want the refusal", bad[0], bad[2], err)
		}
	}
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(conditionsSent(f), " "); got != `{"bytesPerSecondLimit":50000} {}` {
		t.Fatalf("WIP received %s, want the limit and one clear", got)
	}
	res := call(t, c, sid, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
		map[string]any{"urlPattern": "https://x.example/*", "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1},
		map[string]any{"urlPattern": "", "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1},
	}})
	if string(res) != `{"ruleIds":["1","2"]}` {
		t.Fatalf("ruleIds %s", res)
	}
	if e := callErr(t, c, sid, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
		map[string]any{"urlPattern": "https://x.example/*", "offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1},
	}}); !strings.Contains(e.Message, "some URLs only") {
		t.Fatalf("a per-URL offline rule: %v", e)
	}
}

// iOS 18.0 to 27.2 lack Network.setEmulatedConditions: a throttle is
// refused with the command-list reason, and Puppeteer's clear succeeds
// without a WIP call (chrome-devtools-mcp's emulate sends it on every call).
func TestNetworkConditionsByRuleOnIOS18(t *testing.T) {
	c, sid, f := versionPage(t, "18.0")
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatalf("clear: %v", err)
	}
	err := pptrConditions(t, c, sid, false, 0, 50000, -1)
	if err == nil || !strings.Contains(err.Error(), "cannot be emulated on iOS 18.0") {
		t.Fatalf("throttle on iOS 18.0: %v, want the command-list refusal", err)
	}
	if got := conditionsSent(f); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}

// iOS 27.3 takes bandwidth and latency (WebKit commit c4a7ebcc295f): both
// reach WIP; the clear sends {}.
func TestNetworkConditionsByRuleOnIOS273(t *testing.T) {
	c, sid, f := versionPage(t, "27.3")
	if err := pptrConditions(t, c, sid, false, 400, 50000, -1); err != nil {
		t.Fatal(err)
	}
	call(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	if got := strings.Join(conditionsSent(f), " "); got != `{"bandwidth":50000,"latency":400} {}` {
		t.Fatalf("WIP received %s, want bandwidth and latency, then the clear", got)
	}
}

// Offline is emulated through interception (as agent-ios set offline): one
// rule for every URL, and each request it pauses fails with WIP's General
// error; turning it off removes the rule; Network.disable drops it, so a
// later offline installs it again; the detach releases it.
func TestNetworkOfflineThroughInterception(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	if err := pptrConditions(t, c, sid, true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	rules := f.ReceivedMethod("Network.addInterception")
	if len(f.ReceivedMethod("Network.setInterceptionEnabled")) != 1 || len(rules) != 1 || !strings.Contains(string(rules[0].Params), `"stage":"request"`) {
		t.Fatalf("WIP interception %v, want one request-stage rule", rules)
	}
	f.InterceptEvent("wk-One", "7.1", "request", map[string]any{"request": map[string]any{"url": "https://one.example/a", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "7.1", "Network.interceptRequestWithError")
	if got := f.ReceivedMethod("Network.interceptRequestWithError"); !strings.Contains(string(got[0].Params), `"errorType":"General"`) {
		t.Fatalf("interceptRequestWithError %s", got[0].Params)
	}
	if err := pptrConditions(t, c, sid, true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReceivedMethod("Network.addInterception")); n != 1 {
		t.Fatalf("a repeated offline sent %d rules", n)
	}
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got := f.ReceivedMethod("Network.removeInterception"); len(got) != 1 || string(got[0].Params) != string(rules[0].Params) {
		t.Fatalf("removeInterception %v, want the offline rule", got)
	}
	call(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	call(t, c, sid, "Network.disable", nil)
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	if n := len(f.ReceivedMethod("Network.addInterception")); n != 3 {
		t.Fatalf("offline after Network.disable sent %d rules in all, want 3", n)
	}
	before := len(f.ReceivedMethod("Network.removeInterception"))
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	waitUntil(t, "the detach removes the offline rule", func() bool { return len(f.ReceivedMethod("Network.removeInterception")) > before })
}
