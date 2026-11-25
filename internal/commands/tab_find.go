package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/browserurl"
	"github.com/nnemirovsky/agent-ios/internal/core"
)

// ExitNoMatch is the exit code of "tab find" when it read every tab that
// can show the target and none shows it, and of "sim up" when Safari
// shows no tab of its --url in time. No other failure of tab find has it: a
// listing that hides pages ends the command with exit 6, a tab that could
// not be read with exit 1 or the exit code of that read (4: another
// session holds the tab), so a caller that acts on "the target is not
// open" can tell it from "I could not look".
const ExitNoMatch = 7

// tab find answers "which tab shows exactly this URL" without printing a
// title or a URL. A caller that was told to work on one page finds its tab
// with it before the first action: the listing strips the query and the
// fragment of every URL (design 2.5 rule 4), so only a read inside the
// page tells a signed link from another state of the same path.
func init() {
	Register(&Command{
		Name:    "tab find",
		Summary: "Print the id of the tab whose page shows exactly this URL (query and fragment included), compared inside the page. It only reads: it navigates and selects nothing. The session keeps the lease of the tab that the command prints, as after any read of that tab, and no lease that only the command used on another tab. Exit 7 when no tab shows the URL; exit 1 when several do or a tab could not be read; exit 6 when the listing hides pages of a device that the caller has not reserved (--device limits the search, and that count, to one device).",
		Args:    []Arg{{Name: "url", Help: "the URL to find; or --url-file"}},
		Flags:   []Flag{urlFileFlag, tabDeviceFlag},
		Page:    NoPage, Output: OutputText,
		ClientStep: urlFromFile,
		Run:        runTabFind,
	})
}

// sameDocument is browserurl.SameDocument: whether a listed URL (without
// query and fragment) can be the page of target. It only narrows the tabs
// that tab find reads.
func sameDocument(listed, target string) bool { return browserurl.SameDocument(listed, target) }

func runTabFind(ctx context.Context, env Env, in Input) (_ Result, err error) {
	if err := needURL(in, "tab find"); err != nil {
		return Result{}, err
	}
	raw := arg(in, 0) // the target as the caller wrote it
	// The errors of tab find name no target; the read of a page that
	// throws can repeat it, which a target of --url-file must not be.
	defer func() { err = hideFileURL(err, urlFile(in), raw) }()
	target, err := url.Parse(raw)
	if err != nil || target.Scheme == "" || target.Host == "" {
		// The error of url.Parse repeats the URL, which can be a secret.
		return Result{}, usagef("tab find: the URL is not an absolute URL")
	}
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	tabs, hidden, err := m.DeviceTabs(ctx, tabRequest(in, ""), acquireFor(in), flagString(in, "device"))
	if err != nil {
		return Result{}, err
	}
	// A hidden page can show the target: without it the command can prove
	// neither that no tab shows the target nor that a match is the only
	// one. The message names no page (design 2.5 rule 4).
	if hidden > 0 {
		return Result{}, &core.CodedError{Code: core.CodeReservation, Err: fmt.Errorf("tab find: the listing hides %d page(s) of a device that your reservation owner has not reserved (a physical device without your reservation, or the simulator in another owner's turn), and any of them can show the target; search only your device with --device SIMULATOR or --device <UDID>, or reserve the device ('agent-ios device reserve', 'agent-ios sim reserve') and run tab find again", hidden)}
	}
	var candidates []core.Tab // the tabs that can show the target
	for _, t := range tabs {
		if !sameDocument(t.URL, raw) {
			continue
		}
		if !t.Debuggable {
			return Result{}, fmt.Errorf("tab find: tab %s can show the target and cannot be read, because the browser does not offer it for debugging (another debugger may be attached); end that debugging session and run tab find again", t.ID)
		}
		candidates = append(candidates, t)
	}
	// The page compares its own href with the target's, both as the URL
	// parser of the page writes them. The page gets the target as the
	// caller wrote it: net/url writes some characters in another way than
	// a browser does ("|" in a path as "%7C"), and the page would compare
	// with another URL. A JSON string is a JavaScript string literal.
	lit, err := json.Marshal(raw)
	if err != nil {
		return Result{}, fmt.Errorf("tab find: writing the target for the page: %w", err)
	}
	expr := "location.href === new URL(" + string(lit) + ").href"
	// claim records each lease that a read of this command was granted
	// (core.LeaseClaim): a lease that the session held before, or that
	// another command of the session took meanwhile, is renewed by the
	// read and is not in it.
	var claim core.LeaseClaim
	ao := acquireFor(in)
	ao.Claim = &claim
	var (
		found   []core.Tab // the tabs that show the target
		readErr error
	)
	for _, t := range candidates {
		var same bool
		err := m.Do(ctx, tabRequest(in, t.Key), ao, "tab find", core.ClassRead, true, func(ctx context.Context, mo *core.ManagedOp) error {
			raw, err := mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
			if err != nil {
				return err
			}
			same, err = evalValue[bool](raw)
			return err
		})
		if err != nil {
			// A tab that could not be read is not a tab that shows another
			// page: the error keeps its exit code, which is never
			// ExitNoMatch.
			readErr = fmt.Errorf("reading tab %s: %w", t.ID, err)
			break
		}
		if same {
			found = append(found, t)
		}
	}
	// This is the one place that ends the leases that the reads were
	// granted, for every outcome. The session keeps the leases that it
	// held before and every lease that another command took. When the
	// command prints a tab (exit 0), the session also keeps the lease of
	// that tab, as after any other read of it: the tab stays the session's
	// until its first action, so no other session moves it off the target
	// in between. A command that fails keeps no lease that only it used.
	// The core decides, under the lock that grants and uses take, whether
	// a granted lease is still this command's alone (ReleaseClaimed): a
	// lease that another operation of the session used, uses or waits for
	// since the grant stays, and a command that comes during the release
	// gets a new lease.
	end := func(key, id string) error {
		if err := m.ReleaseClaimed(ctx, &claim, key); err != nil {
			return fmt.Errorf("releasing the lease that the read took on tab %s: %w", id, err)
		}
		return nil
	}
	tabIDs := map[string]string{} // page key -> tab id
	for _, t := range candidates {
		tabIDs[t.Key] = t.ID
	}
	keep := "" // the key of the tab that the command prints
	if readErr == nil && len(found) == 1 {
		keep = found[0].Key
	}
	ours := claim.Keys()
	var endErr error
	for _, key := range ours {
		if key != keep {
			endErr = errors.Join(endErr, end(key, tabIDs[key]))
		}
	}
	if endErr != nil && slices.Contains(ours, keep) {
		endErr = errors.Join(endErr, end(keep, tabIDs[keep]))
	}
	switch {
	case readErr != nil:
		return Result{}, errors.Join(readErr, endErr)
	case endErr != nil:
		return Result{}, endErr
	case len(found) == 0:
		return Result{}, &core.CodedError{Code: ExitNoMatch, Err: errors.New("no tab shows the target")}
	case len(found) == 1:
		return Result{Text: found[0].ID, Data: map[string]any{"tab": found[0].ID}}, nil
	}
	ids := make([]string, len(found))
	for i, t := range found {
		ids[i] = t.ID
	}
	return Result{}, fmt.Errorf("%d tabs show the target: %s", len(found), strings.Join(ids, ", "))
}
