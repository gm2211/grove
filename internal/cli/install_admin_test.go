package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

func TestCheckAdminFlag(t *testing.T) {
	if err := checkAdminFlag("gm2211", true); err == nil {
		t.Error("--admin with --no-sudo accepted")
	}
	if checkAdminFlag("gm2211", false) != nil || checkAdminFlag("", true) != nil {
		t.Error("valid combination refused")
	}
}

// With --admin the root steps run through su, and the plan then runs once more in the same
// invocation, so a step that waited for them (as worker-agents waits for retire-worker-daemons)
// applies without a re-run.
func TestDriveInstall_AdminReplansAfterTheRootSteps(t *testing.T) {
	r := install.NewFakeRunner()
	suRan := func() int {
		n := 0
		for _, c := range r.Calls {
			if c.Name == "/usr/bin/su" && c.Attached {
				n++
			}
		}
		return n
	}
	rootDone, deferredApplied := false, 0
	steps := []install.Step{
		{Name: "retire", Privileged: true, Description: "root",
			Check: func(context.Context) (bool, error) { return suRan() > 0, nil },
			Root:  func(context.Context) (string, error) { return "echo retire\n", nil }},
		{Name: "agents", Description: "waits for retire",
			Check: func(context.Context) (bool, error) { return deferredApplied > 0, nil },
			Apply: func(context.Context) error {
				if !rootDone {
					return nil // deferred: says so and waits
				}
				deferredApplied++
				return nil
			}},
	}
	r.Script("/usr/bin/id -Gn", install.FakeResult{Stdout: "staff\n"})
	plans := 0
	plan := func(dry bool) ([]install.Step, []install.StepResult, error) {
		plans++
		if suRan() > 0 {
			rootDone = true
		}
		res, err := install.RunPlan(context.Background(), steps, install.PlanOptions{DryRun: dry, Out: io.Discard})
		return steps, res, err
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	outcome, err := driveInstall(cmd, r, plan, "worker", false,
		install.RootOptions{Admin: "gm2211", Interactive: true, In: strings.NewReader(""), Out: &out})
	if err != nil || outcome != install.RootRan {
		t.Fatalf("= %v, %v\n%s", outcome, err, out.String())
	}
	if plans != 2 || deferredApplied != 1 {
		t.Errorf("plans %d, deferred step applied %d times; want 2 and 1", plans, deferredApplied)
	}
	n := suRan()
	if n != 1 {
		t.Errorf("su ran %d times, want once", n)
	}
}

func TestDriveInstall_AdminAsksNothingWhenNoRootIsNeeded(t *testing.T) {
	steps := []install.Step{{Name: "ok", Check: func(context.Context) (bool, error) { return true, nil }}}
	r := install.NewFakeRunner()
	plan := func(dry bool) ([]install.Step, []install.StepResult, error) {
		res, err := install.RunPlan(context.Background(), steps, install.PlanOptions{DryRun: dry, Out: io.Discard})
		return steps, res, err
	}
	outcome, err := driveInstall(&cobra.Command{}, r, plan, "worker", false, install.RootOptions{Admin: "gm2211", Out: io.Discard})
	if err != nil || outcome != install.RootNone || len(r.Calls) != 0 {
		t.Errorf("= %v, %v, calls %v", outcome, err, r.Calls)
	}
}

// The admin cannot read the worker user's config.yaml (their home is mode 750): the rootless
// setting comes from the user's own view of it, and the admin's root steps follow it.
func TestSetupForUser_RootlessComesFromTheUsersConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	nobody := nobodyUser(t)
	base := userBase(t, nobody.Username)
	cfgDir := filepath.Join(base.Home, ".config", "grove")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("worker:\n  rootless: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The root worker and broker LaunchDaemons are still installed.
	if err := os.MkdirAll(base.LaunchDaemons, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"com.grove.orchard-worker", "com.grove.broker"} {
		if err := os.WriteFile(filepath.Join(base.LaunchDaemons, l+".plist"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(cfgDir, 0); err != nil { // unreadable by this process
		t.Fatal(err)
	}
	defer os.Chmod(cfgDir, 0o755)

	read := "/usr/bin/sudo -n -u " + nobody.Username + ` /bin/sh -c if [ -e "$1" ]; then exec /bin/cat -- "$1"; fi; echo GROVE_NO_CONFIG sh ` + cfg
	run := func(res install.FakeResult) (string, error) {
		r := install.NewFakeRunner()
		r.Script(read, res)
		var out strings.Builder
		_, err := setupForUser(context.Background(), r, base, adminHandoff{ServerURL: "http://100.64.0.1:6130", Repair: true}, "",
			install.RootOptions{NoSudo: true, In: strings.NewReader(""), Out: &out})
		return out.String(), err
	}

	out, err := run(install.FakeResult{Stdout: "worker:\n  rootless: true\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "grove: retire-worker-daemons") || strings.Contains(out, "grove: worker-daemons") {
		t.Errorf("rootless worker: want retire-worker-daemons and not worker-daemons:\n%s", out)
	}
	// A root-based worker keeps its daemons.
	out, err = run(install.FakeResult{Stdout: "worker:\n  rootless: false\n"})
	if err != nil || strings.Contains(out, "retire-worker-daemons") {
		t.Errorf("root worker: %v\n%s", err, out)
	}
	// If the mode cannot be determined, fail rather than guess.
	if _, err = run(install.FakeResult{Err: errors.New("sudo: a password is required")}); err == nil {
		t.Error("an unreadable config was treated as a decision")
	}
}
