package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// orchardBinDir/orchardBinName is where grove installs the Orchard fork binary when it isn't
// already on PATH.
const orchardBinName = "orchard"

func orchardBinDir(opts Options) string { return filepath.Join(opts.homeDir(), ".local", "bin") }

// buildWorkerSteps assembles the worker role plan. Worker is macOS-only (it runs Tart VMs, which
// need Apple Silicon + Virtualization.framework).
func buildWorkerSteps(r Runner, opts Options, out io.Writer) []Step {
	lookPath := opts.lookPath()
	host := opts.hostname()
	agentsDir := opts.LaunchAgentsDir()
	logDir := opts.LogDir()
	plistPath := filepath.Join(agentsDir, "com.grove.orchard-worker.plist")
	destOrchard := filepath.Join(orchardBinDir(opts), orchardBinName)

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
			Name:        "orchard-binary",
			Description: fmt.Sprintf("Install the Orchard fork binary to %s: download a github.com/gm2211/orchard release asset if one exists, else clone and `go build ./cmd/orchard` (the fork keeps the upstream module path, so `go install github.com/gm2211/orchard/...@main` can't resolve it — see docs/INSTALL.md for the manual build if this fails).", destOrchard),
			Check: func(ctx context.Context) (bool, error) {
				if _, err := lookPath("orchard"); err == nil {
					return true, nil
				}
				_, err := os.Stat(destOrchard)
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				return ensureOrchardBinary(ctx, r, opts, destOrchard, out)
			},
		},
		Step{
			Name: "launchagent:orchard-worker",
			Description: fmt.Sprintf(
				"Render %s (KeepAlive+RunAtLoad, logs under %s) running `orchard worker run --name %s --labels host=%s,arch=arm64 %s`.",
				plistPath, logDir, host, host, opts.Controller,
			),
			Check: func(ctx context.Context) (bool, error) {
				want, err := workerLaunchAgentPlist(opts, destOrchard)
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
				content, err := workerLaunchAgentPlist(opts, destOrchard)
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

	steps = append(steps, workerPrivilegedSteps(r, opts, plistPath)...)
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
		Label:      "com.grove.orchard-worker",
		Program:    orchardBin,
		Args:       args,
		WorkingDir: opts.homeDir(),
		KeepAlive:  true,
		RunAtLoad:  true,
		StdoutPath: filepath.Join(opts.LogDir(), "orchard-worker.log"),
		StderrPath: filepath.Join(opts.LogDir(), "orchard-worker.err.log"),
	})
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

// workerPrivilegedSteps are steps a human (or Claude running as root) must run explicitly: they
// require sudo or touch system-wide power/network settings. Apply only executes for real when the
// plan is run with --yes as root; otherwise RunPlan prints the exact command and moves on.
func workerPrivilegedSteps(r Runner, opts Options, plistPath string) []Step {
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
			Name:       "local-network-permission",
			Privileged: true,
			Description: "macOS 15+ blocks the worker's local-network access until granted. Run:\n" +
				`        sudo defaults write com.apple.network.local-network AllowedEthernetLocalNetworkAddresses -array "10.0.0.0/8" "172.16.0.0/12" "192.168.0.0/16"` + "\n" +
				`        sudo defaults write com.apple.network.local-network AllowedWiFiLocalNetworkAddresses -array "10.0.0.0/8" "172.16.0.0/12" "192.168.0.0/16"` + "\n" +
				"        then reboot. (Orchard alternative: run the worker as root with --user <you>, see its README.)",
			Check: func(ctx context.Context) (bool, error) {
				stdout, _, err := r.Run(ctx, "defaults", "read", "com.apple.network.local-network", "AllowedEthernetLocalNetworkAddresses")
				if err != nil {
					return false, nil // not set yet; not a hard failure
				}
				return stdout != "", nil
			},
			Apply: func(ctx context.Context) error {
				for _, key := range []string{"AllowedEthernetLocalNetworkAddresses", "AllowedWiFiLocalNetworkAddresses"} {
					_, _, err := r.Run(ctx, "sudo", "defaults", "write", "com.apple.network.local-network", key,
						"-array", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			Name:        "launchagent-load",
			Privileged:  true,
			Description: fmt.Sprintf("launchctl bootstrap gui/%s %s   # load the worker agent for this session", uid, plistPath),
			Check: func(ctx context.Context) (bool, error) {
				_, _, err := r.Run(ctx, "launchctl", "print", label)
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
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
