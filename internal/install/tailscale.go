package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
)

// TailscaleAppStoreBundle is where the Mac App Store build of Tailscale.app lives. Its CLI binary
// does not run before login, so it cannot supervise a headless worker Mac.
const TailscaleAppStoreBundle = "/Applications/Tailscale.app"

// tailscaleStandaloneBinary is where the standalone (non-App Store) installer puts the CLI.
const tailscaleStandaloneBinary = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// ErrAppStoreTailscale is returned by FindTailscale when only the Mac App Store variant is
// present: its CLI binary does not run before login, so it can't supervise a headless Mac.
var ErrAppStoreTailscale = errors.New(
	"only the Mac App Store build of Tailscale is installed; its CLI doesn't run before login, " +
		"so it can't come up unattended on a headless Mac. Install the standalone variant from " +
		"https://tailscale.com/download/macos and remove the App Store app")

// FindTailscale locates the tailscale CLI, looking first at the standalone app bundle's binary,
// then in PATH via lookPath. If neither is found but the App Store app bundle exists (without a
// working CLI), it returns ErrAppStoreTailscale. exists checks whether a path is present (tests
// pass a fake so they aren't at the mercy of what's actually installed on the machine running
// them); a nil exists defaults to a real os.Stat-backed check.
func FindTailscale(lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if exists == nil {
		exists = defaultPathExists
	}
	if runtime.GOOS == "darwin" {
		if exists(tailscaleStandaloneBinary) {
			return tailscaleStandaloneBinary, nil
		}
	}
	if path, err := lookPath("tailscale"); err == nil {
		return path, nil
	}
	if runtime.GOOS == "darwin" {
		if exists(TailscaleAppStoreBundle) {
			return "", ErrAppStoreTailscale
		}
	}
	return "", errors.New("tailscale not found: install it from https://tailscale.com/download")
}

func defaultPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TailscaleStatus is the subset of `tailscale status --json` grove cares about.
type TailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
	BackendState string `json:"BackendState"`
}

// TailnetIP returns the first tailnet IP, or "" if none.
func (s TailscaleStatus) TailnetIP() string {
	if len(s.Self.TailscaleIPs) == 0 {
		return ""
	}
	return s.Self.TailscaleIPs[0]
}

// GetTailscaleStatus runs `<bin> status --json` and parses it.
func GetTailscaleStatus(ctx context.Context, r Runner, bin string) (*TailscaleStatus, error) {
	stdout, _, err := r.Run(ctx, bin, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st TailscaleStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	return &st, nil
}
