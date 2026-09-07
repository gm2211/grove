package fleet

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// LabelWorkerPin mirrors github.com/cirruslabs/orchard/pkg/resource/v1.LabelWorkerName (the fork
// can't be imported from this package without pulling Orchard's own types into fleet's contract
// surface — see internal/orchard/client.go's vmToV1, which sets this same key). Every VM grove
// creates carries it, pinning the VM to the worker it was planned for; the Reconciler also reads
// it back as the fallback source of a VM's "host" while the VM is still pending and Orchard's own
// Worker field is empty (see PoolAndHost below).
const LabelWorkerPin = "org.cirruslabs.orchard.worker-name"

// Options tunes the Reconciler's behaviour. The zero value is usable.
type Options struct {
	// TailscaleAuthKey, when set, makes generated StartupScripts join the tailnet (see
	// scripts.go). Typically config.Config.Tailscale.AuthKey.
	TailscaleAuthKey string
	// Now returns the current time; defaults to time.Now. Overridable for tests.
	Now func() time.Time
	// Logger receives Run's per-tick errors without stopping the loop. Defaults to log.Default().
	Logger *log.Logger
}

func (o Options) logger() *log.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return log.Default()
}

// Reconciler makes Orchard match a fleet Spec: for every online, non-paused worker whose labels
// satisfy a pool's WorkerSelector, it ensures pool.PerWorker VMs exist, pinned to that worker;
// VMs that are no longer desired (worker gone offline/paused, selector no longer matches,
// perWorker shrank) or whose spec has drifted are deleted.
type Reconciler struct {
	Client  orchard.Client
	Spec    *Spec
	Options Options
}

// PlannedCreate is one VM the Reconciler intends to create.
type PlannedCreate struct {
	Pool   string
	Worker string
	Spec   orchard.VMSpec
}

// PlannedDelete is one VM the Reconciler intends to delete.
type PlannedDelete struct {
	Name   string
	Pool   string
	Worker string
	Reason string
}

// Plan is the result of comparing desired state (Spec) against observed state (Orchard).
type Plan struct {
	Creates []PlannedCreate
	Deletes []PlannedDelete
}

// Empty reports whether applying this Plan would be a no-op.
func (p Plan) Empty() bool {
	return len(p.Creates) == 0 && len(p.Deletes) == 0
}

// Plan lists workers and VMs and computes the creates/deletes needed to match r.Spec.
func (r *Reconciler) Plan(ctx context.Context) (Plan, error) {
	workers, err := r.Client.ListWorkers(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list workers: %w", err)
	}

	vms, err := r.Client.ListVMs(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list vms: %w", err)
	}

	var plan Plan

	desired := make(map[string]bool)

	for _, pool := range r.Spec.Pools {
		for _, worker := range workers {
			if worker.Offline || worker.SchedulingPaused {
				continue
			}

			if !labelsContain(worker.Labels, pool.WorkerSelector) {
				continue
			}

			for n := 0; n < pool.PerWorker; n++ {
				name := VMName(pool.Name, worker.Name, n)
				desired[name] = true

				spec := r.vmSpec(pool, worker.Name, name)

				existing, ok := findVM(vms, name)
				if ok && !specDrifted(spec, existing) {
					continue // already up to date
				}

				if ok {
					plan.Deletes = append(plan.Deletes, PlannedDelete{
						Name:   name,
						Pool:   pool.Name,
						Worker: worker.Name,
						Reason: "spec changed",
					})
				}

				plan.Creates = append(plan.Creates, PlannedCreate{
					Pool:   pool.Name,
					Worker: worker.Name,
					Spec:   spec,
				})
			}
		}
	}

	// Anything grove-managed that isn't desired anymore: worker went offline/paused, pool or
	// workerSelector no longer matches, perWorker shrank, or the pool was removed entirely.
	//
	// VM labels can't tell us that anymore (they're worker selectors now, see Pool.Labels), so
	// "grove-managed" is determined the same way pool/host are derived for display: the name
	// parses as "<pool>-<worker>-<n>" (ParseVMName) *and* the VM is pinned to that same worker
	// (LabelWorkerPin) — every VM grove creates satisfies both, so this only ever picks up VMs
	// grove itself made, never an unrelated resource that happens to share the naming shape.
	for _, vm := range vms {
		pool, worker, _, ok := ParseVMName(vm.Name)
		if !ok || vm.Labels[LabelWorkerPin] != worker {
			continue
		}

		if desired[vm.Name] {
			continue
		}

		plan.Deletes = append(plan.Deletes, PlannedDelete{
			Name:   vm.Name,
			Pool:   pool,
			Worker: hostOf(vm, worker),
			Reason: "no longer desired",
		})
	}

	sort.Slice(plan.Creates, func(i, j int) bool { return plan.Creates[i].Spec.Name < plan.Creates[j].Spec.Name })
	sort.Slice(plan.Deletes, func(i, j int) bool { return plan.Deletes[i].Name < plan.Deletes[j].Name })

	return plan, nil
}

