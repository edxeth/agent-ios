// Package sourcemap reads Source Map revision 3 files (the "source map"
// format of https://tc39.es/ecma426/) far enough to move a location of an
// original source to its generated script and back. The logpoint command
// uses it host-side: WebKit Inspector Protocol knows no source maps, it
// only reports a script's sourceMapURL (Debugger.scriptParsed).
//
// Lines and columns are 0-based everywhere in this package, as in the
// mappings and in the WebKit protocol.
package sourcemap

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Mapping is one segment of the mappings that names an original
// position: the generated position GenLine:GenCol comes from
// OrigLine:OrigCol of Sources[Source].
type Mapping struct {
	GenLine, GenCol   int
	Source            int
	OrigLine, OrigCol int
}

// Map is a parsed source map.
type Map struct {
	// Sources are the source names as the map gives them, each prefixed
	// with sourceRoot (ResolvedSources resolves them against a URL).
	Sources []string
	// Mappings are the segments with an original position, ordered by
	// generated position.
	Mappings []Mapping
}

// Parse reads a source map. An index map (with "sections") is refused:
// bundlers emit it rarely and the logpoint command does not need it.
func Parse(data []byte) (*Map, error) {
	var raw struct {
		Version    int               `json:"version"`
		SourceRoot string            `json:"sourceRoot"`
		Sources    []*string         `json:"sources"`
		Mappings   *string           `json:"mappings"`
		Sections   []json.RawMessage `json:"sections"`
	}
	// A map may start with the XSSI guard ")]}'" on its own line.
	if s := string(data); strings.HasPrefix(s, ")]}'") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("source map: %w", err)
	}
	if raw.Version != 3 {
		return nil, fmt.Errorf("source map: version %d, want 3", raw.Version)
	}
	if raw.Sections != nil {
		return nil, errors.New("source map: index maps (sections) are not supported")
	}
	if raw.Mappings == nil {
		return nil, errors.New("source map: no mappings")
	}
	m := &Map{Sources: make([]string, len(raw.Sources))}
	root := raw.SourceRoot
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	for i, s := range raw.Sources {
		if s != nil {
			m.Sources[i] = root + *s
		}
	}
	ms, err := DecodeMappings(*raw.Mappings)
	if err != nil {
		return nil, fmt.Errorf("source map: %w", err)
	}
	for _, x := range ms {
		if x.Source >= len(m.Sources) {
			return nil, fmt.Errorf("source map: a mapping names source %d of %d", x.Source, len(m.Sources))
		}
	}
	m.Mappings = ms
	return m, nil
}

// ResolvedSources returns the sources resolved against the URL of the map
// (base), as Web Inspector and DevTools show them. A source that does not
// resolve is returned as given.
func (m *Map) ResolvedSources(base string) []string {
	out := make([]string, len(m.Sources))
	b, err := url.Parse(base)
	for i, s := range m.Sources {
		out[i] = s
		if err != nil || b.Scheme == "data" {
			continue
		}
		if u, err := b.Parse(s); err == nil {
			out[i] = u.String()
		}
	}
	return out
}

// Generated returns the mapping of source that comes first at or after
// the original position line:col: the smallest original column at or
// after col on line, else the smallest column of the next original line
// that has a mapping (a line without code moves to the next possible
// location). Among segments with the same original position the first
// generated one wins. ok is false when no such mapping exists.
func (m *Map) Generated(source, line, col int) (Mapping, bool) {
	var best Mapping
	found := false
	for _, x := range m.Mappings {
		if x.Source != source || x.OrigLine < line || x.OrigLine == line && x.OrigCol < col {
			continue
		}
		if !found || origBefore(x, best) {
			best, found = x, true
		}
	}
	return best, found
}

// origBefore orders mappings by original position, then generated one.
func origBefore(a, b Mapping) bool {
	if a.OrigLine != b.OrigLine {
		return a.OrigLine < b.OrigLine
	}
	if a.OrigCol != b.OrigCol {
		return a.OrigCol < b.OrigCol
	}
	return genBefore(a, b)
}

func genBefore(a, b Mapping) bool {
	if a.GenLine != b.GenLine {
		return a.GenLine < b.GenLine
	}
	return a.GenCol < b.GenCol
}

// Original returns the mapping that covers the generated position
// line:col: the last segment of that line that starts at or before col.
func (m *Map) Original(line, col int) (Mapping, bool) {
	i := sort.Search(len(m.Mappings), func(i int) bool {
		x := m.Mappings[i]
		return x.GenLine > line || x.GenLine == line && x.GenCol > col
	})
	if i == 0 || m.Mappings[i-1].GenLine != line {
		return Mapping{}, false
	}
	return m.Mappings[i-1], true
}

