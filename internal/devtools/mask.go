package devtools

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Masked replaces a secret value in HAR files, state files and listings
// (design 7.9 and R17: password and token fields are masked by default).
const Masked = "[masked]"

// secretHeaders are the header names whose values are always secrets.
var secretHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "x-auth-token": true, "x-csrf-token": true, "x-xsrf-token": true,
}

// publicValues are header values that a secret name part (secretParts)
// would mask although they carry no secret: Access-Control-Allow-Credentials
// has the one valid value "true" (Fetch standard), and masking it hides
// the answer to a CORS question. Any other value of that header, also in a
// request, is masked as before.
var publicValues = map[string]string{"access-control-allow-credentials": "true"}

// secretParts are lower-case name parts that mark a password or token
// field (form fields, JSON keys, query parameters, storage keys, cookies).
var secretParts = []string{
	"password", "passwd", "passphrase", "pwd", "token", "secret", "apikey", "api_key", "api-key",
	"authorization", "credential", "session", "sessid", "jwt", "otp",
}

// SecretName reports whether a field, key, parameter or cookie name names
// a password or token.
func SecretName(name string) bool {
	n := strings.ToLower(name)
	if n == "sid" || secretHeaders[n] {
		return true
	}
	for _, p := range secretParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// urlHeaders are the header names whose values are a URL, which can carry
// secret query parameters (a redirect to an OAuth callback).
var urlHeaders = map[string]bool{"location": true, "content-location": true, "referer": true}

// MaskHeaders returns h with the values of secret headers masked, and the
// secret query parameters of URL-valued headers (Location,
// Content-Location, Referer, the url= part of Refresh) masked as in MaskURL.
func MaskHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		switch {
		case publicValues[lk] != "" && v == publicValues[lk]:
		case secretHeaders[lk] || SecretName(k):
			v = Masked
		case urlHeaders[lk]:
			v = MaskURL(v)
		case lk == "refresh":
			if i := strings.Index(strings.ToLower(v), "url="); i >= 0 {
				v = v[:i+len("url=")] + MaskURL(v[i+len("url="):])
			}
		}
		out[k] = v
	}
	return out
}

// maskedEscaped is Masked as a query or fragment value.
var maskedEscaped = url.QueryEscape(Masked)

// MaskURL masks the password of the userinfo (user:password@), the values
// of secret query parameters, and replaces the fragment with Masked when it
// holds one (OAuth implicit-flow tokens). It works on the raw text, so a
// URL that net/url cannot parse, or a pair it would reject, is masked too;
// every other byte stays as it was.
func MaskURL(raw string) string {
	rest, frag, hasFrag := strings.Cut(raw, "#")
	base, query, hasQuery := strings.Cut(rest, "?")
	out := maskUserinfo(base)
	if hasQuery {
		masked, _ := maskPairs(query)
		out += "?" + masked
	}
	if hasFrag {
		if _, secret := maskPairs(frag); secret {
			frag = maskedEscaped
		}
		out += "#" + frag
	}
	return out
}

// specialSchemes are the WHATWG URL special schemes: their URLs always
// have an authority, after any number of "/" or "\" (also none).
var specialSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "ftp": true, "file": true}

