package commands

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// "cookies clear --domain d" deletes the cookies of d and its subdomains
// only (design 4.2). The fake's Page.deleteCookie(name, url) is iOS's: it
// deletes every cookie of the name that the url gets (WebKit
// NetworkStorageSessionCocoa deleteCookie), so a delete by name and url
// would also delete the parent domain's namesake, which is outside
// --domain (W5c-1b review, the W4 finding). The command must keep it.
func TestCookiesClearKeepsANamesakeOutsideTheDomain(t *testing.T) {
	p := devtoolstest.New(t, "https://sub.app.test")
	p.Cookies = []webkit.Cookie{
		{Name: "sid", Value: "valueSub", Domain: "sub.app.test", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "sid", Value: "valueParent", Domain: ".app.test", Path: "/", Session: true, SameSite: "Lax"},
	}
	ps := &core.PageSession{Caller: p.Conn}
	code, out, errOut := spRun(ps, "", "cookies", "clear", "--domain", "sub.app.test")
	if code != 0 || !strings.Contains(out, "Deleted 1 cookie") {
		t.Fatalf("cookies clear --domain sub.app.test: exit %d, out %q, stderr %q", code, out, errOut)
	}
	if got := jarNames(p); !reflect.DeepEqual(got, []string{"sid@.app.test"}) {
		t.Fatalf("jar after cookies clear --domain sub.app.test: %v, want the parent domain's sid kept", got)
	}
}
