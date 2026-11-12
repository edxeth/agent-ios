package devtools

import "testing"

// MaskHeaders masks the values of secret headers and keeps the others. The
// device check of 2026-11-11 showed Access-Control-Allow-Credentials:
// true as [masked], because its name holds "credential": the Fetch
// standard allows only the value "true" there, so it carries no secret.
func TestMaskHeadersKeepsPublicHeaders(t *testing.T) {
	in := map[string]string{
		"Authorization":                    "Bearer abc",
		"Cookie":                           "sid=1",
		"X-Session-Id":                     "s-1",
		"X-Client-Credential":              "c-1",
		"Access-Control-Allow-Credentials": "true",
		"Access-Control-Allow-Origin":      "https://httpbin.org",
		"Content-Type":                     "application/json",
	}
	want := map[string]string{
		"Authorization":                    Masked,
		"Cookie":                           Masked,
		"X-Session-Id":                     Masked,
		"X-Client-Credential":              Masked,
		"Access-Control-Allow-Credentials": "true",
		"Access-Control-Allow-Origin":      "https://httpbin.org",
		"Content-Type":                     "application/json",
	}
	got := MaskHeaders(in)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("MaskHeaders: %s = %q, want %q", k, got[k], v)
		}
	}
	// Only the standard value is shown: any other value of that header,
	// in a request or a nonconforming response, may be a credential.
	for _, v := range []string{"secret-token-123", "TRUE", "true, token"} {
		if got := MaskHeaders(map[string]string{"Access-Control-Allow-Credentials": v}); got["Access-Control-Allow-Credentials"] != Masked {
			t.Errorf("MaskHeaders: Access-Control-Allow-Credentials %q = %q, want %q", v, got["Access-Control-Allow-Credentials"], Masked)
		}
	}
}
