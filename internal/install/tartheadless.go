package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The worker's daemon runs `tart run` through Orchard. Homebrew's tart.app is a regular foreground
// app, so WindowServer checks every `tart run` into the console user's session and "Tart" flashes
// in their Dock. Grove therefore runs Tart only from its own copy of the bundle with LSUIElement
// set, re-signed ad hoc with just the virtualization entitlement, put first on the worker's PATH
// through an exec shim (a shim, not a symlink: AppKit must resolve the bundle from its real path).
// Bridged networking (com.apple.vm.networking, a restricted entitlement that cannot survive an
// ad-hoc signature) is unavailable in the copy; grove never uses it.
const (
	groveTartAppName  = "Grove Tart.app"
	groveTartBundleID = "com.gm2211.grove.tart"
	groveTartName     = "Grove Tart"

	upstreamTartBundleID = "com.github.cirruslabs.tart"
	upstreamTartTeamID   = "9M2P8L4D89"

	entVirtualization = "com.apple.security.virtualization"
	entVMNetworking   = "com.apple.vm.networking"
)

func groveTartAppDir(opts Options) string {
	return filepath.Join(opts.homeDir(), "Applications", groveTartAppName)
}

func groveTartExecutable(opts Options) string {
	return filepath.Join(groveTartAppDir(opts), "Contents", "MacOS", "tart")
}

func groveTartInfoPlist(opts Options) string {
	return filepath.Join(groveTartAppDir(opts), "Contents", "Info.plist")
}

func groveTartProvenancePath(opts Options) string {
	return filepath.Join(groveTartAppDir(opts), "Contents", "Resources", "grove-tart-source")
}

// groveTartBinDir holds the exec shim the worker wrapper puts first on PATH.
func groveTartBinDir(opts Options) string { return filepath.Join(opts.ConfigDir(), "tart-bin") }

func groveTartShimPath(opts Options) string { return filepath.Join(groveTartBinDir(opts), "tart") }

func groveTartEntitlementsPath(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "tart-entitlements.plist")
}

func groveTartShim(opts Options) string {
	return fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", groveTartExecutable(opts))
}

func groveTartEntitlements() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>` + entVirtualization + `</key>
	<true/>
