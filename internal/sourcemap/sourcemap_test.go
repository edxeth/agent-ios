package sourcemap

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"testing/quick"
)

// Known Base64 VLQ values from the format's definition (ecma426, section
// "Base64 VLQ"): the sign is the lowest bit, 5 data bits per digit.
func TestDecodeVLQKnownValues(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{{"A", 0}, {"C", 1}, {"D", -1}, {"gB", 16}, {"hB", -16}, {"2H", 123}, {"+/////D", 1<<31 - 1}, {"//////D", -(1<<31 - 1)}} {
		v, n, err := DecodeVLQ(c.s)
		if err != nil || v != c.want || n != len(c.s) {
			t.Errorf("DecodeVLQ(%q) = %d, %d, %v; want %d, %d", c.s, v, n, err, c.want, len(c.s))
		}
	}
	for _, bad := range []string{"", "g", "!", "gggggggggB"} {
		if _, _, err := DecodeVLQ(bad); err == nil {
			t.Errorf("DecodeVLQ(%q) did not fail", bad)
		}
	}
}

// Property: every value in the int32 range survives an encode and decode,
// and the decoder consumes exactly the encoded digits.
func TestVLQRoundTripProperty(t *testing.T) {
	f := func(v int32, tail uint8) bool {
		s := EncodeVLQ(int(v))
		suffix := EncodeVLQ(int(tail)) // a following value must not be read
		got, n, err := DecodeVLQ(s + suffix)
		return err == nil && got == int(v) && n == len(s)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatal(err)
	}
}

// genMappings is a random mapping list ordered by generated position,
// with distinct generated positions.
type genMappings []Mapping

func (genMappings) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(size + 1)
	seen := map[[2]int]bool{}
	var ms []Mapping
	for len(ms) < n {
		x := Mapping{GenLine: r.Intn(6), GenCol: r.Intn(40), Source: r.Intn(3), OrigLine: r.Intn(8), OrigCol: r.Intn(30)}
		k := [2]int{x.GenLine, x.GenCol}
		if seen[k] {
			continue
		}
		seen[k] = true
		ms = append(ms, x)
	}
	sort.Slice(ms, func(i, j int) bool { return genBefore(ms[i], ms[j]) })
	return reflect.ValueOf(genMappings(ms))
}

// Property: encoding mappings and decoding them gives them back.
func TestMappingsRoundTripProperty(t *testing.T) {
	f := func(g genMappings) bool {
		got, err := DecodeMappings(EncodeMappings(g))
		if err != nil {
			return false
		}
		return len(got) == len(g) && (len(g) == 0 || reflect.DeepEqual([]Mapping(g), got))
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// Property (the logpoint contract "a line without code moves to the next
// possible location"): Generated returns a mapping of the source at or
// after the asked position, and no mapping of that source lies between the
// asked position and the returned one; it finds nothing only when no
// mapping of the source is at or after the position. Original of the
// returned generated position gives the same segment back.
func TestGeneratedLookupProperty(t *testing.T) {
	after := func(x Mapping, line, col int) bool {
		return x.OrigLine > line || x.OrigLine == line && x.OrigCol >= col
	}
	f := func(g genMappings, src, line, col uint8) bool {
		m := &Map{Sources: []string{"a", "b", "c"}, Mappings: g}
		s, l, c := int(src%3), int(line%9), int(col%32)
		got, ok := m.Generated(s, l, c)
		for _, x := range g {
			if x.Source != s || !after(x, l, c) {
				continue
			}
			if !ok || origBefore(x, got) {
				return false // a mapping closer to the asked position was skipped
			}
		}
		if !ok {
			return true
		}
		if got.Source != s || !after(got, l, c) {
			return false
		}
		back, ok := m.Original(got.GenLine, got.GenCol)
		return ok && back == got
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
}

// A map as esbuild writes it for two original lines: the original line
// without code (1) moves to line 2, and the sources resolve against the
// map URL with sourceRoot.
func TestParseAndLookUp(t *testing.T) {
	// gen 0:0 <- src 0 line 0 col 0; gen 1:2 <- line 2 col 2; gen 1:9 <- line 2 col 9
	data := []byte(`)]}'
{"version":3,"sourceRoot":"src","sources":["app.ts"],"names":[],"mappings":"AAAA;EAEE,OAAO"}`)
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ResolvedSources("http://h/js/app.js.map"); got[0] != "http://h/js/src/app.ts" {
		t.Fatalf("resolved sources %v", got)
	}
	want := []Mapping{{0, 0, 0, 0, 0}, {1, 2, 0, 2, 2}, {1, 9, 0, 2, 9}}
	if !reflect.DeepEqual(m.Mappings, want) {
		t.Fatalf("mappings %v, want %v", m.Mappings, want)
	}
	if g, ok := m.Generated(0, 1, 0); !ok || g.GenLine != 1 || g.GenCol != 2 {
		t.Fatalf("Generated(line 1) = %v %v, want gen 1:2", g, ok)
	}
	if _, ok := m.Generated(0, 3, 0); ok {
		t.Fatal("Generated after the last mapped line found a mapping")
	}
	for _, bad := range []string{`{"version":2,"mappings":""}`, `{"version":3,"sections":[]}`, `{"version":3,"sources":[],"mappings":"AAAA"}`, `{"version":3,"sources":["a"],"mappings":"AA"}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) did not fail", bad)
		}
	}
}

func TestDataURL(t *testing.T) {
	for in, want := range map[string]string{
		"data:application/json;base64,eyJhIjoxfQ==":             `{"a":1}`,
		"data:application/json;charset=utf-8;base64,eyJhIjoxfQ": `{"a":1}`,
		"data:application/json,%7B%22a%22%3A1%7D":               `{"a":1}`,
	} {
		got, err := DataURL(in)
		if err != nil || string(got) != want {
			t.Errorf("DataURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := DataURL("http://x/a.map"); err == nil {
		t.Error("DataURL of an http URL did not fail")
	}
}
