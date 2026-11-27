package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The root keeper: one root LaunchDaemon that does a worker Mac's root steps from then on, so an
// admin types a password once, ever. Without it every grove release that changes something root
// owns (the VM firewall's rules, the worker daemons' plists) waits for an admin to run setup
// again, and the Mac runs on the old copy until someone does.
//
// It is `grove setup --user <user>`'s root half on a timer, nothing more: the keeper's own binary
// renders the root script for the worker user (PendingRootSteps, the same rendering and SHA-256
// pins an admin's grove uses) and runs it. Nothing the worker user's process prints or writes
// becomes root script text. Its binary lives in a root-owned directory and is replaced only by a
// release that passes the same checks as every self-update (checksums.txt, Sigstore build
// provenance from this repository's release workflow on main, the version it reports), so the
// worker user can't put code in front of it.
// keeperEUID is os.Geteuid; tests stand in for root.
var keeperEUID = os.Geteuid

const (
	RootKeeperLabel = "com.grove.root-keeper"
	// RootKeeperDir holds the keeper's binary: root's, and nobody else's to write.
	RootKeeperDir = "/usr/local/libexec/grove-root-keeper"
	rootKeeperLog = "/Library/Logs/grove-root-keeper.log"
)

// RootKeeperBin is the keeper's binary.
func RootKeeperBin() string { return filepath.Join(RootKeeperDir, "grove") }

func rootKeeperPlistPath(opts Options) string {
	return filepath.Join(opts.launchDaemonsDir(), RootKeeperLabel+".plist")
}

// renderRootKeeperPlist renders the keeper's LaunchDaemon for user: root's (no UserName), started
// at boot and kept alive, running only the root-owned binary.
func renderRootKeeperPlist(user string) (string, error) {
	if user == "" || user == "root" {
		return "", fmt.Errorf("the root keeper keeps an ordinary worker user's Mac, not root's")
	}
	return RenderLaunchAgent(LaunchAgentSpec{
		Label:   RootKeeperLabel,
		Program: RootKeeperBin(),
		Args:    []string{"root-keeper", "run", "--user", user},
		// No environment from anywhere: the keeper finds what it needs itself.
		Env:        map[string]string{"PATH": "/usr/bin:/bin:/usr/sbin:/sbin"},
		WorkingDir: "/",
		StdoutPath: rootKeeperLog,
		StderrPath: rootKeeperLog,
		KeepAlive:  true,
		RunAtLoad:  true,
	})
}

