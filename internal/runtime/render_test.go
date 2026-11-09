package runtime

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// The expected texts below follow agent-browser 0.41.2's renderer
// (analysis/sources/agent-browser/snapshot.rs; line numbers cited per case),
// not its output: they were written from the rules, never captured.

func tf(b bool) *bool { return &b }

// designSample is the example of design 3.2.
func designSample() []Node {
	return []Node{
		{Role: "banner", Children: []Node{
			{Role: "navigation", Children: []Node{
				{Role: "link", Name: "Home", Ref: "e3", URL: "/", Children: []Node{{Role: "StaticText", Name: "Home"}}},
			}},
			{Role: "button", Name: "Sign In", Ref: "e6", Children: []Node{{Role: "StaticText", Name: "Sign In"}}},
		}},
		{Role: "main", Children: []Node{
			{Role: "heading", Name: "Welcome", Level: 1, Ref: "e8", Children: []Node{{Role: "StaticText", Name: "Welcome"}}},
			{Role: "textbox", Name: "Email", Required: true, Ref: "e10", Value: "user@example.com"},
			{Role: "checkbox", Name: "Remember me", Checked: "false", Ref: "e12"},
			{Role: "generic", Name: "Open menu", Ref: "e14", Cursor: &Cursor{Kind: "clickable", Hints: []string{"cursor:pointer", "onclick"}, Text: "Open menu"}},
		}},
	}
}