// urlScheme matches a URL scheme and its colon.
var urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// maskUserinfo replaces the password in the userinfo of a URL without its
// query and fragment with Masked. It finds the authority as WHATWG URL
// parsing does (W4 repair round 3, review finding mask.go:105):
//   - leading spaces and control characters are skipped, and tabs and
//     line breaks anywhere are dropped (the parser removes them);
//   - a special scheme (http, https, ws, wss, ftp, file) is followed by
//     the authority after any number of "/" or "\" ("https:u:p@host" and
//     "https:\\u:p@host" have one);
//   - another scheme has an authority only after "//";
//   - a reference without a scheme has one after two "/" or "\" (a
//     network-path reference such as "//user:password@host/path", which a
//     Location header can hold), and none after one ("/a/@b:c");
//   - a value without an authority of its own masks the URL that it
//     quotes after its first "://" ("/next/https://u:p@host").
//
// The authority ends at the first "/" or "\" (only "/" for a non-special
// scheme), and the userinfo at the authority's last "@". A user name
// without a password is kept. Only a URL whose password is masked loses
// its tabs and line breaks; every other URL is returned as it was.
func maskUserinfo(base string) string {
	u := strings.TrimLeftFunc(base, func(r rune) bool { return r <= ' ' })
	if strings.ContainsAny(u, "\t\n\r") {
		u = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(u)
	}
	isSlash := func(c byte) bool { return c == '/' || c == '\\' }
	start, ends := -1, "/\\"
	if m := urlScheme.FindString(u); m != "" {
		rest := u[len(m):]
		switch {
		case specialSchemes[strings.ToLower(m[:len(m)-1])]:
			start = len(m)
			for start < len(u) && isSlash(u[start]) {
				start++
			}
		case strings.HasPrefix(rest, "//"):
			start, ends = len(m)+2, "/"
		}
	} else if len(u) >= 2 && isSlash(u[0]) && isSlash(u[1]) {
		start = 2
		for start < len(u) && isSlash(u[start]) {
			start++
		}
	}
	if start < 0 {
		// No authority of its own (a path, or a value that is no URL):
		// as before round 3, a URL quoted in it after "://" is masked.
		i := strings.Index(u, "://")
		if i < 0 {
			return base
		}
		start = i + len("://")
	}
	auth := u[start:]
	if j := strings.IndexAny(auth, ends); j >= 0 {
		auth = auth[:j]
	}
	at := strings.LastIndex(auth, "@")
	if at < 0 {
		return base
	}
	user, _, hasPassword := strings.Cut(auth[:at], ":")
	if !hasPassword {
		return base
	}
	return u[:start] + user + ":" + maskedEscaped + u[start+at:]
}

// urlInText finds absolute URLs in free text: a scheme, "://" and every
// byte up to white space, a quote or an angle bracket.
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// MaskText masks every absolute URL in free text as MaskURL does. It is
// for text that can quote a URL, such as WebKit's error text of a request
// (Request.Error).
func MaskText(s string) string { return urlInText.ReplaceAllStringFunc(s, MaskURL) }

// MaskForm masks secret fields of an application/x-www-form-urlencoded body.
func MaskForm(body string) string {
	out, _ := maskPairs(body)
	return out
}

// maskPairs masks the values of the name=value pairs of s whose name
// (unescaped when it can be) is a SecretName, and reports whether it
// masked one. Pairs are separated by "&" or ";" (some servers read ";" as
// a separator; a split inside a value only masks more). Everything else is
// kept byte for byte, also pairs that do not unescape.
func maskPairs(s string) (string, bool) {
	var b strings.Builder
	masked := false
	for s != "" {
		pair, sep := s, ""
		if i := strings.IndexAny(s, "&;"); i >= 0 {
			pair, sep, s = s[:i], s[i:i+1], s[i+1:]
		} else {
			s = ""
		}
		if name, _, ok := strings.Cut(pair, "="); ok {
			n, err := url.QueryUnescape(name)
			if err != nil {
				n = name
			}
			if SecretName(n) {
				pair, masked = name+"="+maskedEscaped, true
			}
		}
		b.WriteString(pair)
		b.WriteString(sep)
	}
	return b.String(), masked
}

// MaskJSON masks the values of secret keys anywhere in a JSON document. A
// value that is not JSON is returned unchanged with ok false.
func MaskJSON(text string) (string, bool) {
	var v any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return text, false
	}
	if dec.More() {
		return text, false
	}
	out, err := json.Marshal(maskValue(v))
	if err != nil {
		return text, false
	}
	return string(out), true
}

func maskValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if SecretName(k) {
				t[k] = maskAll(x)
			} else {
				t[k] = maskValue(x)
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = maskValue(t[i])
		}
		return t
	}
	return v
}

// maskAll masks the value of a secret key: every scalar in it becomes
// Masked; objects and arrays keep their shape (keys and lengths).
func maskAll(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = maskAll(x)
		}
		return t
	case []any:
		for i := range t {
			t[i] = maskAll(t[i])
		}
		return t
	}
	return Masked
}

// MaskBody masks a request or response body by its MIME type: form fields
// and JSON keys. Other bodies are returned unchanged.
func MaskBody(mimeType, text string) string {
	mt := strings.ToLower(mimeType)
	switch {
	case strings.Contains(mt, "x-www-form-urlencoded"):
		return MaskForm(text)
	case strings.Contains(mt, "json"):
		out, _ := MaskJSON(text)
		return out
	}
	if out, ok := MaskJSON(text); ok {
		return out
	}
	return text
}

// WritePrivateFile writes data to path with mode 0600, also when the file
// exists with a wider mode (HAR and state files hold page data; design 7.9).
func WritePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, errors.Join(err, f.Close()))
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
