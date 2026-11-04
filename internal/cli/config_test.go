package cli

import (
	"os"
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

func TestConfigShowNeverPrintsSecrets(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	t.Setenv("GROVE_CONFIG", path)
	cfg := &config.Config{}
	cfg.Nomad.Token = "nomad-SECRET-1"
	cfg.Orchard.Token = "orchard-SECRET-2"
	cfg.Server.Token = "server-SECRET-3"
	cfg.Enrollment.IssuerToken = "issuer-SECRET-4"
	cfg.Orchard.URL = "http://orchard.example"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	configShowCmd.SetOut(&out)
	t.Cleanup(func() { configShowCmd.SetOut(nil) })
	if err := configShowCmd.RunE(configShowCmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("config show printed a secret:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "<set, len 14>") || !strings.Contains(out.String(), "http://orchard.example") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestConfigSetWarnsOnArgvSecret(t *testing.T) {
	t.Setenv("GROVE_CONFIG", t.TempDir()+"/config.yaml")
	var stderr, stdout strings.Builder
	configSetCmd.SetErr(&stderr)
	configSetCmd.SetOut(&stdout)
	t.Cleanup(func() { configSetCmd.SetErr(nil); configSetCmd.SetOut(nil) })
	if err := configSetCmd.RunE(configSetCmd, []string{"nomad.token", "argv-secret"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "grove config set nomad.token -") || strings.Contains(stderr.String()+stdout.String(), "argv-secret") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	stderr.Reset()
	if err := configSetCmd.RunE(configSetCmd, []string{"nomad.url", "http://x"}); err != nil || stderr.Len() != 0 {
		t.Fatalf("non-secret key warned: %v %q", err, stderr.String())
	}
}

func TestResolveInstallToken(t *testing.T) {
	var warn strings.Builder
	if tok, err := resolveInstallToken(strings.NewReader("from-stdin\n"), &warn, "", "-", "env"); err != nil || tok != "from-stdin" || warn.Len() != 0 {
		t.Fatalf("stdin: %q %v %q", tok, err, warn.String())
	}
	f := t.TempDir() + "/tok"
	if err := os.WriteFile(f, []byte(" from-file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := resolveInstallToken(nil, &warn, "", f, ""); err != nil || tok != "from-file" {
		t.Fatalf("file: %q %v", tok, err)
	}
	if tok, err := resolveInstallToken(nil, &warn, "", "", "from-env"); err != nil || tok != "from-env" || warn.Len() != 0 {
		t.Fatalf("env: %q %v", tok, err)
	}
	if tok, err := resolveInstallToken(nil, &warn, "argv", "", "env"); err != nil || tok != "argv" || !strings.Contains(warn.String(), "--token-file") {
		t.Fatalf("flag: %q %v %q", tok, err, warn.String())
	}
	if _, err := resolveInstallToken(nil, &warn, "a", f, ""); err == nil {
		t.Fatal("--token with --token-file accepted")
	}
	if _, err := resolveInstallToken(strings.NewReader("\n"), &warn, "", "-", ""); err == nil {
		t.Fatal("empty stdin accepted")
	}
}
