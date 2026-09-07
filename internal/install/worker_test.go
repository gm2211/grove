package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
		Token:      "tok",
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

	// The privileged checklist (sudo / launchctl bootstrap) must never actually execute without
	// --yes as root — that's the whole point of gating them.
	for _, forbidden := range []string{
		"sudo pmset -a disablesleep 1",
		"launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + filepath.Join(home, "Library", "LaunchAgents", "com.grove.orchard-worker.plist"),
	} {
		if r.CalledWith(forbidden) {
			t.Errorf("privileged command ran without --yes as root: %q", forbidden)
		}
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
	want, err := workerLaunchAgentPlist(opts, filepath.Join(home, ".local", "bin", "orchard"))
	if err != nil {
		t.Fatalf("workerLaunchAgentPlist: %v", err)
	}
	if string(got) != want {
		t.Errorf("rendered plist mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
