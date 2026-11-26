package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/sourcemap"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// logpoint add, list, remove and clear: logpoints like chrome-devtools-mcp
// 1.14.0's set_logpoints (cdm-114 feasibility 3a). A logpoint is a WebKit
// URL breakpoint (Debugger.setBreakpointByUrl) whose options are
// autoContinue with an evaluate action, so the page never pauses at it:
// the action runs console.log in the scope of the location, and an
// expression that throws becomes a console error (WebKit reports the
// action's exception). The breakpoint is state of the owner
// "logpoint:<session>" in the page's desired state (core.LegacyCall): it
// survives reloads in WebKit, is replayed on a new or provisional target
// (cross-process navigations, reconnects), and the lease release removes
// it. Source maps are read host-side (internal/sourcemap) from the
// sourceMapURL of Debugger.scriptParsed.
func init() {
	Register(&Command{
		Name:    "logpoint add",
		Summary: "Log an expression to the console each time a script location runs, without pausing; the location is <url>:<line>[:<column>] (1-based; the url is a script URL or an original source of a source map, exact or as a path suffix). Prints the bound location.",
		Args: []Arg{
			{Name: "location", Help: "<url>:<line>[:<column>], for example app.js:42 or src/cart.ts:12:5", Required: true},
			{Name: "expression", Help: "what to log, like console.log arguments (several words are joined with spaces)", Required: true, Variadic: true},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runLogpointAdd,
	})
	Register(&Command{
		Name:    "logpoint list",
		Summary: "List the session's logpoints with their bound locations.",
		Page:    LeasedPage,
		Output:  OutputText,
		Run:     runLogpointList,
	})
	Register(&Command{
		Name:    "logpoint remove",
		Summary: "Remove a logpoint by the id that 'logpoint add' printed.",
		Args:    []Arg{{Name: "id", Help: "logpoint id", Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runLogpointRemove,
	})
	Register(&Command{
		Name:    "logpoint clear",
		Summary: "Remove all logpoints of the session.",
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runLogpointClear,
	})
}

// Position is a 1-based script location.
type Position struct {
	URL    string `json:"url"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

func (p Position) String() string { return fmt.Sprintf("%s:%d:%d", p.URL, p.Line, p.Column) }

// Logpoint is one logpoint of a session.
type Logpoint struct {
	ID         int    `json:"id"`
	Location   string `json:"location"` // as given
	Expression string `json:"expression"`
	// URL or URLRegex is the breakpoint's script: the URL of a loaded
	// script, or a regular expression for a path suffix that matched no
	// loaded script yet. Line and Column are 1-based, in that script.
	URL      string `json:"url,omitempty"`
	URLRegex string `json:"urlRegex,omitempty"`
	Line     int    `json:"line"`
	Column   int    `json:"column,omitempty"`
	// Original is the asked position in an original source of a source
	// map; the breakpoint is on the generated position it maps to.
	Original *Position `json:"original,omitempty"`
	// Bound are the locations WebKit resolved (empty while no matching
	// script is loaded), with their original source position for a
	// source-mapped logpoint.
	Bound []BoundPosition `json:"bound"`

	breakpointID string // the id WebKit gave the client (core's breakpoint entry)
}

// BoundPosition is a location where a logpoint is bound.
type BoundPosition struct {
	Position
	Original *Position `json:"original,omitempty"`
}

// lpScript is a script that WebKit reported in Debugger.scriptParsed:
// its id on the target (backend) that reported it, and the order of the
// reports (seq).
type lpScript struct {
	id, url, sourceMapURL, target string
	seq                           uint64
}

// key is the script's key in lpPage.scripts and lpPage.maps: script ids
// are per backend.
func (s lpScript) key() string { return scriptKey(s.target, s.id) }

func scriptKey(target, id string) string { return target + "\x00" + id }

// lpPage is the logpoint state of one page session: the scripts of the
// current documents that WebKit reported since the first logpoint command,
// the source maps read for them, and the logpoints of each session. A
// document's scripts and maps end with Debugger.globalObjectCleared of
// its target; scripts of another target than the active one are not
// candidates (resolve). The logpoints stay: they are desired state.
type lpPage struct {
	mu       sync.Mutex
	scripts  map[string]lpScript // by key()
	seq      uint64
	maps     map[string]*lpMap // by the script's key(); at most maxLpMaps
	mapUse   []string          // keys of maps, least recently used first
	mapSegs  int               // segments of the kept maps; at most maxLpMapSegments
	sessions map[string]*lpSession
	active   func() string // the connection's active target ("" when unknown)
}

// Budgets of the source maps a page keeps (FIX-D F13): a map is read once
// per script and kept until its document ends; each map alone is within
// sourcemap.DefaultLimits. Beyond these, the least recently used maps are
// dropped (and read again when a later logpoint needs them).
const (
	maxLpMaps        = 32
	maxLpMapSegments = 4_000_000
)

type lpMap struct {
	m       *sourcemap.Map
	sources []string // resolved
}

type lpSession struct {
	next   int
	points []*Logpoint
}

var lpPages sync.Map // *core.PageSession -> *lpPage

// lpFor returns the logpoint state of ps, subscribing to the Debugger
// events on first use. Subscriptions in the read loop (SubscribeSync) see
// the scriptParsed events that a Debugger.enable produces before its
// reply.
func lpFor(ps *core.PageSession) *lpPage {
	if p, ok := lpPages.Load(ps); ok {
		return p.(*lpPage)
	}
	p := newLpPage()
	if at, ok := ps.Caller.(interface{ ActiveTarget() string }); ok {
		p.active = at.ActiveTarget
	}
	actual, loaded := lpPages.LoadOrStore(ps, p)
	if loaded {
		return actual.(*lpPage)
	}
	sub := func(method string, h webkit.TargetEventHandler) func() {
		return ps.Caller.Subscribe(method, func(m string, params json.RawMessage) { h("", m, params) })
	}
	if ss, ok := ps.Caller.(interface {
		SubscribeTargetSync(string, webkit.TargetEventHandler) func()
	}); ok {
		sub = ss.SubscribeTargetSync
	}
	u1 := sub("Debugger.scriptParsed", func(target, _ string, params json.RawMessage) { p.scriptParsed(target, params) })
	u2 := sub("Debugger.breakpointResolved", func(target, _ string, params json.RawMessage) { p.resolved(ps, target, params) })
	u3 := sub("Debugger.globalObjectCleared", func(target, _ string, _ json.RawMessage) { p.cleared(target) })
	go func() {
		// The state ends with the page connection, as devtools.Page's
		// does; a lost connection that is taken over keeps it.
		cs, isConn := ps.Caller.(interface{ State() core.ConnState })
		for {
			<-ps.Caller.Done()
			if !isConn || cs.State() != core.ConnLost {
				break
			}
			for cs.State() == core.ConnLost {
				time.Sleep(500 * time.Millisecond)
			}
		}
		u1()
		u2()
		u3()
		lpPages.Delete(ps)
	}()
	return p
}

func newLpPage() *lpPage {
	return &lpPage{scripts: map[string]lpScript{}, maps: map[string]*lpMap{}, sessions: map[string]*lpSession{}, active: func() string { return "" }}
}

func (p *lpPage) scriptParsed(target string, params json.RawMessage) {
	var v struct {
		ScriptID     string `json:"scriptId"`
		URL          string `json:"url"`
		SourceURL    string `json:"sourceURL"`
		SourceMapURL string `json:"sourceMapURL"`
	}
	if json.Unmarshal(params, &v) != nil || v.ScriptID == "" {
		return
	}
	u := v.URL
	if u == "" {
		u = v.SourceURL
	}
	if u == "" {
		return // eval code, among it every logpoint action's
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	sc := lpScript{id: v.ScriptID, url: u, sourceMapURL: v.SourceMapURL, target: target, seq: p.seq}
	if old, ok := p.scripts[sc.key()]; ok && old.sourceMapURL != sc.sourceMapURL {
		p.dropMapLocked(sc.key())
	}
	p.scripts[sc.key()] = sc
}

// cleared forgets the scripts of target's document and their source maps
// (Debugger.globalObjectCleared: a navigation or reload of that target),
// so that a new logpoint resolves against the new document's scripts and
// reads their maps again.
func (p *lpPage) cleared(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, sc := range p.scripts {
		if sc.target == target {
			delete(p.scripts, k)
			p.dropMapLocked(k)
		}
	}
}

// currentScriptsLocked returns the scripts of the active target (all when
// it is unknown), the latest one of each URL, sorted by URL.
func (p *lpPage) currentScriptsLocked() []lpScript {
	active := p.active()
	byURL := map[string]lpScript{}
	for _, sc := range p.scripts {
		if active != "" && sc.target != "" && sc.target != active {
			continue
		}
		if old, ok := byURL[sc.url]; !ok || sc.seq > old.seq {
			byURL[sc.url] = sc
		}
	}
	out := make([]lpScript, 0, len(byURL))
	for _, sc := range byURL {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].url < out[j].url })
	return out
}

// dropMapLocked forgets the map of the script key k.
func (p *lpPage) dropMapLocked(k string) {
	m, ok := p.maps[k]
	if !ok {
		return
	}
	delete(p.maps, k)
	p.mapSegs -= m.m.Segments()
	p.mapUse = slices.DeleteFunc(p.mapUse, func(x string) bool { return x == k })
}

// putMapLocked keeps m as the map of the script key k, dropping the least
// recently used maps beyond the budgets (maxLpMaps, maxLpMapSegments).
func (p *lpPage) putMapLocked(k string, m *lpMap) {
	p.dropMapLocked(k)
	for len(p.mapUse) > 0 && (len(p.maps)+1 > maxLpMaps || p.mapSegs+m.m.Segments() > maxLpMapSegments) {
		p.dropMapLocked(p.mapUse[0])
	}
	p.maps[k] = m
	p.mapSegs += m.m.Segments()
	p.mapUse = append(p.mapUse, k)
}

// useMapLocked returns the kept map of the script key k and marks it used.
func (p *lpPage) useMapLocked(k string) *lpMap {
	m := p.maps[k]
	if m != nil {
		p.mapUse = slices.DeleteFunc(p.mapUse, func(x string) bool { return x == k })
		p.mapUse = append(p.mapUse, k)
	}
	return m
}

// resolved updates the bound location of the logpoint whose breakpoint
// WebKit resolved (a script that loaded later, or the same script after a
// reload): only a logpoint of the session that owns the breakpoint, found
// by the id that session got, also after a replay on a new target gave
// the breakpoint another one.
func (p *lpPage) resolved(ps *core.PageSession, target string, params json.RawMessage) {
	var v struct {
		BreakpointID string      `json:"breakpointId"`
		Location     wipLocation `json:"location"`
	}
	if json.Unmarshal(params, &v) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for session, s := range p.sessions {
		id, ok := ps.State.BreakpointClientID(core.SessionOwner("logpoint", session), target, v.BreakpointID)
		if !ok {
			continue
		}
		for _, lp := range s.points {
			if lp.breakpointID == id {
				lp.Bound = []BoundPosition{p.boundLocked(lp, target, v.Location)}
			}
		}
	}
}

type wipLocation struct {
	ScriptID     string `json:"scriptId"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

// boundLocked is the 1-based position of a WebKit location of target,
// with its original position for a source-mapped logpoint: from the map of
// that script, or, while that map is not read (a reload made a new
// script), the original position lp had at the same generated position.
func (p *lpPage) boundLocked(lp *Logpoint, target string, l wipLocation) BoundPosition {
	k := scriptKey(target, l.ScriptID)
	u := p.scripts[k].url
	if u == "" {
		u = lp.URL
	}
	b := BoundPosition{Position: Position{URL: u, Line: l.LineNumber + 1, Column: l.ColumnNumber + 1}}
	if lp.Original == nil {
		return b
	}
	if m := p.maps[k]; m != nil {
		if x, ok := m.m.Original(l.LineNumber, l.ColumnNumber); ok {
			b.Original = &Position{URL: m.sources[x.Source], Line: x.OrigLine + 1, Column: x.OrigCol + 1}
		}
		return b
	}
	for _, old := range lp.Bound {
		if old.Position == b.Position && old.Original != nil {
			o := *old.Original
			b.Original = &o
		}
	}
	return b
}

// snapshot is a copy of lp that shares no memory with it, for output
// outside p.mu.
func (lp *Logpoint) snapshot() *Logpoint {
	c := *lp
	if lp.Original != nil {
		o := *lp.Original
		c.Original = &o
	}
	c.Bound = make([]BoundPosition, len(lp.Bound))
	for i, b := range lp.Bound {
		c.Bound[i] = b
		if b.Original != nil {
			o := *b.Original
			c.Bound[i].Original = &o
		}
	}
	return &c
}

// pointsLocked returns the session's logpoints whose breakpoint is still
// owned state; a lease release removed the others in the page.
func (p *lpPage) pointsLocked(ps *core.PageSession, session string) *lpSession {
	s := p.sessions[session]
	if s == nil {
		s = &lpSession{next: 1}
		p.sessions[session] = s
	}
	kept := s.points[:0]
	for _, lp := range s.points {
		if ps.State.HasBreakpoint(lp.breakpointID) {
			kept = append(kept, lp)
		}
	}
	s.points = kept
	return s
}

// lpCmd is what a logpoint command works with.
type lpCmd struct {
	ps      *core.PageSession
	c       webkit.Caller
	session string
	owner   core.Owner
	p       *lpPage
}

func logpointCommand(ctx context.Context, env Env) (lpCmd, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return lpCmd{}, err
	}
	if ps.State == nil || ps.Sched == nil {
		return lpCmd{}, errors.New("logpoints are page state of the agent-ios daemon; this page session has none")
	}
	session, _ := ps.Sched.Lease()
	if session == "" {
		return lpCmd{}, errors.New("the page has no lease holder; logpoints belong to a session")
	}
	c := webkit.Caller(ps.Caller)
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		c = oe.Op().Caller()
	}
	return lpCmd{ps: ps, c: c, session: session, owner: core.SessionOwner("logpoint", session), p: lpFor(ps)}, nil
}

// call sends method as the logpoint owner's desired-state call.
func (l lpCmd) call(ctx context.Context, method string, params any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	res, err := core.LegacyCall(ctx, l.ps, l.owner, l.c, method, raw)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			return fmt.Errorf("%s: bad reply %s: %w", method, res, err)
		}
	}
	return nil
}

