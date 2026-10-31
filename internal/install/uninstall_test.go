package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// installedWorkerFixture lays out what a worker install leaves in home (and its LaunchDaemons dir).
func installedWorkerFixture(t *testing.T, opts Options) {
	t.Helper()
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := opts.ConfigDir()
	for _, l := range []string{orchardWorkerLabel, brokerLabel, hostFirewallLabel} {
		write(filepath.Join(opts.launchDaemonsDir(), l+".plist"), "<plist/>")
	}
	write(filepath.Join(cfg, "launchd", orchardWorkerLabel+".plist"), "<plist/>")
	write(filepath.Join(cfg, "pf", hostFirewallLabel+".plist"), "<plist/>")
	write(credentialPath(opts, workerBootstrapCredentialName), "not-a-real-token\n")
	write(workerWrapperPath(opts), "exec orchard worker run --name \"test-worker\"\n")
	write(filepath.Join(cfg, "registry-login.ghcr.io"), "user\n")
	write(filepath.Join(cfg, "config.yaml"), "server: https://cp\n")
	write(workerOrchardExecutable(opts), "binary")
	write(legacyAgentPlist(opts, orchardWorkerLabel), "<plist/>")
	for _, n := range workerLogNames {
		write(filepath.Join(opts.LogDir(), n), "log\n")
	}
	if err := os.MkdirAll(orchardBinDir(opts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workerOrchardExecutable(opts), filepath.Join(orchardBinDir(opts), orchardBinName)); err != nil {
		t.Fatal(err)
	}
}

func uninstallTestOptions(t *testing.T) Options {
	t.Setenv("GROVE_CONFIG", "")
	opts := testWorkerOptions(t.TempDir())
	opts.Token = ""
	return opts
}

// scriptDaemonsUnloaded makes `launchctl print system/<label>` fail, as it does once a job is gone.
func scriptDaemonsUnloaded(r *FakeRunner) {
	for _, l := range []string{orchardWorkerLabel, brokerLabel, hostFirewallLabel} {
		r.Script("launchctl print system/"+l, FakeResult{Err: errors.New("Could not find service")})
	}
}

func stepNames(steps []Step) []string {
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	return names
}

func TestUninstallPlan_RootStepsFirstAndSleepOnlyOnRequest(t *testing.T) {
	opts := uninstallTestOptions(t)
	steps, err := BuildUninstallPlan(NewFakeRunner(), opts, UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(stepNames(steps), " ")
	if !strings.HasPrefix(names, "remove-worker-daemons remove-host-firewall ") {
		t.Errorf("root steps are not first: %s", names)
	}
	if strings.Contains(names, "restore-sleep") || strings.Contains(names, "remove:config") {
		t.Errorf("restore-sleep / config purge without asking: %s", names)
	}
	seenUser := false
	for _, s := range steps {
		if !s.Privileged {
			seenUser = true
		} else if seenUser {
			t.Errorf("privileged step %s comes after a user step", s.Name)
		}
	}

	steps, err = BuildUninstallPlan(NewFakeRunner(), opts, UninstallOptions{RestoreSleep: true, Purge: true})
	if err != nil {
		t.Fatal(err)
	}
	names = strings.Join(stepNames(steps), " ")
	if !strings.Contains(names, "restore-sleep") || !strings.Contains(names, "remove:config") {
		t.Errorf("--restore-sleep / --purge steps missing: %s", names)
	}

	opts.Role = RoleControlPlane
	if _, err := BuildUninstallPlan(NewFakeRunner(), opts, UninstallOptions{}); err == nil {
		t.Error("a control-plane uninstall was planned")
	}
	opts.Role, opts.User = RoleWorker, "root"
	if _, err := BuildUninstallPlan(NewFakeRunner(), opts, UninstallOptions{}); err == nil {
		t.Error("a root worker user was accepted")
	}
}

func TestUninstallRootScript(t *testing.T) {
	opts := uninstallTestOptions(t)
	installedWorkerFixture(t, opts)
	r := NewFakeRunner()
	r.Script("pmset -g", FakeResult{Stdout: " SleepDisabled\t\t1\n"})
	steps, err := BuildUninstallPlan(r, opts, UninstallOptions{RestoreSleep: true})
	if err != nil {
		t.Fatal(err)
	}
	var pending []Step
	for _, s := range steps {
		if !s.Privileged {
			continue
		}
		ok, err := s.Check(context.Background())
		if err != nil || ok {
			t.Fatalf("%s: check = %v, %v on an installed worker", s.Name, ok, err)
		}
		pending = append(pending, s)
	}
	var out bytes.Buffer
	outcome, err := RunRootSteps(context.Background(), r, pending, RootOptions{NoSudo: true, Out: &out})
	if err != nil || outcome != RootPrinted {
		t.Fatalf("RunRootSteps = %v, %v", outcome, err)
	}
	block := out.String()
	for _, want := range []string{
		"/bin/launchctl bootout 'system/com.grove.orchard-worker' 2>/dev/null || true",
		"/bin/launchctl bootout 'system/com.grove.broker' 2>/dev/null || true",
		"/bin/launchctl bootout 'system/com.grove.pf-vm' 2>/dev/null || true",
		"/bin/rm -f " + shQuote(filepath.Join(opts.launchDaemonsDir(), "com.grove.orchard-worker.plist")),
		"/bin/rm -f " + shQuote(filepath.Join(opts.launchDaemonsDir(), "com.grove.broker.plist")),
		"/bin/rm -f " + shQuote(filepath.Join(opts.launchDaemonsDir(), "com.grove.pf-vm.plist")),
		"/sbin/pfctl -q -a 'com.apple/grove-vm' -F all 2>/dev/null || true",
		"/usr/bin/pmset -a disablesleep 0",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("root block lacks %q:\n%s", want, block)
		}
	}
	// The firewall's daemon is booted out before its anchor is flushed, or its reload refills it.
	if strings.Index(block, "system/com.grove.pf-vm") > strings.Index(block, "/sbin/pfctl") {
		t.Error("pf anchor flushed before the firewall daemon is booted out")
	}
	script := block[strings.Index(block, "<<'GROVE_ROOT_STEPS'")+len("<<'GROVE_ROOT_STEPS'") : strings.LastIndex(block, "GROVE_ROOT_STEPS")]
	cmdRE := regexp.MustCompile(`(?m)^(\S+)`)
	for _, m := range cmdRE.FindAllStringSubmatch(script, -1) {
		if c := m[1]; c != "set" && c != "echo" && !strings.HasPrefix(c, "/") {
			t.Errorf("root script runs %q, not an absolute system binary", c)
		}
	}
	if strings.Contains(script, "sudo") {
		t.Errorf("root script contains sudo:\n%s", script)
	}
	if strings.Contains(block, "not-a-real-token") {
		t.Error("a credential appeared in the root block")
	}
}

func TestUninstallUserSteps_RemoveWorkerFilesKeepConfig(t *testing.T) {
	opts := uninstallTestOptions(t)
	installedWorkerFixture(t, opts)
	if got := InstalledWorkerName(opts); got != "test-worker" {
		t.Errorf("InstalledWorkerName = %q", got)
	}
	r := NewFakeRunner()
	steps, err := BuildUninstallPlan(r, opts, UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var user []Step
	for _, s := range steps {
		if !s.Privileged {
			user = append(user, s)
		}
	}
	var out bytes.Buffer
	if _, err := RunPlan(context.Background(), user, PlanOptions{Out: &out}); err != nil {
		t.Fatalf("RunPlan: %v\n%s", err, out.String())
	}
	cfg := opts.ConfigDir()
	for _, gone := range []string{
		filepath.Join(cfg, "launchd"), filepath.Join(cfg, "pf"), filepath.Join(cfg, "credentials"),
		workerWrapperPath(opts), filepath.Join(cfg, "registry-login.ghcr.io"), workerOrchardAppDir(opts),
		filepath.Join(orchardBinDir(opts), orchardBinName), legacyAgentPlist(opts, orchardWorkerLabel), opts.LogDir(),
	} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg, "config.yaml")); err != nil {
		t.Errorf("config.yaml removed without --purge: %v", err)
	}
	if !r.CalledWith("launchctl bootout gui/" + strconv.Itoa(opts.workerUID()) + "/com.grove.orchard-worker") {
		t.Errorf("legacy agent not booted out: %v", r.Calls)
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" || c.Name == "/usr/bin/sudo" {
			t.Errorf("a user step ran sudo: %s", c)
		}
	}
	// The user's steps never touch the root-owned LaunchDaemons.
	if _, err := os.Stat(filepath.Join(opts.launchDaemonsDir(), "com.grove.broker.plist")); err != nil {
		t.Errorf("a user step removed a LaunchDaemon: %v", err)
	}

	steps, _ = BuildUninstallPlan(r, opts, UninstallOptions{Purge: true})
	if _, err := RunPlan(context.Background(), steps[len(steps)-1:], PlanOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("--purge left %s", cfg)
	}
}

func TestUninstall_LeavesAnOrchardOfTheOperatorsOwn(t *testing.T) {
	opts := uninstallTestOptions(t)
	link := filepath.Join(orchardBinDir(opts), orchardBinName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/homebrew/bin/orchard", link); err != nil {
		t.Fatal(err)
	}
	s := removeOrchardSymlinkStep(opts)
	if ok, _ := s.Check(context.Background()); !ok {
		t.Error("a symlink elsewhere counts as the worker's")
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the operator's orchard symlink was removed: %v", err)
	}
}

func TestUninstall_RerunAfterFullUninstallIsANoop(t *testing.T) {
	opts := uninstallTestOptions(t)
	r := NewFakeRunner()
	scriptDaemonsUnloaded(r)
	r.Script("pmset -g", FakeResult{Stdout: " SleepDisabled\t\t0\n"})
	steps, err := BuildUninstallPlan(r, opts, UninstallOptions{RestoreSleep: true, Purge: true})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		if res.Needed {
			t.Errorf("%s still needed on an uninstalled Mac", res.Step)
		}
	}
	for _, c := range r.Calls {
		if c.Name != "launchctl" && c.Name != "pmset" {
			t.Errorf("a no-op re-run ran %s", c)
		}
	}
}
