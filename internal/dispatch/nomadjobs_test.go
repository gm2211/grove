package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureJobs_RegistersAllKindsPerPool(t *testing.T) {
	nc := &fakeNomad{}
	pools := []PoolConfig{{Name: "linux"}, {Name: "macos"}}
	if err := EnsureJobs(context.Background(), nc, pools); err != nil {
		t.Fatalf("EnsureJobs: %v", err)
	}

	want := len(allKinds) * len(pools)
	if len(nc.registerCalls) != want {
		t.Fatalf("registered %d jobs, want %d", len(nc.registerCalls), want)
	}

	for _, kind := range allKinds {
		for _, pool := range pools {
			name := `job "grove-` + string(kind) + "-" + pool.Name + `"`
			found := false
			for _, hcl := range nc.registerCalls {
				if strings.Contains(hcl, name) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("no registered job HCL found containing %s", name)
			}
		}
	}
}

func TestEnsureJobs_PropagatesRegisterError(t *testing.T) {
	nc := &fakeNomad{registerErr: context.DeadlineExceeded}
	if err := EnsureJobs(context.Background(), nc, []PoolConfig{{Name: "linux"}}); err == nil {
		t.Fatal("expected an error")
	}
}

// TestEnsureJobs_RendersPoolCPUAndMemory asserts a pool's PoolConfig.CPU/Memory (fleet.yaml's
// jobCPU/jobMemory, defaulted by fleet.Pool.JobCPUOrDefault/JobMemoryOrDefault before reaching
// here — see internal/cli/serve.go's poolConfigs) actually reaches the rendered Nomad job spec's
// `resources` block, distinct per pool.
func TestEnsureJobs_RendersPoolCPUAndMemory(t *testing.T) {
	nc := &fakeNomad{}
	pools := []PoolConfig{
		{Name: "linux", CPU: 1500, Memory: 3072},
		{Name: "macos", CPU: 500, Memory: 1024},
	}
	if err := EnsureJobs(context.Background(), nc, pools); err != nil {
		t.Fatalf("EnsureJobs: %v", err)
	}

	findRegistered := func(jobName string) string {
		t.Helper()
		for _, hcl := range nc.registerCalls {
			if strings.Contains(hcl, `job "`+jobName+`"`) {
				return hcl
			}
		}
		t.Fatalf("no registered job HCL found for %q", jobName)
		return ""
	}

	linuxShell := findRegistered("grove-shell-linux")
	if !strings.Contains(linuxShell, "cpu    = 1500") || !strings.Contains(linuxShell, "memory = 3072") {
		t.Errorf("grove-shell-linux resources block missing overridden cpu/memory:\n%s", linuxShell)
	}

	macosShell := findRegistered("grove-shell-macos")
	if !strings.Contains(macosShell, "cpu    = 500") || !strings.Contains(macosShell, "memory = 1024") {
		t.Errorf("grove-shell-macos resources block missing overridden cpu/memory:\n%s", macosShell)
	}
}

func TestEnsureJobs_RendersDefaultAndCustomRunnerImages(t *testing.T) {
	nc := &fakeNomad{}
	custom := "registry.example.test/grove/custom-runner:v1"
	if err := EnsureJobs(context.Background(), nc, []PoolConfig{{Name: "linux", RunnerImage: custom}}); err != nil {
		t.Fatalf("EnsureJobs: %v", err)
	}
	if len(nc.registerCalls) != len(allKinds) {
		t.Fatalf("registered %d jobs, want %d", len(nc.registerCalls), len(allKinds))
	}
	for _, hcl := range nc.registerCalls {
		if !strings.Contains(hcl, `image   = "`+custom+`"`) || !strings.Contains(hcl, `DEFAULT_IMAGE="`+custom+`"`) {
			t.Errorf("custom runner image missing from rendered job:\n%s", hcl)
		}
	}

	nc = &fakeNomad{}
	if err := EnsureJobs(context.Background(), nc, []PoolConfig{{Name: "linux"}}); err != nil {
		t.Fatalf("EnsureJobs default: %v", err)
	}
	for _, hcl := range nc.registerCalls {
		if !strings.Contains(hcl, `image   = "`+DefaultRunnerImage+`"`) {
			t.Errorf("default runner image missing from rendered job")
		}
	}
}

func TestEnsureJobs_RejectsUnsafeRunnerImage(t *testing.T) {
	for _, image := range []string{"bad\"\njob ", "${NOMAD_TOKEN}", "{{template}}", "-flag"} {
		nc := &fakeNomad{}
		pools := []PoolConfig{{Name: "valid"}, {Name: "invalid", RunnerImage: image}}
		if err := EnsureJobs(context.Background(), nc, pools); err == nil {
			t.Errorf("unsafe runner image %q accepted", image)
		}
		if len(nc.registerCalls) != 0 {
			t.Errorf("unsafe runner image %q registered a job", image)
		}
	}
}

func TestEnsureJobs_SealedPoolUsesSandboxTemplate(t *testing.T) {
	old := groveBinary
	groveBinary = func() (string, error) { return "/usr/local/bin/grove", nil }
	defer func() { groveBinary = old }()

	nc := &fakeNomad{}
	pools := []PoolConfig{{Name: "linux", Isolated: true, Slots: 4, CPU: 500, Memory: 1024}, {Name: "macos"}}
	if err := EnsureJobs(context.Background(), nc, pools); err != nil {
		t.Fatalf("EnsureJobs: %v", err)
	}
	for _, hcl := range nc.registerCalls {
		sealed := strings.Contains(hcl, `-linux"`)
		if got := strings.Contains(hcl, `"sandbox", "run"`); got != sealed {
			t.Errorf("sandbox runner in job = %v, want %v:\n%s", got, sealed, hcl)
		}
		if sealed && (!strings.Contains(hcl, `command = "/usr/local/bin/grove"`) || !strings.Contains(hcl, `"--slots", "4"`)) {
			t.Errorf("sealed job lacks grove's path or slot count:\n%s", hcl)
		}
	}
}

func TestStableGroveBinary(t *testing.T) {
	prefix := t.TempDir()
	cellar := filepath.Join(prefix, "Cellar", "grove", "0.1.15", "bin", "grove")
	if got, err := stableGroveBinary(cellar); err != nil || got != cellar {
		t.Fatalf("without a bin link: %q, %v; want the Cellar path", got, err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "grove"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := stableGroveBinary(cellar); err != nil || got != filepath.Join(prefix, "bin", "grove") {
		t.Fatalf("with a bin link: %q, %v", got, err)
	}
	for _, bad := range []string{"grove", "/tmp/${x}/grove", `/tmp/a"b`} {
		if _, err := stableGroveBinary(bad); err == nil {
			t.Errorf("stableGroveBinary(%q) accepted it", bad)
		}
	}
}
