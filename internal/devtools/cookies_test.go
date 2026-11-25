package devtools

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// importedSession is a session cookie as the importer builds it: path "/",
// SameSite Lax (tools.DefaultSameSite), no expiry.
func importedSession(name, value, domain string) webkit.Cookie {
	return webkit.Cookie{Name: name, Value: value, Domain: domain, Path: "/", Session: true, SameSite: "Lax"}
}

// agent-browser "cookies set --curl <file>" (commands.md, "Cookie import"):
// the file is a JSON array of {name, value}, a cURL command copied from
// DevTools, or a bare Cookie header; --domain scopes the cookies.
func TestParseCookieImportFormats(t *testing.T) {
	cases := []struct {
		name, data, domainFlag, pageHost, format string
		want                                     []webkit.Cookie
	}{
		{
			"header", "Cookie: a=1; b=x=y\n", "", "page.test", "header",
			[]webkit.Cookie{importedSession("a", "1", "page.test"), importedSession("b", "x=y", "page.test")},
		},
		{
			"bare header with --domain", "a=1;b=2", "flag.test", "page.test", "header",
			[]webkit.Cookie{importedSession("a", "1", "flag.test"), importedSession("b", "2", "flag.test")},
		},
		{
			"curl single quotes", "curl 'https://api.example.com/v1?x=1' \\\n  -H 'accept: */*' \\\n  -H 'cookie: sid=abc; theme=dark' \\\n  --compressed", "", "page.test", "curl",
			[]webkit.Cookie{importedSession("sid", "abc", "api.example.com"), importedSession("theme", "dark", "api.example.com")},
		},
		{
			"curl -b and ansi-c quoting", "curl $'https://a.test/' -b $'k=it\\'s' -H \"Cookie: q=\\\"1\\\"\"", "", "", "curl",
			[]webkit.Cookie{importedSession("k", "it's", "a.test"), importedSession("q", "\"1\"", "a.test")},
		},
		{
			"curl with --domain", "curl https://a.test/ --cookie 'z=9'", "flag.test", "", "curl",
			[]webkit.Cookie{importedSession("z", "9", "flag.test")},
		},
		{
			"json array", `[{"name":"a","value":"1"},{"name":"b","value":"2","domain":".own.test","path":"/p","expires":1800000000,"httpOnly":true,"secure":true,"sameSite":"strict"}]`, "", "page.test", "json",
			[]webkit.Cookie{
				importedSession("a", "1", "page.test"),
				{Name: "b", Value: "2", Domain: ".own.test", Path: "/p", Expires: 1800000000000, HTTPOnly: true, Secure: true, SameSite: "Strict"},
			},
		},
		{
			"json object with cookies (state file)", `{"cookies":[{"name":"a","value":"1","domain":"s.test","path":"/","expires":-1,"sameSite":"None","secure":true}]}`, "", "", "json",
			[]webkit.Cookie{{Name: "a", Value: "1", Domain: "s.test", Path: "/", Session: true, Secure: true, SameSite: "None"}},
		},
	}
	for _, c := range cases {
		got, err := ParseCookieImport([]byte(c.data), c.domainFlag, c.pageHost)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Format != c.format || !reflect.DeepEqual(got.Cookies, c.want) {
			t.Errorf("%s:\n got %s %+v\nwant %s %+v", c.name, got.Format, got.Cookies, c.format, c.want)
		}
	}
}

// Errors name the problem and never echo a cookie value (agent-browser:
// "Errors never echo cookie values"; design R17).
func TestParseCookieImportErrorsHideValues(t *testing.T) {
	const secret = "s3cr3tVALUE"
	cases := []struct{ name, data, domain, want string }{
		{"no domain", "a=" + secret, "", "--domain"},
		{"pair without name", "a=1; =" + secret, "x.test", "pair 2"},
		{"pair without =", "a=1; " + secret, "x.test", "pair 2"},
		{"bad json", `[{"name":"a","value":"` + secret + `"`, "x.test", "JSON"},
		{"json without name", `[{"value":"` + secret + `"}]`, "x.test", "cookie 1"},
		{"json bad sameSite", `[{"name":"a","value":"` + secret + `","sameSite":"sometimes"}]`, "x.test", "sameSite"},
		{"curl without cookies", "curl 'https://a.test/' -H 'x-token: " + secret + "'", "", "no Cookie header"},
		{"curl cookie file", "curl https://a.test/ -b jar.txt", "", "cookie file"},
		{"curl unterminated quote", "curl 'https://a.test/' -H 'cookie: a=" + secret, "", "quote"},
		{"two header lines", "a=1\nb=" + secret, "x.test", "one line"},
		{"empty", "  \n", "x.test", "empty"},
	}
	for _, c := range cases {
		_, err := ParseCookieImport([]byte(c.data), c.domain, "")
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error %q echoes the cookie value", c.name, err)
		}
	}
}

