package heapsnap

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/webkitgtk-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A real WebKitGTK snapshot (version 3, trimmed; testdata/README.md)
// summarizes as Web Inspector's class summary does: the root skipped,
// count and self size by class name. The expected values come from that
// algorithm run in Python on the same file.
func TestParseWebKitGTKSnapshot(t *testing.T) {
	s, err := Parse(readFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 3 || s.Type != "Inspector" || s.Nodes != 260 || s.TotalSize != 33496 || len(s.Classes) != 71 {
		t.Fatalf("got version %d type %q, %d nodes, %d bytes, %d classes; want 3 Inspector, 260 nodes, 33496 bytes, 71 classes",
			s.Version, s.Type, s.Nodes, s.TotalSize, len(s.Classes))
	}
	wantCount := []Class{
		{Name: "HeapProbe045", Count: 40, Size: 1280},
		{Name: "FunctionCodeBlock", Count: 5, Size: 8852},
		{Name: "UnlinkedFunctionCodeBlock", Count: 5, Size: 4446},
		{Name: "FunctionExecutable", Count: 5, Size: 640},
		{Name: "Structure", Count: 5, Size: 560},
	}
	if got := s.TopByCount(5); !reflect.DeepEqual(got, wantCount) {
		t.Fatalf("TopByCount(5) = %+v\nwant %+v", got, wantCount)
	}
	wantSize := []Class{
		{Name: "FunctionCodeBlock", Count: 5, Size: 8852},
		{Name: "Window", Count: 4, Size: 8375},
		{Name: "UnlinkedFunctionCodeBlock", Count: 5, Size: 4446},
		{Name: "HeapProbe045", Count: 40, Size: 1280},
		{Name: "UnlinkedProgramCodeBlock", Count: 2, Size: 1121},
	}
	if got := s.TopBySize(5); !reflect.DeepEqual(got, wantSize) {
		t.Fatalf("TopBySize(5) = %+v\nwant %+v", got, wantSize)
	}
}

// Versions 1 and 2 have the layout of version 3 (HeapSnapshotBuilder.cpp
// at WebKit-7615.3.12.11.2 writes version 2 with the same members; Web
// Inspector's HeapSnapshot.js reads 1 to 3 alike), so the same file read
// as version 1 or 2 gives the same summary. Any other version, a
// GCDebugging snapshot and a file that is not a snapshot fail and say why.
func TestParseVersions(t *testing.T) {
	data := readFixture(t)
	want, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "2"} {
		s, err := Parse(bytes.Replace(data, []byte(`"version":3`), []byte(`"version":`+v), 1))
		if err != nil {
			t.Fatalf("version %s: %v", v, err)
		}
		want.Version = s.Version
		if !reflect.DeepEqual(s, want) {
			t.Fatalf("version %s: summary differs from version 3", v)
		}
	}
	for _, c := range []struct{ in, want string }{
		{`{"version":4,"type":"Inspector","nodes":[0,0,0,0],"nodeClassNames":["<root>"]}`, "version 4 is not supported"},
		{`{"version":0,"nodes":[0,0,0,0],"nodeClassNames":["<root>"]}`, "version 0 is not supported"},
		{`{"type":"Inspector","nodes":[0,0,0,0],"nodeClassNames":["<root>"]}`, "no version member"},
		{`{"version":3,"type":"GCDebugging","nodes":[0,0,0,0,0,"0x0","0x0"],"nodeClassNames":["<root>"]}`, "GCDebugging is not supported"},
		{`{"version":3,"type":"Other","nodes":[0,0,0,0],"nodeClassNames":["<root>"]}`, `type "Other" is not supported`},
		{`{"version":3,"type":"Inspector","nodes":[],"nodeClassNames":[]}`, "the node list has 0 values"},
		{`{"version":3,"type":"Inspector","nodes":[0,0,0,0,1,8,0],"nodeClassNames":["<root>"]}`, "the node list has 7 values"},
		{`{"version":3,"type":"Inspector","nodes":[0,0,0,0,1,8,1,0],"nodeClassNames":["<root>"]}`, "node 1 names class 1, but there are 1 class names"},
		{`{"version":3,"type":"Inspector","nodes":[0,0,0,0,1,-8,0,0],"nodeClassNames":["<root>"]}`, "reading the heap snapshot"},
		{`{"version":3,"type":"Inspector","nodes":[0,0,0,0,1,9223372036854775807,0,0,2,1,0,0],"nodeClassNames":["<root>"]}`, "the total size exceeds"},
		{`[]`, "reading the heap snapshot"},
	} {
		if _, err := Parse([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%s) = %v; want an error with %q", c.in, err, c.want)
		}
	}
}

// ShortName bounds a page's class name to MaxNameBytes bytes, on a UTF-8
// boundary, and marks the cut; shorter names stay as they are.
func TestShortName(t *testing.T) {
	if got := ShortName("Widget"); got != "Widget" {
		t.Fatalf("ShortName(Widget) = %q", got)
	}
	long := strings.Repeat("a", MaxNameBytes-1) + "é" + "tail"
	got := ShortName(long)
	if got != strings.Repeat("a", MaxNameBytes-1)+"…" {
		t.Fatalf("ShortName cut %q", got)
	}
}
