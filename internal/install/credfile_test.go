package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestWriteSecretFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	path := filepath.Join(dir, "token")
	if err := writeSecretFile(path, "one\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, "two\n"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "two\n" {
		t.Fatalf("content = %q, %v", b, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}

	// A directory someone made world-readable is tightened.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, "three\n"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v after a rewrite, want 0700", info.Mode().Perm())
	}
}

func TestWriteSecretFile_RefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "credentials")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(link, "secret\n"); err == nil {
		t.Error("a symlinked credential file must be refused")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("the link target was written through: %q", b)
	}

	// The directory itself being a link is no better.
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(filepath.Join(linkDir, "other"), "secret\n"); err == nil {
		t.Error("a symlinked credentials directory must be refused")
	}
}

func TestReadSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := readSecretFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
	if err := os.WriteFile(path, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecretFile(path); err != nil || got != "abc" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if !secretFileIs(path, "abc") || secretFileIs(path, "abd") || !secretFileIs(path, "") {
		t.Error("secretFileIs: want a match on the value, and any value for \"\"")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("mode 0640: err = %v", err)
	}
	if secretFileIs(path, "abc") {
		t.Error("a group-readable credential file must fail its step's check, so the step rewrites it 0600")
	}
	if err := writeSecretFile(path, "abc\n"); err != nil {
		t.Fatal(err)
	}
	if !secretFileIs(path, "abc") {
		t.Error("rewritten 0600, it matches again")
	}
}

func TestSecretLine(t *testing.T) {
	if secretLine("x", "abc") != nil {
		t.Error("a single line is fine")
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\n"} {
		if secretLine("x", bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestCredentialPaths_MatchWhatGroveBrokerReads(t *testing.T) {
	for _, env := range []string{"", filepath.Join(t.TempDir(), "custom", "config.yaml")} {
		t.Setenv("GROVE_CONFIG", env)
		t.Setenv("HOME", t.TempDir())
		opts := Options{Home: os.Getenv("HOME")}
		dir, err := config.CredentialsDir()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := credentialPath(opts, brokerCredentialName), filepath.Join(dir, config.BrokerCredentialFile); got != want {
			t.Errorf("GROVE_CONFIG=%q: install writes %s, grove broker reads %s", env, got, want)
		}
	}
}
