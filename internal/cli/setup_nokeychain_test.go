package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

// noKeychainRunner fails the Keychain bridge the way a shell reached with `su - <user>` does.
type noKeychainRunner struct{ *install.FakeRunner }

func (noKeychainRunner) RunWithStdinAttached(context.Context, string, string, ...string) (string, string, error) {
	return "", "", errors.New("open interactive terminal for Keychain authorization: no tty")
}

// A su shell has no default keychain. The worker and broker use files, so setup warns and still
// records the control plane instead of failing the whole install.
func TestSaveJoinedServerWarnsWhenThereIsNoKeychain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("GROVE_CONFIG", path)
	var warn strings.Builder
	runner := noKeychainRunner{install.NewFakeRunner()}
	if err := saveJoinedServer(context.Background(), runner, &warn, "http://100.64.0.1:6130", "device-1", "fake-device-secret"); err != nil {
		t.Fatalf("saveJoinedServer: %v", err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.URL != "http://100.64.0.1:6130" || cfg.Server.TokenKeychain != "" {
		t.Errorf("config = %+v; want the URL, and no pointer to a Keychain item that was never stored", cfg.Server)
	}
	if !strings.Contains(warn.String(), "warning") || strings.Contains(warn.String(), "fake-device-secret") {
		t.Errorf("warning = %q", warn.String())
	}
}
