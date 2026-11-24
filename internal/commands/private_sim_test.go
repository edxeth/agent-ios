package commands

import (
	"context"
	"encoding/json"
	"errors"
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

// sim screenshot takes its file once, as path or -o, and refuses both as
// a usage error before it reaches the Mac.
func TestSimScreenshotTakesTheFileOnce(t *testing.T) {
	c, _ := Lookup([]string{"sim", "screenshot"})
	if c == nil || c.Check == nil {
		t.Fatal("sim screenshot has no Check")
	}
	if err := c.Check(Input{Args: []string{"a.png"}, Flags: map[string]any{"output": "b.png"}}); err == nil {
		t.Error("path and -o together passed the check")
	}
	for _, in := range []Input{{Args: []string{"a.png"}, Flags: map[string]any{}}, {Flags: map[string]any{"output": "b.png"}}, {Flags: map[string]any{}}} {
		if err := c.Check(in); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

// The touch commands check their flags as usage errors before they reach
// the Mac: one of --label and --id, numbers for --hold and --duration. Arguments are not Check's (help runs it with the
// arguments' names); TestSimTapNeedsAPointOrAnElement covers them.
func TestSimTouchChecks(t *testing.T) {
	check := func(name string, in Input) error {
		c, _ := Lookup(strings.Fields(name))
		if c == nil || c.Check == nil {
			t.Fatalf("%s has no Check", name)
		}
		if in.Flags == nil {
			in.Flags = map[string]any{}
		}
		return c.Check(in)
	}
	for _, bad := range []struct {
		name string
		in   Input
	}{
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "id": "d"}}},
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "hold": "long"}}},
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "hold": "0"}}},
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "hold": "-1"}}},
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "hold": "11"}}},
		{"sim swipe", Input{Args: []string{"1", "2", "3", "4"}, Flags: map[string]any{"duration": "0"}}},
		{"sim swipe", Input{Args: []string{"1", "2", "3", "4"}, Flags: map[string]any{"duration": "NaN"}}},
		{"sim swipe", Input{Args: []string{"1", "2", "3", "4"}, Flags: map[string]any{"duration": "1s"}}},
	} {
		if err := check(bad.name, bad.in); err == nil {
			t.Errorf("%s %+v passed the check", bad.name, bad.in)
		}
	}
	for _, good := range []struct {
		name string
		in   Input
	}{
		{"sim tap", Input{Args: []string{"201", "571.5"}}},
		{"sim tap", Input{Flags: map[string]any{"label": "Done", "hold": "1"}}},
		{"sim swipe", Input{Args: []string{"200", "600", "200", "300"}, Flags: map[string]any{"duration": "0.5"}}},
	} {
		if err := check(good.name, good.in); err != nil {
			t.Errorf("%s %+v: %v", good.name, good.in, err)
		}
	}
}

// sim tap without a point or an element, with a lone x, or with a point
// that is not a number, is a usage error when it runs, before it reaches
// the Mac; so is a swipe whose points are not numbers.
func TestSimTapNeedsAPointOrAnElement(t *testing.T) {
	for _, in := range []Input{
		{Flags: map[string]any{}},
		{Args: []string{"1"}, Flags: map[string]any{}},
		{Args: []string{"1", "x"}, Flags: map[string]any{}},
		{Args: []string{"-1", "2"}, Flags: map[string]any{}},
		{Args: []string{"1", "Inf"}, Flags: map[string]any{}},
		{Args: []string{"1", "2"}, Flags: map[string]any{"label": "Done"}},
	} {
		var u *UsageError
		if _, err := simTapArgs(in); !errors.As(err, &u) {
			t.Errorf("%+v: %v, want a usage error", in, err)
		}
	}
	if args, err := simTapArgs(Input{Args: []string{"201", "571"}, Flags: map[string]any{"hold": "1"}}); err != nil || strings.Join(args, " ") != "tap -x 201 -y 571 --hold 1" {
		t.Errorf("point: %q, %v", args, err)
	}
	var u *UsageError
	if _, err := runSimKeyboard(context.Background(), nil, Input{Args: []string{"virtual"}, Flags: map[string]any{}}); !errors.As(err, &u) {
		t.Errorf("sim keyboard virtual: %v, want a usage error before anything runs", err)
	}
	for _, args := range [][]string{{"1", "2", "3", "x"}, {"1", "2", "-3", "4"}, {"5", "6", "5", "6.0"}} {
		if _, err := simSwipeArgs(Input{Args: args, Flags: map[string]any{}}); !errors.As(err, &u) {
			t.Errorf("swipe %q: %v, want a usage error", args, err)
		}
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
