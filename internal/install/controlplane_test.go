package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func testControlPlaneOptions(home string, goos string) Options {
	return Options{
		Role:     RoleControlPlane,
		Hostname: "test-cp",
		Home:     home,
		GOOS:     goos,
		GOARCH:   "arm64",
		LookPath: lookPathOnly("tailscale"),
		Exists:   alwaysFalseExists,
	}
}

func TestControlPlanePlan_DarwinRendersConfigAndLaunchAgents(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "darwin")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if !r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("expected nomad to be installed via brew on darwin")
	}
	if !r.CalledWith("brew install minio/stable/minio") {
		t.Error("expected minio to be installed via brew on darwin")
	}

	cfgDir := filepath.Join(home, ".config", "grove")
	for _, rel := range []string{
		"config.yaml",
		"fleet.yaml",
		filepath.Join("orchard-controller", "env"),
		filepath.Join("nomad", "server.hcl"),
		filepath.Join("minio", "env"),
	} {
		if _, err := os.Stat(filepath.Join(cfgDir, rel)); err != nil {
			t.Errorf("expected %s to be rendered: %v", rel, err)
		}
	}

	for _, label := range []string{"orchard-controller", "nomad", "minio", "server"} {
		p := filepath.Join(home, "Library", "LaunchAgents", "com.grove."+label+".plist")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected launch agent for %s: %v", label, err)
		}
	}

	cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		t.Fatalf("loading rendered config.yaml: %v", err)
	}
	if cfg.Server.Token == "" {
		t.Error("expected a generated server token")
	}
	if cfg.Artifacts.AccessKey == "" || cfg.Artifacts.SecretKey == "" {
		t.Error("expected generated MinIO credentials in config.yaml")
	}
	if cfg.Orchard.Token == "" {
		t.Error("expected a grove-generated orchard service-account token")
	}
	if cfg.Fleet != filepath.Join(cfgDir, "fleet.yaml") {
		t.Errorf("cfg.Fleet = %q, want %q", cfg.Fleet, filepath.Join(cfgDir, "fleet.yaml"))
	}

	// `orchard create service-account` must be called with --roles repeated once per role
	// (the fork's --roles is a StringArrayVar and does not split on commas) and with an
	// explicit --token, since the command prints nothing on success for grove to scrape.
	destOrchard := filepath.Join(home, ".local", "bin", "orchard")
	wantCreate := Call{
		Name: destOrchard,
		Args: []string{
			"create", "service-account", "grove",
			"--token", cfg.Orchard.Token,
			"--roles", "compute:read",
			"--roles", "compute:write",
			"--roles", "compute:connect",
		},
	}
	found := false
	for _, c := range r.Calls {
		if c.String() == wantCreate.String() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected call %q, calls were: %v", wantCreate.String(), r.Calls)
	}

	// No sudo anywhere in the control-plane plan; it has no privileged steps.
	if r.CalledWith("sudo") {
		t.Error("control-plane plan should never invoke sudo")
	}
}

// TestControlPlanePlan_TrustsHashicorpAndMinIOTapsBeforeInstalling covers the fix for the observed
// defect on the control-plane role: recent Homebrew refuses `brew install hashicorp/tap/nomad` and
// `brew install minio/stable/minio` from an untrusted tap, so both must be tapped+trusted, in
// order, before their respective install runs.
func TestControlPlanePlan_TrustsHashicorpAndMinIOTapsBeforeInstalling(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "hashicorp/tap", false, false)
	scriptTapInfo(r, "minio/stable", false, false)
	opts := testControlPlaneOptions(home, "darwin")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	assertOrdered := func(tapCmd, trustCmd, installCmd string) {
		t.Helper()
		tapIdx, trustIdx, installIdx := -1, -1, -1
		for i, c := range r.Calls {
			switch c.String() {
			case tapCmd:
				tapIdx = i
			case trustCmd:
				trustIdx = i
			case installCmd:
				installIdx = i
			}
		}
		if tapIdx == -1 || trustIdx == -1 || installIdx == -1 {
			t.Fatalf("expected %q, %q and %q to all run, calls: %v", tapCmd, trustCmd, installCmd, r.Calls)
		}
		if !(tapIdx < trustIdx && trustIdx < installIdx) {
			t.Errorf("expected tap(%d) < trust(%d) < install(%d) ordering for %q", tapIdx, trustIdx, installIdx, installCmd)
		}
	}
	assertOrdered("brew tap hashicorp/tap", "brew trust hashicorp/tap", "brew install hashicorp/tap/nomad")
	assertOrdered("brew tap minio/stable", "brew trust minio/stable", "brew install minio/stable/minio")
}

// TestControlPlanePlan_SkipsTrustWhenTapsAlreadyTrusted mirrors the worker-role idempotency test:
// once tap-info reports both taps installed and trusted, neither `brew tap` nor `brew trust` should
// run again, though the installs themselves still do (their own Check gates that).
func TestControlPlanePlan_SkipsTrustWhenTapsAlreadyTrusted(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "hashicorp/tap", true, true)
	scriptTapInfo(r, "minio/stable", true, true)
	opts := testControlPlaneOptions(home, "darwin")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, forbidden := range []string{
		"brew tap hashicorp/tap", "brew trust hashicorp/tap",
		"brew tap minio/stable", "brew trust minio/stable",
	} {
		if r.CalledWith(forbidden) {
			t.Errorf("%q should not run when the tap is already installed and trusted", forbidden)
		}
	}
	if !r.CalledWith("brew install hashicorp/tap/nomad") || !r.CalledWith("brew install minio/stable/minio") {
		t.Error("expected nomad and minio to still be installed")
	}
}

// TestControlPlanePlan_LinuxDoesNotTrustTaps confirms the darwin-only trust steps aren't part of
// the Linux plan at all (Linux has no Homebrew, and installs nomad/minio via direct download).
func TestControlPlanePlan_LinuxDoesNotTrustTaps(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, s := range steps {
		if s.Name == "brew-trust:hashicorp/tap" || s.Name == "brew-trust:minio/stable" {
			t.Errorf("did not expect a brew-trust step in the Linux plan, got %q", s.Name)
		}
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if r.CalledWith("brew tap-info --json=v1 hashicorp/tap") {
		t.Error("no brew command should run at all on Linux")
	}
}

func TestControlPlanePlan_LinuxRendersSystemdUnits(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, label := range []string{"orchard-controller", "nomad", "minio", "server"} {
		p := filepath.Join(home, ".config", "systemd", "user", "grove-"+label+".service")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected systemd unit for %s: %v", label, err)
		}
	}

	// Linux has no brew: nomad/minio come from direct downloads instead.
	if r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("should not use brew on linux")
	}
}

func TestControlPlanePlan_DryRunAppliesNothing(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "darwin")

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{DryRun: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("RunPlan (dry-run): %v", err)
	}
	for _, res := range results {
		if res.Applied {
			t.Errorf("step %q was applied during --dry-run", res.Step)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "grove", "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no config.yaml written during --dry-run, stat err = %v", err)
	}
	if r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("no install command should run during --dry-run")
	}
}
