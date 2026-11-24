// Package browserurl tells, from the text of two URLs alone, whether a
// browser can show them as the same document: tab find's prefilter, and
// sim up's check that Safari lists the tab of its --url. It reads each
// part in the form that a browser writes (WHATWG URL) only when that form
// is certain from the text.
package browserurl

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// specialSchemes are the schemes whose URLs a browser parses with a host
// name, a port and dot segments (WHATWG URL, "special scheme"), each with
// its default port. "file" is left out: its host and path have other
// rules.
var specialSchemes = map[string]uint64{"http": 80, "https": 443, "ws": 80, "wss": 443, "ftp": 21}

// The prefilter of tab find (SameDocument) excludes a tab only on a part
// of the URL whose browser form is certain from the text alone. It reads
// the raw text, as the caller wrote it or as the browser listed it, never
// a form that net/url rewrote, and it knows only this safe form
// (WHATWG URL; node prints the browser form: new URL(text).href):
//
//   - The whole text has no ASCII tab, line feed or carriage return (a
//     browser removes them anywhere) and no C0 control or space at either
//     end (a browser strips them). Otherwise no part excludes.
//   - The scheme is a letter followed by letters, digits, "+", "-" and
//     ".", then ":", compared in lower case. Otherwise no part excludes.
//   - With a special scheme, exactly "//" and then a character that is
//     neither "/" nor "\" follow the scheme (a browser skips any number of
//     slashes and backslashes there). Otherwise host, port and path
//     exclude nothing. The authority ends at the first "/", "\", "?" or
//     "#": a browser reads "\" as "/" in these schemes.
//   - Host and port need an authority without "@", "[" and "]" and with
//     at most one ":" (userinfo and IPv6 addresses are not taken). The
//     host has only letters, digits, "-" and "."; no label starts with
//     "xn--" (IDNA: its browser result differs between versions); a host
//     whose last label is a number is an IPv4 address for a browser, which
//     also reads hexadecimal, octal and short forms, so only the canonical
//     dotted decimal form is taken. The port is decimal digits or empty.
//   - The path, from the end of the authority to the first "?" or "#",
//     has only unreserved characters (letters, digits, "-", ".", "_",
//     "~"), sub-delimiters ("!$&'()*+,;="), ":", "@", "/" and well-formed
//     "%XX" escapes. A browser changes such a path only by removing its
//     dot segments.
//
// Any other character in a part (a backslash, a space, a control
// character, a character outside ASCII, "|", "^", ...) makes that part
// exclude nothing: the tab is read and the page decides.
const (
	alnum      = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	schemeSafe = alnum + "+-."
	hostSafe   = alnum + "-."
	pathSafe   = alnum + "-._~" + "!$&'()*+,;=" + ":@/%"
)

// docKey is what a listed URL and a target must share to be the same
// document, each part as a browser writes it. A part whose ok field is
// false is not in the safe form and excludes nothing.
type docKey struct {
	scheme                 string
	host, port, path       string
	hostOK, portOK, pathOK bool
}

// documentKey is the docKey of raw, a URL as the caller wrote it or as a
// browser listed it. ok is false when the text is not in the safe form
// that a scheme needs; for a scheme that is not special, only the scheme
// is known.
func documentKey(raw string) (k docKey, ok bool) {
	if raw == "" || strings.ContainsAny(raw, "\t\n\r") || raw[0] <= ' ' || raw[len(raw)-1] <= ' ' {
		return docKey{}, false
	}
	scheme, rest, found := strings.Cut(raw, ":")
	if !found || scheme == "" || !isLetter(scheme[0]) || !onlyOf(scheme, schemeSafe) {
		return docKey{}, false
	}
	k.scheme = strings.ToLower(scheme)
	defaultPort, special := specialSchemes[k.scheme]
	after, slashes := strings.CutPrefix(rest, "//")
	if !special || !slashes || after == "" || after[0] == '/' || after[0] == '\\' {
		return k, true
	}
	end := strings.IndexAny(after, "/\\?#")
	if end < 0 {
		end = len(after)
	}
	authority, path := after[:end], after[end:]
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	host, port, hasPort := strings.Cut(authority, ":")
	if !strings.ContainsAny(authority, "@[]") && !strings.Contains(port, ":") {
		k.host, k.hostOK = browserHost(host)
		k.port, k.portOK = "", true
		if hasPort {
			k.port, k.portOK = browserPort(port, defaultPort)
		}
	}
	k.path, k.pathOK = browserPath(path)
	return k, true
}

