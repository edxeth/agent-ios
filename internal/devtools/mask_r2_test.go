package devtools

import (
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"testing/quick"
)

// W4 round-2 masking gaps (repair reports
// /home/devkit/.pi/artifacts/worker/w4-r1-lib-repairs-20261107-045059.md
// and /home/devkit/.pi/artifacts/worker/w4-r1-cmd-20261107-061500.md,
// Watchouts): the password of a URL's user:password@ userinfo, and URLs
// in WebKit's error text of a request (design 7.9 and R17: secrets are
// masked by default).

func TestMaskURLMasksTheUserinfoPassword(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://user:hunter2@a.test/p?q=1#f", "https://user:%5Bmasked%5D@a.test/p?q=1#f"},
		{"https://user:@a.test/", "https://user:%5Bmasked%5D@a.test/"},
		// The userinfo ends at the authority's last "@" (WHATWG URL).
		{"https://u:p@ss@a.test/x", "https://u:%5Bmasked%5D@a.test/x"},
		{"https://u:pw@a.test/?access_token=t1", "https://u:%5Bmasked%5D@a.test/?access_token=%5Bmasked%5D"},
		// No password, or an "@" outside the authority: nothing to mask.
		{"https://user@a.test/", "https://user@a.test/"},
		{"https://a.test/users/@me:x", "https://a.test/users/@me:x"},
		{"https://a.test:8080/p", "https://a.test:8080/p"},
		{"https://a.test/r?next=https://u:pw@b.test/", "https://a.test/r?next=https://u:pw@b.test/"},
		{"mailto:u:pw@a.test", "mailto:u:pw@a.test"},
		// W4 round 3 (review finding mask.go:105): the authority as WHATWG
		// URL parsing finds it. Network-path references (a Location header
		// or redirectURL can hold one):
		{"//user:pw@a.test/callback", "//user:%5Bmasked%5D@a.test/callback"},
		{"///user:pw@a.test/", "///user:%5Bmasked%5D@a.test/"},
		{"\\\\u:pw@a.test/x", "\\\\u:%5Bmasked%5D@a.test/x"},
		{"/\\u:pw@a.test/x", "/\\u:%5Bmasked%5D@a.test/x"},
		{" //u:pw@a.test", "//u:%5Bmasked%5D@a.test"},
		{"//a.test/@me:x", "//a.test/@me:x"},
		// Special schemes take any number of slashes, also none.
		{"https:u:pw@a.test/", "https:u:%5Bmasked%5D@a.test/"},
		{"HTTPS:/u:pw@a.test", "HTTPS:/u:%5Bmasked%5D@a.test"},
		{"https:\\\\u:pw@a.test/", "https:\\\\u:%5Bmasked%5D@a.test/"},
		// Tabs and line breaks are dropped by the parser.
		{"https://u:p\tw@a.test/", "https://u:%5Bmasked%5D@a.test/"},
		{"htt\nps://u:pw@a.test/", "https://u:%5Bmasked%5D@a.test/"},
		// Other schemes have an authority only after "//".
		{"foo://u:pw@a.test/x", "foo://u:%5Bmasked%5D@a.test/x"},
		{"foo:u:pw@a.test", "foo:u:pw@a.test"},
		// A path has no authority; a URL it quotes is masked.
		{"/a/@b:c", "/a/@b:c"},
		{"/next/https://u:pw@b.test/", "/next/https://u:%5Bmasked%5D@b.test/"},
	} {
		if got := MaskURL(tc.in); got != tc.want {
			t.Errorf("MaskURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// genText is a random string over an alphabet with the URL delimiters.
func genText(r *rand.Rand, max int) string {
	const alphabet = "abcXYZ019 :@/?#%&=;+-._~!$'()*,[]\\é"
	runes := []rune(alphabet)
	var b strings.Builder
	for range r.Intn(max + 1) {
		b.WriteRune(runes[r.Intn(len(runes))])
	}
	return b.String()
}

// Property (RFC 3986 userinfo, WHATWG URL): for any user name and
// password, MaskURL of a URL that net/url wrote gives a URL that parses to
// the same user name, host, path and query and the password Masked.
func TestMaskURLUserinfoProperty(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		u := url.URL{
			Scheme: "https", User: url.UserPassword(genText(r, 8), genText(r, 12)), Host: "a.test:8443",
			Path: "/" + genText(r, 8), RawQuery: url.Values{"q": {genText(r, 6)}}.Encode(),
		}
		raw := u.String()
		got, err := url.Parse(MaskURL(raw))
		if err != nil {
			t.Logf("seed %d: MaskURL(%q) does not parse: %v", seed, raw, err)
			return false
		}
		pw, ok := got.User.Password()
		if !ok || pw != Masked || got.User.Username() != u.User.Username() || got.Host != u.Host || got.Path != u.Path || got.RawQuery != u.RawQuery {
			t.Logf("seed %d: MaskURL(%q) = %q", seed, raw, got)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
}

// Property (WHATWG URL parsing, W4 round 3 review finding mask.go:105):
// for any user name and password and every form of a URL with an
// authority (absolute with any slashes for a special scheme, network-path
// with "/" or "\"), MaskURL keeps everything but the password, which
// becomes Masked. The "//" form is also checked with net/url, which parses
// network-path references.
func TestMaskURLUserinfoFormsProperty(t *testing.T) {
	prefixes := []string{"//", "///", "\\\\", "/\\", "https://", "https:", "https:/", "https:\\\\", "HTTP://", "wss://", "foo://", "  //"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		userinfo := url.UserPassword(genText(r, 8), genText(r, 12)).String()
		user, _, _ := strings.Cut(userinfo, ":")
		path := (&url.URL{Path: "/" + genText(r, 8)}).EscapedPath()
		query := url.Values{"q": {genText(r, 6)}}.Encode()
		for _, prefix := range prefixes {
			raw := prefix + userinfo + "@a.test:8443" + path + "?" + query
			want := strings.TrimLeft(prefix, " ") + user + ":" + maskedEscaped + "@a.test:8443" + path + "?" + query
			got := MaskURL(raw)
			if got != want {
				t.Logf("seed %d: MaskURL(%q) = %q, want %q", seed, raw, got, want)
				return false
			}
			if prefix == "//" {
				u, err := url.Parse(got)
				if err != nil {
					t.Logf("seed %d: MaskURL(%q) = %q does not parse: %v", seed, raw, got, err)
					return false
				}
				if pw, ok := u.User.Password(); !ok || pw != Masked || u.Host != "a.test:8443" {
					t.Logf("seed %d: MaskURL(%q) = %q", seed, raw, got)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestMaskTextMasksURLsInErrorText(t *testing.T) {
	in := "Failed to load https://u:pw-secret@a.test/cb?access_token=tok-secret&x=1: refused (see http://b.test/?q=ok)"
	want := "Failed to load https://u:%5Bmasked%5D@a.test/cb?access_token=%5Bmasked%5D&x=1: refused (see http://b.test/?q=ok)"
	if got := MaskText(in); got != want {
		t.Fatalf("MaskText = %q, want %q", got, want)
	}
	if got := MaskText("Cancelled load because it violates the resource's Content Security Policy."); got != "Cancelled load because it violates the resource's Content Security Policy." {
		t.Fatalf("text without a URL changed: %q", got)
	}
}

// Property: in any text around a URL, MaskText removes the URL's
// password and secret query values and keeps the text around it and every
// other query value.
func TestMaskTextProperty(t *testing.T) {
	words := []string{"Failed", "to", "load", "(", ")", ":", "error", "'", "\"", "<", ">", "https", "\u00e9"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		phrase := func() string {
			var w []string
			for range r.Intn(5) {
				w = append(w, words[r.Intn(len(words))])
			}
			return strings.Join(w, " ")
		}
		pass, token := "pw"+genAlnum(r), "tk"+genAlnum(r)
		raw := "https://user:" + pass + "@a.test/p?access_token=" + token + "&page=7"
		before, after := phrase(), phrase()
		text := before + " " + raw + " " + after
		out := MaskText(text)
		if strings.Contains(out, pass) || strings.Contains(out, token) || !strings.Contains(out, "page=7") ||
			!strings.HasPrefix(out, before+" ") || !strings.HasSuffix(out, " "+after) {
			t.Logf("seed %d: MaskText(%q) = %q", seed, text, out)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func genAlnum(r *rand.Rand) string {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6+r.Intn(10))
	for i := range b {
		b[i] = a[r.Intn(len(a))]
	}
	return string(b)
}

// The HAR comment of a failed request carries WebKit's error text; by
// default its URLs are masked, with ShowValues they are kept.
func TestHARMasksURLsInTheErrorText(t *testing.T) {
	req := Request{
		ID: "r1", Seq: 1, Method: "GET", URL: "https://a.test/x", State: StateFailed,
		Error: "Failed to load https://u:pw-secret@a.test/cb?access_token=tok-secret",
	}
	masked := BuildHAR([]Request{req}, nil, nil, HAROptions{}).Log.Entries[0].Comment
	if strings.Contains(masked, "pw-secret") || strings.Contains(masked, "tok-secret") {
		t.Fatalf("default HAR comment %q has a secret of the error text", masked)
	}
	if shown := BuildHAR([]Request{req}, nil, nil, HAROptions{ShowValues: true}).Log.Entries[0].Comment; !strings.Contains(shown, "tok-secret") {
		t.Fatalf("HAR comment with ShowValues %q lost the error text", shown)
	}
}
