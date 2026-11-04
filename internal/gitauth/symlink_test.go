package gitauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRefusesSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "store.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, err := NewStore(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

func TestStoreTightensLooseMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}
