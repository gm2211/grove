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
// unlocked inside that session is unlocked for the Tart it starts. So when the worker user has
// stored its login password (`grove worker remember-login`, a 0600 file only that user and root
// can read), the wrapper unlocks the login keychain itself before Orchard starts, and again every
// few minutes in case something locked it. Verified on a real worker Mac: with nobody logged in
// as the worker user, a macOS guest that failed with the host-key error booted once the daemon's
// session had unlocked the keychain.
//
// The password goes to `security -i` on stdin, as one interactive command line, never as an
// argument: `security unlock-keychain` takes it only with -p, which would put it in argv.

const loginKeychainCredentialName = "login-keychain"

// loginKeychainRelock is how often the wrapper unlocks the login keychain again.
const loginKeychainRelock = "300"

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

// RememberLogin checks password against this user's login keychain, stores it for the worker
// daemon (credentials/login-keychain, 0600), and restarts a running worker so it unlocks the
// keychain now. Run it as the worker user.
func RememberLogin(ctx context.Context, r Runner, opts Options, password string) error {
	if err := UnlockLoginKeychain(ctx, r, password, ""); err != nil {
		return fmt.Errorf("%w (is this the password of the user the worker runs as? run this command as that user)", err)
	}
	if err := writeSecretFile(credentialPath(opts, loginKeychainCredentialName), password+"\n"); err != nil {
		return err
	}
	restartDaemon(ctx, r, orchardWorkerLabel)
	return nil
}

// ForgetLogin removes the stored login password. The keychain stays as it is until the next
// reboot; after that macOS VMs again need the worker user logged in.
func ForgetLogin(opts Options) error {
	err := os.Remove(credentialPath(opts, loginKeychainCredentialName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// LoginRemembered says whether the worker user has stored its login password for the daemon.
func LoginRemembered(opts Options) bool {
	info, err := os.Lstat(credentialPath(opts, loginKeychainCredentialName))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// workerWrapperUnlockScript defines the wrapper's (zsh) login-keychain unlocking and starts it.
//
//   - grove_unlock_keychain_with unlocks with the password in $grove_pw: escaped for one
//     `security -i` argument and piped in. print is a builtin, so the password is never in any
//     process's argv.
//   - With a stored password (`grove worker remember-login`, credPath), it unlocks before Orchard
//     starts and again every loginKeychainRelock seconds.
//   - Without one, it can still be unlocked from the control panel: a loop reads lines from the
//     FIFO fifoPath, which only the worker's credential broker writes (DeliverKeychainUnlock),
//     and unlocks with each. The password lives only in this shell's memory and is kept nowhere.
//
// A failure only warns: Linux VMs need no keychain. Both loops live as long as Orchard, their
// parent after the exec.
func workerWrapperUnlockScript(credPath, fifoPath string) string {
	return fmt.Sprintf(`grove_login_keychain=%q
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
grove_unlock_login_keychain() {
  local grove_pw
  [ -f "$grove_login_keychain" ] || return 0
  IFS= read -r grove_pw < "$grove_login_keychain" || [ -n "$grove_pw" ] || return 0
  grove_unlock_keychain_with
}
if [ -f "$grove_login_keychain" ]; then
  grove_unlock_login_keychain || true
  ( while sleep %s; do grove_unlock_login_keychain >/dev/null 2>&1 || true; done ) &!
fi
if [ ! -p "$grove_unlock_fifo" ]; then
  rm -f "$grove_unlock_fifo" 2>/dev/null
  /usr/bin/mkfifo -m 600 "$grove_unlock_fifo" 2>/dev/null || true
fi
if [ -p "$grove_unlock_fifo" ]; then
  ( while :; do
      grove_pw=""
      if IFS= read -r grove_pw < "$grove_unlock_fifo" && [ -n "$grove_pw" ]; then
        if grove_unlock_keychain_with; then print -u2 -r -- "grove: unlocked the login keychain from the control panel"; fi
      else
        sleep 1
      fi
      grove_pw=""
    done ) &!
fi
`, credPath, fifoPath, loginKeychainPath, loginKeychainPath, loginKeychainRelock)
}

// KeychainUnlockFIFOName is the FIFO, in the credentials directory, through which the worker's
// credential broker hands the wrapper a login password typed into the control panel
// (POST /workers/{name}/unlock-keychain). The wrapper creates it and reads it; nothing stores the
// password: it passes from the broker's memory through the kernel's pipe buffer to `security -i`.
const KeychainUnlockFIFOName = "keychain-unlock.fifo"

// ErrNoUnlockReader is DeliverKeychainUnlock's error when no wrapper is reading the FIFO yet.
var ErrNoUnlockReader = errors.New("no worker wrapper is reading the keychain-unlock FIFO")
