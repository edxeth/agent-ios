package devtools

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

var vitalsCallRE = regexp.MustCompile(`(?s)^\(function iwdpVitals\(a\) \{.*\}\)\((.*)\)$`)

// vitalsPage is a fake page whose vitals function resolves to reply; it
// answers the way WebKit does for a Promise: Runtime.evaluate returns a
// Promise object and Runtime.awaitPromise its value.
func vitalsPage(t *testing.T, reply map[string]any) (*webkit.Session, *fakewebkit.Server) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var v struct{ Expression string }
		_ = json.Unmarshal(c.Params, &v)
		if !vitalsCallRE.MatchString(v.Expression) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "not the vitals function"}
		}
		return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}}, nil
	})
	f.Handle("Runtime.awaitPromise", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		data, _ := json.Marshal(reply)
		return map[string]any{"result": map[string]any{"type": "string", "value": string(data)}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, f
}

// Design 4.2 "vitals": navigation and paint timing on iOS 16.7; LCP and
// event timing only when the engine has them (MAP 1.12), else reported as
// unavailable with the reason. The text has one line per metric in
// agent-browser's order (LCP, CLS, TTFB, FCP, INP), then the load events.
func TestVitalsFormat(t *testing.T) {
	s, f := vitalsPage(t, map[string]any{
		"ttfb": map[string]any{"value": 12.4}, "fcp": map[string]any{"value": 140.5},
		"lcp":              map[string]any{"unavailable": "this engine has no largest-contentful-paint entries"},
		"cls":              map[string]any{"unavailable": "this engine has no layout-shift entries"},
		"inp":              map[string]any{"unavailable": "no interaction yet"},
		"domContentLoaded": map[string]any{"value": 200}, "load": map[string]any{"value": 250.2},
	})
	v, err := ReadVitals(bg(t), s)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"LCP: unavailable (this engine has no largest-contentful-paint entries)",
		"CLS: unavailable (this engine has no layout-shift entries)",
		"TTFB: 12 ms",
		"FCP: 141 ms",
		"INP: unavailable (no interaction yet)",
		"DOMContentLoaded: 200 ms",
		"Load: 250 ms",
	}, "\n")
	if got := v.Text(); got != want {
		t.Fatalf("text\n%s\nwant\n%s", got, want)
	}
	if v.LCP.Value != nil || v.TTFB.Value == nil || *v.TTFB.Value != 12.4 || v.CLS.Unavailable == "" {
		t.Fatalf("vitals %+v", v)
	}
	// The page function waits for buffered PerformanceObserver entries.
	ev := f.ReceivedMethod("Runtime.evaluate")
	if len(ev) != 1 || !strings.Contains(string(ev[0].Params), `\"waitMs\":`) {
		t.Fatalf("Runtime.evaluate calls %v; want one call of the vitals function with waitMs", ev)
	}
	if got := len(f.ReceivedMethod("Runtime.awaitPromise")); got != 1 {
		t.Fatalf("Runtime.awaitPromise sent %d times, want 1", got)
	}
	data, _ := json.Marshal(v)
	var back map[string]any
	_ = json.Unmarshal(data, &back)
	if !reflect.DeepEqual(back["lcp"], map[string]any{"unavailable": "this engine has no largest-contentful-paint entries"}) || back["cls"] == nil {
		t.Fatalf("JSON %s", data)
	}
	// CLS is a unitless score.
	reply := map[string]any{"cls": map[string]any{"value": 0.0512}}
	for _, k := range []string{"lcp", "ttfb", "fcp", "inp", "domContentLoaded", "load"} {
		reply[k] = map[string]any{"unavailable": "x"}
	}
	s2, _ := vitalsPage(t, reply)
	v2, err := ReadVitals(bg(t), s2)
	if err != nil || !strings.Contains(v2.Text(), "CLS: 0.051\n") {
		t.Fatalf("CLS line: %q, %v", v2.Text(), err)
	}
}

func TestVitalsRejectsAMetricWithoutValueOrReason(t *testing.T) {
	reply := map[string]any{"ttfb": map[string]any{}}
	for _, k := range []string{"lcp", "cls", "fcp", "inp", "domContentLoaded", "load"} {
		reply[k] = map[string]any{"value": 1}
	}
	s, _ := vitalsPage(t, reply)
	if _, err := ReadVitals(bg(t), s); err == nil || !strings.Contains(err.Error(), "ttfb") {
		t.Fatalf("err %v; want an error naming ttfb", err)
	}
}
