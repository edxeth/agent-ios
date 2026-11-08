package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// legacySetCookieParams are the parameters of the W0 MCP tool set_cookie
// (cmd/iwdp-mcp SetCookieInput). Design 4.1: an existing MCP tool that a
// registry command takes over keeps its parameters; design 4.2 row
// "cookies set": the MCP tool is set_cookie, the W4 part adds import.
var legacySetCookieParams = []string{"name", "value", "domain", "path", "expires", "secure", "http_only", "same_site"}

// Design 4.2 row "cookies set": the registry command is the MCP tool
// set_cookie (in the core profile, as the W0 tool was) with every legacy
// parameter plus the import parameter curl; there is no tool
// import_cookies. Help works and an unknown flag exits 2 before any
// connection, as for the other W4 commands.
func TestCookiesSetIsTheSetCookieTool(t *testing.T) {
	c, n := Lookup([]string{"cookies", "set"})
	if c == nil || n != 2 {
		t.Fatal("cookies set is not registered")
	}
	if c.MCPName != "set_cookie" || c.Profile != ProfileCore {
		t.Fatalf("cookies set: MCP %q in profile %q, want set_cookie in core", c.MCPName, c.Profile)
	}
	for _, other := range All() {
		if other.MCPName == "import_cookies" {
			t.Fatalf("%s still registers the MCP tool import_cookies", other.Name)
		}
	}
	props, _ := InputSchema(c)["properties"].(map[string]any)
	for _, p := range append(legacySetCookieParams, "curl") {
		if _, ok := props[p]; !ok {
			t.Errorf("set_cookie schema lacks the parameter %q: %v", p, props)
		}
	}
	for _, p := range []string{"secure", "http_only"} {
		if typ := props[p].(map[string]any)["type"]; typ != "boolean" {
			t.Errorf("set_cookie %s has type %v, want boolean as in the W0 tool", p, typ)
		}
	}
	var out, errb bytes.Buffer
	called := false
	newEnv := func(context.Context, *Invocation) (Env, error) { called = true; return nil, fmt.Errorf("no env") }
	if code := RunCLI(context.Background(), []string{"cookies", "set", "--help"}, &out, &errb, newEnv); code != 0 || !strings.HasPrefix(out.String(), "Usage: iwdp-cli cookies set") || called {
		t.Errorf("cookies set --help: exit %d, called %v, out %q", code, called, out.String())
	}
	errb.Reset()
	if code := RunCLI(context.Background(), []string{"cookies", "set", "--no-such-flag"}, &out, &errb, newEnv); code != ExitUsage || called || !strings.Contains(errb.String(), "unknown flag --no-such-flag") {
		t.Errorf("cookies set --no-such-flag: exit %d, called %v, stderr %q; want exit 2 before any connection", code, called, errb.String())
	}
}

// setCookieMCP serves the core registry tools on page p and returns a
// client session.
func setCookieMCP(t *testing.T, p *devtoolstest.Page, dir string) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ps := &core.PageSession{Caller: p.Conn}
	RegisterMCP(server, ProfileCore, func(context.Context, Globals) (Env, error) { return spEnv{ps: ps, dir: dir}, nil })
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callSetCookie(t *testing.T, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_cookie", Arguments: args})
	if err != nil {
		t.Fatalf("set_cookie %v: %v", args, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func storedCookie(p *devtoolstest.Page, name string) (webkit.Cookie, bool) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for _, c := range p.Cookies {
		if c.Name == name {
			return c, true
		}
	}
	return webkit.Cookie{}, false
}

// The legacy parameters keep their W0 names and meaning (cmd/iwdp-mcp
// SetCookieInput): http_only and same_site, expires in Unix seconds with 0
// for a session cookie. The result keeps the W0 output {ok, cookie} as
// structured content, with the value redacted.
func TestSetCookieToolKeepsLegacyParameters(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	cs := setCookieMCP(t, p, t.TempDir())

	res, text := callSetCookie(t, cs, map[string]any{
		"name": "sid", "value": "topsecret7", "domain": "app.test", "path": "/",
		"expires": 0, "secure": true, "http_only": true, "same_site": "strict",
	})
	if res.IsError {
		t.Fatalf("set_cookie: %s", text)
	}
	got, ok := storedCookie(p, "sid")
	want := webkit.Cookie{Name: "sid", Value: "topsecret7", Domain: "app.test", Path: "/", Secure: true, HTTPOnly: true, Session: true, SameSite: "Strict"}
	if !ok || got != want {
		t.Fatalf("stored %+v, want %+v (expires 0 is a session cookie)", got, want)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(structured), `"ok":true`) || !strings.Contains(string(structured), `"name":"sid"`) || strings.Contains(string(structured)+text, "topsecret7") {
		t.Fatalf("structured %s, text %q; want {ok, cookie} without the value", structured, text)
	}

	if res, text = callSetCookie(t, cs, map[string]any{"name": "persist", "value": "v", "domain": "app.test", "expires": 1893456000}); res.IsError {
		t.Fatalf("set_cookie with expires: %s", text)
	}
	if got, _ = storedCookie(p, "persist"); got.Session || got.Expires != 1893456000*1000 {
		t.Fatalf("stored %+v, want expires 1893456000 s in milliseconds", got)
	}

	res, text = callSetCookie(t, cs, map[string]any{"name": "x", "value": "v", "domain": "app.test", "http_only": true, "http-only": true})
	if !res.IsError || !strings.Contains(text, `"http-only"`) || !strings.Contains(text, `"http_only"`) {
		t.Fatalf("both http_only and http-only: IsError %v, %q; want an error that names both", res.IsError, text)
	}
}

// Design 4.2 row "cookies set": the import part (agent-browser
// "cookies set --curl <file>") is the parameter curl of set_cookie.
func TestSetCookieToolImportsAFile(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("sid=topsecret8; theme=dark"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := setCookieMCP(t, p, dir)
	res, text := callSetCookie(t, cs, map[string]any{"curl": "c.txt"})
	if res.IsError {
		t.Fatalf("set_cookie curl: %s", text)
	}
	for _, name := range []string{"sid", "theme"} {
		if c, ok := storedCookie(p, name); !ok || c.Domain != "app.test" {
			t.Fatalf("cookie %s: %+v, %v; want it on the page's host app.test", name, c, ok)
		}
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(structured), `"ok":true`) || !strings.Contains(string(structured), `"imported":2`) || strings.Contains(string(structured)+text, "topsecret8") {
		t.Fatalf("structured %s, text %q", structured, text)
	}
}

// --expires keeps the W0 meaning of the MCP parameter: Unix seconds, and
// 0 is a session cookie (cmd/iwdp-mcp SetCookieInput: "0 or omit for
// session cookie"). An RFC 3339 time is the CLI form.
func TestCookieExpiresProperty(t *testing.T) {
	session, ms, err := cookieExpiry("0")
	if err != nil || !session || ms != 0 {
		t.Fatalf(`cookieExpiry("0") = %v, %v, %v; want a session cookie`, session, ms, err)
	}
	f := func(s int32) bool {
		if s == 0 {
			return true
		}
		session, ms, err := cookieExpiry(strconv.FormatInt(int64(s), 10))
		return err == nil && !session && ms == float64(int64(s)*1000)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
	if session, ms, err := cookieExpiry("2030-01-02T03:04:05Z"); err != nil || session || ms != 1893553445000 {
		t.Fatalf("RFC 3339: %v, %v, %v", session, ms, err)
	}
}
