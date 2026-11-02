package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
)

// A rootless worker is installed and run entirely by its own unprivileged user, with no sudo and
// nothing for an admin to paste. Everything the default install does as root is replaced by
// something the user can do alone, or is left out and said so:
//
//	worker-daemons    -> worker-agents: the same two jobs as LaunchAgents in ~/Library/LaunchAgents
//	disable-sleep     -> keep-awake: a LaunchAgent running caffeinate (or an existing one of the user's)
//	host-firewall     -> skipped: pf needs root, so VMs are not fenced off the host's ports and LAN
//	softnet-root      -> skipped: setuid root, so isolated network pools are unavailable
//
// The cost is that a LaunchAgent only runs while its user has a GUI login session, so the Mac must
// keep that user logged in (auto-login, or a FileVault unlock at boot); a LaunchDaemon needed
// nobody logged in. See docs/INSTALL.md, "A worker with no root".

const (
	// keepAwakeLabel is the LaunchAgent that keeps a rootless worker's Mac from idle-sleeping.
	keepAwakeLabel = "com.grove.keepawake"
	// caffeinateBin is an absolute path: launchd starts jobs with a minimal PATH.
	caffeinateBin = "/usr/bin/caffeinate"

	rootlessFirewallSkip = "needs root (pf), so it is not installed: VMs on this Mac are NOT fenced off the host's other ports and your LAN"
	rootlessSoftnetSkip  = "needs root (setuid softnet), so it is not done: fleet.yaml pools with network: isolated can't run on this Mac"
)

// agentRecheckDelay is how long to wait between looks at a LaunchAgent that was just loaded: launchd
// starts a RunAtLoad job a moment after bootstrap returns.
var agentRecheckDelay = time.Second

// rootlessLost is what a rootless install gives up, for the notice AutoRootless prints.
const rootlessLost = `  - No VM firewall (host-firewall): VMs on this Mac are not fenced off the host's other ports and your LAN.
  - No isolated network pools (softnet-root): fleet.yaml pools with network: isolated can't run here.
  - The worker runs only while this user is logged in at the Mac (LaunchAgents need a GUI session):
    turn on automatic login, or unlock FileVault and log in after a reboot.`

// applyRootlessConfig turns Rootless on for an install of a worker that config.yaml records as
// rootless, so re-runs stay rootless without the flag. --rootless=false (NoAutoRootless) wins.
func applyRootlessConfig(opts Options) Options {
	if opts.Rootless || opts.NoAutoRootless {
		return opts
	}
	if cfg, err := config.LoadFrom(configFilePath(opts)); err == nil && cfg.Worker.Rootless {
		opts.Rootless = true
	}
	return opts
}

// AutoRootless decides whether `grove install --role worker` should be rootless without being
// asked: the user running it is not an admin (so cannot use sudo), and this Mac's worker is not
// already installed as LaunchDaemons (an install that an admin finished with the root block keeps
// that shape). When it says yes it tells the user what that costs.
func AutoRootless(ctx context.Context, r Runner, opts Options, out io.Writer) bool {
	if opts.Rootless || opts.NoAutoRootless || opts.goos() != "darwin" {
		return false
	}
	if _, err := os.Stat(filepath.Join(opts.launchDaemonsDir(), orchardWorkerLabel+".plist")); err == nil {
		return false
	}
	if userIsAdmin(ctx, r) {
		return false
	}
	fmt.Fprintf(out, "This user is not an admin, so it has no sudo: installing the worker rootless (--rootless), as per-user LaunchAgents.\nWhat that gives up:\n%s\nPass --rootless=false to use the root-based install instead (an admin runs one block).\n\n", rootlessLost)
	return true
}

// agentDomain is the launchd domain of the worker user's GUI session.
func agentDomain(opts Options) string { return fmt.Sprintf("gui/%d", opts.workerUID()) }

