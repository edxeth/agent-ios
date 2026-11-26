package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/sim"
	"github.com/edxeth/agent-ios/internal/skills"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Private output (--private, AGENT_IOS_PRIVATE=1) is for a transcript that
// other parties read, as an agent's is. What agent-ios says about a page
// can be private: its title, its URL, its origin, its key with the device
// identifier, the session that holds it, and the text of an error, which
// can repeat any of them. In private mode agent-ios says none of that.
//
// The mode has two channels. Stdout holds the result, and only for a
// command whose result the caller asked for: a read (privateReads: content
// of the page, a value that the command exists to print, a path that the
// caller gave) or safe facts (privateSummaries). Stderr holds exactly one
// status line for every command: the command and its exit code, the counts
// of an action, and for a failure what the code means, never the text of
// the error. A command that is in neither list prints only its status
// line, so a new command is private until it is listed.
//
// A read prints what it reads. "eval location.href", "snapshot -u",
// "network requests" and "get cdp-url" print URLs, because that is their
// result: the mode does not mask content, it stops the tool from adding
// what the caller did not ask for. Each read names the members of its
// --json data that print (shape, project): a scalar prints as it is, an
// object or an array only through a shape that names its members, at
// every depth, and only a member marked content (free content that the
// caller asked for, as the value of eval) prints whatever it holds. So a
// member that a command adds to its data later, at any depth, does not
// print until it is named here. The text that prints is made from the
// members that print (privateRead.text), so the text holds nothing that
// the JSON data does not hold. The text of an error never prints, also
// inside a result: a failed request prints its state, not WebKit's error
// text. A command that changes something (storage set and clear, state
// save and load) is not a read: it prints its status line.
//
// The content boundary of a result in private mode names no origin
// (privateData, resultOrigin in output.go).
//
// The mode changes what the client prints. The daemon runs the command as
// usual, so a daemon of any version serves it.

// privateEnv is the environment variable that turns private mode on; the
// flag --private overrides it.
const privateEnv = "AGENT_IOS_PRIVATE"

// privateRead is a command whose result private mode prints: what the
// result is, data, the shape of the members of its JSON data that print
// (project), and text, which makes the text that prints from the value
// that project returned. text gets the command's own text only to choose
// among texts that it made from the value (chosen); it never prints it.
// So the text holds only what the JSON data holds, and fixed words. text
// returns false for a result of another shape, which prints as a status
// line.
type privateRead struct {
	what string
	data *shape
	text func(value json.RawMessage, commandText string) (string, bool)
}

