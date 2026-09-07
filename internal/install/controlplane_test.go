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
