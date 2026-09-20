package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func alwaysFailLookPath(string) (string, error) {
	return "", errors.New("not found (fake)")
}

// alwaysFalseExists simulates a fresh machine with none of Tailscale's hardcoded bundle paths
// present, so tests aren't at the mercy of whether the machine running them happens to have the
// real Tailscale.app installed.
func alwaysFalseExists(string) bool { return false }

// lookPathOnly simulates PATH containing exactly the given binaries (each resolving to
// "/usr/bin/<name>"), and nothing else — so a planner test can exercise "tailscale is up but
// nothing else is installed yet" without depending on what's actually on the test machine's PATH.
func lookPathOnly(names ...string) func(string) (string, error) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found (fake)")
	}
}

// scriptTailscaleUp scripts a healthy `tailscale status --json` response on r for the binary
// lookPathOnly("tailscale", ...) resolves to.
func scriptTailscaleUp(r *FakeRunner) {
	r.Script("/usr/bin/tailscale status --json", FakeResult{
		Stdout: `{"Self":{"DNSName":"test-host.tailnet.ts.net.","TailscaleIPs":["100.64.0.1"],"Online":true},"BackendState":"Running"}`,
	})
}

func testWorkerOptions(home string) Options {
	return Options{
		Role:       RoleWorker,
		Controller: "https://cp.tailnet.ts.net:6120",
		Token:      "worker-secret-6f32f988",
		Hostname:   "test-worker",
		Home:       home,
		GOOS:       "darwin",
		GOARCH:     "arm64",
		LookPath:   lookPathOnly("tailscale"),
		Exists:     alwaysFalseExists,
	}
}

