package install

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPinnedDownloads makes every pinned download's hash command report the expected digest,
// i.e. simulates untampered downloads for opts' platform.
func scriptPinnedDownloads(r *FakeRunner, opts Options) {
	for _, d := range []pinnedDownload{homebrewInstallerDownload()} {
		name, args := sha256Command(opts, filepath.Join(downloadDir(opts), d.File))
		r.Script(Call{Name: name, Args: args}.String(), FakeResult{Stdout: d.SHA256 + "  " + d.File + "\n"})
	}
}

func TestHomebrewInstallerPinIsComplete(t *testing.T) {
	if len(HomebrewInstallSHA256) != 64 || strings.ToLower(HomebrewInstallSHA256) != HomebrewInstallSHA256 || len(HomebrewInstallCommit) != 40 {
		t.Error("Homebrew installer pin must be a full commit and a lower-case sha256")
	}
}

func TestFetchVerifiedFailsClosedOnMismatch(t *testing.T) {
	opts := Options{Home: t.TempDir(), GOOS: "linux", GOARCH: "arm64"}
	d := homebrewInstallerDownload()
	staged := filepath.Join(downloadDir(opts), d.File)
	r := NewFakeRunner()
	r.Script("sha256sum "+staged, FakeResult{Stdout: strings.Repeat("0", 64) + "  " + staged + "\n"})

	_, err := fetchVerified(context.Background(), r, opts, d)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	if !r.CalledWith("rm -f " + staged) {
		t.Error("a download that failed verification must be deleted")
	}
	for _, c := range r.Calls {
		if c.Name == "install" || c.Name == "chmod" || c.Name == "unzip" {
			t.Fatalf("nothing may be installed after a mismatch, got %s", c.String())
		}
	}
}

// The control plane no longer installs a scheduler: its plan downloads no Nomad, and nothing
// unpinned.
func TestControlPlanePlan_LinuxDownloadsNoNomad(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	for _, c := range r.Calls {
		if c.Name == "curl" && strings.Contains(c.String(), "latest") {
			t.Errorf("no unpinned 'latest' download allowed: %s", c.String())
		}
		if strings.Contains(c.String(), "nomad") && !strings.Contains(c.String(), "grove-nomad.service") {
			t.Errorf("the control-plane plan still touches Nomad: %s", c.String())
		}
	}
}

func TestWorkerPlan_HomebrewInstallerPinnedAndVerified(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	d := homebrewInstallerDownload()
	script := filepath.Join(downloadDir(opts), d.File)
	if !r.CalledWith("curl -fsSL --proto =https --tlsv1.2 -o " + script + " " + d.URL) {
		t.Errorf("expected pinned Homebrew installer download, calls: %v", r.Calls)
	}
	if strings.Contains(d.URL, "/HEAD/") {
		t.Error("Homebrew installer must not be fetched from HEAD")
	}
	assertBefore(t, r, "shasum -a 256 "+script, "/usr/bin/env NONINTERACTIVE=1 /bin/bash "+script)

	// And a tampered installer never runs.
	r2 := NewFakeRunner()
	scriptTailscaleUp(r2)
	r2.Script("shasum -a 256 "+script, FakeResult{Stdout: strings.Repeat("a", 64) + "  x\n"})
	steps, _ = BuildPlan(r2, opts, io.Discard)
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected homebrew step to fail closed, got %v", err)
	}
	for _, c := range r2.Calls {
		if c.Name == "/usr/bin/env" {
			t.Fatalf("tampered Homebrew installer was executed: %s", c.String())
		}
	}
}

// assertBefore fails unless both command lines ran and first ran before second.
func assertBefore(t *testing.T, r *FakeRunner, first, second string) {
	t.Helper()
	fi, si := -1, -1
	for i, c := range r.Calls {
		if c.String() == first && fi == -1 {
			fi = i
		}
		if c.String() == second && si == -1 {
			si = i
		}
	}
	if fi == -1 || si == -1 || fi > si {
		t.Fatalf("want %q (at %d) before %q (at %d); calls: %v", first, fi, second, si, r.Calls)
	}
}