// agentPlistPath is where the LaunchAgent for label lives: the same file an earlier grove put its
// per-user worker in, so an old-shape agent is replaced in place.
func agentPlistPath(opts Options, label string) string { return legacyAgentPlist(opts, label) }

// agentPrint is `launchctl print` for label in the worker user's GUI domain.
func agentPrint(ctx context.Context, r Runner, opts Options, label string) (string, bool) {
	stdout, _, err := r.Run(ctx, "launchctl", "print", agentDomain(opts)+"/"+label)
	return stdout, err == nil
}

// agentLoaded reports whether launchd has d loaded in the user's GUI domain, running its program.
func agentLoaded(ctx context.Context, r Runner, opts Options, d daemon) bool {
	stdout, ok := agentPrint(ctx, r, opts, d.Label)
	return ok && strings.Contains(stdout, d.Program)
}

// agentRunning reports whether `launchctl print` output shows a live process.
func agentRunning(printOut string) bool {
	return daemonPIDRE.MatchString(printOut) || strings.Contains(printOut, "state = running")
}

// unitLoaded is whether the worker's job d is loaded the way this install runs it: a LaunchAgent in
// the user's GUI domain when rootless, else a LaunchDaemon in the system domain.
func unitLoaded(ctx context.Context, r Runner, opts Options, d daemon) bool {
	if opts.Rootless {
		return agentLoaded(ctx, r, opts, d)
	}
	return daemonLoaded(ctx, r, d)
}

// loadAgent (re)loads the LaunchAgent at plist into the user's GUI domain: boot out whatever has
// the label (an old-shape job, or a previous run; not loaded is fine), then bootstrap the file.
// launchd finishes a bootout asynchronously, so a bootstrap straight after it may be refused.
func loadAgent(ctx context.Context, r Runner, opts Options, label, plist string) error {
	domain := agentDomain(opts)
	_, _, _ = r.Run(ctx, "launchctl", "bootout", domain+"/"+label)
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(agentRecheckDelay)
		}
		if _, _, err = r.Run(ctx, "launchctl", "bootstrap", domain, plist); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl bootstrap %s %s: %w (a LaunchAgent needs %s logged in at the Mac)", domain, plist, err, opts.userLabel())
}

func (o Options) userLabel() string {
	if u, err := o.workerUser(); err == nil {
		return u
	}
	return "its user"
}

