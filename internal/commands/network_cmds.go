package commands

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// network requests, network request, network route and network unroute:
// the daemon's network log and request routes (design
// docs/design/agentic-parity.md, section 4.2 "Network", 7.7 tests 1, 3
// and 8). The log starts with the lease: WebKit replays no network events
// (MAP 1.8). The legacy stream "network -d <duration>" stays a legacy
// command.
func init() {
	Register(&Command{
		Name:    "network requests",
		Summary: "List the requests captured since the lease: id, method, status, type and URL; network resets show as gaps.",
		Flags: []Flag{
			{Name: "filter", Type: FlagString, Help: "only URLs that contain this text"},
			{Name: "type", Type: FlagString, Help: "only this resource type (Document, Script, Fetch, XHR, Image, ...)"},
			{Name: "status", Type: FlagString, Help: "only this status: a code such as 404, a class such as 4xx, or failed"},
			{Name: "clear", Type: FlagBool, Help: "empty the log after printing"},
			{Name: "show-secrets", Type: FlagBool, Help: "print secret header, query and body values instead of " + devtools.Masked},
		},
		Page:    LeasedPage,
		Mutates: true, // starts the capture when it does not run
		Output:  OutputText,
		Run:     runNetworkRequests,
	})
	Register(&Command{
		Name:    "network request",
		Summary: "Show one request of 'network requests': headers, request body and status; --body adds the response body.",
		Args:    []Arg{{Name: "id", Help: "request id from 'network requests'", Required: true}},
		Flags: []Flag{
			{Name: "body", Type: FlagBool, Help: "add the response body (Network.getResponseBody)"},
			{Name: "show-secrets", Type: FlagBool, Help: "print secret header, query and body values instead of " + devtools.Masked},
		},
		Page:    LeasedPage,
		Mutates: true, // starts the capture when it does not run
		Output:  OutputText,
		Run:     runNetworkRequest,
	})
	Register(&Command{
		Name:    "network route",
		Summary: "Abort, fulfill or delay requests whose URL matches: a glob with * and ** matches the whole URL, other text any URL that contains it. Without --abort, --body or --status the request continues after --delay.",
		Args:    []Arg{{Name: "url", Help: "URL pattern", Required: true}},
		Flags: []Flag{
			{Name: "abort", Type: FlagBool, Help: "fail the request"},
			{Name: "body", Type: FlagString, Help: "answer with this body"},
			{Name: "status", Type: FlagInt, Help: "answer with this HTTP status (default 200 with --body)"},
			{Name: "content-type", Type: FlagString, Help: "Content-Type of the answer (default text/plain)"},
			{Name: "delay", Type: FlagDuration, Help: "hold the request this long first"},
			{
				Name: "mode", Type: FlagEnum, Enum: []string{devtools.ModeWIP, devtools.ModeJS}, Default: devtools.ModeWIP,
				Help: "wip: WebKit request interception; js: patch fetch and XMLHttpRequest in the page",
			},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runNetworkRoute,
	})
	Register(&Command{
		Name:    "network unroute",
		Summary: "Remove the routes with this pattern (all without one); requests that a route holds continue unchanged.",
		Args:    []Arg{{Name: "url", Help: "URL pattern as given to 'network route'"}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runNetworkUnroute,
	})
}

// netlogFor returns the session's network log, starting it when it does
// not run, and a note when it started now.
func netlogFor(ctx context.Context, d devtoolsCmd) (*devtools.Netlog, string, error) {
	n, started, err := d.p.StartNetlog(ctx, d.c, d.session)
	if err != nil {
		return nil, "", err
	}
	if started {
		return n, "(the network capture started now; requests made before it are not in the log)", nil
	}
	return n, "", nil
}

func runNetworkRequests(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	n, note, err := netlogFor(ctx, d)
	if err != nil {
		return Result{}, err
	}
	snap := n.Snapshot(devtools.Filter{URL: flagString(in, "filter"), Type: flagString(in, "type"), Status: flagString(in, "status")})
	if flagBool(in, "clear") {
		n.Clear()
	}
	if !flagBool(in, "show-secrets") {
		for i, r := range snap.Requests {
			snap.Requests[i] = maskRequest(r)
		}
	}
	lines := []string{}
	if note != "" {
		lines = append(lines, note)
	}
	if snap.Dropped > 0 {
		lines = append(lines, fmt.Sprintf("(%d older requests were dropped; the log keeps the last %d)", snap.Dropped, devtools.DefaultMaxEntries))
	}
	gaps := snap.Gaps
	for _, r := range snap.Requests {
		for len(gaps) > 0 && gaps[0].AfterSeq < r.Seq {
			lines = append(lines, gapLine(gaps[0]))
			gaps = gaps[1:]
		}
		lines = append(lines, requestLine(r))
	}
	for _, g := range gaps {
		lines = append(lines, gapLine(g))
	}
	if len(snap.Requests) == 0 {
		lines = append(lines, "No requests.")
	}
	if snap.Requests == nil {
		snap.Requests = []devtools.Request{}
	}
	return Result{Text: strings.Join(lines, "\n"), Data: snap}, nil
}

// requestLine is "id METHOD status type URL"; the status is the HTTP
// status, or the state of a request without one. The caller masks r
// (maskRequest) unless secrets are to be shown.
func requestLine(r devtools.Request) string {
	status := r.State
	if r.Status != 0 {
		status = fmt.Sprint(r.Status)
		if r.State != devtools.StateFinished {
			status += " " + r.State
		}
	}
	if r.Error != "" {
		status += " (" + r.Error + ")"
	}
	typ := r.Type
	if typ == "" {
		typ = "-"
	}
	method := r.Method
	if method == "" {
		method = "-" // seen only by its response (devtools.Netlog)
	}
	return fmt.Sprintf("%s %s %s %s %s", r.ID, method, status, typ, r.URL)
}

func gapLine(g devtools.Gap) string {
	return fmt.Sprintf("-- network reset %s to %s: %d request(s) in flight closed as %s; earlier response bodies are gone",
		g.Start.Format("15:04:05.000"), g.End.Format("15:04:05.000"), len(g.Closed), devtools.StateCaptureReset)
}

func runNetworkRequest(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	n, _, err := netlogFor(ctx, d)
	if err != nil {
		return Result{}, err
	}
	id := arg(in, 0)
	r, ok := n.Get(id)
	if !ok {
		return Result{}, fmt.Errorf("network request %q is not in the log (see 'network requests')", id)
	}
	show := flagBool(in, "show-secrets")
	if !show {
		r = maskRequest(r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", r.Method, r.URL)
	status := r.State
	if r.Status != 0 {
		status = fmt.Sprintf("%d %s (%s)", r.Status, r.StatusText, r.State)
	}
	fmt.Fprintf(&b, "Status: %s\n", status)
	if r.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", r.Error)
	}
	if r.Type != "" || r.MimeType != "" {
		fmt.Fprintf(&b, "Type: %s  MIME: %s\n", r.Type, r.MimeType)
	}
	writeHeaders(&b, "Request headers", r.RequestHeaders)
	if r.PostData != "" {
		fmt.Fprintf(&b, "Request body:\n%s\n", r.PostData)
	}
	writeHeaders(&b, "Response headers", r.ResponseHeaders)
	if r.BodyGone != "" {
		fmt.Fprintf(&b, "Note: %s\n", r.BodyGone)
	}
	data := map[string]any{"request": r}
	if flagBool(in, "body") {
		raw, b64, err := n.Body(ctx, d.c, id)
		if err != nil {
			return Result{}, err
		}
		body, err := devtools.DecodedBody(raw, b64)
		if err != nil {
			return Result{}, fmt.Errorf("network request %s: decoding the base64 body: %w", id, err)
		}
		switch {
		case !utf8.Valid(body):
			fmt.Fprintf(&b, "Body: %d bytes of binary data (use --json for base64)\n", len(body))
		case show:
			fmt.Fprintf(&b, "Body:\n%s\n", body)
		default:
			fmt.Fprintf(&b, "Body:\n%s\n", devtools.MaskBody(r.MimeType, string(body)))
		}
		if !utf8.Valid(body) || show {
			data["body"], data["base64Encoded"] = string(raw), b64
		} else {
			data["body"], data["base64Encoded"] = devtools.MaskBody(r.MimeType, string(body)), false
		}
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Data: data}, nil
}

// maskRequest returns r with the secret values that listings mask by
// default (devtools.Masked; design 7.9 and R17): secret query parameters
// and fragments and the userinfo password of the URL and of the URLs in
// WebKit's error text, secret request and response headers, and secret
// fields of the request body. The maps are new, so the copy in the
// network log keeps the values.
func maskRequest(r devtools.Request) devtools.Request {
	r.URL = devtools.MaskURL(r.URL)
	r.Error = devtools.MaskText(r.Error)
	r.RequestHeaders = devtools.MaskHeaders(r.RequestHeaders)
	r.ResponseHeaders = devtools.MaskHeaders(r.ResponseHeaders)
	if r.PostData != "" {
		r.PostData = devtools.MaskBody(headerOf(r.RequestHeaders, "Content-Type"), r.PostData)
	}
	return r
}

func headerOf(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func writeHeaders(b *strings.Builder, title string, h map[string]string) {
	if len(h) == 0 {
		return
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Fprintf(b, "%s:\n", title)
	for _, k := range names {
		fmt.Fprintf(b, "  %s: %s\n", k, h[k])
	}
}

func runNetworkRoute(ctx context.Context, env Env, in Input) (Result, error) {
	r := devtools.Route{Pattern: arg(in, 0), Abort: flagBool(in, "abort"), ContentType: flagString(in, "content-type"), Mode: flagString(in, "mode")}
	if s, ok := in.Flags["status"].(int); ok {
		if s < 100 || s > 599 {
			return Result{}, usagef("network route: --status %d: want an HTTP status from 100 to 599", s)
		}
		r.Status = s
	}
	if b, ok := in.Flags["body"].(string); ok {
		r.Body = &b
	}
	if dl, ok := in.Flags["delay"].(time.Duration); ok {
		if dl < 0 {
			return Result{}, usagef("network route: --delay %s: want 0 or more", dl)
		}
		r.Delay = dl
	}
	if r.Abort && (r.Body != nil || r.Status != 0) {
		return Result{}, usagef("network route: --abort cannot be combined with --body or --status")
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	added, err := d.p.AddRoute(ctx, d.c, d.session, r)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: fmt.Sprintf("Route %d: %s -> %s (mode %s)", added.ID, added.Pattern, routeAction(added), added.Mode), Data: added}, nil
}

// routeAction describes what a route does with a matching request.
func routeAction(r devtools.Route) string {
	var s string
	switch {
	case r.Abort:
		s = "abort"
	case r.Status != 0 || r.Body != nil:
		status := r.Status
		if status == 0 {
			status = http.StatusOK
		}
		size := 0
		if r.Body != nil {
			size = len(*r.Body)
		}
		s = fmt.Sprintf("fulfill %d (%d bytes)", status, size)
	default:
		s = "continue"
	}
	if r.Delay > 0 {
		s += " after " + r.Delay.String()
	}
	return s
}

func runNetworkUnroute(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	n, err := d.p.RemoveRoutes(ctx, d.c, d.session, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	return Result{Text: fmt.Sprintf("Removed %d route(s).", n), Data: map[string]int{"removed": n}}, nil
}
