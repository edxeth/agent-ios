//go:build simulator

package e2e_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/proxy"
	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The iOS Simulator suite: the shared WebKit Inspector Protocol helpers
// (internal/tools), the proxy listing helpers and the iwdp transport's
// page lists against real Safari through iwdp. scripts/sim-setup.sh opens
// a test page of its own on https://example.com (AGENT_IOS_SIM_PAGE_URL), and
// getSimClient checks that page before any test runs. The tests that read
// that page's DOM run before TestSim_QuerySelectorFindsInsertedElements,
// which replaces the body.

// =============================================================================
// Proxy and transport: iwdp status, devices, device ports, page lists
// =============================================================================

func TestSim_IWDPStatus(t *testing.T) {
	_ = getSimClient(t) // ensure simulator is up
	ctx, cancel := simCtx()
	defer cancel()
	if err := proxy.CheckListing(ctx, proxy.ListingAddr()); err != nil {
		t.Fatalf("expected iwdp to answer during simulator tests: %v", err)
	}
}

func TestSim_ListDevices(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()
	devices, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		t.Fatalf("ListDevicesContext: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("expected at least one device")
	}
	t.Logf("found %d device(s)", len(devices))
}

// The TestSim_Proxy* tests were the simulator tests of internal/proxy;
// here they run behind getSimClient's check.

func TestSim_ProxyIsRunning(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	if err := proxy.CheckListing(ctx, proxy.ListingAddr()); err != nil {
		t.Fatalf("expected iwdp to answer on %s (started by sim-setup.sh): %v", proxy.ListingAddr(), err)
	}
}

func TestSim_ProxyListDevices(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	devices, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		t.Fatalf("ListDevicesContext: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("expected at least one device (the simulator)")
	}

	for _, d := range devices {
		t.Logf("device: %s (%s) at %s", d.DeviceName, d.DeviceID, d.URL)
	}

	// iwdp lists the Simulator with deviceId "SIMULATOR".
	found := false
	for _, d := range devices {
		if d.DeviceName == "SIMULATOR" || d.DeviceID == "SIMULATOR" {
			found = true
			break
		}
	}
	if !found {
		t.Log("warning: no device named SIMULATOR — may be a physical device")
	}
}

// TestSim_ProxyDevicePort reads the port of every device entry that a
// real iwdp lists, not only the constructed entries of the unit tests.
func TestSim_ProxyDevicePort(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	devices, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		t.Fatalf("ListDevicesContext: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("expected at least one device (the simulator)")
	}

	for _, d := range devices {
		port, err := proxy.DevicePort(d)
		if err != nil {
			t.Errorf("DevicePort(%q): %v", d.URL, err)
			continue
		}
		if port < 9222 {
			t.Errorf("unexpected port %d for device %s", port, d.DeviceName)
		}
		t.Logf("device %s → port %d", d.DeviceName, port)
	}
}

// TestSim_ListPages reads the first device's port and its page list, as
// the registry commands do (the iwdp transport).
func TestSim_ListPages(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()
	devices, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		t.Fatalf("ListDevicesContext: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("no devices to list pages from")
	}
	port, err := proxy.DevicePort(devices[0])
	if err != nil {
		t.Fatalf("DevicePort: %v", err)
	}
	pages, err := iwdp.New(iwdp.Options{}).Pages(ctx, devices[0].DeviceID)
	if err != nil {
		t.Fatalf("Pages of %s: %v", devices[0].DeviceID, err)
	}
	t.Logf("found %d page(s) on port %d", len(pages), port)
}

// TestSim_ListAllPages: the devices together list at least one page, and
// the simulator's Safari page is debuggable.
func TestSim_ListAllPages(t *testing.T) {
	_ = getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()
	tr := iwdp.New(iwdp.Options{})
	devices, err := tr.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	total, debuggable := 0, 0
	for _, d := range devices {
		pages, err := tr.Pages(ctx, d.ID)
		if err != nil {
			t.Fatalf("Pages of %s: %v", d.Label, err)
		}
		for _, p := range pages {
			t.Logf("page on %s: %s — %s", d.Label, p.Title, p.URL)
			total++
			if p.Debuggable {
				debuggable++
			}
		}
	}
	if total == 0 || debuggable == 0 {
		t.Fatalf("found %d page(s), %d debuggable, across %d device(s); want at least one debuggable page", total, debuggable, len(devices))
	}
}

