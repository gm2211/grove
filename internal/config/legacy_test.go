package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config.yaml written while grove used Nomad (and joined shared VMs to the tailnet) keeps
// loading; its keys are reported by LegacyKeys and dropped by ClearLegacy.
func TestLegacyNomadAndTailscaleKeysStillLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "orchard:\n  url: http://127.0.0.1:6120\nnomad:\n  url: http://127.0.0.1:4646\n  token: nomad-secret\ntailscale:\n  authKey: tskey-secret\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("an old config.yaml must still load: %v", err)
	}
	if got := strings.Join(cfg.LegacyKeys(), ","); got != "nomad.url,nomad.token,tailscale.authKey" {
		t.Errorf("LegacyKeys = %s", got)
	}
	cfg.ClearLegacy()
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, gone := range []string{"nomad", "tailscale", "secret"} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("%q survived ClearLegacy:\n%s", gone, raw)
		}
	}
	if !strings.Contains(string(raw), "http://127.0.0.1:6120") {
		t.Errorf("ClearLegacy dropped orchard:\n%s", raw)
	}
	if (Config{}).LegacyKeys() != nil {
		t.Error("a fresh config has no legacy keys")
	}
}
