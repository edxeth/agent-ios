package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests of the W4 command review findings (repair round 1):
// network listings mask secrets, cookies set keeps the expiry of the W0
// tool set_cookie, the stdin password read ends at the
// command deadline, and diff screenshot refuses a threshold that is not a
// finite number.

// listingSecrets are the secret values of emitSecretRequest; no default
// listing may contain one of them.
var listingSecrets = []string{"q-secret-1", "auth-secret-2", "cookie-secret-3", "body-secret-4", "setcookie-secret-5", "resp-token-secret-6", "frag-secret-7"}

// emitSecretRequest sends a finished request whose URL, request headers,
// request body and response headers hold the listingSecrets next to
// values that are not secret (page=2, alice, Accept).
func emitSecretRequest(h *dtHarness, id string) {
	url := "https://fixture.example/api?token=q-secret-1&page=2#access_token=frag-secret-7"
	h.f.Emit("Network.requestWillBeSent", map[string]any{
		"requestId": id, "frameId": "f1", "loaderId": "l1", "timestamp": 1.0, "type": "Fetch",
		"request": map[string]any{
			"url": url, "method": "POST",
			"headers": map[string]string{
				"Authorization": "Bearer auth-secret-2", "Cookie": "sid=cookie-secret-3",
				"Content-Type": "application/json", "Accept": "text/plain",
			},
			"postData": `{"password":"body-secret-4","user":"alice"}`,
		},
	})
	h.f.Emit("Network.responseReceived", map[string]any{
		"requestId": id, "frameId": "f1", "loaderId": "l1", "timestamp": 1.1, "type": "Fetch",
		"response": map[string]any{
			"url": url, "status": 200, "statusText": "OK", "mimeType": "application/json", "source": "network",
			"headers": map[string]string{"Set-Cookie": "sid=setcookie-secret-5", "X-Auth-Token": "resp-token-secret-6", "Content-Type": "application/json"},
		},
	})
	h.f.Emit("Network.loadingFinished", map[string]any{"requestId": id, "timestamp": 1.2})
}

// leakedSecrets returns the listingSecrets that out contains.
func leakedSecrets(out string) []string {
	var found []string
	for _, s := range listingSecrets {
		if strings.Contains(out, s) {
			found = append(found, s)
		}
	}
	return found
}

// Design 7.9 and R17, internal/devtools/mask.go: listings mask password and
// token values by default. "network requests" masks them in its text and
// in its --json data (query values, the URL fragment, secret request and
// response headers, the request body); --show-secrets prints them. Masking
// does not change the stored capture.
func TestNetworkRequestsMasksSecretsByDefault(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "set", "media", "light") // a registry command starts the lease's capture
	emitSecretRequest(h, "sec1")
	waitFor(t, "the request", func() bool { return strings.Contains(h.mustRun(t, "network", "requests"), "sec1 POST 200") })
	for _, args := range [][]string{{"network", "requests"}, {"network", "requests", "--json"}} {
		out := h.mustRun(t, args...)
		if leaked := leakedSecrets(out); len(leaked) > 0 {
			t.Errorf("%s discloses %v: %s", strings.Join(args, " "), leaked, out)
		}
	}
	var listing struct {
		Requests []devtools.Request `json:"requests"`
	}
	out := h.mustRun(t, "network", "requests", "--json")
	if err := json.Unmarshal([]byte(out), &listing); err != nil || len(listing.Requests) != 1 {
		t.Fatalf("--json listing %s: %v", out, err)
	}
	r := listing.Requests[0]
	if r.RequestHeaders["Authorization"] != devtools.Masked || r.ResponseHeaders["Set-Cookie"] != devtools.Masked ||
		r.RequestHeaders["Accept"] != "text/plain" || !strings.Contains(r.URL, "page=2") ||
		!strings.Contains(r.PostData, "alice") || r.Status != 200 || r.Method != "POST" {
		t.Fatalf("masked request %+v; want the secrets masked and every other value kept", r)
	}
	shown := h.mustRun(t, "network", "requests", "--json", "--show-secrets")
	if leaked := leakedSecrets(shown); len(leaked) != len(listingSecrets) {
		t.Fatalf("--show-secrets shows %v of %v: %s", leaked, listingSecrets, shown)
	}
}

