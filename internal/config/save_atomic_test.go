package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// An existing, too-open file ends up 0600 with the new content.
	if err := os.WriteFile(path, []byte("server:\n  token: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.Server.Token = "new"
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	got, err := LoadFrom(path)
	if err != nil || got.Server.Token != "new" {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}

	// A config that doesn't exist yet is created, with its directory.
	fresh := filepath.Join(dir, "sub", "config.yaml")
	if err := SaveAtomic(fresh, cfg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("fresh config: %v %v", fi, err)
	}
}
