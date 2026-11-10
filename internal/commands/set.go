package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// set media, useragent, headers, credentials, geo, offline, viewport and
// device: the session's emulation and settings (design
// docs/design/agentic-parity.md, sections 4.2 "Emulation and settings" and
// 4.3). Each setting is desired state owned by the session: a reconnect
// or a new target gets it again, and the release of the lease resets it.
func init() {
	Register(&Command{
		Name:    "set media",
		Summary: "Emulate prefers-color-scheme (dark, light or no-preference), and optionally reduced motion and more contrast.",
		Args:    []Arg{{Name: "scheme", Help: "dark, light or no-preference", Required: true}},
		Flags: []Flag{
			{Name: "reduced-motion", Type: FlagBool, Help: "also emulate prefers-reduced-motion: reduce"},
			{Name: "contrast", Type: FlagEnum, Enum: []string{"more"}, Help: "also emulate prefers-contrast"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			m := devtools.Media{ColorScheme: arg(in, 0), ReducedMotion: flagBool(in, "reduced-motion"), ContrastMore: flagString(in, "contrast") == "more"}
			if m.ColorScheme != "dark" && m.ColorScheme != "light" && m.ColorScheme != "no-preference" {
				return Result{}, usagef("set media %q: want dark, light or no-preference", m.ColorScheme)
			}
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			if err := d.p.SetMedia(ctx, d.c, d.session, m); err != nil {
				return Result{}, err
			}
			return Result{Text: "Media set: prefers-color-scheme " + m.ColorScheme + "."}, nil
		},
	})
	Register(&Command{
		Name:    "set useragent",
		Summary: "Override the user agent string of the page (--reset removes the override).",
		Args:    []Arg{{Name: "ua", Help: "user agent string"}},
		Flags:   []Flag{{Name: "reset", Type: FlagBool, Help: "remove the override"}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			ua, reset := arg(in, 0), flagBool(in, "reset")
			if (ua == "") == !reset {
				return Result{}, usagef("set useragent: give a user agent string or --reset")
			}
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			if err := d.p.SetUserAgent(ctx, d.c, d.session, ua); err != nil {
				return Result{}, err
			}
			if reset {
				return Result{Text: "User agent override removed."}, nil
			}
			return Result{Text: "User agent set."}, nil
		},
	})
	Register(&Command{
		Name:    "set headers",
		Summary: "Send these extra HTTP headers with every request of the page; '{}' removes this session's headers.",
		Args:    []Arg{{Name: "json", Help: "a JSON object of header names and string values, for example '{\"X-A\":\"1\"}'", Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runSetHeaders,
	})
	Register(&Command{
		Name:    "set credentials",
		Summary: "Send HTTP Basic credentials to the requests of one origin only; on a command line the password is the first line of stdin. Supported only where a test has shown that other origins and cross-origin redirects never get them (WebKitGTK today).",
		Args:    []Arg{{Name: "user", Help: "user name (on a command line, the password is the first line of stdin)"}},
		Flags: []Flag{
			{Name: "origin", Type: FlagString, Help: "the origin that gets the credentials, for example https://example.com"},
			{Name: "password", Type: FlagString, Help: "the password; a command line refuses it, because argv is readable by other local users: pipe the password to stdin; never logged or printed"},
			{Name: "clear", Type: FlagBool, Help: "remove every credential of this session instead"},
		},
		Page:       LeasedPage,
		Mutates:    true,
		Output:     OutputText,
		ClientStep: credentialsStdin,
		Run:        runApplyCredentials,
	})
	Register(&Command{
		Name:    "set geo",
		Summary: "Answer navigator.geolocation with this position (a page-side shim: WebKit has no geolocation override).",
		Args: []Arg{
			{Name: "latitude", Help: "degrees, -90 to 90", Required: true},
			{Name: "longitude", Help: "degrees, -180 to 180", Required: true},
		},
		Flags:   []Flag{{Name: "accuracy", Type: FlagInt, Default: "10", Help: "accuracy in meters"}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runSetGeo,
	})
	Register(&Command{
		Name:    "set offline",
		Summary: "Fail every request of the page (on) or stop doing so (off), through request interception; unverified on a device.",
		Args:    []Arg{{Name: "state", Help: "on or off", Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			on, ok := map[string]bool{"on": true, "off": false}[arg(in, 0)]
			if !ok {
				return Result{}, usagef("set offline %q: want on or off", arg(in, 0))
			}
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			if err := d.p.SetOffline(ctx, d.c, d.session, on); err != nil {
				return Result{}, err
			}
			return Result{Text: "Offline " + arg(in, 0) + "."}, nil
		},
	})
	for _, name := range []string{"set viewport", "set device"} {
		Register(&Command{
			Name:    name,
			Summary: "Not available on iOS WebKit: Page.setScreenSizeOverride exists only outside Apple platforms (design 4.3). Screenshots use the device's native size; 'set useragent' is the closest substitute.",
			Args:    []Arg{{Name: "args", Help: "ignored", Variadic: true}},
			Output:  OutputText,
			Run: func(context.Context, Env, Input) (Result, error) {
				return Result{}, fmt.Errorf("%w: WebKit compiles Page.setScreenSizeOverride only for non-Apple platforms, so the page keeps the device's viewport; use 'set useragent' to change the user agent", devtools.ErrUnsupported)
			},
		})
	}
}

func runSetHeaders(ctx context.Context, env Env, in Input) (Result, error) {
	var h map[string]string
	if err := json.Unmarshal([]byte(arg(in, 0)), &h); err != nil || h == nil {
		return Result{}, usagef("set headers: want a JSON object of string values, for example '{\"X-A\":\"1\"}'")
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	reset, err := d.p.SetHeaders(ctx, d.c, d.session, h)
	if err != nil {
		return Result{}, err
	}
	text := fmt.Sprintf("Extra headers set: %d.", len(h))
	if len(h) == 0 {
		text = "Extra headers of this session removed."
	}
	if reset {
		text += " Network was reinitialized to remove headers: requests in flight were closed as capture_reset and earlier response bodies are gone."
	}
	return Result{Text: text, Data: map[string]any{"headers": len(h), "networkReset": reset}}, nil
}

// credentialsStdin is the client step of "set credentials" (iwdp-cli
// only): it reads the password from the first line of stdin, under the
// command deadline, into the password flag, which reaches the daemon in
// the /v1/run request body. A --password given on the command line is
// refused, because argv is readable by other local users. The password is
// never part of a result or an error.
func credentialsStdin(ctx context.Context, in *Input, stdin io.Reader) error {
	if _, given := in.Flags["password"]; given {
		return usagef("set credentials: --password is refused on a command line, where other local users can read it; pipe the password to stdin, for example: printf '%%s\\n' \"$PASS\" | iwdp-cli set credentials --origin <origin> <user>")
	}
	if flagBool(*in, "clear") {
		if arg(*in, 0) != "" || flagString(*in, "origin") != "" {
			return usagef("set credentials: --clear takes no user or --origin")
		}
		return nil
	}
	if arg(*in, 0) == "" || flagString(*in, "origin") == "" {
		return usagef("set credentials: want --origin <origin> <user>, with the password on stdin")
	}
	line, err := readWithin(ctx, func() (string, error) {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return line, err
	})
	if err != nil {
		return fmt.Errorf("set credentials: reading the password from stdin: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return usagef("set credentials: no password on stdin; pipe it in, for example: printf '%%s\\n' \"$PASS\" | iwdp-cli set credentials --origin <origin> <user>")
	}
	in.Flags["password"] = password // Parse always makes the map
	return nil
}

func runApplyCredentials(ctx context.Context, env Env, in Input) (Result, error) {
	clearAll := flagBool(in, "clear")
	if !clearAll && (arg(in, 0) == "" || flagString(in, "origin") == "" || flagString(in, "password") == "") {
		return Result{}, usagef("set credentials: want an origin, a user and a password")
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if clearAll {
		n, err := d.p.ClearCredentials(ctx, d.c, d.session)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: fmt.Sprintf("Removed %d credential(s).", n)}, nil
	}
	origin, err := d.p.SetCredentials(ctx, d.c, d.session, flagString(in, "origin"), arg(in, 0), flagString(in, "password"))
	if err != nil {
		return Result{}, err
	}
	return Result{Text: "Credentials set for " + origin + " only.", Data: map[string]string{"origin": origin}}, nil
}

func runSetGeo(ctx context.Context, env Env, in Input) (Result, error) {
	lat, err1 := strconv.ParseFloat(arg(in, 0), 64)
	lng, err2 := strconv.ParseFloat(arg(in, 1), 64)
	if err1 != nil || err2 != nil {
		return Result{}, usagef("set geo %q %q: want two numbers, latitude and longitude", arg(in, 0), arg(in, 1))
	}
	acc, _ := in.Flags["accuracy"].(int)
	if acc <= 0 {
		return Result{}, usagef("set geo: --accuracy %d: want meters above 0", acc)
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if err := d.p.SetGeolocation(ctx, d.c, d.session, lat, lng, float64(acc)); err != nil {
		return Result{}, err
	}
	return Result{Text: fmt.Sprintf("Geolocation set to %v, %v.", lat, lng)}, nil
}