// cookieOctets are the characters RFC 6265 section 4.1.1 allows in a
// cookie value; tokenChars are a subset of the RFC 7230 token characters
// allowed in a cookie name.
const (
	cookieOctets = "!#$%&'()*+-./0123456789:<=>?@ABCXYZ[]^_`abcxyz{|}~"
	tokenChars   = "!#$%&'*+-.^_`|~0123456789ABCXYZabcxyz"
)

func randFrom(r *rand.Rand, chars string, min, max int) string {
	n := min + r.Intn(max-min+1)
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// shellQuote quotes s for a POSIX shell in single quotes.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Property (parsing and encoding): any list of name=value pairs written as
// a Cookie header, as a cURL command (header or -b, shell-quoted) or as a
// JSON array parses back to the same pairs, in order.
func TestParseCookieImportRoundTripProperty(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(5)
		var want []webkit.Cookie
		var pairs []string
		var objs []map[string]string
		for i := 0; i < n; i++ {
			name, value := randFrom(r, tokenChars, 1, 8), randFrom(r, cookieOctets, 0, 12)
			want = append(want, importedSession(name, value, "rt.test"))
			pairs = append(pairs, name+"="+value)
			objs = append(objs, map[string]string{"name": name, "value": value})
		}
		header := strings.Join(pairs, "; ")
		js, _ := json.Marshal(objs)
		inputs := map[string]string{
			"header":      "Cookie: " + header,
			"bare":        header,
			"curl -H":     "curl " + shellQuote("https://rt.test/p") + " -H " + shellQuote("Cookie: "+header) + " --compressed",
			"curl -b":     "curl -b " + shellQuote(header) + " " + shellQuote("https://rt.test/"),
			"json":        string(js),
			"curl dquote": "curl \"https://rt.test/\" -H \"cookie: " + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(header) + "\"",
		}
		for kind, in := range inputs {
			got, err := ParseCookieImport([]byte(in), "", "rt.test")
			if err != nil || !reflect.DeepEqual(got.Cookies, want) {
				fmt.Printf("%s %q: %v\n got %+v\nwant %+v\n", kind, in, err, got.Cookies, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

func TestCookieURLAndDomainMatch(t *testing.T) {
	cases := []struct {
		c    webkit.Cookie
		want string
	}{
		{webkit.Cookie{Domain: ".a.test", Path: "/x"}, "http://a.test/x"},
		{webkit.Cookie{Domain: "a.test", Path: "", Secure: true}, "https://a.test/"},
	}
	for _, c := range cases {
		if got := CookieURL(c.c); got != c.want {
			t.Errorf("CookieURL(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
	// RFC 6265 section 5.1.3 domain matching; a leading dot is ignored
	// (section 5.2.3).
	match := []struct {
		host, domain string
		want         bool
	}{
		{"a.test", "a.test", true},
		{"www.a.test", ".a.test", true},
		{"www.a.test", "a.test", true},
		{"a.test", "www.a.test", false},
		{"xa.test", "a.test", false},
		{"A.TEST", "a.test", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"a.test", "", false},
	}
	for _, m := range match {
		if got := DomainMatch(m.host, m.domain); got != m.want {
			t.Errorf("DomainMatch(%q, %q) = %v, want %v", m.host, m.domain, got, m.want)
		}
	}
}

// RFC 6265 section 5.1.4 path matching: the same path, a path below a
// cookie path that ends in "/", or a path below it at a "/" boundary.
func TestPathMatch(t *testing.T) {
	for _, m := range []struct {
		req, cookie string
		want        bool
	}{
		{"/dc4", "/dc4", true},
		{"/dc4/x", "/dc4", true},
		{"/dc4/x", "/dc4/", true},
		{"/dc4x", "/dc4", false},
		{"/", "/dc4", false},
		{"/dc", "/dc4", false},
		{"", "/", true},
		{"/anything", "/", true},
	} {
		if got := PathMatch(m.req, m.cookie); got != m.want {
			t.Errorf("PathMatch(%q, %q) = %v, want %v", m.req, m.cookie, got, m.want)
		}
	}
	// For any segment a (not empty) and b without "/": "/a/b" is below the
	// cookie path "/a", and "/ab" is a sibling of it unless b is empty. (The
	// cookie path "/" is below no boundary: every path matches it.)
	seg := func(s string) string { return strings.ReplaceAll(s, "/", "") }
	f := func(a, b string) bool {
		a, b = "x"+seg(a), seg(b)
		p := "/" + a
		return PathMatch(p+"/"+b, p) && PathMatch(p+b, p) == (b == "")
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
