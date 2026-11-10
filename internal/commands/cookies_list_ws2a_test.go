package commands

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// listCookiesCmd runs "cookies" with argv on page p.
func listCookiesCmd(t *testing.T, p *devtoolstest.Page, argv ...string) (Result, string) {
	t.Helper()
	res, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, append([]string{"cookies"}, argv...)...)
	if err != nil {
		t.Fatalf("cookies %v: %v", argv, err)
	}
	data, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("cookies %v: result data: %v", argv, err)
	}
	return res, string(data)
}

const listedSecret = "s3cr3t-session-token-1234567890"

// WS2a brief, section 2 (user-stated): "cookies" lists the cookies that
// Page.getCookies returns, with name, domain, path, expiry or session,
// httpOnly, secure and sameSite. Values are hidden by default and printed
// with --show-values. The line form is the legacy listing's (cmd/iwdp-cli
// formatCookie at f731071) with "(hidden)" in place of the value.
func TestCookiesListsAttributesAndHidesValues(t *testing.T) {
	p := devtoolstest.New(t, "https://example.com")
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	p.Cookies = []webkit.Cookie{
		{Name: "sid", Value: listedSecret, Domain: ".example.com", Path: "/", HTTPOnly: true, Secure: true, Session: true, SameSite: "Lax"},
		{Name: "pref", Value: "dark", Domain: "example.com", Path: "/app", Expires: float64(expires.UnixMilli()), SameSite: "Strict"},
	}
	res, data := listCookiesCmd(t, p)
	want := "sid=(hidden)  domain=.example.com path=/ sameSite=Lax httpOnly secure session\n" +
		"pref=(hidden)  domain=example.com path=/app sameSite=Strict expires=2030-01-02T03:04:05Z"
	if res.Text != want {
		t.Errorf("cookies text:\n%s\nwant:\n%s", res.Text, want)
	}
	if strings.Contains(res.Text+data, listedSecret) || strings.Contains(res.Text+data, "dark") {
		t.Errorf("cookies printed a value: text %q, data %s", res.Text, data)
	}
	wantData := `{"cookies":[` +
		`{"name":"sid","domain":".example.com","path":"/","expires":0,"session":true,"httpOnly":true,"secure":true,"sameSite":"Lax"},` +
		`{"name":"pref","domain":"example.com","path":"/app","expires":1893553445000,"session":false,"httpOnly":false,"secure":false,"sameSite":"Strict"}` +
		`],"valuesShown":false}`
	if data != wantData {
		t.Errorf("cookies data:\n%s\nwant:\n%s", data, wantData)
	}

	res, data = listCookiesCmd(t, p, "--show-values")
	if !strings.HasPrefix(res.Text, "sid="+listedSecret+"  domain=.example.com") || !strings.Contains(res.Text, "\npref=dark  domain=example.com") {
		t.Errorf("cookies --show-values text %q, want the values", res.Text)
	}
	if !strings.Contains(data, `"value":"`+listedSecret+`"`) || !strings.Contains(data, `"valuesShown":true`) {
		t.Errorf("cookies --show-values data %s, want the values", data)
	}
}

func TestCookiesWithoutCookies(t *testing.T) {
	p := devtoolstest.New(t, "https://example.com")
	res, data := listCookiesCmd(t, p)
	if res.Text != "No cookies." || data != `{"cookies":[],"valuesShown":false}` {
		t.Errorf("cookies on an empty jar: text %q, data %s", res.Text, data)
	}
}

// The listing is a registry command next to "cookies set" and "cookies
// clear": a read that may be retried, with --show-values its only flag.
func TestCookiesListIsARegistryCommand(t *testing.T) {
	c, n := Lookup([]string{"cookies"})
	if c == nil || n != 1 || c.Name != "cookies" {
		t.Fatal("cookies is not a registry command")
	}
	if c.Page != LeasedPage || c.Mutates || !c.RetrySafe || len(c.Args) != 0 || len(c.Flags) != 1 || c.Flags[0].Name != "show-values" {
		t.Errorf("cookies: page %v, mutates %v, retry-safe %v, args %v, flags %v", c.Page, c.Mutates, c.RetrySafe, c.Args, c.Flags)
	}
	if c, n := Lookup([]string{"cookies", "set"}); c == nil || n != 2 || c.Name != "cookies set" {
		t.Error("cookies set no longer resolves to its own command")
	}
}

// valueMark starts the generated cookie values that may hold any string.
// The other generated fields never hold it, so any mark in the output is
// a leaked value.
const valueMark = "§"

