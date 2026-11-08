package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// realIWDPListing is the device list of a real iwdp, which no test reads.
const realIWDPListing = "127.0.0.1:9221"

// fakeIWDPBin holds the fake iwdp executables (testutil.InstallFakeIWDP).
var fakeIWDPBin string

// No MCP test can start, stop or restart the real ios_webkit_debug_proxy,
// and no core of these tests reads its device list.
func TestMCPTestsNeverRunTheRealIWDP(t *testing.T) {
	if err := testutil.CheckFakeIWDP(fakeIWDPBin); err != nil {
		t.Fatal(err)
	}
	if a := iwdp.DefaultListingAddr; a == realIWDPListing || !strings.HasPrefix(a, "127.0.0.1:") {
		t.Fatalf("iwdp's default listing is %s, want the private dead listing", a)
	}
}

// mcpRunMainEnv makes the test binary run main() instead of the tests: the
// daemon that select_page starts is this binary as "daemon run".
const mcpRunMainEnv = "IWDP_MCP_TEST_RUN_MAIN"

// testDaemonIdle ends a test daemon soon after the tests stop using it.
const testDaemonIdle = "30s"

// testRuntimeDir is this test binary's private XDG_RUNTIME_DIR and
// XDG_STATE_HOME; userRuntimeDir is the one the tests started with.
var testRuntimeDir, userRuntimeDir string

func TestMain(m *testing.M) {
	// Every process of these tests resolves page URLs against a private
	// dead listing, never the device list of the user's iwdp.
	iwdp.DefaultListingAddr = testutil.DeadIWDPListing()
	if os.Getenv(mcpRunMainEnv) == "1" {
		// A daemon process of a test: it inherits the private directories
		// and never starts the real ios_webkit_debug_proxy.
		daemonStartIWDP = nil
		main()
		os.Exit(0)
	}
	userRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	dir, err := os.MkdirTemp("", "iwdp-mcp-test")
	if err != nil {
		panic(err)
	}
	testRuntimeDir = dir
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	// No two tests share a daemon (W1c T02): the binary's default socket
	// directory is a regular file, so a test that reaches the daemon
	// without privateDaemonDir fails with daemon.ErrUnsafeDir.
	if err := os.WriteFile(daemon.SocketDir(), []byte(sharedDaemonDirNote), 0o600); err != nil {
		panic(err)
	}
	for _, k := range []string{"IWDP_SESSION", "IWDP_DEVICE_RESERVATION", "IWDP_TRANSPORT", "IWDP_DAEMON_DIR"} {
		_ = os.Unsetenv(k)
	}
	// The existing tests check the full legacy tool set (decision D2: the
	// default profile is core; IWDP_MCP_TOOLS=all registers every tool).
	_ = os.Setenv("IWDP_MCP_TOOLS", "all")
	daemonSpawnEnv = []string{mcpRunMainEnv + "=1", "IWDP_DAEMON_IDLE=" + testDaemonIdle}
	daemonStartIWDP = nil
	// list_devices, list_pages and iwdp_status start ios_webkit_debug_proxy
	// by name; the tests find fakes first and never restart iwdp.
	fakeIWDPBin = filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeIWDPBin, 0o700); err != nil {
		panic(err)
	}
	if err := testutil.InstallFakeIWDP(fakeIWDPBin); err != nil {
		panic(err)
	}
	code := m.Run()
	stopTestDaemons()
	os.Exit(code)
}

// stopTestDaemons stops every daemon below testRuntimeDir and removes it.
func stopTestDaemons() {
	_ = filepath.WalkDir(testRuntimeDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "daemon.sock" {
			stopDaemonIn(filepath.Dir(p))
		}
		return nil
	})
	_ = os.RemoveAll(testRuntimeDir)
}

func stopDaemonIn(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: dir, NoStart: true})
	if err != nil {
		return
	}
	defer c.Close()
	pid := c.Hello.PID
	if c.Shutdown(ctx, false) == nil {
		_ = c.WaitGone(ctx, 7*time.Second)
	}
	for end := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
}

// sharedDaemonDirNote is the content of the file that blocks the shared
// daemon directory.
const sharedDaemonDirNote = "no shared test daemon: call privateDaemonDir(t) in a test that reaches the daemon\n"

// privateDaemonDir gives the test its own daemon directory, stopped at
// cleanup, and returns the socket directory. Every test that reaches the
// daemon (select_page, a registry tool without a direct page, a daemon
// command) calls it first: TestMain blocks the shared directory.
func privateDaemonDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(testRuntimeDir, "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "iwdp")
	t.Cleanup(func() { stopDaemonIn(sock) })
	return sock
}