// ParseLocation splits <url>:<line>[:<column>] (1-based). The column is 0
// when absent. A trailing ":<n>:<m>" is a line and column unless the
// part before it is a URL authority without a path
// ("http://host:8080:12" is line 12 of http://host:8080).
func ParseLocation(loc string) (u string, line, col int, err error) {
	bad := fmt.Errorf("logpoint location %q: want <url>:<line>[:<column>] with a 1-based line", loc)
	i := strings.LastIndexByte(loc, ':')
	if i <= 0 {
		return "", 0, 0, bad
	}
	last, err := strconv.Atoi(loc[i+1:])
	if err != nil || last < 1 {
		return "", 0, 0, bad
	}
	u, line = loc[:i], last
	if j := strings.LastIndexByte(u, ':'); j > 0 {
		if n, err := strconv.Atoi(u[j+1:]); err == nil && n >= 1 && !authorityOnly(u[:j]) {
			u, line, col = u[:j], n, last
		}
	}
	if u == "" {
		return "", 0, 0, bad
	}
	return u, line, col, nil
}

// authorityOnly reports whether u is "scheme://host" without a path, so
// that a following ":n" is its port.
func authorityOnly(u string) bool {
	_, rest, ok := strings.Cut(u, "://")
	return ok && !strings.Contains(rest, "/")
}