func TestWorkerPlan_AppliesCommandsAndGatesPrivilegedSteps(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if !r.CalledWith("brew install openai/tools/tart") {
		t.Error("expected `brew install openai/tools/tart` to run")
	}
	if r.CalledWith("brew install cirruslabs/cli/tart") {
		t.Error("cirruslabs/cli/tart fallback should not run when the openai/tools tap install succeeds")
	}
	sawHomebrewInstall := false
	sawOrchardBuild := false
	for _, c := range r.Calls {
		if c.Name == "/bin/bash" {
			sawHomebrewInstall = true
		}
		if c.Name == "/bin/sh" && len(c.Args) == 2 && c.Args[0] == "-c" {
			sawOrchardBuild = true
		}
	}
	if !sawHomebrewInstall {
		t.Error("expected the Homebrew installer to run since `brew` isn't on PATH")
	}
	if !sawOrchardBuild {
		t.Error("expected the orchard clone+build fallback to run since neither a release asset nor `orchard` on PATH")
	}

	// The sudo-only checklist must never actually execute without --yes as root. Loading this
	// user's own LaunchAgent is intentionally non-privileged and happens during normal install.
	for _, forbidden := range []string{
		"sudo pmset -a disablesleep 1",
	} {
		if r.CalledWith(forbidden) {
			t.Errorf("privileged command ran without --yes as root: %q", forbidden)
		}
	}
	if !r.CalledWith("launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + filepath.Join(home, "Library", "LaunchAgents", "com.grove.orchard-worker.plist")) {
		t.Error("expected normal install to load the current user's Orchard LaunchAgent")
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" {
			t.Errorf("no `sudo` command should run without --yes as root, got: %s", c.String())
		}
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.grove.orchard-worker.plist")
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading rendered plist: %v", err)
	}
	want, err := workerLaunchAgentPlist(opts, workerWrapperPath(opts))
	if err != nil {
		t.Fatalf("workerLaunchAgentPlist: %v", err)
	}
	if string(got) != want {
		t.Errorf("rendered plist mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(string(got), opts.Token) {
		t.Fatal("worker bootstrap credential leaked into LaunchAgent")
	}
	wrapper, err := os.ReadFile(workerWrapperPath(opts))
	if err != nil {
		t.Fatalf("reading worker wrapper: %v", err)
	}
	if strings.Contains(string(wrapper), opts.Token) || !strings.Contains(string(wrapper), "--bootstrap-token-stdin") {
		t.Fatalf("worker wrapper must use Keychain/stdin without embedding credential: %s", wrapper)
	}
	deleteKeychainCmd := "/usr/bin/security delete-generic-password -a test-worker -s " + orchardWorkerKeychainService
	keychainCmd := "/usr/bin/security add-generic-password -a test-worker -s " + orchardWorkerKeychainService + " -T /usr/bin/security -w"
	if !r.CalledWith(deleteKeychainCmd) {
		t.Fatal("worker credential replacement must delete stale Keychain item before adding")
	}
	stdin, ok := r.StdinFor(keychainCmd)
	if !ok || stdin != opts.Token+"\n"+opts.Token+"\n" {
		t.Fatal("worker credential was not stored through Keychain stdin")
	}
	for _, call := range r.Calls {
		if call.Name == "/usr/bin/security" && len(call.Args) > 1 && call.Args[0] == "add-generic-password" && call.Args[1] == "-U" {
			t.Fatalf("Keychain write must not use -U because it can trigger an ACL password dialog: %s", call.String())
		}
		for _, arg := range call.Args {
			if strings.Contains(arg, opts.Token) {
				t.Fatalf("worker credential leaked into argv: %s", call.String())
			}
		}
	}
	if !strings.Contains(string(got), "<key>AssociatedBundleIdentifiers</key>") ||
		!strings.Contains(string(got), orchardWorkerBundleID) {
		t.Error("worker LaunchAgent must identify the Orchard app as responsible code for Local Network consent")
	}
	infoPlist, err := os.ReadFile(workerOrchardInfoPlistPath(opts))
	if err != nil {
		t.Fatalf("reading worker app Info.plist: %v", err)
	}
	if !strings.Contains(string(infoPlist), "NSLocalNetworkUsageDescription") ||
		!strings.Contains(string(infoPlist), "Grove Orchard Worker") {
		t.Errorf("worker app Info.plist lacks visible Local Network consent identity: %s", infoPlist)
	}
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if target, err := os.Readlink(cliPath); err != nil || target != workerOrchardExecutable(opts) {
		t.Errorf("Orchard CLI link = %q, %v; want %q", target, err, workerOrchardExecutable(opts))
	}
	if !r.CalledWith("/usr/bin/codesign --force --deep --sign - --identifier " + orchardWorkerBundleID +
		" --timestamp=none " + workerOrchardAppDir(opts)) {
		t.Error("expected worker app bundle to receive a stable local code signature")
	}
	for _, c := range r.Calls {
		if c.Name == "defaults" && strings.Contains(c.String(), "com.apple.network.local-network") {
			t.Errorf("worker install must not change system-wide Local Network allowlists: %s", c.String())
		}
	}
}

