package heapsnap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// The scans in this file read JSON that encoding/json already checked
// (json.Valid or a successful json.Unmarshal of the same bytes), so they
// only find value boundaries and never validate the grammar again.

// isSpace reports JSON whitespace.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(data []byte, i int) int {
	for i < len(data) && isSpace(data[i]) {
		i++
	}
	return i
}

// skipString returns the index after the string that starts at data[i]
// (a quote).
func skipString(data []byte, i int) int {
	for i++; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index after the value that starts at data[i].
func skipValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		depth := 0
		for i < len(data) {
			switch data[i] {
			case '"':
				i = skipString(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' && !isSpace(data[i]) {
		i++
	}
	return i
}

// members returns the raw values of the members "nodes" and
// "nodeClassNames" of the checked JSON object data (nil when absent; the
// last one when repeated).
func members(data []byte) (nodes, names []byte) {
	i := skipSpace(data, 0) + 1 // past '{'
	for {
		i = skipSpace(data, i)
		if i >= len(data) || data[i] != '"' {
			return nodes, names // '}'
		}
		kEnd := skipString(data, i)
		key := data[i+1 : kEnd-1]
		i = skipSpace(data, kEnd) + 1 // past ':'
		i = skipSpace(data, i)
		vEnd := skipValue(data, i)
		switch {
		case bytes.Equal(key, []byte("nodes")):
			nodes = data[i:vEnd]
		case bytes.Equal(key, []byte("nodeClassNames")):
			names = data[i:vEnd]
		}
		i = skipSpace(data, vEnd)
		if i < len(data) && data[i] == ',' {
			i++
		}
	}
}

// scanNodes reads the checked JSON node list raw (an array of unsigned
// integers, or null for none) and calls node with the index, the self
// size and the class index of each node after the <root>, in file order.
// It returns the number of values. A complete node is reported only when
// its four values were read.
func scanNodes(raw []byte, node func(i int, size, class uint64)) (int, error) {
	if raw == nil || string(raw) == "null" {
		return 0, nil
	}
	if raw[0] != '[' {
		return 0, fmt.Errorf("reading the heap snapshot: the nodes member is not an array")
	}
	var size, class uint64
	n := 0
	for i := 1; ; {
		i = skipSpace(raw, i)
		if i >= len(raw) || raw[i] == ']' {
			return n, nil
		}
		start := i
		var v uint64
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			d := uint64(raw[i] - '0')
			if v > (1<<64-1-d)/10 {
				return n, fmt.Errorf("reading the heap snapshot: node list value %d does not fit in 64 bits", n)
			}
			v = v*10 + d
			i++
		}
		if i == start || (i < len(raw) && raw[i] != ',' && raw[i] != ']' && !isSpace(raw[i])) {
			end := skipValue(raw, start)
			return n, fmt.Errorf("reading the heap snapshot: node list value %d is %s, not an unsigned integer", n, shortRaw(raw[start:end]))
		}
		switch n % nodeFields {
		case 1:
			size = v
		case 2:
			class = v
		case nodeFields - 1:
			if n >= nodeFields {
				node(n/nodeFields, size, class)
			}
		}
		n++
		i = skipSpace(raw, i)
		if i < len(raw) && raw[i] == ',' {
			i++
		}
	}
}

// shortRaw quotes at most 32 bytes of a JSON value for an error message.
func shortRaw(v []byte) string {
	if len(v) > 32 {
		return fmt.Sprintf("%q…", v[:32])
	}
	return fmt.Sprintf("%q", v)
}

// ReplyData returns the snapshotData string of a Heap.snapshot reply
// ({"timestamp":...,"snapshotData":"..."}) as bytes, unescaped in place:
// the result shares reply's memory and reply is overwritten, so that a
// snapshot of tens of MB is held once. Escapes decode as in encoding/json
// (a lone surrogate becomes U+FFFD); other bytes are kept as they are. A
// missing or null snapshotData gives no bytes and no error.
func ReplyData(reply []byte) ([]byte, error) {
	if !json.Valid(reply) {
		return nil, errors.New("reading the Heap.snapshot reply: it is not JSON")
	}
	i := skipSpace(reply, 0)
	if reply[i] != '{' {
		return nil, errors.New("reading the Heap.snapshot reply: it is not a JSON object")
	}
	var data []byte
	for i++; ; {
		i = skipSpace(reply, i)
		if reply[i] != '"' {
			return data, nil // '}'
		}
		kEnd := skipString(reply, i)
		key := reply[i+1 : kEnd-1]
		i = skipSpace(reply, skipSpace(reply, kEnd)+1)
		vEnd := skipValue(reply, i)
		if bytes.Equal(key, []byte("snapshotData")) {
			switch reply[i] {
			case '"':
				data = unescapeInPlace(reply[i+1 : vEnd-1])
			case 'n':
				data = nil
			default:
				return nil, fmt.Errorf("reading the Heap.snapshot reply: snapshotData is %s, not a string", shortRaw(reply[i:vEnd]))
			}
		}
		i = skipSpace(reply, vEnd)
		if reply[i] == ',' {
			i++
		}
	}
}

// unescapeInPlace decodes the body of a checked JSON string into its own
// memory and returns the decoded prefix. Every escape is at least as long
// as what it decodes to, so the write position never passes the read
// position.
func unescapeInPlace(s []byte) []byte {
	w := 0
	for r := 0; r < len(s); {
		c := s[r]
		if c != '\\' {
			s[w] = c
			w++
			r++
			continue
		}
		switch e := s[r+1]; e {
		case 'u':
			rn := rune(hex4(s[r+2 : r+6]))
			r += 6
			if utf16.IsSurrogate(rn) {
				rn2 := utf8.RuneError
				if r+6 <= len(s) && s[r] == '\\' && s[r+1] == 'u' {
					rn2 = rune(hex4(s[r+2 : r+6]))
				}
				if dec := utf16.DecodeRune(rn, rn2); dec != utf8.RuneError {
					rn = dec
					r += 6
				} else {
					rn = utf8.RuneError
				}
			}
			w += utf8.EncodeRune(s[w:], rn)
		default:
			s[w] = unescapeByte[e]
			w++
			r += 2
		}
	}
	return s[:w]
}

// unescapeByte maps the character after a backslash to its byte.
var unescapeByte = [256]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

func hex4(h []byte) uint16 {
	var v uint16
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		default:
			c -= 'A' - 10
		}
		v = v<<4 | uint16(c)
	}
	return v
}
