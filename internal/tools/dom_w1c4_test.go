package tools_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nnemirovsky/agent-ios/internal/tools"
)

// L01 (w1c backlog): a DOM.getDocument reply without "root" (a page that
// is loading, or a reply that lost its body) is an error that names the
// method and says what to do, never a nil root with a nil error, which the
// callers dereferenced (a Go panic).
func TestGetDocumentWithoutRootIsAnError(t *testing.T) {
	for _, reply := range []map[string]any{{}, {"root": nil}} {
		mock, client := setup(t)
		mock.HandleFunc("DOM.getDocument", reply)
		root, err := tools.GetDocument(context.Background(), client, 1)
		if err == nil || root != nil {
			t.Fatalf("reply %v: root %v, error %v; want no root and an error", reply, root, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "DOM.getDocument") || !strings.Contains(msg, "try again") {
			t.Fatalf("reply %v: error %q does not name DOM.getDocument and what to do", reply, msg)
		}
	}
}

// L01, the element screenshot caller (CaptureScreenshot with a selector):
// it reports the missing root and sends nothing after DOM.getDocument.
func TestElementScreenshotWithoutDocumentRoot(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("DOM.getDocument", map[string]any{})
	_, err := tools.CaptureScreenshot(context.Background(), client, tools.ScreenshotOptions{Selector: "#card"})
	if err == nil || !strings.Contains(err.Error(), "DOM.getDocument") || !strings.Contains(err.Error(), "#card") {
		t.Fatalf("error %v, want the missing DOM.getDocument root for selector #card", err)
	}
	if got := mock.ReceivedMethods(); slices.Contains(got, "DOM.querySelector") || slices.Contains(got, "Page.snapshotNode") {
		t.Fatalf("methods after the missing root: %v", got)
	}
}
