package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

func intp(n int) *int { return &n }

func elasticSpec() *Spec {
	return &Spec{Pools: []Pool{
		{Name: "linux", Image: "l", CPU: 4, Memory: 8192, MinPerWorker: intp(0), MaxPerWorker: intp(2)},
		{Name: "macos", Image: "m", CPU: 4, Memory: 8192, MinPerWorker: intp(0), MaxPerWorker: intp(2)},
	}}
}

func mac(name string, slots uint64) orchard.Worker {
	return orchard.Worker{Name: name, Labels: map[string]string{}, Capabilities: []string{WorkerCapabilityGuestPasswordRotation}, Resources: map[string]uint64{
		"org.cirruslabs.tart-vms": slots, "logical-cores": 16, "memory-mib": 65536,
	}}
}

func runningVM(r *Reconciler, pool Pool, worker string, n int, created time.Time) orchard.VM {
	vm := existingFrom(r.vmSpec(pool, worker, VMName(pool.Name, worker, n)))
	vm.Status = "running"
	vm.CreatedAt = created
	return vm
}

func TestScale_WaitingJobsAddOneVM(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spec := elasticSpec()
	r := &Reconciler{Spec: spec, Client: &fakeOrchardClient{workers: []orchard.Worker{mac("studio", 2), mac("mbp", 2)}},
		Options: Options{Now: func() time.Time { return now }, Load: &JobLoad{Waiting: map[string]int{"linux": 5}}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 || plan.Creates[0].Pool != "linux" || plan.Creates[0].Reason != "scale up: 5 job(s) waiting" {
		t.Fatalf("want exactly one Linux VM for 5 waiting jobs, got %+v", plan.Creates)
	}

	// While it boots, no second one.
	r.Client.(*fakeOrchardClient).vms = []orchard.VM{runningVM(r, spec.Pools[0], plan.Creates[0].Worker, 0, now.Add(-time.Minute))}
	plan, _ = r.Plan(context.Background())
	if !plan.Empty() {
		t.Fatalf("a booting VM must hold further scale-up, got %+v", plan)
	}
}

func TestScale_NoLoadKeepsCurrentSize(t *testing.T) {
	spec := elasticSpec()
	r := &Reconciler{Spec: spec}
	vm := runningVM(r, spec.Pools[1], "mbp", 0, time.Now())
	r.Client = &fakeOrchardClient{workers: []orchard.Worker{mac("mbp", 2)}, vms: []orchard.VM{vm}}
	plan, err := r.Plan(context.Background())
	if err != nil || !plan.Empty() {
		t.Fatalf("without load an elastic pool keeps what runs: plan=%+v err=%v", plan, err)
	}
}

func TestScale_IdleVMScaledDown(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spec := elasticSpec()
	r := &Reconciler{Spec: spec, Options: Options{Now: func() time.Time { return now }}}
	vm := runningVM(r, spec.Pools[0], "studio", 0, now.Add(-time.Hour))
	r.Client = &fakeOrchardClient{workers: []orchard.Worker{mac("studio", 2)}, vms: []orchard.VM{vm}}

	r.Options.Load = &JobLoad{Busy: map[string]bool{vm.Name: false}, IdleSince: map[string]time.Time{vm.Name: now.Add(-5 * time.Minute)}}
	if plan, _ := r.Plan(context.Background()); !plan.Empty() {
		t.Fatalf("idle 5m is under IdleTimeout, got %+v", plan)
	}
	r.Options.Load.IdleSince[vm.Name] = now.Add(-20 * time.Minute)
	plan, _ := r.Plan(context.Background())
	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != vm.Name || plan.Deletes[0].Reason != "scale down: idle for 20m" {
		t.Fatalf("want the idle VM removed, got %+v", plan.Deletes)
	}
}

func TestScale_FullHostSwapsIdleMacOSVMForLinux(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spec := elasticSpec()
	r := &Reconciler{Spec: spec, Options: Options{Now: func() time.Time { return now }}}
	m0 := runningVM(r, spec.Pools[1], "mbp", 0, now.Add(-time.Hour))
	m1 := runningVM(r, spec.Pools[1], "mbp", 1, now.Add(-time.Hour))
	client := &fakeOrchardClient{workers: []orchard.Worker{mac("mbp", 2)}, vms: []orchard.VM{m0, m1}}
	r.Client = client
	load := &JobLoad{
		Waiting:   map[string]int{"linux": 3},
		Busy:      map[string]bool{m0.Name: true, m1.Name: false},
		IdleSince: map[string]time.Time{m1.Name: now.Add(-3 * time.Minute)},
		LastSwap:  map[string]time.Time{},
	}
	r.Options.Load = load

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 0 || len(plan.Deletes) != 1 || plan.Deletes[0].Name != m1.Name || !plan.Deletes[0].Swap {
		t.Fatalf("want the idle macOS VM swapped out (busy one kept), got %+v", plan)
	}

	tracker := &LoadTracker{lastSwap: load.LastSwap}
	tracker.Applied(plan, now)
	if _, ok := load.LastSwap["mbp"]; !ok {
		t.Fatal("an applied swap must start the worker's cooldown")
	}

	// Once the macOS VM is gone the Linux one fits.
	client.vms = []orchard.VM{m0}
	plan, _ = r.Plan(context.Background())
	if len(plan.Creates) != 1 || plan.Creates[0].Pool != "linux" || plan.Creates[0].Worker != "mbp" {
		t.Fatalf("want a Linux VM on mbp, got %+v", plan)
	}
}

func TestScale_NoSwapWhenOtherPoolHasWaitingJobs(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spec := elasticSpec()
	r := &Reconciler{Spec: spec, Options: Options{Now: func() time.Time { return now }}}
	m0 := runningVM(r, spec.Pools[1], "mbp", 0, now.Add(-time.Hour))
	m1 := runningVM(r, spec.Pools[1], "mbp", 1, now.Add(-time.Hour))
	r.Client = &fakeOrchardClient{workers: []orchard.Worker{mac("mbp", 2)}, vms: []orchard.VM{m0, m1}}
	r.Options.Load = &JobLoad{
		Waiting:   map[string]int{"linux": 3, "macos": 1},
		Busy:      map[string]bool{m0.Name: false, m1.Name: false},
		IdleSince: map[string]time.Time{m0.Name: now.Add(-time.Hour), m1.Name: now.Add(-time.Hour)},
	}
	plan, _ := r.Plan(context.Background())
	if len(plan.Deletes) != 0 {
		t.Fatalf("a pool with its own waiting jobs gives up no VM, got %+v", plan.Deletes)
	}
}

func TestScale_MacOSLimitPerHost(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spec := elasticSpec()
	spec.Pools[1].MaxPerWorker = intp(3)
	r := &Reconciler{Spec: spec, Options: Options{Now: func() time.Time { return now }}}
	m0 := runningVM(r, spec.Pools[1], "mbp", 0, now.Add(-time.Hour))
	m1 := runningVM(r, spec.Pools[1], "mbp", 1, now.Add(-time.Hour))
	r.Client = &fakeOrchardClient{workers: []orchard.Worker{mac("mbp", 4)}, vms: []orchard.VM{m0, m1}}
	r.Options.Load = &JobLoad{Waiting: map[string]int{"macos": 4}, Busy: map[string]bool{m0.Name: true, m1.Name: true}}
	plan, _ := r.Plan(context.Background())
	if len(plan.Creates) != 0 {
		t.Fatalf("Apple allows 2 macOS VMs per Mac, got %+v", plan.Creates)
	}
}
