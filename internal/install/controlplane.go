package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/pki"
	"gopkg.in/yaml.v3"
)

// Default ports for the services grove renders on a control-plane host. Only used to fill in a
// bind address when the corresponding --controller/--nomad/--server flag wasn't given (on this
// role those flags describe the service being stood up here, not one to dial out to).
const (
	defaultOrchardControllerPort = "6120"
	defaultNomadPort             = "4646"
	defaultGroveServerPort       = "6130"
)

// buildControlPlaneSteps assembles the control-plane role plan. Works on macOS or Linux.
func buildControlPlaneSteps(r Runner, opts Options, out io.Writer) []Step {
	cfgDir := opts.ConfigDir()
	destOrchard := filepath.Join(orchardBinDir(opts), orchardBinName)
	isDarwin := opts.goos() == "darwin"

	// Set when the Nomad config on disk differs from what this plan renders, so the Nomad start
	// step restarts an already-running agent onto the new config.
	nomadConfigChanged := new(bool)
	// The same for the sandbox-host client agent's config (or the certificates it presents).
	sandboxConfigChanged := new(bool)
	// Set when the Orchard binary was rebuilt at a new OrchardForkCommit, so a running controller
	// restarts onto it.
	orchardRebuilt := new(bool)

	var steps []Step
	steps = append(steps, controlPlaneDependencySteps(r, opts, out, destOrchard, isDarwin, orchardRebuilt)...)
	steps = append(steps, controlPlaneConfigSteps(r, opts, cfgDir, destOrchard, nomadConfigChanged, sandboxConfigChanged)...)
	steps = append(steps, controlPlaneServiceSteps(r, opts, cfgDir, destOrchard, isDarwin)...)
	steps = append(steps, controlPlaneStartStep(r, opts, "orchard-controller", isDarwin, orchardRebuilt))
	steps = append(steps, Step{
		Name:        "orchard-service-account",
		Description: "Create Orchard `grove` service account (compute:read, compute:write, compute:connect); retrieve its generated token without placing it in argv.",
		Check: func(ctx context.Context) (bool, error) {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return false, err
			}
			return cfg.Orchard.Token != "", nil
		},
		Apply: func(ctx context.Context) error {
			// --roles uses pflag's StringArrayVar on the fork, which does NOT split on commas —
			// a single "compute:read,compute:write,compute:connect" value would be treated as
			// one (invalid) role name. Pass --roles once per role instead.
			_, _, createErr := r.Run(ctx, destOrchard, "create", "service-account", "grove",
				"--roles", "compute:read",
				"--roles", "compute:write",
				"--roles", "compute:connect")
			stdout, _, getErr := r.Run(ctx, destOrchard, "get", "service-account", "grove/token")
			if getErr != nil {
				if createErr != nil {
					return fmt.Errorf("create Orchard service account: %w", createErr)
				}
				return fmt.Errorf("read Orchard service account token: %w", getErr)
			}
			token := strings.TrimSpace(stdout)
			if token == "" {
				return errors.New("Orchard returned an empty grove service-account token")
			}
			return updateConfig(cfgDir, func(cfg *config.Config) { cfg.Orchard.Token = token })
		},
	})
	steps = append(steps, Step{
		Name:        "orchard-enrollment-service-account",
		Description: "Create control-plane-only enrollment issuer that can mint only grove-worker-* credentials with compute:write and compute:connect.",
		Check: func(ctx context.Context) (bool, error) {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return false, err
			}
			return cfg.Enrollment.IssuerToken != "", nil
		},
		Apply: func(ctx context.Context) error {
			const name = "grove-enroller"
			_, _, createErr := r.Run(ctx, destOrchard, "create", "service-account", name,
				"--roles", "service-account:issue-worker")
			stdout, _, getErr := r.Run(ctx, destOrchard, "get", "service-account", name+"/token")
			if getErr != nil {
				if createErr != nil {
					return fmt.Errorf("create Orchard worker account: %w", createErr)
				}
				return fmt.Errorf("read Orchard worker account token: %w", getErr)
			}
			token := strings.TrimSpace(stdout)
			if token == "" {
				return errors.New("Orchard returned an empty enrollment issuer token")
			}
			return updateConfig(cfgDir, func(cfg *config.Config) { cfg.Enrollment.IssuerToken = name + ":" + token })
		},
	})
	steps = append(steps, controlPlaneFinalizationSteps(r, opts, isDarwin, nomadConfigChanged, sandboxConfigChanged)...)
	return steps
}

