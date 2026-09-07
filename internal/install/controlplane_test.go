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
	destOrchard := filepath.Join(home, ".local", "bin", "orchard")
	r.Script(destOrchard+" create service-account grove --roles compute:read,compute:write,compute:connect",
		FakeResult{Stdout: "token: abcdef0123456789\n"})

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
	if cfg.Orchard.Token != "abcdef0123456789" {
		t.Errorf("cfg.Orchard.Token = %q, want the token extracted from `orchard create service-account`", cfg.Orchard.Token)
	}
	if cfg.Fleet != filepath.Join(cfgDir, "fleet.yaml") {
		t.Errorf("cfg.Fleet = %q, want %q", cfg.Fleet, filepath.Join(cfgDir, "fleet.yaml"))
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
	destOrchard := filepath.Join(home, ".local", "bin", "orchard")
	r.Script(destOrchard+" create service-account grove --roles compute:read,compute:write,compute:connect",
		FakeResult{Stdout: "token: abcdef0123456789\n"})

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
