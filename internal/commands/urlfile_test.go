package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of the file checks of --url-file (urlfile.go). The tests of the
// content (trimming, white space, empty file) and its property test are in
// private_test.go.

// urlFileCommandTimeout is the command deadline of the regression test of F27,
// as in "agent-ios --timeout 10ms open --url-file <fifo>".
const urlFileCommandTimeout = 10 * time.Millisecond

// urlFileDeadlineMargin is the time that a wait of these tests leaves before the
// test's own deadline (go test -timeout), so that the test reports the
// hang itself before the test binary panics.
const urlFileDeadlineMargin = time.Second

// urlFileStep parses "<name> --url-file <path>" and runs the command's
// ClientStep under the command deadline urlFileCommandTimeout. It waits for the
// step until shortly before the test's own deadline and fails the test
// when the step is still running then: a step that blocks on the file
// does not end at the command deadline. Without a test deadline (go test
// -timeout 0) it waits without a bound.
func urlFileStep(t *testing.T, name, path string) (*Input, error) {
	t.Helper()
	inv, err := Parse(append(strings.Fields(name), "--url-file", path), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), urlFileCommandTimeout)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- inv.Cmd.ClientStep(ctx, &inv.Input, nil) }()
	var bound <-chan time.Time
	if d, ok := t.Deadline(); ok {
		bound = time.After(time.Until(d) - urlFileDeadlineMargin)
	}
	select {
	case err := <-done:
		return &inv.Input, err
	case <-bound:
	}
	// A step that ended together with the bound ended in time.
	select {
	case err := <-done:
		return &inv.Input, err
	default:
		t.Fatalf("%s --url-file %s: the client step still runs %v after it started, past its deadline of %v", name, filepath.Base(path), time.Since(start).Round(time.Millisecond), urlFileCommandTimeout)
		return nil, nil
	}
}

// mkfifo makes a named pipe that no process writes to. Its cleanup opens
// the pipe for writing without blocking and closes it again: that ends the
// open or the read of a reader that still waits (a failed test), so the
// test leaves no blocked goroutine behind.
func mkfifo(t *testing.T, path string) string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	return path
}

// F27: a FIFO as --url-file blocked the client in os.Open until a writer
// came, and --timeout did not end the command. It is a usage error (exit
// 2) at once, before any read.
func TestURLFileFIFOIsAUsageErrorAtOnce(t *testing.T) {
	fifo := mkfifo(t, filepath.Join(t.TempDir(), "fifo"))
	for _, name := range []string{"open", "tab find"} {
		_, err := urlFileStep(t, name, fifo)
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), fifo) {
			t.Errorf("%s --url-file <fifo>: %v; want a usage error that names the path and says it is not a regular file", name, err)
		}
	}
}

// Every file that is not a regular file is a usage error before any read,
// also behind a symbolic link: a read of a FIFO, a device or a socket can
// block, and a directory holds no URL.
func TestURLFileRefusesEveryFileThatIsNotRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := mkfifo(t, filepath.Join(dir, "fifo"))
	// A short directory: the path of a Unix socket is limited to about
	// 100 bytes, and t.TempDir can be longer on macOS.
	sock := filepath.Join(testutil.ShortTestDir(t), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	link := func(name, target string) string {
		p := filepath.Join(dir, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, path := range []string{
		fifo,
		link("to-fifo", fifo),
		dir,
		link("to-dir", dir),
		os.DevNull,
		link("to-null", os.DevNull),
		sock,
		link("to-sock", sock),
	} {
		for _, name := range []string{"open", "tab find"} {
			in, err := urlFileStep(t, name, path)
			var ue *UsageError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), path) {
				t.Errorf("%s --url-file %s: %v (args %v); want a usage error that names the path and says it is not a regular file", name, path, err, in.Args)
			}
		}
	}
}

