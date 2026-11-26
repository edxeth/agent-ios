package commands

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/cssrules"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Regression tests of review findings F8 to F11 (parity-045
// review-logpoint-css.md), from the reviewer's repros.

// F10: a logical property conflicts with the physical one it maps to: in
// a horizontal left-to-right element, margin-inline-start of a higher
// rule overloads margin-left of a lower one.
func TestCSSReviewLogicalAliases(t *testing.T) {
	m := &wipMatchedStyles{MatchedCSSRules: []wipRuleMatch{ruleMatch(".low", "author", 1, cssStyleJSON("margin-left:1px")), ruleMatch(".high", "author", 2, cssStyleJSON("margin-inline-start:2px"))}}
	rep := buildCSSMatched("x", &cssElement{chain: []string{"p"}}, m, &wipInlineStyles{}, &wipComputedStyle{}, nil)
	got := strings.Join(statuses(rep), "|")
	if !strings.Contains(got, "matched .low margin-left overloaded") {
		t.Fatalf("LTR inline-start wins over left, got %s", got)
	}
}

// F10: the element's writing mode and direction decide which physical
// property a logical one conflicts with: in a right-to-left element
// margin-inline-start overloads margin-right, not margin-left.
func TestCSSReviewLogicalAliasesFollowDirection(t *testing.T) {
	m := &wipMatchedStyles{MatchedCSSRules: []wipRuleMatch{
		ruleMatch(".l", "author", 1, cssStyleJSON("margin-left:1px")),
		ruleMatch(".r", "author", 2, cssStyleJSON("margin-right:1px")),
		ruleMatch(".high", "author", 3, cssStyleJSON("margin-inline-start:2px")),
	}}
	comp := &wipComputedStyle{ComputedStyle: []cssrules.ComputedProperty{{Name: "direction", Value: "rtl"}, {Name: "writing-mode", Value: "horizontal-tb"}}}
	rep := buildCSSMatched("x", &cssElement{chain: []string{"p"}}, m, &wipInlineStyles{}, comp, nil)
	got := strings.Join(statuses(rep), "|")
	if !strings.Contains(got, "matched .r margin-right overloaded") || !strings.Contains(got, "matched .l margin-left active") {
		t.Fatalf("RTL inline-start must win over right only, got %s", got)
	}
}

// F10: the logical-to-physical map of each writing mode (CSS Logical
// Properties, section 4 and 6.1 to 6.3).
func TestCascadeKey(t *testing.T) {
	for _, c := range []struct{ wm, dir, name, want string }{
		{"horizontal-tb", "ltr", "padding-block-end", "padding-bottom"},
		{"horizontal-tb", "rtl", "inset-inline-end", "left"},
		{"vertical-rl", "ltr", "margin-block-start", "margin-right"},
		{"vertical-lr", "ltr", "margin-block-start", "margin-left"},
		{"vertical-rl", "rtl", "margin-inline-start", "margin-bottom"},
		{"horizontal-tb", "ltr", "border-inline-start-color", "border-left-color"},
		{"horizontal-tb", "ltr", "border-block-end", "border-bottom"},
		{"horizontal-tb", "rtl", "border-start-end-radius", "border-top-left-radius"},
		{"vertical-rl", "ltr", "border-start-start-radius", "border-top-right-radius"},
		{"vertical-rl", "ltr", "inline-size", "height"},
		{"horizontal-tb", "ltr", "max-block-size", "max-height"},
		{"vertical-lr", "ltr", "overflow-inline", "overflow-y"},
		{"", "", "margin-inline-start", "margin-left"},
		{"horizontal-tb", "ltr", "margin-inline", "margin-inline"}, // a shorthand conflicts through its longhands
		{"horizontal-tb", "ltr", "--margin-inline-start", "--margin-inline-start"},
		{"horizontal-tb", "ltr", "border-inline-start-foo", "border-inline-start-foo"},
	} {
		if got := cascadeKey(c.wm, c.dir)(c.name); got != c.want {
			t.Errorf("%s %s %s = %s, want %s", c.wm, c.dir, c.name, got, c.want)
		}
	}
}

