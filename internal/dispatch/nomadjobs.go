package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"github.com/gm2211/grove/internal/nomad"
	nomadjobs "github.com/gm2211/grove/nomad/jobs"
)

// templateData is what {{.Kind}} / {{.Pool}} resolve to when rendering a job template.
type templateData struct {
	Kind Kind
	Pool string
}

// allKinds lists every job kind grove registers a parameterized Nomad job for.
var allKinds = []Kind{KindBuild, KindAgent, KindShell}

// EnsureJobs registers the parameterized Nomad job grove-<kind>-<pool> for every kind × pool,
// rendering nomad/jobs/<kind>.nomad.hcl (a text/template, see that package) once per pool.
func EnsureJobs(ctx context.Context, nc nomad.Client, pools []string) error {
	for _, kind := range allKinds {
		raw, err := nomadjobs.FS.ReadFile(fmt.Sprintf("%s.nomad.hcl", kind))
		if err != nil {
			return fmt.Errorf("nomadjobs: read template for %s: %w", kind, err)
		}
		tmpl, err := template.New(string(kind)).Parse(string(raw))
		if err != nil {
			return fmt.Errorf("nomadjobs: parse template for %s: %w", kind, err)
		}
		for _, pool := range pools {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, templateData{Kind: kind, Pool: pool}); err != nil {
				return fmt.Errorf("nomadjobs: render %s/%s: %w", kind, pool, err)
			}
			if err := nc.RegisterJobFile(ctx, buf.String()); err != nil {
				return fmt.Errorf("nomadjobs: register grove-%s-%s: %w", kind, pool, err)
			}
		}
	}
	return nil
}