// writeAgentPlist installs content as the LaunchAgent plist at path (0644, the user's own).
func writeAgentPlist(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// ensureDaemonLogs creates d's log files as the user, so launchd opens them as the user's.
func ensureDaemonLogs(opts Options, d daemon) error {
	if err := os.MkdirAll(opts.LogDir(), 0o755); err != nil {
		return err
	}
	for _, l := range d.Logs {
		f, err := os.OpenFile(l, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		f.Close()
	}
	return nil
}

// agentInstalled reports whether the LaunchAgent for d is the one grove renders and is loaded.
func agentInstalled(ctx context.Context, r Runner, opts Options, d daemon) (bool, error) {
	render := func() (string, error) { return RenderLaunchAgent(d.Spec) }
	same, err := fileHasContent(agentPlistPath(opts, d.Label), render)(ctx)
	if err != nil || !same {
		return false, err
	}
	return agentLoaded(ctx, r, opts, d), nil
}

// workerAgentsStep is worker-daemons for a rootless install: render the worker and broker jobs as
// the user's LaunchAgents (no UserName; the same program, arguments, wrapper and log paths as the
// daemons) and load them in gui/<uid>. An agent that already has the label, of this or an older
// shape, is replaced in place. Jobs already right and loaded are left running, so a re-run with
// nothing to change restarts nothing. It fails unless each job is running afterwards.
func workerAgentsStep(r Runner, opts Options, ds []daemon) Step {
	return Step{
		Name: "worker-agents",
		Description: fmt.Sprintf("Install the worker and broker as LaunchAgents in %s (no root; they run while this user is logged in) and load them in %s.",
			opts.LaunchAgentsDir(), agentDomain(opts)),
		Check: func(ctx context.Context) (bool, error) {
			for _, d := range ds {
				ok, err := agentInstalled(ctx, r, opts, d)
				if err != nil || !ok {
					return false, err
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context) error {
			for _, d := range ds {
				if ok, err := agentInstalled(ctx, r, opts, d); err != nil {
					return err
				} else if ok {
					continue
				}
				content, err := RenderLaunchAgent(d.Spec)
				if err != nil {
					return err
				}
				if err := ensureDaemonLogs(opts, d); err != nil {
					return err
				}
				plist := agentPlistPath(opts, d.Label)
				if err := writeAgentPlist(plist, content); err != nil {
					return err
				}
				if err := loadAgent(ctx, r, opts, d.Label, plist); err != nil {
					return err
				}
			}
			for _, d := range ds {
				if err := waitAgentRunning(ctx, r, opts, d); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// waitAgentRunning fails unless launchd shows d running its program within a few seconds.
func waitAgentRunning(ctx context.Context, r Runner, opts Options, d daemon) error {
	var last string
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(agentRecheckDelay)
		}
		stdout, ok := agentPrint(ctx, r, opts, d.Label)
		last = stdout
		if ok && strings.Contains(stdout, d.Program) && agentRunning(stdout) {
			return nil
		}
	}
	if last == "" {
		return fmt.Errorf("%s is not loaded in %s after the install (is %s logged in at the Mac?)", d.Label, agentDomain(opts), opts.userLabel())
	}
	return fmt.Errorf("%s is loaded but not running; see %s", d.Label, strings.Join(d.Logs, " and "))
}

// workerModeStep records the install's mode in config.yaml (worker.rootless) so re-runs keep it.
// A daemon install with no config, or one that never said rootless, has nothing to record.
func workerModeStep(opts Options) Step {
	path := configFilePath(opts)
	desc := "Record worker.rootless in " + path + " so re-runs of the install stay rootless."
	if !opts.Rootless {
		desc = "Clear worker.rootless in " + path + " (the install now uses LaunchDaemons)."
	}
	return Step{
		Name:        "worker-mode",
		Description: desc,
		Check: func(context.Context) (bool, error) {
			cfg, err := config.LoadFrom(path)
			if errors.Is(err, config.ErrNotFound) {
				return !opts.Rootless, nil
			}
			if err != nil {
				return false, err
			}
			return cfg.Worker.Rootless == opts.Rootless, nil
		},
		Apply: func(context.Context) error {
			cfg, err := config.LoadFrom(path)
			if errors.Is(err, config.ErrNotFound) {
				cfg = &config.Config{}
			} else if err != nil {
				return err
			}
			cfg.Worker.Rootless = opts.Rootless
			return config.Save(path, cfg)
		},
	}
}

// keepAwakeSpec is the keep-awake job: caffeinate -i (no idle sleep), -m (no disk idle sleep), -s
// (no system sleep on AC power), for as long as the user's session lasts.
func keepAwakeSpec(opts Options) LaunchAgentSpec {
	return LaunchAgentSpec{
		Label:      keepAwakeLabel,
		Program:    caffeinateBin,
		Args:       []string{"-ims"},
		KeepAlive:  true,
		RunAtLoad:  true,
		StdoutPath: filepath.Join(opts.LogDir(), "keepawake.log"),
		StderrPath: filepath.Join(opts.LogDir(), "keepawake.err.log"),
	}
}

var (
	plistLabelRE    = regexp.MustCompile(`<key>Label</key>\s*<string>([^<]*)</string>`)
	plistProgArgsRE = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`)
	plistStringsRE  = regexp.MustCompile(`<string>([^<]*)</string>`)
	caffeinateFlag  = regexp.MustCompile(`^-[a-z]*[ids][a-z]*$`)
)

// existingKeepAwake finds a LaunchAgent of the user's, not grove's, that already runs caffeinate
// with a flag that holds off sleep (-i, -s or -d) and is loaded: the Studio has one, and a second
// would only duplicate it. It returns that agent's label.
func existingKeepAwake(ctx context.Context, r Runner, opts Options) (string, bool) {
	plists, _ := filepath.Glob(filepath.Join(opts.LaunchAgentsDir(), "*.plist"))
	for _, p := range plists {
		if filepath.Base(p) == keepAwakeLabel+".plist" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		label := plistLabelRE.FindSubmatch(b)
		args := plistProgArgsRE.FindSubmatch(b)
		if label == nil || args == nil {
			continue
		}
		var words []string
		for _, m := range plistStringsRE.FindAllStringSubmatch(string(args[1]), -1) {
			words = append(words, m[1])
		}
		if len(words) == 0 || filepath.Base(words[0]) != "caffeinate" {
			continue
		}
		holdsOff := false
		for _, w := range words[1:] {
			holdsOff = holdsOff || caffeinateFlag.MatchString(w)
		}
		if !holdsOff {
			continue
		}
		if _, loaded := agentPrint(ctx, r, opts, string(label[1])); loaded {
			return string(label[1]), true
		}
	}
	return "", false
}

// keepAwakeStep is disable-sleep for a rootless install: a per-user caffeinate LaunchAgent. It is
// satisfied by an existing caffeinate agent of the user's rather than adding a second.
func keepAwakeStep(r Runner, opts Options) Step {
	spec := keepAwakeSpec(opts)
	plist := agentPlistPath(opts, keepAwakeLabel)
	render := func() (string, error) { return RenderLaunchAgent(spec) }
	ours := func(ctx context.Context) (bool, error) {
		same, err := fileHasContent(plist, render)(ctx)
		if err != nil || !same {
			return false, err
		}
		_, loaded := agentPrint(ctx, r, opts, keepAwakeLabel)
		return loaded, nil
	}
	return Step{
		Name: "keep-awake",
		Description: fmt.Sprintf("Keep this Mac awake while the worker's user is logged in: a LaunchAgent (%s, %s -ims) in place of pmset. An existing caffeinate agent of this user counts as done.",
			keepAwakeLabel, caffeinateBin),
		Check: func(ctx context.Context) (bool, error) {
			if ok, err := ours(ctx); err != nil || ok {
				return ok, err
			}
			_, found := existingKeepAwake(ctx, r, opts)
			return found, nil
		},
		Apply: func(ctx context.Context) error {
			content, err := render()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(opts.LogDir(), 0o755); err != nil {
				return err
			}
			if err := writeAgentPlist(plist, content); err != nil {
				return err
			}
			return loadAgent(ctx, r, opts, keepAwakeLabel, plist)
		},
	}
}

// rootlessFirewallWanted reports whether a rootless install keeps the VM firewall: asked for with
// --firewall, or already installed by an admin (so an upgrade that changes the rules is offered).
func rootlessFirewallWanted(opts Options) bool {
	if opts.Firewall {
		return true
	}
	_, installed := hostFirewallPaths(opts)
	_, err := os.Stat(installed)
	return err == nil
}

// rootlessTailSteps end a rootless plan: the VM firewall when it is wanted (its one privileged step
// goes to an admin, like any root step; the worker stays this user's LaunchAgents), otherwise the
// root-only protections listed as skipped.
func rootlessTailSteps(r Runner, opts Options) []Step {
	steps := rootlessSkipSteps(opts)
	if rootlessFirewallWanted(opts) {
		return append(hostFirewallSteps(r, opts), steps[1:]...)
	}
	return steps
}

// rootlessSkipSteps are the root-only protections a rootless install can't have. Each is listed as
// [skip] with what is lost, so the install never looks as if it did them.
func rootlessSkipSteps(opts Options) []Step {
	steps := []Step{{Name: "host-firewall", SkipReason: rootlessFirewallSkip}}
	if opts.Softnet {
		steps = append(steps, Step{Name: "softnet-root", SkipReason: rootlessSoftnetSkip})
	}
	return steps
}

// checkWorkerAgent is checkWorkerDaemon for a rootless install: the worker LaunchAgent is the one
// this grove renders, loaded in the user's GUI domain, and running.
func checkWorkerAgent(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "worker-agent"
	d := workerDaemons(opts)[0]
	fix := "run `grove install --role worker` again as " + opts.userLabel() + " (rootless: no sudo)"
	same, err := fileHasContent(agentPlistPath(opts, d.Label), func() (string, error) { return RenderLaunchAgent(d.Spec) })(ctx)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	if !same {
		return CheckResult{Name: name, OK: false, Detail: agentPlistPath(opts, d.Label) + " is missing or is not the one this grove renders", Remediation: fix}
	}
	stdout, loaded := agentPrint(ctx, r, opts, d.Label)
	if !loaded || !strings.Contains(stdout, d.Program) {
		return CheckResult{Name: name, OK: false, Detail: d.Label + " is not loaded in " + agentDomain(opts) + ": the worker only runs while " + opts.userLabel() + " is logged in at the Mac", Remediation: fix}
	}
	if !agentRunning(stdout) {
		return CheckResult{Name: name, OK: false, Detail: d.Label + " is loaded but not running", Remediation: "see " + strings.Join(d.Logs, " and ")}
	}
	if _, err := os.Stat(filepath.Join(opts.launchDaemonsDir(), d.Label+".plist")); err == nil {
		return CheckResult{Name: name, OK: false, Detail: "a LaunchDaemon for " + d.Label + " is installed too and would run a second worker", Remediation: "an admin removes it, or re-run the install without --rootless"}
	}
	return CheckResult{Name: name, OK: true, Detail: d.Label + " runs as a LaunchAgent of " + opts.userLabel() + " (needs that user logged in)"}
}

// checkKeepAwake reports whether something holds off sleep for a rootless worker.
func checkKeepAwake(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "keep-awake"
	if stdout, loaded := agentPrint(ctx, r, opts, keepAwakeLabel); loaded && agentRunning(stdout) {
		return CheckResult{Name: name, OK: true, Detail: keepAwakeLabel + " is running caffeinate"}
	}
	if label, ok := existingKeepAwake(ctx, r, opts); ok {
		return CheckResult{Name: name, OK: true, Detail: label + " (an existing caffeinate agent) keeps this Mac awake"}
	}
	return CheckResult{Name: name, OK: false, Detail: "nothing keeps this Mac awake, so a worker Mac that sleeps stops accepting jobs",
		Remediation: "run `grove install --role worker` again as " + opts.userLabel() + ": it installs the " + keepAwakeLabel + " LaunchAgent"}
}

// checkRootlessFirewall is the warning that stands in for host-firewall in a rootless install:
// not a failure, since there is no way to fix it without root.
func checkRootlessFirewall() CheckResult {
	return CheckResult{Name: "host-firewall", OK: true, Warn: true,
		Detail: "not installed (rootless: pf needs root): VMs on this Mac are not fenced off the host's other ports and your LAN"}
}

// removeKeepAwakeStep removes the keep-awake LaunchAgent a rootless install added.
func removeKeepAwakeStep(r Runner, opts Options) Step {
	plist := agentPlistPath(opts, keepAwakeLabel)
	return Step{
		Name:        "remove-keepawake-agent",
		Description: fmt.Sprintf("Unload and remove the keep-awake LaunchAgent (%s).", plist),
		Check:       func(context.Context) (bool, error) { return pathsGone(plist), nil },
		Apply: func(ctx context.Context) error {
			_, _, _ = r.Run(ctx, "launchctl", "bootout", agentDomain(opts)+"/"+keepAwakeLabel) // no GUI session is fine
			return removePaths(plist)
		},
	}
}