// privateReads are the commands whose result private mode prints, each
// with what the result is and what of it prints. A member marked content
// is free content that the caller asked for; every other object or array
// prints only through a shape that names its members.
var privateReads = map[string]privateRead{
	"snapshot": {
		"the accessibility tree of the page; link URLs only with -u, in the text and the member snapshot",
		object(fields{"snapshot": content, "tree": listOf(privateNode), "generation": scalar, "refs": object(fields{"*": object(fields{"role": scalar, "name": scalar})}), "warnings": pageWarnings}),
		snapshotShown,
	},
	"diff snapshot": {
		"a line diff of two snapshots of the page",
		object(fields{"changed": scalar, "additions": scalar, "removals": scalar, "unchanged": scalar, "diff": content}),
		diffSnapshotShown,
	},
	"diff screenshot": {
		"pixel counts",
		object(fields{"width": scalar, "height": scalar, "total": scalar, "different": scalar, "ratio": scalar, "threshold": scalar, "match": scalar}),
		diffScreenshotShown,
	},
	"get": {
		"content of the page; url and title are refused in private mode (Check)",
		object(fields{"value": content, "warnings": pageWarnings}),
		getShown,
	},
	"is":   {"true or false", object(fields{"value": scalar, "warnings": pageWarnings}), isShown},
	"eval": {"the value of the caller's expression", object(fields{"type": scalar, "subtype": scalar, "value": content, "description": content}), evalShown},
	"console": {
		"console messages of the page, without the URL and the stack of their source",
		object(fields{"messages": listOf(privateMessage), "dropped": scalar}),
		consoleShown,
	},
	"errors": {"errors of the page, without the URL and the stack of their source", object(fields{"errors": listOf(privateError)}), errorsShown},
	"read":   {"the visible text of the page or of the caller's URL", object(fields{"text": content, "warnings": pageWarnings}), readShown},
	"find": {
		"elements of the page with refs; with an action, the status line of the action",
		object(fields{"matches": listOf(privateNode), "warnings": pageWarnings}),
		findShown,
	},
	"hit-test": {
		"the element at a point",
		object(fields{
			"match": listOf(privateNode), "element": scalar,
			"box":      object(fields{"x": scalar, "y": scalar, "width": scalar, "height": scalar}),
			"warnings": pageWarnings,
		}),
		hitTestShown,
	},
	"vitals": {
		"numbers of the page's performance",
		object(fields{
			"lcp": privateMetric, "cls": privateMetric, "ttfb": privateMetric, "fcp": privateMetric, "inp": privateMetric,
			"domContentLoaded": privateMetric, "load": privateMetric,
		}),
		vitalsShown,
	},
	"cookies": {
		"cookie names and attributes; values are masked unless the caller asks",
		object(fields{
			"cookies": listOf(object(fields{
				"name": scalar, "value": content, "domain": scalar, "path": scalar, "expires": scalar,
				"session": scalar, "httpOnly": scalar, "secure": scalar, "sameSite": scalar,
			})),
			"valuesShown": scalar,
		}),
		cookiesShown,
	},
	"storage": {
		"storage keys and values of a read; secret values are masked unless the caller asks; set and clear print their status line",
		object(fields{
			"area": scalar, "items": listOf(object(fields{"name": scalar, "value": content})),
			"key": scalar, "found": scalar, "value": content,
		}),
		storageShown,
	},
	"network requests": {
		"requests of the page; secret values are masked unless the caller asks",
		object(fields{
			"requests": listOf(privateRequest),
			"gaps":     listOf(object(fields{"start": scalar, "end": scalar, "afterSeq": scalar, "closed": listOf(scalar)})),
			"dropped":  scalar, "started": scalar,
		}),
		requestsShown,
	},
	"network request": {
		"one request of the page; secret values are masked unless the caller asks",
		object(fields{"request": privateRequest, "body": content, "base64Encoded": scalar}),
		requestShown,
	},
	"screenshot": {
		"the caller's path and the labels of --annotate",
		object(fields{
			"path": scalar, "sha256": scalar, "unchanged": scalar,
			"labels": listOf(object(fields{
				"n": scalar, "ref": scalar, "role": scalar, "name": scalar,
				"box": object(fields{"x": scalar, "y": scalar, "width": scalar, "height": scalar}),
			})),
		}),
		screenshotShown,
	},
	"sim ui": {
		"the screen's elements that the caller asked for, page and app text included",
		object(fields{"elements": content}),
		simUIShown,
	},
	"sim screenshot": {
		"the path of the simulator's screen capture and its size",
		object(fields{"path": scalar, "width": scalar, "height": scalar}),
		screenshotShown,
	},
	"network har stop": {"the caller's path and counts", object(fields{"path": scalar, "entries": scalar}), harStopShown},
	"trace stop":       {"the caller's path and counts", object(fields{"path": scalar, "records": scalar, "events": scalar}), traceStopShown},
	"profiler stop":    {"the caller's path and counts", object(fields{"path": scalar, "samples": scalar, "nodes": scalar}), profilerStopShown},
	"get cdp-url": {
		"the CDP URL that the caller asked for; it holds a token, so the caller keeps it in a variable",
		object(fields{"ws": scalar, "http": scalar, "scope": scalar}),
		cdpURLShown,
	},
	"tab find":    {"a tab id", object(fields{"tab": scalar}), memberShown("tab")},
	"session":     {"the caller's own session name", object(fields{"session": scalar}), memberShown("session")},
	"skills list": {"the skills of this binary", listOf(object(fields{"name": scalar, "description": scalar})), skillsListShown},
	"skills get":  {"a skill of this binary", listOf(object(fields{"name": scalar, "description": scalar, "content": content})), skillsGetShown},
	"skills path": {"a directory in the caller's cache", object(fields{"path": scalar}), memberShown("path")},
}

// privateSummaries are the commands that print safe facts of their result
// in private mode. Each function gets the result's JSON data and returns
// the lines to print and the JSON value for --json; ok is false for a
// result of another shape, which prints as a status line.
var privateSummaries = map[string]func(data json.RawMessage) (text string, value any, ok bool){
	"tab list":     tabsSummary,
	"tab":          tabsSummary, // without an id it lists the tabs
	"session info": sessionSummary,
	"status":       statusSummary,
	"sim up":       simUpSummary,
	"sim keyboard": simKeyboardSummary,
	"sim reserve":  simReserveSummary,
	"sim status":   simStatusSummary,
}

// privateData is the JSON data of a result in private mode. Its content
// boundary names no origin (resultOrigin): the origin is a fact about the
// page that the caller did not ask for.
type privateData struct{ v any }

func (d privateData) MarshalJSON() ([]byte, error) { return json.Marshal(d.v) }

// privateResult is what private mode prints for res, the result of a c
// that succeeded: shown goes to stdout, status is the line for stderr.
// With --json a command without a result of its own prints its status as
// an object, so that stdout is JSON for every command.
func privateResult(c *Command, res Result) (shown Result, status string) {
	shown, status = privateShown(c, res)
	shown.Data = privateData{shown.Data}
	return shown, status
}

