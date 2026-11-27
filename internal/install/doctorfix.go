package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// `sudo grove doctor --fix`: everything root does for a worker Mac, from one command an admin
// runs. It finds the worker user itself, installs (or updates) the root helper for it, and runs
// whatever root steps that user's worker still needs, all rendered by this process, never by
// anything the worker user can write.

// usersDir is where macOS keeps home directories. Test override.
var usersDir = "/Users"

// FindWorkerUser names the account this Mac's grove worker runs as, from what only root writes
// first (the root helper's config, the worker LaunchDaemons) and then from each home's grove
// worker identity. It fails unless exactly one account is found.
func FindWorkerUser() (*user.User, error) {
	if cfg, err := readRootHelperConfig(); err == nil {
		return user.Lookup(cfg.User)
	}
	if inst, ok := FindInstalledWorker(Options{}); ok {
		return user.Lookup(inst.User)
	}
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return nil, err
	}
	var found []*user.User
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "Shared" {
			continue
		}
		u, err := user.Lookup(e.Name())
		if err != nil || u.Uid == "0" {
			continue
		}
		opts := Options{User: u.Username, Home: u.HomeDir}
		if _, err := os.Stat(workerIdentityPath(opts)); err == nil {
			found = append(found, u)
		}
	}
	switch len(found) {
	case 0:
		return nil, errors.New("no grove worker found on this Mac (no account has one installed)")
	case 1:
		return found[0], nil
	default:
		names := make([]string, len(found))
		for i, u := range found {
			names[i] = u.Username
		}
		return nil, fmt.Errorf("more than one account has a grove worker (%s)", strings.Join(names, ", "))
	}
}

// RootRepairOptions are the Options for rendering u's worker's root steps the way an admin's
// `grove setup --user` repair does: u's name and home, the control plane and grove binary the
// root-owned daemons name, and the worker's shape as it is (rootless stays rootless).
func RootRepairOptions(ctx context.Context, r Runner, u *user.User) (Options, error) {
	opts := Options{Role: RoleWorker, User: u.Username, Home: u.HomeDir}
	if inst, ok := FindInstalledWorker(opts); ok {
		opts.ServerURL = inst.ServerURL
	}
	if bin := installedBrokerProgram(opts); bin != "" {
		opts.GroveBin = bin
	}
	if !rootDaemonsPresent(ctx, r, opts, workerDaemons(opts)) {
		opts.Rootless, opts.NoAutoRootless = true, false
		return opts, nil
	}
	rootless, err := WorkerRootless(ctx, r, opts)
	if err != nil {
		return Options{}, err
	}
	opts.Rootless, opts.NoAutoRootless = rootless, !rootless
	return opts, nil
}

// installedBrokerProgram is the program the installed (root-owned) broker LaunchDaemon runs, or "".
func installedBrokerProgram(opts Options) string {
	_, installed := daemonPaths(opts, brokerLabel)
	b, err := os.ReadFile(installed)
	if err != nil {
		return ""
	}
	const open = "<key>ProgramArguments</key>"
	i := strings.Index(string(b), open)
	if i < 0 {
		return ""
	}
	rest := string(b[i+len(open):])
	s := strings.Index(rest, "<string>")
	e := strings.Index(rest, "</string>")
	if s < 0 || e < s {
		return ""
	}
	return rest[s+len("<string>") : e]
}

// FixAsRoot is `sudo grove doctor --fix`: the root helper for the worker user (installed, or
// replaced by this grove when it differs), then the worker's other pending root steps as one
// script. exe is this process's binary.
func FixAsRoot(ctx context.Context, r Runner, exe, version string, out io.Writer) error {
	if rootHelperEUID() != 0 {
		return errors.New("FixAsRoot runs as root")
	}
	u, err := FindWorkerUser()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Fixing the grove worker of %s.\n", u.Username)
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if who := writableBy(resolved, u); who != "" {
		return fmt.Errorf("this grove (%s) can be changed by %s (%s), so root won't run it: run an admin's grove (installed from an admin account) as `sudo grove doctor --fix`", resolved, u.Username, who)
	}

	mine, err := fileSHA256(resolved)
	if err != nil {
		return err
	}
	installed, _ := fileSHA256(rootHelperBin())
	if cfg, err := readRootHelperConfig(); err != nil || cfg.User != u.Username || installed != mine {
		what := "Installing"
		if installed != "" {
			what = "Updating"
		}
		fmt.Fprintf(out, "%s the root helper (%s) with grove %s.\n", what, RootHelperLabel, version)
		if err := InstallRootHelper(ctx, r, RootHelperInstall{User: u, Exe: resolved, GroveVersion: version, Out: out}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "The root helper is installed and current.\n")
	}

	opts, err := RootRepairOptions(ctx, r, u)
	if err != nil {
		return err
	}
	steps, err := PendingRootSteps(ctx, r, opts)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Fprintln(out, "No other root steps are needed.")
		return nil
	}
	script := "set -e\n"
	var names []string
	for _, s := range steps {
		body, err := s.Root(ctx)
		if err != nil {
			return fmt.Errorf("root script for %s: %w", s.Name, err)
		}
		names = append(names, s.Name)
		script += "echo " + shQuote("grove: "+s.Name) + "\n" + strings.TrimRight(body, "\n") + "\n"
	}
	fmt.Fprintf(out, "Running root steps: %s.\n", strings.Join(names, ", "))
	if _, stderr, err := r.Run(ctx, "/bin/sh", "-e", "-c", script); err != nil {
		return fmt.Errorf("root steps %s: %w %s", strings.Join(names, ", "), err, strings.TrimSpace(stderr))
	}
	return recheckRootSteps(ctx, steps)
}
