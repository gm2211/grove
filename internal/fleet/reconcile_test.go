package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

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

	// The created VM's labels must be ONLY what fleet.yaml declared (empty here) — no
	// pool/host/hash bookkeeping labels, since Orchard treats VM labels as worker selectors the
	// target worker must also carry (see Pool.Labels' doc comment). The worker pin itself is
	// applied downstream by orchard.vmToV1, not here.
	if got := plan.Creates[0].Spec.Labels; len(got) != 0 {
		t.Fatalf("want no labels on a pool with no declared Labels, got %+v", got)
	}

	if got := plan.Creates[0].Spec.Worker; got != "mac1" {
		t.Fatalf("want Spec.Worker = mac1, got %q", got)
	}
}

func TestPlan_MissingVM_Create_PoolLabelsPassedThrough(t *testing.T) {
	pool := basePool()
	pool.Labels = map[string]string{"arch": "arm64"}

	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Creates) != 1 || plan.Creates[0].Spec.Labels["arch"] != "arm64" {
		t.Fatalf("want pool.Labels passed through to the VMSpec, got %+v", plan.Creates)
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
		Worker: "ghost",
		Labels: map[string]string{LabelWorkerPin: "ghost"},
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

	if plan.Deletes[0].Pool != "linux" || plan.Deletes[0].Worker != "ghost" {
		t.Fatalf("want delete pool/worker derived from name (linux/ghost), got %+v", plan.Deletes[0])
	}
}

func TestPlan_ExtraVM_NameLikeButNotOurs_NotDeleted(t *testing.T) {
	// A VM whose name happens to parse as "<pool>-<worker>-<n>" but that grove didn't create
	// (no worker pin, or a pin that disagrees with the parsed worker) must never be swept up as
	// "no longer desired" — grove only ever touches resources it created.
	pool := basePool()

	unrelated := orchard.VM{Name: VMName("linux", "someone-else", 0)}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{unrelated},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, d := range plan.Deletes {
		if d.Name == unrelated.Name {
			t.Fatalf("want unrelated VM %q left alone, got it in deletes: %+v", unrelated.Name, plan.Deletes)
		}
	}
}

func TestPlan_SpecChanged_DeleteAndCreate(t *testing.T) {
	pool := basePool()
	name := VMName("linux", "mac1", 0)

	existing := orchard.VM{
		Name:   name,
		Worker: "mac1",
		Image:  "ghcr.io/example/OLD:latest", // differs from pool.Image below
		Labels: map[string]string{LabelWorkerPin: "mac1"},
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

func TestPlan_SpecChanged_DriftOnScriptContent(t *testing.T) {
	pool := basePool()
	pool.ShutdownScript = "echo custom"
	name := VMName("linux", "mac1", 0)

	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}}
	desired := r.vmSpec(pool, "mac1", name)

	existing := orchard.VM{
		Name:           name,
		Worker:         "mac1",
		Image:          desired.Image,
		CPU:            desired.CPU,
		Memory:         desired.Memory,
		RestartPolicy:  desired.RestartPolicy,
		StartupScript:  desired.StartupScript,
		ShutdownScript: "echo old", // stale
		Labels:         map[string]string{LabelWorkerPin: "mac1"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r.Client = client

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Deletes) != 1 || plan.Deletes[0].Reason != "spec changed" {
		t.Fatalf("want 1 delete (spec changed) for shutdown-script drift, got %+v", plan.Deletes)
	}
}

func TestPlan_UpToDate_Empty(t *testing.T) {
	pool := basePool()
	name := VMName("linux", "mac1", 0)

	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}}
	desired := r.vmSpec(pool, "mac1", name)

	existing := orchard.VM{
		Name:            name,
		Worker:          "mac1",
		Image:           desired.Image,
		CPU:             desired.CPU,
		Memory:          desired.Memory,
		DiskSize:        desired.DiskSize,
		RestartPolicy:   desired.RestartPolicy,
		StartupScript:   desired.StartupScript,
		ShutdownScript:  desired.ShutdownScript,
		ShutdownTimeout: desired.ShutdownTimeout,
		Labels:          map[string]string{LabelWorkerPin: "mac1"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r.Client = client

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan when the VM is already up to date, got %+v", plan)
	}
}