// Apply executes a Plan: deletes first (so a spec-changed VM's old copy is gone before its
// replacement is created), then creates.
func (r *Reconciler) Apply(ctx context.Context, plan Plan) error {
	for _, d := range plan.Deletes {
		if err := r.Client.DeleteVM(ctx, d.Name); err != nil {
			return fmt.Errorf("delete vm %s: %w", d.Name, err)
		}
	}

	for _, c := range plan.Creates {
		if _, err := r.Client.CreateVM(ctx, c.Spec); err != nil {
			return fmt.Errorf("create vm %s: %w", c.Spec.Name, err)
		}
	}

	return nil
}

// Run plans and applies on every tick of interval (and once immediately) until ctx is done.
// A failed tick is logged rather than fatal, since it's usually a transient Orchard error and
// the next tick will retry.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	logger := r.Options.logger()

	tick := func() {
		plan, err := r.Plan(ctx)
		if err != nil {
			logger.Printf("fleet: plan failed: %v", err)
			return
		}

		if err := r.Apply(ctx, plan); err != nil {
			logger.Printf("fleet: apply failed: %v", err)
		}
	}

	tick()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick()
		}
	}
}

// vmSpec builds the orchard.VMSpec grove wants running for the n-th VM of pool on worker. Its
// Labels are pool.Labels verbatim (nothing derived is added — see Pool.Labels' doc comment) plus
// the worker pin vmToV1 applies from Worker; pool/host bookkeeping lives in the VM's *name* and
// in Worker, not in labels.
func (r *Reconciler) vmSpec(pool Pool, worker, name string) orchard.VMSpec {
	startup := BuildStartupScript(pool, worker, name, r.Options.TailscaleAuthKey)
	shutdown, shutdownTimeout := BuildShutdownScript(pool)

	return orchard.VMSpec{
		Name:            name,
		Worker:          worker,
		Image:           pool.Image,
		CPU:             pool.CPU,
		Memory:          pool.Memory,
		DiskSize:        pool.DiskSize,
		Labels:          pool.Labels,
		RestartPolicy:   "OnFailure",
		Headless:        true,
		Username:        pool.Username,
		Password:        pool.Password,
		StartupScript:   startup,
		ShutdownScript:  shutdown,
		ShutdownTimeout: shutdownTimeout,
		TTL:             pool.TTL.Std(),
	}
}

func findVM(vms []orchard.VM, name string) (orchard.VM, bool) {
	for _, vm := range vms {
		if vm.Name == name {
			return vm, true
		}
	}

	return orchard.VM{}, false
}

func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}

	return true
}

// specDrifted reports whether an already-existing VM no longer matches what fleet.yaml now
// wants, field by field. This replaces the old grove.spec label-hash comparison now that VM
// labels are worker selectors Orchard evaluates (see Pool.Labels) rather than free-form grove
// bookkeeping: there's nowhere left to stash a hash, so the desired orchard.VMSpec (built fresh
// from the current Pool) is compared directly against the observed orchard.VM.
func specDrifted(desired orchard.VMSpec, existing orchard.VM) bool {
	if desired.Image != existing.Image ||
		desired.DiskSize != existing.DiskSize ||
		desired.RestartPolicy != existing.RestartPolicy ||
		desired.StartupScript != existing.StartupScript ||
		desired.ShutdownScript != existing.ShutdownScript ||
		desired.ShutdownTimeout != existing.ShutdownTimeout ||
		desired.TTL != existing.TTL {
		return true
	}

	// CPU/Memory come from Orchard's *assigned* resources (v1.VM's AssignedCPU/AssignedMemory —
	// see vmFromV1), which the controller only populates once it has actually scheduled the VM
	// onto a worker. Comparing them while the VM is still "pending" would read them as zero and
	// treat every freshly created, not-yet-scheduled VM as instantly drifted — recreating it
	// forever instead of ever letting it reach a worker. Skip that comparison until Orchard has
	// something to report.
	if existing.Status == "pending" {
		return false
	}

	return desired.CPU != existing.CPU || desired.Memory != existing.Memory
}

// hostOf is a VM's best-known host: the controller-observed Worker once Orchard has actually
// scheduled it, falling back to the worker grove pinned/planned it for (fallback, typically
// parsed from the VM's own name) while it's still pending and Worker is empty.
func hostOf(vm orchard.VM, fallback string) string {
	if vm.Worker != "" {
		return vm.Worker
	}

	return fallback
}

// PoolAndHost derives a VM's pool and host the same way the Reconciler does internally, for
// callers that only observe VMs (internal/server's /fleet normalisation, `grove fleet status`)
// and never plan against them: pool comes from parsing the VM's name (see ParseVMName), and host
// prefers the controller-observed Worker, falling back first to the VM's own worker-pin label and
// then to the worker parsed out of its name, in case Worker is empty because Orchard hasn't
// scheduled it yet.
func PoolAndHost(vm orchard.VM) (pool, host string) {
	pool, worker, _, ok := ParseVMName(vm.Name)
	if !ok {
		worker = ""
	}

	if pin := vm.Labels[LabelWorkerPin]; pin != "" {
		worker = pin
	}

	return pool, hostOf(vm, worker)
}
