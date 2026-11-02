package fleet

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// simOrchard is an Orchard that keeps what Apply creates: a created VM is listed, running, with
// exactly the spec it was created from, and a deleted one is gone.
type simOrchard struct {
	fakeOrchardClient
	uids int
}

func (s *simOrchard) CreateVM(_ context.Context, spec orchard.VMSpec) (*orchard.VM, error) {
	for _, vm := range s.vms {
		if vm.Name == spec.Name {
			return nil, fmt.Errorf("vm %s exists", spec.Name)
		}
	}
	s.uids++
	labels := map[string]string{LabelWorkerPin: spec.Worker} // orchard's vmToV1 pins every VM
	for k, v := range spec.Labels {
		labels[k] = v
	}
	vm := orchard.VM{
		Name: spec.Name, UID: fmt.Sprintf("uid-%d", s.uids), Worker: spec.Worker, Status: "running",
		Image: spec.Image, CPU: spec.CPU, Memory: spec.Memory, DiskSize: spec.DiskSize,
		RestartPolicy: spec.RestartPolicy, StartupScript: spec.StartupScript, ShutdownScript: spec.ShutdownScript,
		ShutdownTimeout: spec.ShutdownTimeout, TTL: spec.TTL, HostDirs: slices.Clone(spec.HostDirs),
		Softnet: spec.Softnet, SoftnetBlock: slices.Clone(spec.SoftnetBlock), Labels: labels,
		CreatedAt: time.Now(),
	}
	s.vms = append(s.vms, vm)
	return &vm, nil
}

func (s *simOrchard) DeleteVM(_ context.Context, name string) error {
	s.vms = slices.DeleteFunc(s.vms, func(vm orchard.VM) bool { return vm.Name == name })
	return nil
}

// randomFleet is a fleet.yaml and a set of workers: pools with selectors over two label values,
// workers carrying random labels, some paused, some offline, some on an Orchard too old for macOS
// guest password rotation.
func randomFleet(rng *rand.Rand) (*Spec, []orchard.Worker) {
	tiers := []string{"a", "b"}
	var spec Spec
	for i := range 1 + rng.Intn(3) {
		p := Pool{
			Name:      fmt.Sprintf("p%d", i),
			Image:     []string{"ghcr.io/example/linux:latest", "ghcr.io/cirruslabs/macos-sequoia-base:latest"}[rng.Intn(2)],
			PerWorker: rng.Intn(3),
			CPU:       4, Memory: 8192,
		}
		if rng.Intn(2) == 0 {
			p.WorkerSelector = map[string]string{"tier": tiers[rng.Intn(2)]}
		}
		if rng.Intn(3) == 0 {
			p.TTL = Duration(12 * time.Hour)
		}
		spec.Pools = append(spec.Pools, p)
	}
	var workers []orchard.Worker
	for i := range 1 + rng.Intn(4) {
		w := onlineWorker(fmt.Sprintf("mac%d", i))
		if rng.Intn(4) == 0 {
			w = legacyWorker(w.Name)
		}
		w.Labels = map[string]string{"tier": tiers[rng.Intn(2)]}
		w.SchedulingPaused = rng.Intn(4) == 0
		w.Offline = rng.Intn(5) == 0
		workers = append(workers, w)
	}
	return &spec, workers
}

// L6 (spec/README.md): from any starting fleet, one Plan + Apply reaches a state whose next Plan
// is empty, and stays there: the reconciler converges and never flaps. Every VM it leaves runs on
// an online, unpaused worker its pool's selector matches.
func TestL6_ReconcilerConvergesWithoutFlapping(t *testing.T) {
	ctx := context.Background()
	for seed := int64(0); seed < 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		spec, workers := randomFleet(rng)
		sim := &simOrchard{fakeOrchardClient: fakeOrchardClient{workers: workers}}
		r := &Reconciler{Client: sim, Spec: spec}

		// Start from some other fleet's leftovers: VMs grove made under a random older spec.
		oldSpec, _ := randomFleet(rng)
		old := &Reconciler{Client: sim, Spec: oldSpec}
		if plan, err := old.Plan(ctx); err == nil {
			_ = old.Apply(ctx, plan)
		}

		plan, err := r.Plan(ctx)
		if err != nil {
			t.Fatalf("seed %d: plan: %v", seed, err)
		}
		if err := r.Apply(ctx, plan); err != nil {
			t.Fatalf("seed %d: apply: %v", seed, err)
		}
		for round := range 3 {
			again, err := r.Plan(ctx)
			if err != nil {
				t.Fatalf("seed %d: plan %d: %v", seed, round+2, err)
			}
			if !again.Empty() {
				t.Fatalf("seed %d: plan %d after converging is not empty: creates %v, deletes %v",
					seed, round+2, again.Creates, again.Deletes)
			}
		}

		byName := map[string]orchard.Worker{}
		for _, w := range workers {
			byName[w.Name] = w
		}
		for _, vm := range sim.vms {
			pool, worker, _, ok := ParseVMName(vm.Name)
			if !ok {
				t.Fatalf("seed %d: VM %s has no fleet name", seed, vm.Name)
			}
			w := byName[worker]
			var p *Pool
			for i := range spec.Pools {
				if spec.Pools[i].Name == pool {
					p = &spec.Pools[i]
				}
			}
			switch {
			case p == nil:
				t.Fatalf("seed %d: VM %s belongs to no pool", seed, vm.Name)
			case w.Offline || w.SchedulingPaused:
				t.Fatalf("seed %d: VM %s left on offline or paused worker %s", seed, vm.Name, worker)
			case !labelsContain(w.Labels, p.WorkerSelector):
				t.Fatalf("seed %d: VM %s on worker %s outside pool %s's selector", seed, vm.Name, worker, pool)
			case guestPasswordBlock(*p, w) != "":
				t.Fatalf("seed %d: macOS VM %s on worker %s without guest password rotation", seed, vm.Name, worker)
			}
		}
	}
}
