package dispatch

import (
	"context"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/sandbox"
)

// agentStopGrace is how long a canceled agent job gets to shut down cleanly before it is killed;
// other kinds get sandbox's default.
const agentStopGrace = 8 * time.Minute

// SandboxExecutor is the Executor `grove serve` uses: it runs each job in-process with an
// internal/sandbox Runner, in one of its pool's sealed VMs, driven from the control plane through
// Orchard's exec channel.
type SandboxExecutor struct {
	Orchard orchard.Client
	// Uploader receives build jobs' artifacts; nil skips collecting them.
	Uploader artifacts.Uploader
	// SlotDir holds the per-VM slot lock files (sandbox.Slots.Dir).
	SlotDir string
	// WorkDir is where checkouts are made before being pushed in; "" uses the system default.
	WorkDir string
	// Tune, when set, adjusts each Runner before it runs (tests shorten its intervals).
	Tune func(*sandbox.Runner)
}

// Execute implements Executor.
func (e *SandboxExecutor) Execute(ctx context.Context, run *Run) int {
	pool := run.Pool
	perVM := pool.Slots
	if pool.OS == sandbox.OSDarwin {
		perVM = 1
	}
	r := &sandbox.Runner{
		Orchard:  e.Orchard,
		Uploader: e.Uploader,
		Slots:    sandbox.Slots{Dir: e.SlotDir, PerVM: perVM},
		Stdout:   run.Stdout,
		Stderr:   run.Stderr,
		WorkDir:  e.WorkDir,
		OnStart:  func(vm string) { run.Started(placementOf(vm)) },
	}
	if run.Request.Kind == KindAgent {
		r.StopGrace = agentStopGrace
	}
	if e.Tune != nil {
		e.Tune(r)
	}
	image, err := RunnerImageOrDefault(pool.RunnerImage)
	if err != nil {
		image = "" // refused by the runner, with its reason in the job's log
	}
	return r.Run(ctx, sandbox.Request{
		Kind:           string(run.Request.Kind),
		Pool:           run.Request.Pool,
		OS:             pool.OS,
		AllocID:        run.ID,
		Image:          image,
		CPU:            pool.CPU,
		Memory:         pool.Memory,
		Repo:           run.Request.Repo,
		Ref:            run.Request.Ref,
		CloneToken:     run.CloneToken,
		Env:            run.Request.Env,
		Script:         run.Request.Script,
		Timeout:        run.Request.Timeout.Duration(),
		ArtifactPrefix: run.ArtifactPrefix,
	})
}

// Abandon implements Executor: it deletes the VM the job ran in. The job's container (or a macOS
// VM's one job) may still be running there with nobody supervising it; the VM's shutdown script
// lets it finish within its timeout, and the fleet reconciler recreates the VM from its image.
func (e *SandboxExecutor) Abandon(ctx context.Context, p Placement) error {
	if p.VMID == "" {
		return nil
	}
	return e.Orchard.DeleteVM(ctx, p.VMID)
}

// placementOf names a sealed VM's placement; its worker comes from the VM's name.
func placementOf(vm string) Placement {
	p := Placement{VMID: vm}
	if _, worker, _, ok := fleet.ParseVMName(vm); ok {
		p.WorkerID = worker
	}
	return p
}