func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name  string
		nodes []Node
		opts  RenderOptions
		want  string
	}{
		{
			// render_tree lines 1222-1352: two spaces per level, JSON name,
			// attribute order, cursor suffix, ": value"; build_tree lines
			// 1183-1190 drop a single StaticText child equal to the name.
			name: "design sample", nodes: designSample(), want: `- banner
  - navigation
    - link "Home" [ref=e3]
  - button "Sign In" [ref=e6]
- main
  - heading "Welcome" [level=1, ref=e8]
  - textbox "Email" [required, ref=e10]: user@example.com
  - checkbox "Remember me" [checked=false, ref=e12]
  - generic "Open menu" [ref=e14] clickable [cursor:pointer, onclick]`,
		},
		{
			// -u adds url= after ref= (lines 1317-1323).
			name: "urls", nodes: designSample(), opts: RenderOptions{URLs: true}, want: `- banner
  - navigation
    - link "Home" [ref=e3, url=/]
  - button "Sign In" [ref=e6]
- main
  - heading "Welcome" [level=1, ref=e8]
  - textbox "Email" [required, ref=e10]: user@example.com
  - checkbox "Remember me" [checked=false, ref=e12]
  - generic "Open menu" [ref=e14] clickable [cursor:pointer, onclick]`,
		},
		{
			// -i: nodes without a ref are skipped and their children keep
			// the indent (lines 1259-1265).
			name: "interactive", nodes: designSample(), opts: RenderOptions{Interactive: true}, want: `- link "Home" [ref=e3]
- button "Sign In" [ref=e6]
- heading "Welcome" [level=1, ref=e8]
- textbox "Email" [required, ref=e10]: user@example.com
- checkbox "Remember me" [checked=false, ref=e12]
- generic "Open menu" [ref=e14] clickable [cursor:pointer, onclick]`,
		},
		{
			// -d N: lines deeper than N are cut (lines 1243-1247).
			name: "depth", nodes: designSample(), opts: RenderOptions{Depth: 1}, want: `- banner
  - navigation
  - button "Sign In" [ref=e6]
- main
  - heading "Welcome" [level=1, ref=e8]
  - textbox "Email" [required, ref=e10]: user@example.com
  - checkbox "Remember me" [checked=false, ref=e12]
  - generic "Open menu" [ref=e14] clickable [cursor:pointer, onclick]`,
		},
		{
			name: "depth zero", nodes: designSample(), opts: RenderOptions{Depth: 0, DepthSet: true}, want: "- banner\n- main",
		},
		{
			// -c keeps lines with refs, values or (without -i) quoted text,
			// and their ancestors (compact_tree, lines 1355-1392); a
			// generic with two children is rendered (line 1233) and then
			// dropped by -c because it holds nothing.
			name: "compact",
			nodes: []Node{{Role: "main", Children: []Node{
				{Role: "paragraph", Children: []Node{{Role: "StaticText", Name: "Hello"}}},
				{Role: "generic", Children: []Node{{Role: "separator"}, {Role: "separator"}}},
				{Role: "list", Children: []Node{{Role: "listitem", Children: []Node{{Role: "link", Name: "a", Ref: "e1"}}}}},
			}}},
			opts: RenderOptions{Compact: true}, want: `- main
  - paragraph
    - StaticText "Hello"
  - list
    - listitem
      - link "a" [ref=e1]`,
		},
		{
			name: "compact without compact",
			nodes: []Node{{Role: "main", Children: []Node{
				{Role: "generic", Children: []Node{{Role: "separator"}, {Role: "separator"}}},
			}}},
			want: "- main\n  - generic\n    - separator\n    - separator",
		},
		{
			// -c -i: quoted text alone no longer keeps a line (line 1365).
			name: "compact interactive",
			nodes: []Node{{Role: "main", Children: []Node{
				{Role: "paragraph", Children: []Node{{Role: "StaticText", Name: "Hello"}}},
				{Role: "button", Name: "Go", Ref: "e2"},
			}}},
			opts: RenderOptions{Compact: true, Interactive: true}, want: `- button "Go" [ref=e2]`,
		},
		{
			// normalize_snapshot_name (lines 170-178): NBSP becomes a space,
			// zero-width characters are removed; the name is a JSON string
			// (quotes, backslashes and control characters escaped).
			name: "name normalization",
			nodes: []Node{
				{Role: "button", Name: "Capital\u00a0Federal\u200b\u200c\u200d\u2060\ufeff", Ref: "e1"},
				{Role: "link", Name: "say \"hi\" \\ <b>&\u0007", Ref: "e2"},
			},
			want: "- button \"Capital Federal\" [ref=e1]\n- link \"say \\\"hi\\\" \\\\ <b>&\\u0007\" [ref=e2]",
		},
		{
			// The short escapes of serde_json (ser.rs ESCAPE table: BB, FF,
			// NN, RR, TT for U+0008, U+000C, U+000A, U+000D, U+0009), and
			// \u00XX in lowercase hex (UU) for the other control characters.
			name: "control character escapes",
			nodes: []Node{
				{Role: "button", Name: "a\bb\fc\nd\re\tf\u001fg", Ref: "e1"},
			},
			want: `- button "a\bb\fc\nd\re\tf\u001fg" [ref=e1]`,
		},
		{
			// Duplicate role and name: both get their own ref (the ref map
			// records the duplicate, lines 497-511); the text shows both.
			name: "duplicates",
			nodes: []Node{
				{Role: "button", Name: "Save", Ref: "e1"},
				{Role: "button", Name: "Save", Ref: "e2"},
			},
			want: "- button \"Save\" [ref=e1]\n- button \"Save\" [ref=e2]",
		},
		{
			// Cursor-interactive element without a name: without -i the
			// suffix follows the attributes; with -i the name falls back to
			// the element text (lines 1267-1276, 1327-1334).
			name: "cursor interactive",
			nodes: []Node{{
				Role: "generic", Ref: "e5", Cursor: &Cursor{Kind: "focusable", Hints: []string{"tabindex"}, Text: "Menu"},
				Children: []Node{{Role: "StaticText", Name: "Menu"}},
			}},
			want: "- generic [ref=e5] focusable [tabindex]\n  - StaticText \"Menu\"",
		},
		{
			name: "cursor interactive -i",
			nodes: []Node{{
				Role: "generic", Ref: "e5", Cursor: &Cursor{Kind: "editable", Hints: []string{"contenteditable"}, Text: "Menu"},
				Children: []Node{{Role: "StaticText", Name: "Menu"}},
			}},
			opts: RenderOptions{Interactive: true},
			want: "- generic \"Menu\" [ref=e5] editable [contenteditable]",
		},
		{
			// All attributes in their fixed order (lines 1289-1325); false
			// selected, disabled and required are not printed; ": value"
			// only when it differs from the name (lines 1336-1341).
			name: "attribute order",
			nodes: []Node{
				{Role: "treeitem", Name: "n", Level: 2, Checked: "mixed", Expanded: tf(true), Selected: true, Disabled: true, Required: true, Ref: "e9", URL: "/x"},
				{Role: "combobox", Name: "Size", Expanded: tf(false), Value: "Size", Ref: "e10"},
				{Role: "option", Name: "M", Selected: false, Disabled: false, Ref: "e11"},
			},
			opts: RenderOptions{URLs: true},
			want: "- treeitem \"n\" [level=2, checked=mixed, expanded=true, selected, disabled, required, ref=e9, url=/x]\n- combobox \"Size\" [expanded=false, ref=e10]\n- option \"M\" [ref=e11]",
		},
		{
			// build_tree lines 1140-1180: consecutive StaticText children are
			// joined; a generic without a ref and with at most one child is
			// not rendered (line 1233); blank text is skipped (line 1234).
			name: "text aggregation",
			nodes: []Node{{Role: "paragraph", Children: []Node{
				{Role: "StaticText", Name: "Hello "},
				{Role: "StaticText", Name: " brave  "},
				{Role: "StaticText", Name: "world"},
				{Role: "generic", Children: []Node{{Role: "StaticText", Name: "inner"}}},
				{Role: "StaticText", Name: "   "},
			}}},
			want: "- paragraph\n  - StaticText \"Hello brave world\"\n  - StaticText \"inner\"",
		},
		{
			name: "iframe content nested", nodes: []Node{{Role: "Iframe", Name: "child", Ref: "e1", Children: []Node{
				{Role: "button", Name: "In frame", Ref: "e2"},
			}}},
			want: "- Iframe \"child\" [ref=e1]\n  - button \"In frame\" [ref=e2]",
		},
		{name: "empty page", nodes: nil, want: "(empty page)"},
		{name: "empty interactive", nodes: []Node{{Role: "paragraph", Children: []Node{{Role: "StaticText", Name: "x"}}}}, opts: RenderOptions{Interactive: true}, want: "(no interactive elements)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Render(c.nodes, c.opts)
			if got != c.want {
				t.Fatalf("Render:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// nameAlphabet favors the characters that the name rules of design 3.2
// treat specially: JSON escapes, NBSP and zero-width characters.
var nameAlphabet = []rune("ab \"\\/\b\f\n\r\t\x00\x01\x1f\x7f\u00a0\u200b\u200c\u200d\u2060\ufeff\u2028é😀<>&")

// documentedName applies design 3.2's name rules: NBSP becomes a space and
// zero-width characters are removed.
var documentedName = strings.NewReplacer("\u00a0", " ", "\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "")

// Design 3.2: a name is rendered as one JSON string. Decoding the rendered
// name with encoding/json gives the name after the documented rules, and
// the line stays one line, whatever control characters the name has.
func TestRenderedNameDecodesAsJSON(t *testing.T) {
	cfg := &quick.Config{MaxCount: 1000, Values: func(v []reflect.Value, r *rand.Rand) {
		rs := make([]rune, 1+r.Intn(12))
		for i := range rs {
			rs[i] = nameAlphabet[r.Intn(len(nameAlphabet))]
		}
		v[0] = reflect.ValueOf(string(rs))
	}}
	f := func(name string) bool {
		got := Render([]Node{{Role: "button", Name: name, Ref: "e1"}}, RenderOptions{})
		q, ok := strings.CutPrefix(got, "- button ")
		if q, ok = strings.CutSuffix(q, " [ref=e1]"); !ok || strings.Contains(got, "\n") {
			t.Logf("name %q rendered as %q", name, got)
			return false
		}
		var decoded string
		if err := json.Unmarshal([]byte(q), &decoded); err != nil || decoded != documentedName.Replace(name) {
			t.Logf("name %q rendered as %s: decoded %q, %v", name, q, decoded, err)
			return false
		}
		return true
	}
	if err := quick.Check(f, cfg); err != nil {
		t.Fatal(err)
	}
}

// The page sends the tree as JSON; the model decodes it unchanged.
func TestNodeJSONModel(t *testing.T) {
	raw := `[{"ref":"e1","role":"checkbox","name":"A","checked":"true","expanded":false,"selected":true,"disabled":true,"required":true,"level":3,"value":"v","url":"/u","cursor":{"kind":"clickable","hints":["onclick"],"text":"t"},"children":[{"role":"StaticText","name":"x"}]}]`
	var nodes []Node
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	if n.Ref != "e1" || n.Checked != "true" || n.Expanded == nil || *n.Expanded || !n.Selected || !n.Disabled || !n.Required ||
		n.Level != 3 || n.Value != "v" || n.URL != "/u" || n.Cursor == nil || n.Cursor.Hints[0] != "onclick" || len(n.Children) != 1 {
		t.Fatalf("decoded %+v", n)
	}
	if !strings.Contains(Render(nodes, RenderOptions{}), "[level=3, checked=true, expanded=false, selected, disabled, required, ref=e1]") {
		t.Fatalf("render of decoded node: %s", Render(nodes, RenderOptions{}))
	}
}
