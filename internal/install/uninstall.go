package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Uninstalling a worker undoes `grove install --role worker` in the opposite order: the root steps
// first (boot out and remove the three LaunchDaemons, flush the VM firewall's pf anchor, and, if
// asked, let the Mac sleep again), then the worker user's own files. Removing the user's files first
// would leave KeepAlive daemons restarting a wrapper or broker that is gone.
//
// The root text names only fixed paths (/Library/LaunchDaemons/<label>.plist, the anchor) and
// absolute system binaries, so an admin's grove can render it for another user without reading
// anything that user can write. Every step's Check is "already gone", so a re-run after a full
// uninstall does nothing.

// UninstallOptions are the choices `grove uninstall --role worker` adds to Options.
type UninstallOptions struct {
	// RestoreSleep runs `pmset -a disablesleep 0`. Off by default: the owner may want the Mac kept
	// awake for other reasons.
	RestoreSleep bool
	// Purge also removes config.yaml (this Mac's joined control plane and client credential).
	Purge bool
}

// workerDaemonLabels are the LaunchDaemons a worker install leaves in /Library/LaunchDaemons.
var workerDaemonLabels = []string{orchardWorkerLabel, brokerLabel}

// workerLogNames are the log files the worker's daemons write under LogDir.
var workerLogNames = []string{"orchard-worker.log", "orchard-worker.err.log", "broker.log", "broker.err.log"}

// InstalledWorkerName is the Orchard name this Mac's worker runs under, or "" if no wrapper or
// legacy LaunchAgent records one. Only the worker user can read its wrapper.
func InstalledWorkerName(opts Options) string { return installedWorkerName(opts) }

