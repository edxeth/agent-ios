package e2e_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	cliBinariesMu sync.Mutex
	cliBinaries   = map[string]string{} // listing address -> agent-ios binary
)

func buildCLI(t *testing.T) string {
	t.Helper()
	return buildCLIFor(t, deadCLIListing().Addr())
}

// buildCLIFor builds agent-ios once per iwdp listing address addr, a
// private loopback address of these tests (listingLDFlags).
func buildCLIFor(t *testing.T, addr string) string {
	t.Helper()
	cliBinariesMu.Lock()
	defer cliBinariesMu.Unlock()
	if bin, ok := cliBinaries[addr]; ok {
		return bin
	}
	dir, err := os.MkdirTemp("", "iwdp-e2e-cli-*")
	if err != nil {
		t.Fatalf("building agent-ios: %v", err)
	}
	trackTempDir(dir)
	bin := filepath.Join(dir, "agent-ios")
	args := append([]string{"build", "-ldflags", listingLDFlags(addr)}, coverBuildFlags(t)...)
	cmd := exec.Command("go", append(args, "-o", bin, "../cmd/agent-ios")...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building agent-ios for the listing %s: %v", addr, err)
	}
	cliBinaries[addr] = bin
	return bin
}

func TestCLI_Help(t *testing.T) {
	bin := buildCLI(t)

	out, err := exec.Command(bin, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-ios help: %v\n%s", err, out)
	}

	output := string(out)
	expected := []string{
		"agent-ios",
		"Usage:",
		"devices",
		"eval",
		"open",
		"screenshot",
		"cookies",
		"console",
		"network requests",
	}
	for _, s := range expected {
		if !strings.Contains(output, s) {
			t.Errorf("help output missing %q", s)
		}
	}
}

func TestCLI_HelpFlag(t *testing.T) {
	bin := buildCLI(t)

	out, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-ios --help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Error("--help output missing Usage:")
	}
}

func TestCLI_HFlagShort(t *testing.T) {
	bin := buildCLI(t)

	out, err := exec.Command(bin, "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-ios -h: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Error("-h output missing Usage:")
	}
}

func TestCLI_NoArgs(t *testing.T) {
	bin := buildCLI(t)

	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code when called with no args")
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("no-args output should show usage, got: %s", out)
	}
}

func TestCLI_UnknownCommand(t *testing.T) {
	bin := buildCLI(t)

	cmd := exec.Command(bin, "nonexistent")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code for unknown command")
	}
	output := string(out)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(output, `unknown command "nonexistent"`) {
		t.Errorf("unknown command: %v, want exit 2 naming the command, got: %s", err, output)
	}
}

// eval is a registry command (WS2a): without its expression it is the
// registry's usage error, exit 2, which names the missing argument and the
// command's help.
func TestCLI_EvalNoArgs(t *testing.T) {
	bin := buildCLI(t)

	cmd := exec.Command(bin, "eval")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("eval without expression: %v, want exit 2; output %s", err, out)
	}
	if !strings.Contains(string(out), "missing argument <expression>") || !strings.Contains(string(out), "agent-ios eval --help") {
		t.Errorf("eval with no args should name the missing expression and the help, got: %s", out)
	}
}

func TestCLI_NavigateNoArgs(t *testing.T) {
	bin := buildCLI(t)

	cmd := exec.Command(bin, "navigate")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code for navigate without URL")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "missing argument <url>") {
		t.Errorf("navigate with no args: %v, want the usage error (exit 2) naming <url>, got: %s", err, out)
	}
}

func TestCLI_DevicesWithoutIWDP(t *testing.T) {
	bin := buildCLI(t)

	// Devices command without iwdp running should give a helpful error.
	// The binary reads the dead listing of these tests (deadCLIListing),
	// never the user's iwdp.
	listing := deadCLIListing()
	before := listing.hits.Load()
	cmd := exec.Command(bin, "devices")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("devices succeeded without iwdp: %s", out)
	}
	output := string(out)
	if !strings.Contains(output, "ios_webkit_debug_proxy") {
		t.Errorf("devices error should mention ios_webkit_debug_proxy, got: %s", output)
	}
	if listing.hits.Load() == before {
		t.Errorf("devices did not check the private listing %s", listing.Addr())
	}
}

// The overview lists the registry commands, also those that replace the
// removed commands of earlier versions (docs/migration.md).
func TestCLI_HelpNewCommands(t *testing.T) {
	bin := buildCLI(t)

	out, err := exec.Command(bin, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-ios help: %v\n%s", err, out)
	}
	output := string(out)

	cmds := []string{
		"status", "restart-iwdp", "devices", "reload", "click", "fill", "type",
		"cookies set", "cookies clear", "console clear", "network request",
		"set headers", "emulate network", "highlight", "profiler start", "trace start",
		"storage", "get", "find", "tab list",
	}
	for _, c := range cmds {
		if !strings.Contains(output, "  "+c+" ") {
			t.Errorf("help output missing command %q", c)
		}
	}
}

// Commands that require arguments exit 2 without them and name the
// missing argument and the command's help.
func TestCLI_CommandsRequiringArgs(t *testing.T) {
	bin := buildCLI(t)

	for _, name := range []string{"click", "fill", "open", "eval", "network route"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(bin, strings.Fields(name)...).CombinedOutput()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Fatalf("%s with no args: %v, want exit 2; output %s", name, err, out)
			}
			if output := string(out); !strings.Contains(output, "missing argument <") || !strings.Contains(output, "--help") {
				t.Errorf("%s with no args should name the missing argument and the help, got: %s", name, output)
			}
		})
	}
}
