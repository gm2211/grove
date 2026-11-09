package dispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// maxImageRef caps an environment image reference, as RunnerImageOrDefault does.
const maxImageRef = 512

var (
	// registryHostPattern is a lowercase DNS name with an optional :port.
	registryHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	// repoComponentPattern is one lowercase OCI path component (distribution spec).
	repoComponentPattern = regexp.MustCompile(`^[a-z0-9]+([._-]+[a-z0-9]+)*$`)
	digestPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// checkRepository checks that repo is "<registry>/<path>" with an explicit registry host and a
// lowercase OCI path (no tag, no digest).
func checkRepository(repo string) error {
	if repo == "" {
		return fmt.Errorf("repository is empty")
	}
	if len(repo) > maxImageRef {
		return fmt.Errorf("reference exceeds %d bytes", maxImageRef)
	}
	host, path, ok := strings.Cut(repo, "/")
	if !ok || path == "" {
		return fmt.Errorf("%q has no registry host and repository path", repo)
	}
	if !registryHostPattern.MatchString(host) {
		return fmt.Errorf("registry host %q is not a lowercase host name", host)
	}
	// Docker would read a first segment with no dot, port or "localhost" as part of the path on
	// Docker Hub; require the registry to be spelled out so the reference means one thing.
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return fmt.Errorf("%q has no explicit registry host (e.g. ghcr.io/%s)", repo, path)
	}
	for _, comp := range strings.Split(path, "/") {
		if !repoComponentPattern.MatchString(comp) {
			return fmt.Errorf("repository path component %q must be lowercase letters, digits and . _ - (and carry no tag)", comp)
		}
	}
	return nil
}

// ValidateAllowEntry checks one fleet.yaml `environments.allow` entry: an exact repository
// ("ghcr.io/acme/builder") or a prefix ending in "/" ("ghcr.io/acme/"), each with an explicit
// registry host and no tag or digest.
func ValidateAllowEntry(entry string) error {
	repo := strings.TrimSuffix(entry, "/")
	if strings.ContainsAny(entry, "@") {
		return fmt.Errorf("allow entry %q must not carry a digest or tag", entry)
	}
	if err := checkRepository(repo); err != nil {
		return fmt.Errorf("allow entry %q: %w", entry, err)
	}
	return nil
}

// ValidateEnvironmentImage checks ref is "<registry>/<repository>@sha256:<64 hex>" (explicit
// registry, digest, no tag) and that allow permits its repository: an entry equal to the
// repository, or an entry ending in "/" that the repository starts with (so "ghcr.io/acme/"
// permits "ghcr.io/acme/builder" and not "ghcr.io/acme-evil/builder").
func ValidateEnvironmentImage(ref string, allow []string) error {
	if len(ref) > maxImageRef {
		return fmt.Errorf("environment image exceeds %d bytes", maxImageRef)
	}
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return fmt.Errorf("environment image %q must be pinned by digest (<registry>/<repository>@sha256:<64 hex>); a <registry>/<repository>:<tag> is accepted only when the control plane can resolve it (the pool allows the repository and a registry resolver is configured)", ref)
	}
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("environment image digest %q must be sha256: followed by 64 lowercase hex digits", digest)
	}
	if err := checkRepository(repo); err != nil {
		return fmt.Errorf("environment image: %w", err)
	}
	if repositoryAllowed(repo, allow) {
		return nil
	}
	return fmt.Errorf("environment image repository %q is not allowed by pool's environments.allow", repo)
}

// repositoryAllowed is whether an allow entry permits repo: equal to it, or an entry ending in "/"
// that repo starts with.
func repositoryAllowed(repo string, allow []string) bool {
	for _, entry := range allow {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(repo, entry) && len(repo) > len(entry) {
				return true
			}
		} else if repo == entry {
			return true
		}
	}
	return false
}

// ParseTagRef splits an environment image written "<registry>/<repository>:<tag>" into its
// repository (no tag). It is false for a digest reference, a reference with no tag, and anything
// else that isn't that shape; it does not check the repository (checkRepository does).
func ParseTagRef(ref string) (repo string, ok bool) {
	if len(ref) > maxImageRef || strings.Contains(ref, "@") {
		return "", false
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") || i == len(ref)-1 {
		return "", false
	}
	return ref[:i], true
}

// effectivePin is whether req's job must stay in its pool: it said so, or it has an environment
// with no native toolchain, since the pools it could fall back to (macOS) have no containers to run
// the image in. An environment with a toolchain can run natively on a macOS pool with toolchains
// enabled; fallbackTarget checks the target is one.
func effectivePin(req JobRequest) bool {
	return req.PinPool || (req.Environment != nil && req.Environment.Toolchain == nil)
}

// acceptsToolchain is whether pc is a shared macOS pool with toolchains enabled: the only kind of
// pool that runs an environment's native toolchain.
func (pc PoolConfig) acceptsToolchain() bool {
	return pc.Toolchains && pc.OS == "darwin" && !pc.Isolated
}

// fits is whether pc's per-job size covers hint. A pool with no known size (0) is not refused,
// as in validateResources.
func (pc PoolConfig) fits(hint ResourceHint) bool {
	if pc.CPU > 0 && hint.CPU > pc.CPU {
		return false
	}
	if pc.Memory > 0 && hint.Memory > pc.Memory {
		return false
	}
	return true
}
