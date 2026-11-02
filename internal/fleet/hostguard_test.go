package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hostMutatingBinaries are the commands the guest scripts run, usually under grove_priv, that
// change the machine they run on: DNS, firewall, launchd, accounts, power, keychain, services.
// The tests run those scripts with /bin/sh on the developer's own machine, with grove_priv
// redefined to run its arguments directly, so a binary the test did not fake would be the real one.
// That is how a test once reset the DNS servers of every network service on a MacBook.
var hostMutatingBinaries = []string{
	"networksetup", "dscacheutil", "killall", "pkill", "scutil", "systemsetup", "pmset",
	"pfctl", "launchctl", "dscl", "sysadminctl", "security", "defaults", "softwareupdate",
	"xcode-select", "xcodebuild", "xcrun", "sudo",
	"systemctl", "iptables", "ip6tables", "resolvectl", "tailscale",
}

// newFakeBin returns a directory to put first on PATH. Every host-mutating binary in it is a stub
// that fails loudly (exit 99, naming the binary and its arguments); a test that needs one to
// succeed overwrites it with its own fake.
func newFakeBin(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range hostMutatingBinaries {
		stub := "#!/bin/sh\necho \"grove test: refusing to run the real " + name + " on the host: " + name + " $*\" >&2\nexit 99\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write guard stub %s: %v", name, err)
		}
	}
	return dir
}

// guestCmd is the only way a fleet test runs a generated guest script: /bin/sh with fakeBin first
// on PATH. Pass "" for a directory of nothing but the guard stubs. Callers add environment with
// append(cmd.Env, ...).
func guestCmd(t testing.TB, fakeBin string, args ...string) *exec.Cmd {
	t.Helper()
	if fakeBin == "" {
		fakeBin = newFakeBin(t)
	}
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cmd
}

func TestNewFakeBinStubsFailLoudly(t *testing.T) {
	cmd := guestCmd(t, "", "-c", "networksetup -setdnsservers Wi-Fi 1.1.1.1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "networksetup -setdnsservers Wi-Fi 1.1.1.1") {
		t.Fatalf("unfaked networksetup: err = %v, output %q; want exit 99 naming the call", err, out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 99 {
		t.Fatalf("exit = %v, want 99", err)
	}
}

// Every test file in the package builds its /bin/sh commands through guestCmd, so none can run a
// generated script against the host's real binaries.
func TestGuestScriptsOnlyRunThroughGuestCmd(t *testing.T) {
	direct := regexp.MustCompile(`exec\.Command(Context)?\([^)]*"(/bin/sh|sh|/bin/bash|bash)"`)
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "hostguard_test.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := direct.FindIndex(data); loc != nil {
			line := 1 + strings.Count(string(data[:loc[0]]), "\n")
			t.Errorf("%s:%d runs a shell with exec.Command; use guestCmd(t, fakeBin, ...) so unfaked host-mutating binaries fail", f, line)
		}
	}
}