func TestEnsureWorkerOrchardApp_MigratesExistingBinary(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const existing = "existing fork binary"
	if err := os.WriteFile(cliPath, []byte(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()

	if err := ensureWorkerOrchardApp(context.Background(), r, opts, cliPath, io.Discard); err != nil {
		t.Fatalf("ensureWorkerOrchardApp: %v", err)
	}
	got, err := os.ReadFile(workerOrchardExecutable(opts))
	if err != nil {
		t.Fatalf("reading migrated binary: %v", err)
	}
	if string(got) != existing {
		t.Fatalf("migrated binary = %q, want %q", got, existing)
	}
	if target, err := os.Readlink(cliPath); err != nil || target != workerOrchardExecutable(opts) {
		t.Fatalf("CLI link = %q, %v; want %q", target, err, workerOrchardExecutable(opts))
	}
	for _, c := range r.Calls {
		if c.Name == "/bin/sh" && len(c.Args) > 0 && c.Args[0] == "-c" {
			t.Fatalf("existing binary migration should not clone/build Orchard: %s", c.String())
		}
	}
}

func TestEnsureWorkerOrchardApp_PreservesCLIPathWhenSigningFails(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, []byte("existing fork binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	r.Script("/usr/bin/codesign --force --deep --sign - --identifier "+orchardWorkerBundleID+
		" --timestamp=none "+workerOrchardAppDir(opts), FakeResult{Err: errors.New("sign failed")})

	if err := ensureWorkerOrchardApp(context.Background(), r, opts, cliPath, io.Discard); err == nil {
		t.Fatal("expected signing failure")
	}
	target, err := os.Readlink(cliPath)
	if err != nil {
		t.Fatalf("CLI path disappeared after signing failure: %v", err)
	}
	if target != workerOrchardExecutable(opts) {
		t.Fatalf("CLI link = %q, want %q", target, workerOrchardExecutable(opts))
	}
	if _, err := os.Stat(cliPath); err != nil {
		t.Fatalf("CLI link does not resolve after signing failure: %v", err)
	}
}

func TestEnsureWorkerOrchardApp_RealCodeSign(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS codesign verification")
	}
	home := t.TempDir()
	opts := testWorkerOptions(home)
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, binary, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureWorkerOrchardApp(context.Background(), ExecRunner{}, opts, cliPath, io.Discard); err != nil {
		t.Fatalf("ensureWorkerOrchardApp with real codesign: %v", err)
	}
	ok, err := workerOrchardAppInstalled(context.Background(), ExecRunner{}, opts, cliPath)
	if err != nil {
		t.Fatalf("workerOrchardAppInstalled: %v", err)
	}
	if !ok {
		t.Fatal("signed worker app did not pass its installation check")
	}
}

func TestWorkerPlan_TartFallsBackToCirruslabsTap(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	r.Script("brew install openai/tools/tart", FakeResult{Err: errors.New("no available formula (fake)")})
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if !r.CalledWith("brew install openai/tools/tart") {
		t.Error("expected the openai/tools/tart install to be attempted first")
	}
	if !r.CalledWith("brew install cirruslabs/cli/tart") {
		t.Error("expected the cirruslabs/cli/tart tap to be tried as a fallback after openai/tools/tart failed")
	}

	// The cirruslabs/cli fallback tap must be trusted before falling back to installing from it —
	// but only because the primary openai/tools install failed; trustTap for the fallback tap has
	// no reason to run before that.
	tapIdx, trustIdx, installIdx := -1, -1, -1
	for i, c := range r.Calls {
		switch c.String() {
		case "brew tap cirruslabs/cli":
			tapIdx = i
		case "brew trust cirruslabs/cli":
			trustIdx = i
		case "brew install cirruslabs/cli/tart":
			installIdx = i
		}
	}
	if tapIdx == -1 || trustIdx == -1 {
		t.Fatalf("expected cirruslabs/cli to be tapped and trusted, calls: %v", r.Calls)
	}
	if !(tapIdx < trustIdx && trustIdx < installIdx) {
		t.Errorf("expected tap(%d) < trust(%d) < install(%d) ordering for the cirruslabs/cli fallback",
			tapIdx, trustIdx, installIdx)
	}
}

// TestWorkerPlan_TrustsOpenAIToolsTapBeforeInstallingTart is the primary (non-fallback) path: a
// fresh machine where openai/tools isn't tapped or trusted yet must have both happen, in order,
// before `brew install openai/tools/tart` runs — this is the fix for the observed defect (`brew
// install openai/tools/tart` failing with "Refusing to load formula ... from untrusted tap
// openai/tools" on any Homebrew that gates third-party taps behind `brew trust`).
func TestWorkerPlan_TrustsOpenAIToolsTapBeforeInstallingTart(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "openai/tools", false, false)
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	tapIdx, trustIdx, installIdx := -1, -1, -1
	for i, c := range r.Calls {
		switch c.String() {
		case "brew tap openai/tools":
			tapIdx = i
		case "brew trust openai/tools":
			trustIdx = i
		case "brew install openai/tools/tart":
			installIdx = i
		}
	}
	if tapIdx == -1 || trustIdx == -1 || installIdx == -1 {
		t.Fatalf("expected openai/tools to be tapped, trusted, then installed from, calls: %v", r.Calls)
	}
	if !(tapIdx < trustIdx && trustIdx < installIdx) {
		t.Errorf("expected tap(%d) < trust(%d) < install(%d) ordering for openai/tools", tapIdx, trustIdx, installIdx)
	}
}

