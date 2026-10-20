package sandbox

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Top-level directories of the bundle: extracted at a container's root, or in a macOS job's run
// directory.
const (
	workspaceDir = "workspace"
	groveDir     = "grove"
)

// jobPaths says where run.sh finds things in the VM: workspace and grove are shell words (quoted
// as needed) naming the job's checkout and the directory holding run.sh, script.sh and env.sh, and
// prelude is shell run first, which may define variables those words use.
type jobPaths struct {
	prelude   string
	workspace string
	grove     string
}

// runScript is run.sh for a docker job (see runScriptAt), whose bundle is extracted at its
// container's root.
func runScript(timeout time.Duration, ref string, checkout bool) string {
	return runScriptAt(containerPaths, timeout, ref, checkout)
}

// runScriptAt is the job's entrypoint: for a job with a repository, the checkout of ref from the
// .git the control plane fetched (exit 1 if that fails); then the job's environment, then its
// script in the workspace under the job's timeout (124 on expiry). Runner keeps its own timer too, for guests without timeout(1) (macOS).
func runScriptAt(p jobPaths, timeout time.Duration, ref string, checkout bool) string {
	secs := int64(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	if ref == "" {
		ref = "HEAD"
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
# Written by grove sandbox run: this job's entrypoint inside its sealed VM.
set -uo pipefail
`)
	b.WriteString(p.prelude)
	fmt.Fprintf(&b, "cd %s\n", p.workspace)
	if checkout {
		fmt.Fprintf(&b, `if ! git checkout --force --quiet %[1]s --; then
  echo "grove: could not check out "%[1]s >&2
  exit 1
fi
`, shQuote(ref))
	}
	fmt.Fprintf(&b, `. %[2]s/env.sh
if command -v timeout >/dev/null 2>&1; then
  exec timeout --kill-after=10 %[1]d bash -eo pipefail %[2]s/script.sh
fi
exec bash -eo pipefail %[2]s/script.sh
`, secs, p.grove)
	return b.String()
}

// envScript renders the caller's environment as `export` lines, sorted so the bundle is
// deterministic. Keys a shell can't export are skipped, and so are
// the clone credentials, which served the control plane's clone and never enter a sealed VM.
func envScript(env map[string]string) (script string, dropped []string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b strings.Builder
	b.WriteString("# Written by grove sandbox run: the job's environment.\n")
	for _, k := range keys {
		if !envKeyRE.MatchString(k) {
			continue
		}
		if slices.Contains(cloneCredentialEnv, k) {
			dropped = append(dropped, k)
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shQuote(env[k]))
	}
	return b.String(), dropped
}

// writeBundle writes the tar Runner pushes into the job's container: workspace/ holding the
// checkout (empty for a job without a repo) and grove/ holding run.sh, script.sh and env.sh.
// Symlinks in the checkout stay symlinks: nothing outside checkout is ever read.
func writeBundle(w io.Writer, checkout string, files map[string]string) error {
	tw := tar.NewWriter(w)
	now := time.Now()

	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: now})
	}
	if err := dir(workspaceDir); err != nil {
		return err
	}
	if checkout != "" {
		if err := addTree(tw, checkout, workspaceDir); err != nil {
			return err
		}
	}

	if err := dir(groveDir); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		mode := int64(0o644)
		if strings.HasSuffix(name, "run.sh") {
			mode = 0o755
		}
		body := files[name]
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: path.Join(groveDir, name), Mode: mode, Size: int64(len(body)), ModTime: now}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// addTree adds root's contents under prefix: directories, regular files and symlinks (as links).
func addTree(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := path.Join(prefix, filepath.ToSlash(rel))

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := int64(info.Mode().Perm())
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: mode, ModTime: info.ModTime()})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		default:
			return nil // sockets, devices and the like have no place in a checkout
		}
	})
}
