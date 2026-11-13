package install

import (
	"errors"
	"runtime"
	"testing"
)

// A LaunchAgent's PATH has no Homebrew directory. `grove serve` on the control plane then found
// no tailscale CLI and turned Tailscale identity auth off for every caller.
func TestFindTailscaleLooksInHomebrewWithoutPATH(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew fallback is macOS only")
	}
	noPath := func(string) (string, error) { return "", errors.New("not in PATH") }
	for _, want := range tailscaleHomebrewBinaries {
		got, err := FindTailscale(noPath, func(p string) bool { return p == want })
		if err != nil || got != want {
			t.Fatalf("FindTailscale with only %s present = %q, %v", want, got, err)
		}
	}
	// The App Store bundle alone is still not a CLI.
	if _, err := FindTailscale(noPath, func(p string) bool { return p == TailscaleAppStoreBundle }); !errors.Is(err, ErrAppStoreTailscale) {
		t.Fatalf("App Store bundle only: err = %v, want ErrAppStoreTailscale", err)
	}
}
