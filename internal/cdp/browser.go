package cdp

import (
	"context"
	"errors"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/logsafe"
)

// The Browser domain (design 5.3).

// userAgentWait bounds the read of navigator.userAgent for getVersion.
const userAgentWait = 2 * time.Second

func init() {
	register(
		Method{Name: "Browser.getVersion", Levels: OnBrowser, Handle: getVersion},
		Method{Name: "Browser.close", Levels: OnBrowser, Handle: browserClose},
		// Accepted with no effect: Safari saves downloads itself (design
		// 4.3), and Playwright sends it on every connect (proof P2 item 3).
		Method{Name: "Browser.setDownloadBehavior", Levels: OnBrowser},
		Method{Name: "Browser.getWindowForTarget", Levels: OnBrowser | OnTab | OnPage, Handle: getWindowForTarget},
		Method{Name: "Browser.grantPermissions", Levels: OnBrowser, Handle: grantPermissions},
	)
}

func getVersion(ctx context.Context, call *Call) (any, Later, error) {
	c := call.conn
	return map[string]any{
		"protocolVersion": "1.3", "product": c.rec.product(), "revision": "", "jsVersion": "",
		"userAgent": c.userAgent(ctx),
	}, nil, nil
}

// userAgent reads navigator.userAgent of the selected page only while the
// logical session holds its lease already: getVersion comes before any
// attach, and it must neither take a lease nor fail for a page that
// another session holds. Otherwise it is "".
func (c *conn) userAgent(ctx context.Context) string {
	m := c.s.o.Manager
	key := c.rec.selected.Key
	holder, epoch, ok := m.LeaseOf(key)
	pg := m.LookupPage(key)
	if !ok || holder != c.rec.session || pg == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, userAgentWait)
	defer cancel()
	mo, err := m.BeginLeased(ctx, pg, holder, epoch, "cdp Browser.getVersion", core.ClassRead)
	if err != nil {
		return ""
	}
	defer m.End(mo.ID)
	raw, err := mo.Op.Caller().Send(mo.Op.Context(), "Runtime.evaluate", map[string]any{"expression": "navigator.userAgent", "returnByValue": true})
	if err != nil {
		c.s.o.Logf("cdp: %s: reading navigator.userAgent: %v", logsafe.ID(c.name), logsafe.Error(err))
		return ""
	}
	ua, _ := evalValue(raw).(string)
	return ua
}

// browserClose detaches only (design 5.3): the client's connection ends
// after the reply; Safari keeps running.
func browserClose(_ context.Context, call *Call) (any, Later, error) {
	c := call.conn
	call.after = func() {
		c.mu.Lock()
		ws := c.ws
		c.mu.Unlock()
		if ws != nil {
			_ = ws.Close() // the read loop ends and detaches every session
		}
	}
	return nil, nil, nil
}

// getWindowForTarget is synthetic: one window, whose size the bridge does
// not know (the viewport is fixed by the device, design 4.3).
func getWindowForTarget(context.Context, *Call) (any, Later, error) {
	return map[string]any{"windowId": 1, "bounds": map[string]any{"windowState": "normal"}}, nil, nil
}

func grantPermissions(context.Context, *Call) (any, Later, error) {
	return nil, nil, errors.New("permissions cannot be granted on iOS WebKit: WIP has no permission method; allow them on the device")
}
