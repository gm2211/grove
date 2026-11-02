package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

var installFlags struct {
	role          string
	controller    string
	nomad         string
	server        string
	token         string
	registry      string
	registryUser  string
	registryToken string
	softnet       bool
	name          string
	labels        string
	force         bool
	yes           bool
	noSudo        bool
	rootless      bool
	dryRun        bool
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bootstrap this machine into one of grove's roles (worker, control-plane, client).",
	Long: `grove install renders an idempotent plan for the given --role and applies it: Homebrew/
Tart/Orchard/Nomad dependencies, launchd jobs or systemd --user units, and ~/.config/grove/
config.yaml. Re-running it is safe — steps that are already satisfied are skipped.

Re-running ` + "`--role worker`" + ` is also how an installed worker is repaired. It keeps the worker's
name and labels (from what it finds: its recorded identity, grove's wrapper, a hand-written
run-worker.sh, an old LaunchAgent), re-renders the current launch shape (wrapper, guest-agent
transport, LaunchDaemons) over whatever is there, and backs up under ~/.config/grove/backups/
and removes the older pieces it supersedes. --name/--labels override the identity; on the control
plane host a rename also rewrites fleet.yaml's workerSelectors and deletes the old Orchard worker
record, and anywhere else it refuses (see --force) and prints the change to make. Nothing to
change means nothing is touched or restarted.

Run it as the account the worker should run as (a dedicated standard user is fine, including from
a ` + "`su - <user>`" + ` shell with nobody logged in at the Mac), never as root: the worker and its
credential broker become LaunchDaemons that run as that user from boot.

A user with no sudo (not an admin) gets a rootless install automatically, or with --rootless: the
worker and broker become the user's own LaunchAgents, a caffeinate LaunchAgent keeps the Mac awake,
and nothing needs root or an admin (no block to paste). It gives up the VM firewall and isolated
network pools, and runs only while that user is logged in at the Mac; the install says so, and
records worker.rootless in config.yaml so re-runs stay rootless. --rootless=false keeps the
root-based install. See docs/INSTALL.md, "A worker with no root".

Steps that need root or touch system-wide settings (disabling sleep, the VM firewall, installing
the worker's LaunchDaemons) run last, together, as ONE script through a single sudo: if you are an
admin on a terminal grove shows the script, asks, and sudo asks for your password once. Otherwise
(not an admin, no terminal, declined, or --no-sudo) grove prints the same script as one block for
an admin to paste, and you re-run the same grove command afterwards. Use --dry-run to see the
whole plan without changing anything.`,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().StringVar(&installFlags.role, "role", "", "worker | control-plane | client (required)")
	installCmd.Flags().StringVar(&installFlags.controller, "controller", "", "Orchard controller URL")
	installCmd.Flags().StringVar(&installFlags.nomad, "nomad", "", "Nomad HTTP API URL")
	installCmd.Flags().StringVar(&installFlags.server, "server", "", "grove server URL")
	installCmd.Flags().StringVar(&installFlags.token, "token", "", "bootstrap/server token (meaning depends on --role)")
	installCmd.Flags().StringVar(&installFlags.registry, "registry", "ghcr.io", "container registry a worker authenticates to for private image pulls (worker role; see docs/INSTALL.md)")
	installCmd.Flags().StringVar(&installFlags.registryUser, "registry-user", "", "registry username for `tart login` (worker role)")
	installCmd.Flags().StringVar(&installFlags.registryToken, "registry-token", "", "registry password/PAT for `tart login` (worker role); also read from $GROVE_REGISTRY_TOKEN so it needn't be in shell history")
	installCmd.Flags().BoolVar(&installFlags.softnet, "softnet", false, "also install Softnet and print its one-time root step (worker role; needed on Macs that run fleet.yaml pools with network: isolated)")
	installCmd.Flags().StringVar(&installFlags.name, "name", "", "this worker's Orchard name, which fleet.yaml's workerSelector matches (worker role; default: the name it is already installed under, else the hostname)")
	installCmd.Flags().StringVar(&installFlags.labels, "labels", "", "this worker's Orchard labels, k=v,k=v, which fleet.yaml's workerSelector matches (worker role; default: the labels it is already installed with, else host=<name>,arch=arm64)")
	installCmd.Flags().BoolVar(&installFlags.force, "force", false, "rename a worker on a host that is not the control plane: print the fleet.yaml change it needs instead of refusing (worker role)")
	installCmd.Flags().BoolVar(&installFlags.yes, "yes", false, "run the root steps through sudo without asking (or, as root, apply them directly)")
	installCmd.Flags().BoolVar(&installFlags.noSudo, "no-sudo", false, "never run sudo; print the root steps for an admin")
	installCmd.Flags().BoolVar(&installFlags.rootless, "rootless", false, "install the worker with no root: per-user LaunchAgents, a caffeinate keep-awake agent, no VM firewall (worker role; chosen automatically for a user who isn't an admin; --rootless=false keeps the root-based install)")
	installCmd.Flags().BoolVar(&installFlags.dryRun, "dry-run", false, "print the plan; change nothing")
	_ = installCmd.MarkFlagRequired("role")
	Root.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	role := install.Role(installFlags.role)
	switch role {
	case install.RoleWorker, install.RoleControlPlane, install.RoleClient:
	default:
		return fmt.Errorf("--role must be %q, %q or %q (got %q)", install.RoleWorker, install.RoleControlPlane, install.RoleClient, installFlags.role)
	}

	registryToken := installFlags.registryToken
	if registryToken == "" {
		// Falling back to the environment lets the token skip shell history/process-listing
		// exposure entirely; the flag still exists for scripts that manage their own secret
		// handling.
		registryToken = os.Getenv("GROVE_REGISTRY_TOKEN")
	}

	opts := install.Options{
		Role:          role,
		Controller:    installFlags.controller,
		NomadAddr:     installFlags.nomad,
		ServerURL:     installFlags.server,
		Token:         installFlags.token,
		Registry:      installFlags.registry,
		RegistryUser:  installFlags.registryUser,
		RegistryToken: registryToken,
		Softnet:       installFlags.softnet,
		Hostname:      installFlags.name,
		Labels:        installFlags.labels,
		Force:         installFlags.force,
	}
	opts.Rootless, opts.NoAutoRootless = rootlessFlag(cmd, installFlags.rootless)

	_, err := applyInstall(cmd, opts, installFlags.dryRun, installFlags.yes, installFlags.noSudo)
	return err
}

