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
		User:       "worker-user",
		Home:       home,
		GOOS:       "darwin",
		GOARCH:     "arm64",
		LookPath:   lookPathOnly("tailscale"),
		Exists:     alwaysFalseExists,
		// Never the real /Library/LaunchDaemons.
		LaunchDaemons: filepath.Join(home, "LaunchDaemons"),
	}
}

func TestWorkerPlan_AppliesCommandsAndGatesPrivilegedSteps(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard})
	if err != nil {
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
		if c.Name == "/usr/bin/env" && len(c.Args) >= 2 && c.Args[1] == "/bin/bash" {
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

	// Nothing that needs root runs without --yes as root, and the unprivileged install loads no
	// launchd job itself: the daemons are the admin's step.
	for _, c := range r.Calls {
		if c.Name == "sudo" {
			t.Errorf("no `sudo` command should run without --yes as root, got: %s", c.String())
		}
		if c.Name == "launchctl" && len(c.Args) > 0 && (c.Args[0] == "bootstrap" || c.Args[0] == "bootout" || c.Args[0] == "kickstart") {
			t.Errorf("the unprivileged install must not (re)load launchd jobs, got: %s", c.String())
		}
	}
	for _, res := range results {
		if res.Step == "worker-daemons" && (res.Applied || !res.Skipped) {
			t.Errorf("worker-daemons must be printed for an admin, got %+v", res)
		}
	}

	// The worker's LaunchDaemon is staged, for the admin's install command to copy.
	staged, _ := daemonPaths(opts, orchardWorkerLabel)
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("reading staged plist: %v", err)
	}
	want, err := workerDaemonPlist(opts, workerWrapperPath(opts))
	if err != nil {
		t.Fatalf("workerDaemonPlist: %v", err)
	}
	if string(got) != want {
		t.Errorf("staged plist mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if !strings.Contains(string(got), "<key>UserName</key>\n\t<string>"+opts.User+"</string>") {
		t.Errorf("worker LaunchDaemon must run as the worker user (a daemon without UserName is root):\n%s", got)
	}
	if strings.Contains(string(got), opts.Token) {
		t.Fatal("worker bootstrap credential leaked into the LaunchDaemon")
	}
	if _, err := os.Stat(legacyAgentPlist(opts, orchardWorkerLabel)); !os.IsNotExist(err) {
		t.Errorf("no LaunchAgent should be written any more, stat err = %v", err)
	}

	// The credential is a 0600 file in a 0700 directory, not a Keychain item.
	credFile := credentialPath(opts, workerBootstrapCredentialName)
	if b, err := os.ReadFile(credFile); err != nil || string(b) != opts.Token+"\n" {
		t.Fatalf("worker credential file = %q, %v", b, err)
	}
	if info, err := os.Stat(credFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("worker credential file mode = %v, %v; want 0600", info.Mode(), err)
	}
	if info, err := os.Stat(credentialsDir(opts)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("credentials dir mode = %v, %v; want 0700", info.Mode(), err)
	}
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/expect" || strings.Contains(c.String(), "add-generic-password") {
			t.Errorf("worker credential must not go through Keychain: %s", c.String())
		}
		for _, arg := range c.Args {
			if strings.Contains(arg, opts.Token) {
				t.Fatalf("worker credential leaked into argv: %s", c.String())
			}
		}
	}

	wrapper, err := os.ReadFile(workerWrapperPath(opts))
	if err != nil {
		t.Fatalf("reading worker wrapper: %v", err)
	}
	if strings.Contains(string(wrapper), opts.Token) || !strings.Contains(string(wrapper), "--bootstrap-token-stdin") {
		t.Fatalf("worker wrapper must feed the credential over stdin without embedding it: %s", wrapper)
	}
	if !strings.Contains(string(wrapper), `< "`+credFile+`"`) || strings.Contains(string(wrapper), "security") {
		t.Fatalf("worker wrapper must read the credential file, not Keychain: %s", wrapper)
	}
	if !strings.Contains(string(wrapper), " --guest-agent-transport ") {
		t.Fatalf("worker must reach its VMs through the Tart Guest Agent, not the network: %s", wrapper)
	}
	if !strings.Contains(string(wrapper), `export PATH="$grove_tart_bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"`) {
		t.Fatal("worker wrapper must put the Grove Tart shim first on PATH, ahead of Homebrew")
	}
	if !strings.Contains(string(wrapper), `grove_tart_bin=`+strconv.Quote(groveTartBinDir(opts))) ||
		!strings.Contains(string(wrapper), `[ -x "$grove_tart_bin/tart" ] || {`) ||
		!strings.Contains(string(wrapper), "exit 78") {
		t.Fatalf("worker wrapper must fail closed (exit 78) when the headless Tart is missing: %s", wrapper)
	}
	if !strings.Contains(string(got), "<key>AssociatedBundleIdentifiers</key>") ||
		!strings.Contains(string(got), orchardWorkerBundleID) {
		t.Error("worker LaunchDaemon must identify the Orchard app as responsible code for Local Network consent")
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

func TestEnsureWorkerOrchardApp_MigratesExistingBinaryThenRebuildsAtThePin(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, []byte("existing fork binary of unknown commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	r.Script("launchctl print system/"+orchardWorkerLabel, FakeResult{Stdout: "com.grove.orchard-worker = {\n\tpid = 4242\n}\n"})

	if err := ensureWorkerOrchardApp(context.Background(), r, opts, cliPath, io.Discard); err != nil {
		t.Fatalf("ensureWorkerOrchardApp: %v", err)
	}
	if target, err := os.Readlink(cliPath); err != nil || target != workerOrchardExecutable(opts) {
		t.Fatalf("CLI link = %q, %v; want %q", target, err, workerOrchardExecutable(opts))
	}
	build := orchardBuildCall(t, r)
	if !strings.Contains(build, "git fetch -q --depth 1 https://github.com/gm2211/orchard "+OrchardForkCommit) ||
		!strings.Contains(build, "git rev-parse HEAD") || strings.Contains(build, "git clone") {
		t.Fatalf("Orchard must be built from the pinned commit, not the tip of main: %s", build)
	}
	if !workerOrchardAtPinnedCommit(opts) {
		t.Fatal("the rebuilt bundle doesn't record the pinned commit")
	}
	if !r.CalledWith("kill -TERM 4242") {
		t.Error("a running worker must be restarted onto the rebuilt binary")
	}

	// At the pin already: nothing to build, nothing to restart.
	r = NewFakeRunner()
	if err := ensureWorkerOrchardApp(context.Background(), r, opts, cliPath, io.Discard); err != nil {
		t.Fatalf("ensureWorkerOrchardApp: %v", err)
	}
	for _, c := range r.Calls {
		if c.Name == "/bin/sh" || c.Name == "launchctl" {
			t.Fatalf("a bundle at the pinned commit must be left alone, ran: %s", c.String())
		}
	}
}

func TestWorkerOrchardAppInstalled_RequiresThePinnedCommit(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	cliPath := filepath.Join(home, ".local", "bin", "orchard")
	if err := ensureWorkerOrchardApp(context.Background(), NewFakeRunner(), opts, cliPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The fake build moved nothing into place.
	if err := os.WriteFile(workerOrchardExecutable(opts), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := workerOrchardAppInstalled(context.Background(), NewFakeRunner(), opts, cliPath); !ok {
		t.Fatal("a bundle at the pinned commit should pass its check")
	}
	if err := os.WriteFile(workerOrchardCommitPath(opts), []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := workerOrchardAppInstalled(context.Background(), NewFakeRunner(), opts, cliPath); ok {
		t.Fatal("a bundle built from another commit must be rebuilt when the pin moves")
	}
}

func orchardBuildCall(t *testing.T, r *FakeRunner) string {
	t.Helper()
	for _, c := range r.Calls {
		if c.Name == "/bin/sh" && len(c.Args) == 2 && c.Args[0] == "-c" && strings.Contains(c.Args[1], "./cmd/orchard") {
			return c.Args[1]
		}
	}
	t.Fatal("Orchard was not built")
	return ""
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
	// Already at the pin, so only the signing runs for real.
	for path, content := range map[string][]byte{
		workerOrchardExecutable(opts): binary,
		workerOrchardCommitPath(opts): []byte(OrchardForkCommit + "\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o755); err != nil {
			t.Fatal(err)
		}
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
	scriptPinnedDownloads(r, opts)

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
	scriptPinnedDownloads(r, opts)

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
	scriptPinnedDownloads(r, opts)

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
	scriptPinnedDownloads(r, opts)

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
	scriptPinnedDownloads(r, opts)

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
		"/usr/bin/ditto",
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" {
			t.Errorf("no sudo command should run during --dry-run, got: %s", c.String())
		}
		for _, f := range forbidden {
			if c.String() == f || (f == "/usr/bin/ditto" && c.Name == f) {
				t.Errorf("mutating command %q ran during --dry-run", c.String())
			}
		}
	}

	// Nothing should have been written to disk either.
	staged, _ := daemonPaths(opts, orchardWorkerLabel)
	for _, p := range []string{staged, credentialPath(opts, workerBootstrapCredentialName), workerWrapperPath(opts), groveTartAppDir(opts), groveTartShimPath(opts), groveTartEntitlementsPath(opts)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s not to be written during --dry-run, stat err = %v", p, err)
		}
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
	scriptPinnedDownloads(r, opts)
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

	// The daemon can't read the Keychain `tart login` writes to before anyone logs in, so the login
	// is also a 0600 file, handed to Tart by the wrapper through its environment variables.
	credFile := credentialPath(opts, registryCredentialName)
	if b, err := os.ReadFile(credFile); err != nil || string(b) != "gm2211\n"+opts.RegistryToken+"\n" {
		t.Errorf("registry credential file = %q, %v", b, err)
	}
	if info, err := os.Stat(credFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("registry credential file mode = %v, %v; want 0600", info.Mode(), err)
	}
	wrapper, err := os.ReadFile(workerWrapperPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{credFile, "TART_REGISTRY_USERNAME", "TART_REGISTRY_PASSWORD", "read -r"} {
		if !strings.Contains(string(wrapper), want) {
			t.Errorf("wrapper lacks %q:\n%s", want, wrapper)
		}
	}
	plist, _ := os.ReadFile(func() string { s, _ := daemonPaths(opts, orchardWorkerLabel); return s }())
	for name, text := range map[string]string{"wrapper": string(wrapper), "plist": string(plist)} {
		if strings.Contains(text, opts.RegistryToken) {
			t.Errorf("registry token leaked into the %s", name)
		}
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
	scriptPinnedDownloads(r, opts)

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
	scriptPinnedDownloads(r, opts)
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
	scriptPinnedDownloads(r, opts)
	opts.RegistryUser = "gm2211"
	opts.RegistryToken = "ghp_supersecrettoken"

	markerDir := filepath.Join(home, ".config", "grove")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "registry-login.ghcr.io"), []byte("gm2211 ghcr.io\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := writeSecretFile(credentialPath(opts, registryCredentialName), "gm2211\n"+opts.RegistryToken+"\n"); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
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

// TestWorkerPlan_RerunKeepsInstalledControllerAndCredential covers `grove install --role worker`
// with no --controller/--token on a Mac that already runs a worker (the documented upgrade path,
// or adding a registry login): it keeps the controller in the existing wrapper and the credential
// already in Keychain, instead of rewriting the wrapper with an empty controller.
func TestWorkerPlan_RerunKeepsInstalledControllerAndCredential(t *testing.T) {
	home := t.TempDir()
	first := testWorkerOptions(home)
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptPinnedDownloads(r, first)
	steps, err := BuildPlan(r, first, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan (install): %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan (install): %v", err)
	}
	before, err := os.ReadFile(workerWrapperPath(first))
	if err != nil {
		t.Fatalf("reading the installed wrapper: %v", err)
	}

	rerun := testWorkerOptions(home)
	rerun.Controller, rerun.Token = "", ""
	rerun.RegistryUser, rerun.RegistryToken = "gm2211", "ghp_supersecrettoken"
	r = NewFakeRunner()
	scriptTailscaleUp(r)
	scriptPinnedDownloads(r, rerun)
	steps, err = BuildPlan(r, rerun, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan (re-run): %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan (re-run): %v", err)
	}

	after, err := os.ReadFile(workerWrapperPath(rerun))
	if err != nil {
		t.Fatalf("reading the wrapper after the re-run: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("re-run rewrote the wrapper:\nbefore: %s\nafter:  %s", before, after)
	}
	if !strings.Contains(string(after), first.Controller) {
		t.Errorf("wrapper lost the controller %q: %s", first.Controller, after)
	}
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/expect" || (len(c.Args) > 0 && c.Args[0] == "delete-generic-password") {
			t.Errorf("re-run touched the worker's Keychain credential: %s", c)
		}
	}
	if !r.CalledWith("tart login ghcr.io --username gm2211 --password-stdin") {
		t.Errorf("re-run with a registry token should log in, calls: %v", r.Calls)
	}
}

// TestWorkerPlan_FirstInstallNeedsController: with no worker installed there is no controller to
// reuse, so the plan refuses rather than render a wrapper that points nowhere.
func TestWorkerPlan_FirstInstallNeedsController(t *testing.T) {
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(t.TempDir())
	opts.Controller, opts.Token = "", ""
	if _, err := BuildPlan(r, opts, io.Discard); err == nil || !strings.Contains(err.Error(), "--controller") {
		t.Fatalf("BuildPlan with no controller and no installed worker: err = %v, want one naming --controller", err)
	}
}

// TestWorkerPlan_RerunWithoutKeychainCredentialAsksForSetup: a re-run without --token on a worker
// whose Keychain item is gone can't make one up, and says to run `grove setup`.
func TestWorkerPlan_RerunWithoutKeychainCredentialAsksForSetup(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	if err := os.MkdirAll(filepath.Dir(workerWrapperPath(opts)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerWrapperPath(opts), []byte(workerWrapper(opts, "/x/orchard")), 0o700); err != nil {
		t.Fatal(err)
	}
	opts.Controller, opts.Token = "", ""
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptPinnedDownloads(r, opts)
	r.Script("/usr/bin/security find-generic-password -a test-worker -s "+orchardWorkerKeychainService+" -w",
		FakeResult{Err: errors.New("exit status 44")})
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err == nil || !strings.Contains(err.Error(), "grove setup") {
		t.Fatalf("RunPlan: err = %v, want one pointing at `grove setup`", err)
	}
}

// TestRegistryLogin_LogsInEvenWhenRecorded covers `grove registry login`: it logs in again for the
// same user (a rotated token), feeds the token only over stdin, and records only user and registry.
func TestRegistryLogin_LogsInEvenWhenRecorded(t *testing.T) {
	home := t.TempDir()
	opts := Options{Role: RoleWorker, Home: home, RegistryUser: "gm2211", RegistryToken: "ghp_rotatedtoken"}
	marker := registryLoginMarkerPath(opts, "ghcr.io")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("gm2211 ghcr.io\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewFakeRunner()
	if err := RegistryLogin(context.Background(), r, opts); err != nil {
		t.Fatalf("RegistryLogin: %v", err)
	}
	const wantCmd = "tart login ghcr.io --username gm2211 --password-stdin"
	if stdin, ok := r.StdinFor(wantCmd); !ok || stdin != opts.RegistryToken {
		t.Fatalf("expected %q with the token on stdin, calls: %v", wantCmd, r.Calls)
	}
	for _, c := range r.Calls {
		if strings.Contains(c.String(), opts.RegistryToken) {
			t.Errorf("token leaked into a command line: %s", c)
		}
	}
	if got, _ := os.ReadFile(marker); string(got) != "gm2211 ghcr.io\n" {
		t.Errorf("marker = %q, want only user and registry", got)
	}

	opts.RegistryToken = ""
	if err := RegistryLogin(context.Background(), NewFakeRunner(), opts); err == nil {
		t.Error("RegistryLogin with no token should fail")
	}
}

// TestInstalledWorkerName reads the worker name from grove's wrapper, else from a LaunchAgent
// written before the wrapper existed, so a re-run keeps a name that isn't the hostname.
func TestInstalledWorkerName(t *testing.T) {
	writeLegacyPlist := func(t *testing.T, opts Options, args string) {
		t.Helper()
		if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		plist := `<plist version="1.0"><dict><key>ProgramArguments</key><array>
		<string>/Users/argos/Applications/Grove Orchard Worker.app/Contents/MacOS/orchard</string>
		<string>worker</string><string>run</string>` + args + `
		<string>http://100.101.229.60:6120</string></array></dict></plist>`
		if err := os.WriteFile(filepath.Join(opts.LaunchAgentsDir(), "com.grove.orchard-worker.plist"), []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("none installed", func(t *testing.T) {
		if got := installedWorkerName(testWorkerOptions(t.TempDir())); got != "" {
			t.Errorf("installedWorkerName = %q, want empty", got)
		}
	})
	t.Run("wrapper", func(t *testing.T) {
		opts := testWorkerOptions(t.TempDir())
		opts.Hostname = "studio"
		if err := os.MkdirAll(filepath.Dir(workerWrapperPath(opts)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(workerWrapperPath(opts), []byte(workerWrapper(opts, "/x/orchard")), 0o700); err != nil {
			t.Fatal(err)
		}
		writeLegacyPlist(t, opts, `<string>--name</string><string>ignored</string>`)
		if got := installedWorkerName(opts); got != "studio" {
			t.Errorf("installedWorkerName = %q, want the wrapper's %q", got, "studio")
		}
	})
	t.Run("legacy LaunchAgent", func(t *testing.T) {
		opts := testWorkerOptions(t.TempDir())
		writeLegacyPlist(t, opts, `<string>--name</string>
		<string>studio</string>`)
		if got := installedWorkerName(opts); got != "studio" {
			t.Errorf("installedWorkerName = %q, want %q", got, "studio")
		}
	})
	t.Run("legacy LaunchAgent, --name=", func(t *testing.T) {
		opts := testWorkerOptions(t.TempDir())
		writeLegacyPlist(t, opts, `<string>--name=studio</string>`)
		if got := installedWorkerName(opts); got != "studio" {
			t.Errorf("installedWorkerName = %q, want %q", got, "studio")
		}
	})
}

// TestWorkerPlan_KeepsLegacyWorkerName: a Mac whose worker predates grove's wrapper (and runs
// under a name that isn't its hostname) keeps that name when the install takes it over, so the
// pools whose workerSelector matches it still land there.
func TestWorkerPlan_KeepsLegacyWorkerName(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `<array><string>orchard</string><string>worker</string><string>run</string><string>--name</string><string>studio</string></array>`
	if err := os.WriteFile(filepath.Join(opts.LaunchAgentsDir(), "com.grove.orchard-worker.plist"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Hostname = ""
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptPinnedDownloads(r, opts)
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	wrapper, err := os.ReadFile(workerWrapperPath(opts))
	if err != nil {
		t.Fatalf("reading the wrapper: %v", err)
	}
	if !strings.Contains(string(wrapper), `--name "studio"`) || !strings.Contains(string(wrapper), `host=studio,`) {
		t.Errorf("wrapper doesn't keep the installed worker name: %s", wrapper)
	}

	// --name wins over the installed name
	opts.Hostname = "studio-2"
	if got := workerWrapper(opts, "/x/orchard"); !strings.Contains(got, `--name "studio-2"`) {
		t.Errorf("--name not used: %s", got)
	}
}
