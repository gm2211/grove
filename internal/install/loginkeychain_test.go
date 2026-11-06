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
	if !strings.Contains(w, credentialPath(opts, loginKeychainCredentialName)) || !strings.Contains(w, "/usr/bin/security -i") {
		t.Fatalf("wrapper doesn't unlock the login keychain:\n%s", w)
	}
	if strings.Index(w, "grove_unlock_login_keychain ||") > strings.Index(w, "exec ") {
		t.Fatal("the keychain must be unlocked before Orchard starts")
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
// RememberLogin and the wrapper do it.
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
			cred := filepath.Join(home, "cred")
			run := func(pw string) (string, error) {
				if err := os.WriteFile(cred, []byte(pw+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Only the function: the background re-unlock loop isn't wanted in a test.
				script := strings.ReplaceAll(workerWrapperUnlockScript(cred, filepath.Join(home, "fifo")), loginKeychainPath, testKeychainPath)
				script = script[:strings.Index(script, "if [ -f \"$grove_login_keychain\" ]; then\n  grove_unlock")]
				cmd := exec.Command(zsh, "-c", "set -eu\n"+script+"grove_unlock_login_keychain")
				cmd.Env = append(os.Environ(), "HOME="+home)
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

func TestRememberLogin_StoresOnlyAVerifiedPassword(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	opts := Options{Role: RoleWorker, Home: t.TempDir()}
	bad := &FakeRunner{}
	bad.Script("/usr/bin/security -i", FakeResult{Stderr: "security: SecKeychainUnlock /k: The user name or passphrase you entered is not correct."})
	if err := RememberLogin(context.Background(), bad, opts, "nope"); err == nil {
		t.Fatal("stored a password the keychain refused")
	}
	if LoginRemembered(opts) {
		t.Fatal("remembered after a failed check")
	}
	if err := RememberLogin(context.Background(), &FakeRunner{}, opts, "right"); err != nil {
		t.Fatal(err)
	}
	if !LoginRemembered(opts) {
		t.Fatal("not remembered")
	}
	info, _ := os.Stat(credentialPath(opts, loginKeychainCredentialName))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if err := ForgetLogin(opts); err != nil || LoginRemembered(opts) {
		t.Fatalf("forget: %v", err)
	}
}
