// Package provenance checks that a grove release archive was built by grove's own release
// workflow, from the build provenance attestation GitHub keeps for it.
//
// A release's checksums.txt sits in the release it describes, so whoever can write a release can
// write both. The attestation cannot be made that way: it is signed with a certificate Sigstore's
// CA issues only to a GitHub Actions run, naming the workflow file and the ref it ran from, and it
// is logged in Sigstore's transparency log. Verify checks all of that offline, against the
// Sigstore trusted root embedded here, and then that the run was grove's release workflow.
package provenance

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// trustedRootJSON is Sigstore's public-good trusted root (the Fulcio CAs, the Rekor and CT log
// keys, the timestamp authorities), from
// https://github.com/sigstore/root-signing/blob/main/targets/trusted_root.json. It holds every
// key Sigstore has used, with its validity window; refresh it when Sigstore adds one.
//
//go:embed trusted_root.json
var trustedRootJSON []byte

const (
	// Repository is the only repository whose release workflow grove trusts.
	Repository = "gm2211/grove"
	// Issuer is the OIDC issuer of GitHub Actions' workflow identity tokens.
	Issuer = "https://token.actions.githubusercontent.com"

	repositoryURI = "https://github.com/" + Repository
	workflowURI   = repositoryURI + "/.github/workflows/release.yml"
	mainRef       = "refs/heads/main"
	slsaPredicate = "https://slsa.dev/provenance/v1"
)

// AttestationsURL is where GitHub serves the attestations of the artifact with this SHA-256
// (lower-case hex). Public for a public repository: no token.
func AttestationsURL(sha256hex string) string {
	return "https://api.github.com/repos/" + Repository + "/attestations/sha256:" + sha256hex
}

// AncestryURL is GitHub's comparison of commit with main; OnMain reads its answer.
func AncestryURL(commit string) string {
	return "https://api.github.com/repos/" + Repository + "/compare/" + commit + "...main"
}

// Result says which run built a verified archive.
type Result struct {
	// Ref is the ref the release workflow ran from: refs/heads/main for a release autorelease.yml
	// cut, refs/tags/vX.Y.Z for a tag pushed by hand.
	Ref string
	// Commit is the commit that run was for.
	Commit string
}

// NeedsAncestryCheck reports whether Commit still has to be shown to be on main. A run from main
// used main's own workflow file on main's own tip. A run from a pushed tag used whatever
// release.yml the tagged commit holds, and a tag can be pushed on any commit: the caller must
// confirm the commit is on main (AncestryURL, OnMain) before trusting the archive.
func (r Result) NeedsAncestryCheck() bool { return r.Ref != mainRef }

var (
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// The certificate's subject is "<workflow file URI>@<ref>". Anchored at both ends.
	subjectRE = `^` + regexp.QuoteMeta(workflowURI) + `@refs/(heads/main|tags/v[0-9]+\.[0-9]+\.[0-9]+)$`
)

type attestationsResponse struct {
	Attestations []struct {
		Bundle json.RawMessage `json:"bundle"`
	} `json:"attestations"`
}

