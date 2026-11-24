package commands

import (
	"errors"
	"testing"
)

// The error line names the command once, also when the error already
// starts with its name (live run 18: "Error: tab find: tab find: ...").
func TestErrorLineNamesTheCommandOnce(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"tab find: the listing hides 1 page(s)", "Error: tab find: the listing hides 1 page(s)"},
		{"no matching page", "Error: tab find: no matching page"},
		{"tab finder: x", "Error: tab find: tab finder: x"},
	} {
		if got := errorLine("tab find", errors.New(tc.err)); got != tc.want {
			t.Errorf("errorLine(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