// controlPlaneStartStep starts service if it isn't running. With restart set (its config just
// changed), it restarts a running one too, so the new config takes effect.
func controlPlaneStartStep(r Runner, opts Options, service string, isDarwin bool, restart *bool) Step {
	description := "Start " + service + " as current user."
	if restart != nil {
		description = "Start " + service + " as current user (restarting it if its config changed)."
	}
	mustRestart := func() bool { return restart != nil && *restart }
	if isDarwin {
		label := "com.grove." + service
		plist := filepath.Join(opts.LaunchAgentsDir(), label+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		return Step{Name: "service-start:" + service, Description: description,
			Check: func(ctx context.Context) (bool, error) {
				if mustRestart() {
					return false, nil
				}
				_, _, err := r.Run(ctx, "launchctl", "print", domain+"/"+label)
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				return relaunchAgent(ctx, r, domain, label, plist)
			}}
	}
	unit := "grove-" + service + ".service"
	return Step{Name: "service-start:" + service, Description: description,
		Check: func(ctx context.Context) (bool, error) {
			if mustRestart() {
				return false, nil
			}
			_, _, err := r.Run(ctx, "systemctl", "--user", "is-active", "--quiet", unit)
			return err == nil, nil
		},
		Apply: func(ctx context.Context) error {
			_, _, _ = r.Run(ctx, "systemctl", "--user", "daemon-reload")
			_, _, err := r.Run(ctx, "systemctl", "--user", "enable", "--now", unit)
			if err == nil && mustRestart() {
				_, _, err = r.Run(ctx, "systemctl", "--user", "restart", unit)
			}
			return err
		}}
}

func controlPlaneFinalizationSteps(r Runner, opts Options, isDarwin bool, nomadConfigChanged, sandboxConfigChanged *bool) []Step {
	services := []string{"orchard-controller", "nomad", "nomad-sandbox-client"}
	steps := make([]Step, 0, len(services))
	for _, service := range services {
		var restart *bool
		switch service {
		case "nomad":
			restart = nomadConfigChanged
		case "nomad-sandbox-client":
			restart = sandboxConfigChanged
		}
		steps = append(steps, controlPlaneStartStep(r, opts, service, isDarwin, restart))
	}
	// Nomad is running by now; bootstrap its ACLs before grove serve reloads so the server comes
	// back up holding the management token.
	steps = append(steps, nomadACLBootstrapStep(r, opts, isDarwin))
	// With the management token in hand, let the sandbox-host client register.
	steps = append(steps, nomadSandboxIntroStep(r, opts, isDarwin))
	// Older installs ran MinIO as the artifact store; artifacts now live on this machine's disk.
	steps = append(steps, retireLegacyMinIOStep(r, opts, isDarwin))
	steps = append(steps, controlPlaneReloadServerStep(r, opts, isDarwin))
	return steps
}

func controlPlaneReloadServerStep(r Runner, opts Options, isDarwin bool) Step {
	if isDarwin {
		label := "com.grove.server"
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		plist := filepath.Join(opts.LaunchAgentsDir(), label+".plist")
		return Step{Name: "service-reload:server", Description: "Reload Grove server so new enrollment settings take effect.", Check: func(context.Context) (bool, error) { return false, nil }, Apply: func(ctx context.Context) error {
			return relaunchAgent(ctx, r, domain, label, plist)
		}}
	}
	return Step{Name: "service-reload:server", Description: "Reload Grove server so new enrollment settings take effect.", Check: func(context.Context) (bool, error) { return false, nil }, Apply: func(ctx context.Context) error {
		_, _, _ = r.Run(ctx, "systemctl", "--user", "daemon-reload")
		_, _, err := r.Run(ctx, "systemctl", "--user", "enable", "--now", "grove-server.service")
		if err == nil {
			_, _, err = r.Run(ctx, "systemctl", "--user", "restart", "grove-server.service")
		}
		return err
	}}
}

func controlPlaneDependencySteps(r Runner, opts Options, out io.Writer, destOrchard string, isDarwin bool, orchardRebuilt *bool) []Step {
	lookPath := opts.lookPath()
	var steps []Step
	if isDarwin {
		steps = append(steps, trustTapStep(r, tapHashicorp, out))
	}
	steps = append(steps,
		Step{
			Name:        "nomad-binary",
			Description: nomadInstallDescription(isDarwin),
			Check: func(ctx context.Context) (bool, error) {
				_, err := lookPath("nomad")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				return installNomad(ctx, r, opts, isDarwin)
			},
		},
		Step{
			Name:        "orchard-binary",
			Description: fmt.Sprintf("Build the Orchard fork at the pinned commit %s to %s.", OrchardForkCommit, destOrchard),
			Check: func(ctx context.Context) (bool, error) {
				if _, err := os.Stat(destOrchard); err != nil || !orchardBinaryAtPin(destOrchard) {
					return false, nil
				}
				stdout, stderr, err := r.Run(ctx, destOrchard, "create", "service-account", "--help")
				return err == nil && strings.Contains(stdout+stderr, "service-account:issue-worker"), nil
			},
			Apply: func(ctx context.Context) error {
				if err := ensureOrchardBinary(ctx, r, opts, destOrchard, io.Discard); err != nil {
					return err
				}
				if err := os.WriteFile(orchardCommitPath(destOrchard), []byte(OrchardForkCommit+"\n"), 0o644); err != nil {
					return fmt.Errorf("record Orchard commit: %w", err)
				}
				*orchardRebuilt = true
				return nil
			},
		},
	)
	return steps
}

func nomadInstallDescription(isDarwin bool) string {
	if isDarwin {
		return "Install Nomad (`brew install hashicorp/tap/nomad`; Homebrew verifies the formula's sha256)."
	}
	return fmt.Sprintf("Install Nomad %s to ~/.local/bin from releases.hashicorp.com, verifying the zip against grove's pinned SHA-256 before unpacking.", NomadVersion)
}

func installNomad(ctx context.Context, r Runner, opts Options, isDarwin bool) error {
	if isDarwin {
		_, _, err := r.Run(ctx, "brew", "install", "hashicorp/tap/nomad")
		return err
	}
	return installPinnedNomad(ctx, r, opts)
}

// installPinnedNomad fetches the pinned Nomad release zip for this GOOS/GOARCH, verifies it, and
// unzips the single binary into ~/.local/bin. Used for Nomad on Linux (macOS uses brew).
func installPinnedNomad(ctx context.Context, r Runner, opts Options) error {
	d, err := nomadDownload(opts)
	if err != nil {
		return err
	}
	zipPath, err := fetchVerified(ctx, r, opts, d)
	if err != nil {
		return err
	}
	unpacked := filepath.Join(downloadDir(opts), "nomad-"+NomadVersion)
	if _, _, err := r.Run(ctx, "unzip", "-o", zipPath, "nomad", "-d", unpacked); err != nil {
		return fmt.Errorf("unpack %s: %w", zipPath, err)
	}
	destDir := orchardBinDir(opts)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	_, _, err = r.Run(ctx, "install", "-m", "0755", filepath.Join(unpacked, "nomad"), filepath.Join(destDir, "nomad"))
	return err
}

// controlPlaneConfigSteps render the files under ~/.config/grove/ that the supervised processes
// and the grove CLI itself read.
func controlPlaneConfigSteps(r Runner, opts Options, cfgDir, destOrchard string, nomadConfigChanged, sandboxConfigChanged *bool) []Step {
	tailnetHost := resolvedTailnetHost(r, opts)
	orchardAddr := hostPortOrDefault(opts.Controller, tailnetHost, defaultOrchardControllerPort)
	nomadAddr := hostPortOrDefault(opts.NomadAddr, tailnetHost, defaultNomadPort)
	serverAddr := hostPortOrDefault(opts.ServerURL, tailnetHost, defaultGroveServerPort)

	orchardEnvPath := filepath.Join(cfgDir, "orchard-controller", "env")
	nomadConfPath := filepath.Join(cfgDir, "nomad", "server.hcl")
	artifactDir := artifactStoreDir(opts)
	fleetPath := filepath.Join(cfgDir, "fleet.yaml")
	orchardHome := controlPlaneOrchardHome(cfgDir)
	tlsDir := nomadTLSDir(cfgDir)
	tlsFiles := pki.NomadFiles(tlsDir)
	sans := nomadServerSANs(hostOnly(nomadAddr))
	serverSpec := NomadServerConfigSpec{
		DataDir:  filepath.Join(cfgDir, "nomad", "data"),
		BindAddr: hostOnly(nomadAddr),
		TLS:      NomadTLSFiles{CA: tlsFiles.CA, Cert: tlsFiles.Server, Key: tlsFiles.ServerKey},
	}
	sandboxConfPath := nomadSandboxClientConfPath(cfgDir)
	sandboxSpec := NomadSandboxClientConfigSpec{
		DataDir: nomadSandboxDataDir(cfgDir),
		Server:  net.JoinHostPort(hostOnly(nomadAddr), "4647"),
		TLS:     NomadTLSFiles{CA: tlsFiles.CA, Cert: tlsFiles.Node, Key: tlsFiles.NodeKey},
	}

	return []Step{
		{
			Name:        "config:orchard-controller-env",
			Description: fmt.Sprintf("Render %s (ORCHARD_HOME, ORCHARD_ADDRESS=%s).", orchardEnvPath, orchardAddr),
			Check: fileHasContent(orchardEnvPath, func() (string, error) {
				return RenderOrchardControllerEnv(OrchardControllerEnvSpec{
					Home:    orchardHome,
					Address: orchardAddr,
				})
			}),
			Apply: writeRenderedFile(orchardEnvPath, func() (string, error) {
				return RenderOrchardControllerEnv(OrchardControllerEnvSpec{
					Home:    orchardHome,
					Address: orchardAddr,
				})
			}),
		},
		{
			Name: "config:nomad-tls",
			Description: fmt.Sprintf("Create or renew grove's Nomad CA and certificates in %s (CA key 0600, never leaves this machine; server cert for %s; "+
				"a client cert for grove itself and one for the fleet's VMs).", tlsDir, hostOnly(nomadAddr)),
			Check: func(context.Context) (bool, error) {
				ok := pki.Current(tlsDir, sans, time.Now())
				if !ok {
					// Agents load certificates at start: restart both onto the new ones.
					*nomadConfigChanged = true
					*sandboxConfigChanged = true
				}
				return ok, nil
			},
			Apply: func(context.Context) error {
				_, _, err := pki.EnsureNomad(tlsDir, sans, time.Now())
				return err
			},
		},
		{
			Name:        "config:nomad-server",
			Description: fmt.Sprintf("Render %s (single-node server, bootstrap_expect=1, ACLs on, mutual TLS, strict client introduction, bind %s).", nomadConfPath, nomadAddr),
			Check: func(ctx context.Context) (bool, error) {
				ok, err := fileHasContent(nomadConfPath, func() (string, error) {
					return RenderNomadServerConfig(serverSpec)
				})(ctx)
				if err == nil && !ok {
					*nomadConfigChanged = true
				}
				return ok, err
			},
			Apply: writeRenderedFile(nomadConfPath, func() (string, error) {
				return RenderNomadServerConfig(serverSpec)
			}),
		},
		{
			Name:        "config:nomad-sandbox-client",
			Description: fmt.Sprintf("Render %s (the control plane's own Nomad client, for sealed pools' jobs only; a separate agent so it registers through client introduction like any VM).", sandboxConfPath),
			Check: func(ctx context.Context) (bool, error) {
				ok, err := fileHasContent(sandboxConfPath, func() (string, error) {
					return RenderNomadSandboxClientConfig(sandboxSpec)
				})(ctx)
				if err == nil && !ok {
					*sandboxConfigChanged = true
				}
				return ok, err
			},
			Apply: writeRenderedFile(sandboxConfPath, func() (string, error) {
				return RenderNomadSandboxClientConfig(sandboxSpec)
			}),
		},
		{
			Name:        "config:artifact-store",
			Description: fmt.Sprintf("Create the artifact store directory (0700) and record it in config.yaml as artifacts.dir (default %s); drop any legacy S3/MinIO artifact keys.", artifactDir),
			Check: func(ctx context.Context) (bool, error) {
				cfg, err := loadOrInitConfig(cfgDir)
				if err != nil {
					return false, err
				}
				if cfg.Artifacts.Dir == "" || len(cfg.Artifacts.LegacyKeys()) > 0 {
					return false, nil
				}
				info, err := os.Stat(cfg.Artifacts.Dir)
				if os.IsNotExist(err) {
					return false, nil
				}
				return err == nil && info.IsDir(), err
			},
			Apply: func(ctx context.Context) error {
				cfg, err := loadOrInitConfig(cfgDir)
				if err != nil {
					return err
				}
				dir := cfg.Artifacts.Dir
				if dir == "" {
					dir = artifactDir
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				return updateConfig(cfgDir, func(cfg *config.Config) {
					cfg.Artifacts.Dir = dir
					cfg.Artifacts.ClearLegacy()
				})
			},
		},
		{
			Name:        "config:grove-config-yaml",
			Description: fmt.Sprintf("Write %s/config.yaml (orchard/nomad/server endpoints, Nomad TLS file paths, generated server token).", cfgDir),
			Check: func(ctx context.Context) (bool, error) {
				cfg, err := loadOrInitConfig(cfgDir)
				if err != nil {
					return false, err
				}
				return cfg.Server.Token != "" && cfg.Orchard.URL != "" &&
					strings.HasPrefix(cfg.Nomad.URL, "https://") && nomadTLSConfigured(cfg.Nomad, tlsFiles), nil
			},
			Apply: func(ctx context.Context) error {
				return updateConfig(cfgDir, func(cfg *config.Config) {
					cfg.Orchard.URL = "http://" + orchardAddr
					cfg.Nomad.URL = "https://" + nomadAddr
					cfg.Nomad.CACert = tlsFiles.CA
					cfg.Nomad.ClientCert = tlsFiles.CLI
					cfg.Nomad.ClientKey = tlsFiles.CLIKey
					cfg.Nomad.NodeCert = tlsFiles.Node
					cfg.Nomad.NodeKey = tlsFiles.NodeKey
					cfg.Server.Listen = serverAddr
					cfg.Server.URL = "http://" + serverAddr
					if cfg.Server.Token == "" {
						if tok, err := randomHex(32); err == nil {
							cfg.Server.Token = tok
						}
					}
					cfg.Fleet = fleetPath
				})
			},
		},
		{
			Name:        "config:fleet-yaml",
			Description: fmt.Sprintf("Write starter %s (linux + macos pools, per ARCHITECTURE.md).", fleetPath),
			Check: func(ctx context.Context) (bool, error) {
				_, err := os.Stat(fleetPath)
				if os.IsNotExist(err) {
					return false, nil
				}
				return err == nil, err
			},
			Apply: func(ctx context.Context) error {
				return writeFile(fleetPath, starterFleetYAML())
			},
		},
	}
}

// controlPlaneServiceSteps render the process supervisors (launchd on macOS, systemd --user on
// Linux) for orchard controller, nomad and `grove serve`.
func controlPlaneServiceSteps(r Runner, opts Options, cfgDir, destOrchard string, isDarwin bool) []Step {
	nomadConf := filepath.Join(cfgDir, "nomad", "server.hcl")
	nomadDataDir := filepath.Join(cfgDir, "nomad", "data")
	orchardHome := controlPlaneOrchardHome(cfgDir)

	units := []struct {
		name        string
		label       string
		description string
		// program is an absolute path, or a bare name resolved by supervisorProgram when the
		// unit is rendered: launchd and systemd --user start jobs with a minimal PATH that
		// holds neither ~/.local/bin nor Homebrew, so a bare name never starts.
		program string
		args    []string
		env         map[string]string
		workingDir  string
	}{
		{
			name:        "orchard-controller",
			label:       "com.grove.orchard-controller",
			description: "Orchard controller",
			program:     destOrchard,
			args:        []string{"controller", "run"},
			env:         map[string]string{"ORCHARD_HOME": orchardHome},
		},
		{
			name:        "nomad",
			label:       "com.grove.nomad",
			description: "Nomad server",
			program:     "nomad",
			args:        []string{"agent", "-config", nomadConf},
			workingDir:  nomadDataDir,
		},
		{
			name:        "nomad-sandbox-client",
			label:       "com.grove.nomad-sandbox-client",
			description: "Nomad client for sealed pools' jobs",
			program:     "nomad",
			args:        []string{"agent", "-config", nomadSandboxClientConfPath(cfgDir)},
		},
		{
			name:        "server",
			label:       "com.grove.server",
			description: "grove server (API + UI)",
			program:     "grove",
			args:        []string{"serve"},
		},
	}

	var steps []Step
	for _, u := range units {
		u := u
		if isDarwin {
			plistPath := filepath.Join(opts.LaunchAgentsDir(), u.label+".plist")
			logDir := opts.LogDir()
			steps = append(steps, Step{
				Name:        "launchagent:" + u.name,
				Description: fmt.Sprintf("Render %s for %s (KeepAlive+RunAtLoad, logs under %s).", plistPath, u.description, logDir),
				Check: fileHasContent(plistPath, func() (string, error) {
					program := supervisorProgram(opts, u.program)
					return RenderLaunchAgent(LaunchAgentSpec{
						Label: u.label, Program: program, Args: u.args, Env: u.env,
						WorkingDir: u.workingDir, KeepAlive: true, RunAtLoad: true,
						StdoutPath: filepath.Join(logDir, u.name+".log"),
						StderrPath: filepath.Join(logDir, u.name+".err.log"),
					})
				}),
				Apply: func(ctx context.Context) error {
					program := supervisorProgram(opts, u.program)
					content, err := RenderLaunchAgent(LaunchAgentSpec{
						Label: u.label, Program: program, Args: u.args, Env: u.env,
						WorkingDir: u.workingDir, KeepAlive: true, RunAtLoad: true,
						StdoutPath: filepath.Join(logDir, u.name+".log"),
						StderrPath: filepath.Join(logDir, u.name+".err.log"),
					})
					if err != nil {
						return err
					}
					if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
						return err
					}
					if err := os.MkdirAll(logDir, 0o755); err != nil {
						return err
					}
					return writeFile(plistPath, content)
				},
			})
		} else {
			unitPath := filepath.Join(opts.SystemdUserDir(), "grove-"+u.name+".service")
			steps = append(steps, Step{
				Name:        "systemd-unit:" + u.name,
				Description: fmt.Sprintf("Render %s for %s (`systemctl --user enable --now grove-%s`).", unitPath, u.description, u.name),
				Check: fileHasContent(unitPath, func() (string, error) {
					program := supervisorProgram(opts, u.program)
					return RenderSystemdUnit(SystemdUnitSpec{
						Description: u.description, Program: program, Args: u.args,
						Env: u.env, WorkingDir: u.workingDir,
					})
				}),
				Apply: func(ctx context.Context) error {
					program := supervisorProgram(opts, u.program)
					content, err := RenderSystemdUnit(SystemdUnitSpec{
						Description: u.description, Program: program, Args: u.args,
						Env: u.env, WorkingDir: u.workingDir,
					})
					if err != nil {
						return err
					}
					if err := os.MkdirAll(opts.SystemdUserDir(), 0o755); err != nil {
						return err
					}
					return writeFile(unitPath, content)
				},
			})
		}
	}
	return steps
}

// supervisorProgram resolves a unit's program to the absolute path its launchd/systemd unit must
// name: launchd and systemd --user start jobs with a minimal PATH that holds neither ~/.local/bin
// nor Homebrew, so a bare name never starts. It runs when the unit is rendered, after this plan's
// earlier steps installed the binary, and tries in turn: this shell's PATH, ~/.local/bin, and
// where the install itself puts the program (grove: the binary running the install; nomad:
// ~/.local/bin on Linux, Homebrew's prefix on macOS).
func supervisorProgram(opts Options, program string) string {
	if filepath.IsAbs(program) {
		return program
	}
	if program == "grove" {
		if p := groveExecutable(opts); filepath.IsAbs(p) {
			return p
		}
	}
	if p, err := opts.lookPath()(program); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs
		}
	}
	local := filepath.Join(orchardBinDir(opts), program)
	if opts.exists()(local) || opts.goos() != "darwin" {
		return local
	}
	if opts.goarch() == "amd64" {
		return filepath.Join("/usr/local/bin", program)
	}
	return filepath.Join("/opt/homebrew/bin", program)
}

