package install

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestEscalationScriptParsesAndQuotes(t *testing.T) {
	s := escalationScript("https://github.com/gm2211/grove/releases/download/v0.1.68/grove_darwin_arm64.tar.gz",
		strings.Repeat("a", 64), "argos")
	cmd := "/bin/sh -c " + shQuote(s)
	// What su hands the admin's login shell must parse, and what that shell hands sh must too.
	for _, sh := range []string{"sh", "bash"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		if out, err := exec.Command(sh, "-n", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s -n: %v %s", sh, err, out)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("inner script: %v %s", err, out)
	}
	for _, want := range []string{"shasum -a 256", "sudo ./grove doctor --fix --worker-user 'argos'", "mktemp -d /private/tmp/"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// The checksum is compared before the archive is unpacked or run.
	if strings.Index(s, "shasum") > strings.Index(s, "tar -xzf") {
		t.Error("unpacks before checking the digest")
	}
}

func TestAdminAccounts(t *testing.T) {
	r := NewFakeRunner()
	r.Script("/usr/bin/dscl . -read /Groups/admin GroupMembership", FakeResult{Stdout: "GroupMembership: root _mbsetupuser nosuchuser-xyz\n"})
	got, err := AdminAccounts(context.Background(), r)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want root, setup and unknown accounts dropped", got, err)
	}
}
