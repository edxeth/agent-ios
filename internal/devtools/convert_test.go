package devtools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// URL globs as agent-browser and Playwright read them: "**" any
// characters, "*" any except "/", whole URL; no "*": substring.
func TestPatternRegex(t *testing.T) {
	cases := []struct {
		pattern, url string
		match        bool
	}{
		{"**/api/*", "https://a.test/v1/api/users", true},
		{"**/api/*", "https://a.test/api/users/1", false},
		{"**/api/**", "https://a.test/api/users/1", true},
		{"https://a.test/*.png", "https://a.test/logo.png", true},
		{"https://a.test/*.png", "https://a.test/img/logo.png", false},
		{"/api/", "https://a.test/v1/api/x?y", true},
		{"a.test/x", "https://aXtest/x", false}, // "." is literal
		{"**/q?x=1", "https://a.test/q?x=1", true},
	}
	for _, c := range cases {
		re, err := PatternRegex(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := regexp.MustCompile(re).MatchString(c.url); got != c.match {
			t.Errorf("%q (regex %q) on %q = %v, want %v", c.pattern, re, c.url, got, c.match)
		}
	}
	if _, err := PatternRegex(""); err == nil {
		t.Error("an empty pattern was accepted")
	}
}

func harFixture() ([]Request, map[string]HARBody) {
	ts := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	reqs := []Request{
		{
			ID: "1", URL: "https://a.test/login?user=bob&token=abc#x", Method: "POST", State: StateFinished, WallTime: ts,
			RequestHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": "sid=1; theme=dark", "Authorization": "Bearer t"},
			PostData:       "user=bob&password=hunter2", Status: 302, StatusText: "Found", MimeType: "text/html",
			ResponseHeaders: map[string]string{"Location": "/home", "Set-Cookie": "sid=2; Path=/"}, StartTS: 1, ResponseTS: 1.25, EndTS: 1.5, DataLength: 10,
		},
		{
			ID: "2", URL: "https://a.test/api", Method: "GET", State: StateFinished, WallTime: ts, MimeType: "application/json",
			Status: 200, StatusText: "OK", StartTS: 2, ResponseTS: 2.1, EndTS: 2.2, DataLength: 30,
		},
		{ID: "3", URL: "https://a.test/slow", Method: "GET", State: StateCaptureReset, Error: "closed", WallTime: ts, StartTS: 3},
	}
	bodies := map[string]HARBody{"2": {Text: `{"apiToken":"xyz","name":"ok"}`}, "1": {Err: "gone"}}
	return reqs, bodies
}

// harSchemaDir holds the published HAR schema (har-schema 2.0.0, ISC;
// see its README.md).
var harSchemaDir = filepath.Join("testdata", "har-schema")

// publishedHARSchema resolves har.json of the vendored published schema;
// its $refs ("log.json#", ...) load from the same directory. The files
// declare draft-06, which jsonschema-go does not accept; they are read as
// draft-07, which gives each keyword they use the same meaning (README.md).
func publishedHARSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	const base = "file:///har-schema/"
	load := func(name string) (*jsonschema.Schema, error) {
		raw, err := os.ReadFile(filepath.Join(harSchemaDir, name))
		if err != nil {
			return nil, err
		}
		raw = []byte(strings.Replace(string(raw), "http://json-schema.org/draft-06/schema#", "http://json-schema.org/draft-07/schema#", 1))
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return &s, nil
	}
	root, err := load("har.json")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := root.Resolve(&jsonschema.ResolveOptions{BaseURI: base, Loader: func(u *url.URL) (*jsonschema.Schema, error) {
		return load(path.Base(u.Path))
	}})
	if err != nil {
		t.Fatalf("resolving the published HAR schema: %v", err)
	}
	return rs
}