// F11: var( inside a CSS string is text, not a reference.
func TestCSSReviewQuotedVar(t *testing.T) {
	got, ok := resolveVars(`"var(--x)"`, map[string]string{"--x": "red"}, 0)
	if !ok || got != `"var(--x)"` {
		t.Fatalf("CSS string must stay literal; got %q, %v", got, ok)
	}
	vars := map[string]string{"--x": "red", "--s": `"a)b"`}
	for _, c := range []struct{ in, want string }{
		{`'var(--x)' var(--x)`, `'var(--x)' red`},                 // single quotes too
		{`"say \"var(--x)\"" var(--x)`, `"say \"var(--x)\"" red`}, // escaped quote inside a string
		{`/* var(--x) */ var(--x)`, `/* var(--x) */ red`},         // a comment
		{`myvar(--x) VAR(--x)`, `myvar(--x) red`},                 // a longer name; the name is case-insensitive
		{`var(--y, ")") var(--x)`, `")" red`},                     // a ) in a string of the fallback
		{`var(--y, "a,b")`, `"a,b"`},                              // a comma in a string of the fallback
	} {
		if got, ok := resolveVars(c.in, vars, 0); !ok || got != c.want {
			t.Errorf("resolveVars(%q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
}

// F8: a node id is sent only to the target that gave it: after a
// provisional commit swapped the target, the old id is not sent to the
// new one.
func TestCSSReviewNodeAfterSwap(t *testing.T) {
	fh := frameNodeHarness(t)
	h := fh.h
	h.f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"value": map[string]any{"c": []string{"p#x"}, "u": "https://one.example/"}}})
	h.f.Result("CSS.getComputedStyleForNode", map[string]any{"computedStyle": []any{}})
	op := fh.begin(t)
	defer op.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	el, err := openCSSElement(ctx, opEnv{pageEnv{h.ps}, op}, "#x")
	if err != nil {
		t.Fatal(err)
	}
	defer el.close()
	if el.node.ID != 42 {
		t.Fatal(el.node.ID)
	}
	h.f.Swap("page-2", 0)
	for h.sess.ActiveTarget() != "page-2" {
		select {
		case <-ctx.Done():
			t.Fatal("swap not observed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := h.sess.ActiveSetup(ctx); err != nil {
		t.Fatal(err)
	}
	var out wipComputedStyle
	err = el.call(ctx, "CSS.getComputedStyleForNode", nil, &out)
	for _, rec := range h.f.ReceivedMethod("CSS.getComputedStyleForNode") {
		if rec.Target == "page-2" {
			t.Fatalf("sent old node 42 to new target: %s (error %v)", rec.Params, err)
		}
	}
	if err == nil {
		t.Fatal("the call on the swapped target succeeded")
	}
}

// F9: a canceled read still releases its object group.
func TestCSSReviewCanceledReleasesObjectGroup(t *testing.T) {
	fh := frameNodeHarness(t)
	h := fh.h
	h.f.Stall("Runtime.callFunctionOn")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, epoch := h.ps.Sched.Lease()
	op, err := h.ps.Sched.Begin(ctx, core.OpSpec{Name: "css computed", Class: core.ClassRead, Session: session, Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	done := make(chan error, 1)
	go func() {
		el, err := openCSSElement(ctx, opEnv{pageEnv{h.ps}, op}, "#x")
		if err == nil {
			el.close()
		}
		done <- err
	}()
	for len(h.f.ReceivedMethod("Runtime.callFunctionOn")) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("callFunctionOn was not sent")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	select {
	case err := <-done:
		t.Logf("the canceled read ended with %v", err)
	case <-wait.Done():
		t.Fatal("canceled call did not end")
	}
	if len(h.f.ReceivedMethod("Runtime.releaseObjectGroup")) == 0 {
		t.Fatal("canceled operation leaked the CSS remote-object group")
	}
}

// F9: two reads at once use their own object groups: the release of one
// does not free the other's element.
func TestCSSReviewConcurrentGroups(t *testing.T) {
	h := newHarness(t)
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression  string `json:"expression"`
			ObjectGroup string `json:"objectGroup"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		id := "obj-b"
		if strings.Contains(p.Expression, "#a") {
			id = "obj-a"
		}
		return map[string]any{"result": map[string]any{"objectId": id + "@" + p.ObjectGroup}}, nil
	})
	h.f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"value": map[string]any{"c": []string{"p"}, "u": "https://site/"}}})
	var mu sync.Mutex
	released := map[string]bool{}
	h.f.Handle("Runtime.releaseObjectGroup", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			ObjectGroup string `json:"objectGroup"`
		}
		_ = json.Unmarshal(c.Params, &p)
		mu.Lock()
		released[p.ObjectGroup] = true
		mu.Unlock()
		return map[string]any{}, nil
	})
	h.f.Handle("DOM.requestNode", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			ObjectID string `json:"objectId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		_, group, _ := strings.Cut(p.ObjectID, "@")
		mu.Lock()
		gone := released[group]
		mu.Unlock()
		if gone {
			return nil, &fakewebkit.Error{Code: -32000, Message: "object group released"}
		}
		return map[string]any{"nodeId": 1}, nil
	})
	wrap := reviewCSSCaller{Caller: h.sess, reached: make(chan struct{}), resume: make(chan struct{})}
	h.ps.Caller = wrap
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		el, err := openCSSElement(ctx, pageEnv{h.ps}, "#a")
		if err == nil {
			el.close()
		}
		done <- err
	}()
	select {
	case <-wrap.reached:
	case <-ctx.Done():
		t.Fatal("first did not reach its element")
	}
	el, err := openCSSElement(ctx, pageEnv{h.ps}, "#b")
	if err != nil {
		t.Fatal(err)
	}
	el.close()
	close(wrap.resume)
	if err := <-done; err != nil {
		t.Fatalf("second read released first read's object: %v", err)
	}
}

// reviewCSSCaller holds the first read (#a) after its element lookup until
// the test resumes it.
type reviewCSSCaller struct {
	webkit.Caller
	reached chan struct{}
	resume  chan struct{}
}

func (c reviewCSSCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r, e := c.Caller.Send(ctx, method, params)
	if method == "Runtime.callFunctionOn" {
		if p, ok := params.(map[string]any); ok && strings.HasPrefix(p["objectId"].(string), "obj-a") {
			close(c.reached)
			select {
			case <-c.resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return r, e
}