// TestWorkerPlan_SkipsTrustWhenTapAlreadyTrusted covers the idempotent case: once `brew tap-info`
// reports the tap installed and trusted, the "brew-trust:openai/tools" step's Check is satisfied
// and neither `brew tap` nor `brew trust` should run again.
func TestWorkerPlan_SkipsTrustWhenTapAlreadyTrusted(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "openai/tools", true, true)
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if r.CalledWith("brew tap openai/tools") {
		t.Error("`brew tap` should not run when the tap is already installed and trusted")
	}
	if r.CalledWith("brew trust openai/tools") {
		t.Error("`brew trust` should not run when the tap is already installed and trusted")
	}
	if !r.CalledWith("brew install openai/tools/tart") {
		t.Error("expected tart to still be installed")
	}
}

// TestWorkerPlan_ToleratesOlderHomebrewWithoutTrustCommand makes sure a Homebrew old enough that
// `brew trust` isn't a recognized command at all doesn't fail the plan: the tap gets `brew tap`ped
// (still useful/idempotent on old Homebrew), `brew trust` fails with "Unknown command", and that's
// treated as satisfied rather than a plan failure.
func TestWorkerPlan_ToleratesOlderHomebrewWithoutTrustCommand(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "openai/tools", false, false)
	r.Script("brew trust openai/tools", FakeResult{
		Err: errors.New("brew trust openai/tools: exit status 1: Error: Invalid usage: Unknown command: brew trust openai/tools"),
	})
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan should tolerate an older Homebrew without `brew trust`, got: %v", err)
	}
	if !r.CalledWith("brew install openai/tools/tart") {
		t.Error("expected tart install to still proceed after the tolerated `brew trust` failure")
	}
}

func TestWorkerPlan_DryRunAppliesNothing(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	results, err := RunPlan(context.Background(), steps, PlanOptions{DryRun: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("RunPlan (dry-run) returned an error: %v", err)
	}
	for _, res := range results {
		if res.Applied {
			t.Errorf("step %q was applied during --dry-run", res.Step)
		}
	}

	// No mutating command may run in --dry-run — only read-only Check calls (e.g. `pmset -g`,
	// `launchctl print <label>`) are expected to still execute, since dry-run only skips Apply.
	forbidden := []string{
		"brew install openai/tools/tart",
		"brew install cirruslabs/cli/tart",
		"sudo pmset -a disablesleep 1",
		"launchctl bootstrap",
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" {
			t.Errorf("no sudo command should run during --dry-run, got: %s", c.String())
		}
		for _, f := range forbidden {
			if c.String() == f {
				t.Errorf("mutating command %q ran during --dry-run", c.String())
			}
		}
	}

	// Nothing should have been written to disk either.
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.grove.orchard-worker.plist")
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("expected no plist written during --dry-run, stat err = %v", err)
	}
}

// TestWorkerPlan_RegistryLoginRunsOnlyWithToken covers the primary registry-login path: given a
// --registry-user/--registry-token, `tart login ghcr.io` runs with the token fed over stdin (never
// as an argument), and a marker file recording user+registry (never the token) is written.
func TestWorkerPlan_RegistryLoginRunsOnlyWithToken(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	opts.RegistryUser = "gm2211"
	opts.RegistryToken = "ghp_supersecrettoken"

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	const wantCmd = "tart login ghcr.io --username gm2211 --password-stdin"
	stdin, ok := r.StdinFor(wantCmd)
	if !ok {
		t.Fatalf("expected %q to run, calls: %v", wantCmd, r.Calls)
	}
	if stdin != opts.RegistryToken {
		t.Errorf("stdin fed to `tart login` = %q, want the registry token %q", stdin, opts.RegistryToken)
	}

	// The token must never appear in any recorded call's args (only over stdin).
	for _, c := range r.Calls {
		for _, a := range c.Args {
			if strings.Contains(a, opts.RegistryToken) {
				t.Errorf("token leaked into command args: %+v", c)
			}
		}
	}

	marker := filepath.Join(home, ".config", "grove", "registry-login.ghcr.io")
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading registry-login marker: %v", err)
	}
	if want := "gm2211 ghcr.io\n"; string(got) != want {
		t.Errorf("marker content = %q, want %q (never the token)", got, want)
	}
	if strings.Contains(string(got), opts.RegistryToken) {
		t.Errorf("token leaked into the marker file: %q", got)
	}
}

