package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A macOS guest boots only while the worker user's login keychain is unlocked:
// Virtualization.framework keeps its Secure Enclave host key there, and with the keychain locked
// every macOS VM fails with VZErrorDomain -9 "Failed to get current host key". A GUI login unlocks
// it, but a login does not survive a reboot or a crash, so a worker Mac whose worker user isn't
// logged in at the console loses its macOS pool at every restart.
//
// The worker LaunchDaemon runs in its own security session (SessionCreate), and a keychain
// unlocked inside that session is unlocked for the Tart it starts. An operator types the login
// password into the control panel (POST /workers/{name}/unlock-keychain); it reaches the wrapper
// through a FIFO and lives only in memory. Verified on a real worker Mac: with nobody logged in as
// the worker user, a macOS guest that failed with the host-key error booted once the daemon's
// session had unlocked the keychain.
//
// grove NEVER stores a login password: no file, no Keychain item, no config key, no flag. An
// older grove had `grove worker remember-login`, which kept it in credentials/login-keychain; the
// wrapper now deletes that file at start, and nothing may bring the option back.
//
// The password goes to `security -i` on stdin, as one interactive command line, never as an
// argument: `security unlock-keychain` takes it only with -p, which would put it in argv.

// legacyLoginKeychainCredentialName is the stored-password file of an older grove's
// `grove worker remember-login`. Only ever deleted, never written or read.
const legacyLoginKeychainCredentialName = "login-keychain"

// loginKeychainPath is the worker user's login keychain, relative to its home.
const loginKeychainPath = "Library/Keychains/login.keychain-db"

// securityQuote quotes s for one argument of a `security -i` command line: inside double quotes,
// with backslash and double quote escaped by a backslash.
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// unlockLoginKeychainLine is the `security -i` command that unlocks keychain with password.
func unlockLoginKeychainLine(password, keychain string) string {
	return "unlock-keychain -p " + securityQuote(password) + " " + securityQuote(keychain) + "\n"
}

// unlockFailed says whether `security -i`'s output reports a failed command. Interactive mode
// exits 0 whatever its commands did, and prints "security: <Function> …: <reason>" for one that
// failed.
func unlockFailed(stdout, stderr string) bool {
	out := stdout + stderr
	return strings.Contains(out, "SecKeychain") || strings.Contains(out, "security: ")
}

// UnlockLoginKeychain unlocks keychain (the worker user's login keychain when "") with password,
// in this process's security session, and says whether the password was right.
func UnlockLoginKeychain(ctx context.Context, r Runner, password, keychain string) error {
	if password == "" {
		return errors.New("empty password")
	}
	if err := secretLine("the login password", password); err != nil {
		return err
	}
	if keychain == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		keychain = filepath.Join(home, loginKeychainPath)
	}
	stdout, stderr, err := r.RunWithStdin(ctx, unlockLoginKeychainLine(password, keychain), "/usr/bin/security", "-i")
	if err != nil {
		return fmt.Errorf("security -i: %w", err)
	}
	if unlockFailed(stdout, stderr) {
		// The output names the keychain and the reason, never the password.
		return fmt.Errorf("could not unlock %s: %s", keychain, strings.TrimSpace(strings.ReplaceAll(stdout+stderr, password, "…")))
	}
	return nil
}

// workerWrapperUnlockScript defines the wrapper's (zsh) login-keychain unlocking and starts it.
//
//   - grove_unlock_keychain_with unlocks with the password in $grove_pw: escaped for one
//     `security -i` argument and piped in. print is a builtin, so the password is never in any
//     process's argv.
//   - It deletes legacyPath, an older grove's stored login password, unread.
//   - A loop reads lines from the FIFO fifoPath, which only the worker's credential broker writes
//     (DeliverKeychainUnlock), and unlocks with each: the control panel's Unlock keychain. The
//     password lives only in this shell's memory and is kept nowhere.
//
// A failure only warns: Linux VMs need no keychain. The loop lives as long as Orchard, its
// parent after the exec, and no longer: it checks that pid, so a wrapper that exits never leaves
// it holding its output open. It opens the FIFO read-write so the open never blocks
// and reads with a timeout, which is what lets it notice.
func workerWrapperUnlockScript(legacyPath, fifoPath string) string {
	return fmt.Sprintf(`/bin/rm -f %q
grove_unlock_fifo=%q
grove_unlock_keychain_with() {
  local out
  grove_pw=${grove_pw//\\/\\\\}
  grove_pw=${grove_pw//\"/\\\"}
  out=$(print -r -- "unlock-keychain -p \"$grove_pw\" \"$HOME/%s\"" | /usr/bin/security -i 2>&1)
  grove_pw=""
  if [[ "$out" == *"security: "* || "$out" == *SecKeychain* ]]; then
    print -u2 -r -- "grove: could not unlock the login keychain (macOS VMs need it): ${out}"
    return 1
  fi
  /usr/bin/security set-keychain-settings "$HOME/%s" >/dev/null 2>&1 || true
  return 0
}
if [ ! -p "$grove_unlock_fifo" ]; then
  rm -f "$grove_unlock_fifo" 2>/dev/null
  /usr/bin/mkfifo -m 600 "$grove_unlock_fifo" 2>/dev/null || true
fi
if [ -p "$grove_unlock_fifo" ]; then
  grove_wrapper_pid=$$
  ( exec {grove_fd}<>"$grove_unlock_fifo" || exit 0
    while kill -0 "$grove_wrapper_pid" 2>/dev/null; do
      grove_pw=""
      if IFS= read -r -t 2 -u $grove_fd grove_pw && [ -n "$grove_pw" ]; then
        if grove_unlock_keychain_with; then print -u2 -r -- "grove: unlocked the login keychain from the control panel"; fi
      fi
      grove_pw=""
    done ) &!
fi
`, legacyPath, fifoPath, loginKeychainPath, loginKeychainPath)
}

// KeychainUnlockFIFOName is the FIFO, in the credentials directory, through which the worker's
// credential broker hands the wrapper a login password typed into the control panel
// (POST /workers/{name}/unlock-keychain). The wrapper creates it and reads it; nothing stores the
// password: it passes from the broker's memory through the kernel's pipe buffer to `security -i`.
const KeychainUnlockFIFOName = "keychain-unlock.fifo"

// ErrNoUnlockReader is DeliverKeychainUnlock's error when no wrapper is reading the FIFO yet.
var ErrNoUnlockReader = errors.New("no worker wrapper is reading the keychain-unlock FIFO")
