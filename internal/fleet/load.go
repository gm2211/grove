package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"unicode"

	"gopkg.in/yaml.v3"
)

// poolNameRE restricts pool names to lowercase letters/digits with no dashes or dots, so
// ParseVMName can unambiguously split a VM name "<pool>-<worker>-<n>" on the first "-" even
// though worker names (real hostnames) commonly contain both.
var poolNameRE = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

var hostXcodeBuildVersionRE = regexp.MustCompile(`^[A-Za-z0-9]+$`)

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

		if !poolNameRE.MatchString(p.Name) {
			return fmt.Errorf(
				"pool %q: name must match %s (lowercase letters/digits only, no dashes or dots) "+
					"so VM names \"<pool>-<worker>-<n>\" can be parsed back unambiguously",
				p.Name, poolNameRE.String(),
			)
		}

		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true

		if p.Image == "" {
			return fmt.Errorf("pool %q: image is required", p.Name)
		}

		if p.MaxPerWorker != nil {
			min, max := p.Bounds()
			if max < 1 || min < 0 || min > max {
				return fmt.Errorf("pool %q: need 0 <= minPerWorker <= maxPerWorker and maxPerWorker >= 1, got %d..%d", p.Name, min, max)
			}
			if min != max && p.Isolated() {
				return fmt.Errorf("pool %q: a sealed pool can't be elastic (its jobs' load isn't visible per VM); set perWorker instead", p.Name)
			}
		} else if p.MinPerWorker != nil {
			return fmt.Errorf("pool %q: minPerWorker needs maxPerWorker", p.Name)
		} else if p.PerWorker < 1 {
			return fmt.Errorf("pool %q: perWorker must be >= 1, got %d", p.Name, p.PerWorker)
		}

		if p.HostXcode != nil {
			if p.Name != "macos" {
				return fmt.Errorf("pool %q: hostXcode is only supported for the macos pool", p.Name)
			}
			if err := validateHostXcode(*p.HostXcode); err != nil {
				return fmt.Errorf("pool %q: %w", p.Name, err)
			}
		}

		if p.AllowDockerSocket {
			// The docker socket gave every job root on the guest, past the in-guest fence that
			// keeps jobs off home networks.
			return fmt.Errorf("pool %q: allowDockerSocket is no longer supported (it gave jobs root on the VM, past the network fence); remove it from fleet.yaml", p.Name)
		}
		switch p.Network {
		case "", NetworkShared, NetworkIsolated:
		default:
			return fmt.Errorf("pool %q: network must be %q or %q, got %q", p.Name, NetworkShared, NetworkIsolated, p.Network)
		}
	}

	for _, p := range s.Pools {
		switch {
		case p.Fallback == "" || p.Fallback == FallbackNone:
		case p.Fallback == p.Name:
			return fmt.Errorf("pool %q: fallback can't be the pool itself", p.Name)
		case !seen[p.Fallback]:
			return fmt.Errorf("pool %q: fallback pool %q doesn't exist", p.Name, p.Fallback)
		case s.FallbackFor(p) == "":
			return fmt.Errorf("pool %q: fallback needs two shared (not isolated) pools", p.Name)
		}
	}

	return nil
}

func validateHostXcode(config HostXcodeConfig) error {
	if config.Path == "" {
		return fmt.Errorf("hostXcode.path is required")
	}
	if !filepath.IsAbs(config.Path) || filepath.Clean(config.Path) != config.Path {
		return fmt.Errorf("hostXcode.path must be a clean absolute .app path")
	}
	if filepath.Ext(config.Path) != ".app" {
		return fmt.Errorf("hostXcode.path must end in .app")
	}
	// The worker shares whatever directory this names with every guest of the pool: keep it to an
	// application bundle, never a home directory or anything else that could hold host secrets.
	if filepath.Dir(config.Path) != "/Applications" || len(config.Path) <= len("/Applications/.app") {
		return fmt.Errorf("hostXcode.path must be an .app directly under /Applications")
	}
	for _, r := range config.Path {
		if r == ':' || r == ',' || unicode.IsControl(r) {
			return fmt.Errorf("hostXcode.path must not contain colons, commas, or control characters")
		}
	}
	if config.BuildVersion == "" || !hostXcodeBuildVersionRE.MatchString(config.BuildVersion) {
		return fmt.Errorf("hostXcode.buildVersion is required and must contain only ASCII letters and digits")
	}
	return nil
}
