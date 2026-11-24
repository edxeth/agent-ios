package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/sim"
)

// Review F18: sim up --url prints the tab id of the new tab that it saw,
// as the daemon lists it for the caller's session and owner, and the
// command that selects it; when the daemon gives no id, sim up still
// succeeds and points to tab find. Private mode prints the id too: it
// names no page.
func TestSimUpPrintsTheNewTabsID(t *testing.T) {
	prev := SimulatorTab
	t.Cleanup(func() { SimulatorTab = prev })
	var asked []string
	SimulatorTab = func(_ context.Context, _ Env, in Input, pageID string) (string, error) {
		asked = append(asked, in.Global.Session+" "+pageID)
		if pageID == "ws://new" {
			return "t3", nil
		}
		return "", errors.New("daemon unavailable")
	}
	in := Input{Global: Globals{Session: "s1"}}
	r := sim.UpReport{Pages: 2, TabOpened: true, NewPage: "ws://new"}
	r.Tab = simUpTab(context.Background(), nil, in, r.NewPage)
	text := strings.Join(simUpNext(r, "http://10.0.2.2:8791/"), "\n")
	if r.Tab != "t3" || !strings.Contains(text, "iwdp-cli tab t3") || len(asked) != 1 || asked[0] != "s1 ws://new" {
		t.Fatalf("tab %q, text %q, asked %q; want t3 for session s1 and 'iwdp-cli tab t3'", r.Tab, text, asked)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ws://new") {
		t.Fatalf("sim up's data shows the page's WebSocket URL: %s", data)
	}
	if ptext, _, ok := simUpSummary(data); !ok || !strings.Contains(ptext, "new tab t3") {
		t.Fatalf("private summary %q, %t; want the tab id", ptext, ok)
	}

	r = sim.UpReport{Pages: 2, TabOpened: true, NewPage: "ws://other"}
	r.Tab = simUpTab(context.Background(), nil, in, r.NewPage)
	text = strings.Join(simUpNext(r, "http://10.0.2.2:8791/"), "\n")
	if r.Tab != "" || !strings.Contains(text, "tab find --device SIMULATOR --url-file") {
		t.Fatalf("no id from the daemon: tab %q, text %q; want tab find", r.Tab, text)
	}
	data, _ = json.Marshal(r)
	if ptext, _, ok := simUpSummary(data); !ok || !strings.Contains(ptext, "tab find --device SIMULATOR") {
		t.Fatalf("private summary without an id %q, %t; want tab find", ptext, ok)
	}
}
