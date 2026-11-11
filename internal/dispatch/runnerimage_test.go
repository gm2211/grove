package dispatch

import (
	"strings"
	"testing"
)

// The default runner image is where a build's `source` task holds the private-repo clone token, so
// it names its content: grove's own repository, by digest, never a tag.
func TestDefaultRunnerImageIsPinnedByDigest(t *testing.T) {
	repo, digest, ok := strings.Cut(DefaultRunnerImage, "@")
	if !ok || !digestPattern.MatchString(digest) {
		t.Fatalf("DefaultRunnerImage %q is not <repository>@sha256:<64 hex>", DefaultRunnerImage)
	}
	if repo != "ghcr.io/gm2211/grove-runner" {
		t.Errorf("DefaultRunnerImage repository = %q (a tag before the digest would be ignored by docker and only mislead)", repo)
	}
	if err := checkRepository(repo); err != nil {
		t.Error(err)
	}
	got, err := RunnerImageOrDefault("")
	if err != nil || got != DefaultRunnerImage {
		t.Errorf("RunnerImageOrDefault(\"\") = %q, %v", got, err)
	}
}

// A pool may name its runner image by digest, and still by tag.
func TestRunnerImageOrDefaultTakesADigestOrATag(t *testing.T) {
	for _, ref := range []string{
		"ghcr.io/acme/runner@sha256:206f7ef0cada2c22a1cd916dc065fde1154b3ac12e89e7f2cf19c0cfc86d5be6",
		"ghcr.io/acme/runner:v1@sha256:206f7ef0cada2c22a1cd916dc065fde1154b3ac12e89e7f2cf19c0cfc86d5be6",
		"ghcr.io/acme/runner:v1",
		"registry.example:5000/runner",
	} {
		if got, err := RunnerImageOrDefault(ref); err != nil || got != ref {
			t.Errorf("RunnerImageOrDefault(%q) = %q, %v", ref, got, err)
		}
	}
}
