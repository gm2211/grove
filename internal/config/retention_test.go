package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactRetentionParsesAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "artifacts:\n  endpoint: http://127.0.0.1:9000\n  bucket: grove\n  retention:\n    success: 5m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Artifacts.Retention.WithDefaults()
	if got.Success != 5*time.Minute || got.Failure != DefaultArtifactRetentionFailure {
		t.Errorf("retention = %+v, want success 5m and the default failure", got)
	}
	if d := (ArtifactRetention{}).WithDefaults(); d.Success != 15*time.Minute || d.Failure != time.Hour {
		t.Errorf("defaults = %+v, want 15m/1h", d)
	}
}

// TestLegacyArtifactKeysStillLoad: a config.yaml written when artifacts lived in MinIO must keep
// loading. Its S3 keys are reported by LegacyKeys (so serve can warn) and dropped by ClearLegacy.
func TestLegacyArtifactKeysStillLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "server:\n  listen: 127.0.0.1:6130\nartifacts:\n  endpoint: http://127.0.0.1:9000\n  bucket: grove\n" +
		"  accessKey: grove-artifacts-x\n  secretKey: old-secret\n  someFutureKey: 1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("an old config.yaml must still load: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:6130" || cfg.Artifacts.Dir != "" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	want := "artifacts.endpoint,artifacts.bucket,artifacts.accessKey,artifacts.secretKey"
	if got := strings.Join(cfg.Artifacts.LegacyKeys(), ","); got != want {
		t.Errorf("LegacyKeys = %s, want %s", got, want)
	}
	cfg.Artifacts.ClearLegacy()
	cfg.Artifacts.Dir = "/var/lib/grove-artifacts"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "old-secret") || strings.Contains(string(raw), "endpoint") {
		t.Errorf("legacy keys survived ClearLegacy:\n%s", raw)
	}
	if !strings.Contains(string(raw), "dir: /var/lib/grove-artifacts") {
		t.Errorf("artifacts.dir not saved:\n%s", raw)
	}
	if (ArtifactsConfig{}).LegacyKeys() != nil {
		t.Error("a fresh config has no legacy keys")
	}
}

func TestArtifactsDirRelativeToConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("artifacts:\n  dir: store\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Artifacts.Dir != filepath.Join(dir, "store") {
		t.Errorf("Dir = %q, want it resolved against the config file's directory", cfg.Artifacts.Dir)
	}
}
