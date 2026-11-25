package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// The flag --url-file gives a command its URL in a file. A signed or
// one-time link is a secret, and a transcript keeps the text of every
// command line, so the caller writes only the path of the file. The
// client reads the file (urlFromFile, a ClientStep); the URL reaches the
// daemon in the request body, like the value of fill --stdin. The flag
// stays beside the URL that it gave, also in the request's Argv: until
// its last error the command knows that its URL came from the file
// (urlFile), and its errors name the file instead of the URL
// (hideFileURL).

// urlFileFlag is the flag of a command that takes its URL from a file.
var urlFileFlag = Flag{Name: "url-file", Type: FlagPath, Help: "read the URL from this file (one line), so the URL is not on the command line"}

// maxURLFileBytes bounds the file of --url-file, in bytes: a URL, not a
// document.
const maxURLFileBytes = 64 << 10

// urlFromFile is the ClientStep of a command whose first argument is a URL
// and that has urlFileFlag: with the flag, it reads the URL from the file
// and makes it the first argument, and it keeps the flag, which says
// where the argument came from. The file holds one URL; white space
// around it is dropped. Its errors never repeat the content of the file.
// Without the flag and without the argument it is the usage error of the
// command, before any connection: the argument is optional only because
// the flag can give it.
func urlFromFile(_ context.Context, in *Input, _ io.Reader) error {
	path := flagString(*in, urlFileFlag.Name)
	if path == "" {
		if len(in.Args) == 0 {
			// The words of the parser for a missing required argument.
			return usagef("missing argument <url>, or --url-file <path> (run 'agent-ios <command> --help')")
		}
		return nil
	}
	if len(in.Args) != 0 {
		return usagef("--url-file gives the URL; give no <url> argument with it")
	}
	f, err := openRegular(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // a read-only file; the read's error is the one to report
	data, err := io.ReadAll(io.LimitReader(f, maxURLFileBytes+1))
	if err != nil {
		return fmt.Errorf("--url-file: reading %s: %w", path, err)
	}
	u := strings.TrimSpace(string(data))
	switch {
	case len(data) > maxURLFileBytes:
		return usagef("--url-file: %s is over %d bytes; it must hold one URL", path, maxURLFileBytes)
	case u == "":
		return usagef("--url-file: %s is empty; it must hold one URL", path)
	case strings.ContainsAny(u, " \t\r\n"):
		return usagef("--url-file: %s holds white space inside its content; it must hold one URL on one line", path)
	}
	in.Args = []string{u}
	return nil
}

// urlFile is the path of the --url-file that gave the command its URL
// (urlFromFile), or "" for a URL argument.
func urlFile(in Input) string { return flagString(in, urlFileFlag.Name) }

// fileURLName is how an error names the URL that the file path gave.
func fileURLName(path string) string { return "the URL in --url-file " + path }

// hideFileURL returns err for a command whose URL came from the file path
// (urlFile; "": a URL argument, and err stays as it is): each occurrence
// of urls in its text becomes fileURLName(path). Every error of open and
// tab find passes through it last: an error of the command that names
// its URL ("open <url>: ..."), and a text that repeats the URL from
// elsewhere, as the error of a page that was given it (WebKit: "<url>"
// cannot be parsed as a URL). A text that holds the URL in another form
// (quoted, or as the browser writes it) is not found: those errors name
// the file themselves (runOpen, notNavigated). Give the longest URL
// first. errors.Is, errors.As and the exit code see err as before.
func hideFileURL(err error, path string, urls ...string) error {
	if err == nil || path == "" {
		return err
	}
	var pairs []string
	for _, u := range urls {
		if u != "" {
			pairs = append(pairs, u, fileURLName(path))
		}
	}
	msg := strings.NewReplacer(pairs...).Replace(err.Error())
	if msg == err.Error() {
		return err
	}
	return &fileURLError{msg: msg, err: err}
}

// fileURLError is an error whose text names a URL of --url-file by the
// file's path (hideFileURL).
type fileURLError struct {
	msg string
	err error
}

func (e *fileURLError) Error() string { return e.msg }
func (e *fileURLError) Unwrap() error { return e.err }

// openRegular opens the file of --url-file for reading, and it is a usage
// error when the file is not a regular file: the read of a FIFO, a device
// or a socket can block until another process writes, which the command
// deadline does not end, and a directory holds no URL. The open does not
// block either (O_NONBLOCK: the open of a FIFO otherwise waits for a
// writer), and the check is on the opened file, so a file that replaced
// the path after a check of the path cannot pass it. A symbolic link
// counts as the file it names. O_NOCTTY keeps the open of a terminal from
// making it the controlling terminal of agent-ios.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		// A socket cannot be opened (ENXIO on Linux, EOPNOTSUPP on
		// macOS): name the type of the file, as for an opened one.
		if st, serr := os.Stat(path); serr == nil && !st.Mode().IsRegular() {
			return nil, notRegular(path, st.Mode())
		}
		return nil, fmt.Errorf("--url-file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("--url-file: %w", err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, notRegular(path, st.Mode())
	}
	return f, nil
}

// notRegular is the usage error of a file of --url-file that is not a
// regular file. It names the type of the file.
func notRegular(path string, mode os.FileMode) error {
	kind := "a special file"
	switch {
	case mode.IsDir():
		kind = "a directory"
	case mode&os.ModeNamedPipe != 0:
		kind = "a named pipe (FIFO)"
	case mode&os.ModeSocket != 0:
		kind = "a socket"
	case mode&os.ModeDevice != 0:
		kind = "a device"
	}
	return usagef("--url-file: %s is %s, not a regular file; write the URL to a regular file", path, kind)
}

// needURL is the usage error of a command that got neither a URL argument
// nor the content of --url-file. A caller that did not run the ClientStep
// (a request to the daemon with the flag and without the URL that the
// client read) gets it too: the flag names a file of the client.
func needURL(in Input, name string) error {
	if arg(in, 0) != "" {
		return nil
	}
	if urlFile(in) != "" {
		return usagef("%s --url-file reads a file of agent-ios's caller; other callers pass the URL as the argument", name)
	}
	return usagef("%s needs <url> or --url-file <path>", name)
}
