package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Review F18: with an earlier and a new simulator tab of the same URL,
// the tab id that sim up gets for the new tab's page (SimulatorTab) is the
// id that tab list shows for that page, and "tab <id>" selects that page,
// not the earlier one. A page that the daemon does not list has no id.
func TestSimulatorTabSelectsTheNewTab(t *testing.T) {
	earlier, fresh := newFake(t, fakewebkit.Options{}), newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage("Earlier", "http://10.0.2.2:8791/", earlier.URL)
	dev.AddPage("Mine", "http://10.0.2.2:8791/", fresh.URL)
	o := startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()})
	c := connect(t, o)
	in := commands.Input{Global: commands.Globals{Session: "s1"}}
	ctx := ctxFor(t, 5*time.Second)

	id, err := commands.SimulatorTab(ctx, lifecycleHost{o}, in, fresh.URL)
	if err != nil || !strings.HasPrefix(id, "t") {
		t.Fatalf("SimulatorTab of the new page = %q, %v; want a tab id", id, err)
	}
	if _, err := run(t, c, "s1", "tab", id); err != nil {
		t.Fatalf("tab %s: %v", id, err)
	}
	res, err := run(t, c, "s1", "tab", "list")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Tabs []core.Tab `json:"tabs"`
	}
	if err := json.Unmarshal(res.Data, &list); err != nil {
		t.Fatal(err)
	}
	for _, tab := range list.Tabs {
		if mine := strings.HasSuffix(tab.Key, "|"+fresh.URL); tab.Current != mine || (mine && tab.ID != id) {
			t.Fatalf("after tab %s: %+v; want only the new page current, with that id", id, list.Tabs)
		}
	}

	if id, err := commands.SimulatorTab(ctx, lifecycleHost{o}, in, "ws://127.0.0.1:1/devtools/page/404"); err != nil || id != "" {
		t.Fatalf("SimulatorTab of a page that is not listed = %q, %v; want none", id, err)
	}
}