</dict>
</plist>
`
}

// tartSource is the Homebrew tart.app the copy is made from.
type tartSource struct {
	App     string // <brew prefix>/libexec/tart.app, symlinks resolved
	Version string
	CDHash  string
}

func (s tartSource) provenance() string {
	return fmt.Sprintf("source=%s\nversion=%s\ncdhash=%s\n", s.App, s.Version, s.CDHash)
}

var (
	cdhashRE = regexp.MustCompile(`(?m)^CDHash=(\S+)`)
	teamRE   = regexp.MustCompile(`(?m)^TeamIdentifier=(\S+)`)
)

func plutilRaw(ctx context.Context, r Runner, plist, key string) (string, error) {
	stdout, _, err := r.Run(ctx, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist)
	return strings.TrimSpace(stdout), err
}

// brewTartSource locates Homebrew's tart.app and, when verify is set, refuses anything that is not
// the genuine, validly signed Cirrus Labs bundle. Without verify it only reads the version and
// code-directory hash (the doctor and the idempotency check).
func brewTartSource(ctx context.Context, r Runner, verify bool) (tartSource, error) {
	var s tartSource
	stdout, _, err := r.Run(ctx, "brew", "--prefix", "tart")
	prefix := strings.TrimSpace(stdout)
	if err != nil || prefix == "" {
		return s, fmt.Errorf("`brew --prefix tart` found no Homebrew tart")
	}
	s.App = filepath.Join(prefix, "libexec", "tart.app")
	if real, err := filepath.EvalSymlinks(s.App); err == nil {
		s.App = real
	}
	plist := filepath.Join(s.App, "Contents", "Info.plist")
	if verify {
		id, err := plutilRaw(ctx, r, plist, "CFBundleIdentifier")
		if err != nil || id != upstreamTartBundleID {
			return s, fmt.Errorf("refusing %s: bundle identifier %q, want %s", s.App, id, upstreamTartBundleID)
		}
		if _, _, err := r.Run(ctx, "/usr/bin/codesign", "--verify", "--strict", s.App); err != nil {
			return s, fmt.Errorf("refusing %s: its code signature does not verify: %w", s.App, err)
		}
	}
	_, stderr, err := r.Run(ctx, "/usr/bin/codesign", "-dv", s.App)
	if err != nil {
		return s, fmt.Errorf("read %s code signature: %w", s.App, err)
	}
	if verify {
		if m := teamRE.FindStringSubmatch(stderr); m == nil || m[1] != upstreamTartTeamID {
			return s, fmt.Errorf("refusing %s: not signed by team %s", s.App, upstreamTartTeamID)
		}
	}
	if m := cdhashRE.FindStringSubmatch(stderr); m != nil {
		s.CDHash = m[1]
	}
	s.Version, _ = plutilRaw(ctx, r, plist, "CFBundleShortVersionString")
	return s, nil
}

// groveTartCopyProblems lists everything wrong with the installed copy: missing, unsigned, not
// background-only, wrong entitlements, a stale shim. current, when non-nil, is the brew tart the
// copy must have been made from.
func groveTartCopyProblems(ctx context.Context, r Runner, opts Options, current *tartSource) []string {
	app := groveTartAppDir(opts)
	if info, err := os.Stat(groveTartExecutable(opts)); err != nil || !info.Mode().IsRegular() {
		return []string{app + " is missing"}
	}
	var problems []string
	if v, err := plutilRaw(ctx, r, groveTartInfoPlist(opts), "LSUIElement"); err != nil || v != "true" {
		problems = append(problems, "LSUIElement is not true, so Tart would show in the Dock")
	}
	if _, _, err := r.Run(ctx, "/usr/bin/codesign", "--verify", "--strict", app); err != nil {
		problems = append(problems, "its code signature does not verify")
	} else {
		ents, _, _ := r.Run(ctx, "/usr/bin/codesign", "-d", "--entitlements", ":-", app)
		if !strings.Contains(ents, entVirtualization) || strings.Contains(ents, entVMNetworking) {
			problems = append(problems, "its entitlements are not exactly "+entVirtualization)
		}
	}
	if got, err := os.ReadFile(groveTartShimPath(opts)); err != nil || string(got) != groveTartShim(opts) {
		problems = append(problems, groveTartShimPath(opts)+" is missing or stale")
	}
	if current != nil {
		got, _ := os.ReadFile(groveTartProvenancePath(opts))
		if string(got) != current.provenance() {
			have := "unknown"
			for _, l := range strings.Split(string(got), "\n") {
				if v, ok := strings.CutPrefix(l, "version="); ok {
					have = v
				}
			}
			problems = append(problems, fmt.Sprintf("it was made from Tart %s, Homebrew's tart is %s", have, current.Version))
		}
	}
	return problems
}

func tartHeadlessStep(r Runner, opts Options) Step {
	app := groveTartAppDir(opts)
	return Step{
		Name: "tart-headless-app",
		Description: fmt.Sprintf(
			"Make %s: a copy of Homebrew's tart.app (verified genuine: bundle id, signature, team %s) with LSUIElement set so `tart run` never flashes in the Dock, re-signed ad hoc with only %s (bridged networking is unavailable in it), and an exec shim %s the worker puts first on PATH. Re-made when Homebrew's tart changes.",
			app, upstreamTartTeamID, entVirtualization, groveTartShimPath(opts)),
		Check: func(ctx context.Context) (bool, error) {
			src, err := brewTartSource(ctx, r, false)
			if err != nil {
				return false, nil
			}
			return len(groveTartCopyProblems(ctx, r, opts, &src)) == 0, nil
		},
		Apply: func(ctx context.Context) error {
			src, err := brewTartSource(ctx, r, true)
			if err != nil {
				return err
			}
			return buildGroveTart(ctx, r, opts, src)
		},
	}
}

func buildGroveTart(ctx context.Context, r Runner, opts Options, src tartSource) error {
	dest := groveTartAppDir(opts)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".new"
	_ = os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if _, _, err := r.Run(ctx, "/usr/bin/ditto", src.App, tmp); err != nil {
		return fmt.Errorf("copy %s: %w", src.App, err)
	}
	plist := filepath.Join(tmp, "Contents", "Info.plist")
	for _, kv := range [][]string{
		{"LSUIElement", "-bool", "YES"},
		{"CFBundleIdentifier", "-string", groveTartBundleID},
		{"CFBundleName", "-string", groveTartName},
		{"CFBundleDisplayName", "-string", groveTartName},
	} {
		if _, _, err := r.Run(ctx, "/usr/bin/plutil", "-replace", kv[0], kv[1], kv[2], plist); err != nil {
			return fmt.Errorf("set %s in the Tart copy: %w", kv[0], err)
		}
	}
	// com.apple.vm.networking is restricted: only a provisioning profile allows it, and that cannot
	// survive an ad-hoc signature.
	if err := os.Remove(filepath.Join(tmp, "Contents", "embedded.provisionprofile")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeFile(groveTartEntitlementsPath(opts), groveTartEntitlements()); err != nil {
		return err
	}
	// Provenance goes in before signing: a file added afterwards would break the seal.
	prov := filepath.Join(tmp, "Contents", "Resources", "grove-tart-source")
	if err := os.MkdirAll(filepath.Dir(prov), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(prov, []byte(src.provenance()), 0o644); err != nil {
		return err
	}
	if _, _, err := r.Run(ctx, "/usr/bin/codesign", "--force", "--sign", "-",
		"--entitlements", groveTartEntitlementsPath(opts),
		"--identifier", groveTartBundleID, "--timestamp=none", tmp); err != nil {
		return fmt.Errorf("sign the Tart copy: %w", err)
	}
	old := dest + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Rename(old, dest)
		return fmt.Errorf("install the Tart copy: %w", err)
	}
	_ = os.RemoveAll(old)
	if err := os.MkdirAll(groveTartBinDir(opts), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(groveTartShimPath(opts), []byte(groveTartShim(opts)), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(groveTartShimPath(opts), 0o755); err != nil {
		return err
	}
	restartDaemon(ctx, r, orchardWorkerLabel)
	return nil
}

// checkHeadlessTart is the doctor's view of the same copy.
func checkHeadlessTart(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "headless-tart"
	const fix = "re-run `grove install --role worker` (after `brew upgrade tart`, too): it re-makes the Grove Tart copy"
	var problems []string
	if cur, err := brewTartSource(ctx, r, false); err != nil {
		problems = groveTartCopyProblems(ctx, r, opts, nil)
	} else {
		problems = groveTartCopyProblems(ctx, r, opts, &cur)
	}
	if b, err := os.ReadFile(workerWrapperPath(opts)); err == nil && !strings.Contains(string(b), workerWrapperTartPathPrefix) {
		problems = append(problems, workerWrapperPath(opts)+" does not put the Grove Tart shim first on PATH")
	}
	if len(problems) > 0 {
		return CheckResult{Name: name, OK: false, Detail: "Tart would flash in the Dock: " + strings.Join(problems, "; "), Remediation: fix}
	}
	return CheckResult{Name: name, OK: true, Detail: groveTartAppDir(opts) + " is background-only, signed with only " + entVirtualization}
}
