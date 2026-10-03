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
	ds := []pinnedDownload{homebrewInstallerDownload()}
	for _, f := range []func(Options) (pinnedDownload, error){nomadDownload, minioDownload, minioClientDownload} {
		if d, err := f(opts); err == nil {
			ds = append(ds, d)
		}
	}
	for _, d := range ds {
		name, args := sha256Command(opts, filepath.Join(downloadDir(opts), d.File))
		r.Script(Call{Name: name, Args: args}.String(), FakeResult{Stdout: d.SHA256 + "  " + d.File + "\n"})
	}
}

func TestPinnedChecksumsCoverEverySupportedPlatform(t *testing.T) {
	for _, table := range []map[string]string{nomadZipSHA256, minioSHA256, minioClientSHA256} {
		for _, key := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"} {
			if sum := table[key]; len(sum) != 64 || strings.ToLower(sum) != sum {
				t.Errorf("%s: pinned sha256 %q is not 64 lower-case hex chars", key, sum)
			}
		}
	}
	if len(HomebrewInstallSHA256) != 64 || len(HomebrewInstallCommit) != 40 {
		t.Error("Homebrew installer pin must be a full commit and sha256")
	}
}

func TestPinnedNomadDownloadIsVersionedNotLatest(t *testing.T) {
	d, err := nomadDownload(Options{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://releases.hashicorp.com/nomad/" + NomadVersion + "/nomad_" + NomadVersion + "_linux_amd64.zip"
	if d.URL != want || strings.Contains(d.URL, "latest") {
		t.Fatalf("nomad URL = %q, want %q", d.URL, want)
	}
	if _, err := nomadDownload(Options{GOOS: "linux", GOARCH: "riscv64"}); err == nil {
		t.Fatal("expected an unpinned platform to be refused, not downloaded unverified")
	}
}

func TestFetchVerifiedFailsClosedOnMismatch(t *testing.T) {
	opts := Options{Home: t.TempDir(), GOOS: "linux", GOARCH: "arm64"}
	d, _ := minioDownload(opts)
	staged := filepath.Join(downloadDir(opts), d.File)
	r := NewFakeRunner()
	r.Script("sha256sum "+staged, FakeResult{Stdout: strings.Repeat("0", 64) + "  " + staged + "\n"})

	err := installVerifiedBinary(context.Background(), r, opts, d, filepath.Join(opts.Home, ".local", "bin", "minio"))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	if !r.CalledWith("rm -f " + staged) {
		t.Error("a download that failed verification must be deleted")
	}
	for _, c := range r.Calls {
		if c.Name == "install" || c.Name == "chmod" {
			t.Fatalf("nothing may be installed after a mismatch, got %s", c.String())
		}
	}
}

func TestControlPlanePlan_LinuxVerifiesDownloadsBeforeInstalling(t *testing.T) {
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
	dl := downloadDir(opts)
	bin := filepath.Join(home, ".local", "bin")
	nomadZip := filepath.Join(dl, "nomad_"+NomadVersion+"_linux_arm64.zip")
	minio := filepath.Join(dl, "minio."+MinIOVersion)
	assertBefore(t, r, "sha256sum "+nomadZip, "unzip -o "+nomadZip+" nomad -d "+filepath.Join(dl, "nomad-"+NomadVersion))
	assertBefore(t, r, "sha256sum "+minio, "install -m 0755 "+minio+" "+filepath.Join(bin, "minio"))
	for _, c := range r.Calls {
		if c.Name == "curl" && strings.Contains(c.String(), "latest") {
			t.Errorf("no unpinned 'latest' download allowed: %s", c.String())
		}
	}
}

func TestControlPlanePlan_LinuxNomadMismatchFailsStep(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")
	scriptPinnedDownloads(r, opts)
	nomadZip := filepath.Join(downloadDir(opts), "nomad_"+NomadVersion+"_linux_arm64.zip")
	r.Script("sha256sum "+nomadZip, FakeResult{Stdout: strings.Repeat("f", 64) + "  x\n"})

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "nomad-binary") || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected nomad-binary to fail on checksum mismatch, got %v", err)
	}
	for _, c := range r.Calls {
		if c.Name == "unzip" {
			t.Fatalf("tampered nomad zip must not be unpacked: %s", c.String())
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