// privateShown is privateResult without the mark of private data.
func privateShown(c *Command, res Result) (Result, string) {
	data, err := json.Marshal(res.Data)
	if err != nil {
		data = nil
	}
	status := c.Name + " exit 0"
	if warnings, dialogs, ok := actionCounts(data); ok {
		// An action names elements, other pages and dialog messages in
		// its text; its counts are lengths of arrays, which page text
		// cannot forge.
		status += fmt.Sprintf(" warnings %d dialogs %d", warnings, dialogs)
		return Result{Data: map[string]any{"command": c.Name, "exit": 0, "warnings": warnings, "dialogs": dialogs}}, status
	}
	if read, ok := privateReads[c.Name]; ok {
		if text, value, ok := read.show(res.Text, data); ok {
			return Result{Text: text, Data: value}, status
		}
	}
	if summary, ok := privateSummaries[c.Name]; ok {
		if text, value, ok := summary(data); ok {
			return Result{Text: text, Data: value}, status
		}
	}
	return Result{Data: map[string]any{"command": c.Name, "exit": 0}}, status
}

// shape is what private mode prints of a JSON value, by its kind:
//   - scalar: the value prints when it is a string, a number, a boolean or
//     null, and never when it is an object or an array;
//   - content: free content that the caller asked for and that has no
//     fixed shape (the value of eval, a body, page text): any value prints
//     as it is;
//   - object: an object prints with the members that the shape names,
//     each by its own shape; the member "*" stands for every member (an
//     object whose keys are data, as the refs of a snapshot or headers);
//   - list: an array prints with each element by the element's shape.
//
// So an object or an array prints only through a shape that names its
// members, at every depth, except where the shape says content; a member
// that a command adds later does not print until a shape names it. null
// prints as null for every kind. A value of another kind than its shape
// does not print, and an incomplete shape (no kind, no element shape, a
// nil member) prints nothing.
type shape struct {
	kind    shapeKind
	members fields // object
	elem    *shape // list
}

type shapeKind int

const (
	scalarKind shapeKind = iota + 1
	contentKind
	objectKind
	listKind
)

// fields are the members of an object shape.
type fields map[string]*shape

var (
	// scalar is the shape of a string, a number, a boolean or null.
	scalar = &shape{kind: scalarKind}
	// content is the shape of free content that the caller asked for.
	content = &shape{kind: contentKind}
)

// object is the shape of an object with the members f.
func object(f fields) *shape { return &shape{kind: objectKind, members: f} }

// listOf is the shape of an array whose elements have the shape elem.
func listOf(elem *shape) *shape { return &shape{kind: listKind, elem: elem} }

var (
	// pageWarnings are the warnings of the page runtime (strings).
	pageWarnings = listOf(scalar)
	// privateNode is a node of an accessibility tree (runtime.Node)
	// without its url: snapshot -u prints link URLs in the text, which
	// is what the flag asks for.
	privateNode = treeNode()
	// privateMetric is a metric of vitals (devtools.VitalsMetric); the
	// reason of an unavailable metric is a fixed sentence of agent-ios's
	// page function (devtools.vitalsFn), not the text of an error.
	privateMetric = object(fields{"value": scalar, "unavailable": scalar})
	// headers are HTTP headers: names are data, values strings.
	headers = object(fields{"*": scalar})
	// privateRequest is a request of the network log (devtools.Request)
	// without the protocol's target, frame and loader ids, without
	// WebKit's error text of a failed request (its state says that it
	// failed; the text can name what the caller did not ask for), and
	// without WebKit's resource timing, which is raw protocol JSON that no
	// text prints (startTs, responseTs and endTs give the times).
	privateRequest = object(fields{
		"id": scalar, "requestId": scalar, "seq": scalar, "url": scalar, "method": scalar, "type": scalar,
		"requestHeaders": headers, "postData": content, "status": scalar, "statusText": scalar, "responseHeaders": headers,
		"mimeType": scalar, "fromCache": scalar, "state": scalar, "wallTime": scalar,
		"startTs": scalar, "responseTs": scalar, "endTs": scalar, "dataLength": scalar, "encodedSize": scalar,
		"bodyGone": scalar,
	})
	// privateMessage is a console message (devtools.Message) without the
	// URL and the stack of its source, which name the page.
	privateMessage = object(fields{
		"id": scalar, "source": scalar, "level": scalar, "type": scalar, "text": content,
		"line": scalar, "column": scalar, "repeatCount": scalar, "timestamp": scalar,
	})
	// privateError is an error of the page (devtools.PageError) without
	// the URL and the stack of its source.
	privateError = object(fields{"id": scalar, "kind": scalar, "message": content, "line": scalar, "column": scalar})
)

// treeNode returns the shape of a tree node, whose children are nodes.
func treeNode() *shape {
	s := object(fields{
		"ref": scalar, "role": scalar, "name": scalar, "level": scalar, "checked": scalar, "expanded": scalar,
		"selected": scalar, "disabled": scalar, "required": scalar, "value": scalar,
		"cursor": object(fields{"kind": scalar, "hints": listOf(scalar), "text": scalar}),
	})
	s.members["children"] = listOf(s)
	return s
}

