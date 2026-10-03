package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pinned third-party downloads. grove never installs a "latest" binary it fetched with curl: each
// artifact is fetched at an explicit version, its SHA-256 is compared against the value embedded
// here, and only a match is installed. A mismatch deletes the download and fails the step.
//
// To bump a pin, change the version and every per-platform hash in the same commit:
//   - Nomad: https://releases.hashicorp.com/nomad/<v>/nomad_<v>_SHA256SUMS (the *.zip lines).
//   - MinIO server / mc: the sha256 lines of minio/homebrew-stable's minio.rb / mc.rb for the
//     same RELEASE tag (they mirror dl.min.io's <file>.sha256sum).
//   - Homebrew installer: the commit of github.com/Homebrew/install and `sha256sum install.sh`
//     at that commit (Homebrew/install publishes no tags).
const (
	// NomadVersion matches the upstream release the worker images' patched Nomad
	// (2.0.7+grove.1, see docs/IMAGES.md) is built from. Servers must not be older than clients.
	NomadVersion = "2.0.7"
	// MinIOVersion is the last MinIO community release with published binaries, matching
	// minio/stable's Homebrew formula that macOS control planes install from.
	MinIOVersion = "RELEASE.2025-09-06T17-38-46Z"
	// MinIOClientVersion matches images/runner/Dockerfile's MINIO_CLIENT_VERSION.
	MinIOClientVersion = "RELEASE.2025-08-13T08-35-41Z"
	// HomebrewInstallCommit pins Homebrew's installer script (instead of its HEAD branch).
	HomebrewInstallCommit = "35da6871c4be7d7fdab2fd505fb7fa667926a2a5"
	// HomebrewInstallSHA256 is install.sh's digest at HomebrewInstallCommit.
	HomebrewInstallSHA256 = "5f333bbe53bc490e51e7ccb1df8779b3dd6ee73a1a7379efda216edb08ccb148"
)

// nomadZipSHA256 is the SHA-256 of nomad_<NomadVersion>_<os>_<arch>.zip, keyed "os/arch".
var nomadZipSHA256 = map[string]string{
	"darwin/amd64": "085a1c11cd2de2e5be490cbcc4213b1faaa0af45a4e121a55b2340dcef0d6286",
	"darwin/arm64": "4ada34db43f8b75c80c4e09ce50f4753e661bcb4a3745f415dd4aa7d01483e58",
	"linux/amd64":  "4c9b8a0850d6fd9caadbbab09b3e6fdf8b77aa777729543c70c61b85acca68c1",
	"linux/arm64":  "19708097585efe0d60234f300b62892fa776ff93ca6ba5fb918360cbd118b71e",
}

// minioSHA256 is the SHA-256 of minio.<MinIOVersion> for each "os/arch".
var minioSHA256 = map[string]string{
	"darwin/amd64": "4ca9e7a9546200e6f6991bc719c7c3d30cb9c5dfe0b2358e3bfc5ca805c45bcc",
	"darwin/arm64": "1330658e1be4da8ae97f1bebe9e820bfe3577f068bfc768b978a6a89959fc5f7",
	"linux/amd64":  "0637cc3e23c4e87c9902b5bbd82fd0b69075c8ef1e55630d0a1902d3e66d48a8",
	"linux/arm64":  "69afb8561caa19a0f3a3064c7f4eab69628a9a79f37577afe4313ad760cd53a0",
}

// minioClientSHA256 is the SHA-256 of mc.<MinIOClientVersion> for each "os/arch".
var minioClientSHA256 = map[string]string{
	"darwin/amd64": "2862c79cce11b09be9a8911a279b2e9465bebf74b9f01abca9c348a0d795f0cb",
	"darwin/arm64": "a877fd0c183409da9f20f9d6e1811987298bbbca1aa03428eebdffba79fb9445",
	"linux/amd64":  "01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891",
	"linux/arm64":  "14c8c9616cfce4636add161304353244e8de383b2e2752c0e9dad01d4c27c12c",
}

// pinnedDownload is one fetch-verify unit: a URL and the digest its bytes must have.
type pinnedDownload struct {
	URL    string
	SHA256 string
	// File is the staging file name under downloadDir.
	File string
}

func platformKey(opts Options) string { return opts.goos() + "/" + opts.goarch() }

func pinnedFor(table map[string]string, what string, opts Options) (string, error) {
	sum, ok := table[platformKey(opts)]
	if !ok {
		return "", fmt.Errorf("no pinned %s checksum for %s; refusing to install an unverified binary", what, platformKey(opts))
	}
	return sum, nil
}

