package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// ExitNoMatch is the exit code of "tab find" when it read every tab that
// can show the target and none shows it. No other failure has it: a
// listing that hides pages ends the command with exit 6, a tab that could
// not be read with exit 1 or the exit code of that read (4: another
// session holds the tab), so a caller that acts on "the target is not
// open" can tell it from "I could not look".
const ExitNoMatch = 7

// tab find answers "which tab shows exactly this URL" without printing a
// title or a URL. A caller that was told to work on one page finds its tab
// with it before the first action: the listing strips the query and the
// fragment of every URL (design 2.5 rule 4), so only a read inside the
// page tells a signed link from another state of the same path.
func init() {
	Register(&Command{
		Name:    "tab find",
		Summary: "Print the id of the tab whose page shows exactly this URL (query and fragment included), compared inside the page. It only reads: it navigates and selects nothing. The session keeps the lease of the tab that the command prints, as after any read of that tab, and no lease that only the command used on another tab. Exit 7 when no tab shows the URL; exit 1 when several do or a tab could not be read; exit 6 when the listing hides pages of a device that the caller has not reserved.",
		Args:    []Arg{{Name: "url", Help: "the URL to find; or --url-file"}},
		Flags:   []Flag{urlFileFlag},
		Page:    NoPage, Output: OutputText,
		ClientStep: urlFromFile,
		Run:        runTabFind,
	})
}

// specialSchemes are the schemes whose URLs a browser parses with a host
// name, a port and dot segments (WHATWG URL, "special scheme"), each with
// its default port. "file" is left out: its host and path have other
// rules.
var specialSchemes = map[string]uint64{"http": 80, "https": 443, "ws": 80, "wss": 443, "ftp": 21}

// The prefilter of tab find (sameDocument) excludes a tab only on a part
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

// sameDocument reports whether a listed URL (without query and fragment)
// can be the page of target, the URL as the caller wrote it. It only
// narrows the tabs that tab find reads, so that it reads no tab of another
// site: a Safari tab that is not visible never answers a read. It
// excludes a tab only when a part that both URLs have in the safe form
// differs; the read inside the page decides.
func sameDocument(listed, target string) bool {
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

func runTabFind(ctx context.Context, env Env, in Input) (_ Result, err error) {
	if err := needURL(in, "tab find"); err != nil {
		return Result{}, err
	}
	raw := arg(in, 0) // the target as the caller wrote it
	// The errors of tab find name no target; the read of a page that
	// throws can repeat it, which a target of --url-file must not be.
	defer func() { err = hideFileURL(err, urlFile(in), raw) }()
	target, err := url.Parse(raw)
	if err != nil || target.Scheme == "" || target.Host == "" {
		// The error of url.Parse repeats the URL, which can be a secret.
		return Result{}, usagef("tab find: the URL is not an absolute URL")
	}
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	tabs, hidden, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in))
	if err != nil {
		return Result{}, err
	}
	// A hidden page can show the target: without it the command can prove
	// neither that no tab shows the target nor that a match is the only
	// one. The message names no page (design 2.5 rule 4).
	if hidden > 0 {
		return Result{}, &core.CodedError{Code: core.CodeReservation, Err: fmt.Errorf("tab find: the listing hides %d page(s) of a physical device without a reservation of your reservation owner, and any of them can show the target; reserve the device ('iwdp-cli device reserve') and run tab find again", hidden)}
	}
	var candidates []core.Tab // the tabs that can show the target
	for _, t := range tabs {
		if !sameDocument(t.URL, raw) {
			continue
		}
		if !t.Debuggable {
			return Result{}, fmt.Errorf("tab find: tab %s can show the target and cannot be read, because the browser does not offer it for debugging (another debugger may be attached); end that debugging session and run tab find again", t.ID)
		}
		candidates = append(candidates, t)
	}
	// The page compares its own href with the target's, both as the URL
	// parser of the page writes them. The page gets the target as the
	// caller wrote it: net/url writes some characters in another way than
	// a browser does ("|" in a path as "%7C"), and the page would compare
	// with another URL. A JSON string is a JavaScript string literal.
	lit, err := json.Marshal(raw)
	if err != nil {
		return Result{}, fmt.Errorf("tab find: writing the target for the page: %w", err)
	}
	expr := "location.href === new URL(" + string(lit) + ").href"
	// claim records each lease that a read of this command was granted
	// (core.LeaseClaim): a lease that the session held before, or that
	// another command of the session took meanwhile, is renewed by the
	// read and is not in it.
	var claim core.LeaseClaim
	ao := acquireFor(in)
	ao.Claim = &claim
	var (
		found   []core.Tab // the tabs that show the target
		readErr error
	)
	for _, t := range candidates {
		var same bool
		err := m.Do(ctx, tabRequest(in, t.Key), ao, "tab find", core.ClassRead, true, func(ctx context.Context, mo *core.ManagedOp) error {
			raw, err := mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
			if err != nil {
				return err
			}
			same, err = evalValue[bool](raw)
			return err
		})
		if err != nil {
			// A tab that could not be read is not a tab that shows another
			// page: the error keeps its exit code, which is never
			// ExitNoMatch.
			readErr = fmt.Errorf("reading tab %s: %w", t.ID, err)
			break
		}
		if same {
			found = append(found, t)
		}
	}
	// This is the one place that ends the leases that the reads were
	// granted, for every outcome. The session keeps the leases that it
	// held before and every lease that another command took. When the
	// command prints a tab (exit 0), the session also keeps the lease of
	// that tab, as after any other read of it: the tab stays the session's
	// until its first action, so no other session moves it off the target
	// in between. A command that fails keeps no lease that only it used.
	// The core decides, under the lock that grants and uses take, whether
	// a granted lease is still this command's alone (ReleaseClaimed): a
	// lease that another operation of the session used, uses or waits for
	// since the grant stays, and a command that comes during the release
	// gets a new lease.
	end := func(key, id string) error {
		if err := m.ReleaseClaimed(ctx, &claim, key); err != nil {
			return fmt.Errorf("releasing the lease that the read took on tab %s: %w", id, err)
		}
		return nil
	}
	tabIDs := map[string]string{} // page key -> tab id
	for _, t := range candidates {
		tabIDs[t.Key] = t.ID
	}
	keep := "" // the key of the tab that the command prints
	if readErr == nil && len(found) == 1 {
		keep = found[0].Key
	}
	ours := claim.Keys()
	var endErr error
	for _, key := range ours {
		if key != keep {
			endErr = errors.Join(endErr, end(key, tabIDs[key]))
		}
	}
	if endErr != nil && slices.Contains(ours, keep) {
		endErr = errors.Join(endErr, end(keep, tabIDs[keep]))
	}
	switch {
	case readErr != nil:
		return Result{}, errors.Join(readErr, endErr)
	case endErr != nil:
		return Result{}, endErr
	case len(found) == 0:
		return Result{}, &core.CodedError{Code: ExitNoMatch, Err: errors.New("no tab shows the target")}
	case len(found) == 1:
		return Result{Text: found[0].ID, Data: map[string]any{"tab": found[0].ID}}, nil
	}
	ids := make([]string, len(found))
	for i, t := range found {
		ids[i] = t.ID
	}
	return Result{}, fmt.Errorf("%d tabs show the target: %s", len(found), strings.Join(ids, ", "))
}
