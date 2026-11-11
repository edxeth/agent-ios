package devtools

import (
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Test support of this package's tests only: nothing in the daemon uses
// it, so it lives here and not in the production files.

// CookieURL is a URL to which the browser sends c: its domain without the
// leading dot, its path, and https for a secure cookie.
func CookieURL(c webkit.Cookie) string {
	scheme := "http"
	if c.Secure {
		scheme = "https"
	}
	path := c.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + strings.TrimPrefix(c.Domain, ".") + path
}