// lpTarget is where a location resolved.
type lpTarget struct {
	url, regex string
	line, col  int // 0-based, generated
	original   *Position
}

// resolve finds the script of a location: an exact script URL, an exact
// original source of a loaded source map, else a unique path suffix of
// either (an original source wins over a script at the same URL, as in
// chrome-devtools-mcp: a dev server serves a transpiled file at the URL
// of its source). Every loaded script whose map has the source is a
// candidate: the one with the mapping nearest at or after the position
// wins, and two scripts with the same nearest original position are
// reported as ambiguous. Without a match the breakpoint waits for the
// script: an absolute URL as given, a suffix as a regular expression.
func (l lpCmd) resolve(ctx context.Context, u string, line, col int) (lpTarget, error) {
	l.p.mu.Lock()
	scripts := l.p.currentScriptsLocked()
	l.p.mu.Unlock()

	type cand struct {
		url    string
		script lpScript
		m      *lpMap
		src    int // source index when m != nil
	}
	var cands []cand
	matches := func(c string) bool { return c == u || strings.HasSuffix(c, "/"+strings.TrimPrefix(u, "/")) }
	var mapErrs []string
	for _, s := range scripts {
		if matches(s.url) {
			cands = append(cands, cand{url: s.url, script: s})
		}
		if s.sourceMapURL == "" {
			continue
		}
		m, err := l.loadMap(ctx, s)
		if err != nil {
			mapErrs = append(mapErrs, err.Error())
			continue
		}
		for i, src := range m.sources {
			if matches(src) || m.m.Sources[i] == u {
				cands = append(cands, cand{url: src, script: s, m: m, src: i})
			}
		}
	}
	names := map[string]bool{}
	for _, c := range cands {
		names[c.url] = true
	}
	if names[u] {
		cands = slices.DeleteFunc(cands, func(c cand) bool { return c.url != u })
	} else if len(names) > 1 {
		return lpTarget{}, fmt.Errorf("logpoint location %q matches several sources: %s; give a longer path", u, strings.Join(slices.Sorted(maps.Keys(names)), ", "))
	}
	col0 := max(col-1, 0)
	var mapped []cand
	for _, c := range cands {
		if c.m != nil {
			mapped = append(mapped, c)
		}
	}
	switch {
	case len(mapped) > 0:
		var best []cand
		var bestG sourcemap.Mapping
		for _, c := range mapped {
			g, found := c.m.m.Generated(c.src, line-1, col0)
			switch {
			case !found:
			case len(best) == 0 || g.OrigLine < bestG.OrigLine || g.OrigLine == bestG.OrigLine && g.OrigCol < bestG.OrigCol:
				best, bestG = []cand{c}, g
			case g.OrigLine == bestG.OrigLine && g.OrigCol == bestG.OrigCol:
				best = append(best, c)
			}
		}
		pick := mapped[0]
		if len(best) == 0 {
			var in []string
			for _, c := range mapped {
				in = append(in, c.script.url)
			}
			return lpTarget{}, fmt.Errorf("logpoint location %s:%d: the source map of %s maps no code at or after this line", pick.url, line, strings.Join(in, ", "))
		}
		if len(best) > 1 {
			var in []string
			for _, c := range best {
				in = append(in, c.script.url)
			}
			return lpTarget{}, fmt.Errorf("logpoint location %s:%d is code of several loaded scripts (%s); a logpoint logs in one script: give that script's URL and generated line instead", best[0].url, line, strings.Join(in, ", "))
		}
		pick = best[0]
		g, _ := pick.m.m.Generated(pick.src, line-1, col0)
		orig := &Position{URL: pick.url, Line: line, Column: col}
		return lpTarget{url: pick.script.url, line: g.GenLine, col: g.GenCol, original: orig}, nil
	case len(cands) > 0:
		return lpTarget{url: cands[0].url, line: line - 1, col: col0}, nil
	case strings.Contains(u, "://"):
		return lpTarget{url: u, line: line - 1, col: col0}, nil
	}
	if len(mapErrs) > 0 {
		return lpTarget{}, fmt.Errorf("logpoint location %q matches no loaded script, and source maps could not be read: %s", u, strings.Join(mapErrs, "; "))
	}
	return lpTarget{regex: "(^|/)" + regexp.QuoteMeta(strings.TrimPrefix(u, "/")) + "$", line: line - 1, col: col0}, nil
}