// cookieOctets are the characters of an ordinary cookie value (RFC 6265
// section 4.1.1, cookie-octet): printable ASCII without space, the double
// quote, the comma, the semicolon and the backslash.
const cookieOctets = "!#$%&'()*+-./0123456789:<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^_`abcdefghijklmnopqrstuvwxyz{|}~"

// cookieJar is a generated list of cookies (testing/quick Generator).
type cookieJar []webkit.Cookie

func (cookieJar) Generate(r *rand.Rand, size int) reflect.Value {
	word := func(alphabet string, min int) string {
		b := make([]byte, min+r.Intn(8))
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	sameSites := []string{"", "Lax", "Strict", "None"}
	jar := make(cookieJar, r.Intn(size+1))
	for i := range jar {
		// Half the values are ordinary ASCII cookie values, half any
		// string after valueMark (review ws2a-a F4: a leak of the values
		// without the mark must fail the property).
		value := word(cookieOctets, 1)
		if r.Intn(2) == 0 {
			v, _ := quick.Value(reflect.TypeFor[string](), r)
			value = valueMark + v.String()
		}
		jar[i] = webkit.Cookie{
			Name:     word("abcdefghijklmnopqrstuvwxyz_-0123456789", 1),
			Value:    value,
			Domain:   word(".abcdefghijklmnopqrstuvwxyz", 1),
			Path:     "/" + word("abcdefghijklmnopqrstuvwxyz/", 0),
			Expires:  float64(r.Int63n(4102444800000)),
			HTTPOnly: r.Intn(2) == 0,
			Secure:   r.Intn(2) == 0,
			Session:  r.Intn(2) == 0,
			SameSite: sameSites[r.Intn(len(sameSites))],
		}
	}
	return reflect.ValueOf(jar)
}

// withValues returns jar with the values of other (cycled), so that two
// jars differ only in their values.
func withValues(jar, other cookieJar) cookieJar {
	out := make(cookieJar, len(jar))
	for i, c := range jar {
		c.Value = valueMark
		if len(other) > 0 {
			c.Value = other[i%len(other)].Value
		}
		out[i] = c
	}
	return out
}

// renderCookies returns the text and the JSON data of the listing.
func renderCookies(t *testing.T, jar cookieJar, show bool) (string, string) {
	t.Helper()
	res := cookieListing(jar, show)
	data, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("cookie listing data: %v", err)
	}
	return res.Text, string(data)
}

// WS2a brief, section 2 (privacy invariant, user-stated): without
// --show-values no cookie value appears in the output, text or JSON. A
// short ordinary value can occur in the listing by chance (a value "a" in
// the domain "a.test"), so the check covers the values that the listing
// of the same cookies with the mark as every value does not contain. The
// property also holds for a part of a value (no mark appears) and for what
// a value could reveal otherwise: two jars that differ only in their
// values list the same. With --show-values every value is in the data,
// which shows that the generated values reach the listing.
func TestCookiesHideEveryValueProperty(t *testing.T) {
	prop := func(jar, other cookieJar) bool {
		text, data := renderCookies(t, jar, false)
		baseText, baseData := renderCookies(t, withValues(jar, nil), false)
		for _, c := range jar {
			if strings.Contains(baseText+baseData, c.Value) {
				continue // the value occurs in the listing without values
			}
			if strings.Contains(text, c.Value) || strings.Contains(data, c.Value) {
				t.Logf("value %q appears: text %q, data %s", c.Value, text, data)
				return false
			}
		}
		if strings.Contains(text+data, valueMark) {
			t.Logf("a part of a value appears: text %q, data %s", text, data)
			return false
		}
		if t2, d2 := renderCookies(t, withValues(jar, other), false); t2 != text || d2 != data {
			t.Logf("the listing depends on the values: %q / %s, and with other values %q / %s", text, data, t2, d2)
			return false
		}
		_, shown := renderCookies(t, jar, true)
		var got struct {
			Cookies []struct {
				Value *string `json:"value"`
			} `json:"cookies"`
		}
		if err := json.Unmarshal([]byte(shown), &got); err != nil || len(got.Cookies) != len(jar) {
			t.Logf("--show-values data %s: %v", shown, err)
			return false
		}
		for i, c := range got.Cookies {
			if c.Value == nil || *c.Value != jar[i].Value {
				t.Logf("--show-values cookie %d: value %v, want %q", i, c.Value, jar[i].Value)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