// mcpServerSession connects an MCP client to a server with registerTools
// and resets the selected page at cleanup.
func mcpServerSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	sess.mu.Lock()
	old := sess.client
	sess.mu.Unlock()
	impl := &mcp.Implementation{Name: "iwdp-mcp-test", Version: "0.1.0"}
	server := mcp.NewServer(impl, nil)
	registerTools(server)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(impl, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		sess.mu.Lock()
		cur := sess.client
		sess.client = old
		sess.mu.Unlock()
		if cur != nil && cur != old {
			_ = cur.Close()
		}
	})
	return cs
}

func toolNames(t *testing.T) []string {
	t.Helper()
	cs := mcpServerSession(t)
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// Task item 4: daemons that tests start live in a private runtime
// directory, never the user's.
func TestDaemonTestsUseAPrivateRuntimeDir(t *testing.T) {
	sock := daemon.SocketDir()
	if !strings.HasPrefix(sock, testRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is not below the private test dir %s", sock, testRuntimeDir)
	}
	if userRuntimeDir != "" && strings.HasPrefix(sock, userRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is in the user's runtime dir %s", sock, userRuntimeDir)
	}
	if p, err := daemon.LogPath(); err != nil || !strings.HasPrefix(p, testRuntimeDir) {
		t.Fatalf("daemon log %s (%v) is not below %s", p, err, testRuntimeDir)
	}
	if daemonStartIWDP != nil {
		t.Fatal("test daemons could start the real ios_webkit_debug_proxy")
	}
}

// Design 7.3 item 13 (MCP part), D1: select_page attaches through the
// daemon (lease only), later tools run there, and registry tools see the
// MCP server's session (IWDP_SESSION).
func TestSelectPageRunsThroughTheDaemon(t *testing.T) {
	sock := privateDaemonDir(t)
	t.Setenv("IWDP_SESSION", "mcp-a")
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	cs := mcpServerSession(t)

	// Before select_page, registry tools run in the daemon for the session.
	res, text := callTool(t, cs, "session_info", map[string]any{})
	if res.IsError || !strings.Contains(text, `"session":"mcp-a"`) || !strings.Contains(text, `"pages":null`) {
		t.Fatalf("session_info before select_page: error %v, %s", res.IsError, text)
	}
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}
	sess.mu.Lock()
	_, viaDaemon := sess.client.(*daemon.PageCaller)
	sess.mu.Unlock()
	if !viaDaemon {
		t.Fatal("select_page did not attach through the daemon")
	}
	res, text = callTool(t, cs, "evaluate_script", map[string]any{"expression": "1+1", "return_by_value": true})
	if res.IsError {
		t.Fatalf("evaluate_script: %s", text)
	}
	if out, _ := json.Marshal(res.StructuredContent); !strings.Contains(string(out), `"value":2`) {
		t.Fatalf("evaluate_script = %s, want 2", out)
	}
	if n := mock.ConnectionCount(); n != 1 {
		t.Fatalf("page connections %d, want 1 (the daemon's)", n)
	}

	ctx := context.Background()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: sock, NoStart: true})
	if err != nil {
		t.Fatalf("no daemon in the test's directory: %v", err)
	}
	defer c.Close()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pages) != 1 || st.Pages[0].Session != "mcp-a" || st.Pages[0].Lease != "active" {
		t.Fatalf("daemon pages %+v; want one page leased by session mcp-a", st.Pages)
	}

	res, text = callTool(t, cs, "session_info", map[string]any{})
	if res.IsError || !strings.Contains(text, `"session":"mcp-a"`) || !strings.Contains(text, `"lease":"active"`) {
		t.Fatalf("session_info: error %v, %s", res.IsError, text)
	}

	// Another session gets the lease error (design 2.5), and the selected
	// page stays.
	sess.mu.Lock()
	before := sess.client
	sess.mu.Unlock()
	t.Setenv("IWDP_SESSION", "mcp-b")
	res, text = callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL})
	if !res.IsError || !strings.Contains(text, `leased by session "mcp-a"`) {
		t.Fatalf("select_page from session mcp-b: error %v, %q; want the lease error", res.IsError, text)
	}
	sess.mu.Lock()
	after := sess.client
	sess.mu.Unlock()
	if after != before {
		t.Fatal("a refused select_page replaced the selected page")
	}
}

// Design 2.10: one MCP tool call runs as one exclusive operation, so a
// concurrent tool call's sends never come between its sends.
func TestToolCallIsOneOperation(t *testing.T) {
	privateDaemonDir(t)
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.Handle("Runtime.evaluate", func(string, json.RawMessage) (any, error) {
		time.Sleep(300 * time.Millisecond)
		return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}}, nil
	})
	mock.HandleFunc("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "number", "value": 1}})
	mock.HandleFunc("DOM.getDocument", map[string]any{"root": map[string]any{"nodeId": 1, "nodeName": "#document"}})
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "evaluate_script",
			Arguments: map[string]any{"expression": "p", "await_promise": true, "return_by_value": true},
		})
		if err != nil || res.IsError {
			t.Errorf("evaluate_script: %v %+v", err, res)
		}
	}()
	time.Sleep(100 * time.Millisecond) // the evaluation is in the page
	if res, text := callTool(t, cs, "get_document", map[string]any{}); res.IsError {
		t.Fatalf("get_document: %s", text)
	}
	wg.Wait()
	var got []string
	for _, m := range mock.ReceivedMethods() {
		if m == "Runtime.evaluate" || m == "Runtime.awaitPromise" || m == "DOM.getDocument" {
			got = append(got, m)
		}
	}
	want := []string{"Runtime.evaluate", "Runtime.awaitPromise", "DOM.getDocument"}
	if !slices.Equal(got, want) {
		t.Fatalf("page received %v, want %v: the second tool call ran inside the first", got, want)
	}
}