// project returns the parts of the JSON value raw that s prints, and
// false when raw does not have the kind of s.
func project(raw json.RawMessage, s *shape) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if s == nil || len(trimmed) == 0 {
		return nil, false
	}
	if string(trimmed) == "null" {
		return json.RawMessage("null"), true
	}
	switch s.kind {
	case contentKind:
		return raw, true
	case scalarKind:
		if trimmed[0] == '{' || trimmed[0] == '[' {
			return nil, false
		}
		return raw, true
	case listKind:
		var list []json.RawMessage
		if trimmed[0] != '[' || json.Unmarshal(raw, &list) != nil {
			return nil, false
		}
		out := make([]json.RawMessage, 0, len(list))
		for _, el := range list {
			if p, ok := project(el, s.elem); ok {
				out = append(out, p)
			}
		}
		return marshalRaw(out)
	case objectKind:
		var obj map[string]json.RawMessage
		if trimmed[0] != '{' || json.Unmarshal(raw, &obj) != nil {
			return nil, false
		}
		out := make(map[string]json.RawMessage, len(obj))
		for k, v := range obj {
			sub, named := s.members[k]
			if !named {
				sub, named = s.members["*"]
			}
			if !named {
				continue
			}
			if p, ok := project(v, sub); ok {
				out[k] = p
			}
		}
		return marshalRaw(out)
	}
	return nil, false
}

// marshalRaw is json.Marshal for project, whose values are valid JSON.
func marshalRaw(v any) (json.RawMessage, bool) {
	data, err := json.Marshal(v)
	return data, err == nil
}

// show is what private mode prints of a result of r: the text and the
// value of the JSON data. Data that is not an object or an array (a result
// without data) prints as a status line.
func (r privateRead) show(commandText string, data json.RawMessage) (string, json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
		return "", nil, false
	}
	value, ok := project(data, r.data)
	if !ok {
		return "", nil, false
	}
	text, ok := r.text(value, commandText)
	if !ok {
		return "", nil, false
	}
	return text, value, true
}

// chosen returns the candidate that is the command's text, else the
// first: a text function uses the command's text only to choose among
// texts that it made from the value.
func chosen(commandText string, candidates ...string) string {
	for _, c := range candidates {
		if c == commandText {
			return c
		}
	}
	return candidates[0]
}

// The text functions of privateReads. Each decodes the value that project
// returned, which holds only the members that private mode prints, and
// makes the command's text from it with the command's own text builder.

func snapshotShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Snapshot string   `json:"snapshot"`
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return withWarnings(v.Snapshot, v.Warnings), true
}

func diffSnapshotShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Diff      string `json:"diff"`
		Additions int    `json:"additions"`
		Removals  int    `json:"removals"`
		Unchanged int    `json:"unchanged"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return diffSnapshotText(v.Diff, v.Additions, v.Removals, v.Unchanged), true
}

func diffScreenshotShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Different int     `json:"different"`
		Total     int     `json:"total"`
		Ratio     float64 `json:"ratio"`
		Threshold float64 `json:"threshold"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return diffScreenshotText(v.Different, v.Total, v.Ratio, v.Threshold), true
}

// getShown prints the value of get as formatGet does: a string as it is,
// a count as a number, a missing attribute as nothing, the box (numbers)
// as JSON and the styles (strings) as "name: value" lines, sorted by name:
// the JSON object keeps no order.
func getShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Value    json.RawMessage `json:"value"`
		Warnings []string        `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	raw := bytes.TrimSpace(v.Value)
	text := string(raw)
	switch {
	case len(raw) == 0 || text == "null":
		text = ""
	case raw[0] == '"':
		if json.Unmarshal(raw, &text) != nil {
			return "", false
		}
	case raw[0] == '{':
		var styles map[string]string
		if json.Unmarshal(raw, &styles) == nil {
			names := slices.Sorted(maps.Keys(styles))
			lines := make([]string, len(names))
			for i, n := range names {
				lines[i] = n + ": " + styles[n]
			}
			text = strings.Join(lines, "\n")
		}
	}
	return withWarnings(text, v.Warnings), true
}

func isShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Value    *bool    `json:"value"`
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil || v.Value == nil {
		return "", false
	}
	return withWarnings(strconv.FormatBool(*v.Value), v.Warnings), true
}

func evalShown(value json.RawMessage, _ string) (string, bool) {
	var v webkit.RemoteObject
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	text, err := evalText(v)
	return text, err == nil
}

func readShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Text     string   `json:"text"`
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return withWarnings(v.Text, v.Warnings), true
}

func findShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Matches  []runtime.Node `json:"matches"`
		Warnings []string       `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return withWarnings(runtime.Render(v.Matches, runtime.RenderOptions{}), v.Warnings), true
}

func hitTestShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Match    []runtime.Node `json:"match"`
		Element  string         `json:"element"`
		Box      hitBox         `json:"box"`
		Warnings []string       `json:"warnings"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return withWarnings(hitTestText(v.Match, v.Element, v.Box), v.Warnings), true
}

func vitalsShown(value json.RawMessage, _ string) (string, bool) {
	var v devtools.Vitals
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return v.Text(), true
}

func cookiesShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Cookies []listedCookie `json:"cookies"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	if len(v.Cookies) == 0 {
		return noCookies, true
	}
	lines := make([]string, len(v.Cookies))
	for i, c := range v.Cookies {
		shown := hiddenValue
		if c.Value != nil {
			shown = *c.Value
		}
		lines[i] = cookieLine(webkit.Cookie{
			Name: c.Name, Domain: c.Domain, Path: c.Path, Expires: c.Expires,
			Session: c.Session, HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: c.SameSite,
		}, shown)
	}
	return strings.Join(lines, "\n"), true
}

