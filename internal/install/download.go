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
//   - Homebrew installer: the commit of github.com/Homebrew/install and `sha256sum install.sh`
//     at that commit (Homebrew/install publishes no tags).
const (
	// HomebrewInstallCommit pins Homebrew's installer script (instead of its HEAD branch).
	HomebrewInstallCommit = "35da6871c4be7d7fdab2fd505fb7fa667926a2a5"
	// HomebrewInstallSHA256 is install.sh's digest at HomebrewInstallCommit.
	HomebrewInstallSHA256 = "5f333bbe53bc490e51e7ccb1df8779b3dd6ee73a1a7379efda216edb08ccb148"
)

// pinnedDownload is one fetch-verify unit: a URL and the digest its bytes must have.
type pinnedDownload struct {
	URL    string
	SHA256 string
	// File is the staging file name under downloadDir.
	File string
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