// configFilePath is config.yaml: GROVE_CONFIG if set, else ConfigDir()/config.yaml.
func configFilePath(opts Options) string {
	if p := os.Getenv("GROVE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(opts.ConfigDir(), "config.yaml")
}

// BuildUninstallPlan returns the steps that remove a worker from this Mac: the privileged ones
// first, then the user's files. Privileged steps go through RunRootSteps, like the install's.
func BuildUninstallPlan(r Runner, opts Options, u UninstallOptions) ([]Step, error) {
	if opts.Role != RoleWorker {
		return nil, fmt.Errorf("uninstall supports --role %s only", RoleWorker)
	}
	if _, err := opts.workerUser(); err != nil {
		return nil, err
	}
	steps := []Step{removeWorkerDaemonsStep(r, opts), removeHostFirewallStep(r, opts)}
	if u.RestoreSleep {
		steps = append(steps, restoreSleepStep(r))
	}
	steps = append(steps, removeLegacyAgentsStep(r, opts), removeKeepAwakeStep(r, opts))
	cfg := opts.ConfigDir()
	steps = append(steps,
		removePathsStep("worker-staged-plists", "the staged LaunchDaemon plists", filepath.Join(cfg, "launchd"), filepath.Join(cfg, "pf")),
		removePathsStep("worker-credentials", "the worker's credential files (Orchard bootstrap, broker, registry)", credentialsDir(opts)),
		removePathsStep("orchard-worker-wrapper", "the Orchard worker's wrapper script and recorded identity", workerWrapperPath(opts), workerIdentityPath(opts)),
		removeRegistryMarkersStep(opts),
		removePathsStep("orchard-worker-app", "the Orchard worker app", workerOrchardAppDir(opts)),
		removePathsStep("grove-tart-app", "the Grove Tart copy, its PATH shim and entitlements", groveTartAppDir(opts), groveTartBinDir(opts), groveTartEntitlementsPath(opts)),
		removeOrchardSymlinkStep(opts),
		removeWorkerLogsStep(opts),
	)
	if u.Purge {
		steps = append(steps, purgeConfigStep(opts))
	}
	return steps, nil
}

// daemonGone reports whether label's plist is absent from LaunchDaemons and launchd has no such
// job in the system domain.
func daemonGone(ctx context.Context, r Runner, opts Options, label string) bool {
	if _, err := os.Lstat(filepath.Join(opts.launchDaemonsDir(), label+".plist")); err == nil {
		return false
	}
	_, _, err := r.Run(ctx, "launchctl", "print", "system/"+label)
	return err != nil
}

// bootoutAndRemove is the root text that unloads label (not loaded is fine) and removes its plist.
func bootoutAndRemove(opts Options, label string) string {
	return fmt.Sprintf("/bin/launchctl bootout %s 2>/dev/null || true\n/bin/rm -f %s\n",
		shQuote("system/"+label), shQuote(filepath.Join(opts.launchDaemonsDir(), label+".plist")))
}

// runRootText applies a privileged step when this process is already root (RunPlan with --yes).
func runRootText(ctx context.Context, r Runner, root func(context.Context) (string, error)) error {
	script, err := root(ctx)
	if err != nil {
		return err
	}
	_, _, err = r.Run(ctx, "/bin/sh", "-e", "-c", script)
	return err
}

func removeWorkerDaemonsStep(r Runner, opts Options) Step {
	root := func(context.Context) (string, error) {
		var b strings.Builder
		for _, l := range workerDaemonLabels {
			b.WriteString(bootoutAndRemove(opts, l))
		}
		return b.String(), nil
	}
	return Step{
		Name:       "remove-worker-daemons",
		Privileged: true,
		Description: fmt.Sprintf("Stop the Orchard worker and broker LaunchDaemons (%s) and remove their plists from %s; VMs the worker runs stop with it.",
			strings.Join(workerDaemonLabels, ", "), opts.launchDaemonsDir()),
		Root: root,
		Check: func(ctx context.Context) (bool, error) {
			for _, l := range workerDaemonLabels {
				if !daemonGone(ctx, r, opts, l) {
					return false, nil
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context) error { return runRootText(ctx, r, root) },
	}
}

// removeHostFirewallStep boots out the firewall's LaunchDaemon before flushing the anchor, so its
// five-minute reload can't put the rules back.
func removeHostFirewallStep(r Runner, opts Options) Step {
	root := func(context.Context) (string, error) {
		return bootoutAndRemove(opts, hostFirewallLabel) +
			fmt.Sprintf("/sbin/pfctl -q -a %s -F all 2>/dev/null || true\n", shQuote(hostFirewallAnchor)), nil
	}
	return Step{
		Name:        "remove-host-firewall",
		Privileged:  true,
		Description: fmt.Sprintf("Stop and remove the VM firewall LaunchDaemon (%s) and flush its pf anchor %s; the Mac's own pf.conf is untouched.", hostFirewallLabel, hostFirewallAnchor),
		Root:        root,
		Check: func(ctx context.Context) (bool, error) {
			return daemonGone(ctx, r, opts, hostFirewallLabel), nil
		},
		Apply: func(ctx context.Context) error { return runRootText(ctx, r, root) },
	}
}

func restoreSleepStep(r Runner) Step {
	root := func(context.Context) (string, error) { return "/usr/bin/pmset -a disablesleep 0\n", nil }
	return Step{
		Name:        "restore-sleep",
		Privileged:  true,
		Description: "Let this Mac sleep again (pmset disablesleep 0).",
		Root:        root,
		Check: func(ctx context.Context) (bool, error) {
			stdout, _, err := r.Run(ctx, "pmset", "-g")
			if err != nil {
				return false, err
			}
			return !containsDisableSleep1(stdout), nil
		},
		Apply: func(ctx context.Context) error { return runRootText(ctx, r, root) },
	}
}

// removeLegacyAgentsStep removes per-user LaunchAgents an older grove installed for the same jobs.
func removeLegacyAgentsStep(r Runner, opts Options) Step {
	return Step{
		Name:        "remove-legacy-launchagents",
		Description: fmt.Sprintf("Unload and remove the per-user LaunchAgents an earlier grove installed under %s.", opts.LaunchAgentsDir()),
		Check: func(ctx context.Context) (bool, error) {
			return pathsGone(legacyAgentPaths(opts)...), nil
		},
		Apply: func(ctx context.Context) error {
			for _, l := range workerDaemonLabels {
				_, _, _ = r.Run(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", opts.workerUID(), l)) // no GUI session is fine
			}
			return removePaths(legacyAgentPaths(opts)...)
		},
	}
}

func legacyAgentPaths(opts Options) []string {
	var ps []string
	for _, l := range workerDaemonLabels {
		ps = append(ps, legacyAgentPlist(opts, l))
	}
	return ps
}

// removePathsStep removes files or whole directories. os.RemoveAll never follows a symlink, so a
// path that is one loses only the link.
func removePathsStep(name, what string, paths ...string) Step {
	return Step{
		Name:        "remove:" + name,
		Description: fmt.Sprintf("Remove %s: %s.", what, strings.Join(paths, ", ")),
		Check:       func(context.Context) (bool, error) { return pathsGone(paths...), nil },
		Apply:       func(context.Context) error { return removePaths(paths...) },
	}
}

func removeRegistryMarkersStep(opts Options) Step {
	glob := filepath.Join(opts.ConfigDir(), "registry-login.*")
	markers := func() []string { m, _ := filepath.Glob(glob); return m }
	return Step{
		Name:        "remove:registry-login-markers",
		Description: fmt.Sprintf("Remove the registry login markers (%s).", glob),
		Check:       func(context.Context) (bool, error) { return len(markers()) == 0, nil },
		Apply:       func(context.Context) error { return removePaths(markers()...) },
	}
}

// removeOrchardSymlinkStep removes ~/.local/bin/orchard only while it is the symlink into the worker
// app the install made; an orchard binary of the operator's own stays.
func removeOrchardSymlinkStep(opts Options) Step {
	link := filepath.Join(orchardBinDir(opts), orchardBinName)
	ours := func() bool {
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return false
		}
		target, err := os.Readlink(link)
		return err == nil && target == workerOrchardExecutable(opts)
	}
	return Step{
		Name:        "remove:orchard-symlink",
		Description: fmt.Sprintf("Remove %s, the symlink into the worker app (left alone if it is anything else).", link),
		Check:       func(context.Context) (bool, error) { return !ours(), nil },
		Apply: func(context.Context) error {
			if !ours() {
				return nil
			}
			return removePaths(link)
		},
	}
}

func removeWorkerLogsStep(opts Options) Step {
	var logs []string
	for _, n := range workerLogNames {
		logs = append(logs, filepath.Join(opts.LogDir(), n))
	}
	return Step{
		Name:        "remove:worker-logs",
		Description: fmt.Sprintf("Remove the worker and broker logs under %s (and the directory, once empty).", opts.LogDir()),
		Check:       func(context.Context) (bool, error) { return pathsGone(logs...), nil },
		Apply: func(context.Context) error {
			if err := removePaths(logs...); err != nil {
				return err
			}
			_ = os.Remove(opts.LogDir()) // fails, and stays, while anything else is in it
			return nil
		},
	}
}

func purgeConfigStep(opts Options) Step {
	cfg := configFilePath(opts)
	return Step{
		Name:        "remove:config",
		Description: fmt.Sprintf("Remove %s (--purge), and %s once it is empty.", cfg, opts.ConfigDir()),
		Check:       func(context.Context) (bool, error) { return pathsGone(cfg), nil },
		Apply: func(context.Context) error {
			if err := removePaths(cfg); err != nil {
				return err
			}
			_ = os.Remove(opts.ConfigDir()) // kept while anything else is in it
			return nil
		},
	}
}

func pathsGone(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func removePaths(paths ...string) error {
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}
