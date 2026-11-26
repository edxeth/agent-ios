package wip

import "testing"

// Design 2.10 and 5.4: a raw call of a method that is not in the device's
// iOS command list is refused. Expected lists come from WebKit's
// iOS-<version>-InspectorBackendCommands.js files.
func TestSupported(t *testing.T) {
	cases := []struct {
		os, method string
		known, ok  bool
	}{
		{"16.7.10", "Runtime.evaluate", true, true},
		{"16.7.10", "Page.snapshotRect", true, true},
		// DOM.getMediaStats first appears in iOS 18.0's list.
		{"16.7.10", "DOM.getMediaStats", true, false},
		{"17.0", "DOM.getMediaStats", true, false},
		{"18.0", "DOM.getMediaStats", true, true},
		{"16.7.10", "Emulation.setDeviceMetricsOverride", true, false}, // CDP only
		{"16.7.10", "Bogus", true, false},
		{"16.3", "Runtime.evaluate", true, true},  // uses the 16.0 list
		{"15.8", "Runtime.evaluate", false, true}, // older than every list: no gate
		{"", "Anything.goes", false, true},        // unknown version (WebKitGTK, mock): no gate
		{"garbage", "Anything.goes", false, true},
		{"0.0.0", "Anything.goes", false, true},  // iwdp's version of the iOS Simulator: unknown
		{"99.1", "Runtime.evaluate", true, true}, // newer than every list: the newest list
	}
	for _, c := range cases {
		known, ok := Supported(c.os, c.method)
		if known != c.known || ok != c.ok {
			t.Errorf("Supported(%q, %q) = %v, %v; want %v, %v", c.os, c.method, known, ok, c.known, c.ok)
		}
	}
}

// FIX-K (sim recheck claim 1): iwdp lists the iOS Simulator as 0.0.0, which
// is an unknown version like "" (WebKitGTK), not iOS 0: the throttle takes
// the parameters of current WebKit (bandwidth), as for every unknown version.
func TestEmulatedConditionsBandwidth(t *testing.T) {
	for _, c := range []struct {
		os   string
		want bool
	}{
		{"16.7.10", false},
		{"17.0", false},
		{"27.2", false},
		{"27.3", true},
		{"28.0", true},
		{"", true},
		{"garbage", true},
		{"0.0.0", true},
	} {
		if got := EmulatedConditionsBandwidth(c.os); got != c.want {
			t.Errorf("EmulatedConditionsBandwidth(%q) = %v, want %v", c.os, got, c.want)
		}
	}
}

// KnownVersion: a version that names an iOS release; "", an unparsable
// text and iwdp's 0.0.0 of the iOS Simulator do not.
func TestKnownVersion(t *testing.T) {
	for v, want := range map[string]bool{"16.7.10": true, "26.5": true, "18": true, "": false, "garbage": false, "0.0.0": false, "0": false} {
		if got := KnownVersion(v); got != want {
			t.Errorf("KnownVersion(%q) = %v, want %v", v, got, want)
		}
	}
}
