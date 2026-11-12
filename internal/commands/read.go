package commands

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// read: the readable text of the current page, or of a URL fetched over
// HTTP on the host without a device (design
// docs/design/agentic-parity.md, section 4.2 row read; agent-browser
// "read"). --outline prints the headings as "#" lines.
func init() {
	Register(&Command{
		Name:    "read",
		Summary: "Print the visible text of the current page, or of an http(s) URL fetched on this computer; --outline prints only the headings.",
		Args: []Arg{
			{Name: "url", Help: "http or https URL to fetch on this computer instead of reading the current page"},
		},
		Flags: []Flag{
			{Name: "outline", Type: FlagBool, Help: "print only the headings, one \"#\" per level"},
		},
		Page:      LeasedPage, // only without a URL
		RetrySafe: true,
		Output:    OutputText,
		Run:       runRead,
	})
}

// maxReadBytes caps the body that read <url> reads from the server.
const maxReadBytes = 10 << 20

func runRead(ctx context.Context, env Env, in Input) (Result, error) {
	outline := flagBool(in, "outline")
	if u := arg(in, 0); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return Result{}, usagef("read %q: want an http:// or https:// URL", u)
		}
		text, err := readURL(ctx, u, outline)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: text, Data: map[string]any{"text": text, "url": u}}, nil
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	var text string
	warnings, err := p.rt.CallWarn(ctx, "read", map[string]any{"outline": outline}, &text)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: withWarnings(text, warnings), Data: map[string]any{"text": text, "warnings": warnings}}, nil
}

// readURL fetches u and returns its readable text or heading outline.
func readURL(ctx context.Context, u string, outline bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", u, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("read %s: the server answered HTTP %s", u, resp.Status)
	}
	body := io.LimitReader(resp.Body, maxReadBytes)
	ct := resp.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	switch {
	case ct == "" || (err == nil && (mt == "text/html" || mt == "application/xhtml+xml")):
		doc, err := html.Parse(body)
		if err != nil {
			return "", fmt.Errorf("read %s: parsing HTML: %w", u, err)
		}
		if outline {
			return htmlOutline(doc), nil
		}
		return htmlText(doc), nil
	case err == nil && strings.HasPrefix(mt, "text/"):
		if outline {
			return "", nil
		}
		data, err := io.ReadAll(body)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", u, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", fmt.Errorf("read %s: the content type is %q, not HTML or text", u, ct)
}

// skipped elements have no readable text.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Iframe: true, atom.Object: true, atom.Select: true,
}

// Line breaks around block elements: paragraph-like blocks get a blank
// line, the others a line break, as innerText does.
var (
	paragraphBlocks = map[atom.Atom]bool{
		atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
		atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Blockquote: true, atom.Pre: true, atom.Table: true,
		atom.Figure: true, atom.Hr: true,
	}
	lineBlocks = map[atom.Atom]bool{
		atom.Div: true, atom.Li: true, atom.Tr: true, atom.Dt: true, atom.Dd: true, atom.Br: true,
		atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true, atom.Nav: true,
		atom.Main: true, atom.Aside: true, atom.Form: true, atom.Fieldset: true, atom.Address: true,
		atom.Details: true, atom.Summary: true, atom.Figcaption: true, atom.Caption: true, atom.Option: true,
	}
	headingLevel = map[atom.Atom]int{atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6}
	manyBreaks   = regexp.MustCompile(`\n{3,}`)
)

// hiddenNode: an element the reader skips with its subtree.
func hiddenNode(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if skipped[n.DataAtom] {
		return true
	}
	for _, a := range n.Attr {
		if a.Namespace == "" && (a.Key == "hidden" || (a.Key == "aria-hidden" && strings.EqualFold(a.Val, "true"))) {
			return true
		}
	}
	return false
}

// htmlText returns the text of doc: white space collapsed, block elements
// on their own lines, at most one blank line in a row.
func htmlText(doc *html.Node) string {
	var b strings.Builder
	// pending is the number of line breaks owed before the next text:
	// adjacent block edges merge to the larger break instead of adding up.
	pending := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if hiddenNode(n) {
			return
		}
		if n.Type == html.TextNode {
			if strings.TrimSpace(n.Data) == "" && pending > 0 {
				return // white space between blocks
			}
			if pending > 0 && b.Len() > 0 {
				b.WriteString(strings.Repeat("\n", pending))
			}
			pending = 0
			b.WriteString(n.Data)
			return
		}
		brk := 0
		switch {
		case n.Type != html.ElementNode:
		case paragraphBlocks[n.DataAtom]:
			brk = 2
		case lineBlocks[n.DataAtom]:
			brk = 1
		}
		pending = max(pending, brk)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		pending = max(pending, brk)
	}
	walk(doc)
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ") // also maps NBSP to a space
	}
	return strings.TrimSpace(manyBreaks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// htmlOutline returns the headings of doc, one "#" per level.
func htmlOutline(doc *html.Node) string {
	var out []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if hiddenNode(n) {
			return
		}
		if lvl := headingLevel[n.DataAtom]; lvl > 0 && n.Type == html.ElementNode {
			if t := strings.Join(strings.Fields(htmlText(n)), " "); t != "" {
				out = append(out, strings.Repeat("#", lvl)+" "+t)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.Join(out, "\n")
}