// A symbolic link to a regular file is a regular file: the link only
// names it.
func TestURLFileFollowsALinkToARegularFile(t *testing.T) {
	dir := t.TempDir()
	secret := "https://example.test/r/" + privateMarker
	target := filepath.Join(dir, "url")
	if err := os.WriteFile(target, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	in, err := urlFileStep(t, "open", link)
	if err != nil || len(in.Args) != 1 || in.Args[0] != secret {
		t.Errorf("open --url-file <link to a file>: %v, args %v; want the URL as the argument", err, in.Args)
	}
}

// The file holds one URL, not a document: a file of more than
// maxURLFileBytes bytes is a usage error that does not repeat the content,
// and a file of exactly maxURLFileBytes bytes is a URL.
func TestURLFileBoundsTheSize(t *testing.T) {
	dir := t.TempDir()
	body := func(n int) string { return strings.Repeat(privateMarker, n/len(privateMarker)+1)[:n] }
	path := filepath.Join(dir, "url")
	if err := os.WriteFile(path, []byte(body(maxURLFileBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if in, err := urlFileStep(t, "open", path); err != nil || len(in.Args) != 1 || len(in.Args[0]) != maxURLFileBytes {
		t.Errorf("open --url-file <%d bytes>: %v; want the content as the argument", maxURLFileBytes, err)
	}
	if err := os.WriteFile(path, []byte(body(maxURLFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := urlFileStep(t, "open", path)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "over") || strings.Contains(err.Error(), privateMarker) {
		t.Errorf("open --url-file <%d bytes>: %v; want a usage error about the size, without the content", maxURLFileBytes+1, err)
	}
}

// For any size of a file without white space, --url-file gives the content
// when the file holds 1 to maxURLFileBytes bytes, and otherwise a usage
// error that does not repeat the content.
func TestURLFileSizeProperty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "url")
	property := func(size uint32) bool {
		n := int(size % (2*maxURLFileBytes + 2))
		content := strings.Repeat(privateMarker, n/len(privateMarker)+1)[:n]
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		in := Input{Flags: map[string]any{"url-file": path}}
		err := urlFromFile(context.Background(), &in, nil)
		var ue *UsageError
		switch {
		case n >= 1 && n <= maxURLFileBytes:
			return err == nil && len(in.Args) == 1 && in.Args[0] == content
		case errors.As(err, &ue) && !strings.Contains(err.Error(), privateMarker):
			return true
		}
		t.Logf("%d bytes: %v", n, err)
		return false
	}
	// The sizes on both sides of the limit, then random sizes.
	for _, n := range []uint32{0, 1, maxURLFileBytes - 1, maxURLFileBytes, maxURLFileBytes + 1} {
		if !property(n) {
			t.Errorf("a file of %d bytes", n)
		}
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// F41: --url-file keeps a signed or one-time link out of the command line
// and out of every error of the command, with or without private mode,
// in the client and in the daemon. A URL argument keeps its messages.

// writeURLFile writes url to a new file and returns its path.
func writeURLFile(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(path, []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// daemonInput is the input of name --url-file path as the daemon parses
// it: the client runs the ClientStep and sends Argv (daemon_cli.go), and
// the daemon parses that Argv with the same registry spec.
func daemonInput(t *testing.T, name, path string) Input {
	t.Helper()
	noEnv := func(string) string { return "" }
	inv, err := Parse(append(strings.Fields(name), "--url-file", path), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Cmd.ClientStep(context.Background(), &inv.Input, nil); err != nil {
		t.Fatalf("%s --url-file: %v", name, err)
	}
	sent, err := Parse(Argv(inv.Cmd, inv.Input), noEnv)
	if err != nil {
		t.Fatalf("the daemon's parse of %v: %v", Argv(inv.Cmd, inv.Input), err)
	}
	return sent.Input
}

// wantFileNamed fails the test unless err names the file and holds
// neither the URL nor any part of the file's content.
func wantFileNamed(t *testing.T, what string, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	if msg := err.Error(); strings.Contains(msg, privateMarker) || strings.Contains(msg, "secret.example") || !strings.Contains(msg, path) {
		t.Fatalf("%s: %q; want an error that names %s and not the URL", what, msg, path)
	}
}

// The reviewer's repro TestReviewURLFileMalformedNotInError (finding F41):
// a URL from the file that does not parse was repeated by absoluteURL.
// The parser's error writes the URL with Go quoting (a quote becomes \"),
// so it need not hold the file's text as it is, and it repeats the bad
// part ("%zz") once more.
func TestURLFileMalformedURLIsNotInTheError(t *testing.T) {
	secret := "https://secret.example/%zz?token=" + privateMarker + "&q=\"x\""
	path := writeURLFile(t, secret)
	noBadPart := func(what string, err error) {
		t.Helper()
		if strings.Contains(err.Error(), "%zz") {
			t.Fatalf("%s: %q repeats a part of the URL", what, err)
		}
	}
	in := Input{Flags: map[string]any{"url-file": path}}
	if err := urlFromFile(context.Background(), &in, nil); err != nil {
		t.Fatal(err)
	}
	_, err := runOpen(context.Background(), nil, in)
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("open --url-file <malformed URL>: %v; want a usage error", err)
	}
	wantFileNamed(t, "open --url-file <malformed URL> in the client", err, path)
	noBadPart("open --url-file <malformed URL> in the client", err)
	_, err = runOpen(context.Background(), nil, daemonInput(t, "open", path))
	if !errors.As(err, &ue) {
		t.Fatalf("open --url-file <malformed URL> in the daemon: %v; want a usage error", err)
	}
	wantFileNamed(t, "open --url-file <malformed URL> in the daemon", err, path)
	noBadPart("open --url-file <malformed URL> in the daemon", err)
	// A URL argument keeps its message, which repeats the URL.
	inv, err := Parse([]string{"open", secret}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	_, err = runOpen(context.Background(), nil, inv.Input)
	quoted := strconv.Quote(secret) // the parser's quoting (url.Error, %q)
	want := "open " + quoted + ": parse " + quoted + ": invalid URL escape \"%zz\""
	if err == nil || err.Error() != want {
		t.Fatalf("open <malformed URL>: %v; want %q", err, want)
	}
}

// The start of the navigation names its URL, and the page's error can
// repeat it: with --url-file the error names the file instead.
func TestURLFileNavigationStartErrorNamesTheFile(t *testing.T) {
	secret := "https://secret.example/r?token=" + privateMarker
	path := writeURLFile(t, secret)
	h := newActionHarness(t)
	pageErr := "runtime error in the page: \"" + secret + "\" is not a valid URL"
	h.page.set("dialogs", errEnv("internal", pageErr))
	code, _, errOut := h.run("open", "--url-file", path)
	if code == ExitOK || strings.Contains(errOut, privateMarker) || !strings.Contains(errOut, path) {
		t.Fatalf("open --url-file with a refused navigation: exit %d, stderr %q; want an error that names %s and not the URL", code, errOut, path)
	}
	code, _, errOut = h.run("open", secret)
	if want := "Error: open: open " + secret + ": internal: " + pageErr + "\n"; code == ExitOK || errOut != want {
		t.Fatalf("open <url> with a refused navigation: exit %d, stderr %q; want %q", code, errOut, want)
	}
}

// A page that does not navigate keeps its URL, which is the file's URL
// when the page already shows that link: the error names no URL then.
// The page writes the URL in its own form (a host in lower case), so its
// text differs from the file's.
func TestURLFileNoNavigationErrorNamesNoURL(t *testing.T) {
	path := writeURLFile(t, "https://SECRET.EXAMPLE/r?token="+privateMarker)
	n := newNavFake(t)
	n.navigates = false
	n.mu.Lock()
	n.href = "https://secret.example/r?token=" + privateMarker
	n.mu.Unlock()
	code, _, errOut := n.h.run("--timeout", "600ms", "open", "--url-file", path)
	if code != ExitWaitTimeout || strings.Contains(errOut, privateMarker) || !strings.Contains(errOut, "did not navigate") || !strings.Contains(errOut, path) {
		t.Fatalf("open --url-file without a navigation: exit %d, stderr %q; want exit 3 and an error that names %s and not the URL", code, errOut, path)
	}
}

// tab find sends the target to each tab that can show it; a page that
// throws repeats the target in its error (WebKit: "<url>" cannot be
// parsed as a URL). With --url-file the error names the file instead; a
// URL argument keeps the page's message.
func TestURLFileTabFindReadErrorNamesTheFile(t *testing.T) {
	secret := "https://secret.example/r?token=" + privateMarker
	path := writeURLFile(t, secret)
	m, f := newTabManager(t)
	thrown := "TypeError: \"" + secret + "\" cannot be parsed as a URL."
	s := f.add("Throws", secret)
	s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if !strings.HasPrefix(p.Expression, "location.href === new URL(") {
			return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "description": thrown}, "wasThrown": true}, nil
	})
	_, err := runIn(t, m, "a", Argv(mustLookup(t, "tab find"), daemonInput(t, "tab find", path))...)
	wantFileNamed(t, "tab find --url-file with a page that throws", err, path)
	_, err = runIn(t, m, "a", "tab", "find", secret)
	if want := "reading tab t1: " + thrown; err == nil || err.Error() != want {
		t.Fatalf("tab find <url> with a page that throws: %v; want %q", err, want)
	}
}

// mustLookup returns the registry command name.
func mustLookup(t *testing.T, name string) *Command {
	t.Helper()
	c, _ := Lookup(strings.Fields(name))
	if c == nil || c.Name != name {
		t.Fatalf("no registry command %q", name)
	}
	return c
}

// A request with --url-file and without the URL that the client read (a
// caller other than agent-ios's client, which runs no ClientStep) is a
// usage error that says so: the flag names a file of the client.
func TestURLFileWithoutItsURLIsAUsageErrorInTheDaemon(t *testing.T) {
	for _, name := range []string{"open", "tab find"} {
		inv, err := Parse(append(strings.Fields(name), "--url-file", "/caller/target"), func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		_, err = inv.Cmd.Run(context.Background(), nil, inv.Input)
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--url-file reads a file of agent-ios's caller") {
			t.Errorf("%s --url-file without the URL: %v; want the usage error about the caller's file", name, err)
		}
	}
}
