package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/install.sh is the curl|sh entry point; it can't import Go constants, so this test keeps
// its pins in lock-step with download.go and exercises its verification helper.
func readBootstrapScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBootstrapScriptPinsMatchInstaller(t *testing.T) {
	s := readBootstrapScript(t)
	if strings.Contains(s, "Homebrew/install/HEAD") {
		t.Error("install.sh must not run Homebrew's installer from HEAD")
	}
	for _, want := range []string{
		"HOMEBREW_INSTALL_COMMIT=" + HomebrewInstallCommit,
		"HOMEBREW_INSTALL_SHA256=" + HomebrewInstallSHA256,
		`verify_sha256 "$brew_tmp/install.sh" "$HOMEBREW_INSTALL_SHA256"`,
		`"$base/checksums.txt"`, // .goreleaser.yaml checksum.name_template
		`verify_sha256 "$tmp/$asset" "$expected"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh is missing %q", want)
		}
	}
}

func TestBootstrapScriptVerifySHA256(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	// Load the script's functions without running main.
	lib := strings.Replace(readBootstrapScript(t), "\nmain \"$@\"\n", "\n", 1)
	dir := t.TempDir()
	file := filepath.Join(dir, "artifact")
	if err := os.WriteFile(file, []byte("grove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const good = "8c9f16acb8f4604d95b58fb49ad1c7a3555f7cd22266f08dbc10a1c96a67b3a3" // sha256("grove\n")
	run := func(want string) (string, error) {
		cmd := exec.Command(sh, "-c", lib+"\nverify_sha256 \"$1\" \"$2\" && echo verified", "sh", file, want)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(good); err != nil || !strings.Contains(out, "verified") {
		t.Fatalf("matching digest rejected: %v %s", err, out)
	}
	out, err := run(strings.Repeat("0", 64))
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("mismatch accepted: %v %s", err, out)
	}
	if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
		t.Error("a file that failed verification must be deleted")
	}
}

// install.sh checks a release archive's build provenance with the GitHub CLI when it can, never
// requires gh, and refuses an archive whose attestation does not verify.
func TestBootstrapScriptVerifyProvenance(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	// A PATH with only what the function needs, so the machine's own gh is never run.
	bin := t.TempDir()
	for _, tool := range []string{"awk", "grep", "basename"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	// A stand-in gh: signed in or not, with or without --source-ref, verifying or refusing, as
	// FAKE_GH says; it records the verify command it was given.
	const fakeGH = `#!/bin/sh
case "$1 $2 $3" in
"attestation verify --help")
	case "$FAKE_GH" in *old*) exit 1 ;; esac
	echo "      --signer-workflow string"
	case "$FAKE_GH" in *sourceref*) echo "      --source-ref string" ;; esac
	exit 0 ;;
esac
case "$1 $2" in
"auth token")
	case "$FAKE_GH" in *signedout*) exit 1 ;; esac
	echo ghp_not_a_real_token
	exit 0 ;;
"attestation verify")
	echo "$*" >> "$FAKE_GH_LOG"
	case "$FAKE_GH" in *refuse*) echo "no attestations found" >&2; exit 1 ;; esac
	echo "verified"
	exit 0 ;;
