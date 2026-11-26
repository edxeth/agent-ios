package heapsnap

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// model is a generated heap snapshot: a class name table (with repeated
// names, as a hostile file can have, and names with quotes, controls and
// non-ASCII) and nodes after the root.
type model struct {
	version int
	// untyped omits the type member, as version 1 did before WebKit
	// 6a4a1033 (2018) added it (HeapSnapshotBuilder.cpp at 0be502df).
	untyped bool
	names   []string
	nodes   []modelNode
}

type modelNode struct {
	id, size uint64
	class    int
	flags    uint64
}

var nameAtoms = []string{"Object", "Function", "string", "Structure", "Widget", "a\"b", "line\nbreak", "é", "日本", "", "<root>"}

func (model) Generate(r *rand.Rand, size int) reflect.Value {
	m := model{version: 1 + r.Intn(3), names: []string{"<root>"}}
	m.untyped = m.version == 1 && r.Intn(2) == 0
	for range 1 + r.Intn(6) {
		m.names = append(m.names, nameAtoms[r.Intn(len(nameAtoms))])
	}
	n := r.Intn(size + 1)
	if r.Intn(8) == 0 {
		n = 0 // only the root
	}
	for i := range n {
		var s uint64
		switch r.Intn(4) {
		case 0:
			s = 0
		case 1:
			s = uint64(r.Int63n(1 << 40)) // large sizes
		default:
			s = uint64(r.Intn(200))
		}
		m.nodes = append(m.nodes, modelNode{id: uint64(i + 1), size: s, class: r.Intn(len(m.names)), flags: uint64(r.Intn(8))})
	}
	return reflect.ValueOf(m)
}

// encode writes m as HeapSnapshotBuilder does (Inspector type): the root
// 0,0,0,0, then id, size, class index and flags of each node, with edges
// from the root to each node.
func (m model) encode() []byte {
	var b strings.Builder
	typ := `"type":"Inspector",`
	if m.untyped {
		typ = ""
	}
	fmt.Fprintf(&b, `{"version":%d,%s"nodes":[0,0,0,0`, m.version, typ)
	for _, n := range m.nodes {
		fmt.Fprintf(&b, ",%d,%d,%d,%d", n.id, n.size, n.class, n.flags)
	}
	names, _ := json.Marshal(m.names)
	fmt.Fprintf(&b, `],"nodeClassNames":%s,"edges":[`, names)
	for i, n := range m.nodes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "0,%d,0,0", n.id)
	}
	b.WriteString(`],"edgeTypes":["Internal","Property","Index","Variable"],"edgeNames":[]}`)
	return []byte(b.String())
}

// oracle sums the model by class name, as Web Inspector's class summary
// does (HeapSnapshot.js updateCategoriesAndMetadata).
func (m model) oracle() (map[string]Class, int64) {
	by := map[string]Class{}
	var total int64
	for _, n := range m.nodes {
		name := m.names[n.class]
		c := by[name]
		c.Name, c.Count, c.Size = name, c.Count+1, c.Size+int64(n.size)
		by[name] = c
		total += int64(n.size)
	}
	return by, total
}

