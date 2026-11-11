package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const trickyPassword = `p a"s\s'w$o` + "`" + `rd\`

func TestUnlockLoginKeychainLine_QuotesForSecurityInteractive(t *testing.T) {
	got := unlockLoginKeychainLine(`a"b\c`, "/k/login.keychain-db")
	want := `unlock-keychain -p "a\"b\\c" "/k/login.keychain-db"` + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnlockLoginKeychain_PasswordOnlyOnStdin(t *testing.T) {
	r := &FakeRunner{}
	if err := UnlockLoginKeychain(context.Background(), r, "s3cret", "/k/login.keychain-db"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "s3cret") || strings.Contains(c.Name, "s3cret") {
			t.Fatalf("password in argv: %+v", c)
		}
	}
}

func TestWorkerWrapper_UnlocksTheLoginKeychainWithoutTheSecret(t *testing.T) {
	opts := Options{Role: RoleWorker, Home: t.TempDir(), Controller: "http://c:6120"}
	w := workerWrapper(opts, "/bin/orchard")
	if !strings.Contains(w, "/usr/bin/security -i") {
		t.Fatalf("wrapper doesn't unlock the login keychain:\n%s", w)
	}
	if strings.Contains(w, "security unlock-keychain") {
		t.Fatal("unlock-keychain on argv would put the password there")
	}
}

// testKeychainPath is where the real-keychain tests put theirs, relative to a temporary home. Not
// "login": on a CI runner, a new keychain named login.keychain refused even its own password on
// argv, so a test there couldn't tell the code's failures from the runner's.
const testKeychainPath = "Library/Keychains/grove-test.keychain-db"

// macOS only: real keychains, with passwords that need each escape, unlocked the way
// UnlockLoginKeychain and the wrapper do it.
var keychainPasswords = map[string]string{
	"plain":      "plainpassword1",
	"space":      "two words",
	"quote":      `say"hi`,
	"backslash":  `back\slash`,
	"apostrophe": "it's",
	"shell":      "a$b`c",
	"all":        trickyPassword,
}

func newLockedKeychain(t *testing.T, home, password string) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS security(1)")
	}
	kc := filepath.Join(home, testKeychainPath)
	if err := os.MkdirAll(filepath.Dir(kc), 0o700); err != nil {
		t.Fatal(err)
	}
	// security names a new keychain itself: create "login.keychain" and use the file it writes
	// (login.keychain-db on current macOS).
	if out, err := exec.Command("/usr/bin/security", "create-keychain", "-p", password, strings.TrimSuffix(kc, "-db")).CombinedOutput(); err != nil {
		t.Fatalf("create-keychain: %v\n%s", err, out)
	}
	if _, err := os.Stat(kc); err != nil {
		entries, _ := os.ReadDir(filepath.Dir(kc))
		t.Fatalf("create-keychain didn't write %s: %v (dir holds %v)", kc, err, entries)
	}
	t.Cleanup(func() { _ = exec.Command("/usr/bin/security", "delete-keychain", kc).Run() })
	// Control: the keychain takes this password through argv, so a failure below is the
	// interactive path's.
	if out, err := exec.Command("/usr/bin/security", "unlock-keychain", "-p", password, kc).CombinedOutput(); err != nil {
		entries, _ := os.ReadDir(filepath.Dir(kc))
		t.Fatalf("control unlock-keychain on argv: %v\n%s (dir holds %v)", err, out, entries)
	}
	if out, err := exec.Command("/usr/bin/security", "lock-keychain", kc).CombinedOutput(); err != nil {
		t.Fatalf("lock-keychain: %v\n%s", err, out)
	}
	return kc
}

func TestUnlockLoginKeychain_RealKeychain(t *testing.T) {
	for name, password := range keychainPasswords {
		t.Run(name, func(t *testing.T) {
			kc := newLockedKeychain(t, t.TempDir(), password)
			if err := UnlockLoginKeychain(context.Background(), ExecRunner{}, "wrong", kc); err == nil {
				t.Fatal("a wrong password unlocked the keychain")
			}
			if err := UnlockLoginKeychain(context.Background(), ExecRunner{}, password, kc); err != nil {
				t.Fatalf("the right password: %v", err)
			}
		})
	}
}

func TestWorkerWrapperUnlockScript_RealKeychain(t *testing.T) {
	for name, password := range keychainPasswords {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			newLockedKeychain(t, home, password)
			zsh, err := exec.LookPath("zsh")
			if err != nil {
				t.Skip("no zsh")
			}
			run := func(pw string) (string, error) {
				// Only the function: the FIFO loop isn't wanted here. The password comes in the
				// environment, as the FIFO's read would set it.
				script := strings.ReplaceAll(workerWrapperUnlockScript(filepath.Join(home, "legacy"), filepath.Join(home, "fifo")), loginKeychainPath, testKeychainPath)
				script = script[:strings.Index(script, "if [ ! -p \"$grove_unlock_fifo\" ]")]
				cmd := exec.Command(zsh, "-c", "set -eu\n"+script+"grove_pw=$GROVE_TEST_PW; grove_unlock_keychain_with")
				cmd.Env = append(os.Environ(), "HOME="+home, "GROVE_TEST_PW="+pw)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			if out, err := run("wrong"); err == nil {
				t.Fatalf("a wrong password unlocked the keychain: %s", out)
			} else if strings.Contains(out, "-p \"wrong\"") {
				t.Fatalf("the failure message shows the password: %s", out)
			}
			if out, err := run(password); err != nil {
				t.Fatalf("the right password: %v\n%s", err, out)
			}
		})
	}
}

// grove never stores a login password: the wrapper only ever deletes an older grove's stored one,
// and never reads it.
func TestWorkerWrapper_NeverReadsAStoredLoginPassword(t *testing.T) {
	opts := Options{Role: RoleWorker, Home: t.TempDir(), Controller: "http://c:6120"}
	w := workerWrapper(opts, "/bin/orchard")
	legacy := credentialPath(opts, legacyLoginKeychainCredentialName)
	if !strings.Contains(w, "/bin/rm -f "+`"`+legacy+`"`) {
		t.Fatalf("wrapper must delete the legacy stored password:\n%s", w)
	}
	if strings.Count(w, legacy) != 1 {
		t.Fatalf("wrapper names the legacy stored password other than to delete it:\n%s", w)
	}
	if strings.Index(w, legacy) > strings.Index(w, "exec ") {
		t.Fatal("the legacy file must be deleted before Orchard starts")
	}
	for _, bad := range []string{"remember-login", "while sleep"} {
		if strings.Contains(w, bad) {
			t.Fatalf("wrapper contains %q:\n%s", bad, w)
		}
	}
}
