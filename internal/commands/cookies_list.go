package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// cookies lists the page's cookies (agent-browser "cookies"; the legacy W0
// listing). Values are hidden unless --show-values: a cookie value is
// often a session token, which must not land in agent transcripts and logs
// (W0, design R17). Hidden means absent: no part of a value, and nothing
// derived from it, reaches the text or the --json data.
func init() {
	Register(&Command{
		Name:      "cookies",
		Summary:   "List the page's cookies (those Page.getCookies lists, httpOnly included): name, domain, path, sameSite, expiry or session, httpOnly, secure. Values are hidden unless --show-values.",
		Flags:     []Flag{{Name: "show-values", Type: FlagBool, Help: "print the cookie values (they can be session tokens)"}},
		Page:      LeasedPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runCookiesList,
	})
}

func runCookiesList(ctx context.Context, env Env, in Input) (Result, error) {
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	cookies, err := tools.GetCookies(ctx, c)
	if err != nil {
		return Result{}, err
	}
	return cookieListing(cookies, flagBool(in, "show-values")), nil
}

// listedCookie is a cookie of the --json data; Value is nil unless the
// values are shown.
type listedCookie struct {
	Name     string  `json:"name"`
	Value    *string `json:"value,omitempty"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // milliseconds since the Unix epoch, as WebKit gives it
	Session  bool    `json:"session"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

// noCookies is the text of a cookie listing without cookies.
const noCookies = "No cookies."

// hiddenValue stands for a cookie value in the text without --show-values.
const hiddenValue = "(hidden)"

// cookieListing renders cookies, one line each, and their --json data.
// Without show, no value reaches either.
func cookieListing(cookies []webkit.Cookie, show bool) Result {
	list := make([]listedCookie, len(cookies))
	lines := make([]string, len(cookies))
	for i, c := range cookies {
		list[i] = listedCookie{Name: c.Name, Domain: c.Domain, Path: c.Path, Expires: c.Expires, Session: c.Session, HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: c.SameSite}
		value := hiddenValue
		if show {
			value = c.Value
			list[i].Value = &value
		}
		lines[i] = cookieLine(c, value)
	}
	data := map[string]any{"cookies": list, "valuesShown": show}
	if len(cookies) == 0 {
		return Result{Text: noCookies, Data: data}
	}
	return Result{Text: strings.Join(lines, "\n"), Data: data}
}

// cookieLine renders a cookie as the legacy listing did: name=value, then
// its attributes; value is what to print for the value.
func cookieLine(c webkit.Cookie, value string) string {
	line := fmt.Sprintf("%s=%s  domain=%s path=%s", c.Name, value, c.Domain, c.Path)
	if c.SameSite != "" {
		line += " sameSite=" + c.SameSite
	}
	if !c.Session && c.Expires > 0 {
		line += " expires=" + time.UnixMilli(int64(c.Expires)).UTC().Format(time.RFC3339)
	}
	if c.HTTPOnly {
		line += " httpOnly"
	}
	if c.Secure {
		line += " secure"
	}
	if c.Session {
		line += " session"
	}
	return line
}
