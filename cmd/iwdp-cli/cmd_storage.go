package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

func cmdSetCookie(ctx context.Context, args []string) {
	const usage = "Usage: iwdp-cli set-cookie <name> <value> <domain> [--same-site Lax|Strict|None] [--path /] [--expires <time>] [--secure] [--http-only] [ws-url]"
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	cookie, wsArgs, err := parseSetCookieArgs(args, time.Now())
	if err != nil {
		usageError(usage, err)
	}
	client, err := connectToPage(ctx, wsArgs)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	stored, err := tools.SetCookieVerified(ctx, client, cookie)
	if err != nil {
		fatal(err)
	}
	redacted := *stored
	redacted.Value = tools.RedactCookieValue(stored.Value)
	fmt.Printf("Cookie %s set and verified: %s\n", cookie.Name, formatCookie(redacted))
}

// parseSetCookieArgs parses "<name> <value> <domain> [options] [ws-url]".
// now anchors relative --expires values.
func parseSetCookieArgs(args []string, now time.Time) (webkit.Cookie, []string, error) {
	cookie := webkit.Cookie{
		Name:     args[0],
		Value:    args[1],
		Domain:   args[2],
		Path:     "/",
		SameSite: tools.DefaultSameSite,
	}
	var wsArgs []string
	for i := 3; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--path", "--same-site", "--sameSite", "--expires":
			if i+1 >= len(args) {
				return cookie, nil, fmt.Errorf("%s needs a value", a)
			}
			value := args[i+1]
			i++
			switch a {
			case "--path":
				cookie.Path = value
			case "--expires":
				expires, err := parseCookieExpiry(value, now)
				if err != nil {
					return cookie, nil, err
				}
				cookie.Expires = float64(expires.UnixMilli())
				cookie.Session = false
			default:
				sameSite, err := tools.NormalizeSameSite(value)
				if err != nil {
					return cookie, nil, err
				}
				cookie.SameSite = sameSite
			}
		case "--secure":
			cookie.Secure = true
		case "--http-only", "--httpOnly":
			cookie.HTTPOnly = true
		default:
			if strings.HasPrefix(a, "-") {
				return cookie, nil, fmt.Errorf("unknown option %s", a)
			}
			wsArgs = append(wsArgs, a)
		}
	}
	if cookie.Expires == 0 {
		cookie.Session = true
	}
	return cookie, wsArgs, nil
}

// parseCookieExpiry accepts Unix seconds, an RFC 3339 time, or a duration
// from now (24h).
func parseCookieExpiry(v string, now time.Time) (time.Time, error) {
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(secs, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --expires %q: use Unix seconds, RFC 3339 (2026-12-31T00:00:00Z) or a duration from now (24h)", v)
}

func cmdDeleteCookie(ctx context.Context, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli delete-cookie <name> <url> [ws-url]")
		os.Exit(1)
	}
	name := args[0]
	cookieURL := args[1]
	client, err := connectToPage(ctx, args[2:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.DeleteCookie(ctx, client, name, cookieURL); err != nil {
		fatal(err)
	}
	fmt.Printf("Cookie %s deleted\n", name)
}

func cmdGetLocalStorage(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli get-local-storage <origin> [ws-url]")
		os.Exit(1)
	}
	origin := args[0]
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	items, err := tools.GetLocalStorage(ctx, client, origin)
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(items, "", "  ")
	fmt.Println(string(out))
}

func cmdSetLocalStorageItem(ctx context.Context, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli set-local-storage-item <origin> <key> <value> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[3:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.SetLocalStorageItem(ctx, client, args[0], args[1], args[2]); err != nil {
		fatal(err)
	}
	fmt.Printf("Set %s = %s\n", args[1], args[2])
}

func cmdRemoveLocalStorageItem(ctx context.Context, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli remove-local-storage-item <origin> <key> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[2:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.RemoveLocalStorageItem(ctx, client, args[0], args[1]); err != nil {
		fatal(err)
	}
	fmt.Printf("Removed %s\n", args[1])
}

func cmdClearLocalStorage(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli clear-local-storage <origin> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.ClearLocalStorage(ctx, client, args[0]); err != nil {
		fatal(err)
	}
	fmt.Println("Local storage cleared")
}

func cmdGetSessionStorage(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli get-session-storage <origin> [ws-url]")
		os.Exit(1)
	}
	origin := args[0]
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	items, err := tools.GetSessionStorage(ctx, client, origin)
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(items, "", "  ")
	fmt.Println(string(out))
}

func cmdSetSessionStorageItem(ctx context.Context, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli set-session-storage-item <origin> <key> <value> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[3:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.SetSessionStorageItem(ctx, client, args[0], args[1], args[2]); err != nil {
		fatal(err)
	}
	fmt.Printf("Set %s = %s\n", args[1], args[2])
}

func cmdRemoveSessionStorageItem(ctx context.Context, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli remove-session-storage-item <origin> <key> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[2:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.RemoveSessionStorageItem(ctx, client, args[0], args[1]); err != nil {
		fatal(err)
	}
	fmt.Printf("Removed %s\n", args[1])
}

func cmdClearSessionStorage(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli clear-session-storage <origin> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.ClearSessionStorage(ctx, client, args[0]); err != nil {
		fatal(err)
	}
	fmt.Println("Session storage cleared")
}

func cmdListIndexedDatabases(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli list-indexed-databases <origin> [ws-url]")
		os.Exit(1)
	}
	origin := args[0]
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	dbs, err := tools.ListIndexedDatabases(ctx, client, origin)
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(dbs, "", "  ")
	fmt.Println(string(out))
}

func cmdGetIndexedDBData(ctx context.Context, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli get-indexed-db-data <origin> <dbName> <storeName> [--skip N] [--count N] [ws-url]")
		os.Exit(1)
	}
	origin := args[0]
	dbName := args[1]
	storeName := args[2]
	skip, count := 0, 10
	var wsArgs []string
	for i := 3; i < len(args); i++ {
		switch args[i] {
		case "--skip":
			if i+1 < len(args) {
				skip, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--count":
			if i+1 < len(args) {
				count, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			wsArgs = append(wsArgs, args[i])
		}
	}
	client, err := connectToPage(ctx, wsArgs)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	data, err := tools.GetIndexedDBData(ctx, client, origin, dbName, storeName, skip, count)
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(json.RawMessage(data), "", "  ")
	fmt.Println(string(out))
}

func cmdClearIndexedDBStore(ctx context.Context, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli clear-indexed-db-store <origin> <dbName> <storeName> [ws-url]")
		os.Exit(1)
	}
	client, err := connectToPage(ctx, args[3:])
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := tools.ClearIndexedDBStore(ctx, client, args[0], args[1], args[2]); err != nil {
		fatal(err)
	}
	fmt.Println("IndexedDB store cleared")
}
