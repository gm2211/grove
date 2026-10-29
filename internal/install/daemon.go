package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A worker Mac runs Orchard's worker and grove's credential broker as system LaunchDaemons with a
// UserName, not as LaunchAgents: a daemon starts at boot with nobody logged in, so the Mac serves
// jobs after a reboot until someone signs in, and the install can be run from a `su - <user>`
// shell that has no GUI session to bootstrap an agent into. UserName keeps the processes (and the
// VMs they boot) the user's, with the user's files and no more privilege than an agent had.
//
// Installing a daemon needs root, so it is staged and installed like the VM firewall
// (hostfirewall.go): the user's install writes ConfigDir()/launchd/<label>.plist, and one
// privileged step prints (or, as root with --yes, runs) the command that copies the staged plists
// into /Library/LaunchDaemons as root's and loads them.

// daemon is one of the worker Mac's LaunchDaemons.
type daemon struct {
	Name    string // short, for step names: "orchard-worker", "broker"
	Label   string // launchd label, and the plist's file name
	Program string // what `launchctl print` shows for it once it is loaded
	Render  func() (string, error)
	Logs    []string // files launchd opens as StandardOut/ErrorPath, pre-created as the user
}

// daemonPaths are where a daemon's plist is staged (the user's, for the admin to copy) and where it
// is installed (root's).
func daemonPaths(opts Options, label string) (staged, installed string) {
	return filepath.Join(opts.ConfigDir(), "launchd", label+".plist"),
		filepath.Join(opts.launchDaemonsDir(), label+".plist")
}

// legacyAgentPlist is where earlier groves rendered the same job as a per-user LaunchAgent.
func legacyAgentPlist(opts Options, label string) string {
	return filepath.Join(opts.LaunchAgentsDir(), label+".plist")
}

// daemonPlist renders spec as a LaunchDaemon for the worker user. UserName is never left out
// (a daemon without one runs as root) and never root; the install command repeats the check on the
// root-owned copy before it loads anything.
func daemonPlist(opts Options, spec LaunchAgentSpec) (string, error) {
	user, err := opts.workerUser()
	if err != nil {
		return "", err
	}
	spec.UserName = user
	return RenderLaunchAgent(spec)
}

// workerDaemons are the two daemons a worker Mac runs.
func workerDaemons(opts Options) []daemon {
	wrapper := workerWrapperPath(opts)
	groveBin := groveExecutable(opts)
	logs := func(name string) []string {
		return []string{filepath.Join(opts.LogDir(), name+".log"), filepath.Join(opts.LogDir(), name+".err.log")}
	}
	return []daemon{
		{
			Name: "orchard-worker", Label: orchardWorkerLabel, Program: wrapper, Logs: logs("orchard-worker"),
			Render: func() (string, error) { return workerDaemonPlist(opts, wrapper) },
		},
		{
			Name: "broker", Label: brokerLabel, Program: groveBin, Logs: logs("broker"),
			Render: func() (string, error) { return brokerDaemonPlist(opts, groveBin) },
		},
	}
}

// daemonStageStep renders d's plist into the user's config dir and creates its log files as the
// user (launchd would otherwise create them as root, and the daemon could not append to them).
func daemonStageStep(opts Options, d daemon) Step {
	staged, installed := daemonPaths(opts, d.Label)
	return Step{
		Name: "daemon-stage:" + d.Name,
		Description: fmt.Sprintf("Render %s, the LaunchDaemon (%s, running as the installing user) that the worker-daemons step installs as %s; logs under %s.",
			staged, d.Label, installed, opts.LogDir()),
		Check: func(ctx context.Context) (bool, error) {
			for _, l := range d.Logs {
				if _, err := os.Stat(l); err != nil {
					return false, nil
				}
			}
			return fileHasContent(staged, d.Render)(ctx)
		},
		Apply: func(ctx context.Context) error {
			content, err := d.Render()
			if err != nil {
				return err
			}
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
			return writeFile(staged, content)
		},
	}
}

// daemonsInstallCommand is the one command an admin runs to install and (re)load the daemons: for
// each, copy the staged plist into place as root's and check the copy runs as the worker user (the
// copy is root's, so nothing can change it between the check and the load; a failing check removes
// it), then boot out the same job if it was a LaunchAgent in the user's session or already a
// daemon, bootstrap it into the system domain, and remove the user's old agent plist so it can't
// run a second worker at the next login.
func daemonsInstallCommand(opts Options, user string, ds []daemon) string {
	uid := os.Getuid()
	var installs, loads []string
	for _, d := range ds {
		staged, installed := daemonPaths(opts, d.Label)
		installs = append(installs, fmt.Sprintf("sudo install -o root -g wheel -m 0644 %q %q && "+
			"{ [ \"$(/usr/libexec/PlistBuddy -c 'Print :UserName' %q)\" = %q ] || { sudo rm -f %q; false; }; }",
			staged, installed, installed, user, installed))
		loads = append(loads, fmt.Sprintf("{ sudo launchctl bootout gui/%d/%s 2>/dev/null; sudo launchctl bootout system/%s 2>/dev/null; "+
			"sudo launchctl bootstrap system %q && sudo rm -f %q; }",
			uid, d.Label, d.Label, installed, legacyAgentPlist(opts, d.Label)))
	}
	return strings.Join(append(installs, loads...), " && ")
}

// daemonLoaded reports whether launchd has d loaded in the system domain running its program.
func daemonLoaded(ctx context.Context, r Runner, d daemon) bool {
	stdout, _, err := r.Run(ctx, "launchctl", "print", "system/"+d.Label)
	return err == nil && strings.Contains(stdout, d.Program)
}

