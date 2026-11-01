package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Tart normally comes from Homebrew. A worker that runs as its own user on a Mac whose Homebrew
// belongs to an admin cannot install into it (and should not depend on it), so grove can instead
// install Tart's pinned release tarball under the user's own ~/.local: the bundle in
// lib/grove/tart.app, linked from bin/tart. Whichever way Tart got there, tartHeadlessStep makes
// its background-only copy from the genuine bundle (see findTartSource).

// groveManagedTartApp is where grove extracts the release tarball's tart.app.
func groveManagedTartApp(opts Options) string {
	return filepath.Join(opts.homeDir(), ".local", "lib", "grove", "tart.app")
}

func groveManagedTartExecutable(opts Options) string {
	return filepath.Join(groveManagedTartApp(opts), "Contents", "MacOS", "tart")
}

// groveManagedTartLink is the `tart` on the user's PATH that points into the managed bundle.
func groveManagedTartLink(opts Options) string { return filepath.Join(orchardBinDir(opts), "tart") }

// tartUsable is the tart step's check: a `tart` on PATH, or grove's own install.
func tartUsable(opts Options) bool {
	return onPath(opts.lookPath(), "tart") || isRegularFile(groveManagedTartExecutable(opts))
}

// tartBrewInstallPending reports whether the plan still has to install Tart through the
// openai/tools tap: Tart is missing and Homebrew is usable by this user. When Homebrew is another
// user's, the `tart` step installs the release tarball instead and the tap needs no trust.
func tartBrewInstallPending(ctx context.Context, r Runner, opts Options) bool {
	if tartUsable(opts) {
		return false
	}
	_, _, _, foreign := brewForeignOwner(ctx, r)
	return !foreign
}

func tartStep(r Runner, opts Options, out io.Writer) Step {
	return Step{
		Name: "tart",
		Description: fmt.Sprintf("Install Tart (`brew install openai/tools/tart`, falling back to `brew install cirruslabs/cli/tart` if that tap doesn't have it). If Homebrew belongs to another user, install Tart %s from its release tarball instead (pinned SHA-256; bundle id, signature and team checked) into %s, linked at %s.",
			TartVersion, groveManagedTartApp(opts), groveManagedTartLink(opts)),
		Check: func(ctx context.Context) (bool, error) {
			return tartUsable(opts), nil
		},
		Apply: func(ctx context.Context) error {
			if _, _, _, foreign := brewForeignOwner(ctx, r); foreign {
				return installTartFromRelease(ctx, r, opts, out)
			}
			// Cirrus Labs joined OpenAI; the canonical tap moved to openai/homebrew-tools
			// (openai/tools/tart), trusted by the "brew-trust:openai/tools" step above. Fall
			// back to the original cirruslabs/cli/tart tap only if the new one fails, in case
			// a host's brew hasn't picked up the move yet — trusting that fallback tap only
			// when it's actually needed, rather than unconditionally on every install.
			if _, _, err := r.Run(ctx, "brew", "install", "openai/tools/tart"); err == nil {
				return nil
			}
			if err := trustTap(ctx, r, tapCirruslabsCLI, out); err != nil {
				return err
			}
			_, _, err := r.Run(ctx, "brew", "install", "cirruslabs/cli/tart")
			return err
		},
	}
}

// installTartFromRelease fetches the pinned tarball, extracts its tart.app next to the final
// location, verifies it is the genuine signed bundle, and only then swaps it in and links it.
func installTartFromRelease(ctx context.Context, r Runner, opts Options, out io.Writer) error {
	d, err := tartDownload(opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "        Homebrew is not this user's; installing Tart %s from its release tarball instead.\n", TartVersion)
	staged, err := fetchVerified(ctx, r, opts, d)
	if err != nil {
		return err
	}
	dest := groveManagedTartApp(opts)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	extract := dest + ".extract"
	_ = os.RemoveAll(extract)
	if err := os.MkdirAll(extract, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(extract)
	if _, _, err := r.Run(ctx, "/usr/bin/tar", "-xzf", staged, "-C", extract, "tart.app"); err != nil {
		return fmt.Errorf("extract %s: %w", d.File, err)
	}
	fresh := filepath.Join(extract, "tart.app")
	if _, err := inspectTartApp(ctx, r, fresh, true); err != nil {
		return err
	}
	old := dest + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(fresh, dest); err != nil {
		_ = os.Rename(old, dest)
		return fmt.Errorf("install Tart: %w", err)
	}
	_ = os.RemoveAll(old)
	return linkManagedTart(opts)
}

// linkManagedTart points ~/.local/bin/tart at the managed bundle's executable. It replaces its own
// earlier link, never a file or link it did not make.
func linkManagedTart(opts Options) error {
	link := groveManagedTartLink(opts)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a link; move it aside so grove can link Tart there", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Symlink(groveManagedTartExecutable(opts), link)
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func resolvedPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