// DecodeMappings decodes the "mappings" string. Segments without an
// original position (one field) are skipped; the result is ordered by
// generated position.
func DecodeMappings(s string) ([]Mapping, error) {
	var out []Mapping
	var src, oline, ocol, name int
	line := 0
	for _, l := range strings.Split(s, ";") {
		col := 0
		if l != "" {
			for _, seg := range strings.Split(l, ",") {
				if seg == "" {
					continue
				}
				f, err := decodeSegment(seg)
				if err != nil {
					return nil, fmt.Errorf("mappings line %d: %w", line, err)
				}
				switch len(f) {
				case 1, 4, 5:
				default:
					return nil, fmt.Errorf("mappings line %d: a segment of %d fields", line, len(f))
				}
				col += f[0]
				if len(f) == 1 {
					continue
				}
				src, oline, ocol = src+f[1], oline+f[2], ocol+f[3]
				if len(f) == 5 {
					name += f[4]
				}
				if col < 0 || src < 0 || oline < 0 || ocol < 0 || name < 0 {
					return nil, fmt.Errorf("mappings line %d: a negative position", line)
				}
				out = append(out, Mapping{GenLine: line, GenCol: col, Source: src, OrigLine: oline, OrigCol: ocol})
			}
		}
		line++
	}
	sort.SliceStable(out, func(i, j int) bool { return genBefore(out[i], out[j]) })
	return out, nil
}

// EncodeMappings is the inverse of DecodeMappings for mappings ordered by
// generated position (each segment with four fields).
func EncodeMappings(ms []Mapping) string {
	var b strings.Builder
	var src, oline, ocol int
	line, col, first := 0, 0, true
	for _, x := range ms {
		for line < x.GenLine {
			b.WriteByte(';')
			line++
			col, first = 0, true
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		for _, v := range []int{x.GenCol - col, x.Source - src, x.OrigLine - oline, x.OrigCol - ocol} {
			b.WriteString(EncodeVLQ(v))
		}
		col, src, oline, ocol = x.GenCol, x.Source, x.OrigLine, x.OrigCol
	}
	return b.String()
}

const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64Value = func() (v [256]int8) {
	for i := range v {
		v[i] = -1
	}
	for i := 0; i < len(b64); i++ {
		v[b64[i]] = int8(i)
	}
	return v
}()

// maxVLQBits bounds a value: positions beyond 2^31 are corrupt input.
const maxVLQBits = 32

// decodeSegment decodes the VLQ values of one segment.
func decodeSegment(seg string) ([]int, error) {
	var out []int
	for i := 0; i < len(seg); {
		v, n, err := DecodeVLQ(seg[i:])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		i += n
	}
	return out, nil
}

// DecodeVLQ decodes the first Base64 VLQ value of s and returns it with
// the number of bytes it took.
func DecodeVLQ(s string) (value, n int, err error) {
	var acc uint64
	shift := 0
	for n < len(s) {
		d := b64Value[s[n]]
		if d < 0 {
			return 0, 0, fmt.Errorf("invalid VLQ character %q", s[n])
		}
		n++
		acc |= uint64(d&31) << shift
		shift += 5
		if d&32 == 0 {
			v := int(acc >> 1)
			if acc&1 != 0 {
				v = -v
			}
			return v, n, nil
		}
		if shift >= maxVLQBits+5 {
			return 0, 0, errors.New("VLQ value too large")
		}
	}
	return 0, 0, errors.New("VLQ value ends without its last digit")
}

// EncodeVLQ encodes v as Base64 VLQ.
func EncodeVLQ(v int) string {
	u := uint64(v) << 1
	if v < 0 {
		u = uint64(-v)<<1 | 1
	}
	var b []byte
	for {
		d := u & 31
		u >>= 5
		if u != 0 {
			d |= 32
		}
		b = append(b, b64[d])
		if u == 0 {
			return string(b)
		}
	}
}

// DataURL returns the content of a data: URL, base64 or percent-encoded.
func DataURL(u string) ([]byte, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return nil, fmt.Errorf("not a data: URL")
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, fmt.Errorf("data: URL without a comma")
	}
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		out, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			// Some generators drop the padding.
			if out2, err2 := base64.RawStdEncoding.DecodeString(strings.TrimRight(data, "=")); err2 == nil {
				return out2, nil
			}
			return nil, fmt.Errorf("data: URL: %w", err)
		}
		return out, nil
	}
	s, err := url.PathUnescape(data)
	if err != nil {
		return nil, fmt.Errorf("data: URL: %w", err)
	}
	return []byte(s), nil
}