// "set credentials" reads the password under the command deadline: a stdin
// that never sends a full line ends the command at the deadline with exit
// 124, and the part of the line that was read reaches no output and no
// daemon command (W4 command review, finding set.go:224).
func TestSetCredentialsStdinEndsAtTheDeadline(t *testing.T) {
	const partial = "partial-pa55"
	stdinPipe(t, partial, false)
	h := newDTHarness(t)
	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		var out, errb bytes.Buffer
		code := RunCLIStarted(context.Background(), []string{"--timeout", "50ms", "set", "credentials", "--origin", "https://fixture.example", "alice"}, &out, &errb, nil,
			func(context.Context, *Invocation) (Env, error) { return dtEnv{h: h}, nil })

		done <- result{code, out.String(), errb.String()}
	}()
	select {
	case res := <-done:
		if res.code != ExitTimeout || strings.Contains(res.out+res.err, partial) || len(h.forwarded) != 0 {
			t.Fatalf("exit %d after %v, stdout %q, stderr %q, %d forwards; want exit %d without the partial input and no forward",
				res.code, time.Since(start), res.out, res.err, len(h.forwarded), ExitTimeout)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("still reading stdin 3 s after a 50 ms deadline")
	}
}

// The W0 tool set_cookie took expires as a number of Unix seconds; a
// fractional value keeps its milliseconds, as the W0 tool's expires*1000
// did.
func TestSetCookieKeepsFractionalSeconds(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	if _, _, err := setCookie(t, p, t.TempDir(), "frac", "v", "--domain", "app.test", "--expires", "1893456000.5"); err != nil {
		t.Fatalf("cookies set with fractional expires: %v", err)
	}
	if got, _ := storedCookie(p, "frac"); got.Session || got.Expires != 1893456000500 {
		t.Fatalf("stored %+v, want expires 1893456000.5 s as 1893456000500 ms", got)
	}
}

// "diff screenshot --threshold" refuses a value that is not a finite
// number with exit 2: NaN used to report "No pixel differs" (W4 storage
// review, finding diff_screenshot.go:46).
func TestDiffScreenshotRejectsANonFiniteThreshold(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ps := &core.PageSession{Caller: spDial(t, f)}
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "devtools", "testdata", "diff", "base.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, th := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf"} {
		if code, out, errOut := spRun(ps, dir, "diff", "screenshot", "--baseline", "base.png", "--threshold", th); code != ExitUsage {
			t.Errorf("--threshold %s: exit %d, %q, %q; want exit 2", th, code, out, errOut)
		}
	}
	if n := len(f.ReceivedMethod("Page.snapshotRect")); n != 0 {
		t.Fatalf("a refused threshold took %d screenshots", n)
	}
}

// Pools of names for genRequest: some are secret names (devtools.SecretName),
// some are not, in mixed case.
var (
	queryNames  = []string{"token", "access_token", "page", "q", "sid", "Session_ID", "apikey", "lang", "PASSWORD", "id"}
	headerNames = []string{"Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Session", "Accept", "X-Trace", "User-Agent", "x-csrf-token", "Content-Language"}
	bodyKeys    = []string{"password", "user", "token", "name", "refreshToken", "otp", "comment", "id"}
)

// genValue is a value that no other part of a request contains.
func genValue(r *rand.Rand) string { return fmt.Sprintf("v%016x", r.Uint64()) }

// genRequest is a random request with values under random query, header
// and body names, and the values under secret and other names.
func genRequest(r *rand.Rand) (req devtools.Request, secret, plain []string) {
	put := func(m map[string]string, name string) {
		m[name] = genValue(r)
	}
	query := map[string]string{}
	for range r.Intn(4) {
		put(query, queryNames[r.Intn(len(queryNames))])
	}
	reqH, respH, body := map[string]string{}, map[string]string{}, map[string]string{}
	for range r.Intn(4) {
		put(reqH, headerNames[r.Intn(len(headerNames))])
	}
	for range r.Intn(4) {
		put(respH, headerNames[r.Intn(len(headerNames))])
	}
	for range r.Intn(4) {
		put(body, bodyKeys[r.Intn(len(bodyKeys))])
	}
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	req = devtools.Request{
		ID: "r1", Seq: 1, Method: "POST", State: devtools.StateFinished, Status: 200,
		URL: "https://fixture.example/p?" + q.Encode(), RequestHeaders: reqH, ResponseHeaders: respH,
	}
	if len(body) > 0 {
		if r.Intn(2) == 0 {
			reqH["Content-Type"] = "application/json"
			data, _ := json.Marshal(body)
			req.PostData = string(data)
		} else {
			reqH["Content-Type"] = "application/x-www-form-urlencoded"
			f := url.Values{}
			for k, v := range body {
				f.Set(k, v)
			}
			req.PostData = f.Encode()
		}
	}
	for _, m := range []map[string]string{query, reqH, respH, body} {
		for k, v := range m {
			if k == "Content-Type" {
				continue
			}
			if devtools.SecretName(k) {
				secret = append(secret, v)
			} else {
				plain = append(plain, v)
			}
		}
	}
	return req, secret, plain
}

