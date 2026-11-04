//go:build darwin || linux

package secfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRefusesForeignOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := getuid
	getuid = func() int { return os.Getuid() + 1 } // the file now looks foreign-owned
	t.Cleanup(func() { getuid = orig })
	if err := Open(p); err == nil {
		t.Fatal("foreign-owned file accepted")
	}
}
