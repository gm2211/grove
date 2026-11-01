package install

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

const orchardTestVersion = "0.1.21"

func orchardDownloadOpts(t *testing.T, version string, haveGo bool) (Options, string) {
	t.Helper()
	home := t.TempDir()
	opts := Options{Home: home, GOOS: "darwin", GOARCH: "arm64", GroveVersion: version, LookPath: alwaysFailLookPath}
	if haveGo {
		opts.LookPath = lookPathOnly("go")
	}
	return opts, filepath.Join(home, "bin", "orchard")
}

// scriptOrchardRelease scripts the release's checksums.txt and the digest the staged asset hashes to.
func scriptOrchardRelease(r *FakeRunner, opts Options, listed, hashed string) pinnedDownload {
	asset := orchardAssetName(opts)
	sums := strings.Repeat("a", 64) + "  grove_darwin_arm64.tar.gz\n" + listed + "  " + asset + "\n"
	r.Script("curl -fsSL --proto =https --tlsv1.2 "+groveReleaseURL(opts, "checksums.txt"), FakeResult{Stdout: sums})
	d := pinnedDownload{File: "grove-" + opts.GroveVersion + "-" + asset}
	name, args := sha256Command(opts, filepath.Join(downloadDir(opts), d.File))
	r.Script(Call{Name: name, Args: args}.String(), FakeResult{Stdout: hashed + "  " + d.File + "\n"})
	return d
}

func calledWithPrefix(r *FakeRunner, prefix string) bool {
	for _, c := range r.Calls {
		if strings.HasPrefix(c.String(), prefix) {
			return true
		}
	}
	return false
}

func builtFromSource(r *FakeRunner) bool {
	for _, c := range r.Calls {
		if c.Name == "/bin/sh" && strings.Contains(strings.Join(c.Args, " "), "go build") {
			return true
		}
	}
	return false
}

func TestOrchardAssetNameAndURL(t *testing.T) {
	opts := Options{GOOS: "linux", GOARCH: "arm64", GroveVersion: "v" + orchardTestVersion}
	if got := orchardAssetName(opts); got != "orchard_linux_arm64.tar.gz" {
		t.Fatalf("asset = %q", got)
	}
	want := "https://github.com/gm2211/grove/releases/download/v" + orchardTestVersion + "/orchard_linux_arm64.tar.gz"
	if got := groveReleaseURL(opts, orchardAssetName(opts)); got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestEnsureOrchardBinary_DownloadsVerifiedReleaseAsset(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, orchardTestVersion, false)
	r := NewFakeRunner()
	want := strings.Repeat("b", 64)
	d := scriptOrchardRelease(r, opts, want, want)

	if err := ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard); err != nil {
		t.Fatalf("ensureOrchardBinary: %v", err)
	}
	staged := filepath.Join(downloadDir(opts), d.File)
	if !r.CalledWith("curl -fsSL --proto =https --tlsv1.2 -o " + staged + " " + groveReleaseURL(opts, orchardAssetName(opts))) {
		t.Errorf("the asset was not downloaded from grove's own release: %v", r.Calls)
	}
	if !calledWithPrefix(r, "tar -xzf "+staged+" -C ") || !calledWithPrefix(r, "mv -f ") {
		t.Errorf("the verified asset was not extracted into place: %v", r.Calls)
	}
	if builtFromSource(r) {
		t.Error("a successful download must not build from source")
	}
}

func TestEnsureOrchardBinary_ChecksumMismatchInstallsNothing(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, orchardTestVersion, false)
	r := NewFakeRunner()
	d := scriptOrchardRelease(r, opts, strings.Repeat("b", 64), strings.Repeat("c", 64))

	err := ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "install Go") {
		t.Fatalf("err = %v, want a checksum mismatch that says what to do", err)
	}
	if !r.CalledWith("rm -f " + filepath.Join(downloadDir(opts), d.File)) {
		t.Error("the unverified download must be deleted")
	}
	if calledWithPrefix(r, "tar ") || calledWithPrefix(r, "mv ") || builtFromSource(r) {
		t.Errorf("nothing may be installed or built after a mismatch without Go: %v", r.Calls)
	}
}

func TestEnsureOrchardBinary_FallsBackToSourceWhenAssetMissing(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, orchardTestVersion, true)
	r := NewFakeRunner()
	r.Script("curl -fsSL --proto =https --tlsv1.2 "+groveReleaseURL(opts, "checksums.txt"),
		FakeResult{Err: errors.New("curl: (22) The requested URL returned error: 404")})

	if err := ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard); err != nil {
		t.Fatalf("ensureOrchardBinary: %v", err)
	}
	if !builtFromSource(r) {
		t.Error("with Go present and no asset, Orchard must be built from source")
	}
}

func TestEnsureOrchardBinary_FallsBackToSourceOnBadDownloadWithGo(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, orchardTestVersion, true)
	r := NewFakeRunner()
	scriptOrchardRelease(r, opts, strings.Repeat("b", 64), strings.Repeat("c", 64))

	if err := ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard); err != nil {
		t.Fatalf("ensureOrchardBinary: %v", err)
	}
	if !builtFromSource(r) || calledWithPrefix(r, "mv -f "+filepath.Dir(dest)) {
		t.Errorf("a bad download must never be installed; source build expected: %v", r.Calls)
	}
}

func TestEnsureOrchardBinary_DevBuildSkipsDownload(t *testing.T) {
	for _, v := range []string{"", "dev", "0.1.22-next"} {
		opts, dest := orchardDownloadOpts(t, v, true)
		r := NewFakeRunner()
		if err := ensureOrchardBinary(context.Background(), r, opts, dest, io.Discard); err != nil {
			t.Fatalf("version %q: %v", v, err)
		}
		if calledWithPrefix(r, "curl ") || !builtFromSource(r) {
			t.Errorf("version %q must go straight to the source build: %v", v, r.Calls)
		}
	}
}

func TestEnsureOrchardBinary_DevBuildWithoutGoExplainsWhatToDo(t *testing.T) {
	opts, dest := orchardDownloadOpts(t, "dev", false)
	r := NewFakeRunner()
	// The source build fails (no go on PATH) under a runner that reports it.
	failing := &failBuildRunner{FakeRunner: r}

	err := ensureOrchardBinary(context.Background(), failing, opts, dest, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "install a released grove or install Go") {
		t.Fatalf("err = %v, want guidance for a dev build without Go", err)
	}
}

type failBuildRunner struct{ *FakeRunner }

func (f *failBuildRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	if name == "/bin/sh" {
		f.Calls = append(f.Calls, Call{Name: name, Args: args})
		return "", "", errors.New("go: command not found")
	}
	return f.FakeRunner.Run(ctx, name, args...)
}