// =============================================================================
// Page: evaluate_script, take_screenshot, snapshot_node
// =============================================================================

func TestSim_EvaluateBasic(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	result, err := tools.EvaluateScript(ctx, client, "2 + 2", true)
	if err != nil {
		t.Fatalf("EvaluateScript: %v", err)
	}
	if result.WasThrown {
		t.Fatal("expression was thrown")
	}
	var val float64
	_ = json.Unmarshal(result.Result.Value, &val)
	if val != 4 {
		t.Errorf("expected 4, got %v", val)
	}
}

func TestSim_EvaluateScript_Error(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	_, err := tools.EvaluateScript(ctx, client, "throw new Error('test')", true)
	if err == nil {
		t.Fatal("expected error for thrown expression")
	}
	t.Logf("got expected error: %v", err)
}

func TestSim_EvaluateScript_ComplexObject(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	result, err := tools.EvaluateScript(ctx, client, "({name: 'test', count: 42})", true)
	if err != nil {
		t.Fatalf("EvaluateScript: %v", err)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(result.Result.Value, &obj)
	if obj["name"] != "test" {
		t.Errorf("expected name=test, got %v", obj["name"])
	}
	if obj["count"] != float64(42) {
		t.Errorf("expected count=42, got %v", obj["count"])
	}
}

// TestSim_EvaluateScriptByReference keeps the evaluation checks of the
// former call_function and get_properties tests: without returnByValue an
// object comes back as a remote reference.
func TestSim_EvaluateScriptByReference(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	for _, expr := range []string{"({x: 1, y: 2})", "({name: 'test', count: 42})"} {
		result, err := tools.EvaluateScript(ctx, client, expr, false)
		if err != nil {
			t.Fatalf("EvaluateScript(%s): %v", expr, err)
		}
		if result.Result.ObjectID == "" {
			t.Fatalf("EvaluateScript(%s): no objectId returned", expr)
		}
	}
}

// TestSim_EvaluateScriptPromise keeps the evaluation check of the former
// IndexedDB test (simIndexedDBRoundTrip, on this run's own database).
func TestSim_EvaluateScriptPromise(t *testing.T) {
	_ = getSimClient(t)
	simIndexedDBRoundTrip(t, sim)
}

func TestSim_TakeScreenshot(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	dataURL, err := tools.TakeScreenshot(ctx, client)
	if err != nil {
		t.Fatalf("TakeScreenshot returned error: %v", err)
	}
	if dataURL == "" {
		t.Fatal("expected non-empty screenshot dataURL")
	}
	if !strings.HasPrefix(dataURL, "data:image/") {
		t.Errorf("unexpected screenshot prefix: %.50s...", dataURL)
	}
	t.Logf("screenshot: %d bytes", len(dataURL))
}

func TestSim_SnapshotNode(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	root, err := tools.GetDocument(ctx, client, 0)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	nodeID, err := tools.QuerySelector(ctx, client, root.NodeID, "body")
	if err != nil {
		t.Fatalf("QuerySelector body: %v", err)
	}

	dataURL, err := tools.SnapshotNode(ctx, client, nodeID)
	if err != nil {
		t.Fatalf("SnapshotNode: %v", err)
	}
	if dataURL == "" {
		t.Fatal("expected non-empty snapshot dataURL")
	}
	t.Logf("snapshot_node: %d bytes", len(dataURL))
}

// =============================================================================
// DOM: get_document, query_selector
// =============================================================================

func TestSim_GetDocument(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	root, err := tools.GetDocument(ctx, client, 2)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if root.NodeID == 0 {
		t.Error("expected non-zero root nodeId")
	}
	if root.NodeType != 9 {
		t.Errorf("expected document nodeType 9, got %d", root.NodeType)
	}
	t.Logf("document root: nodeId=%d, nodeName=%s, children=%d", root.NodeID, root.NodeName, len(root.Children))
}

// TestSim_QuerySelector finds the page's body element. DOM.getOuterHTML,
// sent directly, shows that the node id is that element. The body is in
// every HTML document (the HTML parser makes one); example.com's own
// content is not this test's to rely on: its h1 is gone (CI run
// 45669625167, "no element matches selector "h1"").
func TestSim_QuerySelector(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	root, err := tools.GetDocument(ctx, client, 0)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}

	nodeID, err := tools.QuerySelector(ctx, client, root.NodeID, "body")
	if err != nil {
		t.Fatalf("no body found: %v", err)
	}
	if nodeID == 0 {
		t.Error("expected non-zero nodeId for body")
	}

	raw, err := client.Send(ctx, "DOM.getOuterHTML", map[string]int{"nodeId": nodeID})
	if err != nil {
		t.Fatalf("DOM.getOuterHTML: %v", err)
	}
	var reply struct {
		OuterHTML string `json:"outerHTML"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decoding DOM.getOuterHTML %s: %v", raw, err)
	}
	if !strings.HasPrefix(reply.OuterHTML, "<body") || !strings.HasSuffix(reply.OuterHTML, "</body>") {
		t.Errorf("body outer HTML = %q; want the body element", reply.OuterHTML)
	}
	t.Logf("body outerHTML: %.200s", reply.OuterHTML)
}

// =============================================================================
// Cookies: get_cookies, set_cookie
// =============================================================================

func TestSim_GetCookies(t *testing.T) {
	client := getSimClient(t)
	ctx, cancel := simCtx()
	defer cancel()

	cookies, err := tools.GetCookies(ctx, client)
	if err != nil {
		t.Fatalf("GetCookies: %v", err)
	}
	t.Logf("got %d cookies", len(cookies))
}

// TestSim_SetAndDeleteCookie sets this run's cookie and deletes it again
// (simCookieRoundTrip).
func TestSim_SetAndDeleteCookie(t *testing.T) {
	_ = getSimClient(t)
	simCookieRoundTrip(t, sim)
}

// =============================================================================
// DOM: elements that a script inserts
// =============================================================================

// TestSim_QuerySelectorFindsInsertedElements keeps the lookup checks of the
// former attribute, event listener, inline style, style text, pseudo state
// and DOM breakpoint tests: after a script changes the page, GetDocument
// and QuerySelector find the new element. Several cases replace the body,
// so this test runs after the tests that read example.com's DOM.
func TestSim_QuerySelectorFindsInsertedElements(t *testing.T) {
	client := getSimClient(t)
	cases := []struct {
		name, script, selector string
		settle                 time.Duration
	}{
		{"attributes", "if(!document.querySelector('#attr-test-link')){var a=document.createElement('a');a.id='attr-test-link';a.href='https://example.com';a.textContent='test';document.body.appendChild(a)}", "#attr-test-link", 300 * time.Millisecond},
		{"pseudo state", "if(!document.querySelector('#pseudo-test-link')){var a=document.createElement('a');a.id='pseudo-test-link';a.href='#';a.textContent='hover me';document.body.appendChild(a)}", "#pseudo-test-link", 300 * time.Millisecond},
		{"event listeners", "document.body.innerHTML='<button id=\"btn\">Click</button>';document.getElementById('btn').addEventListener('click',()=>{})", "#btn", 500 * time.Millisecond},
		{"inline styles", "document.body.innerHTML='<div id=\"styled\" style=\"color:red\">test</div>'", "#styled", 500 * time.Millisecond},
		{"style text", "document.body.innerHTML='<div id=\"style-target\" style=\"color:red\">test</div>'", "#style-target", 500 * time.Millisecond},
		{"DOM breakpoint", "document.body.innerHTML='<div id=\"bp-target\">test</div>'", "#bp-target", 300 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := simCtx()
			defer cancel()
			simMutate(t, "insert "+tc.selector, func(ctx context.Context, client *webkit.Client) error {
				_, err := tools.EvaluateScript(ctx, client, tc.script, false)
				return err
			})
			time.Sleep(tc.settle)

			root, err := tools.GetDocument(ctx, client, 0)
			if err != nil {
				t.Fatalf("GetDocument: %v", err)
			}
			nodeID, err := tools.QuerySelector(ctx, client, root.NodeID, tc.selector)
			if err != nil {
				t.Fatalf("no %s found: %v", tc.selector, err)
			}
			if nodeID == 0 {
				t.Fatalf("expected non-zero nodeId for %s", tc.selector)
			}
		})
	}
}
