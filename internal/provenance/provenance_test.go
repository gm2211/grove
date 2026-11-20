package provenance

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The attestation GitHub holds for grove_linux_amd64.tar.gz of v0.1.49, as its API returned it
// (the short-lived bundle_url left out), and that archive's SHA-256 from the release's
// checksums.txt. autorelease.yml cut that release, so the workflow ran from main.
const (
	fixture       = "testdata/grove_linux_amd64-v0.1.49.attestations.json"
	fixtureDigest = "4a64e3905707a9398a3863e80086e2b7cc12b0fe2c1d925bc091fa5ccdb5e8af"
	fixtureCommit = "2472467cd85f0174d0cce5e1a418dfab49e64d36"
)

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVerify_AcceptsARealRelease(t *testing.T) {
	for _, version := range []string{"v0.1.49", "0.1.49"} {
		res, err := Verify(readFixture(t), fixtureDigest, version)
		if err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if res.Ref != "refs/heads/main" || res.Commit != fixtureCommit || res.NeedsAncestryCheck() {
			t.Fatalf("%s: result = %+v", version, res)
		}
	}
}

func TestVerify_RefusesAnotherArchive(t *testing.T) {
	// A genuine attestation, presented for an archive it does not name.
	other := strings.Repeat("ab", 32)
	if _, err := Verify(readFixture(t), other, "v0.1.49"); err == nil {
		t.Fatal("an attestation for another digest was accepted")
	}
}

func TestVerify_RefusesNoAttestation(t *testing.T) {
	for _, body := range []string{`{"attestations":[]}`, `{}`} {
		_, err := Verify([]byte(body), fixtureDigest, "v0.1.49")
		if err == nil || !strings.Contains(err.Error(), "no build attestation") {
			t.Fatalf("%s: err = %v", body, err)
		}
	}
	for _, body := range []string{``, `not json`, `{"attestations":[{"bundle":{}}]}`, `{"attestations":[{"bundle":"x"}]}`} {
		if _, err := Verify([]byte(body), fixtureDigest, "v0.1.49"); err == nil {
			t.Fatalf("%q was accepted", body)
		}
	}
	if _, err := Verify(readFixture(t), "nothex", "v0.1.49"); err == nil {
		t.Fatal("a malformed digest was accepted")
	}
}

// mutate returns the fixture with one edit applied to its first bundle.
func mutate(t *testing.T, edit func(bundle map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readFixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc["attestations"].([]any)[0].(map[string]any)["bundle"].(map[string]any))
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerify_RefusesATamperedBundle(t *testing.T) {
	flip := func(s string) string {
		b := []byte(s)
		i := len(b) / 2
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}
	cases := map[string]func(map[string]any){
		"statement changed": func(b map[string]any) {
			env := b["dsseEnvelope"].(map[string]any)
			env["payload"] = flip(env["payload"].(string))
		},
		"signature changed": func(b map[string]any) {
			sig := b["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
			sig["sig"] = flip(sig["sig"].(string))
		},
		"certificate changed": func(b map[string]any) {
			cert := b["verificationMaterial"].(map[string]any)["certificate"].(map[string]any)
			cert["rawBytes"] = flip(cert["rawBytes"].(string))
		},
		"no transparency log entry": func(b map[string]any) {
			b["verificationMaterial"].(map[string]any)["tlogEntries"] = []any{}
		},
	}
	for name, edit := range cases {
		data := mutate(t, edit)
		if bytes.Equal(data, readFixture(t)) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := Verify(data, fixtureDigest, "v0.1.49"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSubjectPattern(t *testing.T) {
	re := regexpMust(t)
	for subject, want := range map[string]bool{
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/heads/main":       true,
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/tags/v0.2.0":      true,
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/heads/evil":       false,
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/heads/main-evil":  false,
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/tags/v0.2.0-evil": false,
		"https://github.com/gm2211/grove/.github/workflows/ci.yml@refs/heads/main":            false,
		"https://github.com/gm2211/grove-evil/.github/workflows/release.yml@refs/heads/main":  false,
		"https://github.com/evil/grove/.github/workflows/release.yml@refs/heads/main":         false,
		"https://github.com/gm2211/grove/.github/workflows/release.yml@refs/pull/1/merge":     false,
		"https://github.comXgm2211/grove/.github/workflows/releaseXyml@refs/heads/main":       false,
	} {
		if got := re(subject); got != want {
			t.Errorf("%s: matched = %t, want %t", subject, got, want)
		}
	}
}

func TestOnMain(t *testing.T) {
	for status, want := range map[string]bool{"ahead": true, "identical": true, "behind": false, "diverged": false} {
		got, err := OnMain([]byte(`{"status":"` + status + `"}`))
		if err != nil || got != want {
			t.Errorf("%s: %t, %v", status, got, err)
		}
	}
	for _, body := range []string{``, `{}`, `{"status":"new"}`, `{"message":"Not Found"}`} {
		if ok, err := OnMain([]byte(body)); err == nil || ok {
			t.Errorf("%q: %t, %v", body, ok, err)
		}
	}
}

func regexpMust(t *testing.T) func(string) bool {
	t.Helper()
	re, err := regexp.Compile(subjectRE)
	if err != nil {
		t.Fatal(err)
	}
	return re.MatchString
}
