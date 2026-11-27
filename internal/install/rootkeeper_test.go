package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootKeeperPlist_IsRootsAndRunsOnlyItsOwnBinary(t *testing.T) {
	plist, err := renderRootKeeperPlist("argos")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + RootKeeperLabel + "</string>", "<string>" + RootKeeperBin() + "</string>",
		"<string>root-keeper</string>", "<string>run</string>", "<string>--user</string>", "<string>argos</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %s:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "UserName") {
		t.Errorf("the keeper is root's; its plist must not name a user:\n%s", plist)
	}
	if _, err := renderRootKeeperPlist("root"); err == nil {
		t.Error("a keeper for root was rendered")
	}
}

// The binary is pinned by the SHA-256 it had when grove planned, copied only when the keeper has
// none (it updates its own), and the plist is written by the root script itself, never copied from
// a file the worker user could change.
func TestRootKeeperRootScript_PinsTheBinaryAndWritesThePlist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "grove")
	if err := os.WriteFile(src, []byte("grove binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{rootKeeperSource: src, LaunchDaemons: filepath.Join(dir, "LaunchDaemons")}
	installed := rootKeeperPlistPath(opts)
	script, err := rootKeeperRoot(opts, "argos", installed)
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(src)
	plist, _ := renderRootKeeperPlist("argos")
	for _, want := range []string{
		"if [ ! -x '" + RootKeeperBin() + "' ]; then",
		"/usr/bin/install -o root -g wheel -m 0755 '" + src + "' '" + RootKeeperBin() + ".new'",
		"= '" + sum + "' ]",
		strings.TrimRight(plist, "\n"),
		"/bin/launchctl bootstrap system '" + installed + "'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("root script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "sudo") {
		t.Errorf("a root text never runs sudo:\n%s", script)
	}

	// An unreadable binary installs nothing and stops unless the keeper has one already.
	opts.rootKeeperSource = filepath.Join(dir, "missing")
	script, _ = rootKeeperRoot(opts, "argos", installed)
	if strings.Contains(script, "/usr/bin/install") || !strings.Contains(script, "[ -x '"+RootKeeperBin()+"' ] || {") {
		t.Errorf("unreadable source:\n%s", script)
	}
}

// On a rootless worker whose VM firewall is installed but stale (the Studio), the keeper's pass
// runs the firewall's root text and never its own install; the install command runs both.
func TestKeepRootSteps_RunsPendingStepsButNeverReinstallsItself(t *testing.T) {
	keeperEUID = func() int { return 0 }
	rootRecheckDelay = time.Millisecond
	t.Cleanup(func() { keeperEUID = os.Geteuid; rootRecheckDelay = time.Second })
	e, opts := rootlessEnv(t)
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	opts.Rootless, opts.NoAutoRootless = true, false
	_, fw := hostFirewallPaths(opts)
	e.write(fw, "old", 0o644)

	script := func() string {
		for i := len(e.r.Calls) - 1; i >= 0; i-- {
			c := e.r.Calls[i]
			if c.Name == "/bin/sh" && len(c.Args) == 3 && c.Args[0] == "-e" {
				return c.Args[2]
			}
		}
		return ""
	}
	ran, err := KeepRootSteps(context.Background(), e.r, opts)
	if strings.Join(ran, ",") != "host-firewall" {
		t.Fatalf("keeper ran %v (err %v), want host-firewall only", ran, err)
	}
	// The fake runner installs nothing, so the re-check fails: what matters is what ran.
	if err == nil || !strings.Contains(err.Error(), "host-firewall") {
		t.Fatalf("err = %v, want the firewall's re-check", err)
	}
	if s := script(); !strings.Contains(s, hostFirewallLabel) || strings.Contains(s, RootKeeperLabel) {
		t.Fatalf("keeper script:\n%s", s)
	}

	ran, _ = InstallRootKeeper(context.Background(), e.r, opts)
	if strings.Join(ran, ",") != "host-firewall,root-keeper" {
		t.Fatalf("install ran %v, want the firewall and the keeper", ran)
	}
	if s := script(); !strings.Contains(s, RootKeeperLabel) {
		t.Fatalf("install script lacks the keeper:\n%s", s)
	}

	keeperEUID = func() int { return 501 }
	if _, err := KeepRootSteps(context.Background(), e.r, opts); err == nil {
		t.Fatal("the keeper ran its root steps as a user")
	}
}
