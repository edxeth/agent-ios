package commands

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
)

// Content boundaries (--content-boundaries, design 2.11) mark the output of
// a command as page content, which an agent must treat as data and never as
// instructions. The format is agent-browser 0.41.2's (cli/src/output.rs,
// format_with_boundaries and print_primary_response), so agents that know
// agent-browser's markers read these too:
//
//	--- AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> origin=<origin> ---
//	<content>
//	--- END_AGENT_BROWSER_PAGE_CONTENT nonce=<nonce> ---
//
// With --json the result object gets a "_boundary" member {"nonce",
// "origin"} instead. The nonce is 16 random bytes in lowercase hex, new for
// every run, so page text cannot predict and close the boundary.
//
// The markers go around the output of every registry command, not only of
// commands that lease a page: commands without a lease also print page
// titles, URLs and protocol messages (tab list, session info, cdp log).
// Marking other text as page content is harmless; missing page text is not.
// Error messages on stderr are not marked, as in agent-browser. In private
// mode the marker names no origin (private.go).
const (
	boundaryStart = "--- AGENT_BROWSER_PAGE_CONTENT nonce=%s origin=%s ---"
	boundaryEnd   = "--- END_AGENT_BROWSER_PAGE_CONTENT nonce=%s ---"
	nonceBytes    = 16
	unknownOrigin = "unknown" // agent-browser's origin when the result names none
)

// boundary is the marker of one command result.
type boundary struct{ nonce, origin string }

// newBoundary returns the marker of a result whose JSON data (Result.Data)
// is data.
func newBoundary(data any) boundary {
	b := make([]byte, nonceBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error; it fills b or crashes the program
	return boundary{nonce: hex.EncodeToString(b), origin: resultOrigin(data)}
}

// resultOrigin is the origin (scheme://host[:port]) of the first of the
// result's "origin", "finalUrl" and "url" members that holds a URL with a
// host, the members agent-browser reads (boundary_origin); else "unknown".
// Only the origin is shown: a path, query or fragment can carry tokens.
// The data of a result in private mode (privateData) names no origin.
func resultOrigin(data any) string {
	if _, private := data.(privateData); private {
		return unknownOrigin
	}
	raw, err := json.Marshal(data) // also a json.RawMessage of the daemon (RemoteEnv)
	if err != nil {
		return unknownOrigin // writeResult reports the error with --json
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return unknownOrigin // not an object: no member names a URL
	}
	for _, key := range []string{"origin", "finalUrl", "url"} {
		var s string
		if json.Unmarshal(members[key], &s) != nil {
			continue
		}
		if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return unknownOrigin
}

// wrapText puts the markers around text.
func (b boundary) wrapText(text string) string {
	return fmt.Sprintf(boundaryStart, b.nonce, b.origin) + "\n" + text + "\n" + fmt.Sprintf(boundaryEnd, b.nonce)
}

// markJSON returns v, the JSON value of a result, with the "_boundary"
// member added. A result that is not an object goes under "data", as in
// agent-browser's response object. A "_boundary" member of the result
// itself is replaced. Members stay raw JSON, so numbers keep their
// precision.
func (b boundary) markJSON(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v // the caller's json.Marshal of v reports this error
	}
	obj := map[string]any{}
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) == nil && members != nil {
		for k, m := range members {
			obj[k] = m
		}
	} else {
		obj["data"] = json.RawMessage(data)
	}
	obj["_boundary"] = map[string]string{"nonce": b.nonce, "origin": b.origin}
	return obj
}
