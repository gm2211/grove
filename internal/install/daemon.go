package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/url"
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
// privileged step runs (or, without sudo, prints for an admin) the script that copies the staged plists
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

// plistSHA256 is the hex SHA-256 of the plist grove renders for d, or "" if it can't render one.
func plistSHA256(d daemon) string {
	plist, err := d.Render()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(plist))
	return hex.EncodeToString(sum[:])
}

// daemonsRootScript is the root script that installs and (re)loads the daemons: for each, copy the
// staged plist into place as root's and check the copy is byte-identical to the plist grove renders
// (its SHA-256 is in the script). The staged file belongs to the worker user, who could add keys
// such as GroupName or a different Label, or make it a symlink to a file only root can read; the
// root-owned copy is 0600 until it passes the check (so nothing it was pointed at is readable),
// can't change between the check and the load, and a failing check removes it and stops the script. Then boot out the same job if it
// was a LaunchAgent in the user's session or already a daemon, bootstrap it into the system
// domain, and remove the user's old agent plist so it can't run a second worker at the next login.
func daemonsRootScript(opts Options, user string, ds []daemon) (string, error) {
	if user == "" || user == "root" {
		return "", fmt.Errorf("the worker user must not be root")
	}
	uid := opts.workerUID()
	var installs, loads []string
	for _, d := range ds {
		staged, installed := daemonPaths(opts, d.Label)
		sum := plistSHA256(d)
		if len(sum) != 64 {
			return "", fmt.Errorf("cannot render the %s plist to pin it", d.Label)
		}
		installs = append(installs,
			fmt.Sprintf("/usr/bin/install -o root -g wheel -m 0600 %s %s", shQuote(staged), shQuote(installed)),
			fmt.Sprintf("[ \"$(/usr/bin/shasum -a 256 < %s | /usr/bin/cut -d' ' -f1)\" = %s ] || { /bin/rm -f %s; echo %s >&2; exit 1; }",
				shQuote(installed), shQuote(sum), shQuote(installed),
				shQuote(installed+" is not the plist grove renders for "+user+"; removed it rather than load it")),
			fmt.Sprintf("/bin/chmod 0644 %s", shQuote(installed)))
		loads = append(loads,
			fmt.Sprintf("/bin/launchctl bootout gui/%d/%s 2>/dev/null || true", uid, d.Label),
			fmt.Sprintf("/bin/launchctl bootout system/%s 2>/dev/null || true", d.Label),
			fmt.Sprintf("/bin/launchctl bootstrap system %s", shQuote(installed)),
			fmt.Sprintf("/bin/rm -f %s", shQuote(legacyAgentPlist(opts, d.Label))))
	}
	return strings.Join(append(installs, loads...), "\n") + "\n", nil
}

// daemonLoaded reports whether launchd has d loaded in the system domain running its program.
func daemonLoaded(ctx context.Context, r Runner, d daemon) bool {
	stdout, _, err := r.Run(ctx, "launchctl", "print", "system/"+d.Label)
	return err == nil && strings.Contains(stdout, d.Program)
}

// workerDaemonsStep installs and loads the daemons. It is privileged, so without root and --yes it
// is run as part of the root script, like pmset and the VM firewall. Its check compares what is installed
// with what grove renders, never with the staged copy.
func workerDaemonsStep(r Runner, opts Options, ds []daemon) Step {
	user, _ := opts.workerUser() // BuildPlan has already refused a root user
	return Step{
		Name:        "worker-daemons",
		Privileged:  true,
		Description: "Install the worker and broker LaunchDaemons as root's, pinned by SHA-256, and load them (they run at boot as " + user + ", with nobody logged in).",
		Root:        func(context.Context) (string, error) { return daemonsRootScript(opts, user, ds) },
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
				want, err := d.Render()
				got, readErr := os.ReadFile(installed) // root's copy, world-readable
				if err != nil || readErr != nil || string(got) != want || user == "" || user == "root" {
					_, _, _ = r.Run(ctx, "sudo", "rm", "-f", installed)
					return fmt.Errorf("%s is not the plist grove renders for %s (the staged copy was changed?); removed it rather than load it", installed, user)
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
	return "run `grove setup` (or `grove install --role worker`) again: it runs the worker-daemons root step with one sudo prompt, or prints one block for an admin"
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

// InstalledWorker is what the worker's root-owned LaunchDaemons say about the worker installed on
// this Mac: the user it runs as, and the control plane its broker talks to.
type InstalledWorker struct {
	User      string
	ServerURL string // "" for a broker installed without --server (a control plane from before it)
}

var (
	plistUserNameRE = regexp.MustCompile(`<key>UserName</key>\s*<string>([^<]*)</string>`)
	plistServerRE   = regexp.MustCompile(`<string>--server</string>\s*<string>([^<]*)</string>`)
)

// FindInstalledWorker reads the installed (not staged) Orchard worker and broker LaunchDaemons.
// Only root writes /Library/LaunchDaemons, and only after a plist passed its SHA-256 pin, so an
// admin's grove can rely on them for another user without reading anything that user can change.
// ok is false unless both are installed, for the same, non-root user.
func FindInstalledWorker(opts Options) (InstalledWorker, bool) {
	var users []string
	var server string
	for _, label := range []string{orchardWorkerLabel, brokerLabel} {
		_, installed := daemonPaths(opts, label)
		b, err := os.ReadFile(installed)
		if err != nil {
			return InstalledWorker{}, false
		}
		m := plistUserNameRE.FindSubmatch(b)
		if m == nil {
			return InstalledWorker{}, false
		}
		users = append(users, html.UnescapeString(string(m[1])))
		if label == brokerLabel {
			if m := plistServerRE.FindSubmatch(b); m != nil {
				server = html.UnescapeString(string(m[1]))
			}
		}
	}
	if users[0] == "" || users[0] == "root" || users[0] != users[1] {
		return InstalledWorker{}, false
	}
	if server != "" {
		if u, err := url.Parse(server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return InstalledWorker{}, false
		}
	}
	return InstalledWorker{User: users[0], ServerURL: server}, true
}