// loadMap reads the source map of s once per script (a new document's
// script is read again): a data: URL in Go, another URL through WebKit's
// Network.loadResource (the page's network, as Web Inspector loads source
// maps; a device's localhost is not the host's). A map beyond
// sourcemap.DefaultLimits is refused.
func (l lpCmd) loadMap(ctx context.Context, s lpScript) (*lpMap, error) {
	key := s.key()
	l.p.mu.Lock()
	m := l.p.useMapLocked(key)
	l.p.mu.Unlock()
	if m != nil {
		return m, nil
	}
	lim := sourcemap.DefaultLimits
	tooLarge := func(what string, err error) error {
		return fmt.Errorf("source map %s of %s: %w; set the logpoint on the generated script (%s:<line>) instead", what, s.url, err, s.url)
	}
	mapURL := s.sourceMapURL
	var data []byte
	if strings.HasPrefix(mapURL, "data:") {
		if len(mapURL) > lim.Bytes/3*4+1024 {
			return nil, tooLarge("(inline)", fmt.Errorf("%w: a data: URL of %d bytes, more than %d bytes of map", sourcemap.ErrTooLarge, len(mapURL), lim.Bytes))
		}
		d, err := sourcemap.DataURL(mapURL)
		if err != nil {
			return nil, fmt.Errorf("source map of %s: %w", s.url, err)
		}
		data, mapURL = d, s.url
	} else {
		if b, err := url.Parse(s.url); err == nil {
			if r, err := b.Parse(mapURL); err == nil {
				mapURL = r.String()
			}
		}
		var tree struct {
			FrameTree struct {
				Frame struct {
					ID string `json:"id"`
				} `json:"frame"`
			} `json:"frameTree"`
		}
		if err := sendInto(ctx, l.c, "Page.getResourceTree", nil, &tree); err != nil {
			return nil, fmt.Errorf("source map %s: %w", mapURL, err)
		}
		var res struct {
			Content string `json:"content"`
			Status  int    `json:"status"`
		}
		if err := sendInto(ctx, l.c, "Network.loadResource", map[string]string{"frameId": tree.FrameTree.Frame.ID, "url": mapURL}, &res); err != nil {
			return nil, fmt.Errorf("source map %s: %w", mapURL, err)
		}
		if res.Status >= 400 {
			return nil, fmt.Errorf("source map %s: HTTP status %d", mapURL, res.Status)
		}
		data = []byte(res.Content)
	}
	sm, err := sourcemap.ParseWithLimits(data, lim)
	if errors.Is(err, sourcemap.ErrTooLarge) {
		return nil, tooLarge(mapURL, err)
	}
	if err != nil {
		return nil, fmt.Errorf("source map %s: %w", mapURL, err)
	}
	m = &lpMap{m: sm, sources: sm.ResolvedSources(mapURL)}
	l.p.mu.Lock()
	if _, live := l.p.scripts[key]; live {
		// Kept only while its script's document lives (cleared).
		l.p.putMapLocked(key, m)
	}
	l.p.mu.Unlock()
	return m, nil
}

