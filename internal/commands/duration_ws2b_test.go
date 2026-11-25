package commands

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// baseParsePositiveDuration is the duration parser of --timeout,
// AGENT_IOS_TIMEOUT and --wait-lease before the registry took over these
// settings: parsePositiveDuration in cmd/agent-ios/deadline.go at f731071,
// copied unchanged. It is the reference of the tests below.
func baseParsePositiveDuration(v string) (time.Duration, error) {
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if parsed, err := time.ParseDuration(v); err == nil {
		d = parsed
	} else {
		return 0, errors.New("invalid duration")
	}
	if d <= 0 {
		return 0, errors.New("invalid duration")
	}
	return d, nil
}

// durationSettings parse v as each of the three duration settings and
// return the parsed value.
var durationSettings = []struct {
	name  string
	parse func(v string) (time.Duration, error)
}{
	{"--timeout", func(v string) (time.Duration, error) {
		inv, err := Parse([]string{"--timeout", v, "eval", "1"}, noEnv)
		if err != nil {
			return 0, err
		}
		return inv.Input.Global.Timeout, nil
	}},
	{"--timeout=", func(v string) (time.Duration, error) {
		inv, err := Parse([]string{"eval", "--timeout=" + v, "1"}, noEnv)
		if err != nil {
			return 0, err
		}
		return inv.Input.Global.Timeout, nil
	}},
	{"AGENT_IOS_TIMEOUT", func(v string) (time.Duration, error) {
		inv, err := Parse([]string{"eval", "1"}, func(k string) string {
			if k == "AGENT_IOS_TIMEOUT" {
				return v
			}
			return ""
		})
		if err != nil {
			return 0, err
		}
		return inv.Input.Global.Timeout, nil
	}},
	{"--wait-lease", func(v string) (time.Duration, error) {
		inv, err := Parse([]string{"--wait-lease", v, "eval", "1"}, noEnv)
		if err != nil {
			return 0, err
		}
		return inv.Input.Global.WaitLease, nil
	}},
}

func noEnv(string) string { return "" }

// bareThirty matches the example "30" of a bare second count, not "30s".
var bareThirty = regexp.MustCompile(`(^|[^0-9])30([^0-9a-zµ]|$)`)

// WS2b-1b-b item 1: --timeout, AGENT_IOS_TIMEOUT and --wait-lease accept what
// the base parser accepted, as the same values: a Go duration or a bare
// whole number of seconds (strconv.Atoi, so a leading "+" or zeros too),
// greater than zero. The expected values follow baseParsePositiveDuration.
func TestDurationSettingsAcceptBareSecondsAsTheBaseDid(t *testing.T) {
	accept := map[string]time.Duration{
		"5":          5 * time.Second,
		"45":         45 * time.Second,
		"+5":         5 * time.Second,
		"005":        5 * time.Second,
		"9223372036": 9223372036 * time.Second, // the largest whole second count a Duration holds
		"5s":         5 * time.Second,
		"1m30s":      90 * time.Second,
		"1.5s":       1500 * time.Millisecond,
		"300ms":      300 * time.Millisecond,
	}
	reject := []string{"0", "-5", "+0", "0s", "-1s", "5.5", "1e3", "0x10", " 5", "5 ", "bogus", "s"}
	for _, s := range durationSettings {
		for v, want := range accept {
			if base, err := baseParsePositiveDuration(v); err != nil || base != want {
				t.Fatalf("table error: base parser gives %v, %v for %q, table says %v", base, err, v, want)
			}
			got, err := s.parse(v)
			if err != nil || got != want {
				t.Errorf("%s %q: got %v, %v; want %v", s.name, v, got, err, want)
			}
		}
		for _, v := range reject {
			if _, err := baseParsePositiveDuration(v); err == nil {
				t.Fatalf("table error: base parser accepts %q", v)
			}
			got, err := s.parse(v)
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Errorf("%s %q: got %v, %v; want a usage error", s.name, v, got, err)
				continue
			}
			// The error names the setting, the value and both accepted
			// forms: a duration (30s) and a bare second count (30).
			name := strings.TrimSuffix(s.name, "=")
			for _, part := range []string{name, strconv.Quote(v), "30s"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s %q: error %q does not name %q", s.name, v, err, part)
				}
			}
			if !bareThirty.MatchString(err.Error()) {
				t.Errorf("%s %q: error %q does not name the bare form 30", s.name, v, err)
			}
		}
	}
}

// A bare second count whose nanoseconds overflow a Duration is refused.
// The base parser wrapped it around to an unrelated value (18446744074
// became about 0.3 s, -18446744073 about 0.7 s); time.ParseDuration
// refuses "18446744074s", and the bare form now does too.
func TestDurationSettingsRefuseOverflowingSeconds(t *testing.T) {
	for _, v := range []string{"9223372037", "18446744074", "-18446744073", strconv.Itoa(math.MaxInt)} {
		for _, s := range durationSettings {
			if got, err := s.parse(v); err == nil {
				t.Errorf("%s %q: got %v; want a usage error", s.name, v, got)
			}
		}
	}
}

// Property: for any value, each setting accepts it exactly when the base
// parser does and gives the same duration, apart from the overflowing
// second counts above.
func TestDurationSettingsMatchTheBaseParser(t *testing.T) {
	overflows := func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && (n > math.MaxInt64/int(time.Second) || n < math.MinInt64/int(time.Second))
	}
	check := func(v string) bool {
		// An empty AGENT_IOS_TIMEOUT is unset, in the base parser too.
		if v == "" || overflows(v) {
			return true
		}
		want, baseErr := baseParsePositiveDuration(v)
		for _, s := range durationSettings {
			got, err := s.parse(v)
			if (err == nil) != (baseErr == nil) || (err == nil && got != want) {
				t.Logf("%s %q: got %v, %v; base gives %v, %v", s.name, v, got, err, want, baseErr)
				return false
			}
		}
		return true
	}
	// Values from a small alphabet of digits, signs, units and dots, so
	// that many of them are numbers or durations.
	alphabet := []rune("0123456789+-.smhµun ")
	gen := func(r []byte) string {
		var b strings.Builder
		for _, c := range r {
			b.WriteRune(alphabet[int(c)%len(alphabet)])
		}
		return b.String()
	}
	if err := quick.Check(func(r []byte) bool { return check(gen(r)) }, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(func(n int64) bool { return check(strconv.FormatInt(n%100000, 10)) }, nil); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(check, nil); err != nil {
		t.Fatal(err)
	}
}
