package dispatch

import (
	"fmt"
	"strings"
)

// PoolConfig is what the dispatch service needs to know about one fleet.yaml pool to run its jobs.
// Every pool is sealed (network isolated): its jobs are driven from the control plane in the
// pool's VMs (see internal/sandbox).
type PoolConfig struct {
	Name string
	// RunnerImage is the container image a Linux pool's jobs run in (see RunnerImageOrDefault).
	RunnerImage string
	// CPU/Memory are this pool's per-JOB sizing (MHz at 2000 per core / MiB) — see
	// fleet.Pool.JobCPU/JobMemory. They size each job's container and are what Slots is computed
	// from; they are distinct from the pool's VM-level cpu/memory. Zero leaves a job unlimited.
	CPU    int
	Memory int
	// Slots is how many jobs one of the pool's VMs runs at once (see sandbox.Slots). Values below
	// 1 mean 1, and a macOS pool always runs one job per VM.
	Slots int
	// OS is the pool's guest OS ("linux" or "darwin", see sandbox.Request.OS). "" means linux.
	OS string
}

const DefaultRunnerImage = "ghcr.io/gm2211/grove-runner:latest"

// RunnerImageOrDefault validates an image reference before it reaches a command line in a VM.
func RunnerImageOrDefault(value string) (string, error) {
	if value == "" {
		return DefaultRunnerImage, nil
	}
	if len(value) > 512 {
		return "", fmt.Errorf("runner image reference exceeds 512 bytes")
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/:@+", r)) {
			return "", fmt.Errorf("runner image reference contains unsafe character %q", r)
		}
	}
	if strings.ContainsRune("-.:@/", rune(value[0])) {
		return "", fmt.Errorf("runner image reference has invalid prefix")
	}
	return value, nil
}
