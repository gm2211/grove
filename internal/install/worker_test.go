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

	if !r.CalledWith("brew install cirruslabs/cli/tart") {
		t.Error("expected `brew install cirruslabs/cli/tart` to run")
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
