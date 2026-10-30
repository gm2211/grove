package install

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The worker runs Orchard from an app bundle so macOS attributes it to a stable, named
// responsible-code identity. It reaches its VMs through the Tart Guest Agent
// (--guest-agent-transport), not the network, so it needs no Local Network consent. The
// ~/.local/bin entry remains a symlink for operators and scripts that invoke Orchard directly.
const (
	orchardBinName        = "orchard"
	orchardWorkerAppName  = "Grove Orchard Worker.app"
	orchardWorkerBundleID = "com.gm2211.grove.orchard-worker"
	orchardWorkerLabel    = "com.grove.orchard-worker"
	// orchardWorkerKeychainService names the Keychain item an older grove kept the worker's bootstrap
	// credential in. It is only a migration source now: the credential lives in a 0600 file.
	orchardWorkerKeychainService = "com.gm2211.grove.orchard-worker.bootstrap"
)

func workerWrapperPath(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "run-orchard-worker.zsh")
}

func orchardBinDir(opts Options) string { return filepath.Join(opts.homeDir(), ".local", "bin") }

func workerOrchardAppDir(opts Options) string {
	return filepath.Join(opts.homeDir(), "Applications", orchardWorkerAppName)
}

func workerOrchardExecutable(opts Options) string {
	return filepath.Join(workerOrchardAppDir(opts), "Contents", "MacOS", orchardBinName)
}

// workerOrchardCommitPath records which OrchardForkCommit the bundle's binary was built from.
func workerOrchardCommitPath(opts Options) string {
	return filepath.Join(workerOrchardAppDir(opts), "Contents", "Resources", "grove-orchard-commit")
}

func workerOrchardAtPinnedCommit(opts Options) bool {
	got, err := os.ReadFile(workerOrchardCommitPath(opts))
	return err == nil && strings.TrimSpace(string(got)) == OrchardForkCommit
}

func workerOrchardInfoPlistPath(opts Options) string {
	return filepath.Join(workerOrchardAppDir(opts), "Contents", "Info.plist")
}

func workerOrchardInfoPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDisplayName</key>
	<string>Grove Orchard Worker</string>
	<key>CFBundleExecutable</key>
	<string>orchard</string>
	<key>CFBundleIdentifier</key>
	<string>com.gm2211.grove.orchard-worker</string>
	<key>CFBundleName</key>
	<string>Grove Orchard Worker</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSLocalNetworkUsageDescription</key>
	<string>Grove uses Orchard to connect to virtual machines running on this Mac.</string>
