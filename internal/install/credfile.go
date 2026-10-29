package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A worker Mac's secrets (Orchard's bootstrap token, the broker's credential, registry
// credentials) live in 0600 files under ~/.config/grove/credentials, not in the login Keychain: the
// worker and the broker run as boot-time LaunchDaemons for a user nobody has logged in as, and
// that user's Keychain is locked, or doesn't exist, until someone does. A file only that user can
// read is what the daemon itself can open. The wrapper reads it into a pipe or a variable, so a
// secret still never appears in argv, a plist, or the wrapper's text.
const (
	workerBootstrapCredentialName = "orchard-worker-bootstrap"
	brokerCredentialName          = "broker" // config.BrokerCredentialFile
	registryCredentialName        = "registry"
)

func credentialsDir(opts Options) string { return filepath.Join(opts.ConfigDir(), "credentials") }

func credentialPath(opts Options, name string) string {
	return filepath.Join(credentialsDir(opts), name)
}

// writeSecretFile writes content to path (mode 0600) in a directory only this user can enter. It
// refuses to follow a symlink or to replace a file another user owns, and replaces the file
// atomically, so a reader never sees a half-written secret.
func writeSecretFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkOwnedByUs(dir, dirInfo); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace %s: it is not a regular file", path)
		}
		if err := checkOwnedByUs(path, info); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readSecretFile returns the file's content without its trailing newline. A missing file is
// os.ErrNotExist; a file group or world can read, or that is not a plain file, is an error, because
// a secret that was ever that open is not one to keep using.
func readSecretFile(path string) (string, error) {
	s, err := readSecretFileRaw(path)
	return strings.TrimRight(s, "\r\n"), err
}

// readSecretFileRaw is readSecretFile without trimming, for a file of several lines.
func readSecretFileRaw(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users (mode %04o)", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// secretFileIs reports whether path holds exactly want (or, with want == "", any non-empty secret).
// Any problem reading it counts as "no": the step that owns the file then rewrites it.
func secretFileIs(path, want string) bool {
	got, err := readSecretFile(path)
	if err != nil || got == "" {
		return false
	}
	return want == "" || got == want
}

// secretLine rejects a secret that can't be one line of a file the wrapper reads line by line.
func secretLine(what, v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("%s must be a single line", what)
	}
	return nil
}
