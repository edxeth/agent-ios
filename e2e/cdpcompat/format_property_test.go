//go:build cdpcompat

package cdpcompat

import (
	"encoding/json"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// propConfig fixes the generator (seed 1), so a failure reproduces.
func propConfig() *quick.Config {
	return &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(1))}
}

var propMethods = []string{"Network.enable", "Inspector.enable", "Page.enable", "WebMCP.enable", "Log.enable", "Audits.enable"}

// The format contract of README.md, as properties (F4 of the W5c-2a
// review): a valid result and a valid expected-missing list round-trip
// through their parsers; the same data with any non-whitespace suffix is
// refused; the unknown-method rule fails exactly for the methods that are
// not on the client's list, and names each of them.
func TestFormatProperties(t *testing.T) {
	suffix := func(r *rand.Rand) string {
		s := []string{"x", "{}", "[]", "0", "null", `{"client":"x"}`, "\"s\""}[r.Intn(7)]
		return []string{"", " ", "\n", "\t "}[r.Intn(4)] + s
	}
	result := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		in := clientResult{
			Client: knownClients[r.Intn(len(knownClients))], Version: "1." + string(rune('0'+r.Intn(10))), Scenario: "resolve",
			Form: []string{"ws", "http"}[r.Intn(2)], URL: "ws://127.0.0.1:1/t/" + strings.Repeat("a", r.Intn(5)), OK: r.Intn(2) == 0,
		}
		for range r.Intn(4) {
			in.Sent = append(in.Sent, sentCommand{Method: propMethods[r.Intn(len(propMethods))], SessionID: []string{"", "S1"}[r.Intn(2)]})
		}
		data, err := json.Marshal(in)
		if err != nil {
			return false
		}
		out, err := parseResult(append(data, []byte(" \n")...))
		if err != nil || out.Client != in.Client || out.URL != in.URL || out.OK != in.OK || !slices.Equal(out.Sent, in.Sent) {
			t.Logf("round trip of %s: %+v, %v", data, out, err)
			return false
		}
		if _, err := parseResult(append(data, suffix(r)...)); err == nil {
			t.Logf("%s with a suffix was accepted", data)
			return false
		}
		return true
	}
	lists := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		em := expectedMissing{}
		for _, c := range knownClients {
			em[c] = []missingEntry{}
			for _, m := range propMethods {
				if r.Intn(3) == 0 {
					em[c] = append(em[c], missingEntry{Method: m, Feature: "f", Review: "r"})
				}
			}
		}
		data, err := json.Marshal(em)
		if err != nil {
			return false
		}
		back, err := parseExpectedMissing(data)
		if err != nil || len(back) != len(em) {
			t.Logf("round trip of %s: %v", data, err)
			return false
		}
		if _, err := parseExpectedMissing(append(data, suffix(r)...)); err == nil {
			t.Logf("%s with a suffix was accepted", data)
			return false
		}
		client := knownClients[r.Intn(len(knownClients))]
		unknown := map[string]int{}
		for _, m := range propMethods {
			if r.Intn(2) == 0 {
				unknown[m] = 1 + r.Intn(3)
			}
		}
		var off []string
		for m := range unknown {
			if !slices.ContainsFunc(em[client], func(e missingEntry) bool { return e.Method == m }) {
				off = append(off, m)
			}
		}
		err = em.unexpectedUnknown(client, unknown)
		if (err != nil) != (len(off) > 0) {
			t.Logf("rule for %s, list %v, unknown %v: %v; off the list %v", client, em[client], unknown, err, off)
			return false
		}
		for _, m := range off {
			if !strings.Contains(err.Error(), m+" (") {
				t.Logf("rule error %v does not name %s", err, m)
				return false
			}
		}
		return true
	}
	if err := quick.Check(result, propConfig()); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(lists, propConfig()); err != nil {
		t.Fatal(err)
	}
}
