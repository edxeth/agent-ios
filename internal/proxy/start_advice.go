package proxy

import (
	"context"
	"io"
	"os"
	"strings"
)

// Advice for a failed iwdp start, from what the started iwdp wrote to its
// log and from usbmuxd's device list.

// inspectorRefused is the start of the line that ios_webkit_debug_proxy
// 1.9.2 prints when a device's inspector connection closes before any
// inspector data arrived (iwdp_log_disconnect), which a locked device
// caused in the device check of 2026-11-11. iwdp attaches a device only
// on usbmuxd's attach event and runs the attach inside its one event
// loop, so during such an attach its listing does not answer, and it
// never tries the device again.
const inspectorRefused = "Unable to connect to "

// maxStartLog bounds how much of a start's log startAdvice reads.
const maxStartLog = 64 << 10

// startAdvice returns advice for the failed start whose output begins at
// offset start of the iwdp log path, prefixed with "; ", or "" when it
// has none. A device whose inspector refused the connection needs to be
// unlocked. An iwdp that exited (exited) while usbmuxd reports no device
// may have found no usbmuxd: iwdp exits when it cannot subscribe to
// usbmuxd, and some systems run usbmuxd only while a device is plugged in.
// An iwdp that did not answer while usbmuxd reports a device may still be
// connecting to it: iwdp connects a device inside its one event loop.
func (s *Starter) startAdvice(ctx context.Context, path string, start int64, exited bool) string {
	if strings.Contains(readFrom(path, start), inspectorRefused) {
		return "; a device did not accept the Web Inspector connection (the iwdp log has \"" + inspectorRefused + "...\"): " + deviceAdvice + ", then run the command again"
	}
	udids, err := s.attached(ctx)
	if err != nil {
		return "" // an unknown device list supports no device advice
	}
	attached := len(udids)
	switch {
	case exited && attached == 0:
		return "; usbmuxd reports no USB device, and ios_webkit_debug_proxy exits when usbmuxd does not run, which some systems start only while a device is plugged in: connect the device with a cable and unlock it"
	case !exited && attached > 0:
		// Device check of 2026-11-11: a restart right after a replug did
		// not answer for 5 s and logged nothing about the device; the
		// start 30 s later answered at once.
		return "; ios_webkit_debug_proxy does not answer while it connects to a USB device, which can take long right after the device is plugged in: keep the device unlocked and run the command again after the start back-off (" + StartBackoff.String() + ")"
	}
	return ""
}

// readFrom returns at most maxStartLog bytes of the file path from offset
// start, or "" when it cannot be read.
func readFrom(path string, start int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, start, maxStartLog), maxStartLog))
	if err != nil {
		return ""
	}
	return string(data)
}
