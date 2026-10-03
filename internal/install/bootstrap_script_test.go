package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/install.sh is the curl|sh entry point; it can't import Go constants, so this test keeps
// its pins in lock-step with download.go and exercises its verification helper.
func readBootstrapScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBootstrapScriptPinsMatchInstaller(t *testing.T) {
	s := readBootstrapScript(t)
	if strings.Contains(s, "Homebrew/install/HEAD") {
		t.Error("install.sh must not run Homebrew's installer from HEAD")
	}
	for _, want := range []string{
		"HOMEBREW_INSTALL_COMMIT=" + HomebrewInstallCommit,
		"HOMEBREW_INSTALL_SHA256=" + HomebrewInstallSHA256,
		`verify_sha256 "$brew_tmp/install.sh" "$HOMEBREW_INSTALL_SHA256"`,
		`"$base/checksums.txt"`, // .goreleaser.yaml checksum.name_template
		`verify_sha256 "$tmp/$asset" "$expected"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh is missing %q", want)
		}
	}
}

func TestBootstrapScriptVerifySHA256(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	// Load the script's functions without running main.
	lib := strings.Replace(readBootstrapScript(t), "\nmain \"$@\"\n", "\n", 1)
	dir := t.TempDir()
	file := filepath.Join(dir, "artifact")
	if err := os.WriteFile(file, []byte("grove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const good = "8c9f16acb8f4604d95b58fb49ad1c7a3555f7cd22266f08dbc10a1c96a67b3a3" // sha256("grove\n")
	run := func(want string) (string, error) {
		cmd := exec.Command(sh, "-c", lib+"\nverify_sha256 \"$1\" \"$2\" && echo verified", "sh", file, want)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(good); err != nil || !strings.Contains(out, "verified") {
		t.Fatalf("matching digest rejected: %v %s", err, out)
	}
	out, err := run(strings.Repeat("0", 64))
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("mismatch accepted: %v %s", err, out)
	}
	if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
		t.Error("a file that failed verification must be deleted")
	}
}
