package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// Label keys the Reconciler uses to identify and group the VMs it manages. LabelSpecHash lets it
// detect drift between fleet.yaml and what's actually running: when a pool's config changes, its
// hash changes, and every VM carrying the old hash is deleted and recreated.
const (
	LabelPool     = "pool"
	LabelHost     = "host"
	LabelSpecHash = "grove.spec"
)

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
		hash := specHash(pool)

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

				existing, ok := findVM(vms, name)
				if ok && existing.Labels[LabelSpecHash] == hash {
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
					Spec:   r.vmSpec(pool, worker.Name, name, hash),
				})
			}
		}
	}

	// Anything grove-managed that isn't desired anymore: worker went offline/paused, pool or
	// workerSelector no longer matches, perWorker shrank, or the pool was removed entirely.
	for _, vm := range vms {
		pool, managed := vm.Labels[LabelPool]
		if !managed {
			continue
		}

		if desired[vm.Name] {
			continue
		}

		plan.Deletes = append(plan.Deletes, PlannedDelete{
			Name:   vm.Name,
			Pool:   pool,
			Worker: vm.Labels[LabelHost],
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

func (r *Reconciler) vmSpec(pool Pool, worker, name, hash string) orchard.VMSpec {
	labels := make(map[string]string, len(pool.Labels)+3)
	for k, v := range pool.Labels {
		labels[k] = v
	}

	labels[LabelPool] = pool.Name
	labels[LabelHost] = worker
	labels[LabelSpecHash] = hash

	startup := BuildStartupScript(pool, worker, name, r.Options.TailscaleAuthKey)
	shutdown, shutdownTimeout := BuildShutdownScript(pool)

	return orchard.VMSpec{
		Name:            name,
		Image:           pool.Image,
		CPU:             pool.CPU,
		Memory:          pool.Memory,
		DiskSize:        pool.DiskSize,
		Labels:          labels,
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

// specHash fingerprints the parts of a Pool that determine what a VM created for it looks like,
// so that changing any of them (in fleet.yaml) causes existing VMs to be recreated. It
// deliberately excludes per-VM values (worker, VM name) — those come from vmSpec, not the pool.
type specFingerprint struct {
	Image           string
	CPU             uint64
	Memory          uint64
	DiskSize        uint64
	TTL             string
	Labels          map[string]string
	StartupScript   string
	ShutdownScript  string
	ShutdownTimeout string
	Username        string
	Password        string
}

func specHash(p Pool) string {
	fp := specFingerprint{
		Image:           p.Image,
		CPU:             p.CPU,
		Memory:          p.Memory,
		DiskSize:        p.DiskSize,
		TTL:             p.TTL.Std().String(),
		Labels:          p.Labels,
		StartupScript:   p.StartupScript,
		ShutdownScript:  p.ShutdownScript,
		ShutdownTimeout: p.ShutdownTimeout.Std().String(),
		Username:        p.Username,
		Password:        p.Password,
	}

	b, err := json.Marshal(fp)
	if err != nil {
		// json.Marshal on this struct (plain strings/uints/map[string]string) cannot fail.
		panic(fmt.Sprintf("fleet: marshal spec fingerprint: %v", err))
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])[:12]
}