func nomadDownload(opts Options) (pinnedDownload, error) {
	sum, err := pinnedFor(nomadZipSHA256, "Nomad "+NomadVersion, opts)
	if err != nil {
		return pinnedDownload{}, err
	}
	file := fmt.Sprintf("nomad_%s_%s_%s.zip", NomadVersion, opts.goos(), opts.goarch())
	return pinnedDownload{
		URL:    fmt.Sprintf("https://releases.hashicorp.com/nomad/%s/%s", NomadVersion, file),
		SHA256: sum,
		File:   file,
	}, nil
}

func minioDownload(opts Options) (pinnedDownload, error) {
	sum, err := pinnedFor(minioSHA256, "MinIO "+MinIOVersion, opts)
	if err != nil {
		return pinnedDownload{}, err
	}
	file := "minio." + MinIOVersion
	return pinnedDownload{
		URL:    fmt.Sprintf("https://dl.min.io/server/minio/release/%s-%s/archive/%s", opts.goos(), opts.goarch(), file),
		SHA256: sum,
		File:   file,
	}, nil
}

func minioClientDownload(opts Options) (pinnedDownload, error) {
	sum, err := pinnedFor(minioClientSHA256, "MinIO client "+MinIOClientVersion, opts)
	if err != nil {
		return pinnedDownload{}, err
	}
	file := "mc." + MinIOClientVersion
	return pinnedDownload{
		URL:    fmt.Sprintf("https://dl.min.io/client/mc/release/%s-%s/archive/%s", opts.goos(), opts.goarch(), file),
		SHA256: sum,
		File:   file,
	}, nil
}

func homebrewInstallerDownload() pinnedDownload {
	return pinnedDownload{
		URL:    "https://raw.githubusercontent.com/Homebrew/install/" + HomebrewInstallCommit + "/install.sh",
		SHA256: HomebrewInstallSHA256,
		File:   "homebrew-install-" + HomebrewInstallCommit[:12] + ".sh",
	}
}

// downloadDir is where pinned artifacts are staged before verification. Deterministic (under the
// user's home, not os.TempDir) so a planner test can script the hash command for an exact path.
func downloadDir(opts Options) string {
	return filepath.Join(opts.homeDir(), ".cache", "grove", "downloads")
}

// sha256Command is the host tool that prints "<hex>  <file>": shasum ships with macOS,
// sha256sum with every coreutils Linux.
func sha256Command(opts Options, path string) (string, []string) {
	if opts.goos() == "darwin" {
		return "shasum", []string{"-a", "256", path}
	}
	return "sha256sum", []string{path}
}

// fetchVerified downloads d into downloadDir, hashes it through r, and returns the staged path
// only when the digest matches d.SHA256. On a mismatch (or an unreadable hash) the staged file is
// removed and an error returned: nothing is ever installed from an unverified download.
func fetchVerified(ctx context.Context, r Runner, opts Options, d pinnedDownload) (string, error) {
	dir := downloadDir(opts)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	staged := filepath.Join(dir, d.File)
	if _, _, err := r.Run(ctx, "curl", "-fsSL", "--proto", "=https", "--tlsv1.2", "-o", staged, d.URL); err != nil {
		return "", fmt.Errorf("download %s: %w", d.URL, err)
	}
	if err := verifySHA256(ctx, r, opts, staged, d.SHA256); err != nil {
		_, _, _ = r.Run(ctx, "rm", "-f", staged)
		return "", fmt.Errorf("verify %s: %w", d.URL, err)
	}
	return staged, nil
}

// verifySHA256 hashes path through r and compares it with want (lower-case hex).
func verifySHA256(ctx context.Context, r Runner, opts Options, path, want string) error {
	name, args := sha256Command(opts, path)
	stdout, _, err := r.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return fmt.Errorf("checksum mismatch: %s printed no digest", name)
	}
	if got := strings.ToLower(fields[0]); got != want {
		return fmt.Errorf("checksum mismatch: got sha256 %s, want %s", got, want)
	}
	return nil
}

// installVerifiedBinary fetches d, verifies it, and installs it as dest (mode 0755).
func installVerifiedBinary(ctx context.Context, r Runner, opts Options, d pinnedDownload, dest string) error {
	staged, err := fetchVerified(ctx, r, opts, d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// install(1) goes through r (not os.Rename) so the whole flow is inert under a FakeRunner.
	_, _, err = r.Run(ctx, "install", "-m", "0755", staged, dest)
	return err
}
