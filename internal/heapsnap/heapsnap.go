// Package heapsnap reads JavaScriptCore heap snapshots, the JSON that the
// WebKit Inspector Protocol's Heap.snapshot returns, and summarizes them by
// class on the host.
//
// The format is JavaScriptCore's HeapSnapshotBuilder JSON
// (Source/JavaScriptCore/heap/HeapSnapshotBuilder.cpp, its header comment;
// Web Inspector reads it in
// Source/WebInspectorUI/UserInterface/Workers/HeapSnapshot/HeapSnapshot.js):
//
//	{"version":3,"type":"Inspector",
//	 "nodes":[<id>,<sizeInBytes>,<classNameIndex>,<flags>, ...],
//	 "nodeClassNames":["<root>", ...],
//	 "edges":[<fromId>,<toId>,<edgeTypeIndex>,<extra>, ...],
//	 "edgeTypes":[...],"edgeNames":[...]}
//
// The first node is the <root> (0,0,0,0), which Web Inspector skips in its
// class summary; so does Summary. Versions 1 to 3 share this layout: in
// version 1 the fourth field is 0 or 1 (internal), version 2 made it a bit
// mask, and version 3 added the element bit (HeapSnapshot.js, "Version
// Differences"). Safari 16.6 (WebKit 7615) writes version 2, current
// WebKit version 3. GCDebugging snapshots (7 fields per node) come from
// JSC's own debugging tools, never from Heap.snapshot, and are refused.
package heapsnap

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

// nodeFields is the number of values per node of an Inspector snapshot.
const nodeFields = 4

// MaxNameBytes bounds a class name in a summary: names come from page
// script (constructor names), so a page can make them arbitrarily long.
const MaxNameBytes = 256

// Class is the count and the total self size (JavaScriptCore's
// estimatedSizeInBytes, no retained size) of the nodes of one class.
type Class struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Size  int64  `json:"size"`
}

// Snapshot is the class summary of one heap snapshot.
type Snapshot struct {
	Version   int    // format version (1 to 3)
	Type      string // "Inspector" (or empty in version 1)
	Nodes     int64  // nodes without the <root>
	TotalSize int64  // sum of the self sizes of all nodes, in bytes
	// Classes holds every class with at least one node, sorted by name.
	Classes []Class
}

// header is the version and the type, read first: the node list of
// another type need not be numbers (GCDebugging has address strings).
type header struct {
	Version *int   `json:"version"`
	Type    string `json:"type"`
}

// Parse reads a JavaScriptCore heap snapshot and summarizes it by class.
// It fails on a version or type that it does not know, and on a node list
// that does not have the format's shape.
//
// Parse reads data in place and allocates little beyond the class table:
// the node list, the bulk of a snapshot, is summed while it is scanned and
// never decoded into a slice, and the edges are only skipped (a summary by
// class needs no graph). The members "nodes" and "nodeClassNames" are
// matched exactly, as JavaScriptCore writes them; a repeated member counts
// with its last value, as in encoding/json.
func Parse(data []byte) (Snapshot, error) {
	var h header
	// The header decode also checks that data is one JSON value: the scans
	// below rely on that.
	if err := json.Unmarshal(data, &h); err != nil {
		return Snapshot{}, fmt.Errorf("reading the heap snapshot: %w", err)
	}
	if h.Version == nil {
		return Snapshot{}, fmt.Errorf("reading the heap snapshot: no version member; this is not a JavaScriptCore heap snapshot")
	}
	if v := *h.Version; v < 1 || v > 3 {
		return Snapshot{}, fmt.Errorf("heap snapshot version %d is not supported: agent-ios reads versions 1 to 3 of JavaScriptCore's heap snapshot format", v)
	}
	switch h.Type {
	case "Inspector", "":
	case "GCDebugging":
		return Snapshot{}, fmt.Errorf("heap snapshot type GCDebugging is not supported: agent-ios reads Inspector snapshots (Heap.snapshot)")
	default:
		return Snapshot{}, fmt.Errorf("heap snapshot type %q is not supported: agent-ios reads Inspector snapshots", h.Type)
	}
	// The header decoded into a struct, so data is an object.
	nodes, namesRaw := members(data)
	var names []string
	if namesRaw != nil {
		if err := json.Unmarshal(namesRaw, &names); err != nil {
			return Snapshot{}, fmt.Errorf("reading the heap snapshot: nodeClassNames: %w", err)
		}
	}
	s := Snapshot{Version: *h.Version, Type: h.Type}
	type acc struct{ count, size int64 }
	byClass := make([]acc, len(names))
	var total uint64
	// The first error of a node in file order; a malformed value or a
	// node list of the wrong length is reported before it.
	var nodeErr error
	n, err := scanNodes(nodes, func(i int, size, class uint64) {
		if nodeErr != nil {
			return
		}
		if class >= uint64(len(names)) {
			nodeErr = fmt.Errorf("heap snapshot: node %d names class %d, but there are %d class names", i, class, len(names))
			return
		}
		if size > math.MaxInt64-total {
			nodeErr = fmt.Errorf("heap snapshot: the total size exceeds %d bytes at node %d", int64(math.MaxInt64), i)
			return
		}
		total += size
		byClass[class].count++
		byClass[class].size += int64(size)
		s.Nodes++
	})
	if err != nil {
		return Snapshot{}, err
	}
	if n < nodeFields || n%nodeFields != 0 {
		return Snapshot{}, fmt.Errorf("heap snapshot: the node list has %d values; want the <root> node and %d values per node", n, nodeFields)
	}
	if nodeErr != nil {
		return Snapshot{}, nodeErr
	}
	s.TotalSize = int64(total)
	byName := make(map[string]*acc)
	for class, a := range byClass {
		if a.count == 0 {
			continue
		}
		name := names[class]
		if b := byName[name]; b != nil {
			b.count += a.count
			b.size += a.size
			continue
		}
		byName[name] = &acc{a.count, a.size}
	}
	s.Classes = make([]Class, 0, len(byName))
	for name, a := range byName {
		s.Classes = append(s.Classes, Class{Name: name, Count: a.count, Size: a.size})
	}
	slices.SortFunc(s.Classes, func(a, b Class) int { return cmp.Compare(a.Name, b.Name) })
	return s, nil
}

