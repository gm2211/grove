package cli

import (
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

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

	configSetCmd.SetIn(strings.NewReader("nomad-secret-token\n"))
	var out strings.Builder
	configSetCmd.SetOut(&out)
	t.Cleanup(func() {
		configSetCmd.SetIn(nil)
		configSetCmd.SetOut(nil)
	})

	if err := configSetCmd.RunE(configSetCmd, []string{"nomad.token", "-"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "nomad-secret-token") {
		t.Fatalf("config set echoed the secret: %q", out.String())
	}

	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nomad.Token != "nomad-secret-token" {
		t.Fatalf("Nomad.Token = %q, want the value read from stdin", cfg.Nomad.Token)
	}
}

func TestSetConfigKey_ArtifactsDirAndRetiredS3Keys(t *testing.T) {
	cfg := &config.Config{}
	if err := setConfigKey(cfg, "artifacts.dir", "/srv/grove/artifacts"); err != nil {
		t.Fatal(err)
	}
	if cfg.Artifacts.Dir != "/srv/grove/artifacts" {
		t.Errorf("artifacts.dir = %q", cfg.Artifacts.Dir)
	}
	for _, key := range []string{"artifacts.endpoint", "artifacts.bucket", "artifacts.accessKey", "artifacts.secretKey", "tailscale.authKey"} {
		err := setConfigKey(cfg, key, "x")
		if err == nil || !strings.Contains(err.Error(), "no longer used") {
			t.Errorf("setConfigKey(%s) err = %v, want a retired-key error", key, err)
		}
	}
	if len(cfg.Artifacts.LegacyKeys()) != 0 {
		t.Error("retired keys must not be written")
	}
}

func TestSetConfigKey_LocalSocket(t *testing.T) {
	cfg := &config.Config{}
	if err := setConfigKey(cfg, "server.socket", "/Users/Shared/grove/api.sock"); err != nil {
		t.Fatal(err)
	}
	if err := setConfigKey(cfg, "server.socketUsers", "giulio, argos,"); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Socket != "/Users/Shared/grove/api.sock" || strings.Join(cfg.Server.SocketUsers, "|") != "giulio|argos" {
		t.Fatalf("server = %+v", cfg.Server)
	}
}