// 7.7 test 2: the HAR validates against the published HAR 1.2 schema
// (testdata/har-schema, W4-2a decision 3) and against a schema written
// from the spec (testdata/har-1.2.schema.json), the entry time is the sum
// of its timings (spec), and secrets are masked by default.
func TestHARValidatesAgainstHAR12Schema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "har-1.2.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	published := publishedHARSchema(t)
	reqs, bodies := harFixture()
	for _, show := range []bool{false, true} {
		h := BuildHAR(reqs, []Gap{{Start: time.Unix(5, 0), End: time.Unix(6, 0)}}, bodies, HAROptions{ShowValues: show})
		data, _ := json.Marshal(h)
		var inst any
		_ = json.Unmarshal(data, &inst)
		if err := published.Validate(inst); err != nil {
			t.Fatalf("show=%v: HAR does not validate against the published HAR schema: %v\n%s", show, err, data)
		}
		if err := rs.Validate(inst); err != nil {
			t.Fatalf("show=%v: HAR does not validate: %v\n%s", show, err, data)
		}
		for _, e := range h.Log.Entries {
			sum := e.Timings.Send + e.Timings.Wait + e.Timings.Receive
			if diff := e.Time - sum; diff > 1e-6 || diff < -1e-6 {
				t.Errorf("entry time %v != sum of timings %v", e.Time, sum)
			}
		}
		if e := h.Log.Entries[0]; e.Time != 500 || e.Timings.Wait != 250 {
			t.Errorf("entry 1 time %v wait %v, want 500 ms and 250 ms (timestamps 1, 1.25, 1.5 s)", e.Time, e.Timings.Wait)
		}
		text := string(data)
		for _, secret := range []string{"hunter2", "Bearer t", "sid=1", "xyz", "token=abc"} {
			if strings.Contains(text, secret) == !show {
				t.Errorf("show=%v: HAR contains %q = %v", show, secret, !show)
			}
		}
		if !strings.Contains(text, "theme") || !strings.Contains(text, "capture_reset") || !strings.Contains(text, "network reset") {
			t.Errorf("show=%v: HAR lacks a cookie name, the capture_reset entry or the gap note", show)
		}
	}
}

