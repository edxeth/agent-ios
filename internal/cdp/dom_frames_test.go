package cdp

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Root request m-261201e2 (agent-browser 0.45.0 "frame #child", then
// "click #inner"): WebKit's DOM.requestNode refuses a node before
// DOM.getDocument with an empty message, because
// InspectorDOMAgent::pushNodePathToFrontend takes its error string by
// value (WebKit 7615.3.12.11.2, iOS 16.6; seen on WebKitGTK as
// {"code":-32000,"message":""}). The bridge takes that refusal as the
// missing document too: it sends DOM.getDocument once, then the push again.
func TestDOMRequestNodeRetriesAfterAnEmptyRefusal(t *testing.T) {
	w := newW5b2Page(t)
	var documented atomic.Bool
	w.f.Handle("DOM.getDocument", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		documented.Store(true)
		return map[string]any{"root": map[string]any{"nodeId": 1}}, nil
	})
	w.f.Handle("DOM.requestNode", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if !documented.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "", Data: []fakewebkit.Error{{Code: -32000, Message: ""}}}
		}
		return map[string]any{"nodeId": 42}, nil
	})
	m := w.mark()
	if got := w.call(t, "DOM.requestNode", map[string]any{"objectId": "frame-obj"}); string(got) != `{"nodeId":42}` {
		t.Fatalf("requestNode = %s", got)
	}
	if got := w.methodsSince(m); strings.Join(got, " ") != "DOM.requestNode DOM.getDocument DOM.requestNode" {
		t.Fatalf("requestNode sent %v, want the push, DOM.getDocument and the push again", got)
	}
}
