package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/gm2211/grove/internal/orchard"
)

// fakeOrchardClient is a minimal in-memory orchard.Client for table-driven Plan/Apply tests.
type fakeOrchardClient struct {
	workers []orchard.Worker
	vms     []orchard.VM

	created []orchard.VMSpec
	deleted []string
}

func (f *fakeOrchardClient) ListWorkers(context.Context) ([]orchard.Worker, error) {
	return f.workers, nil
}

func (f *fakeOrchardClient) PauseWorker(context.Context, string) error  { return nil }
func (f *fakeOrchardClient) ResumeWorker(context.Context, string) error { return nil }

func (f *fakeOrchardClient) ListVMs(context.Context) ([]orchard.VM, error) {
	return f.vms, nil
}

func (f *fakeOrchardClient) GetVM(_ context.Context, name string) (*orchard.VM, error) {
	for _, vm := range f.vms {
		if vm.Name == name {
			out := vm
			return &out, nil
		}
	}

	return nil, errors.New("vm not found")
}

func (f *fakeOrchardClient) CreateVM(_ context.Context, spec orchard.VMSpec) (*orchard.VM, error) {
	f.created = append(f.created, spec)

	return &orchard.VM{Name: spec.Name, Labels: spec.Labels}, nil
}

func (f *fakeOrchardClient) DeleteVM(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)

	return nil
}

func (f *fakeOrchardClient) Exec(
	context.Context,
	string,
	[]string,
	orchard.ExecOptions,
) (orchard.ExecSession, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeOrchardClient) Ping(context.Context) error { return nil }

func basePool() Pool {
	return Pool{
		Name:      "linux",
		Image:     "ghcr.io/example/linux:latest",
		PerWorker: 1,
		CPU:       4,
		Memory:    8192,
	}
}

func onlineWorker(name string) orchard.Worker {
	return orchard.Worker{
		Name:   name,
		Labels: map[string]string{},
	}
}

func TestPlan_MissingVM_Create(t *testing.T) {
	pool := basePool()
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	wantName := VMName("linux", "mac1", 0)

	if len(plan.Creates) != 1 || plan.Creates[0].Spec.Name != wantName {
		t.Fatalf("want 1 create of %q, got %+v", wantName, plan.Creates)
	}

	if len(plan.Deletes) != 0 {
		t.Fatalf("want 0 deletes, got %+v", plan.Deletes)
	}
}

func TestPlan_PausedWorker_Skipped(t *testing.T) {
	pool := basePool()
	worker := onlineWorker("mac1")
	worker.SchedulingPaused = true

	client := &fakeOrchardClient{workers: []orchard.Worker{worker}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan for a paused worker, got %+v", plan)
	}
}

func TestPlan_OfflineWorker_Skipped(t *testing.T) {
	pool := basePool()
	worker := onlineWorker("mac1")
	worker.Offline = true

	client := &fakeOrchardClient{workers: []orchard.Worker{worker}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan for an offline worker, got %+v", plan)
	}
}

func TestPlan_WorkerSelector_NoMatch_Skipped(t *testing.T) {
	pool := basePool()
	pool.WorkerSelector = map[string]string{"arch": "arm64"}

	worker := onlineWorker("mac1")
	worker.Labels = map[string]string{"arch": "amd64"}

	client := &fakeOrchardClient{workers: []orchard.Worker{worker}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan when workerSelector doesn't match, got %+v", plan)
	}
}

func TestPlan_ExtraVM_Delete(t *testing.T) {
	pool := basePool()

	extraVM := orchard.VM{
		Name:   VMName("linux", "ghost", 0),
		Labels: map[string]string{LabelPool: "linux", LabelHost: "ghost"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")}, // "ghost" isn't a known worker anymore
		vms:     []orchard.VM{extraVM},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	wantCreate := VMName("linux", "mac1", 0)
	if len(plan.Creates) != 1 || plan.Creates[0].Spec.Name != wantCreate {
		t.Fatalf("want 1 create of %q, got %+v", wantCreate, plan.Creates)
	}

	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != extraVM.Name || plan.Deletes[0].Reason != "no longer desired" {
		t.Fatalf("want delete of %q (no longer desired), got %+v", extraVM.Name, plan.Deletes)
	}
}

func TestPlan_SpecChanged_DeleteAndCreate(t *testing.T) {
	pool := basePool()
	name := VMName("linux", "mac1", 0)

	existing := orchard.VM{
		Name:   name,
		Labels: map[string]string{LabelPool: "linux", LabelHost: "mac1", LabelSpecHash: "stalehash0000"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != name || plan.Deletes[0].Reason != "spec changed" {
		t.Fatalf("want 1 delete of %q (spec changed), got %+v", name, plan.Deletes)
	}

	if len(plan.Creates) != 1 || plan.Creates[0].Spec.Name != name {
		t.Fatalf("want 1 create of %q, got %+v", name, plan.Creates)
	}
}

func TestPlan_UpToDate_Empty(t *testing.T) {
	pool := basePool()
	name := VMName("linux", "mac1", 0)

	existing := orchard.VM{
		Name:   name,
		Labels: map[string]string{LabelPool: "linux", LabelHost: "mac1", LabelSpecHash: specHash(pool)},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan when the VM is already up to date, got %+v", plan)
	}
}

func TestPlan_PerWorkerMultiple(t *testing.T) {
	pool := basePool()
	pool.PerWorker = 3

	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Creates) != 3 {
		t.Fatalf("want 3 creates, got %d: %+v", len(plan.Creates), plan.Creates)
	}
}

func TestApply_DeletesThenCreates(t *testing.T) {
	client := &fakeOrchardClient{}
	r := &Reconciler{Client: client}

	plan := Plan{
		Deletes: []PlannedDelete{{Name: "old-vm"}},
		Creates: []PlannedCreate{{Spec: orchard.VMSpec{Name: "new-vm"}}},
	}

	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(client.deleted) != 1 || client.deleted[0] != "old-vm" {
		t.Fatalf("want delete of old-vm, got %+v", client.deleted)
	}

	if len(client.created) != 1 || client.created[0].Name != "new-vm" {
		t.Fatalf("want create of new-vm, got %+v", client.created)
	}
}

func TestSpecHash_ChangesWithPoolFields(t *testing.T) {
	a := basePool()
	b := basePool()
	b.Memory = a.Memory * 2

	if specHash(a) == specHash(b) {
		t.Fatal("want different hashes for pools with different memory")
	}

	if specHash(a) != specHash(basePool()) {
		t.Fatal("want identical hashes for identical pools")
	}
}
