// Package wip knows which WebKit Inspector Protocol commands each iOS
// version's backend accepts (design docs/design/agentic-parity.md, sections
// 2.10 and 5.4). Unknown methods can wedge a page target (MAP fact 9), so
// the daemon refuses a raw call of a method that the device's list lacks.
package wip

//go:generate go run gen/main.go /home/devkit/.pi/artifacts/retro/browser-checker-20261106/analysis/sources/webkit-legacy-ios

import (
	"strconv"
	"strings"
	"sync"
)

type commandList struct {
	major, minor int
	methods      string
}

var (
	setsOnce sync.Once
	sets     []map[string]bool // parallel to commandLists
)

func buildSets() {
	sets = make([]map[string]bool, len(commandLists))
	for i, l := range commandLists {
		sets[i] = make(map[string]bool)
		for _, m := range strings.Fields(l.methods) {
			sets[i][m] = true
		}
	}
}

// parseVersion reads "16.7.10" as (16, 7).
func parseVersion(v string) (major, minor int, ok bool) {
	parts := strings.Split(v, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	if len(parts) > 1 {
		if minor, err = strconv.Atoi(parts[1]); err != nil {
			return 0, 0, false
		}
	}
	return major, minor, true
}

// KnownVersion reports whether osVersion names an iOS release. "" (WebKitGTK,
// a device that iwdp lists without a version), an unparsable text and
// iwdp's 0.0.0 for the iOS Simulator do not: no list or form applies to
// them, and messages must not name them as an iOS version.
func KnownVersion(osVersion string) bool {
	major, _, ok := parseVersion(osVersion)
	return ok && major > 0
}

// Supported reports whether the backend of iOS osVersion accepts method.
// It uses the newest command list at or below osVersion. known is false
// when no list applies (empty or unparsable version, or older than every
// list); ok is then true, because there is nothing to check against.
func Supported(osVersion, method string) (known, ok bool) {
	if !KnownVersion(osVersion) {
		return false, true
	}
	major, minor, _ := parseVersion(osVersion)
	setsOnce.Do(buildSets)
	idx := -1
	for i, l := range commandLists {
		if l.major < major || (l.major == major && l.minor <= minor) {
			idx = i
		}
	}
	if idx < 0 {
		return false, true
	}
	return true, sets[idx][method]
}

// EmulatedConditionsBandwidth reports whether Network.setEmulatedConditions
// of iOS osVersion takes bandwidth and latency (27.3 and later, WebKit
// commit c4a7ebcc295f; also an unknown version (KnownVersion), such as
// WebKitGTK's or the iOS Simulator's 0.0.0, whose WebKit is current) rather
// than bytesPerSecondLimit (16.4 to 17.x). The CDP bridge and "emulate
// network" pick their parameters by it.
func EmulatedConditionsBandwidth(osVersion string) bool {
	if !KnownVersion(osVersion) {
		return true
	}
	major, minor, _ := parseVersion(osVersion)
	return major > 27 || major == 27 && minor >= 3
}
