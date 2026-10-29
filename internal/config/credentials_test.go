package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBrokerToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GROVE_CONFIG", filepath.Join(dir, "config.yaml"))
	credDir := filepath.Join(dir, "credentials")
	path := filepath.Join(credDir, BrokerCredentialFile)

	var keychain string
	var keychainErr error
	var keychainReads int
	orig := keychainReader
	keychainReader = func(context.Context, string, string) (string, error) {
		keychainReads++
		return keychain, keychainErr
	}
	t.Cleanup(func() { keychainReader = orig })

	// No file: the Keychain item an older grove stored is the fallback.
	keychain = "from-keychain"
	if got, err := ResolveBrokerToken(context.Background()); err != nil || got != "from-keychain" {
		t.Fatalf("fallback = %q, %v; want the Keychain item", got, err)
	}
	keychainErr = errors.New("User interaction is not allowed")
	if _, err := ResolveBrokerToken(context.Background()); err == nil || !strings.Contains(err.Error(), "grove setup") {
		t.Fatalf("no file and no Keychain: err = %v, want one pointing at `grove setup`", err)
	}

	// A 0600 file wins and the Keychain is not consulted.
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keychainReads = 0
	if got, err := ResolveBrokerToken(context.Background()); err != nil || got != "from-file" {
		t.Fatalf("file = %q, %v; want from-file", got, err)
	}
	if keychainReads != 0 {
		t.Error("Keychain was read although the credential file exists")
	}

	// Readable by others: refused, and not silently replaced by the Keychain item.
	keychainErr = nil
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveBrokerToken(context.Background()); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("mode 0644: err = %v, want a refusal", err)
	}

	// Empty file: the existing "run grove setup" error.
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveBrokerToken(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file: err = %v, want an empty-credential error", err)
	}

	// A symlink is not a credential file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveBrokerToken(context.Background()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink: err = %v, want a refusal", err)
	}
}