// storageShown prints a storage read: the value of a key, or the keys and
// values of the area, and the hint when a value is masked, without the
// origin that the command's own text names. Set and clear change the
// storage: they print their status line.
func storageShown(value json.RawMessage, _ string) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(value, &m) != nil {
		return "", false
	}
	var area string
	if json.Unmarshal(m["area"], &area) != nil || area == "" {
		return "", false
	}
	_, listing := m["items"]
	_, read := m["found"]
	switch {
	case listing:
		var items []devtools.StorageEntry
		if json.Unmarshal(m["items"], &items) != nil {
			return "", false
		}
		if len(items) == 0 {
			return area + "Storage is empty.", true
		}
		lines := make([]string, 0, len(items)+1)
		masked := false
		for _, it := range items {
			masked = masked || storageMasked(it.Name, it.Value)
			lines = append(lines, it.Name+"="+it.Value)
		}
		if masked {
			lines = append(lines, maskedHint)
		}
		return strings.Join(lines, "\n"), true
	case read:
		var get struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if json.Unmarshal(value, &get) != nil {
			return "", false
		}
		text := get.Value
		if storageMasked(get.Key, get.Value) {
			text += "\n" + maskedHint
		}
		return text, true
	}
	return "", false
}

// storageMasked reports whether the storage command masked the value of
// key (runStorage). A value of a secret key that is the mask itself also
// counts; the hint then says what --show-values does.
func storageMasked(key, value string) bool {
	return value == devtools.Masked && devtools.SecretName(key)
}

// requestsShown prints the requests as network requests does, with the
// note that the capture started now when the command's text has it.
func requestsShown(value json.RawMessage, commandText string) (string, bool) {
	var snap devtools.Snapshot
	if json.Unmarshal(value, &snap) != nil {
		return "", false
	}
	first, _, _ := strings.Cut(commandText, "\n")
	return requestsText(chosen(first, "", netlogStartedNote), snap), true
}

// requestShown prints a request as network request does; the request that
// project returned has no error text.
func requestShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Request       *devtools.Request `json:"request"`
		Body          *string           `json:"body"`
		Base64Encoded bool              `json:"base64Encoded"`
	}
	if json.Unmarshal(value, &v) != nil || v.Request == nil {
		return "", false
	}
	text := requestHead(*v.Request)
	if v.Body != nil {
		body, err := devtools.DecodedBody([]byte(*v.Body), v.Base64Encoded)
		if err != nil {
			return "", false
		}
		text += bodyText(body)
	}
	return strings.TrimRight(text, "\n"), true
}

// simUIShown prints the elements in sim ui's text form.
func simUIShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Elements []sim.Element `json:"elements"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return simUIText(v.Elements), true
}

// screenshotShown prints the caller's path and the labels; the size of
// the image is not in the data, so the text does not say it.
func screenshotShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Path      string      `json:"path"`
		Unchanged bool        `json:"unchanged"`
		Labels    []shotLabel `json:"labels"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	if v.Unchanged {
		return "unchanged", true
	}
	lines := []string{"Screenshot saved to " + v.Path}
	for _, l := range v.Labels {
		lines = append(lines, l.line())
	}
	return strings.Join(lines, "\n"), true
}