// sendInto sends method and decodes its reply into out.
func sendInto(ctx context.Context, c webkit.Caller, method string, params, out any) error {
	raw, err := c.Send(ctx, method, params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: bad reply: %w", method, err)
	}
	return nil
}

// logpointAction is the JavaScript of a logpoint's evaluate action: it
// logs the expression's values in one console message (the console
// command prints a message's text, which WebKit takes from the first
// argument only), strings as they are and other values as JSON. The
// expression is a list like console.log's arguments; its own line breaks
// and comments cannot reach the wrapper.
func logpointAction(expr string) string {
	return "console.log([\n" + expr + "\n].map(function (v) { if (typeof v === \"string\") return v; try { var j = JSON.stringify(v); if (j !== undefined) return j; } catch (e) {} return String(v); }).join(\" \"))"
}

// syntaxCheck compiles the action in the page without running it
// (new Function), so that an expression with a syntax error is refused
// instead of logging an error on every hit. A page whose content security
// policy forbids new Function skips the check.
func (l lpCmd) syntaxCheck(ctx context.Context, action string) error {
	src, _ := json.Marshal(action)
	expr := "(function () { try { new Function(" + string(src) + "); return \"\"; } catch (e) { return e instanceof SyntaxError ? String(e.message) : \"\"; } })()"
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := sendInto(ctx, l.c, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &r); err != nil {
		return err
	}
	if r.Result.Value != "" {
		return fmt.Errorf("the logpoint expression does not compile: %s; give it like console.log arguments", r.Result.Value)
	}
	return nil
}