// TestWorkerPlan_NoRegistryTokenSkipsLoginStep covers the "skip if no token was given" case: no
// registry-login step is added to the plan at all, and grove prints a NOTE explaining the images
// must be public instead.
func TestWorkerPlan_NoRegistryTokenSkipsLoginStep(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home) // no RegistryUser/RegistryToken set

	var out bytes.Buffer
	steps, err := BuildPlan(r, opts, &out)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	for _, s := range steps {
		if strings.HasPrefix(s.Name, "registry-login") {
			t.Errorf("expected no registry-login step without a token, got %q", s.Name)
		}
	}

	if !strings.Contains(out.String(), "NOTE") || !strings.Contains(out.String(), "public") {
		t.Errorf("expected a NOTE about images needing to be public when no token was given, got: %s", out.String())
	}

	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if r.CalledWith("tart login ghcr.io --username  --password-stdin") {
		t.Error("`tart login` must not run when no --registry-token was given")
	}
}

// TestWorkerPlan_RegistryLoginDryRunNeverExecutes ensures --dry-run only prints the planned `tart
// login` command line and never actually runs it or sends the token anywhere.
func TestWorkerPlan_RegistryLoginDryRunNeverExecutes(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	opts.RegistryUser = "gm2211"
	opts.RegistryToken = "ghp_supersecrettoken"

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var out bytes.Buffer
	results, err := RunPlan(context.Background(), steps, PlanOptions{DryRun: true, Out: &out})
	if err != nil {
		t.Fatalf("RunPlan (dry-run): %v", err)
	}

	const wantCmd = "tart login ghcr.io --username gm2211 --password-stdin"
	if r.CalledWith(wantCmd) {
		t.Error("`tart login` must not run during --dry-run")
	}
	if _, ok := r.StdinFor(wantCmd); ok {
		t.Error("no stdin should have been sent to `tart login` during --dry-run")
	}
	if _, err := os.ReadFile(filepath.Join(home, ".config", "grove", "registry-login.ghcr.io")); !os.IsNotExist(err) {
		t.Errorf("expected no registry-login marker written during --dry-run, err = %v", err)
	}

	found := false
	for _, res := range results {
		if res.Step == "registry-login:ghcr.io" {
			found = true
			if res.Applied {
				t.Error("registry-login step must not be Applied during --dry-run")
			}
			if !res.Needed || !res.Skipped {
				t.Errorf("expected registry-login to be Needed+Skipped during --dry-run, got %+v", res)
			}
		}
	}
	if !found {
		t.Fatalf("expected a registry-login:ghcr.io step in the plan, got: %+v", results)
	}
	if !strings.Contains(out.String(), wantCmd) {
		t.Errorf("expected --dry-run output to print the planned command %q, got: %s", wantCmd, out.String())
	}
}

// TestWorkerPlan_RegistryLoginIdempotent covers the Check half: once the marker file matches the
// current --registry-user, `tart login` doesn't run again.
func TestWorkerPlan_RegistryLoginIdempotent(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	opts.RegistryUser = "gm2211"
	opts.RegistryToken = "ghp_supersecrettoken"

	markerDir := filepath.Join(home, ".config", "grove")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "registry-login.ghcr.io"), []byte("gm2211 ghcr.io\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if r.CalledWith("tart login ghcr.io --username gm2211 --password-stdin") {
		t.Error("`tart login` should not run again once the marker already matches")
	}
}
