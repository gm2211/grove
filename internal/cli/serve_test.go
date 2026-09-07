package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

// TestPoolConfigs_ThreadsJobCPUAndMemoryFromFleetSpec is the regression test for the
// hardcoded-job-resources defect: EnsureJobs used to always render every job at the templates'
// fixed 2000 MHz/4096 MiB fallback because poolConfigs never read fleet.yaml's jobCPU/jobMemory.
// It must now populate dispatch.PoolConfig.CPU/Memory from the pool's configured (or defaulted)
// job sizing.
func TestPoolConfigs_ThreadsJobCPUAndMemoryFromFleetSpec(t *testing.T) {
	fleetPath := filepath.Join(t.TempDir(), "fleet.yaml")
	spec := `
pools:
  - name: linux
    image: ghcr.io/example/linux:latest
    perWorker: 1
    jobCPU: 1500
    jobMemory: 3072
  - name: macos
    image: ghcr.io/example/macos:latest
    perWorker: 1
`
	if err := os.WriteFile(fleetPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Fleet: fleetPath}
	pools, err := poolConfigs(cfg)
	if err != nil {
		t.Fatalf("poolConfigs: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(pools))
	}

	byName := map[string]int{}
	for i, p := range pools {
		byName[p.Name] = i
	}

	linux := pools[byName["linux"]]
	if linux.CPU != 1500 || linux.Memory != 3072 {
		t.Errorf("linux pool = %+v, want CPU=1500 Memory=3072 (explicit fleet.yaml values)", linux)
	}

	// macos didn't set jobCPU/jobMemory — poolConfigs must still populate the built-in defaults
	// (not leave them at 0, which would fall through to the *template's* different 2000/4096
	// default instead of grove's own 500/1024 per-job default).
	macos := pools[byName["macos"]]
	if macos.CPU != 500 || macos.Memory != 1024 {
		t.Errorf("macos pool = %+v, want the built-in defaults CPU=500 Memory=1024", macos)
	}
}

func TestPoolConfigs_NoFleetConfigured(t *testing.T) {
	pools, err := poolConfigs(&config.Config{})
	if err != nil {
		t.Fatalf("poolConfigs: %v", err)
	}
	if pools != nil {
		t.Errorf("pools = %+v, want nil when cfg.Fleet is unset", pools)
	}
}
