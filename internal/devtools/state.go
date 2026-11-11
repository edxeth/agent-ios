package devtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// "state save <path>" and "state load <path>" (design 4.2): the cookies
// and storage of the page's current origin as JSON. The layout follows
// Playwright's storageState (cookies with expires in Unix seconds, -1 for
// a session cookie; origins with localStorage), plus sessionStorage and a
// version. A state file holds live credentials by design: it is written
// with mode 0600 (WriteStateFile) and command output never shows values.

// StateVersion is the version of the state file layout.
const StateVersion = 1

// State is a state file.
type State struct {
	Version int           `json:"version"`
	SavedAt string        `json:"savedAt,omitempty"` // RFC 3339 UTC
	Cookies []StateCookie `json:"cookies"`
	Origins []StateOrigin `json:"origins"`
}

// StateCookie is a cookie in a state file.
type StateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // Unix seconds to the millisecond; -1: a session cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

// StateOrigin is the storage of one origin.
type StateOrigin struct {
	Origin         string         `json:"origin"`
	LocalStorage   []StorageEntry `json:"localStorage"`
	SessionStorage []StorageEntry `json:"sessionStorage"`
}

// stateCookie converts a WebKit cookie (expires in milliseconds).
func stateCookie(c webkit.Cookie) StateCookie {
	s := StateCookie{
		Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: -1,
		HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: c.SameSite,
	}
	if !c.Session {
		s.Expires = math.Round(c.Expires) / 1000
	}
	return s
}

// WebKit converts a state cookie to a WebKit cookie.
func (s StateCookie) WebKit() webkit.Cookie {
	c := webkit.Cookie{
		Name: s.Name, Value: s.Value, Domain: s.Domain, Path: s.Path, HTTPOnly: s.HTTPOnly,
		Secure: s.Secure, SameSite: s.SameSite, Session: true,
	}
	if s.Expires > 0 {
		c.Session, c.Expires = false, math.Round(s.Expires*1000)
	}
	return c
}

// originHost returns the host of an origin such as https://a.test:8443.
func originHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || origin == "null" {
		return "", fmt.Errorf("the page has no usable origin (%q): open an http or https page of the site first", origin)
	}
	return u.Hostname(), nil
}

// SaveState reads the page's origin, the cookies that the browser sends to
// that origin's host (RFC 6265 domain match; Page.getCookies) and both
// storage areas. Cookies are sorted by domain, path and name, storage
// entries by name.
func SaveState(ctx context.Context, c webkit.Caller) (State, error) {
	snap, err := storageCall(ctx, c, storageArgs{Op: "snapshot"})
	if err != nil {
		return State{}, err
	}
	host, err := originHost(snap.Origin)
	if err != nil {
		return State{}, err
	}
	all, err := tools.GetCookies(ctx, c)
	if err != nil {
		return State{}, fmt.Errorf("Page.getCookies: %w", err)
	}
	st := State{
		Version: StateVersion, SavedAt: time.Now().UTC().Format(time.RFC3339), Cookies: []StateCookie{},
		Origins: []StateOrigin{{Origin: snap.Origin, LocalStorage: entries(snap.Local), SessionStorage: entries(snap.Session)}},
	}
	// A state that "state load" would refuse is not saved. Every state
	// that passes this check loads: keys and values of any length are
	// sent in parts (planApplies).
	if err := checkStorageSize(st.Origins[0]); err != nil {
		return State{}, err
	}
	for _, ck := range all {
		if DomainMatch(host, ck.Domain) {
			st.Cookies = append(st.Cookies, stateCookie(ck))
		}
	}
	sort.Slice(st.Cookies, func(i, j int) bool {
		a, b := st.Cookies[i], st.Cookies[j]
		return a.Domain+"\x00"+a.Path+"\x00"+a.Name < b.Domain+"\x00"+b.Path+"\x00"+b.Name
	})
	return st, nil
}

// StateLoadResult says what LoadState changed.
type StateLoadResult struct {
	Origin         string `json:"origin"`
	CookiesSet     int    `json:"cookiesSet"`
	CookiesRemoved int    `json:"cookiesRemoved"`
	LocalStorage   int    `json:"localStorage"`
	SessionStorage int    `json:"sessionStorage"`
}

