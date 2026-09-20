package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The worker runs Orchard from an app bundle so macOS can attribute Local Network consent to a
// stable, named responsible-code identity. The ~/.local/bin entry remains a symlink for operators
// and scripts that invoke Orchard directly.
const (
	orchardBinName        = "orchard"
	orchardWorkerAppName  = "Grove Orchard Worker.app"
	orchardWorkerBundleID = "com.gm2211.grove.orchard-worker"
)

func orchardBinDir(opts Options) string { return filepath.Join(opts.homeDir(), ".local", "bin") }

func workerOrchardAppDir(opts Options) string {
	return filepath.Join(opts.homeDir(), "Applications", orchardWorkerAppName)
}

func workerOrchardExecutable(opts Options) string {
	return filepath.Join(workerOrchardAppDir(opts), "Contents", "MacOS", orchardBinName)
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
	agentsDir := opts.LaunchAgentsDir()
	logDir := opts.LogDir()
	plistPath := filepath.Join(agentsDir, "com.grove.orchard-worker.plist")
	cliOrchard := filepath.Join(orchardBinDir(opts), orchardBinName)
	workerOrchard := workerOrchardExecutable(opts)

	steps := []Step{
		{
			Name:        "homebrew",
			Description: "Install Homebrew (`/bin/bash -c \"$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"`, NONINTERACTIVE=1).",
			Check: func(ctx context.Context) (bool, error) {
				_, err := lookPath("brew")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				_, _, err := r.Run(ctx, "/bin/bash", "-c",
					`NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`)
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

	if step := registryLoginStep(r, opts, out); step != nil {
		steps = append(steps, *step)
	}

	steps = append(steps,
		Step{
			Name: "orchard-worker-app",
			Description: fmt.Sprintf(
				"Install the Orchard fork in %s with a stable macOS app identity and Local Network usage description; keep %s as a symlink. Existing Grove Orchard binaries migrate in place, otherwise grove downloads a github.com/gm2211/orchard release asset or builds the fork from source.",
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
			Name: "launchagent:orchard-worker",
			Description: fmt.Sprintf(
				"Render %s (KeepAlive+RunAtLoad, logs under %s) running `orchard worker run --name %s --labels host=%s,arch=arm64 %s`.",
				plistPath, logDir, host, host, opts.Controller,
			),
			Check: func(ctx context.Context) (bool, error) {
				want, err := workerLaunchAgentPlist(opts, workerOrchard)
				if err != nil {
					return false, err
				}
				got, err := os.ReadFile(plistPath)
				if err != nil {
					if os.IsNotExist(err) {
						return false, nil
					}
					return false, err
				}
				return string(got) == want, nil
			},
			Apply: func(ctx context.Context) error {
				content, err := workerLaunchAgentPlist(opts, workerOrchard)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(agentsDir, 0o755); err != nil {
					return err
				}
				if err := os.MkdirAll(logDir, 0o755); err != nil {
					return err
				}
				return os.WriteFile(plistPath, []byte(content), 0o644)
			},
		},
	)

	steps = append(steps, workerFinalizationSteps(r, opts, plistPath, workerOrchard)...)
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
			"re-run `grove install --role worker --registry-token …`.\n", registry)
		return nil
	}

	markerPath := registryLoginMarkerPath(opts, registry)
	marker := opts.RegistryUser + " " + registry + "\n"

	return &Step{
		Name: "registry-login:" + registry,
		Description: fmt.Sprintf(
			"tart login %s --username %s --password-stdin  (token from --registry-token / GROVE_REGISTRY_TOKEN)",
			registry, opts.RegistryUser,
		),
		Check: func(ctx context.Context) (bool, error) {
			got, err := os.ReadFile(markerPath)
			if err != nil {
				if os.IsNotExist(err) {
					return false, nil
				}
				return false, err
			}
			return string(got) == marker, nil
		},
		Apply: func(ctx context.Context) error {
			if opts.RegistryUser == "" {
				return fmt.Errorf("registry-login: --registry-user is required when --registry-token is set")
			}
			if _, _, err := r.RunWithStdin(ctx, opts.RegistryToken, "tart", "login", registry,
				"--username", opts.RegistryUser, "--password-stdin"); err != nil {
				return fmt.Errorf("tart login %s: %w", registry, err)
			}
			if err := os.MkdirAll(opts.ConfigDir(), 0o755); err != nil {
				return err
			}
			return os.WriteFile(markerPath, []byte(marker), 0o644)
		},
	}
}

// workerLaunchAgentPlist renders the LaunchAgent that supervises `orchard worker run`.
func workerLaunchAgentPlist(opts Options, orchardBin string) (string, error) {
	host := opts.hostname()
	args := []string{
		"worker", "run",
		"--name", host,
		"--labels", fmt.Sprintf("host=%s,arch=arm64", host),
	}
	if opts.Token != "" {
		args = append(args, "--token", opts.Token)
	}
	args = append(args, opts.Controller)
	return RenderLaunchAgent(LaunchAgentSpec{
		Label:                       "com.grove.orchard-worker",
		Program:                     orchardBin,
		Args:                        args,
		WorkingDir:                  opts.homeDir(),
		AssociatedBundleIdentifiers: []string{orchardWorkerBundleID},
		KeepAlive:                   true,
		RunAtLoad:                   true,
		StdoutPath:                  filepath.Join(opts.LogDir(), "orchard-worker.log"),
		StderrPath:                  filepath.Join(opts.LogDir(), "orchard-worker.err.log"),
	})
}

func workerOrchardAppInstalled(ctx context.Context, r Runner, opts Options, cliPath string) (bool, error) {
	executable := workerOrchardExecutable(opts)
	if info, err := os.Stat(executable); err != nil || !info.Mode().IsRegular() {
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
	} else if _, err := os.Stat(executable); os.IsNotExist(err) {
		if err := ensureOrchardBinary(ctx, r, opts, executable, out); err != nil {
			return err
		}
	}

	if err := os.WriteFile(workerOrchardInfoPlistPath(opts), []byte(workerOrchardInfoPlist()), 0o644); err != nil {
		return fmt.Errorf("write Orchard worker app Info.plist: %w", err)
	}
	if _, _, err := r.Run(ctx, "/usr/bin/codesign", "--force", "--deep", "--sign", "-",
		"--identifier", orchardWorkerBundleID, "--timestamp=none", appDir); err != nil {
		return fmt.Errorf("sign Orchard worker app: %w", err)
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

// ensureOrchardBinary tries a released asset first, falling back to a from-source build in a
// scratch clone. Both paths run entirely through r so they're inert under a FakeRunner.
func ensureOrchardBinary(ctx context.Context, r Runner, opts Options, dest string, out io.Writer) error {
	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	if lp := opts.lookPath(); lp != nil {
		if _, err := lp("gh"); err == nil {
			pattern := fmt.Sprintf("*%s*%s*", opts.goos(), opts.goarch())
			_, _, err := r.Run(ctx, "gh", "release", "download",
				"--repo", "gm2211/orchard",
				"--pattern", pattern,
				"--dir", destDir,
				"--clobber",
			)
			if err == nil {
				_, _, err := r.Run(ctx, "chmod", "+x", dest)
				return err
			}
			fmt.Fprintf(out, "        no gm2211/orchard release asset matching %s (%v); building from source instead\n", pattern, err)
		}
	}

	tmp, err := os.MkdirTemp("", "grove-orchard-build-")
	if err != nil {
		return err
	}
	build := fmt.Sprintf("set -e; git clone --depth 1 https://github.com/gm2211/orchard %q && cd %q && go build -o %q ./cmd/orchard", tmp, tmp, dest)
	if _, _, err := r.Run(ctx, "/bin/sh", "-c", build); err != nil {
		return fmt.Errorf("build orchard from source: %w (see docs/INSTALL.md for the manual steps)", err)
	}
	return nil
}

// workerFinalizationSteps prevent sleep (a privileged, opt-in host setting) and load the current
// user's worker agent (an ordinary-user operation performed during installation).
func workerFinalizationSteps(r Runner, opts Options, plistPath, workerOrchard string) []Step {
	uid := fmt.Sprint(os.Getuid())
	label := "gui/" + uid + "/com.grove.orchard-worker"
	return []Step{
		{
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
		},
		{
			Name:        "launchagent-load",
			Description: fmt.Sprintf("Reload %s in gui/%s so macOS associates the worker with %s and can present its one-time Local Network consent alert.", plistPath, uid, orchardWorkerBundleID),
			Check: func(ctx context.Context) (bool, error) {
				stdout, _, err := r.Run(ctx, "launchctl", "print", label)
				return err == nil && strings.Contains(stdout, workerOrchard), nil
			},
			Apply: func(ctx context.Context) error {
				// bootout returns non-zero when the job is not loaded; bootstrap is still the right
				// next action in that case, so this is deliberately best-effort.
				_, _, _ = r.Run(ctx, "launchctl", "bootout", label)
				_, _, err := r.Run(ctx, "launchctl", "bootstrap", "gui/"+uid, plistPath)
				return err
			},
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