func TestPlan_TTLChangeAlone_IsDrift(t *testing.T) {
	// TTL is observable on orchard.VM (populated from the controller's ttl_seconds), so a
	// TTL-only change in fleet.yaml must, on its own, be detected as drift and recreate the VM.
	pool := basePool()
	pool.TTL = Duration(2 * time.Hour)
	name := VMName("linux", "mac1", 0)

	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}}
	desired := r.vmSpec(pool, "mac1", name)

	existing := orchard.VM{
		Name:            name,
		Worker:          "mac1",
		Image:           desired.Image,
		CPU:             desired.CPU,
		Memory:          desired.Memory,
		DiskSize:        desired.DiskSize,
		RestartPolicy:   desired.RestartPolicy,
		StartupScript:   desired.StartupScript,
		ShutdownScript:  desired.ShutdownScript,
		ShutdownTimeout: desired.ShutdownTimeout,
		TTL:             time.Hour, // stale — desired is 2h
		Labels:          map[string]string{LabelWorkerPin: "mac1"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r.Client = client

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Deletes) != 1 || plan.Deletes[0].Reason != "spec changed" {
		t.Fatalf("want 1 delete (spec changed) for TTL drift, got %+v", plan.Deletes)
	}
}

func TestPlan_UpToDate_TTLMatches_Empty(t *testing.T) {
	pool := basePool()
	pool.TTL = Duration(2 * time.Hour)
	name := VMName("linux", "mac1", 0)

	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}}
	desired := r.vmSpec(pool, "mac1", name)

	existing := orchard.VM{
		Name:            name,
		Worker:          "mac1",
		Image:           desired.Image,
		CPU:             desired.CPU,
		Memory:          desired.Memory,
		DiskSize:        desired.DiskSize,
		RestartPolicy:   desired.RestartPolicy,
		StartupScript:   desired.StartupScript,
		ShutdownScript:  desired.ShutdownScript,
		ShutdownTimeout: desired.ShutdownTimeout,
		TTL:             desired.TTL,
		Labels:          map[string]string{LabelWorkerPin: "mac1"},
	}

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existing},
	}
	r.Client = client

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !plan.Empty() {
		t.Fatalf("want empty plan when TTL (and everything else) matches, got %+v", plan)
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

func TestSpecDrifted_FieldByField(t *testing.T) {
	r := &Reconciler{}
	base := r.vmSpec(basePool(), "mac1", "linux-mac1-0")

	cases := []struct {
		name    string
		status  string
		mutate  func(orchard.VM) orchard.VM
		drifted bool
	}{
		{"identical", "running", func(vm orchard.VM) orchard.VM { return vm }, false},
		{"image changed", "running", func(vm orchard.VM) orchard.VM { vm.Image = "other:latest"; return vm }, true},
		{"cpu changed", "running", func(vm orchard.VM) orchard.VM { vm.CPU++; return vm }, true},
		{"memory changed", "running", func(vm orchard.VM) orchard.VM { vm.Memory++; return vm }, true},
		{"diskSize changed", "running", func(vm orchard.VM) orchard.VM { vm.DiskSize++; return vm }, true},
		{"restartPolicy changed", "running", func(vm orchard.VM) orchard.VM { vm.RestartPolicy = "Never"; return vm }, true},
		{"startupScript changed", "running", func(vm orchard.VM) orchard.VM { vm.StartupScript += "\n# extra"; return vm }, true},
		{"shutdownScript changed", "running", func(vm orchard.VM) orchard.VM { vm.ShutdownScript += "\n# extra"; return vm }, true},
		{"shutdownTimeout changed", "running", func(vm orchard.VM) orchard.VM { vm.ShutdownTimeout += time.Minute; return vm }, true},
		{"ttl changed", "running", func(vm orchard.VM) orchard.VM { vm.TTL += time.Hour; return vm }, true},
		{
			// The bug this guards against: a freshly created VM is "pending" until Orchard
			// actually schedules it, and its AssignedCPU/AssignedMemory (what CPU/Memory map
			// from — see vmFromV1) read zero the whole time it's pending. Comparing them
			// unconditionally would make every brand-new VM look drifted before it ever gets a
			// chance to schedule, and the reconciler would delete+recreate it forever.
			"pending vm with zero assigned cpu/memory: not drift",
			"pending",
			func(vm orchard.VM) orchard.VM { vm.CPU, vm.Memory = 0, 0; return vm },
			false,
		},
		{
			"pending vm with an actual image change: still drift",
			"pending",
			func(vm orchard.VM) orchard.VM { vm.CPU, vm.Memory = 0, 0; vm.Image = "other:latest"; return vm },
			true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			existing := orchard.VM{
				Status:          c.status,
				Image:           base.Image,
				CPU:             base.CPU,
				Memory:          base.Memory,
				DiskSize:        base.DiskSize,
				RestartPolicy:   base.RestartPolicy,
				StartupScript:   base.StartupScript,
				ShutdownScript:  base.ShutdownScript,
				ShutdownTimeout: base.ShutdownTimeout,
				TTL:             base.TTL,
			}
			existing = c.mutate(existing)

			if got := specDrifted(base, existing); got != c.drifted {
				t.Errorf("specDrifted() = %v, want %v", got, c.drifted)
			}
		})
	}
}