</dict>
</plist>
`
}

// buildWorkerSteps assembles the worker role plan. Worker is macOS-only (it runs Tart VMs, which
// need Apple Silicon + Virtualization.framework).
func buildWorkerSteps(r Runner, opts Options, out io.Writer) []Step {
	lookPath := opts.lookPath()
	host := opts.hostname()
	cliOrchard := filepath.Join(orchardBinDir(opts), orchardBinName)
	workerOrchard := workerOrchardExecutable(opts)
	wrapper := workerWrapperPath(opts)

	steps := []Step{
		{
			Name: "homebrew",
			Description: fmt.Sprintf("Install Homebrew from its installer pinned at Homebrew/install@%s, verified against grove's pinned SHA-256 before it runs (NONINTERACTIVE=1).",
				HomebrewInstallCommit[:12]),
			Check: func(ctx context.Context) (bool, error) {
				_, err := lookPath("brew")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				script, err := fetchVerified(ctx, r, opts, homebrewInstallerDownload())
				if err != nil {
					return err
				}
				_, _, err = r.Run(ctx, "/usr/bin/env", "NONINTERACTIVE=1", "/bin/bash", script)
				return err
			},
		},
		trustTapStep(r, tapOpenAITools, out),
		{
			Name:        "tart",
			Description: "Install Tart (`brew install openai/tools/tart`, falling back to `brew install cirruslabs/cli/tart` if that tap doesn't have it).",
			Check: func(ctx context.Context) (bool, error) {
				_, err := lookPath("tart")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
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
		},
	}
	if opts.Softnet {
		steps = append(steps, softnetSteps(r, lookPath, out)...)
	}

	if step := registryLoginStep(r, opts, out); step != nil {
		steps = append(steps, *step)
	}

	steps = append(steps,
		Step{
			Name: "orchard-worker-app",
			Description: fmt.Sprintf(
				"Install the Orchard fork in %s with a stable macOS app identity and Local Network usage description; keep %s as a symlink. Existing Grove Orchard binaries migrate in place; any binary not built from the pinned github.com/gm2211/orchard commit (OrchardForkCommit) is rebuilt from source at that commit.",
				workerOrchardAppDir(opts), cliOrchard,
			),
			Check: func(ctx context.Context) (bool, error) {
				return workerOrchardAppInstalled(ctx, r, opts, cliOrchard)
			},
			Apply: func(ctx context.Context) error {
				return ensureWorkerOrchardApp(ctx, r, opts, cliOrchard, out)
			},
		},
		Step{
			Name:        "worker-bootstrap-credential",
			Description: fmt.Sprintf("Store the worker-only Orchard credential in %s (0600, this user's); it never appears in process arguments, the wrapper or a launchd plist. A re-run without --token keeps it, moving it out of the login Keychain if that is where an older grove put it.", credentialPath(opts, workerBootstrapCredentialName)),
			Check: func(ctx context.Context) (bool, error) {
				return secretFileIs(credentialPath(opts, workerBootstrapCredentialName), opts.Token), nil
			},
			Apply: func(ctx context.Context) error {
				token := opts.Token
				if token == "" {
					// Migrate the credential an older grove kept in the login Keychain. That needs
					// this user's Keychain unlocked, which a `su -` shell leaves locked.
					stdout, _, err := r.Run(ctx, "/usr/bin/security", "find-generic-password", "-a", host, "-s", orchardWorkerKeychainService, "-w")
					token = strings.TrimSpace(stdout)
					if err != nil || token == "" {
						return fmt.Errorf("worker bootstrap credential missing; run `grove setup` (if it is only in the login Keychain, unlock it first with `security unlock-keychain`)")
					}
				}
				if err := secretLine("the worker bootstrap credential", token); err != nil {
					return err
				}
				if err := writeSecretFile(credentialPath(opts, workerBootstrapCredentialName), token+"\n"); err != nil {
					return err
				}
				// A running worker already consumed its previous credential from stdin. Restart it
				// so enrollment uses the replacement immediately; first installs are loaded later.
				restartDaemon(ctx, r, orchardWorkerLabel)
				return nil
			},
		},
		Step{
			Name:        "orchard-worker-wrapper",
			Description: fmt.Sprintf("Render %s; it feeds Orchard the bootstrap credential file over stdin, and exports the registry credentials from their file, if there is one, for Tart.", wrapper),
			Check:       fileHasContent(wrapper, func() (string, error) { return workerWrapper(opts, workerOrchard), nil }),
			Apply: func(ctx context.Context) error {
				if err := writeFile(wrapper, workerWrapper(opts, workerOrchard)); err != nil {
					return err
				}
				if err := os.Chmod(wrapper, 0o700); err != nil {
					return err
				}
				// A running worker keeps the old wrapper's environment until it restarts. Initial
				// installs have nothing running yet.
				restartDaemon(ctx, r, orchardWorkerLabel)
				return nil
			},
		},
	)

	daemons := workerDaemons(opts)
	steps = append(steps, daemonStageStep(opts, daemons[0]))
	steps = append(steps, workerSleepStep(r))
	steps = append(steps, brokerSteps(r, opts, daemons[1])...)
	steps = append(steps, workerDaemonsStep(r, opts, daemons), retireLaunchAgentsStep(r, opts, daemons))
	steps = append(steps, hostFirewallSteps(r, opts)...)
	return steps
}

// registryLoginMarkerPath is where a successful `tart login <registry>` is recorded: "<user>
// <registry>\n", under opts.ConfigDir(). It deliberately never records the token itself — only
// enough to tell whether a *different* --registry-user needs a fresh login.
func registryLoginMarkerPath(opts Options, registry string) string {
	return filepath.Join(opts.ConfigDir(), "registry-login."+registry)
}

// registryLoginStep returns the idempotent "tart login <registry>" step, or nil if
// opts.RegistryToken wasn't given. grove's own worker images (ghcr.io/gm2211/grove-{linux,macos}-
// worker) are private by default — GHCR has no API to flip that, only the "Package settings ->
// Change visibility" UI (see docs/IMAGES.md) — so a worker needs either that flip or credentials
// cached by `tart login` before it can pull them.
//
// The token is fed to `tart login --password-stdin` over stdin via Runner.RunWithStdin — never as
// a command argument, so it can't leak into a Call's rendered command line, a process listing, or
// this step's own Description/logging. When no token is given, grove doesn't touch registry auth
// at all: it just prints a NOTE that the images must be public instead, and buildWorkerSteps omits
// the step entirely (there being nothing for its Check/Apply to do).
func registryLoginStep(r Runner, opts Options, out io.Writer) *Step {
	registry := opts.registry()
	if opts.RegistryToken == "" {
		fmt.Fprintf(out, "        NOTE: no --registry-token (or GROVE_REGISTRY_TOKEN) given; %s "+
			"images must be public or workers will fail to pull them — make the packages public "+
			"(GitHub -> Packages -> grove-*-worker -> Package settings -> Change visibility) or "+
			"run `grove registry login` on this worker.\n", registry)
		return nil
	}

	markerPath := registryLoginMarkerPath(opts, registry)
	marker := opts.RegistryUser + " " + registry + "\n"
	credPath := credentialPath(opts, registryCredentialName)
	credContent := opts.RegistryUser + "\n" + opts.RegistryToken + "\n"

	return &Step{
		Name: "registry-login:" + registry,
		Description: fmt.Sprintf(
			"tart login %s --username %s --password-stdin  (token from --registry-token / GROVE_REGISTRY_TOKEN), and keep the login in %s (0600) for the worker daemon, which can't read this user's Keychain before anyone logs in",
			registry, opts.RegistryUser, credPath,
		),
		Check: func(ctx context.Context) (bool, error) {
			got, err := os.ReadFile(markerPath)
			if err != nil {
				if os.IsNotExist(err) {
					return false, nil
				}
				return false, err
			}
			if string(got) != marker {
				return false, nil
			}
			cred, err := readSecretFileRaw(credPath)
			return err == nil && cred == credContent, nil
		},
		Apply: func(ctx context.Context) error {
			if opts.RegistryUser == "" {
				return fmt.Errorf("registry-login: --registry-user is required when --registry-token is set")
			}
			if err := secretLine("the registry username", opts.RegistryUser); err != nil {
				return err
			}
			if err := secretLine("the registry token", opts.RegistryToken); err != nil {
				return err
			}
			// `tart login` keeps the login in the Keychain, which serves a person at the Mac. The
			// worker daemon starts before anyone has logged in, so it gets the same login from a
			// file, through the TART_REGISTRY_* variables its wrapper exports.
			if _, _, err := r.RunWithStdin(ctx, opts.RegistryToken, "tart", "login", registry,
				"--username", opts.RegistryUser, "--password-stdin"); err != nil {
				if !isKeychainUnavailable(err) {
					return fmt.Errorf("tart login %s: %w", registry, err)
				}
				// No Keychain session (a `su - user` shell, ssh): the Keychain copy is only a
				// convenience for someone at this Mac, so the file below is enough for the daemon.
				fmt.Fprintf(out, "        WARNING: tart login could not use the Keychain here (no GUI session); "+
					"keeping the %s login in %s only, which is what the worker daemon reads.\n", registry, credPath)
			}
			if err := writeSecretFile(credPath, credContent); err != nil {
				return err
			}
			if err := os.MkdirAll(opts.ConfigDir(), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(markerPath, []byte(marker), 0o644); err != nil {
				return err
			}
			// The running worker's environment predates this login.
			restartDaemon(ctx, r, orchardWorkerLabel)
			return nil
		},
	}
}

// isKeychainUnavailable reports whether a `tart login` failure is the Keychain refusing to
// work in this session ("User interaction is not allowed"), as opposed to the registry rejecting
// the credentials. Only the former is survivable: the daemon reads the login from a file.
func isKeychainUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "User interaction is not allowed") ||
		(strings.Contains(msg, "Keychain") && strings.Contains(msg, "failed to add item"))
}

// RegistryLogin is `grove registry login`: it runs registryLoginStep's Apply even when the marker
// already matches, because the reason to run it on an installed worker is usually a new or
// rotated token for the same user. Re-running `grove install --role worker` for that would need
// the worker's controller URL and bootstrap credential again.
func RegistryLogin(ctx context.Context, r Runner, opts Options) error {
	if opts.RegistryToken == "" {
		return errors.New("registry login: no token given")
	}
	return registryLoginStep(r, opts, os.Stderr).Apply(ctx)
}

// workerDaemonPlist renders the LaunchDaemon, run as the worker user, that supervises `orchard
// worker run`.
func workerDaemonPlist(opts Options, wrapper string) (string, error) {
	return daemonPlist(opts, LaunchAgentSpec{
		Label:                       orchardWorkerLabel,
		Program:                     wrapper,
		WorkingDir:                  opts.homeDir(),
		AssociatedBundleIdentifiers: []string{orchardWorkerBundleID},
		KeepAlive:                   true,
		RunAtLoad:                   true,
		StdoutPath:                  filepath.Join(opts.LogDir(), "orchard-worker.log"),
		StderrPath:                  filepath.Join(opts.LogDir(), "orchard-worker.err.log"),
	})
}