// launchdRelaunchWait bounds how long relaunchAgent waits for launchd to finish unloading a job
// before loading it again, and launchdPoll how often it checks. Overridable in tests.
var (
	launchdRelaunchWait = 10 * time.Second
	launchdPoll         = 250 * time.Millisecond
)

// relaunchAgent (re)loads label from plist in domain: bootout, wait until launchd has actually
// unloaded it (bootout returns before it has, and bootstrapping a still-loaded job fails with
// "5: Input/output error"), then bootstrap.
func relaunchAgent(ctx context.Context, r Runner, domain, label, plist string) error {
	target := domain + "/" + label
	_, _, _ = r.Run(ctx, "launchctl", "bootout", target)
	deadline := time.Now().Add(launchdRelaunchWait)
	// Past the deadline, bootstrap anyway: its own error then says what's wrong.
	for time.Now().Before(deadline) {
		if _, _, err := r.Run(ctx, "launchctl", "print", target); err != nil {
			break // unloaded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(launchdPoll):
		}
	}
	_, stderr, err := r.Run(ctx, "launchctl", "bootstrap", domain, plist)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap %s %s: %w: %s", domain, plist, err, strings.TrimSpace(stderr))
	}
	return nil
}

// artifactStoreDir is where a fresh control plane keeps job artifacts: next to grove serve's job
// history (~/.local/state/grove/jobs.json). Written into config.yaml as an absolute path, so
// `grove serve` and `grove sandbox run` agree on it whatever environment their supervisor gives
// them.
func artifactStoreDir(opts Options) string {
	return filepath.Join(opts.homeDir(), ".local", "state", "grove", "artifacts")
}

