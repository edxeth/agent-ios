package devtools

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W4 lib round-4 review, P1 (intercept.go:416): the safety net named a rule
// by its URL pattern in the daemon log, and an exact rule can be a full URL
// with a token. No daemon log line and no release error of the interceptor
// may carry rule text: the safety net's line (for a tombstone and for a
// devtools rule whose handler is gone) and the tombstone's failed attempts
// at a commit and at the lease release. The lines name a rule by the
// interceptor's number for it, the same number in every line.
func TestRuleURLNeverReachesTheLog(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	r := newRig(t)
	p := r.page(t)
	ic := p.icpt

	const secret = "rule-secret"
	tomb := core.InterceptRule{URL: "https://fixture.example/private?access_token=" + secret, Stage: "request", CaseSensitive: true}
	stateRule := core.InterceptRule{URL: `^https://fixture\.example/state\?token=` + secret, Stage: "request", CaseSensitive: true, IsRegex: true}
	ic.mu.Lock()
	ic.tombs["secret-tomb"] = tomb
	ic.mu.Unlock()
	p.ps.State.AddInterception(owner("route", "s1"), stateRule)

	intercept(r.f, "log-tomb", tomb.URL)
	oneUnchangedContinue(t, r.f, "log-tomb", "request")
	intercept(r.f, "log-state", "https://fixture.example/state?token="+secret)
	oneUnchangedContinue(t, r.f, "log-state", "request")

	// The tombstone's removal fails at the commit and at the release.
	r.f.Handle("Network.removeInterception", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var got core.InterceptRule
		_ = json.Unmarshal(c.Params, &got)
		if got == tomb {
			return nil, &fakewebkit.Error{Code: -32000, Message: "removal refused"}
		}
		return map[string]any{}, nil
	})
	ic.applyTombstones(r.f.ActiveTarget())
	ic.mu.Lock()
	delete(ic.tombs, "secret-tomb")
	ic.mu.Unlock()
	ic.putTombstone(owner("route", "s1"), tomb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, relErr := r.m.ReleaseSession(ctx, "s1")
	if relErr == nil {
		t.Fatal("the release reported no error, want the tombstone's failed removal")
	}

	out := buf.String() + "\n" + relErr.Error()
	for _, leak := range []string{secret, "fixture.example/private", `fixture\.example/state`} {
		if strings.Contains(out, leak) {
			t.Fatalf("rule text %q reached the log or the release error:\n%s", leak, out)
		}
	}
	// Each path wrote its line, naming the rule by its number: two
	// safety-net lines (two rules), the commit's and the release's
	// attempt (the tombstone's rule).
	name := regexp.MustCompile(`interception rule #(\d+) \(request stage\)`)
	ids := map[string][]string{}
	for _, what := range []string{"no handler claims it", "commit of a provisional target: removing", "lease release: removing"} {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, what) {
				continue
			}
			m := name.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("a %q line without a rule number:\n%s", what, out)
			}
			ids[what] = append(ids[what], m[1])
		}
	}
	safety, commit, release := ids["no handler claims it"], ids["commit of a provisional target: removing"], ids["lease release: removing"]
	if len(safety) != 2 || safety[0] == safety[1] || len(commit) != 1 || len(release) != 1 ||
		commit[0] != safety[0] || release[0] != safety[0] {
		t.Fatalf("rule numbers: safety net %v, commit %v, release %v; want two rules, the tombstone's first and in every tombstone line:\n%s", safety, commit, release, out)
	}
}
