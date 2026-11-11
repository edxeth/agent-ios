package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Storage.clearDataForOrigin (design 5.3, Storage, tier 2: "JS storage
// only"). The cookie methods of the Storage domain are in
// cookies_domain.go.
//
// WebKit has no inspector command that clears an origin's storage, so the
// bridge clears it with page script in each page of the endpoint's scope
// that shows the origin. storageTypes is parsed as Chrome parses it
// (storage_handler.cc GetRemoveDataMask): a comma list, each value trimmed,
// unknown values ignored, and "No valid storage type specified" when no
// value of Chrome's list remains. Of Chrome's values the bridge honors:
//   - local_storage: localStorage of the origin;
//   - all: the same; the other types of "all" stay.
//
// sessionStorage belongs to one tab, and Chrome's storageTypes have no
// value for it, so it is never cleared. A request whose values name none of
// the honored ones fails and clears nothing: iOS cannot clear cookies
// (Storage.clearCookies does, for the page's cookies), IndexedDB, Cache
// Storage, service workers or the other types through this call.

func init() {
	register(Method{Name: "Storage.clearDataForOrigin", Levels: OnBrowser | OnPage, Handle: storageClearDataForOrigin})
}

// chromeStorageTypes are the storageTypes values that Chrome clears; the
// value tells whether the bridge honors it.
var chromeStorageTypes = map[string]bool{
	"cookies": false, "file_systems": false, "indexeddb": false, "local_storage": true,
	"shader_cache": false, "service_workers": false, "cache_storage": false, "all": true,
}

// clearOriginScript clears localStorage when the page shows the origin
// (its argument) and returns JSON text {origin, cleared}; cleared is -1
// when the page shows another origin. The check and the clear run in one
// evaluation, so a navigation between them cannot clear another origin.
const clearOriginScript = `(function (o) {
  if (location.origin !== o) { return JSON.stringify({origin: location.origin, cleared: -1}); }
  var n = localStorage.length;
  localStorage.clear();
  return JSON.stringify({origin: o, cleared: n});
})(%s)`

// storageClearDataForOrigin clears the honored storage types of origin in
// every page of the scope that shows that origin, for the record's logical
// session (record.inPage). A page that another session leases fails the
// call with the lease message before any page is cleared.
func storageClearDataForOrigin(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Origin       string `json:"origin"`
		StorageTypes string `json:"storageTypes"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	origin, ok := webOrigin(p.Origin)
	if !ok {
		// Chrome's text; the bridge also refuses origins other than http
		// and https, which no page it can reach shows.
		return nil, nil, paramError(p.Origin + " is not a valid URL")
	}
	var honored, other []string
	for _, t := range strings.Split(p.StorageTypes, ",") {
		t = strings.TrimSpace(t)
		switch h, known := chromeStorageTypes[t]; {
		case h:
			honored = append(honored, t)
		case known:
			other = append(other, t)
		}
	}
	if len(honored)+len(other) == 0 {
		return nil, nil, paramError("No valid storage type specified")
	}
	if len(honored) == 0 {
		return nil, nil, serverError("Storage.clearDataForOrigin: iOS WebKit cannot clear %q through the bridge; it clears local_storage (also as part of all) only, and Storage.clearCookies clears the page's cookies", p.StorageTypes)
	}
	rec := call.conn.rec
	ts, err := rec.targets(ctx)
	if err != nil {
		return nil, nil, err
	}
	var pages []target
	for _, t := range ts {
		if o, ok := webOrigin(t.url); ok && o == origin {
			pages = append(pages, t)
		}
	}
	if len(pages) == 0 {
		return nil, nil, serverError("Storage.clearDataForOrigin: no page of this endpoint's scope shows %s; WebKit clears an origin's storage only through a page of that origin, so open one first", origin)
	}
	m := rec.s.o.Manager
	for _, t := range pages {
		if holder, _, held := m.LeaseOf(t.key); held && holder != rec.session {
			// Touch refuses with the lease message and changes nothing.
			if _, _, err := m.Touch(ctx, t.req, rec.ao()); err != nil {
				return nil, nil, err
			}
		}
	}
	lit, err := json.Marshal(origin)
	if err != nil {
		return nil, nil, err
	}
	cleared := 0
	for _, t := range pages {
		err := rec.inPage(ctx, t, call.Method, core.ClassMutate, func(ctx context.Context, c webkit.Caller) error {
			res, err := tools.EvaluateScript(ctx, c, fmt.Sprintf(clearOriginScript, lit), true)
			if err != nil {
				return fmt.Errorf("clearing localStorage of %s in %q: %w", origin, t.title, err)
			}
			var text string
			var r struct {
				Cleared int `json:"cleared"`
			}
			if json.Unmarshal(res.Result.Value, &text) != nil || json.Unmarshal([]byte(text), &r) != nil {
				return fmt.Errorf("clearing localStorage of %s in %q: the page returned %s", origin, t.title, res.Result.Value)
			}
			if r.Cleared >= 0 {
				cleared++
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if cleared == 0 {
		return nil, nil, serverError("Storage.clearDataForOrigin: the pages of %s navigated to another origin before their storage was cleared; nothing was cleared", origin)
	}
	if len(other) > 0 {
		rec.s.o.Logf("cdp: %s: Storage.clearDataForOrigin cleared local_storage only; iOS WebKit cannot clear %s", logsafe.ID(call.conn.name), logsafe.ID(strings.Join(other, ",")))
	}
	return map[string]any{}, nil, nil
}

// webOrigin is the serialized origin of an http or https URL (as
// location.origin gives it: lower-case scheme and host, no default port),
// or false for any other URL.
func webOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	defaultPort := (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
	if port != "" && !defaultPort {
		host += ":" + port
	}
	return scheme + "://" + host, true
}
