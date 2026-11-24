package commands

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
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

// durationInput is a generated value of a duration flag with its expected
// reading, computed from its parts alone, not with time.ParseDuration:
//   - bare: a whole number of seconds with an optional decimal fraction
//     of up to 9 digits ("40", "1.5"); want is that many seconds;
//   - unit: one to three amounts with a unit each ("1m30s", "250ms",
//     "3µs"); want is their sum;
//   - invalid: a bare or unit form spoiled so that it is neither: a sign
//     before a bare number, a dot without digits on one side, a space, an
//     unknown unit, or nothing.
type durationInput struct {
	text string
	want time.Duration
	kind string // "bare", "unit" or "invalid"
}

var durationUnits = []struct {
	name string
	d    time.Duration
}{
	{"ns", time.Nanosecond},
	{"us", time.Microsecond},
	{"µs", time.Microsecond},
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"m", time.Minute},
	{"h", time.Hour},
}

func bareInput(r *rand.Rand) durationInput {
	whole := r.Int63n(1_000_000)
	text, want := strconv.FormatInt(whole, 10), time.Duration(whole)*time.Second
	if r.Intn(2) == 0 {
		var frac strings.Builder
		for range 1 + r.Intn(9) {
			frac.WriteByte(byte('0' + r.Intn(10)))
		}
		f := frac.String()
		ns, _ := strconv.ParseInt((f + "000000000")[:9], 10, 64)
		text, want = text+"."+f, want+time.Duration(ns)
	}
	return durationInput{text: text, want: want, kind: "bare"}
}

func unitInput(r *rand.Rand) durationInput {
	var b strings.Builder
	var want time.Duration
	for range 1 + r.Intn(3) {
		u := durationUnits[r.Intn(len(durationUnits))]
		n := r.Int63n(1000)
		b.WriteString(strconv.FormatInt(n, 10) + u.name)
		want += time.Duration(n) * u.d
	}
	return durationInput{text: b.String(), want: want, kind: "unit"}
}

func (durationInput) Generate(r *rand.Rand, _ int) reflect.Value {
	switch r.Intn(3) {
	case 0:
		return reflect.ValueOf(bareInput(r))
	case 1:
		return reflect.ValueOf(unitInput(r))
	}
	bare, unit := bareInput(r).text, unitInput(r).text
	whole := strconv.FormatInt(r.Int63n(1000), 10)
	spoiled := []string{
		"-" + bare,
		"+" + bare,
		whole + ".",
		"." + whole,
		whole + "..5",
		" " + bare,
		bare + " ",
		bare[:len(bare)/2] + " " + bare[len(bare)/2:],
		bare + "d",
		unit + "x",
		unit + " " + unit,
		"",
	}
	return reflect.ValueOf(durationInput{text: spoiled[r.Intn(len(spoiled))], kind: "invalid"})
}

// Final review F5: the command duration parser, against generated values:
// a bare number is that many seconds, a unit form is the sum of its parts,
// and a spoiled form is refused, on every registered duration flag; a
// flag with NeedsUnit (type --delay, keyboard --delay) refuses the bare
// number and reads the other forms alike.
func TestDurationFlagsProperty(t *testing.T) {
	var flags []Flag
	for _, c := range All() {
		for _, f := range c.Flags {
			if f.Type == FlagDuration {
				flags = append(flags, f)
			}
		}
	}
	needsUnit := false
	for _, f := range flags {
		needsUnit = needsUnit || f.NeedsUnit
	}
	if len(flags) == 0 || !needsUnit {
		t.Fatalf("duration flags %v: want some, with one that needs a unit", flags)
	}
	prop := func(in durationInput) bool {
		for _, f := range flags {
			got, err := parseFlagValue(f, in.text)
			refuse := in.kind == "invalid" || (in.kind == "bare" && f.NeedsUnit)
			switch {
			case refuse && err == nil:
				t.Errorf("--%s (needs a unit: %t) %q = %v; want an error", f.Name, f.NeedsUnit, in.text, got)
				return false
			case !refuse && (err != nil || got != in.want):
				t.Errorf("--%s (needs a unit: %t) %q = %v, %v; want %v", f.Name, f.NeedsUnit, in.text, got, err, in.want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261124))}); err != nil {
		t.Fatal(err)
	}
}