// retireLegacyMinIOStep stops and removes the MinIO service an older `grove install --role
// control-plane` set up, now that artifacts live on the control plane's disk. It removes only the
// launchd plist / systemd unit; MinIO's data and credentials under ~/.config/grove/minio are left
// for the operator to delete (docs/OPERATIONS.md). Satisfied when no such service file exists.
func retireLegacyMinIOStep(r Runner, opts Options, isDarwin bool) Step {
	if isDarwin {
		label := "com.grove.minio"
		plist := filepath.Join(opts.LaunchAgentsDir(), label+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		return Step{
			Name:        "legacy-minio:retire",
			Description: fmt.Sprintf("Stop and remove the MinIO LaunchAgent (%s) an older grove installed; artifacts are now kept on disk.", plist),
			Check:       fileAbsent(plist),
			Apply: func(ctx context.Context) error {
				_, _, _ = r.Run(ctx, "launchctl", "bootout", domain+"/"+label)
				return removeIfExists(plist)
			},
		}
	}
	unit := "grove-minio.service"
	unitPath := filepath.Join(opts.SystemdUserDir(), unit)
	return Step{
		Name:        "legacy-minio:retire",
		Description: fmt.Sprintf("Stop and remove the MinIO systemd --user unit (%s) an older grove installed; artifacts are now kept on disk.", unitPath),
		Check:       fileAbsent(unitPath),
		Apply: func(ctx context.Context) error {
			_, _, _ = r.Run(ctx, "systemctl", "--user", "disable", "--now", unit)
			if err := removeIfExists(unitPath); err != nil {
				return err
			}
			_, _, err := r.Run(ctx, "systemctl", "--user", "daemon-reload")
			return err
		},
	}
}

func fileAbsent(path string) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) {
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// controlPlaneOrchardHome preserves both layouts Grove has shipped. Early/manual installs put
// Orchard state directly under orchard-controller; v0.1.2's installer used a data subdirectory.
// Choosing an empty new directory would make an existing fleet appear lost.
func controlPlaneOrchardHome(cfgDir string) string {
	root := filepath.Join(cfgDir, "orchard-controller")
	if _, err := os.Stat(filepath.Join(root, ".orchard", "orchard.yml")); err == nil {
		return root
	}
	nested := filepath.Join(root, "data")
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	return root
}

// --- small shared helpers ---

func fileHasContent(path string, render func() (string, error)) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		want, err := render()
		if err != nil {
			return false, err
		}
		got, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return string(got) == want, nil
	}
}

