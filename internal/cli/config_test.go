package cli

import (
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestSetConfigKey_TailscaleTags(t *testing.T) {
	var cfg config.Config
	if err := setConfigKey(&cfg, "tailscale.tags", " tag:grove-vm, tag:ci ,"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Tailscale.Tags, ","); got != "tag:grove-vm,tag:ci" {
		t.Fatalf("Tailscale.Tags = %q, want [tag:grove-vm tag:ci]", cfg.Tailscale.Tags)
	}
}

func TestReadConfigValue(t *testing.T) {
	got, err := readConfigValue(strings.NewReader("tskey-client-k1-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "tskey-client-k1-secret" {
		t.Fatalf("readConfigValue = %q, want the value without its trailing newline", got)
	}

	if _, err := readConfigValue(strings.NewReader("\n")); err == nil {
		t.Fatal("an empty value on stdin must be refused, not saved as a blank key")
	}
}

func TestConfigSet_ReadsValueFromStdin(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	t.Setenv("GROVE_CONFIG", path)

	configSetCmd.SetIn(strings.NewReader("tskey-auth-secret\n"))
	var out strings.Builder
	configSetCmd.SetOut(&out)
	t.Cleanup(func() {
		configSetCmd.SetIn(nil)
		configSetCmd.SetOut(nil)
	})

	if err := configSetCmd.RunE(configSetCmd, []string{"tailscale.authKey", "-"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "tskey-auth-secret") {
		t.Fatalf("config set echoed the secret: %q", out.String())
	}

	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tailscale.AuthKey != "tskey-auth-secret" {
		t.Fatalf("Tailscale.AuthKey = %q, want the value read from stdin", cfg.Tailscale.AuthKey)
	}
}
