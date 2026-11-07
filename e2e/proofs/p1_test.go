//go:build webkitgtk

package proofs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// firstScriptPage records, in its first inline script, what the bootstrap
// part left on window before any page script ran.
const firstScriptPage = `<!doctype html><html><head><script>window.__firstSaw = String(window.__p1marker);</script></head><body>p1</body></html>`

const markerPart = `window.__p1marker = "bootstrap-ran";`

func evalString(ctx context.Context, c webkit.Caller, expr string) (string, error) {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		return "", err
	}
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return fmt.Sprint(r.Result.Value), nil
}

// waitStartPage waits until the start page has committed and loaded.
// MiniBrowser lists its page before the start URL commits, and WebKit keeps
// the process for any navigation from a web process that has not committed
// a load yet (WebProcessPool::processForNavigationInternal, "Process has
// not yet committed any provisional loads", also in webkitgtk-2.54.2), so
// run 1 must start from the loaded start page to be a process swap.
func waitStartPage(ctx context.Context, t *testing.T, c webkit.Caller, startURL string) {
	t.Helper()
	want := startURL + " complete"
	var last string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		v, err := evalString(cctx, c, "location.href + ' ' + document.readyState")
		cancel()
		if err == nil && v == want {
			return
		}
		last = fmt.Sprintf("%q, %v", v, err)
	}
	t.Fatalf("the start page %s did not load within 30 s; last answer %s", startURL, last)
}

type targetLog struct {
	mu     sync.Mutex
	events []string
}

func (l *targetLog) add(method string, params json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, method+" "+string(params))
}

func (l *targetLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// TestP1ProvisionalSetup: a cross-origin navigation between two local
// ports; the session sends Page.enable and a bootstrap part to the
// provisional target before commit. Pass: with pause-on-start on, the first
// inline script of the new document sees the marker in 10 of 10 runs; in
// both modes the active target changes at commit and an abandoned
// provisional target is dropped. With pause-on-start off the marker count
// is only logged: design 2.7 item 3 amended the original criterion of 7.1
// after this proof ("Runs before page scripts" requires pause-on-start and
// is partial without it; the setup races the new document).
func TestP1ProvisionalSetup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(firstScriptPage))
	})
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// 127.0.0.1 and localhost are different sites, so WebKit swaps the
	// process between them (two ports of one host are the same site).
	origins := []string{serve(t, mux), strings.Replace(serve(t, mux), "127.0.0.1", "localhost", 1)}

	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("pauseOnStart=%v", pause), func(t *testing.T) {
			// A fresh browser per mode: WebKitGTK refuses quick reconnects (spike A step B).
			startURL := origins[0] + "/page?start"
			tr, page := browser(t, startURL)
			ds := core.NewDesiredState()
			ds.Acquire("session", "Page")
			if err := ds.SetPart("session", "p1-marker", core.RankRuntime, markerPart); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var setupMu sync.Mutex
			setups := map[string]error{} // provisional target -> result of its setup
			s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return tr.Open(ctx, page) },
				webkit.SessionOptions{TargetWait: 12 * time.Second, PauseOnStart: pause, Setup: func(ctx context.Context, tc webkit.TargetCaller) error {
					err := ds.Apply(ctx, tc)
					t.Logf("setup of provisional target %s: %v", tc.TargetID(), err)
					setupMu.Lock()
					setups[tc.TargetID()] = err
					setupMu.Unlock()
					return err
				}})
			if err != nil {
				t.Fatalf("connecting: %v", err)
			}
			defer s.Close()
			var log targetLog
			s.Subscribe(webkit.AllEvents, func(m string, p json.RawMessage) {
				if len(m) > 7 && m[:7] == "Target." {
					log.add(m, p)
				}
			})
			if err := ds.Apply(ctx, s); err != nil {
				t.Fatalf("applying desired state to the current target: %v", err)
			}
			waitStartPage(ctx, t, s, startURL)

			pass := 0
			for run := 1; run <= 10; run++ {
				before := s.ActiveTarget()
				url := fmt.Sprintf("%s/page?run=%d&pause=%v", origins[run%2], run, pause)
				if _, err := evalString(ctx, s, fmt.Sprintf("location.href = %q", url)); err != nil {
					t.Logf("run %d: navigate: %v", run, err)
				}
				var saw string
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
					v, err := evalString(cctx, s, "location.href + ' ' + String(window.__firstSaw)")
					ccancel()
					if err == nil && strings.HasPrefix(v, url+" ") {
						saw = v // the new document answers; keep polling only while the marker is missing
					}
					if saw == url+" bootstrap-ran" {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if s.ActiveTarget() == before {
					t.Errorf("run %d: the active target did not change; the navigation was not a process swap", run)
				} else {
					// In both modes the setup of the committed target completes
					// (design 2.7); only its order against page scripts differs.
					setupMu.Lock()
					serr, ran := setups[s.ActiveTarget()]
					setupMu.Unlock()
					if !ran || serr != nil {
						t.Errorf("run %d: setup of the committed target %s: ran %v, error %v; want a completed setup", run, s.ActiveTarget(), ran, serr)
					}
				}
				ok := saw == url+" bootstrap-ran"
				if ok {
					pass++
				}
				t.Logf("run %d: target %s -> %s, generation %d, first script saw %q, pass=%v", run, before, s.ActiveTarget(), s.Generation(), saw, ok)
			}
			t.Logf("P1 pauseOnStart=%v: %d of 10 runs passed", pause, pass)
			if pause && pass != 10 { // design 2.7 item 3: guaranteed only with pause-on-start
				t.Errorf("P1 pauseOnStart=%v: %d of 10 runs saw the marker in the first inline script", pause, pass)
			}

			// Abandoned provisional target: a cross-origin navigation answered
			// with 204 No Content is never committed.
			before := s.ActiveTarget()
			n := len(log.snapshot())
			cross := origins[0] + "/nocontent"
			if cur, _ := evalString(ctx, s, "location.origin"); cur == origins[0] || strings.Contains(cur, "127.0.0.1") {
				cross = origins[1] + "/nocontent"
			}
			_, _ = evalString(ctx, s, fmt.Sprintf("location.href = %q", cross))
			time.Sleep(3 * time.Second)
			events := log.snapshot()[n:]
			for _, e := range events {
				t.Logf("after 204 navigation: %s", e)
			}
			if s.ActiveTarget() != before {
				t.Errorf("active target changed from %s to %s on a 204 navigation", before, s.ActiveTarget())
			}
			if v, err := evalString(ctx, s, "1+1"); err != nil || v != "2" {
				t.Errorf("session after abandoned navigation: %q, %v", v, err)
			}
		})
	}
}