func harStopShown(value json.RawMessage, commandText string) (string, bool) {
	var v struct {
		Path    string `json:"path"`
		Entries int    `json:"entries"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return chosen(commandText, harStopText(v.Entries, v.Path, false), harStopText(v.Entries, v.Path, true)), true
}

func traceStopShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Path    string `json:"path"`
		Records int    `json:"records"`
		Events  int    `json:"events"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return traceStopText(v.Path, v.Records, v.Events), true
}

func profilerStopShown(value json.RawMessage, _ string) (string, bool) {
	var v struct {
		Path    string `json:"path"`
		Samples int    `json:"samples"`
		Nodes   int    `json:"nodes"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return profilerStopText(v.Path, v.Samples, v.Nodes), true
}

// cdpURLShown prints the URL of the form that the caller asked for
// (--form): ws, else http.
func cdpURLShown(value json.RawMessage, commandText string) (string, bool) {
	var v struct {
		WS   string `json:"ws"`
		HTTP string `json:"http"`
	}
	if json.Unmarshal(value, &v) != nil {
		return "", false
	}
	return chosen(commandText, v.WS, v.HTTP), true
}

// memberShown returns the text function of a read whose text is the
// string member name.
func memberShown(name string) func(json.RawMessage, string) (string, bool) {
	return func(value json.RawMessage, _ string) (string, bool) {
		var v map[string]json.RawMessage
		var s string
		if json.Unmarshal(value, &v) != nil || json.Unmarshal(v[name], &s) != nil {
			return "", false
		}
		return s, true
	}
}

func skillsListShown(value json.RawMessage, _ string) (string, bool) {
	var list []skills.Skill
	if json.Unmarshal(value, &list) != nil {
		return "", false
	}
	return skillsListText(list), true
}

func skillsGetShown(value json.RawMessage, _ string) (string, bool) {
	var list []skills.Skill
	if json.Unmarshal(value, &list) != nil {
		return "", false
	}
	return skillsGetText(list), true
}

// consoleShown prints console messages as the console command does, with
// the id, the level, the text and the repeat count of each, but without
// the URL and the stack of the message's source: the URL of an inline
// script is the page's URL.
func consoleShown(value json.RawMessage, _ string) (string, bool) {
	var log struct {
		Messages []devtools.Message `json:"messages"`
		Dropped  *int               `json:"dropped"`
	}
	if json.Unmarshal(value, &log) != nil || log.Dropped == nil {
		return "", false
	}
	var lines []string
	if *log.Dropped > 0 {
		lines = append(lines, fmt.Sprintf("(%d older messages were dropped; the buffer keeps the last %d)", *log.Dropped, devtools.DefaultMaxMessages))
	}
	for _, m := range log.Messages {
		line := fmt.Sprintf("#%d [%s] %s", m.ID, m.Level, m.Text)
		if m.RepeatCount > 1 {
			line += fmt.Sprintf(" x%d", m.RepeatCount)
		}
		lines = append(lines, line)
	}
	if len(log.Messages) == 0 {
		lines = append(lines, "No console messages.")
	}
	return strings.Join(lines, "\n"), true
}

// errorsShown prints errors of the page as the errors command does, with
// the id, the kind and the message of each, but without the URL and the
// stack of the error's source, which name the page.
func errorsShown(value json.RawMessage, _ string) (string, bool) {
	var list struct {
		Errors *[]devtools.PageError `json:"errors"`
	}
	if json.Unmarshal(value, &list) != nil || list.Errors == nil {
		return "", false
	}
	if len(*list.Errors) == 0 {
		return "No errors.", true
	}
	lines := make([]string, len(*list.Errors))
	for i, e := range *list.Errors {
		lines[i] = fmt.Sprintf("#%d %s: %s", e.ID, e.Kind, e.Message)
	}
	return strings.Join(lines, "\n"), true
}

// actionCounts reports whether data is the result of an element or key
// action (actResult: it has the members trusted and steps), and the
// lengths of its warnings and dialogs.
func actionCounts(data json.RawMessage) (warnings, dialogs int, ok bool) {
	var act struct {
		Trusted  *bool             `json:"trusted"`
		Steps    *int              `json:"steps"`
		Warnings []json.RawMessage `json:"warnings"`
		Dialogs  []json.RawMessage `json:"dialogs"`
	}
	if json.Unmarshal(data, &act) != nil || act.Trusted == nil || act.Steps == nil {
		return 0, 0, false
	}
	return len(act.Warnings), len(act.Dialogs), true
}

// tabsSummary prints, for a tab listing, one line per tab (the current
// mark, the id, whether it is debuggable) and the number of hidden pages:
// no title, URL, label, key, device identity or lease holder; a simulator
// tab is marked as such.
func tabsSummary(data json.RawMessage) (string, any, bool) {
	var list struct {
		Tabs []struct {
			ID         string `json:"id"`
			Debuggable bool   `json:"debuggable"`
			Current    bool   `json:"current"`
			DeviceID   string `json:"deviceId"`
		} `json:"tabs"`
		Hidden *int `json:"hidden"`
	}
	if json.Unmarshal(data, &list) != nil || list.Hidden == nil {
		return "", nil, false
	}
	var b strings.Builder
	tabs := make([]map[string]any, 0, len(list.Tabs))
	for _, t := range list.Tabs {
		mark, state := " ", "debuggable"
		if t.Current {
			mark = "*"
		}
		if !t.Debuggable {
			state = "not-debuggable"
		}
		// A simulator tab is marked, without any device identity, so
		// that an agent tells it from a phone's (tabDevice).
		sim := t.DeviceID == core.SimulatorDevice
		kind := ""
		if sim {
			kind = " simulator"
		}
		fmt.Fprintf(&b, "%s %s %s%s\n", mark, t.ID, state, kind)
		tab := map[string]any{"id": t.ID, "current": t.Current, "debuggable": t.Debuggable}
		if sim {
			tab["simulator"] = true
		}
		tabs = append(tabs, tab)
	}
	fmt.Fprintf(&b, "hidden %d", *list.Hidden)
	if *list.Hidden > 0 {
		b.WriteString("\nnot listed: pages of physical devices without a reservation of your owner (run 'agent-ios device reserve --owner <owner>'), and the simulator's while another owner reserves it (run 'agent-ios sim reserve', it waits)")
	}
	return b.String(), map[string]any{"tabs": tabs, "hidden": *list.Hidden}, true
}

// sessionSummary prints, for each page of the session, its number in the
// list, its lease, its epoch and the state of its connection: no key or
// title. The member "session" marks the result of session info; "pages"
// is null for a session that holds no page.
func sessionSummary(data json.RawMessage) (string, any, bool) {
	var info struct {
		Session *string `json:"session"`
		Pages   []struct {
			Lease string `json:"lease"`
			Epoch uint64 `json:"epoch"`
			State string `json:"state"`
		} `json:"pages"`
	}
	if json.Unmarshal(data, &info) != nil || info.Session == nil {
		return "", nil, false
	}
	lines := []string{fmt.Sprintf("pages %d", len(info.Pages))}
	pages := make([]map[string]any, 0, len(info.Pages))
	for i, p := range info.Pages {
		lines = append(lines, fmt.Sprintf("page %d lease %s epoch %d connection %s", i+1, p.Lease, p.Epoch, p.State))
		pages = append(pages, map[string]any{"lease": p.Lease, "epoch": p.Epoch, "state": p.State})
	}
	return strings.Join(lines, "\n"), map[string]any{"pages": pages}, true
}

// statusSummary prints whether ios_webkit_debug_proxy answers and how many
// devices it lists: not a device identifier, and not the path of the log,
// which names the user.
func statusSummary(data json.RawMessage) (string, any, bool) {
	var st struct {
		Running             *bool `json:"running"`
		Devices             *int  `json:"devices"`
		SimulatorConfigured bool  `json:"simulatorConfigured"`
	}
	if json.Unmarshal(data, &st) != nil || st.Running == nil {
		return "", nil, false
	}
	text, value := fmt.Sprintf("iwdp answers: %t", *st.Running), map[string]any{"running": *st.Running}
	if st.Devices != nil {
		text += fmt.Sprintf("\ndevices %d", *st.Devices)
		value["devices"] = *st.Devices
	}
	if st.SimulatorConfigured {
		text += "\n" + simulatorHint
		value["simulatorConfigured"] = true
	}
	return text, value, true
}

// simUpSummary prints which steps sim up did and how many tabs Safari
// lists, without the simulator's name, UDID, host or socket path.
func simUpSummary(data json.RawMessage) (string, any, bool) {
	var r struct {
		MacStarted     *bool  `json:"macStarted"`
		Booted         bool   `json:"booted"`
		WindowOpened   bool   `json:"windowOpened"`
		SafariStarted  bool   `json:"safariStarted"`
		ForwardStarted bool   `json:"forwardStarted"`
		IWDPStarted    bool   `json:"iwdpStarted"`
		IWDPRestarted  bool   `json:"iwdpRestarted"`
		Pages          *int   `json:"pages"`
		TabOpened      bool   `json:"tabOpened"`
		SharedURL      bool   `json:"sharedURL"`
		Tab            string `json:"tab"`
	}
	if json.Unmarshal(data, &r) != nil || r.MacStarted == nil || r.Pages == nil {
		return "", nil, false
	}
	value := map[string]any{
		"macStarted": *r.MacStarted, "booted": r.Booted, "windowOpened": r.WindowOpened, "safariStarted": r.SafariStarted,
		"forwardStarted": r.ForwardStarted, "iwdpStarted": r.IWDPStarted, "iwdpRestarted": r.IWDPRestarted, "pages": *r.Pages,
	}
	text := fmt.Sprintf("mac started %t\nbooted %t\nwindow opened %t\nsafari started %t\nforward started %t\niwdp started %t\niwdp restarted %t\ntabs %d",
		*r.MacStarted, r.Booted, r.WindowOpened, r.SafariStarted, r.ForwardStarted, r.IWDPStarted, r.IWDPRestarted, *r.Pages)
	// The new tab's id (review F18) names no page: tab find prints ids too.
	switch {
	case r.Tab != "":
		text += "\nnew tab " + r.Tab
		value["tab"] = r.Tab
	case r.TabOpened && r.SharedURL:
		text += "\nnew tab unknown: " + simSharedURLNext
		value["sharedURL"] = true
	case r.TabOpened:
		text += "\nnew tab unknown: run agent-ios tab find --device SIMULATOR --url-file <file of the --url>"
	}
	return text, value, true
}

// simReserveSummary prints that the reservation is held and how long the
// wait took, without the owner's name or the time.
func simReserveSummary(data json.RawMessage) (string, any, bool) {
	var v struct {
		Reserved *bool  `json:"reserved"`
		Waited   string `json:"waited"`
	}
	if json.Unmarshal(data, &v) != nil || v.Reserved == nil {
		return "", nil, false
	}
	if _, err := time.ParseDuration(v.Waited); err != nil {
		return "", nil, false
	}
	return fmt.Sprintf("reserved %t\nwaited %s", *v.Reserved, v.Waited), map[string]any{"reserved": *v.Reserved, "waited": v.Waited}, true
}

// simKeyboardSummary prints the keyboard mode and whether it changed.
func simKeyboardSummary(data json.RawMessage) (string, any, bool) {
	var v struct {
		Mode    *string `json:"mode"`
		Changed bool    `json:"changed"`
	}
	if json.Unmarshal(data, &v) != nil || v.Mode == nil || (*v.Mode != "onscreen" && *v.Mode != "hardware") {
		return "", nil, false
	}
	return fmt.Sprintf("keyboard %s\nchanged %t", *v.Mode, v.Changed), map[string]any{"mode": *v.Mode, "changed": v.Changed}, true
}

// simStatusSummary prints the state of each part that sim status checks,
// without the simulator's name, UDID or host.
func simStatusSummary(data json.RawMessage) (string, any, bool) {
	var st struct {
		Ready        *bool `json:"ready"`
		MacReachable bool  `json:"macReachable"`
		LoggedIn     bool  `json:"loggedIn"`
		Device       *struct {
			State string `json:"state"`
		} `json:"device"`
		Window        bool   `json:"window"`
		Forward       bool   `json:"forward"`
		Listed        bool   `json:"listed"`
		ReservedBy    string `json:"reservedBy"`
		ReservedByYou bool   `json:"reservedByYou"`
	}
	if json.Unmarshal(data, &st) != nil || st.Ready == nil {
		return "", nil, false
	}
	booted := st.Device != nil && st.Device.State == "Booted"
	value := map[string]any{
		"ready": *st.Ready, "macReachable": st.MacReachable, "loggedIn": st.LoggedIn,
		"booted": booted, "window": st.Window, "forward": st.Forward, "listed": st.Listed,
		"reserved": st.ReservedBy != "", "reservedByYou": st.ReservedByYou,
	}
	text := fmt.Sprintf("ready %t\nmac answers %t\nlogged in %t\nbooted %t\nwindow %t\nforward %t\niwdp lists %t\nreserved %t\nreserved by you %t",
		*st.Ready, st.MacReachable, st.LoggedIn, booted, st.Window, st.Forward, st.Listed, st.ReservedBy != "", st.ReservedByYou)
	return text, value, true
}

// privateMeaning is what an exit code means, in words that name no page,
// device or session.
func privateMeaning(code int) string {
	switch code {
	case ExitUsage:
		return "usage error; 'agent-ios <command> --help' prints the usage"
	case ExitWaitTimeout:
		return "a wait timed out"
	case ExitBusy:
		return "the page is leased by another session, or its connection was lost"
	case ExitNoPage:
		return "no device or page"
	case ExitReservation:
		return "the device reservation is missing, or another owner holds it (on the simulator: run 'agent-ios sim reserve', which waits)"
	case ExitNoMatch:
		return "no tab shows the target"
	case ExitTimeout:
		return "the command deadline passed"
	case ExitInterrupted:
		return "interrupted by a signal"
	}
	return "the command failed"
}

// privateErrorLine is the error line of private mode: the command (""
// when the line names none), the exit code and its meaning. The text of
// the error can name a page, so it is not printed; the same command
// without --private prints it.
func privateErrorLine(name string, code int) string {
	if name == "" {
		name = "agent-ios"
	}
	// The simulator's turn-taking: fixed sentences that tell an agent what
	// to do, since the message that names the owner is hidden.
	switch {
	case name == "device release" && code == ExitReservation:
		return name + " exit 6: nothing of yours released: another owner holds that device, or you hold the simulator, which 'agent-ios sim release' releases (private mode hides the message)"
	case name == "sim reserve" && code == ExitWaitTimeout:
		return name + " exit 3: the simulator is still reserved by another agent; run 'agent-ios sim reserve' again to keep waiting, and do not use the simulator meanwhile (private mode hides the message)"
	case name == "sim up" && code == ExitNoMatch:
		return name + " exit 7: Safari shows no new tab of your --url (a tab of an earlier run with that URL does not count): check that your server answers on the same port of this host (curl http://127.0.0.1:<port>/) and runs outside your tool call; the simulator reaches this host at 10.0.2.2; sim up closes the new tab that its open made, and its message, which private mode hides, names a tab that it could not close"
	case strings.HasPrefix(name, "sim ") && code == ExitReservation:
		return fmt.Sprintf("%s exit %d: the simulator is reserved by another agent; run 'agent-ios sim reserve' (it waits), then this command (private mode hides the message)", name, code)
	}
	return fmt.Sprintf("%s exit %d: %s (private mode hides the message)", name, code, privateMeaning(code))
}

// privateRequested reports whether a command line asks for private mode,
// for a line that Parse refused. It reads the global options as Parse
// does (splitGlobals), so the value of another option is never taken for
// the flag: in "--private --session --private=false x" the session is
// named "--private=false" and the mode is on. When the global options
// themselves are malformed, the mode counts as on if the variable or any
// argument asks for it: a line that cannot be read stays private.
func privateRequested(args []string, env func(string) string) bool {
	g := Globals{}
	asked := false // the variable or an argument names the mode, in any form
	if v := env(privateEnv); v != "" {
		b, err := strconv.ParseBool(v)
		g.Private = err != nil || b
		asked = g.Private
	}
	for _, a := range args {
		if a == "--private" || strings.HasPrefix(a, "--private=") {
			asked = true
		}
	}
	if _, _, err := splitGlobals(args, &g); err != nil {
		return asked
	}
	return g.Private
}
