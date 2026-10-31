package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/install"
)

func uninstallTarget(t *testing.T) *workerTarget {
	t.Setenv("GROVE_CONFIG", "")
	return &workerTarget{User: "argos", Home: t.TempDir(), GroveBin: "/opt/homebrew/bin/grove", launchDaemons: filepath.Join(t.TempDir(), "LaunchDaemons")}
}

// unloadingRunner is a FakeRunner whose `launchctl print` succeeds (the daemons are loaded) until
// the root script has run through sudo.
type unloadingRunner struct {
	*install.FakeRunner
	ran bool
}

func (u *unloadingRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	stdout, stderr, err := u.FakeRunner.Run(ctx, name, args...)
	if u.ran && name == "launchctl" && len(args) > 0 && args[0] == "print" {
		return "", "", errors.New("Could not find service")
	}
	return stdout, stderr, err
}

func (u *unloadingRunner) RunAttached(ctx context.Context, name string, args ...string) error {
	u.ran = true
	return u.FakeRunner.RunAttached(ctx, name, args...)
}

// The admin's side runs the root script it rendered through sudo first, then the worker user's
// half through `sudo -n -u`, with nothing on stdin and no text of the user's in the root script.
func TestUninstallForUser_RootScriptThenUsersHalf(t *testing.T) {
	tgt := uninstallTarget(t)
	fake := install.NewFakeRunner()
	fake.Script("/usr/bin/id -Gn", install.FakeResult{Stdout: "staff admin\n"})
	r := &unloadingRunner{FakeRunner: fake}
	var out bytes.Buffer
	outcome, err := uninstallForUserSteps(context.Background(), r, tgt, install.UninstallOptions{Purge: true}, false,
		install.RootOptions{Yes: true, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if outcome != install.RootRan {
		t.Errorf("outcome %v, want RootRan", outcome)
	}
	rootAt, userAt := -1, -1
	for i, c := range r.Calls {
		switch {
		case c.Attached && strings.HasPrefix(c.String(), "/usr/bin/sudo /bin/sh -e -c "):
			rootAt = i
			if strings.Contains(c.String(), tgt.Home) {
				t.Errorf("root script names a path in %s's home:\n%s", tgt.User, c.String())
			}
		case c.Streamed:
			userAt = i
			want := "/usr/bin/sudo -n -u argos -H -- /opt/homebrew/bin/grove uninstall --role worker --from-admin --purge"
			if c.String() != want || c.Stdin != "" {
				t.Errorf("user's half = %q (stdin %q), want %q", c.String(), c.Stdin, want)
			}
		}
	}
	if rootAt < 0 || userAt < 0 || rootAt > userAt {
		t.Fatalf("want the root script, then the user's half; calls: %v", r.Calls)
	}
}

func TestUninstallForUser_DryRunRunsNoRootScript(t *testing.T) {
	tgt := uninstallTarget(t)
	r := install.NewFakeRunner()
	var out bytes.Buffer
	if _, err := uninstallForUserSteps(context.Background(), r, tgt, install.UninstallOptions{}, true,
		install.RootOptions{Yes: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Calls {
		if c.Attached {
			t.Errorf("dry-run ran %s", c)
		}
	}
	want := "/usr/bin/sudo -n -u argos -H -- /opt/homebrew/bin/grove uninstall --role worker --from-admin --dry-run"
	if !r.CalledWith(want) {
		t.Errorf("user's half not run as a dry-run: %v", r.Calls)
	}
	if !strings.Contains(out.String(), "[plan]  remove-worker-daemons") {
		t.Errorf("dry-run doesn't list the root steps:\n%s", out.String())
	}
}

func TestUninstallForUser_RefusesRoot(t *testing.T) {
	tgt := uninstallTarget(t)
	tgt.User = "root"
	if _, err := uninstallForUserSteps(context.Background(), install.NewFakeRunner(), tgt, install.UninstallOptions{}, false,
		install.RootOptions{Out: &bytes.Buffer{}}); err == nil {
		t.Error("root accepted as the worker user")
	}
}

func TestPrintForgetWorker(t *testing.T) {
	var out bytes.Buffer
	printForgetWorker(&out, "mac mini")
	if !strings.Contains(out.String(), "orchard delete worker 'mac mini'") || !strings.Contains(out.String(), "grove access revoke") {
		t.Errorf("forget instructions:\n%s", out.String())
	}
}
