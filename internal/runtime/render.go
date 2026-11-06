package runtime

import (
	"fmt"
	"strings"
)

// Node is one node of the snapshot tree that the runtime's snapshot
// function returns (design 3.2). Go renders the text, so the format has
// golden tests without a browser.
type Node struct {
	Ref      string  `json:"ref,omitempty"`
	Role     string  `json:"role"`
	Name     string  `json:"name,omitempty"`
	Level    int     `json:"level,omitempty"`
	Checked  string  `json:"checked,omitempty"` // "true", "false" or "mixed"
	Expanded *bool   `json:"expanded,omitempty"`
	Selected bool    `json:"selected,omitempty"`
	Disabled bool    `json:"disabled,omitempty"`
	Required bool    `json:"required,omitempty"`
	Value    string  `json:"value,omitempty"`
	URL      string  `json:"url,omitempty"`
	Cursor   *Cursor `json:"cursor,omitempty"`
	Children []Node  `json:"children,omitempty"`
}

// Cursor marks an element without an interactive role that reacts to the
// pointer or keyboard (cursor:pointer, onclick, tabindex, contenteditable).
type Cursor struct {
	Kind  string   `json:"kind"` // clickable, focusable or editable
	Hints []string `json:"hints"`
	Text  string   `json:"text,omitempty"` // element text, the name fallback of -i
}

// RenderOptions are the snapshot flags that shape the text.
type RenderOptions struct {
	Interactive bool // -i: only nodes with refs
	Compact     bool // -c: lines with refs, values or text, and their ancestors
	Depth       int  // -d: deepest indent level shown; applies when DepthSet or Depth > 0
	DepthSet    bool
	URLs        bool // -u: url= on links
}

// textRole is the role of a run of text, as in Chrome's accessibility tree.
const textRole = "StaticText"

// Render returns the snapshot text in agent-browser's format (renderer of
// agent-browser 0.41.2, snapshot.rs render_tree and compact_tree; behavior
// only, no code copied).
func Render(nodes []Node, o RenderOptions) string {
	nodes = normalizeText(nodes, "")
	var b strings.Builder
	for i := range nodes {
		render(&b, &nodes[i], 0, o)
	}
	out := b.String()
	if o.Compact {
		out = compact(out, o.Interactive)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		if o.Interactive {
			return "(no interactive elements)"
		}
		return "(empty page)"
	}
	return out
}

// normalizeText joins consecutive text runs into one, collapses white
// space, drops blank runs and drops a single text child that repeats its
// parent's name (snapshot.rs build_tree).
func normalizeText(nodes []Node, parentName string) []Node {
	var out []Node
	for _, n := range nodes {
		if n.Role == textRole && len(out) > 0 && out[len(out)-1].Role == textRole {
			out[len(out)-1].Name += n.Name
			continue
		}
		out = append(out, n)
	}
	kept := out[:0]
	for _, n := range out {
		if n.Role == textRole {
			n.Name = strings.Join(strings.Fields(n.Name), " ")
			if normalizeName(n.Name) == "" {
				continue
			}
		}
		n.Children = normalizeText(n.Children, n.Name)
		kept = append(kept, n)
	}
	if len(kept) == 1 && kept[0].Role == textRole && kept[0].Name == parentName {
		return nil
	}
	return kept
}

// normalizeName maps NBSP to a space and removes zero-width characters
// (snapshot.rs normalize_snapshot_name).
func normalizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u00a0':
			return ' '
		case '\ufeff', '\u200b', '\u200c', '\u200d', '\u2060':
			return -1
		}
		return r
	}, s)
}

// quote returns s as a JSON string with the escapes of serde_json: quote,
// backslash, the short control escapes and \u00XX (lowercase) for the other
// control characters; nothing else is escaped.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func render(b *strings.Builder, n *Node, indent int, o RenderOptions) {
	children := func(indent int) {
		for i := range n.Children {
			render(b, &n.Children[i], indent, o)
		}
	}
	// Not rendered: the node's children take its place.
	if n.Role == "" || (n.Role == "generic" && n.Ref == "" && len(n.Children) <= 1) ||
		(n.Role == textRole && strings.TrimSpace(normalizeName(n.Name)) == "") {
		children(indent)
		return
	}
	if (o.DepthSet || o.Depth > 0) && indent > o.Depth {
		return
	}
	if n.Role == "RootWebArea" || n.Role == "WebArea" || (o.Interactive && n.Ref == "") {
		children(indent)
		return
	}
	b.WriteString(strings.Repeat("  ", indent))
	b.WriteString("- ")
	b.WriteString(n.Role)
	name := n.Name
	if name == "" && o.Interactive && n.Cursor != nil {
		name = n.Cursor.Text
	}
	if name != "" {
		b.WriteString(" " + quote(normalizeName(name)))
	}
	var attrs []string
	if n.Level != 0 {
		attrs = append(attrs, fmt.Sprintf("level=%d", n.Level))
	}
	if n.Checked != "" {
		attrs = append(attrs, "checked="+n.Checked)
	}
	if n.Expanded != nil {
		attrs = append(attrs, fmt.Sprintf("expanded=%t", *n.Expanded))
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{n.Selected, "selected"}, {n.Disabled, "disabled"}, {n.Required, "required"}} {
		if f.on {
			attrs = append(attrs, f.name)
		}
	}
	if n.Ref != "" {
		attrs = append(attrs, "ref="+n.Ref)
	}
	if o.URLs && n.URL != "" {
		attrs = append(attrs, "url="+n.URL)
	}
	if len(attrs) > 0 {
		b.WriteString(" [" + strings.Join(attrs, ", ") + "]")
	}
	if n.Cursor != nil {
		b.WriteString(" " + n.Cursor.Kind + " [" + strings.Join(n.Cursor.Hints, ", ") + "]")
	}
	if n.Value != "" && n.Value != n.Name {
		b.WriteString(": " + n.Value)
	}
	b.WriteByte('\n')
	children(indent + 1)
}

// compact keeps the lines with a ref, a value or (without -i) quoted text,
// and the ancestors of each (snapshot.rs compact_tree).
func compact(tree string, interactive bool) string {
	lines := strings.Split(strings.TrimRight(tree, "\n"), "\n")
	keep := make([]bool, len(lines))
	indentOf := func(l string) int { return (len(l) - len(strings.TrimLeft(l, " "))) / 2 }
	for i, l := range lines {
		if !strings.Contains(l, "ref=") && !strings.Contains(l, ": ") && (interactive || !strings.Contains(l, " \"")) {
			continue
		}
		keep[i] = true
		my := indentOf(l)
		for j := i - 1; j >= 0 && my > 0; j-- {
			if a := indentOf(lines[j]); a < my {
				keep[j] = true
				my = a
			}
		}
	}
	var out []string
	for i, l := range lines {
		if keep[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
