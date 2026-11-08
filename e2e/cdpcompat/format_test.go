//go:build cdpcompat

package cdpcompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// clientResult is the result file of one client script run (README.md,
// "Result format"; written by clients/result.mjs).
type clientResult struct {
	Client   string `json:"client"`
	Version  string `json:"version"`
	Scenario string `json:"scenario"`
	Form     string `json:"form"` // ws or http, from the URL's scheme
	URL      string `json:"url"`  // the URL exactly as the script received it
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Steps    []struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		MS    int    `json:"ms"`
		Error string `json:"error,omitempty"`
	} `json:"steps"`
	Sent []sentCommand `json:"sent"`
}

// sentCommand is one CDP command that a client sent, in send order.
type sentCommand struct {
	Method    string `json:"method"`
	SessionID string `json:"sessionId"` // "" for the browser session
}

// decodeStrict decodes exactly one JSON value into v: unknown fields are
// errors, and only whitespace may follow the value.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("data after the JSON value at offset %d", dec.InputOffset())
	}
	return nil
}

// parseResult reads a result file strictly: unknown fields and a missing
// client, version, scenario, form or url are errors.
func parseResult(data []byte) (clientResult, error) {
	var r clientResult
	if err := decodeStrict(data, &r); err != nil {
		return r, fmt.Errorf("result file: %w", err)
	}
	for name, v := range map[string]string{"client": r.Client, "version": r.Version, "scenario": r.Scenario, "form": r.Form, "url": r.URL} {
		if v == "" {
			return r, fmt.Errorf("result file: %s is empty", name)
		}
	}
	if r.Form != "ws" && r.Form != "http" {
		return r, fmt.Errorf("result file: form %q, want ws or http", r.Form)
	}
	return r, nil
}

// step returns the named step of r and whether it ran.
func (r clientResult) step(name string) (ok bool, errText string, ran bool) {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.OK, s.Error, true
		}
	}
	return false, "", false
}

// methods returns the distinct methods r sent, in first-send order.
func (r clientResult) methods() []string {
	var out []string
	for _, s := range r.Sent {
		if !slices.Contains(out, s.Method) {
			out = append(out, s.Method)
		}
	}
	return out
}

// The clients of the suite, as the result files and the expected-missing
// lists name them.
var knownClients = []string{"agent-browser", "chrome-devtools-mcp", "playwright-core", "puppeteer-core"}

// missingEntry is one method of a client's expected-missing list: an
// unknown method (design 5.4) that the client may send without failing a
// scenario, because it belongs to an optional client feature.
type missingEntry struct {
	Method  string `json:"method"`  // Domain.method
	Feature string `json:"feature"` // the optional client feature that sends it
	Review  string `json:"review"`  // who reviewed it, when, and the evidence
}

// expectedMissing maps each client to its list (expected-missing.json).
type expectedMissing map[string][]missingEntry

// cdpMethod is the form of a CDP method name.
var cdpMethod = regexp.MustCompile(`^[A-Z][A-Za-z]*\.[a-z][A-Za-z]*$`)

// parseExpectedMissing reads expected-missing.json strictly: every known
// client has a list, no other client appears, and each entry names a CDP
// method once per client, its feature and a review note.
func parseExpectedMissing(data []byte) (expectedMissing, error) {
	var em expectedMissing
	if err := decodeStrict(data, &em); err != nil {
		return nil, fmt.Errorf("expected-missing list: %w", err)
	}
	for client := range em {
		if !slices.Contains(knownClients, client) {
			return nil, fmt.Errorf("expected-missing list: unknown client %q; want one of %v", client, knownClients)
		}
	}
	for _, client := range knownClients {
		entries, ok := em[client]
		if !ok {
			return nil, fmt.Errorf("expected-missing list: no list for %s (an empty list is [])", client)
		}
		seen := map[string]bool{}
		for i, e := range entries {
			switch {
			case !cdpMethod.MatchString(e.Method):
				return nil, fmt.Errorf("expected-missing list: %s entry %d: method %q is not Domain.method", client, i, e.Method)
			case seen[e.Method]:
				return nil, fmt.Errorf("expected-missing list: %s lists %s twice", client, e.Method)
			case strings.TrimSpace(e.Feature) == "":
				return nil, fmt.Errorf("expected-missing list: %s %s names no optional feature", client, e.Method)
			case strings.TrimSpace(e.Review) == "":
				return nil, fmt.Errorf("expected-missing list: %s %s has no review note", client, e.Method)
			}
			seen[e.Method] = true
		}
	}
	return em, nil
}

// loadExpectedMissing reads expected-missing.json of this directory.
func loadExpectedMissing(t *testing.T) expectedMissing {
	t.Helper()
	data, err := os.ReadFile("expected-missing.json")
	if err != nil {
		t.Fatal(err)
	}
	em, err := parseExpectedMissing(data)
	if err != nil {
		t.Fatal(err)
	}
	return em
}

