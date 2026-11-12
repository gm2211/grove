package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// grove updates itself where its binary is this user's own file (a release tarball copied in
// place, e.g. ~/.local/bin/grove): it downloads the newer release's archive, verifies it against
// that release's checksums.txt, checks that the new binary runs and reports the expected version,
// keeps the old one beside it as grove.<old>.bak, and renames the new one over it. A Homebrew
// install is left to `brew upgrade` (the Cellar belongs to whoever runs brew), and a development
// build never updates.

// groveLatestURL redirects to the latest release's page, .../releases/tag/vX.Y.Z: no API, no
// token, no rate limit worth the name.
const groveLatestURL = "https://github.com/gm2211/grove/releases/latest"

var releaseTagRE = regexp.MustCompile(`/releases/tag/(v\d+\.\d+\.\d+)$`)

// LatestGroveRelease returns the newest released version, as "vX.Y.Z".
func LatestGroveRelease(ctx context.Context, r Runner) (string, error) {
	stdout, _, err := r.Run(ctx, "curl", "-fsSL", "--proto", "=https", "--tlsv1.2", "-o", "/dev/null", "-w", "%{url_effective}", groveLatestURL)
	if err != nil {
		return "", fmt.Errorf("look up the latest grove release: %w", err)
	}
	m := releaseTagRE.FindStringSubmatch(strings.TrimSpace(stdout))
	if m == nil {
		return "", fmt.Errorf("look up the latest grove release: %s led to %q, not a release", groveLatestURL, strings.TrimSpace(stdout))
	}
	return m[1], nil
}

// parseVersion reads "vX.Y.Z" or "X.Y.Z"; anything else (dev, a -next snapshot) is not a release.
func parseVersion(v string) (out [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// NewerVersion reports whether release version candidate is newer than current. False when either
// isn't a plain release version: a development build is never replaced.
func NewerVersion(candidate, current string) bool {
	a, okA := parseVersion(candidate)
	b, okB := parseVersion(current)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// SelfUpdatable says why the grove binary at exe (symlinks resolved) can't be replaced by this
// user, or returns nil.
func SelfUpdatable(exe string) error {
	if strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/homebrew/") || strings.Contains(exe, "/linuxbrew/") {
		return fmt.Errorf("%s is Homebrew's; upgrade it with `brew upgrade grove`", exe)
	}
	info, err := os.Lstat(exe)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", exe)
	}
	probe, err := os.CreateTemp(filepath.Dir(exe), ".grove-update-probe-")
	if err != nil {
		return fmt.Errorf("%s: its directory is not writable by this user", exe)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// InstalledGroveVersion asks the binary at exe for its version ("X.Y.Z", as `grove --version`
// prints it).
func InstalledGroveVersion(ctx context.Context, r Runner, exe string) (string, error) {
	stdout, _, err := r.Run(ctx, exe, "--version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return "", errors.New("no version printed")
	}
	return strings.TrimPrefix(fields[len(fields)-1], "v"), nil
}

// SelfUpdate replaces the grove binary at exe with release target ("vX.Y.Z") for opts' platform.
// Nothing is installed unless the archive matches the release's checksums.txt and the extracted
// binary reports target as its version. The replaced binary is kept as <exe>.<its version>.bak.
func SelfUpdate(ctx context.Context, r Runner, opts Options, exe, target string, out io.Writer) error {
	if err := SelfUpdatable(exe); err != nil {
		return err
	}
	want := strings.TrimPrefix(target, "v")
	if _, ok := parseVersion(want); !ok {
		return fmt.Errorf("%q is not a release version", target)
	}
	rel := opts
	rel.GroveVersion = want
	d, err := releaseDownload(ctx, r, rel, fmt.Sprintf("grove_%s_%s.tar.gz", opts.goos(), opts.goarch()))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "downloading grove %s\n", want)
	staged, err := fetchVerified(ctx, r, opts, d)
	if err != nil {
		return err
	}
	defer os.Remove(staged)

	tmp, err := os.MkdirTemp(filepath.Dir(exe), ".grove-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, _, err := r.Run(ctx, "tar", "-xzf", staged, "-C", tmp, "grove"); err != nil {
		return fmt.Errorf("extract %s: %w", d.File, err)
	}
	fresh := filepath.Join(tmp, "grove")
	if _, _, err := r.Run(ctx, "chmod", "755", fresh); err != nil {
		return err
	}
	got, err := InstalledGroveVersion(ctx, r, fresh)
	if err != nil {
		return fmt.Errorf("the downloaded grove does not run: %w", err)
	}
	if got != want {
		return fmt.Errorf("the downloaded grove reports version %s, want %s", got, want)
	}

	if old, err := InstalledGroveVersion(ctx, r, exe); err == nil && old != "" {
		backup := exe + "." + old + ".bak"
		if _, _, err := r.Run(ctx, "cp", "-pf", exe, backup); err != nil {
			return fmt.Errorf("keep the current grove as %s: %w", backup, err)
		}
	}
	if _, _, err := r.Run(ctx, "mv", "-f", fresh, exe); err != nil {
		return fmt.Errorf("install grove %s to %s: %w", want, exe, err)
	}
	fmt.Fprintf(out, "grove %s installed at %s\n", want, exe)
	return nil
}

// WorkerInstalledHere reports whether this user has a worker installed (its Orchard wrapper
// exists), i.e. whether an updated grove should re-run the worker install.
func WorkerInstalledHere(opts Options) bool {
	_, err := os.Stat(filepath.Join(opts.ConfigDir(), workerWrapperName))
	return err == nil
}

// UnattendedEnv, set in the environment of an install that nobody is watching (the one a
// self-update runs), keeps it to re-rendering what grove owns: it never bootstraps Homebrew.
const UnattendedEnv = "GROVE_UNATTENDED"

// UnattendedPath is the PATH for such an install: the caller's, with the places Homebrew and a
// user-local grove live put first. A LaunchAgent or LaunchDaemon gets only
// /usr/bin:/bin:/usr/sbin:/sbin, where neither brew nor tailscale is.
func UnattendedPath(current, home string) string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range append(dirs, filepath.SplitList(current)...) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}