func TestParseVMName(t *testing.T) {
	cases := []struct {
		name       string
		wantPool   string
		wantWorker string
		wantN      int
		wantOK     bool
	}{
		{"linux-mac1-0", "linux", "mac1", 0, true},
		{"synth-g-macbook-m5.local-0", "synth", "g-macbook-m5.local", 0, true},
		{"macos-worker-01.example.com-12", "macos", "worker-01.example.com", 12, true},
		{"linux-mac1-3", "linux", "mac1", 3, true},
		{"noseparator", "", "", 0, false},
		{"linux-", "", "", 0, false},
		{"linux--0", "", "", 0, false}, // empty worker between the two dashes
		{"linux-mac1-", "", "", 0, false},
		{"linux-mac1-notanumber", "", "", 0, false},
		{"", "", "", 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, worker, n, ok := ParseVMName(c.name)
			if ok != c.wantOK {
				t.Fatalf("ParseVMName(%q) ok = %v, want %v", c.name, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if pool != c.wantPool || worker != c.wantWorker || n != c.wantN {
				t.Errorf("ParseVMName(%q) = (%q, %q, %d), want (%q, %q, %d)",
					c.name, pool, worker, n, c.wantPool, c.wantWorker, c.wantN)
			}
		})
	}
}

func TestParseVMName_RoundTripsWithVMName(t *testing.T) {
	name := VMName("linux", "g-macbook-m5.local", 7)

	pool, worker, n, ok := ParseVMName(name)
	if !ok {
		t.Fatalf("ParseVMName(%q) ok = false, want true", name)
	}
	if pool != "linux" || worker != "g-macbook-m5.local" || n != 7 {
		t.Errorf("ParseVMName(%q) = (%q, %q, %d), want (linux, g-macbook-m5.local, 7)", name, pool, worker, n)
	}
}

func TestPoolAndHost(t *testing.T) {
	cases := []struct {
		name     string
		vm       orchard.VM
		wantPool string
		wantHost string
	}{
		{
			name:     "scheduled: prefers observed Worker",
			vm:       orchard.VM{Name: "linux-mac1-0", Worker: "mac1"},
			wantPool: "linux",
			wantHost: "mac1",
		},
		{
			name: "pending: falls back to the worker-pin label",
			vm: orchard.VM{
				Name:   "linux-mac1-0",
				Worker: "",
				Labels: map[string]string{LabelWorkerPin: "mac1"},
			},
			wantPool: "linux",
			wantHost: "mac1",
		},
		{
			name:     "pending, no pin label: falls back to the name's worker component",
			vm:       orchard.VM{Name: "linux-mac1-0"},
			wantPool: "linux",
			wantHost: "mac1",
		},
		{
			name:     "unparseable name: pool empty, host still from Worker",
			vm:       orchard.VM{Name: "not-a-grove-name", Worker: "mac1"},
			wantPool: "",
			wantHost: "mac1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, host := PoolAndHost(c.vm)
			if pool != c.wantPool || host != c.wantHost {
				t.Errorf("PoolAndHost(%+v) = (%q, %q), want (%q, %q)", c.vm, pool, host, c.wantPool, c.wantHost)
			}
		})
	}
}
