package proxy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// BuildConfig builds the -c config string for ios_webkit_debug_proxy.
//
// iwdp enumerates the SIMULATOR pseudo-device before real devices, so a plain
// wildcard range lets that ghost entry take DefaultFirstDevicePort and pushes
// the first real device to the next port. Pinning the first UDID to
// DefaultFirstDevicePort keeps that port on a real device; the SIMULATOR
// ghost and any extra devices use the range after it.
func BuildConfig(udids []string) string {
	if len(udids) == 0 {
		return fmt.Sprintf("null:%d,:%d-%d", DefaultListingPort, DefaultFirstDevicePort, DefaultLastDevicePort)
	}
	return fmt.Sprintf("null:%d,%s:%d,:%d-%d", DefaultListingPort, udids[0], DefaultFirstDevicePort, DefaultFirstDevicePort+1, DefaultLastDevicePort)
}

// udidWait bounds one run of idevice_id, which asks usbmuxd and returns at
// once on a healthy host.
const udidWait = 3 * time.Second

// DeviceUDIDs returns the UDIDs of the USB devices that usbmuxd reports
// (idevice_id -l), one per line, within udidWait and ctx, or the error of
// that read (a missing or failing idevice_id, the end of ctx). Callers
// that pin ports treat an error as no device, so BuildConfig falls back to
// the plain wildcard range (the behavior before pinning existed).
func DeviceUDIDs(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, udidWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "idevice_id", "-l")
	// A child that keeps the output pipe open (a wrapper script) does not
	// hold Output past the end of ctx.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing USB devices with idevice_id -l: %w", err)
	}
	var udids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			udids = append(udids, line)
		}
	}
	return udids, nil
}