// Verify checks that one of the attestations in attestationsJSON (GitHub's answer at
// AttestationsURL) proves the artifact with SHA-256 sha256hex was built by grove's release
// workflow for release version ("vX.Y.Z" or "X.Y.Z"). It needs no network.
//
// It verifies, for a bundle: the signing certificate chains to Sigstore's CA and carries a signed
// certificate timestamp; the signature is in the transparency log, at a time the certificate was
// valid; the signed statement is SLSA provenance naming this digest; the certificate was issued to
// a GitHub Actions run of release.yml in gm2211/grove, on a GitHub-hosted runner, from main or
// from the tag of this version. Anything else is an error: an archive is never trusted on a
// bundle this cannot read.
func Verify(attestationsJSON []byte, sha256hex, version string) (Result, error) {
	if !sha256RE.MatchString(sha256hex) {
		return Result{}, fmt.Errorf("provenance: %q is not a SHA-256 digest", sha256hex)
	}
	digest, _ := hex.DecodeString(sha256hex)
	tag := "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")

	var resp attestationsResponse
	if err := json.Unmarshal(attestationsJSON, &resp); err != nil {
		return Result{}, fmt.Errorf("provenance: unreadable attestations: %w", err)
	}
	if len(resp.Attestations) == 0 {
		return Result{}, errors.New("provenance: GitHub holds no build attestation for this archive; grove's release workflow did not build it")
	}

	trusted, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return Result{}, fmt.Errorf("provenance: embedded Sigstore trusted root: %w", err)
	}
	verifier, err := verify.NewVerifier(trusted,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1))
	if err != nil {
		return Result{}, fmt.Errorf("provenance: %w", err)
	}
	identity, err := verify.NewShortCertificateIdentity(Issuer, "", "", subjectRE)
	if err != nil {
		return Result{}, fmt.Errorf("provenance: %w", err)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(identity))

	var errs []error
	for i, att := range resp.Attestations {
		res, err := verifyBundle(verifier, policy, att.Bundle, tag)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("attestation %d: %w", i+1, err))
	}
	return Result{}, fmt.Errorf("provenance: no attestation shows grove's release workflow built this archive: %w", errors.Join(errs...))
}

func verifyBundle(verifier *verify.Verifier, policy verify.PolicyBuilder, raw json.RawMessage, tag string) (Result, error) {
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(raw); err != nil {
		return Result{}, fmt.Errorf("unreadable bundle: %w", err)
	}
	res, err := verifier.Verify(b, policy)
	if err != nil {
		return Result{}, err
	}
	if res.Statement == nil || res.Statement.PredicateType != slsaPredicate {
		return Result{}, errors.New("not a SLSA build provenance statement")
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return Result{}, errors.New("no signing certificate")
	}
	cert := res.Signature.Certificate
	// The subject already matched subjectRE; these say the same thing through the certificate's
	// other fields, which a later Sigstore or GitHub change must not let drift apart unnoticed.
	switch {
	case cert.SourceRepositoryURI != repositoryURI:
		return Result{}, fmt.Errorf("built from %q, not %s", cert.SourceRepositoryURI, repositoryURI)
	case cert.RunnerEnvironment != "github-hosted":
		return Result{}, fmt.Errorf("built on a %q runner, not a GitHub-hosted one", cert.RunnerEnvironment)
	case !strings.HasPrefix(cert.BuildSignerURI, workflowURI+"@"):
		return Result{}, fmt.Errorf("signed by %q, not the release workflow", cert.BuildSignerURI)
	case !commitRE.MatchString(cert.SourceRepositoryDigest):
		return Result{}, fmt.Errorf("names no source commit (%q)", cert.SourceRepositoryDigest)
	}
	ref := cert.SourceRepositoryRef
	if ref != mainRef && ref != "refs/tags/"+tag {
		return Result{}, fmt.Errorf("built from %s, which is neither main nor the tag %s", ref, tag)
	}
	if !strings.HasSuffix(cert.SubjectAlternativeName, "@"+ref) {
		return Result{}, fmt.Errorf("workflow ran from %q but the source ref is %s", cert.SubjectAlternativeName, ref)
	}
	return Result{Ref: ref, Commit: cert.SourceRepositoryDigest}, nil
}

// OnMain reads GitHub's answer at AncestryURL(commit): whether commit is main's tip or one of
// its ancestors. The comparison is commit...main, so "ahead" means main has moved on from commit
// and "identical" that commit is the tip; "behind" and "diverged" mean commit is not on main.
func OnMain(compareJSON []byte) (bool, error) {
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(compareJSON, &resp); err != nil {
		return false, fmt.Errorf("provenance: unreadable comparison with main: %w", err)
	}
	switch resp.Status {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	}
	return false, fmt.Errorf("provenance: comparison with main has status %q", resp.Status)
}
