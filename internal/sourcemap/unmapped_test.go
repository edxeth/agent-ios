package sourcemap

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// FIX-D F6 (ecma426 "Mappings structure": a segment of one field maps a
// generated position to no original one): a one-field segment ends the
// mapping before it, so Original finds no original position from its
// column on, until the next segment with an original position.
func TestOneFieldSegmentEndsTheMapping(t *testing.T) {
	// gen 0:0 <- a.ts 0:0; gen 0:5 unmapped; gen 0:7 <- a.ts 0:2
	m, err := Parse([]byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA,K,EAAE"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		col    int
		ok     bool
		origCo int
	}{{0, true, 0}, {4, true, 0}, {5, false, 0}, {6, false, 0}, {7, true, 2}, {30, true, 2}} {
		got, ok := m.Original(0, c.col)
		if ok != c.ok || ok && got.OrigCol != c.origCo {
			t.Errorf("Original(0, %d) = %+v, %v; want ok %v, original column %d", c.col, got, ok, c.ok, c.origCo)
		}
	}
	// A one-field segment at the column of a mapped one comes later in the
	// line, so it wins.
	m, err = Parse([]byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA,A"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Original(0, 0); ok {
		t.Fatalf("Original(0, 0) after a later one-field segment at column 0 = %+v", got)
	}
	// Generated never returns an unmapped position.
	if g, ok := m.Generated(0, 0, 0); ok {
		t.Fatalf("Generated found %+v in a map whose only mapping a later segment ended", g)
	}
}

// seg is a generated segment for the property: mapped carries an original
// position, otherwise it has one field.
type seg struct {
	line, col int
	mapped    bool
	m         Mapping
}

// genSegs is a random segment list in mappings order (by line, then column;
// columns may repeat within a line).
type genSegs []seg

func (genSegs) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(size + 1)
	out := make([]seg, 0, n)
	for range n {
		s := seg{line: r.Intn(5), col: r.Intn(25), mapped: r.Intn(3) != 0}
		if s.mapped {
			s.m = Mapping{GenLine: s.line, GenCol: s.col, Source: r.Intn(2), OrigLine: r.Intn(6), OrigCol: r.Intn(20)}
		}
		out = append(out, s)
	}
	// Mappings order: by line, then column; equal positions keep their
	// random order (the later one wins).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].line < out[j-1].line || out[j].line == out[j-1].line && out[j].col < out[j-1].col); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return reflect.ValueOf(genSegs(out))
}

// encode writes the segments as a mappings string, one-field segments
// included (the encoder of the format's definition).
func (g genSegs) encode() string {
	var b strings.Builder
	var src, oline, ocol int
	line, col, first := 0, 0, true
	for _, s := range g {
		for line < s.line {
			b.WriteByte(';')
			line++
			col, first = 0, true
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(EncodeVLQ(s.col - col))
		col = s.col
		if s.mapped {
			for _, v := range []int{s.m.Source - src, s.m.OrigLine - oline, s.m.OrigCol - ocol} {
				b.WriteString(EncodeVLQ(v))
			}
			src, oline, ocol = s.m.Source, s.m.OrigLine, s.m.OrigCol
		}
	}
	return b.String()
}

// Property (reference model of ecma426 reverse lookup): Original of a
// generated position is the original position of the last segment of
// that line at or before the column, and nothing when that segment has
// one field or there is none; Generated returns only positions whose
// Original gives the same mapping back.
func TestOriginalMatchesTheSegmentModelProperty(t *testing.T) {
	f := func(g genSegs, line, col uint8) bool {
		m, err := Parse([]byte(`{"version":3,"sources":["a","b"],"mappings":"` + g.encode() + `"}`))
		if err != nil {
			return false
		}
		l, c := int(line%6), int(col%30)
		var want *seg
		for i := range g {
			if g[i].line == l && g[i].col <= c {
				want = &g[i]
			}
		}
		got, ok := m.Original(l, c)
		switch {
		case want == nil || !want.mapped:
			if ok {
				return false
			}
		case !ok || got != want.m:
			return false
		}
		for s := range 2 {
			if x, ok := m.Generated(s, l, c); ok {
				if back, ok := m.Original(x.GenLine, x.GenCol); !ok || back != x {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatal(err)
	}
}

// FIX-D F13: a map larger than the byte budget or with more segments than
// the segment budget is refused with an error that says so, before its
// mappings are kept.
func TestParseBudgets(t *testing.T) {
	data := []byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA,CAAC,CAAC,K"}`)
	if _, err := ParseWithLimits(data, Limits{Bytes: len(data) - 1, Segments: 100}); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a map over the byte budget: %v; want ErrTooLarge naming bytes", err)
	}
	if _, err := ParseWithLimits(data, Limits{Bytes: len(data), Segments: 3}); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "segments") {
		t.Fatalf("a map over the segment budget: %v; want ErrTooLarge naming segments", err)
	}
	m, err := ParseWithLimits(data, Limits{Bytes: len(data), Segments: 4})
	if err != nil || len(m.Mappings) != 3 {
		t.Fatalf("a map within its budgets: %v, %v", m, err)
	}
	if m.Segments() != 4 {
		t.Fatalf("Segments() = %d, want 4", m.Segments())
	}
}
