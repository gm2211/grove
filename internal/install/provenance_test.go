package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/provenance"
)

// provenanceFromMain stands in for provenance.Verify where a test has no real release: the
// archive was built by the release workflow, from main.
func provenanceFromMain(_ []byte, _, _ string) (provenance.Result, error) {
	return provenance.Result{Ref: "refs/heads/main", Commit: strings.Repeat("1", 40)}, nil
}

func provenanceFromTag(commit string) func([]byte, string, string) (provenance.Result, error) {
	return func(_ []byte, _, version string) (provenance.Result, error) {
		return provenance.Result{Ref: "refs/tags/v" + strings.TrimPrefix(version, "v"), Commit: commit}, nil
	}
}

// With nothing stubbed, self-update runs the real verifier: a release whose archive matches its
// own checksums.txt but has no attestation GitHub can show (one made by hand, by anyone who can
// write a release) is refused, and the binary stays as it was.
func TestSelfUpdate_RefusesAReleaseWithNoAttestation(t *testing.T) {
	r, opts := newUpdateRunner(t)
	opts.provenanceVerify = nil
	err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "attestation") || r.replaced {
		t.Fatalf("err = %v, replaced = %t", err, r.replaced)
	}
	if got, _ := os.ReadFile(r.exe); string(got) != "old" {
		t.Fatalf("binary touched: %q", got)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, provenance.AttestationsURL(strings.Repeat("a", 64))) {
		t.Errorf("the attestation was never asked for:\n%s", joined)
	}
	if strings.Contains(joined, "tar ") {
		t.Errorf("an unverified archive was unpacked:\n%s", joined)
	}
}

func TestSelfUpdate_RefusesWhenTheVerifierRefuses(t *testing.T) {
	r, opts := newUpdateRunner(t)
	opts.provenanceVerify = func([]byte, string, string) (provenance.Result, error) {
		return provenance.Result{}, errors.New("signed by someone else")
	}
	err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "signed by someone else") || r.replaced {
		t.Fatalf("err = %v, replaced = %t", err, r.replaced)
	}
}

// The verifier is asked about the digest checksums.txt gave (which the download hashed to) and
// the version being installed, never the running one.
func TestSelfUpdate_VerifiesTheDigestAndVersionItInstalls(t *testing.T) {
	r, opts := newUpdateRunner(t)
	var gotDigest, gotVersion string
	opts.provenanceVerify = func(_ []byte, digest, version string) (provenance.Result, error) {
		gotDigest, gotVersion = digest, version
		return provenanceFromMain(nil, digest, version)
	}
	if err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotDigest != strings.Repeat("a", 64) || gotVersion != "0.1.44" {
		t.Fatalf("verified digest %q for version %q", gotDigest, gotVersion)
	}
}

// A release built from a hand-pushed tag ran the tagged commit's own release.yml, so the commit
// has to be on main.
func TestVerifyReleaseProvenance_TagReleaseMustBeOnMain(t *testing.T) {
	commit := strings.Repeat("2", 40)
	digest := strings.Repeat("a", 64)
	api := "curl -fsSL --proto =https --tlsv1.2 -H Accept: application/vnd.github+json "
	for name, tc := range map[string]struct {
		compare FakeResult
		wantErr string
	}{
		"on main":          {FakeResult{Stdout: `{"status":"ahead"}`}, ""},
		"main's tip":       {FakeResult{Stdout: `{"status":"identical"}`}, ""},
		"on a side branch": {FakeResult{Stdout: `{"status":"diverged"}`}, "not on main"},
		"ahead of main":    {FakeResult{Stdout: `{"status":"behind"}`}, "not on main"},
		"no answer":        {FakeResult{Err: errors.New("curl: (22) 404")}, "is on main"},
		"unreadable":       {FakeResult{Stdout: `<html>`}, "unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewFakeRunner()
			r.Script(api+provenance.AttestationsURL(digest), FakeResult{Stdout: `{"attestations":[{}]}`})
			r.Script(api+provenance.AncestryURL(commit), tc.compare)
			opts := Options{GroveVersion: "0.2.0", provenanceVerify: provenanceFromTag(commit)}
			err := verifyReleaseProvenance(context.Background(), r, opts, digest)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A release from main needs no second question.
func TestVerifyReleaseProvenance_MainReleaseAsksNothingMore(t *testing.T) {
	r := NewFakeRunner()
	opts := Options{GroveVersion: "0.1.50", provenanceVerify: provenanceFromMain}
	if err := verifyReleaseProvenance(context.Background(), r, opts, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 1 || !strings.Contains(r.Calls[0].String(), "/attestations/sha256:") {
		t.Fatalf("calls = %v", r.Calls)
	}
}

// The whole path with nothing stubbed: GitHub's real answer for grove_linux_amd64.tar.gz of
// v0.1.49 goes through the Runner into the real verifier.
func TestVerifyReleaseProvenance_RealAttestation(t *testing.T) {
	const digest = "4a64e3905707a9398a3863e80086e2b7cc12b0fe2c1d925bc091fa5ccdb5e8af"
	body, err := os.ReadFile(filepath.Join("..", "provenance", "testdata", "grove_linux_amd64-v0.1.49.attestations.json"))
	if err != nil {
		t.Fatal(err)
	}
	api := "curl -fsSL --proto =https --tlsv1.2 -H Accept: application/vnd.github+json "
	r := NewFakeRunner()
	r.Script(api+provenance.AttestationsURL(digest), FakeResult{Stdout: string(body)})
	if err := verifyReleaseProvenance(context.Background(), r, Options{GroveVersion: "0.1.49"}, digest); err != nil {
		t.Fatal(err)
	}
	// The same genuine attestation does not vouch for another archive.
	other := strings.Repeat("c", 64)
	r.Script(api+provenance.AttestationsURL(other), FakeResult{Stdout: string(body)})
	if err := verifyReleaseProvenance(context.Background(), r, Options{GroveVersion: "0.1.49"}, other); err == nil {
		t.Fatal("an attestation for another archive was accepted")
	}
}

// The prebuilt Orchard is refused the same way, and the install falls back to building it from
// the pinned commit.
func TestEnsureOrchardBinary_UnattestedAssetFallsBackToSource(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, orchardTestVersion, true)
	r := NewFakeRunner()
	want := strings.Repeat("b", 64)
	scriptOrchardRelease(r, opts, want, want)
	_ = ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard)
	if calledWithPrefix(r, "tar -xzf") {
		t.Errorf("an unattested Orchard archive was unpacked: %v", r.Calls)
	}
	if !builtFromSource(r) {
		t.Errorf("no fallback to building from source: %v", r.Calls)
	}
}
