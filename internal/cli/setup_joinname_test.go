package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gm2211/grove/internal/install"
)

// A re-run of `grove setup` on a Mac whose worker is installed as "studio" must join as "studio".
// It joined under the hostname ("Mac") instead: the control plane issued credentials for "Mac",
// refused the worker still running as "studio", and the Mac's VMs were deleted.
func TestSetupJoinsUnderTheInstalledWorkerName(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	home := t.TempDir()
	opts := install.Options{Home: home}
	hostname := func() (string, error) { return "Mac", nil }

	if got := setupJoinName(opts, hostname); got != "Mac" {
		t.Fatalf("no worker installed: join name = %q, want the hostname", got)
	}

	dir := filepath.Join(home, ".config", "grove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-identity.json"), []byte(`{"name":"studio","labels":"host=studio"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := setupJoinName(opts, hostname); got != "studio" {
		t.Fatalf("worker installed as studio: join name = %q, want studio", got)
	}
}
