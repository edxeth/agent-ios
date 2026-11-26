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
	"math"
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
	// unmapped are the generated positions of the one-field segments
	// (no original position), ordered; each ends the mapping before it
	// (Original). segments counts every segment the map kept.
	unmapped []genPos
	segments int
}

// genPos is a generated position.
type genPos struct{ line, col int }

// Limits are the budgets of one source map (Parse): its size in bytes and
// its number of segments. A map is read host-side and kept for the page
// connection, so a page cannot make the daemon decode and keep maps of
// any size.
type Limits struct {
	Bytes    int
	Segments int
}

// DefaultLimits are Parse's budgets: 16 MiB of map text and 2 million
// segments (a segment is kept as 40 bytes, so about 80 MB). A large
// production bundle has a few hundred thousand segments.
var DefaultLimits = Limits{Bytes: 16 << 20, Segments: 2_000_000}

// ErrTooLarge reports a source map beyond its Limits.
var ErrTooLarge = errors.New("source map too large")

// Segments returns the number of segments the map holds, one-field
// segments included.
func (m *Map) Segments() int { return m.segments }

// Parse reads a source map within DefaultLimits. An index map (with
// "sections") is refused: bundlers emit it rarely and the logpoint command
// does not need it.
func Parse(data []byte) (*Map, error) { return ParseWithLimits(data, DefaultLimits) }

// ParseWithLimits is Parse with the budgets lim; a map beyond them fails
// with ErrTooLarge.
func ParseWithLimits(data []byte, lim Limits) (*Map, error) {
	if len(data) > lim.Bytes {
		return nil, fmt.Errorf("%w: %d bytes, more than the %d bytes agent-ios reads", ErrTooLarge, len(data), lim.Bytes)
	}
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
	ms, ends, n, err := decode(*raw.Mappings, lim.Segments)
	if err != nil {
		return nil, fmt.Errorf("source map: %w", err)
	}
	m.unmapped, m.segments = ends, n
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
// ok is false when there is none or that segment has no original position
// (a one-field segment ends the mapping before it).
func (m *Map) Original(line, col int) (Mapping, bool) {
	i := sort.Search(len(m.Mappings), func(i int) bool {
		x := m.Mappings[i]
		return x.GenLine > line || x.GenLine == line && x.GenCol > col
	})
	if i == 0 || m.Mappings[i-1].GenLine != line {
		return Mapping{}, false
	}
	x := m.Mappings[i-1]
	j := sort.Search(len(m.unmapped), func(j int) bool { return posBefore(genPos{line, col}, m.unmapped[j]) })
	if j > 0 && m.unmapped[j-1].line == line && m.unmapped[j-1].col > x.GenCol {
		return Mapping{}, false
	}
	return x, true
}

// DecodeMappings decodes the "mappings" string. Segments without an
// original position (one field) are skipped; the result is ordered by
// generated position.
func DecodeMappings(s string) ([]Mapping, error) {
	ms, _, _, err := decode(s, math.MaxInt)
	return ms, err
}

// decode decodes the "mappings" string into the segments with an original
// position and the positions of the one-field segments, each ordered by
// generated position, and counts the segments; more than maxSegs fail with
// ErrTooLarge.
func decode(s string, maxSegs int) ([]Mapping, []genPos, int, error) {
	var out []Mapping
	var ends []genPos
	var all []rawSeg
	count := 0
	var src, oline, ocol, name int
	line := 0
	for _, l := range strings.Split(s, ";") {
		col := 0
		if l != "" {
			for _, seg := range strings.Split(l, ",") {
				if seg == "" {
					continue
				}
				if count++; count > maxSegs {
					return nil, nil, 0, fmt.Errorf("%w: more than %d segments, the limit agent-ios reads", ErrTooLarge, maxSegs)
				}
				f, err := decodeSegment(seg)
				if err != nil {
					return nil, nil, 0, fmt.Errorf("mappings line %d: %w", line, err)
				}
				switch len(f) {
				case 1, 4, 5:
				default:
					return nil, nil, 0, fmt.Errorf("mappings line %d: a segment of %d fields", line, len(f))
				}
				col += f[0]
				if col < 0 {
					return nil, nil, 0, fmt.Errorf("mappings line %d: a negative position", line)
				}
				if len(f) == 1 {
					all = append(all, rawSeg{pos: genPos{line, col}})
					continue
				}
				src, oline, ocol = src+f[1], oline+f[2], ocol+f[3]
				if len(f) == 5 {
					name += f[4]
				}
				if src < 0 || oline < 0 || ocol < 0 || name < 0 {
					return nil, nil, 0, fmt.Errorf("mappings line %d: a negative position", line)
				}
				all = append(all, rawSeg{pos: genPos{line, col}, mapped: true, m: Mapping{GenLine: line, GenCol: col, Source: src, OrigLine: oline, OrigCol: ocol}})
			}
		}
		line++
	}
	// Of segments at the same generated position the last one counts, as
	// it would for a reader that walks the line.
	sort.SliceStable(all, func(i, j int) bool { return posBefore(all[i].pos, all[j].pos) })
	for i, x := range all {
		if i+1 < len(all) && all[i+1].pos == x.pos {
			continue
		}
		if x.mapped {
			out = append(out, x.m)
		} else {
			ends = append(ends, x.pos)
		}
	}
	return out, ends, count, nil
}

// rawSeg is a decoded segment: with an original position (mapped) or one
// field.
type rawSeg struct {
	pos    genPos
	mapped bool
	m      Mapping
}

func posBefore(a, b genPos) bool { return a.line < b.line || a.line == b.line && a.col < b.col }

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
