// Package ci turns grove into a CI system for GitHub repositories: `grove serve` watches the
// repositories named under config.yaml's ci: key, runs each new commit's .grove.yml jobs in the
// fleet, and reports every job's result back on the commit as a GitHub commit status (the check
// or cross a pull request shows).
//
// The watcher only ever calls out to GitHub; GitHub never calls grove. There is no webhook, so
// the control plane needs no public address, at the cost of noticing a push up to one poll
// interval late.
package ci

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gm2211/grove/internal/dispatch"
)

// SpecPath is where a repository keeps its grove CI jobs, at the root of the repository.
const SpecPath = ".grove.yml"

// maxSpecBytes bounds a .grove.yml the watcher will parse.
const maxSpecBytes = 256 << 10

// maxJobs bounds how many jobs one commit may start.
const maxJobs = 32

// Spec is a parsed .grove.yml.
//
//	jobs:
//	  test:
//	    pool: linux
//	    timeout: 30m
//	    env: { CI: "1" }
//	    script: |
//	      make test
type Spec struct {
	Jobs map[string]JobSpec `yaml:"jobs"`
}

// JobSpec is one job of a Spec. Each job runs as a grove build job of the commit, and reports as
// its own commit status, named "grove/<job name>".
type JobSpec struct {
	Pool    string            `yaml:"pool"`
	Script  string            `yaml:"script"`
	Timeout string            `yaml:"timeout,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

// jobNamePattern is what a job name must look like: it becomes part of a commit status context
// and of an idempotency key.
var jobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ParseSpec parses and validates a .grove.yml. Unknown keys are an error, so a typo fails loudly
// on the commit instead of silently running less than the author meant.
func ParseSpec(data []byte) (*Spec, error) {
	if len(data) > maxSpecBytes {
		return nil, fmt.Errorf("%s is larger than %d KiB", SpecPath, maxSpecBytes>>10)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var spec Spec
	if err := dec.Decode(&spec); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty", SpecPath)
		}
		return nil, fmt.Errorf("parse %s: %w", SpecPath, err)
	}
	if len(spec.Jobs) == 0 {
		return nil, fmt.Errorf("%s names no jobs", SpecPath)
	}
	if len(spec.Jobs) > maxJobs {
		return nil, fmt.Errorf("%s names %d jobs; at most %d are allowed", SpecPath, len(spec.Jobs), maxJobs)
	}
	for name, job := range spec.Jobs {
		if !jobNamePattern.MatchString(name) {
			return nil, fmt.Errorf("job name %q: use letters, digits, '.', '_' and '-' only", name)
		}
		if strings.TrimSpace(job.Pool) == "" {
			return nil, fmt.Errorf("job %q: pool is required", name)
		}
		if strings.TrimSpace(job.Script) == "" {
			return nil, fmt.Errorf("job %q: script is required", name)
		}
		if job.Timeout != "" {
			d, err := time.ParseDuration(job.Timeout)
			if err != nil || d < time.Second {
				return nil, fmt.Errorf("job %q: timeout %q is not a duration of at least 1s (like \"30m\")", name, job.Timeout)
			}
		}
	}
	return &spec, nil
}

// JobNames returns the spec's job names, sorted, so jobs are always started in the same order.
func (s *Spec) JobNames() []string {
	names := make([]string, 0, len(s.Jobs))
	for name := range s.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Request builds the grove job for one of the spec's jobs at commit sha of repoURL.
func (s *Spec) Request(name, repoURL, sha string, meta map[string]string) dispatch.JobRequest {
	job := s.Jobs[name]
	req := dispatch.JobRequest{
		Kind:      dispatch.KindBuild,
		Pool:      strings.TrimSpace(job.Pool),
		Repo:      repoURL,
		Ref:       sha,
		Script:    job.Script,
		Env:       job.Env,
		Meta:      meta,
		Requester: Requester,
		// One job per (repository, commit, job name): a restarted watcher that sees the same
		// commit again gets the job it already started back, rather than starting it twice.
		IdempotencyKey: "grove-ci:" + repoURL + "@" + sha + ":" + name,
	}
	if job.Timeout != "" {
		if d, err := time.ParseDuration(job.Timeout); err == nil {
			req.Timeout = dispatch.Duration(d)
		}
	}
	return req
}

// Requester labels every job the CI watcher starts.
const Requester = "grove-ci"