// rootOwnedExecutable reports whether path is a regular file owned by root, writable by nobody
// else, in a directory with the same property.
func rootOwnedExecutable(path string) bool {
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Lstat(p)
		if err != nil {
			return false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		if p == path && !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// fileSHA256 is the hex SHA-256 of path's bytes.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// rootKeeperStep installs the keeper: the grove binary this plan runs as (pinned by its SHA-256 at
// plan time, so a binary swapped in between plan and root script is refused) copied into the
// root-owned directory unless one is there already (the keeper updates its own copy; reinstalling
// would only go back a version), and the LaunchDaemon, written by the root script itself.
func rootKeeperStep(r Runner, opts Options) Step {
	user, _ := opts.workerUser()
	installed := rootKeeperPlistPath(opts)
	return Step{
		Name:       "root-keeper",
		Privileged: true,
		Description: "Install the root keeper (" + RootKeeperLabel + "): from now on it does this Mac's root steps itself " +
			"(a newer VM firewall, the worker daemons), so no admin has to run setup again after an upgrade.",
		Root: func(ctx context.Context) (string, error) { return rootKeeperRoot(opts, user, installed) },
		Check: func(ctx context.Context) (bool, error) {
			want, err := renderRootKeeperPlist(user)
			if err != nil {
				return false, err
			}
			same, err := fileHasContent(installed, func() (string, error) { return want, nil })(ctx)
			if err != nil || !same {
				return false, err
			}
			if !rootOwnedExecutable(RootKeeperBin()) {
				return false, nil
			}
			_, _, err = r.Run(ctx, "launchctl", "print", "system/"+RootKeeperLabel)
			return err == nil, nil
		},
		// Only ever applied by a process that is root already, so the root text runs as it is.
		Apply: func(ctx context.Context) error {
			script, err := rootKeeperRoot(opts, user, installed)
			if err != nil {
				return err
			}
			_, stderr, err := r.Run(ctx, "/bin/sh", "-e", "-c", script)
			if err != nil {
				return fmt.Errorf("install the root keeper: %w %s", err, strings.TrimSpace(stderr))
			}
			return nil
		},
	}
}

// rootKeeperRoot renders the keeper step's root text. The binary to copy is this process's own
// (the grove rendering the root script: an admin's, or root's), pinned by its SHA-256 now. When it
// can't be read, the script installs no binary and stops unless the keeper already has one.
func rootKeeperRoot(opts Options, user, installed string) (string, error) {
	plist, err := renderRootKeeperPlist(user)
	if err != nil {
		return "", err
	}
	src := opts.rootKeeperSource
	if src == "" {
		src, _ = os.Executable()
	}
	var sum string
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		if s, err := fileSHA256(resolved); err == nil {
			src, sum = resolved, s
		}
	}
	return rootKeeperRootScript(src, sum, installed, plist), nil
}

// rootKeeperRootScript is the keeper step's root text.
func rootKeeperRootScript(src, sum, installed, plist string) string {
	dir, bin := RootKeeperDir, RootKeeperBin()
	staged := bin + ".new"
	tmpPlist := installed + ".grove-new"
	lines := []string{
		fmt.Sprintf("/bin/mkdir -p %s && /usr/sbin/chown root:wheel %s && /bin/chmod 0755 %s", shQuote(dir), shQuote(dir), shQuote(dir)),
	}
	if sum == "" {
		lines = append(lines, fmt.Sprintf("[ -x %s ] || { echo %s >&2; exit 1; }",
			shQuote(bin), shQuote("grove could not read its own binary to install as the root keeper")))
	} else {
		lines = append(lines, fmt.Sprintf("if [ ! -x %s ]; then", shQuote(bin)),
			fmt.Sprintf("  /usr/bin/install -o root -g wheel -m 0755 %s %s", shQuote(src), shQuote(staged)),
			fmt.Sprintf("  [ \"$(/usr/bin/shasum -a 256 < %s | /usr/bin/cut -d' ' -f1)\" = %s ] || { /bin/rm -f %s; echo %s >&2; exit 1; }",
				shQuote(staged), shQuote(sum), shQuote(staged), shQuote(src+" changed after grove planned this; not installing it as root's")),
			fmt.Sprintf("  /bin/mv -f %s %s", shQuote(staged), shQuote(bin)),
			"fi")
	}
	lines = append(lines,
		fmt.Sprintf("/bin/cat > %s <<'GROVE_ROOT_KEEPER_PLIST'", shQuote(tmpPlist)),
		strings.TrimRight(plist, "\n"),
		"GROVE_ROOT_KEEPER_PLIST",
		fmt.Sprintf("/usr/sbin/chown root:wheel %s && /bin/chmod 0644 %s && /bin/mv -f %s %s", shQuote(tmpPlist), shQuote(tmpPlist), shQuote(tmpPlist), shQuote(installed)),
	)
	lines = append(lines, systemReloadRoot(RootKeeperLabel, installed)...)
	return strings.Join(lines, "\n") + "\n"
}

// RootKeeperOptions are the Options the keeper renders a worker user's root steps with, the way an
// admin's `grove setup --user` repair does: that user's name and home, rootless or not as that
// user's config says, and, for a worker with root daemons, the grove binary and control plane
// their root-owned plists name.
func RootKeeperOptions(ctx context.Context, r Runner, user, home string) (Options, error) {
	opts := Options{Role: RoleWorker, User: user, Home: home}
	if inst, ok := FindInstalledWorker(opts); ok {
		if inst.User != user {
			return Options{}, fmt.Errorf("this Mac's worker daemons run as %s, not %s", inst.User, user)
		}
		opts.ServerURL = inst.ServerURL
	}
	if bin := installedBrokerProgram(opts); bin != "" {
		opts.GroveBin = bin
	}
	rootless, err := WorkerRootless(ctx, r, opts)
	if err != nil {
		return Options{}, err
	}
	// A worker an admin never finished as root daemons is rootless whatever its config says (the
	// user's own install decided so, AutoRootless): the keeper never changes a worker's shape.
	if !rootless && !rootDaemonsPresent(ctx, r, opts, workerDaemons(opts)) {
		rootless = true
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

// KeepRootSteps is one pass of the keeper, run as root: render the worker user's pending root
// steps with this binary and run them as one script, then check them again. It reports the steps
// it ran. The keeper never rewrites its own plist or binary; InstallRootKeeper does.
func KeepRootSteps(ctx context.Context, r Runner, opts Options) ([]string, error) {
	return runRootStepsAsRoot(ctx, r, opts, false)
}

// InstallRootKeeper is `sudo grove root-keeper install --user <user>`: every pending root step for
// that user, the keeper's own install among them, done now as root.
func InstallRootKeeper(ctx context.Context, r Runner, opts Options) ([]string, error) {
	return runRootStepsAsRoot(ctx, r, opts, true)
}

func runRootStepsAsRoot(ctx context.Context, r Runner, opts Options, withKeeper bool) ([]string, error) {
	if keeperEUID() != 0 {
		return nil, fmt.Errorf("the root keeper runs as root")
	}
	steps, err := PendingRootSteps(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	var names []string
	script := "set -e\n"
	for _, s := range steps {
		if s.Name == "root-keeper" && !withKeeper {
			continue // its own plist and binary: never rewritten from inside
		}
		body, err := s.Root(ctx)
		if err != nil {
			return names, fmt.Errorf("root script for %s: %w", s.Name, err)
		}
		names = append(names, s.Name)
		script += "echo " + shQuote("grove: "+s.Name) + "\n" + strings.TrimRight(body, "\n") + "\n"
	}
	if len(names) == 0 {
		return nil, nil
	}
	if _, stderr, err := r.Run(ctx, "/bin/sh", "-e", "-c", script); err != nil {
		return names, fmt.Errorf("root steps %s: %w %s", strings.Join(names, ", "), err, strings.TrimSpace(stderr))
	}
	var ran []Step
	for _, s := range steps {
		if s.Name != "root-keeper" || withKeeper {
			ran = append(ran, s)
		}
	}
	return names, recheckRootSteps(ctx, ran)
}
