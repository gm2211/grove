//go:build darwin || linux

package install

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// DeliverKeychainUnlock writes password, as one line, to the FIFO at path. It refuses anything
// but a FIFO this user owns (never a symlink or a regular file, which would store the password),
// and never blocks: with no reader it returns ErrNoUnlockReader, for the caller to retry.
func DeliverKeychainUnlock(path, password string) error {
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("the password must be one non-empty line")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoUnlockReader
		}
		return err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("%s is not a FIFO; refusing to write a password to it", path)
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return ErrNoUnlockReader
		}
		return err
	}
	defer f.Close()
	_, err = f.WriteString(password + "\n")
	return err
}
