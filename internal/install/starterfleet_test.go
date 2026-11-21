package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/fleet"
)

// The starter fleet names what jobs can ask for, never where they run: it loads, selects no
// worker, and makes Linux VMs only on demand.
func TestStarterFleet_IsDynamic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := os.WriteFile(path, []byte(starterFleetYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := fleet.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range spec.Warnings() {
		if strings.Contains(w, "workerSelector") || strings.Contains(w, "preferWorkers") {
			t.Errorf("warning: %q", w)
		}
	}
	for _, p := range spec.Pools {
		if len(p.Labels) != 0 || len(p.WorkerSelector) != 0 || len(p.PreferWorkers) != 0 {
			t.Errorf("pool %s selects workers: labels=%v selector=%v", p.Name, p.Labels, p.WorkerSelector)
		}
		if p.Name == "linux" {
			if min, max := p.Bounds(); !p.Elastic() || min != 0 || max < 1 {
				t.Errorf("linux bounds %d..%d, want elastic from 0", min, max)
			}
		}
	}
}
