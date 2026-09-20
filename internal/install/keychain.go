package install

import (
	"context"
	"fmt"
	"strings"
)

const keychainExpectScript = `
log_user 0
set timeout 120
if {[gets stdin account] < 0} { exit 2 }
if {[gets stdin service] < 0} { exit 2 }
if {[gets stdin secret] < 0} { exit 2 }
spawn /usr/bin/security add-generic-password -a $account -s $service -w
expect {
  -exact "password data for new item:" { send -- "$secret\r" }
  timeout { exit 3 }
  eof { catch wait result; exit [lindex $result 3] }
}
expect {
  -exact "retype password for new item:" { send -- "$secret\r" }
  timeout { exit 4 }
  eof { catch wait result; exit [lindex $result 3] }
}
unset secret
expect eof
catch wait result
exit [lindex $result 3]
`

// StoreKeychainSecret replaces a generic-password item through security's interactive interface.
// macOS requires a pseudo-terminal to authorize Keychain item creation, but security has no safe
// password-stdin flag. expect supplies that pseudo-terminal while reading account, service, and
// secret from stdin. log_user stays disabled, so the secret cannot be echoed into captured output.
// Omitting -T uses security's documented default: trust only the application creating the item,
// /usr/bin/security. Omitting -U avoids ACL-change login-password dialogs on replacement.
func StoreKeychainSecret(ctx context.Context, r Runner, account, service, secret string) error {
	if strings.ContainsAny(account, "\r\n") || strings.ContainsAny(service, "\r\n") || strings.ContainsAny(secret, "\r\n") {
		return fmt.Errorf("Keychain credential fields must be single-line")
	}
	// Missing item is expected on first install, so deletion errors are intentionally ignored.
	_, _, _ = r.Run(ctx, "/usr/bin/security", "delete-generic-password", "-a", account, "-s", service)
	input := account + "\n" + service + "\n" + secret + "\n"
	if _, _, err := r.RunWithStdinAttached(ctx, input, "/usr/bin/expect", "-c", keychainExpectScript); err != nil {
		return fmt.Errorf("store Keychain credential: %w", err)
	}
	return nil
}
