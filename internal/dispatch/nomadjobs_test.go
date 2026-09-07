package dispatch

import (
	"context"
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
