package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// storage local|session: read, set and clear localStorage and
// sessionStorage through the page (design docs/design/agentic-parity.md,
// section 4.2; agent-browser "storage local [key]", "storage local set k
// v", "storage local clear"). Values of password and token keys are masked
// unless --show-values (design 7.9).
func init() {
	Register(&Command{
		Name:    "storage",
		Summary: "Read, set or clear localStorage or sessionStorage of the page: storage local [key], storage local set <key> <value>, storage local clear (same for session).",
		Args: []Arg{
			{Name: "area", Help: "local or session", Required: true},
			{Name: "action", Help: "a key to read, or get, set or clear (none: list every item)"},
			{Name: "key", Help: "get and set: the key"},
			{Name: "value", Help: "set: the value"},
		},
		Flags: []Flag{
			{Name: "show-values", Type: FlagBool, Help: "show the values of password and token keys instead of [masked]"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runStorage,
	})
}

// maskedHint follows a listing in which a value was masked.
const maskedHint = "(values of password and token keys are masked; pass --show-values to see them)"

// pageAndCaller returns the command's page session and the caller its
// protocol calls go through: in the daemon the operation's fenced caller
// (design 2.5, 2.10), otherwise the page connection.
func pageAndCaller(ctx context.Context, env Env) (*core.PageSession, webkit.Caller, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ps == nil || ps.Caller == nil {
		return nil, nil, errors.New("this command needs a page connection")
	}
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		return ps, oe.Op().Caller(), nil
	}
	return ps, ps.Caller, nil
}

// nounCount renders "1 cookie" or "2 cookies".
func nounCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func runStorage(ctx context.Context, env Env, in Input) (Result, error) {
	area := arg(in, 0)
	if area != "local" && area != "session" {
		return Result{}, usagef("storage %q: want local or session", area)
	}
	store := area + "Storage"
	rest := in.Args[1:]
	op, key, value := "list", "", ""
	switch {
	case len(rest) == 0:
	case rest[0] == "set":
		if len(rest) != 3 {
			return Result{}, usagef("storage %s set needs a key and a value", area)
		}
		op, key, value = "set", rest[1], rest[2]
	case rest[0] == "clear":
		if len(rest) != 1 {
			return Result{}, usagef("storage %s clear takes no arguments", area)
		}
		op = "clear"
	case rest[0] == "get":
		if len(rest) != 2 {
			return Result{}, usagef("storage %s get needs one key", area)
		}
		op, key = "get", rest[1]
	case len(rest) == 1:
		op, key = "get", rest[0]
	default:
		return Result{}, usagef("storage %s: want [key], get <key>, set <key> <value> or clear", area)
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	show := flagBool(in, "show-values")
	mask := func(k, v string) (string, bool) {
		if !show && devtools.SecretName(k) {
			return devtools.Masked, true
		}
		return v, false
	}
	switch op {
	case "set":
		origin, err := devtools.StorageSet(ctx, c, area, key, value)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: fmt.Sprintf("Set %s %s on %s.", store, key, origin), Data: map[string]any{"origin": origin, "area": area, "key": key}}, nil
	case "clear":
		origin, n, err := devtools.StorageClear(ctx, c, area)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: fmt.Sprintf("Cleared %s on %s.", nounCount(n, store+" item"), origin), Data: map[string]any{"origin": origin, "area": area, "cleared": n}}, nil
	case "get":
		origin, v, found, err := devtools.StorageGet(ctx, c, area, key)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, fmt.Errorf("%s of %s has no key %q", store, origin, key)
		}
		v, masked := mask(key, v)
		text := v
		if masked {
			text += "\n" + maskedHint
		}
		return Result{Text: text, Data: map[string]any{"origin": origin, "area": area, "key": key, "found": true, "value": v}}, nil
	}
	origin, items, err := devtools.StorageList(ctx, c, area)
	if err != nil {
		return Result{}, err
	}
	if len(items) == 0 {
		return Result{Text: fmt.Sprintf("%s of %s is empty.", store, origin), Data: map[string]any{"origin": origin, "area": area, "items": items}}, nil
	}
	lines := make([]string, 0, len(items)+1)
	anyMasked := false
	for i, it := range items {
		v, masked := mask(it.Name, it.Value)
		anyMasked = anyMasked || masked
		items[i].Value = v
		lines = append(lines, it.Name+"="+v)
	}
	if anyMasked {
		lines = append(lines, maskedHint)
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"origin": origin, "area": area, "items": items}}, nil
}

// pageHostname is location.hostname of the page ("" on about:blank).
func pageHostname(ctx context.Context, c webkit.Caller) (string, error) {
	res, err := tools.EvaluateScript(ctx, c, "location.hostname", true)
	if err != nil {
		return "", fmt.Errorf("reading the page's host: %w", err)
	}
	var host string
	if err := json.Unmarshal(res.Result.Value, &host); err != nil {
		return "", fmt.Errorf("reading the page's host: the page returned %s", res.Result.Type)
	}
	return host, nil
}
