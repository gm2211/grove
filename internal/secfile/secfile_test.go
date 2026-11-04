package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAcceptsOwnFileAndTightensMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Open(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestOpenMissingIsNotExist(t *testing.T) {
	if err := Open(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := Open(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestOpenRefusesDirectory(t *testing.T) {
	if err := Open(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
}