func runLogpointAdd(ctx context.Context, env Env, in Input) (Result, error) {
	loc := in.Args[0]
	expr := strings.Join(in.Args[1:], " ")
	u, line, col, err := ParseLocation(loc)
	if err != nil {
		return Result{}, &UsageError{err.Error()}
	}
	if strings.TrimSpace(expr) == "" {
		return Result{}, usagef("logpoint add: the expression is empty")
	}
	l, err := logpointCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	action := logpointAction(expr)
	if err := l.syntaxCheck(ctx, action); err != nil {
		return Result{}, err
	}
	// Debugger for the owner first, in a state where the page cannot
	// pause (no pause on debugger statements or exceptions before the
	// enable, then active breakpoints: a new WebKit backend starts with
	// them inactive). Its enable reports the loaded scripts
	// (scriptParsed) that resolve needs.
	if err := core.AcquireNoPauseDebugger(ctx, l.ps, l.owner, l.c); err != nil {
		return Result{}, l.dropIfEmpty(ctx, fmt.Errorf("logpoint add: Debugger could not be enabled without pausing the page, so no logpoint was set: %w", err))
	}
	t, err := l.resolve(ctx, u, line, col)
	if err != nil {
		return Result{}, l.dropIfEmpty(ctx, err)
	}
	params := map[string]any{
		"lineNumber": t.line, "columnNumber": t.col,
		"options": map[string]any{"autoContinue": true, "actions": []any{map[string]any{"type": "evaluate", "data": action}}},
	}
	if t.url != "" {
		params["url"] = t.url
	} else {
		params["urlRegex"] = t.regex
	}
	var reply struct {
		BreakpointID string        `json:"breakpointId"`
		Locations    []wipLocation `json:"locations"`
	}
	if err := l.call(ctx, "Debugger.setBreakpointByUrl", params, &reply); err != nil {
		return Result{}, l.dropIfEmpty(ctx, fmt.Errorf("%w (a logpoint or breakpoint at the same location exists already: see 'logpoint list')", err))
	}
	lp := &Logpoint{
		Location: loc, Expression: expr, URL: t.url, URLRegex: t.regex, Line: t.line + 1, Original: t.original,
		breakpointID: reply.BreakpointID, Bound: []BoundPosition{},
	}
	if col > 0 || t.original != nil {
		lp.Column = t.col + 1
	}
	l.p.mu.Lock()
	target := l.p.active()
	for _, x := range reply.Locations {
		lp.Bound = append(lp.Bound, l.p.boundLocked(lp, target, x))
	}
	known := slices.ContainsFunc(l.p.currentScriptsLocked(), func(sc lpScript) bool { return sc.url == t.url })
	l.p.mu.Unlock()
	if len(lp.Bound) == 0 && known {
		// The script is loaded and WebKit bound nothing: no code at or
		// after the line, or another breakpoint holds the resolved
		// location. A logpoint that cannot log is not kept.
		_ = l.call(ctx, "Debugger.removeBreakpoint", map[string]string{"breakpointId": reply.BreakpointID}, nil)
		return Result{}, l.dropIfEmpty(ctx, fmt.Errorf("no executable code at or after %s in %s, or another logpoint is bound at the line it moves to", loc, t.url))
	}
	l.p.mu.Lock()
	s := l.p.pointsLocked(l.ps, l.session)
	lp.ID = s.next
	s.next++
	s.points = append(s.points, lp)
	out := lp.snapshot() // a breakpointResolved may change lp from now on
	l.p.mu.Unlock()
	return Result{Text: "logpoint " + lpLine(out), Data: out}, nil
}