func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// onlyOf reports whether every byte of s is one of the bytes of set.
func onlyOf(s, set string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(set, s[i]) < 0 {
			return false
		}
	}
	return true
}

// browserHost is a host in the safe form as a browser writes it: in lower
// case, without a trailing dot. A browser keeps that dot (another host),
// but leaving it out only makes the filter read one more tab.
func browserHost(host string) (string, bool) {
	if !onlyOf(host, hostSafe) {
		return "", false
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return "", false
	}
	for label := range strings.SplitSeq(h, ".") {
		if strings.HasPrefix(label, "xn--") {
			return "", false
		}
	}
	if endsInNumber(h) {
		ip, err := netip.ParseAddr(h)
		if err != nil || !ip.Is4() || ip.String() != h {
			return "", false
		}
	}
	return h, true
}

// endsInNumber reports whether the last label of an ASCII host is a
// number for a browser (WHATWG URL, "ends in a number"): decimal digits,
// or "0x" with hexadecimal digits.
func endsInNumber(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	hex, ok := strings.CutPrefix(last, "0x")
	return ok && strings.Trim(hex, "0123456789abcdef") == ""
}

// browserPort is the port of a URL as a browser writes it: a decimal
// number without leading zeros, and "" for an empty port or the scheme's
// default port.
func browserPort(port string, defaultPort uint64) (string, bool) {
	if port == "" {
		return "", true
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", false
	}
	if n == defaultPort {
		return "", true
	}
	return strconv.FormatUint(n, 10), true
}

// browserPath is a path in the safe form with its dot segments removed as
// a browser removes them (WHATWG URL, path state: ".", "..", and their
// percent-encoded forms "%2e" in any case), "" as "/", and each segment
// percent-decoded. A browser keeps the percent-encoding of a path, so the
// decoding takes some paths for one that a browser tells apart; it only
// makes the filter read more tabs.
func browserPath(p string) (string, bool) {
	if !onlyOf(p, pathSafe) {
		return "", false
	}
	segments := strings.Split(strings.TrimPrefix(p, "/"), "/")
	var out []string
	for i, s := range segments {
		last := i == len(segments)-1
		switch strings.ReplaceAll(strings.ToLower(s), "%2e", ".") {
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case ".":
			if last {
				out = append(out, "")
			}
		default:
			d, err := url.PathUnescape(s)
			if err != nil {
				return "", false
			}
			out = append(out, d)
		}
	}
	return "/" + strings.Join(out, "/"), true
}

// SameDocument reports whether a listed URL (without query and fragment)
// can be the page of target, the URL as the caller wrote it. It only
// narrows the tabs that tab find reads, so that it reads no tab of another
// site: a Safari tab that is not visible never answers a read. It
// excludes a tab only when a part that both URLs have in the safe form
// differs; the read inside the page decides.
func SameDocument(listed, target string) bool {
	l, lok := documentKey(listed)
	t, tok := documentKey(target)
	if !lok || !tok {
		return true
	}
	differ := func(lok, tok bool, l, t string) bool { return lok && tok && l != t }
	return l.scheme == t.scheme &&
		!differ(l.hostOK, t.hostOK, l.host, t.host) &&
		!differ(l.portOK, t.portOK, l.port, t.port) &&
		!differ(l.pathOK, t.pathOK, l.path, t.path)
}
