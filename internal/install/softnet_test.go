package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestWorkerPlan_InstallsSoftnetAndLeavesRootStepToAHuman(t *testing.T) {
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(t.TempDir())
	opts.Softnet = true

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if !r.CalledWith("brew install openai/tools/softnet") {
		t.Error("expected `brew install openai/tools/softnet` to run")
	}

	var root *Step
	for i := range steps {
		if steps[i].Name == "softnet-root" {
			root = &steps[i]
		}
	}
	if root == nil {
		t.Fatal("worker plan has no softnet-root step")
	}
	if !root.Privileged || !strings.Contains(root.Description, softnetRootCommand) {
		t.Fatalf("softnet-root must be a privileged, printed step, got %+v", *root)
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" || c.Name == "chmod" || c.Name == "chown" {
			t.Errorf("no privileged command should run without --yes as root, got: %s", c.String())
		}
	}
}

func TestWorkerPlan_NoSoftnetUnlessAsked(t *testing.T) {
	r := NewFakeRunner()
	scriptTailscaleUp(r)

	steps, err := BuildPlan(r, testWorkerOptions(t.TempDir()), io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, step := range steps {
		if strings.HasPrefix(step.Name, "softnet") {
			t.Errorf("worker plan without --softnet has step %q", step.Name)
		}
	}
}

func TestWorkerPlan_SoftnetRootSatisfiedBySUIDBit(t *testing.T) {
	r := NewFakeRunner()
	r.Script("stat -L -f %u %p /usr/bin/softnet", FakeResult{Stdout: "0 104755\n"})
	opts := testWorkerOptions(t.TempDir())
	opts.LookPath = lookPathOnly("tailscale", "softnet")

	for _, step := range softnetSteps(r, opts.lookPath(), io.Discard) {
		done, err := step.Check(context.Background())
		if err != nil || !done {
			t.Errorf("step %s: Check() = %v, %v; want satisfied", step.Name, done, err)
		}
	}
}

func TestSoftnetRootStep_AppliesToTheResolvedBinary(t *testing.T) {
	r := NewFakeRunner()
	r.Script("realpath /opt/homebrew/bin/softnet", FakeResult{Stdout: "/opt/homebrew/Cellar/softnet/0.24.0/bin/softnet\n"})
	lookPath := func(string) (string, error) { return "/opt/homebrew/bin/softnet", nil }

	steps := softnetSteps(r, lookPath, io.Discard)
	if err := steps[1].Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []string{
		"sudo chown root:wheel /opt/homebrew/Cellar/softnet/0.24.0/bin/softnet",
		"sudo chmod u+s /opt/homebrew/Cellar/softnet/0.24.0/bin/softnet",
	} {
		if !r.CalledWith(want) {
			t.Errorf("expected %q, calls: %v", want, r.Calls)
		}
	}
}

func TestSuidRoot(t *testing.T) {
	for in, want := range map[string]bool{
		"0 104755\n":  true,
		"0 100755":    false,
		"501 104755":  false,
		"0":           false,
		"0 notoctal":  false,
		"0 4755 more": false,
	} {
		if got := suidRoot(in); got != want {
			t.Errorf("suidRoot(%q) = %v, want %v", in, got, want)
		}
	}
}

func writeIsolatedFleetYAML(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	content := "pools:\n  - name: linux\n    image: ghcr.io/example/linux:latest\n    perWorker: 1\n    network: isolated\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckSoftnet(t *testing.T) {
	notSUID := FakeResult{Stdout: "501 100755\n"}
	noSudo := FakeResult{Err: errors.New("sudo: a password is required")}

	for _, tt := range []struct {
		name       string
		installed  bool
		stat       FakeResult
		sudo       FakeResult
		isolated   bool
		wantOK     bool
		wantDetail string
	}{
		{name: "missing, not needed", wantOK: true, wantDetail: "only needed for fleet.yaml pools with network: isolated"},
		{name: "missing, needed", isolated: true, wantOK: false, wantDetail: "not installed"},
		{name: "SUID root", installed: true, stat: FakeResult{Stdout: "0 104755\n"}, isolated: true, wantOK: true, wantDetail: "SUID root"},
		{name: "sudo rule", installed: true, stat: notSUID, isolated: true, wantOK: true, wantDetail: "passwordless sudo"},
		{name: "needs a password", installed: true, stat: notSUID, sudo: noSudo, isolated: true, wantOK: false, wantDetail: "can't become root without a password"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := NewFakeRunner()
			r.Script("stat -L -f %u %p /usr/bin/softnet", tt.stat)
			r.Script("sudo -n -l /usr/bin/softnet", tt.sudo)
			opts := Options{GOOS: "darwin", LookPath: lookPathOnly("tart")}
			if tt.installed {
				opts.LookPath = lookPathOnly("tart", "softnet")
			}
			cfg := &config.Config{}
			if tt.isolated {
				cfg.Fleet = writeIsolatedFleetYAML(t)
			}

			got := checkSoftnet(context.Background(), r, opts, cfg)
			if got.OK != tt.wantOK || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Fatalf("checkSoftnet() = %+v, want OK=%v with detail containing %q", got, tt.wantOK, tt.wantDetail)
			}
			if !got.OK && !strings.Contains(got.Remediation, softnetRootCommand) {
				t.Errorf("remediation should give the command to run, got %q", got.Remediation)
			}
		})
	}
}

func TestRunDoctor_ChecksSoftnetOnlyOnMacWorkers(t *testing.T) {
	for _, tt := range []struct {
		name string
		goos string
		path []string
		want bool
	}{
		{name: "mac worker", goos: "darwin", path: []string{"tart"}, want: true},
		{name: "mac without tart", goos: "darwin", want: false},
		{name: "linux", goos: "linux", path: []string{"tart"}, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{Home: t.TempDir(), GOOS: tt.goos, LookPath: lookPathOnly(tt.path...), Exists: alwaysFalseExists}
			results := RunDoctor(context.Background(), NewFakeRunner(), opts, nil)
			found := false
			for _, res := range results {
				if res.Name == "softnet" {
					found = true
				}
			}
			if found != tt.want {
				t.Fatalf("softnet row present = %v, want %v: %+v", found, tt.want, results)
			}
		})
	}
}