// TopByCount returns at most n classes with the most nodes; ties go to the
// larger size, then to the name.
func (s Snapshot) TopByCount(n int) []Class {
	return top(s.Classes, n, func(a, b Class) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(b.Size, a.Size), cmp.Compare(a.Name, b.Name))
	})
}

// TopBySize returns at most n classes with the largest total self size;
// ties go to the larger count, then to the name.
func (s Snapshot) TopBySize(n int) []Class {
	return top(s.Classes, n, func(a, b Class) int {
		return cmp.Or(cmp.Compare(b.Size, a.Size), cmp.Compare(b.Count, a.Count), cmp.Compare(a.Name, b.Name))
	})
}

func top(cs []Class, n int, order func(a, b Class) int) []Class {
	out := slices.Clone(cs)
	slices.SortFunc(out, order)
	if n >= 0 && n < len(out) {
		out = out[:n]
	}
	return out
}

// ClassDelta is the change of one class from snapshot a to snapshot b.
type ClassDelta struct {
	Name       string `json:"name"`
	CountA     int64  `json:"countA"`
	CountB     int64  `json:"countB"`
	CountDelta int64  `json:"countDelta"`
	SizeA      int64  `json:"sizeA"`
	SizeB      int64  `json:"sizeB"`
	SizeDelta  int64  `json:"sizeDelta"`
}

// Diff returns the classes whose count or size differs between a and b,
// the largest size change (in either direction) first; ties go to the
// larger count change, then to the name. Classes match by name.
func Diff(a, b Snapshot) []ClassDelta {
	m := make(map[string]*ClassDelta)
	get := func(name string) *ClassDelta {
		d := m[name]
		if d == nil {
			d = &ClassDelta{Name: name}
			m[name] = d
		}
		return d
	}
	for _, c := range a.Classes {
		d := get(c.Name)
		d.CountA, d.SizeA = c.Count, c.Size
	}
	for _, c := range b.Classes {
		d := get(c.Name)
		d.CountB, d.SizeB = c.Count, c.Size
	}
	var out []ClassDelta
	for _, d := range m {
		d.CountDelta, d.SizeDelta = d.CountB-d.CountA, d.SizeB-d.SizeA
		if d.CountDelta != 0 || d.SizeDelta != 0 {
			out = append(out, *d)
		}
	}
	slices.SortFunc(out, func(x, y ClassDelta) int {
		return cmp.Or(cmp.Compare(abs(y.SizeDelta), abs(x.SizeDelta)), cmp.Compare(abs(y.CountDelta), abs(x.CountDelta)), cmp.Compare(x.Name, y.Name))
	})
	return out
}

// abs is |v|; sizes and counts are below 2^63, so their differences are
// above math.MinInt64.
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ShortName bounds a class name for output: at most MaxNameBytes bytes,
// cut on a UTF-8 boundary, with "…" when it was cut.
func ShortName(name string) string {
	if len(name) <= MaxNameBytes {
		return name
	}
	cut := MaxNameBytes
	for cut > 0 && name[cut]&0xC0 == 0x80 {
		cut--
	}
	return name[:cut] + "…"
}