// installedWorkerController is the controller URL in this Mac's existing worker wrapper, or "" if
// there is none or it can't be read. It lets `grove install --role worker` re-run (to upgrade, or
// to add a registry login) without the --controller that `grove setup` supplied the first time.
func installedWorkerController(opts Options) string {
	b, err := os.ReadFile(workerWrapperPath(opts))
	if err != nil {
		return ""
	}
	const flag = "--bootstrap-token-stdin "
	i := strings.Index(string(b), flag)
	if i < 0 {
		return ""
	}
	quoted, err := strconv.QuotedPrefix(string(b)[i+len(flag):])
	if err != nil {
		return ""
	}
	controller, err := strconv.Unquote(quoted)
	if err != nil {
		return ""
	}
	return controller
}

// installedWorkerName is the Orchard worker name this Mac already runs under, or "" if there is
// none: the --name in grove's worker wrapper, else in a LaunchAgent written before the wrapper
// existed (which ran Orchard with its arguments inline). Re-running the install keeps that name,
// so fleet.yaml's workerSelector and the VMs pinned to it still match, even when it isn't this
// Mac's hostname.
func installedWorkerName(opts Options) string {
	if b, err := os.ReadFile(workerWrapperPath(opts)); err == nil {
		const flag = "--name "
		if i := strings.Index(string(b), flag); i >= 0 {
			if quoted, err := strconv.QuotedPrefix(string(b)[i+len(flag):]); err == nil {
				if name, err := strconv.Unquote(quoted); err == nil {
					return name
				}
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(opts.LaunchAgentsDir(), "com.grove.orchard-worker.plist"))
	if err != nil {
		return ""
	}
	if m := legacyWorkerNameRE.FindSubmatch(b); m != nil {
		return html.UnescapeString(string(m[1]))
	}
	return ""
}

// legacyWorkerNameRE finds `--name <name>` or `--name=<name>` in a LaunchAgent's
// ProgramArguments.
var legacyWorkerNameRE = regexp.MustCompile(`<string>--name(?:</string>\s*<string>|=)([^<]+)</string>`)

// workerWrapper is what the LaunchDaemon runs. It holds no secret: it points Orchard's stdin at the
// bootstrap credential file, and, when there is a registry login, reads the registry file's two
// lines (user, token) into the variables Tart takes registry credentials from, so neither is ever
// in argv or in this text. HOME is set by launchd for a daemon with a UserName.
func workerWrapper(opts Options, orchardBin string) string {
	return fmt.Sprintf(`#!/bin/zsh
set -eu
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
registry_credential=%q
if [ -f "$registry_credential" ]; then
  { IFS= read -r TART_REGISTRY_USERNAME; IFS= read -r TART_REGISTRY_PASSWORD; } < "$registry_credential"
  export TART_REGISTRY_USERNAME TART_REGISTRY_PASSWORD
fi
exec %q worker run --name %q --labels %q --guest-agent-transport --bootstrap-token-stdin %q < %q
`, credentialPath(opts, registryCredentialName), orchardBin, opts.hostname(), fmt.Sprintf("host=%s,arch=arm64", opts.hostname()), opts.Controller,
		credentialPath(opts, workerBootstrapCredentialName))
}

func workerOrchardAppInstalled(ctx context.Context, r Runner, opts Options, cliPath string) (bool, error) {
	executable := workerOrchardExecutable(opts)
	if info, err := os.Stat(executable); err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if !workerOrchardAtPinnedCommit(opts) {
		return false, nil
	}
	gotPlist, err := os.ReadFile(workerOrchardInfoPlistPath(opts))
	if err != nil || string(gotPlist) != workerOrchardInfoPlist() {
		return false, nil
	}
	target, err := os.Readlink(cliPath)
	if err != nil || target != executable {
		return false, nil
	}
	_, _, err = r.Run(ctx, "/usr/bin/codesign", "--verify", "--strict", workerOrchardAppDir(opts))
	return err == nil, nil
}

func ensureWorkerOrchardApp(ctx context.Context, r Runner, opts Options, cliPath string, out io.Writer) error {
	appDir := workerOrchardAppDir(opts)
	executable := workerOrchardExecutable(opts)
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		return err
	}

	// Migrate an existing Grove-managed binary into the bundle. This preserves the exact fork
	// build already running on an installed worker and avoids an unnecessary network/build step.
	if info, err := os.Lstat(cliPath); err == nil && info.Mode().IsRegular() {
		if err := os.Rename(cliPath, executable); err != nil {
			return fmt.Errorf("move existing Orchard binary into app bundle: %w", err)
		}
		// Restore the CLI path before later fallible work. If plist writing or signing fails,
		// operators and the old LaunchAgent can still reach the migrated binary.
		if err := os.Symlink(executable, cliPath); err != nil {
			return fmt.Errorf("link Orchard CLI to migrated worker app: %w", err)
		}
	}
	// A binary from any other commit (a migrated one, or one built before the pin moved) is
	// replaced; a running worker is restarted onto the new one below.
	rebuilt := false
	if !workerOrchardAtPinnedCommit(opts) {
		if err := ensureOrchardBinary(ctx, r, opts, executable, out); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(workerOrchardCommitPath(opts)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(workerOrchardCommitPath(opts), []byte(OrchardForkCommit+"\n"), 0o644); err != nil {
			return fmt.Errorf("record Orchard worker commit: %w", err)
		}
		rebuilt = true
	}

	if err := os.WriteFile(workerOrchardInfoPlistPath(opts), []byte(workerOrchardInfoPlist()), 0o644); err != nil {
		return fmt.Errorf("write Orchard worker app Info.plist: %w", err)
	}
	if _, _, err := r.Run(ctx, "/usr/bin/codesign", "--force", "--deep", "--sign", "-",
		"--identifier", orchardWorkerBundleID, "--timestamp=none", appDir); err != nil {
		return fmt.Errorf("sign Orchard worker app: %w", err)
	}
	if rebuilt {
		// Not loaded yet on a first install; the worker-daemons step loads it.
		restartDaemon(ctx, r, orchardWorkerLabel)
	}

	if err := os.MkdirAll(filepath.Dir(cliPath), 0o755); err != nil {
		return err
	}
	if target, err := os.Readlink(cliPath); err == nil {
		if target == executable {
			return nil
		}
		if err := os.Remove(cliPath); err != nil {
			return fmt.Errorf("replace stale Orchard symlink: %w", err)
		}
	} else if _, err := os.Lstat(cliPath); err == nil {
		return fmt.Errorf("refusing to replace non-symlink Orchard path %s", cliPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(executable, cliPath); err != nil {
		return fmt.Errorf("link Orchard CLI to worker app: %w", err)
	}
	return nil
}

// ensureOrchardBinary builds the fork at OrchardForkCommit in a scratch checkout and moves it
// over dest. Fetching the commit by its SHA means a push to the fork's main changes nothing here.
// It runs entirely through r so it's inert under a FakeRunner.
func ensureOrchardBinary(ctx context.Context, r Runner, opts Options, dest string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "grove-orchard-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	fmt.Fprintf(out, "        building github.com/gm2211/orchard at %s\n", OrchardForkCommit)
	built := filepath.Join(tmp, "orchard")
	build := fmt.Sprintf("set -e; cd %q && git init -q src && cd src && "+
		"git fetch -q --depth 1 https://github.com/gm2211/orchard %s && git checkout -q FETCH_HEAD && "+
		"[ \"$(git rev-parse HEAD)\" = %s ] && go build -o %q ./cmd/orchard && mv -f %q %q",
		tmp, OrchardForkCommit, OrchardForkCommit, built, built, dest)
	if _, _, err := r.Run(ctx, "/bin/sh", "-c", build); err != nil {
		return fmt.Errorf("build orchard %s from source: %w (see docs/INSTALL.md for the manual steps)", OrchardForkCommit, err)
	}
	return nil
}

// workerSleepStep keeps the Mac awake: a privileged, opt-in host setting.
func workerSleepStep(r Runner) Step {
	return Step{
		Name:        "disable-sleep",
		Privileged:  true,
		Description: "sudo pmset -a disablesleep 1   # a worker Mac that sleeps stops accepting jobs",
		Check: func(ctx context.Context) (bool, error) {
			stdout, _, err := r.Run(ctx, "pmset", "-g")
			if err != nil {
				return false, err
			}
			return containsDisableSleep1(stdout), nil
		},
		Apply: func(ctx context.Context) error {
			_, _, err := r.Run(ctx, "sudo", "pmset", "-a", "disablesleep", "1")
			return err
		},
	}
}

func containsDisableSleep1(pmsetOutput string) bool {
	for _, line := range strings.Split(pmsetOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "SleepDisabled" && fields[1] == "1" {
			return true
		}
	}
	return false
}
