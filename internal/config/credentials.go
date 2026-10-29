package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CredentialsDir is where a worker Mac keeps its secrets as 0600 files: <grove config dir>/
// credentials (~/.config/grove/credentials, or next to $GROVE_CONFIG). Files, not Keychain items,
// because the worker and the broker run as boot-time LaunchDaemons that nobody has logged in
// for, and a login Keychain is locked until someone does.
func CredentialsDir() (string, error) {
	if p := os.Getenv("GROVE_CONFIG"); p != "" {
		return filepath.Join(filepath.Dir(p), "credentials"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "grove", "credentials"), nil
}

// BrokerCredentialFile is the file, under CredentialsDir, holding this worker's broker credential.
const BrokerCredentialFile = "broker"

// readCredentialFile returns the trimmed content of a credential file. It refuses a symlink, a
// file that is not a regular file, and any file group or world can read.
func readCredentialFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users (mode %04o); run `chmod 600` on it", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