func writeRenderedFile(path string, render func() (string, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		content, err := render()
		if err != nil {
			return err
		}
		return writeFile(path, content)
	}
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loadOrInitConfig loads config.yaml from dir, returning a zero Config if it doesn't exist yet
// (not an error here: install renders it incrementally, step by step).
func loadOrInitConfig(dir string) (*config.Config, error) {
	path := filepath.Join(dir, "config.yaml")
	cfg, err := config.LoadFrom(path)
	if err != nil {
		if err == config.ErrNotFound {
			return &config.Config{}, nil
		}
		return nil, err
	}
	return cfg, nil
}

// updateConfig loads config.yaml (or starts from zero value), applies mutate, and saves it back.
func updateConfig(dir string, mutate func(*config.Config)) error {
	cfg, err := loadOrInitConfig(dir)
	if err != nil {
		return err
	}
	mutate(cfg)
	return config.Save(filepath.Join(dir, "config.yaml"), cfg)
}

func starterFleetYAML() string {
	spec := fleet.Spec{
		Pools: []fleet.Pool{
			{
				Name:      "linux",
				Image:     "ghcr.io/gm2211/grove-linux-worker:latest",
				PerWorker: 1,
				CPU:       4,
				Memory:    8192,
				TTL:       fleet.Duration(12 * hour),
				Labels:    map[string]string{"pool": "linux"},
			},
			{
				Name:      "macos",
				Image:     "ghcr.io/gm2211/grove-macos-worker:latest",
				PerWorker: 1,
				CPU:       4,
				Memory:    8192,
				TTL:       fleet.Duration(12 * hour),
				Labels:    map[string]string{"pool": "macos"},
			},
		},
	}
	out, err := yaml.Marshal(spec)
	if err != nil {
		// Spec marshals unconditionally in practice (plain structs); keep Apply infallible-looking
		// callers honest by surfacing this rather than silently writing an empty file.
		return "# error rendering fleet.yaml: " + err.Error() + "\n"
	}
	return string(out)
}

// resolvedTailnetHost is the host grove binds/advertises control-plane services on when a flag
// doesn't say otherwise: per ARCHITECTURE.md, that's this machine's tailnet IP. Falls back to the
// plain hostname if Tailscale can't be queried (e.g. --dry-run planning docs, or not installed
// yet — the "tailscale" step will already be flagging that).
func resolvedTailnetHost(r Runner, opts Options) string {
	bin, err := FindTailscale(opts.lookPath(), opts.exists())
	if err != nil {
		return opts.hostname()
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpCheckTimeout)
	defer cancel()
	st, err := GetTailscaleStatus(ctx, r, bin)
	if err != nil || st.TailnetIP() == "" {
		return opts.hostname()
	}
	return st.TailnetIP()
}

// normalizeURL prepends "http://" when raw has no scheme, so url.Parse treats "host:port" as a
// host+port rather than as "scheme:opaque".
func normalizeURL(raw string) string {
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "http://" + raw
}

// hostPortOrDefault returns "host:port" from raw if it parses as a URL with a host, else
// "fallbackHost:defaultPort".
func hostPortOrDefault(raw, fallbackHost, defaultPort string) string {
	if raw != "" {
		if u, err := url.Parse(normalizeURL(raw)); err == nil && u.Host != "" {
			if u.Port() != "" {
				return u.Host
			}
			return u.Host + ":" + defaultPort
		}
	}
	return fallbackHost + ":" + defaultPort
}

func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

const hour = time.Hour

// nomadTLSDir holds grove's Nomad CA and certificates (internal/pki).
func nomadTLSDir(cfgDir string) string { return filepath.Join(cfgDir, "nomad", "tls") }

func nomadSandboxClientConfPath(cfgDir string) string {
	return filepath.Join(cfgDir, "nomad", "sandbox-client.hcl")
}

func nomadSandboxDataDir(cfgDir string) string { return filepath.Join(cfgDir, "nomad", "sandbox-data") }

// nomadServerSANs is what the server certificate must cover besides Nomad's role name: the host
// grove and the VMs dial it by.
func nomadServerSANs(host string) pki.ServerSANs {
	if ip := net.ParseIP(host); ip != nil {
		return pki.ServerSANs{IPs: []net.IP{ip}}
	}
	return pki.ServerSANs{DNSNames: []string{host}}
}

func nomadTLSConfigured(ep config.Endpoint, files pki.Files) bool {
	return ep.CACert == files.CA && ep.ClientCert == files.CLI && ep.ClientKey == files.CLIKey &&
		ep.NodeCert == files.Node && ep.NodeKey == files.NodeKey
}

// orchardCommitPath records which OrchardForkCommit the control plane's Orchard binary was built
// from, beside it.
func orchardCommitPath(binary string) string { return binary + ".grove-commit" }

func orchardBinaryAtPin(binary string) bool {
	got, err := os.ReadFile(orchardCommitPath(binary))
	return err == nil && strings.TrimSpace(string(got)) == OrchardForkCommit
}
