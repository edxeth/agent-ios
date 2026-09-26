package proxy

import (
	"fmt"
	"os/exec"
	"strings"
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

// DeviceUDIDs returns the UDIDs of devices visible to usbmuxd, one per line.
// Best effort: any error yields nil, so BuildConfig falls back to the plain
// wildcard range (the behavior before pinning existed).
func DeviceUDIDs() []string {
	out, err := exec.Command("idevice_id", "-l").Output()
	if err != nil {
		return nil
	}
	var udids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			udids = append(udids, line)
		}
	}
	return udids
}
