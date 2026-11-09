package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
)

func cmdRestartIWDP(ctx context.Context) {
	if err := proxy.Restart(ctx); err != nil {
		fatal(err)
	}
	fmt.Println("ios_webkit_debug_proxy restarted")
}

// iwdpLogLine names the log of an iwdp that iwdp-mcp starts (its stdout
// and stderr, proxy.LogPath).
func iwdpLogLine() string {
	p, err := proxy.LogPath()
	if err != nil {
		return "iwdp log of an iwdp that iwdp-mcp starts: unknown (" + err.Error() + ")"
	}
	return "iwdp log of an iwdp that iwdp-mcp starts: " + p
}

func cmdStatus(ctx context.Context) {
	err := proxy.CheckListing(ctx, proxy.ListingAddr())
	notListening := errors.Is(err, proxy.ErrNotListening)
	switch {
	case err == nil:
		fmt.Println("ios_webkit_debug_proxy is running")
	case notListening:
		fmt.Println("ios_webkit_debug_proxy is NOT running")
	}
	// Every outcome names the log, before any exit: a broken listing is
	// when the user needs it most.
	fmt.Println(iwdpLogLine())
	switch {
	case notListening:
		fmt.Fprintln(os.Stderr, "Start it with: "+proxy.ManualCommand())
		os.Exit(1)
	case err != nil:
		// A process owns the listing port but does not answer as iwdp.
		fatal(err)
	}
}

func cmdReload(ctx context.Context, args []string) {
	client, err := connectToPage(ctx, args)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	ignoreCache := false
	for _, a := range args {
		if a == "--ignore-cache" {
			ignoreCache = true
		}
	}
	if err := tools.Reload(ctx, client, ignoreCache); err != nil {
		fatal(err)
	}
	fmt.Println("Page reloaded")
}

func cmdSnapshotNode(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli snapshot-node <nodeId> [-o file] [ws-url]")
		os.Exit(1)
	}

	nodeID, err := strconv.Atoi(args[0])
	if err != nil {
		fatal(fmt.Errorf("invalid nodeId: %s", args[0]))
	}

	const usage = "Usage: iwdp-cli snapshot-node <nodeId> [-o file] [ws-url]"
	outputFile := ""
	var wsArgs []string
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				usageError(usage, fmt.Errorf("%s needs a value", a))
			}
			outputFile = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			usageError(usage, fmt.Errorf("unknown option %s", a))
		default:
			wsArgs = append(wsArgs, a)
		}
	}

	client, err := connectToPage(ctx, wsArgs)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	dataURL, err := tools.SnapshotNode(ctx, client, nodeID)
	if err != nil {
		fatal(err)
	}
	path, info, err := saveCapture(dataURL, outputFile, "iwdp-snapshot")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Snapshot saved to %s (%s)\n", path, info)
}