esac
exit 2
`
	ghBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghBin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := strings.Replace(readBootstrapScript(t), "\nmain \"$@\"\n", "\n", 1)
	archive := filepath.Join(t.TempDir(), "grove_linux_arm64.tar.gz")
	if err := os.WriteFile(archive, []byte("grove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const verify = "attestation verify ARCHIVE --repo gm2211/grove --signer-workflow gm2211/grove/.github/workflows/release.yml"

	for name, tc := range map[string]struct {
		gh       string // FAKE_GH; "none" for no gh on PATH
		version  string
		skip     bool
		wantOK   bool
		wantOut  string
		wantCall string // "" for no `gh attestation verify`
	}{
		"no gh":                       {gh: "none", version: "v0.1.43", wantOK: true, wantOut: "provenance not verified (no GitHub CLI"},
		"gh without attestation":      {gh: "old", version: "v0.1.43", wantOK: true, wantOut: "provenance not verified (no GitHub CLI"},
		"gh signed out":               {gh: "signedout", version: "v0.1.43", wantOK: true, wantOut: "provenance not verified (gh is not signed in"},
		"release before attestations": {gh: "refuse", version: "v0.1.42", wantOK: true, wantOut: "predates grove's build attestations"},
		"older release":               {gh: "refuse", version: "v0.0.99", wantOK: true, wantOut: "predates grove's build attestations"},
		"verified":                    {gh: "ok", version: "v0.1.43", wantOK: true, wantOut: "build provenance verified", wantCall: verify},
		"verified, minor bump":        {gh: "ok", version: "v0.2.0", wantOK: true, wantOut: "build provenance verified", wantCall: verify},
		"verified against the tag": {gh: "ok sourceref", version: "v1.0.0", wantOK: true, wantOut: "build provenance verified",
			wantCall: verify + " --source-ref refs/tags/v1.0.0"},
		"refused": {gh: "refuse", version: "v0.1.43", wantOut: "could not be verified", wantCall: verify},
		"refused against the tag": {gh: "refuse sourceref", version: "v0.1.43", wantOut: "could not be verified for v0.1.43",
			wantCall: verify + " --source-ref refs/tags/v0.1.43"},
		"refused, skip asked": {gh: "refuse", version: "v0.1.43", skip: true, wantOK: true, wantOut: "provenance not verified (GROVE_SKIP_PROVENANCE=1)"},
	} {
		t.Run(name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "gh.log")
			path := bin
			if tc.gh != "none" {
				path = ghBin + string(os.PathListSeparator) + bin
			}
			cmd := exec.Command(sh, "-c", lib+"\nverify_provenance \"$1\" \"$2\" && echo installed", "sh", archive, tc.version)
			cmd.Env = []string{"PATH=" + path, "FAKE_GH=" + tc.gh, "FAKE_GH_LOG=" + log}
			if tc.skip {
				cmd.Env = append(cmd.Env, "GROVE_SKIP_PROVENANCE=1")
			}
			out, err := cmd.CombinedOutput()
			if ok := err == nil && strings.Contains(string(out), "installed"); ok != tc.wantOK {
				t.Fatalf("went ahead = %v, want %v: %v\n%s", ok, tc.wantOK, err, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			if strings.Contains(string(out), "ghp_not_a_real_token") {
				t.Errorf("gh's token reached the output:\n%s", out)
			}
			calls, _ := os.ReadFile(log)
			want := strings.Replace(tc.wantCall, "ARCHIVE", archive, 1)
			if got := strings.TrimSpace(string(calls)); got != want {
				t.Errorf("gh attestation verify call = %q, want %q", got, want)
			}
		})
	}
}

// Everything install.sh does happens inside a function called from its last line, so a download
// cut short cannot run half of it: outside functions there are only comments, `set -eu` and
// constants.
func TestBootstrapScriptRunsOnlyFromMain(t *testing.T) {
	s := readBootstrapScript(t)
	if !strings.HasSuffix(s, "\nmain \"$@\"\n") {
		t.Fatal("install.sh must end with the call to main")
	}
	inFunc := false
	for i, line := range strings.Split(strings.TrimSuffix(s, "main \"$@\"\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inFunc:
			inFunc = line != "}"
			continue
		case strings.HasSuffix(trimmed, "() {"):
			inFunc = true
			continue
		case strings.Contains(trimmed, "() {") && strings.HasSuffix(trimmed, "}"):
			continue // a one-line function
		case trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "set -eu":
			continue
		}
		if name, _, ok := strings.Cut(trimmed, "="); ok && name == strings.ToUpper(name) && !strings.ContainsAny(name, " $(") {
			continue // a constant
		}
		t.Errorf("install.sh line %d runs outside a function: %s", i+1, line)
	}
}
