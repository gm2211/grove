package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/server"
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

// fakeReconcileOrchard is a minimal in-memory orchard.Client for fleetReconcileLoop tests: it
// actually stores what CreateVM/DeleteVM are given (mirroring what the real Orchard controller
// would report back on the next ListVMs) so a second Plan/Apply against the same fake sees the
// created VM and doesn't recreate it — that idempotency is exactly what these tests are proving
// `grove serve`'s reconcile loop now gives operators.
type fakeReconcileOrchard struct {
	mu sync.Mutex

	workers []orchard.Worker
	vms     []orchard.VM

	createCalls int
	deleteCalls int
}

func (f *fakeReconcileOrchard) ListWorkers(context.Context) ([]orchard.Worker, error) {
	return f.workers, nil
}

func (f *fakeReconcileOrchard) PauseWorker(context.Context, string) error  { return nil }
func (f *fakeReconcileOrchard) ResumeWorker(context.Context, string) error { return nil }

func (f *fakeReconcileOrchard) ListVMs(context.Context) ([]orchard.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]orchard.VM, len(f.vms))
	copy(out, f.vms)
	return out, nil
}

func (f *fakeReconcileOrchard) GetVM(_ context.Context, name string) (*orchard.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.vms {
		if vm.Name == name {
			cp := vm
			return &cp, nil
		}
	}
	return nil, errors.New("fakeReconcileOrchard: vm not found")
}

func (f *fakeReconcileOrchard) CreateVM(_ context.Context, spec orchard.VMSpec) (*orchard.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	vm := orchard.VM{
		Name: spec.Name,
		// Status "running" (rather than Orchard's real "pending" while a VM is still being
		// scheduled) so specDrifted compares CPU/Memory immediately instead of skipping them —
		// see internal/fleet/reconcile.go's specDrifted doc comment. That's what lets the second
		// Plan() in these tests see "already up to date" instead of missing a real drift check.
		Status:          "running",
		Image:           spec.Image,
		Worker:          spec.Worker,
		CPU:             spec.CPU,
		Memory:          spec.Memory,
		DiskSize:        spec.DiskSize,
		Labels:          spec.Labels,
		RestartPolicy:   spec.RestartPolicy,
		StartupScript:   spec.StartupScript,
		ShutdownScript:  spec.ShutdownScript,
		ShutdownTimeout: spec.ShutdownTimeout,
		TTL:             spec.TTL,
	}
	f.vms = append(f.vms, vm)
	return &vm, nil
}

func (f *fakeReconcileOrchard) DeleteVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	for i, vm := range f.vms {
		if vm.Name == name {
			f.vms = append(f.vms[:i], f.vms[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeReconcileOrchard) Exec(context.Context, string, []string, orchard.ExecOptions) (orchard.ExecSession, error) {
	return nil, errors.New("fakeReconcileOrchard: Exec not implemented")
}

func (f *fakeReconcileOrchard) Ping(context.Context) error { return nil }

// TestFleetReconcileLoop_CreatesOnceThenNoOp is the regression test for the fix itself: this is
// exactly the shape of the gap ARCHITECTURE.md's "Recycling / hygiene" step 3 promises and
// `grove serve` never delivered — a pool wants 1 VM per worker, one worker is online, so the
// first tick must create it; the second tick, seeing the VM Orchard now reports back, must be a
// no-op rather than creating a duplicate.
func TestFleetReconcileLoop_CreatesOnceThenNoOp(t *testing.T) {
	fleetPath := filepath.Join(t.TempDir(), "fleet.yaml")
	spec := `
pools:
  - name: synth
    image: ghcr.io/example/synth:latest
    perWorker: 1
    cpu: 4
    memory: 4096
`
	if err := os.WriteFile(fleetPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	oc := &fakeReconcileOrchard{workers: []orchard.Worker{{Name: "mac1"}}}
	cfg := &config.Config{Fleet: fleetPath}
	status := &server.FleetReconcileStatus{}

	loop := newFleetReconcileLoop(cfg, oc, status)

	loop.tick(context.Background())
	if oc.createCalls != 1 {
		t.Fatalf("after first tick: createCalls = %d, want 1", oc.createCalls)
	}
	if oc.deleteCalls != 0 {
		t.Fatalf("after first tick: deleteCalls = %d, want 0", oc.deleteCalls)
	}

	loop.tick(context.Background())
	if oc.createCalls != 1 {
		t.Fatalf("after second tick: createCalls = %d, want 1 (already up to date, no new create)", oc.createCalls)
	}
	if oc.deleteCalls != 0 {
		t.Fatalf("after second tick: deleteCalls = %d, want 0", oc.deleteCalls)
	}
}

// TestFleetReconcileLoop_MissingFleetSpec_SkipsTickWithoutPanicking guards the "don't crash the
// API/UI" requirement: a tick against an unreadable/invalid fleet.yaml must not panic and must
// not touch Orchard at all — it's skipped, logged once, and retried on the next tick.
func TestFleetReconcileLoop_MissingFleetSpec_SkipsTickWithoutPanicking(t *testing.T) {
	oc := &fakeReconcileOrchard{}
	cfg := &config.Config{Fleet: filepath.Join(t.TempDir(), "does-not-exist.yaml")}
	status := &server.FleetReconcileStatus{}

	loop := newFleetReconcileLoop(cfg, oc, status)
	loop.tick(context.Background())

	if oc.createCalls != 0 || oc.deleteCalls != 0 {
		t.Fatalf("expected no orchard calls for an unreadable fleet spec, got creates=%d deletes=%d", oc.createCalls, oc.deleteCalls)
	}
}
