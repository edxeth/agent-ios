package commands

import (
	"testing"
	"time"
)

// Duration flags take a bare non-negative number as seconds (agents write
// "sim reserve --wait 40" and got a usage error), except a flag with
// NeedsUnit, whose bare number other tools read as milliseconds: there it
// stays a usage error (exit 2) instead of a guess.
func TestDurationFlagsTakeBareSeconds(t *testing.T) {
	noEnv := func(string) string { return "" }
	inv, err := Parse([]string{"sim", "reserve", "--wait", "40"}, noEnv)
	if err != nil {
		t.Fatalf("sim reserve --wait 40: %v", err)
	}
	if got := inv.Input.Flags["wait"]; got != 40*time.Second {
		t.Fatalf("sim reserve --wait 40 = %v, want 40s", got)
	}
	if _, err := Parse([]string{"type", "hello", "--delay", "50"}, noEnv); ExitCodeOf(err) != ExitUsage {
		t.Fatalf("type --delay 50: %v (exit %d), want a usage error", err, ExitCodeOf(err))
	}
	inv, err = Parse([]string{"type", "hello", "--delay", "50ms"}, noEnv)
	if err != nil || inv.Input.Flags["delay"] != 50*time.Millisecond {
		t.Fatalf("type --delay 50ms: %v, %v", inv.Input.Flags["delay"], err)
	}

	cases := []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"40", 40 * time.Second, true},
		{"0", 0, true},
		{"1.5", 1500 * time.Millisecond, true},
		{"90", 90 * time.Second, true},
		{"40s", 40 * time.Second, true},
		{"300ms", 300 * time.Millisecond, true},
		{"1m30s", 90 * time.Second, true},
		{"-5", 0, false},
		{"5x", 0, false},
		{"1.", 0, false},
		{".5", 0, false},
		{"", 0, false},
		{"99999999999999999999", 0, false}, // too large for a time.Duration
	}
	n := 0
	for _, c := range All() {
		for _, f := range c.Flags {
			if f.Type != FlagDuration {
				continue
			}
			n++
			for _, tc := range cases {
				got, err := parseFlagValue(f, tc.v)
				bare := bareSeconds.MatchString(tc.v)
				switch {
				case f.NeedsUnit && bare:
					if err == nil {
						t.Errorf("%s --%s %q = %v, want an error: the flag needs a unit", c.Name, f.Name, tc.v, got)
					}
				case tc.ok && (err != nil || got != tc.want):
					t.Errorf("%s --%s %q = %v, %v; want %v", c.Name, f.Name, tc.v, got, err, tc.want)
				case !tc.ok && err == nil:
					t.Errorf("%s --%s %q = %v, want an error", c.Name, f.Name, tc.v, got)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no registered command has a duration flag")
	}
}
