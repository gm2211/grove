package fleet

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and validates a fleet.yaml spec from path.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return &spec, nil
}

// Validate checks Spec invariants: unique pool names, perWorker >= 1, image set.
func (s *Spec) Validate() error {
	seen := make(map[string]bool, len(s.Pools))

	for i, p := range s.Pools {
		if p.Name == "" {
			return fmt.Errorf("pools[%d]: name is required", i)
		}

		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true

		if p.Image == "" {
			return fmt.Errorf("pool %q: image is required", p.Name)
		}

		if p.PerWorker < 1 {
			return fmt.Errorf("pool %q: perWorker must be >= 1, got %d", p.Name, p.PerWorker)
		}
	}

	return nil
}