// Property (masking is parsing and encoding): for any JSON object, MaskJSON
// keeps every non-secret value and leaves no value of a secret key.
func TestMaskJSONProperty(t *testing.T) {
	keys := []string{"name", "password", "apiToken", "id", "client_secret", "note", "Session", "x"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		obj := map[string]any{}
		inner := map[string]any{}
		for i := 0; i < 1+r.Intn(6); i++ {
			obj[keys[r.Intn(len(keys))]] = randString(r)
			inner[keys[r.Intn(len(keys))]] = randString(r)
		}
		obj["nested"] = []any{inner}
		raw, _ := json.Marshal(obj)
		out, ok := MaskJSON(string(raw))
		if !ok {
			return false
		}
		var got map[string]any
		if json.Unmarshal([]byte(out), &got) != nil {
			return false
		}
		check := func(want, have map[string]any) bool {
			for k, v := range want {
				if SecretName(k) && have[k] != Masked {
					return false
				}
				if !SecretName(k) && have[k] != v {
					return false
				}
			}
			return true
		}
		gi := got["nested"].([]any)[0].(map[string]any)
		delete(obj, "nested")
		return check(obj, got) && check(inner, gi)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

func randString(r *rand.Rand) string {
	const chars = "ab\"\\\n<>&\u2028é🙂"
	rs := []rune(chars)
	n := r.Intn(8)
	b := make([]rune, n)
	for i := range b {
		b[i] = rs[r.Intn(len(rs))]
	}
	return string(b)
}

// CDP shape (Chrome DevTools Protocol, Network domain): wallTime, CDP
// resource type names, required response fields.
func TestToCDP(t *testing.T) {
	in := `{"requestId":"1","frameId":"f","loaderId":"l","documentURL":"https://a.test/","timestamp":2.5,"walltime":1700000000.5,"type":"StyleSheet","initiator":{"type":"parser","url":"https://a.test/"},"request":{"url":"https://a.test/s.css","method":"GET","headers":{"Accept":"text/css"}}}`
	m, out, ok := ToCDP("Network.requestWillBeSent", json.RawMessage(in))
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if !ok || m != "Network.requestWillBeSent" || got["wallTime"] != 1700000000.5 || got["type"] != "Stylesheet" ||
		got["request"].(map[string]any)["url"] != "https://a.test/s.css" || got["initiator"].(map[string]any)["type"] != "parser" {
		t.Fatalf("requestWillBeSent = %s", out)
	}
	_, out, ok = ToCDP("Network.loadingFinished", json.RawMessage(`{"requestId":"1","timestamp":3,"metrics":{"responseBodyBytesReceived":42}}`))
	if !ok || !strings.Contains(string(out), `"encodedDataLength":42`) {
		t.Fatalf("loadingFinished = %s", out)
	}
	if _, _, ok := ToCDP("Network.requestServedFromMemoryCache", json.RawMessage(`{"requestId":"1"}`)); ok {
		t.Fatal("memory-cache hits have no CDP event without a requestWillBeSent")
	}
}

// validateTrace checks the Trace Event Format rules the converter relies
// on: name, ph, pid, tid and ts on every event; ph X with dur >= 0; ph I
// with scope g, p or t; ph M with args; X events of one thread nest
// (none overlaps another partially).
func validateTrace(t *testing.T, tr Trace) {
	t.Helper()
	type span struct{ a, b float64 }
	open := map[int][]span{}
	for _, e := range tr.TraceEvents {
		if e.Name == "" || e.Pid == 0 || e.Tid == 0 {
			t.Fatalf("event without name, pid or tid: %+v", e)
		}
		switch e.Ph {
		case "M":
			if e.Args == nil {
				t.Fatalf("metadata without args: %+v", e)
			}
		case "I":
			if e.S != "g" && e.S != "p" && e.S != "t" {
				t.Fatalf("instant event scope %q", e.S)
			}
		case "X":
			if e.Dur == nil || *e.Dur < 0 {
				t.Fatalf("complete event without dur: %+v", e)
			}
			st := open[e.Tid]
			for len(st) > 0 && st[len(st)-1].b <= e.Ts {
				st = st[:len(st)-1]
			}
			if len(st) > 0 && e.Ts+*e.Dur > st[len(st)-1].b {
				t.Fatalf("event %q overlaps its parent partially", e.Name)
			}
			open[e.Tid] = append(st, span{e.Ts, e.Ts + *e.Dur})
		default:
			t.Fatalf("unexpected phase %q", e.Ph)
		}
	}
}

// 7.7 test 5: a hand-written Timeline fixture converts to the expected
// trace events, and the trace passes the format checks.
func TestConvertTimelineFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "timeline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Start   float64          `json:"start"`
		Records []TimelineRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	tr := ConvertTimeline(fx.Records, []ScriptEvent{{StartTime: 10.5, EndTime: 10.6, Type: "API"}}, fx.Start)
	validateTrace(t, tr)
	type ev struct {
		Name, Ph string
		Ts, Dur  float64
		Tid      int
	}
	var got []ev
	for _, e := range tr.TraceEvents {
		if e.Ph != "M" {
			got = append(got, ev{e.Name, e.Ph, e.Ts, dur(e), e.Tid})
		}
	}
	// Expected from the fixture: (time - 10 s) in microseconds.
	want := []ev{
		{"EventDispatch click", "X", 500000, 250000, 1},
		{"FunctionCall", "X", 500000, 200000, 1},
		{"Script API", "X", 500000, 100000, 2},
		{"TimerInstall", "I", 600000, 0, 1},
		{"TimerFire", "X", 1000000, 2000, 1},
		{"TimeStamp mark", "I", 1500000, 0, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace events\n got %v\nwant %v", got, want)
	}
	if b, err := json.Marshal(tr); err != nil || !strings.Contains(string(b), `"displayTimeUnit":"ms"`) {
		t.Fatalf("trace JSON: %v %s", err, b)
	}
}

// Property: a CPU profile from any samples is a tree rooted at node 1,
// every sample names a node, hit counts sum to the samples, and the time
// deltas add up to the last sample's offset (CDP Profiler.Profile).
func TestConvertSamplesProperty(t *testing.T) {
	names := []string{"a", "b", "c", ""}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var samples []StackSample
		ts := 100.0
		for i := 0; i < r.Intn(40); i++ {
			ts += float64(r.Intn(1000)) / 1e6
			var frames []StackFrame
			for j := 0; j < r.Intn(5); j++ {
				n := names[r.Intn(len(names))]
				frames = append(frames, StackFrame{SourceID: n, Name: n, Line: 1 + r.Intn(3), Column: 1, URL: "u"})
			}
			samples = append(samples, StackSample{Timestamp: ts, StackFrames: frames})
		}
		p := ConvertSamples(samples, 100, 0)
		ids := map[int]bool{}
		parents := map[int]int{}
		hits := 0
		for _, n := range p.Nodes {
			ids[n.ID] = true
			hits += n.HitCount
			for _, c := range n.Children {
				if _, dup := parents[c]; dup {
					return false
				}
				parents[c] = n.ID
			}
		}
		if len(p.Nodes) == 0 || p.Nodes[0].ID != 1 || len(parents) != len(p.Nodes)-1 || hits != len(samples) || len(p.Samples) != len(samples) || len(p.TimeDeltas) != len(samples) {
			return false
		}
		var sum int64
		for i, id := range p.Samples {
			if !ids[id] || id == 1 {
				return false
			}
			sum += p.TimeDeltas[i]
		}
		return len(samples) == 0 || sum == int64(math.Round(samples[len(samples)-1].Timestamp*1e6))-p.StartTime
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
	// Frames are innermost first (WebKit ScriptProfilerAgent): the leaf
	// gets the hit, lines become 0-based.
	p := ConvertSamples([]StackSample{{Timestamp: 1, StackFrames: []StackFrame{{Name: "inner", Line: 5, Column: 2}, {Name: "outer", Line: 1, Column: 1}}}}, 1, 1)
	if len(p.Nodes) != 3 || p.Nodes[1].CallFrame.FunctionName != "outer" || p.Nodes[2].CallFrame.FunctionName != "inner" || p.Nodes[2].HitCount != 1 || p.Nodes[2].CallFrame.LineNumber != 4 {
		t.Fatalf("profile = %+v", p.Nodes)
	}
}

// W4 review finding (har.go base64 content): a response body that WebKit
// sends as base64 is masked like a text body. Base64 is an encoding, not a
// redaction; only ShowValues keeps the value.
func TestHARMasksBase64Bodies(t *testing.T) {
	reqs, _ := harFixture()
	b64 := base64.StdEncoding.EncodeToString
	binary := b64([]byte{0xff, 0xfe, 'p', 'a', 's', 's'})
	bodies := map[string]HARBody{"2": {Text: b64([]byte(`{"access_token":"b64-secret","name":"ok"}`)), Base64: true}}
	content := func(o HAROptions, body HARBody) HARContent {
		t.Helper()
		bodies["2"] = body
		return BuildHAR(reqs[1:2], nil, bodies, o).Log.Entries[0].Response.Content
	}
	decoded := func(c HARContent) string {
		t.Helper()
		if c.Encoding != "base64" {
			t.Fatalf("content encoding %q, want base64", c.Encoding)
		}
		data, err := base64.StdEncoding.DecodeString(c.Text)
		if err != nil {
			t.Fatalf("content text is not base64: %v", err)
		}
		return string(data)
	}
	jsonBody := bodies["2"]
	if got := decoded(content(HAROptions{}, jsonBody)); got != `{"access_token":"[masked]","name":"ok"}` {
		t.Errorf("masked base64 JSON body decodes to %s", got)
	}
	if got := decoded(content(HAROptions{ShowValues: true}, jsonBody)); !strings.Contains(got, "b64-secret") {
		t.Errorf("ShowValues: base64 JSON body decodes to %s, want the value kept", got)
	}
	if c := content(HAROptions{}, HARBody{Text: binary, Base64: true}); c.Text != binary {
		t.Errorf("binary body %q changed to %q", binary, c.Text)
	}
	if c := content(HAROptions{}, HARBody{Text: "password=not base64!", Base64: true}); c.Text != "" || c.Encoding != "" || !strings.Contains(c.Comment, "does not decode") {
		t.Errorf("a body that is not base64 gives %+v, want no text and the reason", c)
	}
}

// W4 review finding (har.go redirectURL): URL-valued response and request
// headers and the redirect URL are masked like the request URL.
func TestHARMasksRedirectURLs(t *testing.T) {
	reqs, _ := harFixture()
	reqs[0].ResponseHeaders["Location"] = "https://a.test/callback?state=1&access_token=loc-secret"
	reqs[0].ResponseHeaders["Refresh"] = "0; URL=https://a.test/next?token=refresh-secret"
	reqs[0].RequestHeaders["Referer"] = "https://a.test/form?session=ref-secret"
	for _, show := range []bool{false, true} {
		h := BuildHAR(reqs[:1], nil, nil, HAROptions{ShowValues: show})
		raw, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"loc-secret", "refresh-secret", "ref-secret"} {
			if strings.Contains(string(raw), secret) == !show {
				t.Errorf("show=%v: HAR contains %q = %v", show, secret, !show)
			}
		}
		want := "https://a.test/callback?state=1&access_token=%5Bmasked%5D"
		if show {
			want = reqs[0].ResponseHeaders["Location"]
		}
		if got := h.Log.Entries[0].Response.RedirectURL; got != want {
			t.Errorf("show=%v: redirectURL %q, want %q", show, got, want)
		}
	}
}

// W4 review finding (mask.go): the value of a secret key is masked as a
// whole, also when it is an array or an object.
func TestMaskJSONMasksSecretContainers(t *testing.T) {
	got, ok := MaskJSON(`{"tokens":["t1",{"x":"t2"}],"session":{"id":"s1","n":3},"name":["bob"]}`)
	want := `{"name":["bob"],"session":{"id":"[masked]","n":"[masked]"},"tokens":["[masked]",{"x":"[masked]"}]}`
	if !ok || got != want {
		t.Fatalf("MaskJSON = %s, %v; want %s", got, ok, want)
	}
}

// randJSON returns a random JSON value of at most depth levels whose
// scalars are unique markers "v<n>" (so a leak is a substring), with keys
// from keys.
func randJSON(r *rand.Rand, keys []string, depth int, next *int) any {
	switch k := r.Intn(4); {
	case depth > 0 && k == 0:
		a := []any{}
		for range r.Intn(3) {
			a = append(a, randJSON(r, keys, depth-1, next))
		}
		return a
	case depth > 0 && k == 1:
		o := map[string]any{}
		for range r.Intn(4) {
			o[keys[r.Intn(len(keys))]] = randJSON(r, keys, depth-1, next)
		}
		return o
	}
	*next++
	return fmt.Sprintf("v%d", *next)
}

// checkMasked walks the input and the masked output together: a scalar
// under a secret key (at any depth) is Masked, every other one is kept,
// and the shape is the same.
func checkMasked(in, out any, secret bool) bool {
	switch t := in.(type) {
	case map[string]any:
		o, ok := out.(map[string]any)
		if !ok || len(o) != len(t) {
			return false
		}
		for k, v := range t {
			if !checkMasked(v, o[k], secret || SecretName(k)) {
				return false
			}
		}
		return true
	case []any:
		o, ok := out.([]any)
		if !ok || len(o) != len(t) {
			return false
		}
		for i := range t {
			if !checkMasked(t[i], o[i], secret) {
				return false
			}
		}
		return true
	}
	if secret {
		return out == Masked
	}
	return out == in
}

// Property (masking is parsing and encoding): for any JSON document with
// secret keys at any depth and with any value, MaskJSON masks every
// scalar under a secret key and keeps everything else.
func TestMaskJSONSubtreeProperty(t *testing.T) {
	keys := []string{"name", "tokens", "apiToken", "id", "client_secret", "items", "Session", "x"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		next := 0
		doc := map[string]any{}
		for range 1 + r.Intn(5) {
			doc[keys[r.Intn(len(keys))]] = randJSON(r, keys, 3, &next)
		}
		raw, _ := json.Marshal(doc)
		var in any
		_ = json.Unmarshal(raw, &in)
		out, ok := MaskJSON(string(raw))
		var got any
		if !ok || json.Unmarshal([]byte(out), &got) != nil {
			return false
		}
		return checkMasked(in, got, false)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Property: for any query or form text made of name=value pairs joined by
// "&" or ";", with names that are percent-encoded or not and values that
// do not unescape, MaskURL and MaskForm leave no value of a secret name
// and keep every other pair byte for byte.
func TestMaskPairsProperty(t *testing.T) {
	names := []string{"user", "token", "tok%65n", "access_token", "q", "pass+word", "page", "SID", "id_token", "api_key%zz", "%zz"}
	values := []string{"%zz", "a+b", "x%20y", "=", "é", ""}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var parts, secrets, kept []string
		for i := range 1 + r.Intn(6) {
			name := names[r.Intn(len(names))]
			value := fmt.Sprintf("m%dx%s", i, values[r.Intn(len(values))])
			pair := name + "=" + value
			parts = append(parts, pair)
			unescaped, err := url.QueryUnescape(name)
			if err != nil {
				unescaped = name
			}
			if SecretName(unescaped) {
				secrets = append(secrets, fmt.Sprintf("m%dx", i))
			} else {
				kept = append(kept, pair)
			}
		}
		seps := []string{"&", ";"}
		text := parts[0]
		for _, p := range parts[1:] {
			text += seps[r.Intn(2)] + p
		}
		frag := ""
		if r.Intn(2) == 0 {
			frag = "#state=1&access_token=fragsecret"
		}
		for _, out := range []string{MaskURL("https://a.test/p?" + text + frag), MaskForm(text)} {
			for _, s := range append(secrets, "fragsecret") {
				if strings.Contains(out, s) {
					return false
				}
			}
			for _, k := range kept {
				if !strings.Contains(out, k) {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Property: for any JSON response body sent as base64, the default HAR
// has no value of a secret key, at any depth, and keeps the others.
func TestHARBase64BodyProperty(t *testing.T) {
	keys := []string{"name", "tokens", "apiToken", "id", "password", "items"}
	reqs, _ := harFixture()
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		next := 0
		doc := map[string]any{"name": randJSON(r, keys, 2, &next), "password": randJSON(r, keys, 2, &next)}
		raw, _ := json.Marshal(doc)
		var in any
		_ = json.Unmarshal(raw, &in)
		h := BuildHAR(reqs[1:2], nil, map[string]HARBody{"2": {Text: base64.StdEncoding.EncodeToString(raw), Base64: true}}, HAROptions{})
		c := h.Log.Entries[0].Response.Content
		data, err := base64.StdEncoding.DecodeString(c.Text)
		var got any
		if err != nil || json.Unmarshal(data, &got) != nil {
			return false
		}
		return checkMasked(in, got, false)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
