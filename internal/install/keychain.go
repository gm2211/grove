package install

import (
	"context"
	"fmt"
)

// StoreKeychainSecret replaces a generic-password item without using security's -U flag.
// On macOS, -U may try to change an existing item's ACL and display a login-keychain password
// dialog. Deleting the old item first keeps unattended setup deterministic while preserving the
// explicit trust restriction on the newly created item. The secret is supplied only on stdin.
func StoreKeychainSecret(ctx context.Context, r Runner, account, service, secret string) error {
	// Missing item is expected on first install, so deletion errors are intentionally ignored.
	_, _, _ = r.Run(ctx, "/usr/bin/security", "delete-generic-password", "-a", account, "-s", service)
	input := secret + "\n" + secret + "\n"
	if _, _, err := r.RunWithStdin(ctx, input, "/usr/bin/security", "add-generic-password", "-a", account, "-s", service, "-T", "/usr/bin/security", "-w"); err != nil {
		return fmt.Errorf("store Keychain credential: %w", err)
	}
	return nil
}