// dropIfEmpty ends the owner's Debugger state when the session has no
// logpoint (an add that failed after the enable), and returns err.
func (l lpCmd) dropIfEmpty(ctx context.Context, err error) error {
	l.p.mu.Lock()
	n := len(l.p.pointsLocked(l.ps, l.session).points)
	l.p.mu.Unlock()
	if n == 0 {
		if derr := l.call(ctx, "Debugger.disable", nil, nil); derr != nil {
			return errors.Join(err, derr)
		}
	}
	return err
}

// lpLine is "<id>: <location> <expression> -> <where>".
func lpLine(lp *Logpoint) string {
	where := "not bound yet: no loaded script matches; it binds when one loads"
	if len(lp.Bound) > 0 {
		parts := make([]string, len(lp.Bound))
		for i, b := range lp.Bound {
			parts[i] = b.String()
			if b.Original != nil {
				parts[i] = b.Original.String() + " (generated " + b.String() + ")"
			}
		}
		where = "bound at " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%d: %s %s -> %s", lp.ID, lp.Location, lp.Expression, where)
}

func runLogpointList(ctx context.Context, env Env, _ Input) (Result, error) {
	l, err := logpointCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	l.p.mu.Lock()
	points := []*Logpoint{}
	for _, lp := range l.p.pointsLocked(l.ps, l.session).points {
		points = append(points, lp.snapshot())
	}
	l.p.mu.Unlock()
	if len(points) == 0 {
		return Result{Text: "No logpoints.", Data: map[string]any{"logpoints": points}}, nil
	}
	lines := make([]string, len(points))
	for i, lp := range points {
		lines[i] = lpLine(lp)
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"logpoints": points}}, nil
}