// Property (design 7.9, R17: listings mask password and token values by
// default; the rule is devtools.SecretName): for any request, the masked
// copy that "network requests" and "network request" print holds no value
// of a secret query parameter, header or body field, keeps every other
// value, and leaves the logged request unchanged.
func TestMaskRequestProperty(t *testing.T) {
	f := func(seed int64) bool {
		req, secret, plain := genRequest(rand.New(rand.NewSource(seed)))
		before, _ := json.Marshal(req)
		data, _ := json.Marshal(maskRequest(req))
		out := string(data)
		for _, v := range secret {
			if strings.Contains(out, v) {
				t.Logf("seed %d: secret value %s in %s", seed, v, out)
				return false
			}
		}
		for _, v := range plain {
			if !strings.Contains(out, v) {
				t.Logf("seed %d: value %s lost in %s", seed, v, out)
				return false
			}
		}
		if after, _ := json.Marshal(req); string(after) != string(before) {
			t.Logf("seed %d: masking changed the logged request:\n%s\n%s", seed, before, after)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Property (W0 tool set_cookie: expires is a number of seconds and the
// cookie's expiry is expires*1000 ms, 0 a session cookie): any number in
// decimal notation without exponent, as --expires takes it, gives the W0
// tool's expiry when that is within the range of a JavaScript Date
// (ECMA-262 "Time Values and Time Range": ±8.64e15 ms); beyond it (WS2
// round-2 review F7: the milliseconds overflowed to +Inf, which JSON
// cannot encode) it is a usage error. testing/quick draws most float64
// values beyond that range, so each value is also tried reduced into it.
func TestCookieExpiresNumberProperty(t *testing.T) {
	const jsDateRangeMs = 8.64e15
	f := func(x float64) bool {
		text := strconv.FormatFloat(x, 'f', -1, 64)
		session, ms, err := cookieExpiry(text)
		if x == 0 {
			return err == nil && session
		}
		if math.Abs(x*1000) > jsDateRangeMs {
			if ExitCodeOf(err) != ExitUsage {
				t.Logf("%v: text %q gives %v, %v, %v; want a usage error", x, text, session, ms, err)
				return false
			}
			return true
		}
		if err != nil || session || ms != x*1000 {
			t.Logf("%v: text %q gives %v, %v, %v; want %v ms", x, text, session, ms, err, x*1000)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
	inRange := func(x float64) bool { return f(math.Mod(x, jsDateRangeMs/1000)) }
	if err := quick.Check(inRange, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
	for _, x := range []float64{1893456000, 1893456000.5, -1, 1e-7, 1e21, 8.64e12, -8.64e12, 8.64e12 + 1, 1e306} {
		if !f(x) {
			t.Errorf("expires %v", x)
		}
	}
	// Seconds beyond the float64 range are a flag value error (exit 2).
	if _, _, err := cookieExpiry(strings.Repeat("9", 400)); ExitCodeOf(err) != ExitUsage {
		t.Fatalf("400-digit --expires: %v; want a usage error", err)
	}
}

// Property (design 2.11: global flags anywhere before "--"): for any
// registry command and any global flags, with or without their values,
// placed before, between and after its words, Parse finds the command.
func TestParseSkipsGlobalFlagsProperty(t *testing.T) {
	values := map[string]string{
		"--session": "s1", "--page": "t1", "--timeout": "5s", "--transport": "iwdp", "--max-output": "100",
		"--reservation": "me", "--wait-lease": "1s",
	}
	cmds := All()
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		c := cmds[r.Intn(len(cmds))]
		words := strings.Fields(c.Name)
		var line []string
		addGlobals := func() {
			for range r.Intn(3) {
				g := globalSpecs[r.Intn(len(globalSpecs))]
				switch {
				case !g.value:
					line = append(line, g.name)
				case r.Intn(2) == 0:
					line = append(line, g.name+"="+values[g.name])
				default:
					line = append(line, g.name, values[g.name])
				}
			}
			if r.Intn(5) == 0 {
				line = append(line, "--help")
			}
		}
		for _, w := range words {
			addGlobals()
			line = append(line, w)
		}
		addGlobals()
		inv, err := Parse(append(line, "--help"), func(string) string { return "" })
		if err != nil || inv.Cmd != c {
			t.Logf("Parse(%q): %v, %v; want %s", line, inv, err, c.Name)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
	// A global flag without its value ends the line, and Parse reports the
	// missing value.
	line := []string{"network", "--json", "requests", "--page"}
	if _, err := Parse(line, func(string) string { return "" }); ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), "--page needs a value") {
		t.Fatalf("Parse(%q): %v; want the usage error for --page", line, err)
	}
}
