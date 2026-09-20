package dispatch

import (
	"bytes"
	"context"
	"fmt"
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
	CPU               int
	Memory            int
	AllowDockerSocket bool
}

// templateData is what a nomad/jobs/<kind>.nomad.hcl template is rendered with — see
// nomad/jobs/README.md's "Rendering" section (the images/jobs agent's authoritative contract for
// these templates). CPU/Memory come from the pool's PoolConfig; a pool built without them (CPU==0
// && Memory==0) leaves every job at the templates' own hardcoded defaults (2000 MHz / 4096 MiB).
type templateData struct {
	Kind              Kind
	Pool              string
	CPU               int
	Memory            int
	AllowDockerSocket bool
	RunnerImage       string
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

	for _, kind := range allKinds {
		file := fmt.Sprintf("%s.nomad.hcl", kind)
		tmpl, err := template.New(file).ParseFS(nomadjobs.FS, file)
		if err != nil {
			return fmt.Errorf("nomadjobs: parse template for %s: %w", kind, err)
		}
		for _, pool := range normalizedPools {
			var buf bytes.Buffer
			data := templateData{Kind: kind, Pool: pool.Name, CPU: pool.CPU, Memory: pool.Memory, AllowDockerSocket: pool.AllowDockerSocket, RunnerImage: pool.RunnerImage}
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
