package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"github.com/gm2211/grove/internal/nomad"
	nomadjobs "github.com/gm2211/grove/nomad/jobs"
)

// templateData is what a nomad/jobs/<kind>.nomad.hcl template is rendered with — see
// nomad/jobs/README.md's "Rendering" section (the images/jobs agent's authoritative contract for
// these templates). CPU/Memory are left at zero here: EnsureJobs only knows pool names, not their
// resource sizing, so every job falls back to the templates' own defaults (2000 MHz / 4096 MiB).
type templateData struct {
	Kind   Kind
	Pool   string
	CPU    int
	Memory int
}

// allKinds lists every job kind grove registers a parameterized Nomad job for.
var allKinds = []Kind{KindBuild, KindAgent, KindShell}

// EnsureJobs registers the parameterized Nomad job grove-<kind>-<pool> for every kind × pool,
// rendering nomad/jobs/<kind>.nomad.hcl (a text/template, see that package) once per pool.
func EnsureJobs(ctx context.Context, nc nomad.Client, pools []string) error {
	for _, kind := range allKinds {
		file := fmt.Sprintf("%s.nomad.hcl", kind)
		tmpl, err := template.New(file).ParseFS(nomadjobs.FS, file)
		if err != nil {
			return fmt.Errorf("nomadjobs: parse template for %s: %w", kind, err)
		}
		for _, pool := range pools {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, file, templateData{Kind: kind, Pool: pool}); err != nil {
				return fmt.Errorf("nomadjobs: render %s/%s: %w", kind, pool, err)
			}
			if err := nc.RegisterJobFile(ctx, buf.String()); err != nil {
				return fmt.Errorf("nomadjobs: register grove-%s-%s: %w", kind, pool, err)
			}
		}
	}
	return nil
}