// unexpectedUnknown applies the rule of design 5.4: every unknown method
// that the endpoint counted during a client's scenario (cdp log) must be
// on that client's expected-missing list. It returns an error that lists
// each other method with its count, or nil.
func (em expectedMissing) unexpectedUnknown(client string, unknown map[string]int) error {
	var bad []string
	for m, n := range unknown {
		if !slices.ContainsFunc(em[client], func(e missingEntry) bool { return e.Method == m }) {
			bad = append(bad, fmt.Sprintf("%s (%d)", m, n))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s sent methods the endpoint does not translate and its expected-missing list does not hold: %s; translate them (internal/cdp) or add reviewed entries to e2e/cdpcompat/expected-missing.json", client, strings.Join(bad, ", "))
}

// The checked-in lists are valid.
func TestExpectedMissingFile(t *testing.T) {
	loadExpectedMissing(t)
}

// The rule of design 5.4: an unknown method that is not on the client's
// list fails; one on the list passes; a list of another client does not
// count.
func TestUnknownMethodRule(t *testing.T) {
	em := expectedMissing{
		"playwright-core": {{Method: "Inspector.enable", Feature: "inspector pause", Review: "r"}},
		"puppeteer-core":  {},
	}
	if err := em.unexpectedUnknown("playwright-core", map[string]int{"Inspector.enable": 2}); err != nil {
		t.Fatalf("a listed method failed: %v", err)
	}
	if err := em.unexpectedUnknown("playwright-core", nil); err != nil {
		t.Fatalf("no unknown method failed: %v", err)
	}
	err := em.unexpectedUnknown("puppeteer-core", map[string]int{"Inspector.enable": 1, "Network.enable": 3})
	if err == nil || !strings.Contains(err.Error(), "Inspector.enable (1), Network.enable (3)") {
		t.Fatalf("methods missing from puppeteer-core's list: %v, want both named with counts", err)
	}
	if err := em.unexpectedUnknown("playwright-core", map[string]int{"Network.enable": 1}); err == nil {
		t.Fatal("an unlisted method passed")
	}
}

// parseExpectedMissing refuses each malformed list.
func TestExpectedMissingFormat(t *testing.T) {
	entry := func(s string) string {
		return `{"agent-browser": [], "chrome-devtools-mcp": [], "puppeteer-core": [], "playwright-core": [` + s + `]}`
	}
	ok := `{"method": "Inspector.enable", "feature": "f", "review": "r"}`
	if _, err := parseExpectedMissing([]byte(entry(ok))); err != nil {
		t.Fatalf("a valid list: %v", err)
	}
	for _, c := range []struct{ name, data, want string }{
		{"missing client", `{"agent-browser": [], "chrome-devtools-mcp": [], "puppeteer-core": []}`, "no list for playwright-core"},
		{"unknown client", `{"agent-browser": [], "chrome-devtools-mcp": [], "puppeteer-core": [], "playwright-core": [], "selenium": []}`, "unknown client"},
		{"bad method", entry(`{"method": "enable", "feature": "f", "review": "r"}`), "not Domain.method"},
		{"twice", entry(ok + "," + ok), "twice"},
		{"no feature", entry(`{"method": "Inspector.enable", "feature": " ", "review": "r"}`), "no optional feature"},
		{"no review", entry(`{"method": "Inspector.enable", "feature": "f"}`), "no review note"},
		{"unknown field", entry(`{"method": "Inspector.enable", "feature": "f", "review": "r", "why": "x"}`), "unknown field"},
		{"trailing garbage", entry(ok) + " x", "data after the JSON value"},
		{"second object", entry(ok) + "\n" + entry(ok), "data after the JSON value"},
	} {
		if _, err := parseExpectedMissing([]byte(c.data)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", c.name, err, c.want)
		}
	}
}

// parseResult refuses a result without its identity, with unknown fields,
// or with data after the JSON object; trailing whitespace is accepted.
func TestResultFormat(t *testing.T) {
	good := `{"client":"puppeteer-core","version":"25.16.0","scenario":"resolve","form":"http","url":"http://x","ok":true,"steps":[{"name":"connect","ok":false,"ms":3,"error":"e"}],"sent":[{"method":"Target.getBrowserContexts","sessionId":""},{"method":"Target.getBrowserContexts","sessionId":"s"}]}`
	r, err := parseResult([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if ok, e, ran := r.step("connect"); ok || e != "e" || !ran {
		t.Fatalf("step connect = %v %q %v", ok, e, ran)
	}
	if m := r.methods(); !slices.Equal(m, []string{"Target.getBrowserContexts"}) {
		t.Fatalf("methods = %v", m)
	}
	if _, err := parseResult([]byte(good + " \n\t")); err != nil {
		t.Fatalf("a result with trailing whitespace: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"client":"puppeteer-core",`, "", 1),
		strings.Replace(good, `"form":"http"`, `"form":"tcp"`, 1),
		strings.Replace(good, `"ok":true`, `"ok":true,"extra":1`, 1),
		good + " x",        // trailing garbage
		good + "\n" + good, // a second object
	} {
		if _, err := parseResult([]byte(bad)); err == nil {
			t.Errorf("parseResult(%s) passed", bad)
		}
	}
}
