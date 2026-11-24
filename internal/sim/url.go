package sim

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// sameURL reports whether page, a URL that Safari lists, is target, sim
// up's --url: both have a browser form (browserForm), and the two are
// equal. A URL without one never matches, so Up never takes a tab that
// only may show its URL.
func sameURL(page, target string) bool {
	p, ok := browserForm(page)
	if !ok {
		return false
	}
	t, ok := browserForm(target)
	return ok && p == t
}

// browserForm returns raw as a browser writes it (WHATWG URL, the href of
// new URL(raw)), when that form is certain from the text: the escapes
// that it adds are in upper-case hex, and the escapes of raw keep their
// hex case, as a browser keeps them ("%2f" and "%2F" are different hrefs;
// final review F3). ok is false for any other URL. The form:
//
//   - Leading and trailing C0 controls and spaces are stripped, tabs and
//     line breaks removed, as a browser does; the rest is valid UTF-8 with
//     no other C0 control and no DEL.
//   - The scheme is http or https in any case, followed by exactly "//"
//     and an authority; written in lower case.
//   - The authority has no user info ("@"). The host is a name of ASCII
//     letters, digits and "-" in dot-separated non-empty labels, none
//     starting with "xn--" (IDNA), written in lower case; or a plain IPv4
//     address (four decimal numbers up to 255 without leading zeros): a
//     host whose last label is a number in any other form is refused, as
//     a browser reads it as an IPv4 address in another notation; or an
//     IPv6 address in brackets, without a zone, an embedded IPv4 address
//     or an IPv4-mapped address, written in its standard form (RFC 5952).
//   - The port is decimal digits up to 65535, written without leading
//     zeros and dropped when it is the scheme's default; ":" alone is no
//     port.
//   - The path (from the authority to "?" or "#"; "/" when empty) loses
//     its dot segments ("." and "..", also escaped as "%2e"), as a
//     browser removes them. A backslash before the query is refused.
//   - Path, query and fragment keep their literal unreserved characters,
//     sub-delimiters, ":", "@", "/" (and "?" in the query and fragment),
//     and their well-formed escapes as written; characters
//     outside ASCII, the space, '"', "<" and ">", and in the path "`",
//     "{" and "}", and in the fragment "`", are escaped as their UTF-8
//     bytes, as a browser escapes them. Any other character ("^", "|",
//     "[", "]", "\\", "'" in the query, "#" in the fragment, a "%" that
//     starts no escape, ...) is refused: browsers do not all escape them
//     alike.
//   - An empty query ("?") and an empty fragment ("#") stay: they are part
//     of the URL.
func browserForm(raw string) (string, bool) {
	// Checked first: TrimFunc and Map read invalid bytes as U+FFFD.
	if !utf8.ValidString(raw) {
		return "", false
	}
	raw = strings.TrimFunc(raw, func(r rune) bool { return r <= ' ' })
	raw = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, raw)
	scheme, rest, found := strings.Cut(raw, "://")
	scheme = strings.ToLower(scheme)
	if !found || (scheme != "http" && scheme != "https") {
		return "", false
	}
	end := strings.IndexAny(rest, "/?#\\")
	if end < 0 {
		end = len(rest)
	}
	host, ok := browserHost(rest[:end], scheme)
	if !ok {
		return "", false
	}
	rest = rest[end:]
	pathEnd := strings.IndexAny(rest, "?#")
	if pathEnd < 0 {
		pathEnd = len(rest)
	}
	path, ok := browserPath(rest[:pathEnd])
	if !ok {
		return "", false
	}
	rest = rest[pathEnd:]
	var query, fragment string
	if strings.HasPrefix(rest, "?") {
		q, f, hasF := strings.Cut(rest[1:], "#")
		if query, ok = escapeAs(q, queryEscaped, queryKept); !ok {
			return "", false
		}
		query = "?" + query
		rest = ""
		if hasF {
			rest = "#" + f
		}
	}
	if strings.HasPrefix(rest, "#") {
		if fragment, ok = escapeAs(rest[1:], fragmentEscaped, fragmentKept); !ok {
			return "", false
		}
		fragment = "#" + fragment
	}
	return scheme + "://" + host + path + query + fragment, true
}

// browserHost is the host and port of authority in their browser form.
func browserHost(authority, scheme string) (string, bool) {
	if authority == "" || strings.Contains(authority, "@") {
		return "", false
	}
	var host, port string
	if strings.HasPrefix(authority, "[") {
		addr, after, found := strings.Cut(authority[1:], "]")
		if !found || strings.ContainsAny(addr, ".%") {
			return "", false
		}
		ip, err := netip.ParseAddr(addr)
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			return "", false
		}
		host = "[" + ip.String() + "]"
		if after != "" {
			p, ok := strings.CutPrefix(after, ":")
			if !ok {
				return "", false
			}
			port = p
		}
	} else {
		if strings.ContainsAny(authority, "[]") || strings.Count(authority, ":") > 1 {
			return "", false
		}
		h, p, _ := strings.Cut(authority, ":")
		host, port = strings.ToLower(h), p
		if !hostName(host) {
			return "", false
		}
	}
	if port == "" {
		return host, true
	}
	if strings.Trim(port, "0123456789") != "" {
		return "", false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", false
	}
	if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
		return host, true
	}
	return host + ":" + strconv.FormatUint(n, 10), true
}

// hostName reports whether h, in lower case, is a host name or a plain
// IPv4 address that a browser writes as it is.
func hostName(h string) bool {
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.HasPrefix(l, "xn--") {
			return false
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") != "" && !strings.HasPrefix(last, "0x") {
		return true // a name: its last label is no number
	}
	if len(labels) != 4 {
		return false
	}
	for _, l := range labels {
		n, err := strconv.ParseUint(l, 10, 8)
		if err != nil || strconv.FormatUint(n, 10) != l {
			return false
		}
	}
	return true
}

// browserPath is path, "" or starting with "/", without dot segments and
// escaped as a browser escapes it (WHATWG URL, path state).
func browserPath(path string) (string, bool) {
	if path == "" {
		return "/", true
	}
	if strings.Contains(path, "\\") {
		return "", false
	}
	segments := strings.Split(path[1:], "/")
	var out []string
	for i, s := range segments {
		last := i == len(segments)-1
		e, ok := escapeAs(s, pathEscaped, pathKept)
		if !ok {
			return "", false
		}
		switch strings.ToLower(e) {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case ".", "%2e":
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, e)
		}
	}
	return "/" + strings.Join(out, "/"), true
}

// The ASCII characters that each part keeps as they are, and those that a
// browser escapes in it; every other ASCII character is refused.
const (
	urlUnreserved   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~" + "!$&()*+,;=" + ":@/"
	pathKept        = urlUnreserved + "'"
	pathEscaped     = " \"<>`{}"
	queryKept       = urlUnreserved + "?"
	queryEscaped    = " \"<>"
	fragmentKept    = urlUnreserved + "'?"
	fragmentEscaped = " \"<>`"
)

// escapeAs writes s with its well-formed escapes as they are (a browser
// keeps their hex case), and escaped, and every byte outside ASCII,
// escaped in upper-case hex; ok is false when s has
// a "%" that starts no escape, or an ASCII character outside kept and
// escaped.
func escapeAs(s, escaped, kept string) (string, bool) {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", false
			}
			b.WriteString(s[i : i+3])
			i += 2
		case c >= 0x80 || strings.IndexByte(escaped, c) >= 0:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		case strings.IndexByte(kept, c) >= 0:
			b.WriteByte(c)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