// Parse of every generated snapshot of versions 1 to 3 gives the model's
// node count (without the root), total self size, and per class name the
// count and self size of its nodes, each class once, sorted by name.
func TestParseSummaryProperty(t *testing.T) {
	prop := func(m model) bool {
		s, err := Parse(m.encode())
		if err != nil {
			t.Errorf("Parse: %v", err)
			return false
		}
		by, total := m.oracle()
		wantType := "Inspector"
		if m.untyped {
			wantType = ""
		}
		if s.Type != wantType {
			t.Errorf("type %q, want %q", s.Type, wantType)
			return false
		}
		if s.Version != m.version || s.Nodes != int64(len(m.nodes)) || s.TotalSize != total || len(s.Classes) != len(by) {
			t.Errorf("got version %d, %d nodes, %d bytes, %d classes; model %d, %d, %d, %d", s.Version, s.Nodes, s.TotalSize, len(s.Classes), m.version, len(m.nodes), total, len(by))
			return false
		}
		for i, c := range s.Classes {
			if by[c.Name] != c || (i > 0 && s.Classes[i-1].Name >= c.Name) {
				t.Errorf("class %d %+v; model %+v (or not sorted by name)", i, c, by[c.Name])
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}

// TopByCount(k) and TopBySize(k) return min(k, classes) classes, ordered
// by their key (count, else size, else name; size, else count, else name),
// and no class left out ranks before the last one returned.
func TestTopProperty(t *testing.T) {
	byCount := func(a, b Class) bool {
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		return a.Name < b.Name
	}
	bySize := func(a, b Class) bool {
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name < b.Name
	}
	prop := func(m model, k8 uint8) bool {
		s, err := Parse(m.encode())
		if err != nil {
			t.Errorf("Parse: %v", err)
			return false
		}
		k := int(k8 % 10)
		for _, c := range []struct {
			name   string
			top    []Class
			before func(a, b Class) bool
		}{{"TopByCount", s.TopByCount(k), byCount}, {"TopBySize", s.TopBySize(k), bySize}} {
			if len(c.top) != min(k, len(s.Classes)) {
				t.Errorf("%s(%d): %d classes of %d", c.name, k, len(c.top), len(s.Classes))
				return false
			}
			in := map[string]bool{}
			for i, x := range c.top {
				in[x.Name] = true
				if i > 0 && !c.before(c.top[i-1], x) {
					t.Errorf("%s(%d): %+v before %+v", c.name, k, c.top[i-1], x)
					return false
				}
			}
			for _, x := range s.Classes {
				if !in[x.Name] && len(c.top) > 0 && c.before(x, c.top[len(c.top)-1]) {
					t.Errorf("%s(%d): left out %+v, which ranks before %+v", c.name, k, x, c.top[len(c.top)-1])
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}

// Diff(a, b) lists exactly the class names whose count or size differs
// between the models, each with both sides and b minus a; so its deltas
// sum to the change of the node count and the total size. Diff(a, a) is
// empty.
func TestDiffProperty(t *testing.T) {
	prop := func(ma, mb model) bool {
		a, errA := Parse(ma.encode())
		b, errB := Parse(mb.encode())
		if errA != nil || errB != nil {
			t.Errorf("Parse: %v, %v", errA, errB)
			return false
		}
		if d := Diff(a, a); len(d) != 0 {
			t.Errorf("Diff(a, a) = %+v", d)
			return false
		}
		oa, _ := ma.oracle()
		ob, _ := mb.oracle()
		want := map[string]bool{}
		for _, o := range []map[string]Class{oa, ob} {
			for name := range o {
				if oa[name].Count != ob[name].Count || oa[name].Size != ob[name].Size {
					want[name] = true
				}
			}
		}
		d := Diff(a, b)
		var nodes, size int64
		for i, x := range d {
			ca, cb := oa[x.Name], ob[x.Name]
			if !want[x.Name] || x.CountA != ca.Count || x.CountB != cb.Count || x.SizeA != ca.Size || x.SizeB != cb.Size ||
				x.CountDelta != cb.Count-ca.Count || x.SizeDelta != cb.Size-ca.Size {
				t.Errorf("delta %+v; models %+v -> %+v", x, ca, cb)
				return false
			}
			if i > 0 && abs(d[i-1].SizeDelta) < abs(x.SizeDelta) {
				t.Errorf("not ordered by size change: %+v before %+v", d[i-1], x)
				return false
			}
			nodes += x.CountDelta
			size += x.SizeDelta
		}
		if len(d) != len(want) || nodes != b.Nodes-a.Nodes || size != b.TotalSize-a.TotalSize {
			t.Errorf("%d deltas (want %d), sums %d nodes %d bytes; totals change %d and %d", len(d), len(want), nodes, size, b.Nodes-a.Nodes, b.TotalSize-a.TotalSize)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}

// A node that names a class beyond the class name table makes Parse fail,
// for every generated snapshot with at least one node.
func TestParseRejectsBadClassIndexProperty(t *testing.T) {
	prop := func(m model, pick uint16) bool {
		if len(m.nodes) == 0 {
			return true
		}
		m.nodes[int(pick)%len(m.nodes)].class = len(m.names) + int(pick%3)
		_, err := Parse(m.encode())
		if err == nil || !strings.Contains(err.Error(), "class names") {
			t.Errorf("Parse with a bad class index: %v", err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}