func runLogpointRemove(ctx context.Context, env Env, in Input) (Result, error) {
	id, err := strconv.Atoi(in.Args[0])
	if err != nil {
		return Result{}, usagef("logpoint remove: %q is not a logpoint id (see 'logpoint list')", in.Args[0])
	}
	l, err := logpointCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	l.p.mu.Lock()
	s := l.p.pointsLocked(l.ps, l.session)
	var lp *Logpoint
	for _, x := range s.points {
		if x.ID == id {
			lp = x
		}
	}
	l.p.mu.Unlock()
	if lp == nil {
		return Result{}, fmt.Errorf("logpoint %d does not exist (see 'logpoint list')", id)
	}
	if err := l.remove(ctx, lp); err != nil {
		return Result{}, err
	}
	return Result{Text: fmt.Sprintf("Removed logpoint %d.", id), Data: map[string]any{"removed": []int{id}}}, nil
}

func runLogpointClear(ctx context.Context, env Env, _ Input) (Result, error) {
	l, err := logpointCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	l.p.mu.Lock()
	points := append([]*Logpoint{}, l.p.pointsLocked(l.ps, l.session).points...)
	l.p.mu.Unlock()
	removed := []int{}
	for _, lp := range points {
		if err := l.remove(ctx, lp); err != nil {
			return Result{}, err
		}
		removed = append(removed, lp.ID)
	}
	if len(points) == 0 {
		// Nothing listed; still end the owner's Debugger state that an
		// earlier failed command may have left.
		if err := l.call(ctx, "Debugger.disable", nil, nil); err != nil {
			return Result{}, err
		}
	}
	return Result{Text: fmt.Sprintf("Removed %d logpoint(s).", len(removed)), Data: map[string]any{"removed": removed}}, nil
}

// remove removes lp in the page and from the session; the last one ends
// the owner's Debugger state (its breakpoint activation and pause
// setting, and Debugger itself when no other owner needs it).
func (l lpCmd) remove(ctx context.Context, lp *Logpoint) error {
	if err := l.call(ctx, "Debugger.removeBreakpoint", map[string]string{"breakpointId": lp.breakpointID}, nil); err != nil {
		return err
	}
	l.p.mu.Lock()
	s := l.p.pointsLocked(l.ps, l.session)
	for i, x := range s.points {
		if x == lp {
			s.points = append(s.points[:i], s.points[i+1:]...)
			break
		}
	}
	empty := len(s.points) == 0
	l.p.mu.Unlock()
	if empty {
		return l.call(ctx, "Debugger.disable", nil, nil)
	}
	return nil
}
