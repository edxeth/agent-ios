package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

// sim up refuses a --url that is not an absolute http or https URL as a
// usage error, before it starts or changes anything.
func TestSimUpChecksTheURLBeforeItRuns(t *testing.T) {
	c, _ := Lookup([]string{"sim", "up"})
	if c == nil || c.Check == nil {
		t.Fatal("sim up has no Check")
	}
	for _, bad := range []string{"file:///etc/passwd", "example.com", "http://"} {
		if err := c.Check(Input{Flags: map[string]any{"url": bad}}); err == nil {
			t.Errorf("--url %q passed the check", bad)
		}
	}
	if err := c.Check(Input{Flags: map[string]any{"url": "http://10.0.2.2:8080/"}}); err != nil {
		t.Errorf("a good URL: %v", err)
	}
}

// In private mode sim up and sim status print their steps and states, and
// never the simulator's name, UDID, host or socket path.
func TestPrivateSimSummaries(t *testing.T) {
	secret := []string{"iPhone 17 Pro", "E485E2B7-A0F9-413B-AB85-EFF4F63398F5", "/private/var/tmp/x/com.apple.webinspectord_sim.socket"}
	up := json.RawMessage(`{"device":{"udid":"E485E2B7-A0F9-413B-AB85-EFF4F63398F5","name":"iPhone 17 Pro","state":"Booted"},"macStarted":true,"booted":true,"windowOpened":false,"safariStarted":true,"forwardStarted":true,"iwdpStarted":false,"iwdpRestarted":true,"inspectorSocket":"/private/var/tmp/x/com.apple.webinspectord_sim.socket","pages":2}`)
	st := json.RawMessage(`{"macReachable":true,"loggedIn":true,"device":{"udid":"E485E2B7-A0F9-413B-AB85-EFF4F63398F5","name":"iPhone 17 Pro","state":"Booted"},"window":true,"forward":true,"listed":false,"ready":false}`)
	for _, tc := range []struct {
		cmd  string
		data json.RawMessage
		want string
	}{
		{"sim up", up, "mac started true\nbooted true\nwindow opened false\nsafari started true\nforward started true\niwdp started false\niwdp restarted true\ntabs 2"},
		{"sim status", st, "ready false\nmac answers true\nlogged in true\nbooted true\nwindow true\nforward true\niwdp lists false"},
	} {
		text, value, ok := privateSummaries[tc.cmd](tc.data)
		if !ok || text != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.cmd, text, ok, tc.want)
		}
		js, _ := json.Marshal(value)
		for _, s := range secret {
			if strings.Contains(text, s) || strings.Contains(string(js), s) {
				t.Errorf("%s prints %q in private mode: %s %s", tc.cmd, s, text, js)
			}
		}
	}
}