// Decision D2: without IWDP_MCP_TOOLS the server registers the core
// profile (about 40 tools, coreLegacyTools plus the core registry
// tools); IWDP_MCP_TOOLS=all registers every tool. The core registry tools
// are the session tools of W1b and the page runtime tools of W3a (design
// 4.2: snapshot, diff snapshot, get, is, find, hit-test and read are in the
// core profile), so the profile has 47 tools.
func TestDefaultProfileIsCore(t *testing.T) {
	t.Setenv("IWDP_MCP_TOOLS", "")
	core := toolNames(t)
	if len(core) < 35 || len(core) > 50 {
		t.Fatalf("core profile has %d tools, want about 40: %v", len(core), core)
	}
	// W3b decision 3: the agentic tools replace the legacy reads they
	// supersede; the long-tail input tools (dblclick, focus,
	// scroll_into_view, drag, mouse, keyboard) are in devtools. W3b-2a
	// adds history, tab and frame selection to core.
	want := append(slices.Clone(coreLegacyTools), "session_info", "close_session",
		"snapshot", "diff_snapshot", "get", "is", "find", "hit_test", "read_page",
		"dialog", "wait", "hover", "check", "select_option", "scroll", "press", "tap", "upload_file", "swipe",
		"go_back", "go_forward", "tab_list", "tab_select", "select_frame")
	sort.Strings(want)
	if !slices.Equal(core, want) {
		t.Fatalf("core tools = %v\nwant %v", core, want)
	}
	for _, name := range []string{"select_page", "evaluate_script", "navigate", "take_screenshot", "click"} {
		if !slices.Contains(core, name) {
			t.Errorf("core profile lacks %s", name)
		}
	}
	for _, name := range []string{"heap_snapshot", "restart_iwdp", "get_shader_source"} {
		if slices.Contains(core, name) {
			t.Errorf("core profile has the non-core tool %s", name)
		}
	}

	t.Setenv("IWDP_MCP_TOOLS", "all")
	all := toolNames(t)
	if len(all) < 116 {
		t.Fatalf("IWDP_MCP_TOOLS=all registers %d tools, want every legacy tool (116+)", len(all))
	}
	for _, name := range core {
		if !slices.Contains(all, name) {
			t.Errorf("the full set lacks core tool %s", name)
		}
	}
}

// iwdp-mcp runs the daemon lifecycle commands too ("<exe> daemon run" is
// how clients start the daemon, design 2.1).
func TestMCPBinaryRunsDaemonCommands(t *testing.T) {
	sock := privateDaemonDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, mcpDaemonOptions())
	if err != nil {
		t.Fatalf("starting the daemon as iwdp-mcp daemon run: %v", err)
	}
	defer c.Close()
	if c.Dir() != sock {
		t.Fatalf("daemon dir %s, want %s", c.Dir(), sock)
	}
	if code := runDaemonCommand([]string{"daemon", "status"}); code != 0 {
		t.Fatalf("iwdp-mcp daemon status: exit %d", code)
	}
	if code := runDaemonCommand([]string{"daemon", "frobnicate"}); code != 2 {
		t.Fatalf("iwdp-mcp daemon frobnicate: exit %d, want 2", code)
	}
	// Only the daemon commands run as iwdp-mcp commands.
	if code := runDaemonCommand([]string{"session", "info"}); code == 0 {
		t.Fatal("iwdp-mcp session info: exit 0, want a refusal")
	}
}

// browser_extensions_enable and _disable send Browser.enable and
// Browser.disable (the legacy Enable and Disable helpers of
// *webkit.Client, now plain sends through the Caller).
func TestBrowserExtensionsToolsSend(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	cs := connectMCP(t, mock)
	for _, name := range []string{"browser_extensions_enable", "browser_extensions_disable"} {
		if res, text := callTool(t, cs, name, map[string]any{}); res.IsError {
			t.Fatalf("%s: %s", name, text)
		}
	}
	var got []string
	for _, m := range mock.ReceivedMethods() {
		if strings.HasPrefix(m, "Browser.") {
			got = append(got, m)
		}
	}
	if !slices.Equal(got, []string{"Browser.enable", "Browser.disable"}) {
		t.Fatalf("page received %v, want Browser.enable then Browser.disable", got)
	}
}
