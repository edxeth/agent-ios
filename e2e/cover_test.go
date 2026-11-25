package e2e_test

import (
	"flag"
	"os"
	"sync"
	"testing"
)

// coverModule instruments every package of the module in a built binary,
// as the quality gate's -coverpkg=./... does in the test binaries.
const coverModule = "github.com/edxeth/agent-ios/..."

var (
	coverEnvOnce sync.Once
	coverEnvErr  error
)

// coverBuildFlags returns the go build flags for a binary of these tests.
// When this test binary measures coverage (go test -cover, or the quality
// gate's run with -test.gocoverdir), the binary is instrumented the same
// way and writes its counters to the same directory: GOCOVERDIR, which the
// binaries inherit. So code that runs only in a built binary, such as the
// legacy listing commands, counts in the coverage report. Without
// coverage it returns nil, and the binary is built as before.
func coverBuildFlags(t *testing.T) []string {
	t.Helper()
	mode := testing.CoverMode()
	f := flag.Lookup("test.gocoverdir")
	if mode == "" || f == nil || f.Value.String() == "" {
		return nil
	}
	coverEnvOnce.Do(func() { coverEnvErr = os.Setenv("GOCOVERDIR", f.Value.String()) })
	if coverEnvErr != nil {
		t.Fatalf("setting GOCOVERDIR for the built binaries: %v", coverEnvErr)
	}
	return []string{"-cover", "-covermode=" + mode, "-coverpkg=" + coverModule}
}
