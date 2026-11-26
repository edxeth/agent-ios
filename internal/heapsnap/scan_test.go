package heapsnap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf16"
)

// parseReference is Parse before the review's F5 (encoding/json decodes
// the node list into a []uint64): the reference that the scanning Parse
// must agree with.
func parseReference(data []byte) (Snapshot, error) {
	var h header
	if err := json.Unmarshal(data, &h); err != nil {
		return Snapshot{}, err
	}
	if h.Version == nil || *h.Version < 1 || *h.Version > 3 || (h.Type != "Inspector" && h.Type != "") {
		return Snapshot{}, fmt.Errorf("bad header")
	}
	var r struct {
		Nodes      []uint64 `json:"nodes"`
		ClassNames []string `json:"nodeClassNames"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Snapshot{}, err
	}
	if len(r.Nodes) < nodeFields || len(r.Nodes)%nodeFields != 0 {
		return Snapshot{}, fmt.Errorf("bad length")
	}
	s := Snapshot{Version: *h.Version, Type: h.Type}
	by := map[string]*Class{}
	var total uint64
	for i := nodeFields; i < len(r.Nodes); i += nodeFields {
		size, class := r.Nodes[i+1], r.Nodes[i+2]
		if class >= uint64(len(r.ClassNames)) || size > math.MaxInt64-total {
			return Snapshot{}, fmt.Errorf("bad node")
		}
		total += size
		name := r.ClassNames[class]
		if by[name] == nil {
			by[name] = &Class{Name: name}
		}
		by[name].Count++
		by[name].Size += int64(size)
		s.Nodes++
	}
	s.TotalSize = int64(total)
	s.Classes = []Class{}
	for _, c := range by {
		s.Classes = append(s.Classes, *c)
	}
	return s, nil
}

// variant is a generated snapshot in one of the encodings that JSON
// allows for the same content: compact, indented, the members reordered
// (names before nodes, edges first), a repeated nodes member, or with one
// node value spoiled (negative, fraction, exponent, string, beyond 64
// bits, a bad class index, a missing value).
type variant struct {
	m     model
	form  int
	spoil int
	pick  int
}

func (variant) Generate(r *rand.Rand, size int) reflect.Value {
	m := model{}.Generate(r, size).Interface().(model)
	v := variant{m: m, form: r.Intn(5), pick: r.Int()}
	if r.Intn(3) == 0 {
		v.spoil = 1 + r.Intn(7)
	}
	return reflect.ValueOf(v)
}

func (v variant) encode() []byte {
	data := v.m.encode()
	if v.spoil != 0 {
		var d map[string]json.RawMessage
		_ = json.Unmarshal(data, &d)
		nodes := strings.Split(strings.Trim(string(d["nodes"]), "[]"), ",")
		at := v.pick % len(nodes)
		switch v.spoil {
		case 1:
			nodes[at] = "-1"
		case 2:
			nodes[at] = "1.5"
		case 3:
			nodes[at] = "1e2"
		case 4:
			nodes[at] = `"7"`
		case 5:
			nodes[at] = "18446744073709551616"
		case 6:
			nodes = append(nodes[:at], nodes[at+1:]...)
		case 7:
			if len(nodes) > 4 {
				nodes[4+(at%(len(nodes)-4))/4*4+2] = "99"
			}
		}
		d["nodes"] = json.RawMessage("[" + strings.Join(nodes, ",") + "]")
		data, _ = json.Marshal(d)
		v.form = 4 // keep the spoiled map's own order
	}
	switch v.form {
	case 1:
		var b bytes.Buffer
		_ = json.Indent(&b, data, " ", "\t")
		return b.Bytes()
	case 2:
		var d map[string]json.RawMessage
		_ = json.Unmarshal(data, &d)
		return fmt.Appendf(nil, `{"edges":%s,"nodeClassNames":%s,"version":%d,"nodes":%s}`, d["edges"], d["nodeClassNames"], v.m.version, d["nodes"])
	case 3:
		return append([]byte(`{"nodes":[1,2,3],`), data[1:]...) // the later nodes member counts
	}
	return data
}

// Review frames-heap F5: the scanning Parse gives the summary of the
// decoding reference for every generated snapshot in every encoding, and
// fails exactly when the reference fails.
func TestParseMatchesTheDecodingReferenceProperty(t *testing.T) {
	prop := func(v variant) bool {
		data := v.encode()
		got, err := Parse(data)
		want, werr := parseReference(data)
		if (err == nil) != (werr == nil) {
			t.Errorf("Parse(%s): %v; reference %v", data, err, werr)
			return false
		}
		if err != nil {
			return true
		}
		if len(want.Classes) != len(got.Classes) {
			t.Errorf("Parse(%s) = %+v; reference %+v", data, got, want)
			return false
		}
		for _, c := range want.Classes {
			found := false
			for _, g := range got.Classes {
				found = found || g == c
			}
			if !found {
				t.Errorf("Parse(%s) misses %+v: %+v", data, c, got)
				return false
			}
		}
		if got.Version != want.Version || got.Type != want.Type || got.Nodes != want.Nodes || got.TotalSize != want.TotalSize {
			t.Errorf("Parse(%s) = %+v; reference %+v", data, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}

// escapedString is a JSON string literal whose runes are written raw, as
// short escapes or as \uXXXX (surrogate pairs above the BMP), with lone
// surrogates mixed in.
type escapedString string

func (escapedString) Generate(r *rand.Rand, size int) reflect.Value {
	atoms := []rune{'a', '"', '\\', '/', '\n', '\t', '\b', '\f', '\r', 0, 0x1f, 'é', '日', 0x1F600, 0x2028}
	var b strings.Builder
	b.WriteByte('"')
	for range r.Intn(size + 1) {
		c := atoms[r.Intn(len(atoms))]
		switch {
		case r.Intn(6) == 0:
			fmt.Fprintf(&b, "\\u%04x", 0xD800+r.Intn(0x800)) // a lone surrogate
		case r.Intn(2) == 0 || c < 0x20 || c == '"' || c == '\\':
			if c > 0xFFFF {
				hi, lo := utf16.EncodeRune(c)
				fmt.Fprintf(&b, "\\u%04X\\u%04x", hi, lo)
			} else if short, ok := map[rune]string{'"': `\"`, '\\': `\\`, '/': `\/`, '\n': `\n`, '\t': `\t`, '\b': `\b`, '\f': `\f`, '\r': `\r`}[c]; ok && r.Intn(2) == 0 {
				b.WriteString(short)
			} else {
				fmt.Fprintf(&b, "\\u%04x", c)
			}
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return reflect.ValueOf(escapedString(b.String()))
}

// Review frames-heap F5: ReplyData decodes the snapshotData string of a
// reply in place to the bytes that encoding/json decodes it to, wherever
// the member stands, and leaves the result inside the reply's memory.
func TestReplyDataDecodesLikeEncodingJSONProperty(t *testing.T) {
	prop := func(s escapedString, after bool) bool {
		reply := []byte(`{"timestamp":1.5,"snapshotData":` + string(s) + `}`)
		if after {
			reply = []byte(`{ "snapshotData" : ` + string(s) + ` , "timestamp":{"a":["}"]} }`)
		}
		var want string
		if err := json.Unmarshal([]byte(s), &want); err != nil {
			t.Errorf("generator: %s: %v", s, err)
			return false
		}
		got, err := ReplyData(reply)
		if err != nil || string(got) != want {
			t.Errorf("ReplyData(%s) = %q, %v; want %q", s, got, err, want)
			return false
		}
		if len(got) > 0 && !bytes.Contains(reply, got) {
			t.Errorf("ReplyData did not decode in place")
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Fatal(err)
	}
}

// A reply without snapshotData, or with null, gives no data; one that is
// not JSON, or whose snapshotData is not a string, fails.
func TestReplyDataRefusesOtherShapes(t *testing.T) {
	for _, in := range []string{`{"timestamp":1}`, `{"snapshotData":null}`} {
		if got, err := ReplyData([]byte(in)); err != nil || got != nil {
			t.Errorf("ReplyData(%s) = %q, %v; want no data", in, got, err)
		}
	}
	for _, in := range []string{`{"snapshotData":"x"`, `[]`, `{"snapshotData":7}`} {
		if _, err := ReplyData([]byte(in)); err == nil {
			t.Errorf("ReplyData(%s) gave no error", in)
		}
	}
}
