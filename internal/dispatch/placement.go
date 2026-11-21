package dispatch

import (
	"fmt"
	"sort"
	"strings"
)

// placePool picks the pool for a job that names none, from what the job asks for. A caller
// describes its job (kind, script, repository, toolchain, image, size); where it runs is the
// control plane's business.
//
// Only shared pools are chosen: a sealed pool is a decision about how much the job is trusted,
// which a job's description doesn't carry, so it stays something the caller asks for by name.
// Among the shared pools that can run the job, macOS ones come first: a macOS guest runs what a
// Linux one does once the job names its toolchain, and it is the only one that runs Apple work,
// which no field of the request tells apart. Linux pools follow, and are the only ones that can
// run a job whose environment is an image with no toolchain. Ties break by name, so the same
// fleet always gives the same answer.
func (s *service) placePool(req JobRequest) (string, error) {
	names := make([]string, 0, len(s.pools))
	for name := range s.pools {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		di, dj := s.pools[names[i]].OS == "darwin", s.pools[names[j]].OS == "darwin"
		if di != dj {
			return di
		}
		return names[i] < names[j]
	})
	var refused []string
	for _, name := range names {
		pc := s.pools[name]
		if pc.Isolated {
			continue
		}
		if why := pc.refuses(req); why != "" {
			refused = append(refused, name+": "+why)
			continue
		}
		return name, nil
	}
	if len(refused) == 0 {
		return "", fmt.Errorf("dispatch: no shared pool is configured to place this job in")
	}
	return "", fmt.Errorf("dispatch: no pool can run this job (%s)", strings.Join(refused, "; "))
}

// refuses is why pc, a shared pool, can't run req, or "" when it can. It is the cheap half of what
// Submit checks once the pool is known (validateResources, taskImage): enough to choose a pool
// that Submit then accepts.
func (pc PoolConfig) refuses(req JobRequest) string {
	if req.Resources != nil && !pc.fits(*req.Resources) {
		return "its jobs are smaller than the job asks for"
	}
	env := req.Environment
	if env == nil {
		return ""
	}
	if pc.OS == "darwin" {
		if env.Toolchain != nil && pc.acceptsToolchain() {
			return ""
		}
		if env.Toolchain == nil {
			return "macOS runs no container image, and the job names no toolchain"
		}
		return "toolchains are not enabled"
	}
	if env.Image == "" {
		// A toolchain alone: only a macOS pool installs one natively.
		return "the job names a toolchain and no image"
	}
	repo := env.Image
	if r, _, ok := strings.Cut(env.Image, "@"); ok {
		repo = r
	} else if r, ok := ParseTagRef(env.Image); ok {
		repo = r
	}
	if !repositoryAllowed(repo, pc.AllowedImages) {
		return "environments.allow does not permit the job's image"
	}
	return ""
}