// workerDaemonsStep installs and loads the daemons. It is privileged, so without root and --yes it
// is printed for an admin, like pmset and the VM firewall. Its check compares what is installed
// with what grove renders, never with the staged copy.
func workerDaemonsStep(r Runner, opts Options, ds []daemon) Step {
	user, _ := opts.workerUser() // BuildPlan has already refused a root user
	return Step{
		Name:       "worker-daemons",
		Privileged: true,
		Description: daemonsInstallCommand(opts, user, ds) +
			"   # runs the worker and the broker at boot as " + user + ", with nobody logged in; redo after an upgrade that changes them",
		Check: func(ctx context.Context) (bool, error) {
			for _, d := range ds {
				_, installed := daemonPaths(opts, d.Label)
				same, err := fileHasContent(installed, d.Render)(ctx)
				if err != nil || !same {
					return false, err
				}
				if !daemonLoaded(ctx, r, d) {
					return false, nil
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context) error {
			for _, d := range ds {
				staged, installed := daemonPaths(opts, d.Label)
				if _, _, err := r.Run(ctx, "sudo", "install", "-o", "root", "-g", "wheel", "-m", "0644", staged, installed); err != nil {
					return err
				}
				got, _, err := r.Run(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :UserName", installed)
				if err != nil || strings.TrimSpace(got) != user || user == "" || user == "root" {
					_, _, _ = r.Run(ctx, "sudo", "rm", "-f", installed)
					return fmt.Errorf("%s does not run as %q; removed it rather than load a daemon that might run as root", installed, user)
				}
			}
			for _, d := range ds {
				_, installed := daemonPaths(opts, d.Label)
				// Not loaded is fine for both bootouts: the job may never have been an agent here.
				_, _, _ = r.Run(ctx, "sudo", "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), d.Label))
				_, _, _ = r.Run(ctx, "sudo", "launchctl", "bootout", "system/"+d.Label)
				if _, _, err := r.Run(ctx, "sudo", "launchctl", "bootstrap", "system", installed); err != nil {
					return err
				}
				_, _, _ = r.Run(ctx, "sudo", "rm", "-f", legacyAgentPlist(opts, d.Label))
			}
			return nil
		},
	}
}

// retireLaunchAgentsStep removes the per-user LaunchAgents earlier groves installed for the same
// jobs, once the daemon that replaces each is loaded: removing one first would leave the Mac with
// no worker until an admin runs the daemons step.
func retireLaunchAgentsStep(r Runner, opts Options, ds []daemon) Step {
	return Step{
		Name:        "retire-launchagents",
		Description: "Unload and remove the per-user LaunchAgents (com.grove.orchard-worker, com.grove.broker) an earlier grove installed, once their LaunchDaemons are loaded.",
		Check: func(ctx context.Context) (bool, error) {
			for _, d := range ds {
				if _, err := os.Lstat(legacyAgentPlist(opts, d.Label)); err == nil {
					return false, nil
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context) error {
			for _, d := range ds {
				plist := legacyAgentPlist(opts, d.Label)
				if _, err := os.Lstat(plist); err != nil || !daemonLoaded(ctx, r, d) {
					continue
				}
				_, _, _ = r.Run(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), d.Label)) // no GUI session is fine
				if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		},
	}
}

// daemonsRemediation is what `grove doctor` tells an operator to do about a daemon that isn't
// installed or loaded.
func daemonsRemediation(opts Options) string {
	user, _ := opts.workerUser()
	return "run `grove install --role worker` and do its printed worker-daemons step: " + daemonsInstallCommand(opts, user, workerDaemons(opts))
}

// checkWorkerDaemon reports on the Orchard worker's LaunchDaemon: that it is the one this grove
// renders (so it runs as the worker user), that launchd has it loaded, and that no per-user
// LaunchAgent for the same job is left to start a second worker at the next login.
func checkWorkerDaemon(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "worker-daemon"
	user, err := opts.workerUser()
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	d := workerDaemons(opts)[0]
	_, installed := daemonPaths(opts, d.Label)
	same, err := fileHasContent(installed, d.Render)(ctx)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	if !same {
		detail := installed + " is not the one this grove renders"
		if _, statErr := os.Stat(installed); statErr != nil {
			detail = "not installed as a LaunchDaemon, so the worker stops when " + user + " logs out and does not start at boot"
		}
		return CheckResult{Name: name, OK: false, Detail: detail, Remediation: daemonsRemediation(opts)}
	}
	if !daemonLoaded(ctx, r, d) {
		return CheckResult{Name: name, OK: false, Detail: d.Label + " is not loaded in the system domain", Remediation: daemonsRemediation(opts)}
	}
	if _, err := os.Stat(legacyAgentPlist(opts, d.Label)); err == nil {
		return CheckResult{Name: name, OK: false,
			Detail:      legacyAgentPlist(opts, d.Label) + " is still there and would start a second worker at login",
			Remediation: "run `grove install --role worker`, which removes it now that the daemon is loaded"}
	}
	return CheckResult{Name: name, OK: true, Detail: d.Label + " runs at boot as " + user}
}

var daemonPIDRE = regexp.MustCompile(`(?m)^\s*pid = (\d+)\s*$`)

// restartDaemon makes a running daemon start over onto new files or a new credential: SIGTERM, and
// its KeepAlive restarts it. (`launchctl kickstart` on a system job needs root; the process is the
// user's, so the user can signal it.) A daemon that isn't loaded or isn't running is left alone:
// the install loads it, or launchd starts it, with the new files anyway.
func restartDaemon(ctx context.Context, r Runner, label string) {
	stdout, _, err := r.Run(ctx, "launchctl", "print", "system/"+label)
	if err != nil {
		return
	}
	if m := daemonPIDRE.FindStringSubmatch(stdout); m != nil {
		_, _, _ = r.Run(ctx, "kill", "-TERM", m[1])
	}
}