// checkState validates a state against the rules of SaveState: one origin,
// and only cookies that the browser sends to its host.
func checkState(s State) (string, error) {
	if s.Version != StateVersion {
		return "", fmt.Errorf("state file version %d: this iwdp-cli reads version %d", s.Version, StateVersion)
	}
	if len(s.Origins) != 1 {
		return "", fmt.Errorf("a state file holds one origin; this one has %d", len(s.Origins))
	}
	host, err := originHost(s.Origins[0].Origin)
	if err != nil {
		return "", fmt.Errorf("the state file's origin: %w", err)
	}
	for _, c := range s.Cookies {
		if !DomainMatch(host, c.Domain) {
			return "", fmt.Errorf("the state file has cookie %q of domain %s, which is not sent to %s", c.Name, c.Domain, s.Origins[0].Origin)
		}
	}
	return host, nil
}

// LoadState makes the page's origin hold exactly the state's cookies and
// storage: the page must be at the state's origin, and each storage area
// of the state must fit WebKit's quota (checkStorageSize; a state that
// does not is refused before any write). The storage is restored first
// (restoreStorage: only the differences, in an order that keeps each area
// within the quota, in page scripts that check the origin again; a failure
// puts the earlier values back); then the cookies
// that the browser sends to the origin's host are deleted and the state's
// cookies are set and read back (tools.SetCookieVerified). Cookies of
// other sites stay.
func LoadState(ctx context.Context, c webkit.Caller, s State) (StateLoadResult, error) {
	host, err := checkState(s)
	if err != nil {
		return StateLoadResult{}, err
	}
	o := s.Origins[0]
	if err := checkStorageSize(o); err != nil {
		return StateLoadResult{}, fmt.Errorf("%w; the storage and the cookies were not changed", err)
	}
	snap, err := storageCall(ctx, c, storageArgs{Op: "snapshot"})
	if err != nil {
		return StateLoadResult{}, err
	}
	if snap.Origin != o.Origin {
		return StateLoadResult{}, fmt.Errorf("the state is for %s and the page is at %s: open a page of %s first", o.Origin, snap.Origin, o.Origin)
	}
	if err := restoreStorage(ctx, c, o, snap); err != nil {
		return StateLoadResult{}, fmt.Errorf("restoring the storage of %s: %w", o.Origin, err)
	}
	res := StateLoadResult{Origin: o.Origin, LocalStorage: len(o.LocalStorage), SessionStorage: len(o.SessionStorage)}
	all, err := tools.GetCookies(ctx, c)
	if err != nil {
		return res, fmt.Errorf("the storage is restored, the cookies are not: Page.getCookies: %w", err)
	}
	var old []webkit.Cookie
	for _, ck := range all {
		if DomainMatch(host, ck.Domain) {
			old = append(old, ck)
		}
	}
	if err := DeleteCookies(ctx, c, old); err != nil {
		return res, fmt.Errorf("the storage is restored, the cookies are not: %w", err)
	}
	res.CookiesRemoved = len(old)
	cookies := make([]webkit.Cookie, len(s.Cookies))
	for i, sc := range s.Cookies {
		cookies[i] = sc.WebKit()
	}
	if res.CookiesSet, err = ImportCookies(ctx, c, cookies); err != nil {
		return res, fmt.Errorf("the storage is restored, the cookies are not: %w", err)
	}
	return res, nil
}

// EncodeState renders a state file.
func EncodeState(s State) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the state: %w", err)
	}
	return append(data, '\n'), nil
}

// DecodeState parses a state file. Unknown fields are refused, so a file
// of another tool is not taken for a state file.
func DecodeState(data []byte) (State, error) {
	var s State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, fmt.Errorf("the file is not an iwdp state file: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if s.Version != StateVersion {
		return State{}, fmt.Errorf("the file is not an iwdp state file: version %d, want %d", s.Version, StateVersion)
	}
	return s, nil
}
