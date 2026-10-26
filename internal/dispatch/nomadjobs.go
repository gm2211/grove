package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gm2211/grove/internal/nomad"
	nomadjobs "github.com/gm2211/grove/nomad/jobs"
)

// PoolConfig is what EnsureJobs needs to know about one fleet.yaml pool to render its
// parameterized Nomad jobs.
type PoolConfig struct {
	Name        string
	RunnerImage string
	// CPU/Memory are this pool's per-JOB Nomad resource defaults (MHz / MiB) — see
	// fleet.Pool.JobCPU/JobMemory. These size every build/agent/shell job's `resources` block for
	// this pool; they are distinct from the pool's VM-level cpu/memory (fleet.Pool.CPU/Memory, the
	// Orchard VM's own sizing). Zero means "let the job template fall back to its own hardcoded
	// default (2000 MHz / 4096 MiB)" — callers that know the pool (internal/cli/serve.go) instead
	// pass fleet.Pool.JobCPUOrDefault()/JobMemoryOrDefault() so that fallback is rarely hit.
	CPU    int
	Memory int
	// Isolated marks a sealed pool (fleet.yaml `network: isolated`): its VMs can't reach home, so
	// every kind is registered from nomad/jobs/sandbox.nomad.hcl instead, which runs on the
	// control plane and drives the job in a sealed VM from outside (see internal/sandbox).
	Isolated bool
	// Slots is how many jobs one of a sealed pool's VMs runs at once (see sandbox.Slots), since
	// no Nomad client in the VM counts them. Values below 1 mean 1.
	Slots int
	// OS is a sealed pool's guest OS ("linux" or "darwin", see sandbox.Request.OS), which picks
	// how `grove sandbox run` drives its jobs. "" means linux.
	OS string
	// NodePool, when set, pins this pool's jobs to the Nomad node pool of that name, which
	// EnsureJobs creates if needed. Set it for a shared pool whose VMs enrol into their own node
	// pool (Nomad over mutual TLS, see fleet.Reconciler.Enrol). Sealed pools ignore it: their jobs
	// run on the control plane's own client.
	NodePool string
	// Fallback is the pool this pool's jobs move to when none of its nodes can place them (see
	// fleet.Spec.FallbackFor), or "" to keep them waiting here.
	Fallback string
}

// templateData is what a nomad/jobs/<kind>.nomad.hcl template is rendered with — see
// nomad/jobs/README.md's "Rendering" section (the images/jobs agent's authoritative contract for
// these templates). CPU/Memory come from the pool's PoolConfig; a pool built without them (CPU==0
// && Memory==0) leaves every job at the templates' own hardcoded defaults (2000 MHz / 4096 MiB).
type templateData struct {
	Kind        Kind
	Pool        string
	CPU         int
	Memory      int
	RunnerImage string
	// GroveBin and Slots are only used by sandbox.nomad.hcl: the grove binary the control plane's
	// Nomad client runs, and the pool's per-VM slot count.
	GroveBin string
	Slots    int
	// OS is the pool's guest OS for sandbox.nomad.hcl's --os; "" for a Linux pool.
	OS string
	// NodePool is the Nomad node pool the job is pinned to, "" for none.
	NodePool string
}

const DefaultRunnerImage = "ghcr.io/gm2211/grove-runner:latest"

// RunnerImageOrDefault validates the reference before it enters HCL and embedded shell text.
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

// allKinds lists every job kind grove registers a parameterized Nomad job for.
var allKinds = []Kind{KindBuild, KindAgent, KindShell}

// EnsureJobs registers the parameterized Nomad job grove-<kind>-<pool> for every kind × pool,
// rendering nomad/jobs/<kind>.nomad.hcl (a text/template, see that package) once per pool.
func EnsureJobs(ctx context.Context, nc nomad.Client, pools []PoolConfig) error {
	normalizedPools := make([]PoolConfig, len(pools))
	copy(normalizedPools, pools)
	for i := range normalizedPools {
		runnerImage, err := RunnerImageOrDefault(normalizedPools[i].RunnerImage)
		if err != nil {
			return fmt.Errorf("nomadjobs: pool %s runner image: %w", normalizedPools[i].Name, err)
		}
		normalizedPools[i].RunnerImage = runnerImage
	}

	groveBin := ""
	for _, pool := range normalizedPools {
		if pool.Isolated {
			bin, err := groveBinary()
			if err != nil {
				return fmt.Errorf("nomadjobs: pool %s is sealed, and its jobs run grove itself: %w", pool.Name, err)
			}
			groveBin = bin
			break
		}
	}

	for _, pool := range normalizedPools {
		if pool.NodePool == "" || pool.Isolated {
			continue
		}
		pools, ok := nomad.AsNodePools(nc)
		if !ok {
			return fmt.Errorf("nomadjobs: pool %s needs node pool %s, and this Nomad client can't create one", pool.Name, pool.NodePool)
		}
		if err := pools.EnsureNodePool(ctx, pool.NodePool, "grove fleet pool "+pool.Name); err != nil {
			return fmt.Errorf("nomadjobs: %w", err)
		}
	}

	for _, kind := range allKinds {
		for _, pool := range normalizedPools {
			file := fmt.Sprintf("%s.nomad.hcl", kind)
			if pool.Isolated {
				file = sandboxJobFile
			}
			tmpl, err := template.New(file).ParseFS(nomadjobs.FS, file)
			if err != nil {
				return fmt.Errorf("nomadjobs: parse template %s: %w", file, err)
			}
			var buf bytes.Buffer
			data := templateData{Kind: kind, Pool: pool.Name, CPU: pool.CPU, Memory: pool.Memory, RunnerImage: pool.RunnerImage, GroveBin: groveBin, Slots: max(pool.Slots, 1), OS: pool.OS}
			if !pool.Isolated {
				data.NodePool = pool.NodePool
			}
			if err := tmpl.ExecuteTemplate(&buf, file, data); err != nil {
				return fmt.Errorf("nomadjobs: render %s/%s: %w", kind, pool.Name, err)
			}
			if err := nc.RegisterJobFile(ctx, buf.String()); err != nil {
				return fmt.Errorf("nomadjobs: register grove-%s-%s: %w", kind, pool.Name, err)
			}
		}
	}
	return nil
}

// sandboxJobFile is the template every kind of a sealed pool is registered from.
const sandboxJobFile = "sandbox.nomad.hcl"

// groveBinary is the grove binary sealed pools' jobs run on the control plane. It is this very
// binary, since EnsureJobs runs inside `grove serve` there. Overridden in tests.
var groveBinary = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return stableGroveBinary(exe)
}

// stableGroveBinary prefers Homebrew's version-independent link (…/bin/grove) over the versioned
// Cellar path os.Executable reports, which `brew upgrade` removes from under the registered jobs.
// It also refuses a path the job file can't carry safely.
func stableGroveBinary(exe string) (string, error) {
	if prefix, _, ok := strings.Cut(exe, "/Cellar/grove/"); ok {
		link := filepath.Join(prefix, "bin", "grove")
		if _, err := os.Stat(link); err == nil {
			exe = link
		}
	}
	if !filepath.IsAbs(exe) || strings.ContainsAny(exe, "\"\\\n$%") {
		return "", fmt.Errorf("unusable grove binary path %q", exe)
	}
	return exe, nil
}
