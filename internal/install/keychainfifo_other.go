//go:build !darwin && !linux

package install

import "errors"

// DeliverKeychainUnlock is macOS's (and Linux's, for tests); elsewhere there is no worker wrapper.
func DeliverKeychainUnlock(path, password string) error {
	return errors.New("keychain unlock is only supported on macOS")
}
