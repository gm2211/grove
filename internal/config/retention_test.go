package config

import (
	"os"
	"path/filepath"
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
