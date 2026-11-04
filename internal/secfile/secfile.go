// Package secfile is the check every reader of a file that holds secrets (config.yaml,
// secrets.json, devices.json, github.json) applies before trusting it: it must be a plain file,
// not a symlink, and owned by the user running grove. It matches internal/install's credfile.go
// (writeSecretFile / checkOwnedByUs), which does the same for the credentials directory.
package secfile

import (
	"fmt"
	"os"
)

// Open validates path and then makes it owner-only (0600). A missing path is os.ErrNotExist
// (wrapped, so errors.Is works). A symlink, a non-regular file or a file another user owns is
// refused: following or trusting it could hand a secret to, or take one from, someone else.
func Open(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; grove refuses to read a secrets file through one (replace it with the real file)", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("secure %s: %w", path, err)
		}
	}
	return nil
}

// ReadFile is Open followed by os.ReadFile.
func ReadFile(path string) ([]byte, error) {
	if err := Open(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
