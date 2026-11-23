// Package sim starts, checks and stops an iOS Simulator that runs on a Mac
// that this host reaches over SSH (a macOS VM or a real Mac), and connects
// its Web Inspector to ios_webkit_debug_proxy on this host: iwdp finds a
// simulator on 127.0.0.1:27753 (its -s address), and sim forwards that port
// over SSH to the simulator's webinspectord_sim socket on the Mac.
package sim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config says where the simulator runs. It is read from ConfigPath.
type Config struct {
	// SSH is the ssh destination of the Mac: a Host of SSHConfig or of the
	// user's ssh configuration, or user@host. ssh must log in without a
	// prompt (BatchMode).
	SSH string `json:"ssh"`
	// SSHConfig is the ssh configuration file (ssh -F); "" for ssh's own.
	SSHConfig string `json:"sshConfig,omitempty"`
	// Start is the command that this host runs when the Mac does not answer
	// over SSH, for example ["reims-ctl", "start", "macos-tahoe"] for a VM;
	// none means sim up fails instead.
	Start []string `json:"start,omitempty"`
	// Stop is the command that 'sim down --mac' runs on this host.
	Stop []string `json:"stop,omitempty"`
	// Device is the simulator to use: its UDID or its exact name. "" means
	// the one booted iOS simulator.
	Device string `json:"device,omitempty"`
}

// ErrNoConfig is the error of Load when the configuration file is missing.
var ErrNoConfig = errors.New("no simulator configuration")

// ConfigPath is the configuration file: $XDG_CONFIG_HOME/iwdp-cli/simulator.json
// (os.UserConfigDir).
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the simulator configuration: %w", err)
	}
	return filepath.Join(dir, "iwdp-cli", "simulator.json"), nil
}

// configExample is the configuration that the error of a missing file shows.
const configExample = `{"ssh": "my-mac", "start": ["reims-ctl", "start", "macos-tahoe"], "device": "iPhone 17 Pro"}`

// Load reads the configuration at path. A missing file is ErrNoConfig, with
// the path and an example; an unknown field or a missing ssh destination is
// an error that names it. "~/" at the start of SSHConfig is the home
// directory.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%w: create %s, for example %s (docs/simulator.md)", ErrNoConfig, path, configExample)
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading the simulator configuration: %w", err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("reading the simulator configuration %s: %w", path, err)
	}
	if strings.TrimSpace(c.SSH) == "" {
		return Config{}, fmt.Errorf("the simulator configuration %s names no ssh destination (\"ssh\")", path)
	}
	if strings.HasPrefix(c.SSH, "-") {
		return Config{}, fmt.Errorf("the simulator configuration %s: ssh destination %q starts with \"-\"", path, c.SSH)
	}
	if rest, ok := strings.CutPrefix(c.SSHConfig, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("expanding sshConfig %q: %w", c.SSHConfig, err)
		}
		c.SSHConfig = filepath.Join(home, rest)
	}
	return c, nil
}
