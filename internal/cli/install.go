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
	yes           bool
	noSudo        bool
	dryRun        bool
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bootstrap this machine into one of grove's roles (worker, control-plane, client).",
	Long: `grove install renders an idempotent plan for the given --role and applies it: Homebrew/
Tart/Orchard/Nomad dependencies, launchd jobs or systemd --user units, and ~/.config/grove/
config.yaml. Re-running it is safe — steps that are already satisfied are skipped.

Run it as the account the worker should run as (a dedicated standard user is fine, including from
a ` + "`su - <user>`" + ` shell with nobody logged in at the Mac), never as root: the worker and its
credential broker become LaunchDaemons that run as that user from boot.

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
	installCmd.Flags().BoolVar(&installFlags.yes, "yes", false, "run the root steps through sudo without asking (or, as root, apply them directly)")
	installCmd.Flags().BoolVar(&installFlags.noSudo, "no-sudo", false, "never run sudo; print the root steps for an admin")
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
	}

	_, err := applyInstall(cmd, opts, installFlags.dryRun, installFlags.yes, installFlags.noSudo)
	return err
}

// applyInstall runs the role's plan, then the privileged steps it left (root script, one sudo).
// The outcome says whether those ran, or were printed for an admin and are still to do.
func applyInstall(cmd *cobra.Command, opts install.Options, dryRun, yes, noSudo bool) (install.RootOutcome, error) {
	out := cmd.OutOrStdout()
	runner := install.ExecRunner{}
	steps, err := install.BuildPlan(runner, opts, out)
	if err != nil {
		return install.RootNone, err
	}

	fmt.Fprintf(out, "grove install --role %s (%d steps%s)\n", opts.Role, len(steps), dryRunSuffix(dryRun))
	results, err := install.RunPlan(cmd.Context(), steps, install.PlanOptions{
		DryRun: dryRun,
		Yes:    yes,
		IsRoot: os.Geteuid() == 0,
		Out:    out,
	})
	if err != nil {
		return install.RootNone, fmt.Errorf("install: %w", err)
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
	fmt.Fprintln(out, "\nrun `grove doctor` to verify.")
	return outcome, nil
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return ", dry-run"
	}
	return ""
}