// rootlessFlag turns --rootless into the install options: --rootless asks for it, --rootless=false
// turns off both the automatic choice and a rootless install recorded in config.yaml.
func rootlessFlag(cmd *cobra.Command, rootless bool) (on, off bool) {
	if f := cmd.Flags().Lookup("rootless"); f != nil && f.Changed {
		return rootless, !rootless
	}
	return false, false
}

// applyInstall runs the role's plan, then the privileged steps it left (root script, one sudo).
// The outcome says whether those ran, or were printed for an admin and are still to do.
func applyInstall(cmd *cobra.Command, opts install.Options, dryRun, yes, noSudo bool) (install.RootOutcome, error) {
	out := cmd.OutOrStdout()
	runner := install.ExecRunner{}
	if opts.Role == install.RoleWorker && install.AutoRootless(cmd.Context(), runner, opts, out) {
		opts.Rootless = true
	}
	steps, results, err := runInstallPlan(cmd, runner, opts, dryRun, yes)
	if err != nil {
		return install.RootNone, err
	}
	needed := false
	for _, res := range results {
		needed = needed || res.Needed
	}
	if !dryRun && !needed {
		fmt.Fprintf(out, "\nnothing to change: %s is already up to date; nothing was touched or restarted.\n", opts.Role)
	}
	outcome := install.RootNone
	if !dryRun {
		var rootSteps []install.Step
		for _, res := range results {
			if !res.Skipped {
				continue
			}
			for _, s := range steps {
				if s.Name == res.Step && s.Privileged {
					rootSteps = append(rootSteps, s)
				}
			}
		}
		outcome, err = install.RunRootSteps(cmd.Context(), runner, rootSteps, install.RootOptions{
			Interactive: stdinIsTerminal(),
			Yes:         yes,
			NoSudo:      noSudo,
			In:          cmd.InOrStdin(),
			Out:         out,
		})
		if err != nil {
			return outcome, fmt.Errorf("install: %w", err)
		}
	}
	if outcome == install.RootPrinted {
		return outcome, nil
	}
	if outcome == install.RootRan && opts.Role == install.RoleWorker {
		// The LaunchDaemons are loaded now: finish what waited for them (retire the superseded
		// shape, complete a rename).
		if err := install.FinishWorkerInstall(cmd.Context(), runner, opts, out); err != nil {
			return outcome, fmt.Errorf("install: %w", err)
		}
	}
	fmt.Fprintln(out, "\nrun `grove doctor` to verify.")
	return outcome, nil
}

// runInstallPlan builds the role's plan and runs it, leaving the privileged steps (listed [root])
// for the caller.
func runInstallPlan(cmd *cobra.Command, runner install.Runner, opts install.Options, dryRun, yes bool) ([]install.Step, []install.StepResult, error) {
	out := cmd.OutOrStdout()
	if opts.GroveVersion == "" {
		opts.GroveVersion = Version
	}
	steps, err := install.BuildPlan(runner, opts, out)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(out, "grove install --role %s (%d steps%s)\n", opts.Role, len(steps), dryRunSuffix(dryRun))
	results, err := install.RunPlan(cmd.Context(), steps, install.PlanOptions{
		DryRun: dryRun,
		Yes:    yes,
		IsRoot: os.Geteuid() == 0,
		Out:    out,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("install: %w", err)
	}
	return steps, results, nil
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return ", dry-run"
	}
	return ""
}
